package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Exec-service mode is a long-lived guest control service reachable over
// virtio-vsock. aether-env runs as PID 1; the service never exits on its own
// and never reboots the guest when a command finishes, so one VM can serve
// many commands. This is the one-shot supervisor's opposite: process mode runs
// one child and resets, exec-service keeps the VM alive.
//
// The guest is the server and the host is the client: the host connects to the
// vsock device's Unix socket and writes "CONNECT <port>\n" (a host-initiated
// connection), after which Firecracker bridges the stream to the guest
// listener. Host-initiated is chosen because the guest is a service with a
// fixed, well-known port: it needs only one listening socket and a trivial
// accept loop, while the host controls connection lifetime and can open a new
// connection whenever it wants. A guest-initiated design would instead require
// the guest to know a host port up front and to reconnect on failure, for no
// benefit here.
const (
	// execServiceFlag is the explicit, MMDS-free entry point:
	// aether-env --exec-service
	execServiceFlag = "--exec-service"

	// execServicePort is the guest vsock port the service listens on.
	execServicePort = 5252

	// maxStreamBytes caps how much of each output stream (stdout and stderr
	// separately) is forwarded to the host. A chatty command cannot grow guest
	// memory without bound; past the cap the remaining bytes are drained from
	// the pipe but discarded, and a truncation note is sent in-band.
	maxStreamBytes = 1 << 20 // 1 MiB per stream

	// execBusyExitCode is reported when an Exec arrives while another is
	// running. 125 is the shell's "command cannot execute" convention.
	execBusyExitCode = 125
)

// --- wire types -------------------------------------------------------------
//
// Wire-compatible copy of shared/protocol/exec.go. init is a dependency-free
// module (its own go.mod, stdlib only) so it cannot import aether/shared;
// these declarations must stay in lockstep with shared/protocol/exec.go.

const (
	typeHello    = "hello"
	typeReady    = "ready"
	typeExec     = "exec"
	typeShutdown = "shutdown"

	eventStarted = "started"
	eventStdout  = "stdout"
	eventStderr  = "stderr"
	eventExited  = "exited"
)

type hello struct {
	Type    string `json:"type"`
	Version int    `json:"version,omitempty"`
}

type ready struct {
	Type  string `json:"type"`
	Error string `json:"error,omitempty"`
}

type execRequest struct {
	Type           string            `json:"type"`
	ID             string            `json:"id,omitempty"`
	Argv           []string          `json:"argv"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

type execEvent struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	Data     string `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Error    string `json:"error,omitempty"`
}

func writeMessage(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func readMessage(r io.Reader, v any) error {
	var line []byte
	var one [1]byte
	for {
		n, err := r.Read(one[:])
		if n > 0 {
			if one[0] == '\n' {
				break
			}
			line = append(line, one[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(line) == 0 {
					return io.EOF
				}
				break
			}
			return err
		}
	}
	if len(line) == 0 {
		return errors.New("exec protocol: empty message")
	}
	return json.Unmarshal(line, v)
}

// --- guest vsock sockets ----------------------------------------------------
//
// The AF_VSOCK socket type has no portable net.Listener support in the stdlib,
// so the listener and accepted sockets are driven with raw syscalls. Addresses
// use the kernel's struct sockaddr_vm layout. AF_VSOCK is not defined in
// syscall for linux/amd64, hence the literal 0x28 (40).

const (
	afVsock      = 0x28
	vmaddrCIDAny = 0xFFFFFFFF
)

type sockaddrVM struct {
	Family   uint16
	Reserved uint16
	Port     uint32
	CID      uint32
	Zero     [4]byte
}

// listenVsock binds and listens on port for any CID, returning the raw fd with
// close-on-exec set so it is never leaked into a child command.
func listenVsock(port uint32) (int, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("vsock socket: %w", err)
	}
	sa := &sockaddrVM{Family: afVsock, Port: port, CID: vmaddrCIDAny}
	if _, _, errno := syscall.Syscall(syscall.SYS_BIND,
		uintptr(fd), uintptr(unsafe.Pointer(sa)), unsafe.Sizeof(*sa)); errno != 0 {
		syscall.Close(fd)
		return -1, fmt.Errorf("vsock bind port %d: %w", port, errno)
	}
	if err := syscall.Listen(fd, 16); err != nil {
		syscall.Close(fd)
		return -1, fmt.Errorf("vsock listen: %w", err)
	}
	return fd, nil
}

// acceptVsock accepts one connection with the raw accept4 syscall. It cannot
// use syscall.Accept/Accept4 because those try to parse the peer address and
// fail for AF_VSOCK.
func acceptVsock(fd int) (int, error) {
	nfd, _, errno := syscall.Syscall6(syscall.SYS_ACCEPT4,
		uintptr(fd), 0, 0, uintptr(syscall.SOCK_CLOEXEC), 0, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(nfd), nil
}

// --- service ----------------------------------------------------------------

// execActive is the process-wide single-exec gate. Only one command runs at a
// time across all control connections; a concurrent Exec is rejected as busy
// rather than queued.
var execActive atomic.Bool

// runExecService is the long-lived guest control service. It never returns on
// its own: it accepts control connections forever, serving each on its own
// goroutine. A guest reset (Shutdown) terminates it by rebooting the VM.
func runExecService() error {
	lfd, err := listenVsock(execServicePort)
	if err != nil {
		return err
	}
	defer syscall.Close(lfd)
	fmt.Fprintf(os.Stderr, "aether-env: exec service listening on vsock port %d (pid %d)\n", execServicePort, os.Getpid())

	for {
		nfd, err := acceptVsock(lfd)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			// An accept failure must not kill the service: log, back off and
			// keep serving so the VM stays useful.
			fmt.Fprintf(os.Stderr, "aether-env: vsock accept: %v\n", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		conn := os.NewFile(uintptr(nfd), "vsock-conn")
		go serveControlConn(conn)
	}
}

// serveControlConn performs the Hello/Ready handshake and then serves control
// messages until the peer disconnects or asks for Shutdown.
func serveControlConn(conn *os.File) {
	defer conn.Close()
	br := bufio.NewReader(conn)

	var h hello
	if err := readMessage(br, &h); err != nil {
		fmt.Fprintf(os.Stderr, "aether-env: control handshake read: %v\n", err)
		return
	}
	if h.Type != typeHello {
		_ = writeMessage(conn, ready{Type: typeReady, Error: fmt.Sprintf("expected %q, got %q", typeHello, h.Type)})
		return
	}
	if err := writeMessage(conn, ready{Type: typeReady}); err != nil {
		return
	}

	for {
		var req execRequest
		if err := readMessage(br, &req); err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "aether-env: control read: %v\n", err)
			}
			return
		}
		switch req.Type {
		case typeExec, "":
			if !execActive.CompareAndSwap(false, true) {
				_ = writeMessage(conn, execEvent{
					Type:     eventExited,
					ID:       req.ID,
					ExitCode: execBusyExitCode,
					Error:    "exec service busy: another command is running",
				})
				continue
			}
			serveExec(conn, req)
			execActive.Store(false)
		case typeShutdown:
			shutdownGuest()
			return
		default:
			_ = writeMessage(conn, execEvent{
				Type:     eventExited,
				ID:       req.ID,
				ExitCode: 1,
				Error:    fmt.Sprintf("unknown control message type %q", req.Type),
			})
		}
	}
}

// eventSink serialises events for one exec onto one writer. stdout and stderr
// are pumped from separate goroutines, so writes must be interlocked.
type eventSink struct {
	mu sync.Mutex
	w  io.Writer
	id string
}

func (s *eventSink) send(ev execEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = writeMessage(s.w, ev)
}

// serveExec runs one command and streams its result. Every failure of the
// service itself is reported as a terminal exited event, never as a panic or a
// dead service: a non-zero exit, a missing binary and a timeout are all normal
// results.
func serveExec(conn io.Writer, req execRequest) {
	sink := &eventSink{w: conn, id: req.ID}
	sink.send(execEvent{Type: eventStarted, ID: req.ID})

	if len(req.Argv) == 0 {
		sink.send(execEvent{Type: eventExited, ID: req.ID, ExitCode: 1, Error: "empty argv"})
		return
	}

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Cwd
	cmd.Env = mergeEnv(os.Environ(), req.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		sink.send(execEvent{Type: eventExited, ID: req.ID, ExitCode: 1, Error: err.Error()})
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		sink.send(execEvent{Type: eventExited, ID: req.ID, ExitCode: 1, Error: err.Error()})
		return
	}

	if err := cmd.Start(); err != nil {
		// A command that cannot even start (not found, not executable) is a
		// normal result: 127 matches the shell convention.
		code := 1
		if errors.Is(err, exec.ErrNotFound) {
			code = 127
		}
		sink.send(execEvent{Type: eventExited, ID: req.ID, ExitCode: code, Error: err.Error()})
		return
	}

	// Pump both pipes concurrently so a child that fills one pipe buffer
	// cannot block the other, and so the service cannot deadlock on output.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pumpOutput(stdoutPipe, sink, eventStdout) }()
	go func() { defer wg.Done(); pumpOutput(stderrPipe, sink, eventStderr) }()

	waitCh := make(chan error, 1)
	go func() {
		// Drain both pipes before reaping. cmd.Wait closes the pipes, so it
		// must run only after all reads have completed.
		wg.Wait()
		waitCh <- cmd.Wait()
	}()

	timedOut := false
	if t := req.TimeoutSeconds; t > 0 {
		timer := time.NewTimer(time.Duration(t) * time.Second)
		select {
		case <-waitCh:
			timer.Stop()
		case <-timer.C:
			timedOut = true
			// Kill the whole process group so grandchildren cannot outlive the
			// deadline, then wait for the reaper to finish.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-waitCh
		}
	} else {
		<-waitCh
	}

	code := exitCodeFromState(cmd.ProcessState)
	if timedOut {
		code = processTimeoutExitCode
	}
	sink.send(execEvent{Type: eventExited, ID: req.ID, ExitCode: code, TimedOut: timedOut})
}

// pumpOutput forwards r to the sink as events of kind, capped at maxStreamBytes
// for the life of the exec. Bytes past the cap are still read (so the child is
// never blocked on a full pipe) but dropped, with a one-time truncation note.
func pumpOutput(r io.Reader, sink *eventSink, kind string) {
	buf := make([]byte, 32*1024)
	remaining := maxStreamBytes
	noted := false
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if remaining <= 0 {
				if !noted {
					// Defensive: the note is normally emitted with the last
					// retained chunk, below.
					sink.send(execEvent{Type: kind, ID: sink.id,
						Data: fmt.Sprintf("\n[aether: %s truncated after %d bytes]\n", kind, maxStreamBytes)})
					noted = true
				}
			} else if len(chunk) > remaining {
				sink.send(execEvent{Type: kind, ID: sink.id, Data: string(chunk[:remaining])})
				sink.send(execEvent{Type: kind, ID: sink.id,
					Data: fmt.Sprintf("\n[aether: %s truncated after %d bytes]\n", kind, maxStreamBytes)})
				remaining = 0
				noted = true
			} else {
				remaining -= len(chunk)
				sink.send(execEvent{Type: kind, ID: sink.id, Data: string(chunk)})
			}
		}
		if err != nil {
			return
		}
	}
}

// mergeEnv returns base with overrides applied: an override replaces an
// existing variable and otherwise appends a new one. Base order is preserved so
// the child environment is stable and readable.
func mergeEnv(base []string, overrides map[string]string) []string {
	vals := make(map[string]string, len(base)+len(overrides))
	order := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		if _, ok := vals[k]; !ok {
			order = append(order, k)
		}
		vals[k] = v
	}
	extra := make([]string, 0, len(overrides))
	for k := range overrides {
		if _, ok := vals[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	order = append(order, extra...)
	for k, v := range overrides {
		vals[k] = v
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+vals[k])
	}
	return out
}

// shutdownGuest flushes filesystem writes and resets the guest.
//
// A reset (LINUX_REBOOT_CMD_RESTART) is used rather than poweroff because
// poweroff does not terminate Firecracker on x86; with the kernel command
// line's reboot=k, a reset makes the VMM exit. This mirrors runSupervisor.
func shutdownGuest() {
	syscall.Sync()
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
		fmt.Fprintf(os.Stderr, "aether-env: shutdown reboot failed: %v\n", err)
	}
}

// isExecServiceFlag reports whether argv selects the long-lived exec service.
// Only the first argument is inspected, mirroring parseProcessModeFlag, so the
// flag cannot be smuggled in as an argument to a command.
func isExecServiceFlag(argv []string) bool {
	return len(argv) > 0 && argv[0] == execServiceFlag
}
