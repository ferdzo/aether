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

	// maxTimeoutSeconds bounds a guest-side exec timeout, in lockstep with
	// protocol.MaxTimeoutSeconds. It keeps an over-large value from overflowing
	// time.Duration (producing an immediate, bogus timeout).
	maxTimeoutSeconds = 24 * 60 * 60

	// maxMessageBytes bounds one newline-delimited protocol message, in lockstep
	// with maxMessageBytes in shared/protocol/exec.go. A peer that sends an
	// unterminated line must not be able to grow guest memory without bound.
	maxMessageBytes = 4 << 20 // 4 MiB

	// sendWriteTimeout bounds a single event write to the host. If the host
	// stops reading but stays connected, a write past this deadline fails and
	// the exec is abandoned instead of blocking the pumps (and the child)
	// forever with the single-exec gate held.
	sendWriteTimeout = 15 * time.Second

	// pipeDrainGrace is how long serveExec waits for the output pipes to reach
	// EOF after the child is reaped. A child that leaves a pipe open (a
	// background process that inherited stdout, or a setsid-escaped daemon) must
	// not hold the gate; past the grace the pipes are closed explicitly.
	pipeDrainGrace = 500 * time.Millisecond

	// controlIdleTimeout closes a control connection that sends nothing while no
	// exec is running, so an abandoned connection cannot pin a goroutine
	// forever. It is not applied while an exec is in flight.
	controlIdleTimeout = 10 * time.Minute
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
	typeCancel   = "cancel"

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

// execEvent mirrors shared/protocol.ExecEvent. Data is []byte so the wire is
// byte-exact (JSON base64-encodes byte slices); a string would corrupt invalid
// UTF-8 and a chunk boundary that split a rune.
type execEvent struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	Data     []byte `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Busy     bool   `json:"busy,omitempty"`
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
			if len(line) > maxMessageBytes {
				return fmt.Errorf("exec protocol: message exceeds %d bytes", maxMessageBytes)
			}
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

// runningExec is the exec currently holding the gate, if any. It lets the
// control reader cancel an in-flight exec without blocking the exec itself.
var (
	execControlMu sync.Mutex
	runningExec   *execControl
)

// execControl is the cancellation handle for one running exec.
type execControl struct {
	pid       int
	cancelled bool
}

func setRunningExec(c *execControl) {
	execControlMu.Lock()
	runningExec = c
	execControlMu.Unlock()
}

func clearRunningExec(c *execControl) {
	execControlMu.Lock()
	if runningExec == c {
		runningExec = nil
	}
	execControlMu.Unlock()
}

// cancelRunningExec kills the process group of the running exec, if any. A
// cancel that arrives before the child is started is remembered so serveExec
// kills it as soon as it knows the pid.
func cancelRunningExec() {
	execControlMu.Lock()
	c := runningExec
	pid := 0
	if c != nil {
		c.cancelled = true
		pid = c.pid
	}
	execControlMu.Unlock()
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// recordPID publishes the child's process-group id and reports whether a cancel
// arrived before it was known.
func (c *execControl) recordPID(pid int) (cancelNow bool) {
	execControlMu.Lock()
	c.pid = pid
	cancelNow = c.cancelled
	execControlMu.Unlock()
	return cancelNow
}

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
	done := make(chan struct{})
	defer close(done)
	br := bufio.NewReader(conn)

	// Bound the handshake so an idle peer cannot pin this goroutine.
	_ = conn.SetReadDeadline(time.Now().Add(controlIdleTimeout))
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
	_ = conn.SetReadDeadline(time.Time{})

	// The reader runs concurrently so a cancel can be honoured while an exec is
	// in flight; requests are served one at a time on this goroutine.
	reqs := make(chan execRequest)
	go controlReader(conn, br, reqs, done)

	for req := range reqs {
		switch req.Type {
		case typeExec, "":
			if !execActive.CompareAndSwap(false, true) {
				_ = writeMessage(conn, execEvent{
					Type:     eventExited,
					ID:       req.ID,
					ExitCode: execBusyExitCode,
					Busy:     true,
					Error:    "exec service busy: another command is running",
				})
				continue
			}
			runExecGuarded(conn, req)
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

// runExecGuarded runs one exec while guaranteeing the single-exec gate is
// released on every path, including a panic in a handler. Without the defer a
// panicking or stuck handler would weld the gate shut and make every later exec
// fail busy until the VM is destroyed.
func runExecGuarded(conn *os.File, req execRequest) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "aether-env: exec handler panic: %v\n", r)
		}
		execActive.Store(false)
		// Re-arm the idle deadline so a connection abandoned after an exec is
		// eventually closed instead of pinning its reader forever.
		_ = conn.SetReadDeadline(time.Now().Add(controlIdleTimeout))
	}()
	// Clear any idle read deadline the reader installed before it saw this exec.
	_ = conn.SetReadDeadline(time.Time{})
	serveExec(conn, req)
}

// controlReader reads control messages and forwards exec requests to the
// serving loop. A cancel is handled inline (killing the running exec) so it can
// be honoured while an exec is in flight. It returns when the peer disconnects
// or the idle deadline fires while no exec is running.
func controlReader(conn *os.File, br *bufio.Reader, reqs chan<- execRequest, done <-chan struct{}) {
	defer close(reqs)
	for {
		if execActive.Load() {
			// An exec is in flight: a long command may legitimately run for a
			// while, so do not time the read out.
			_ = conn.SetReadDeadline(time.Time{})
		} else {
			_ = conn.SetReadDeadline(time.Now().Add(controlIdleTimeout))
		}
		var req execRequest
		if err := readMessage(br, &req); err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "aether-env: control read: %v\n", err)
			}
			return
		}
		_ = conn.SetReadDeadline(time.Time{})
		if req.Type == typeCancel {
			cancelRunningExec()
			continue
		}
		select {
		case reqs <- req:
		case <-done:
			return
		}
	}
}

// eventSink serialises events for one exec onto one writer. stdout and stderr
// are pumped from separate goroutines, so writes must be interlocked.
//
// send returns an error rather than discarding it and bounds each write with a
// deadline, so a host that stops reading but stays connected cannot block the
// pumps (and therefore the child) forever with the gate held.
type eventSink struct {
	mu     sync.Mutex
	w      io.Writer
	id     string
	failed bool
}

var errSinkFailed = errors.New("event sink write failed")

func (s *eventSink) send(ev execEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		return errSinkFailed
	}
	if d, ok := s.w.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = d.SetWriteDeadline(time.Now().Add(sendWriteTimeout))
	}
	if err := writeMessage(s.w, ev); err != nil {
		s.failed = true
		return err
	}
	return nil
}

// serveExec runs one command and streams its result. Every failure of the
// service itself is reported as a terminal exited event, never as a panic or a
// dead service: a non-zero exit, a missing binary and a timeout are all normal
// results.
func serveExec(conn *os.File, req execRequest) {
	ctl := &execControl{}
	setRunningExec(ctl)
	defer clearRunningExec(ctl)

	sink := &eventSink{w: conn, id: req.ID}
	if err := sink.send(execEvent{Type: eventStarted, ID: req.ID}); err != nil {
		return
	}

	if len(req.Argv) == 0 {
		_ = sink.send(execEvent{Type: eventExited, ID: req.ID, ExitCode: 1, Error: "empty argv"})
		return
	}

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Cwd
	cmd.Env = mergeEnv(os.Environ(), req.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Use explicit pipes rather than cmd.StdoutPipe so cmd.Wait does not close
	// the read ends: completion must be gated on the child, never on pipe EOF.
	// A child (or a setsid-escaped grandchild) that keeps the write end open
	// would otherwise hold the single-exec gate forever.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = sink.send(execEvent{Type: eventExited, ID: req.ID, ExitCode: 1, Error: err.Error()})
		return
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = sink.send(execEvent{Type: eventExited, ID: req.ID, ExitCode: 1, Error: err.Error()})
		return
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		// A command that cannot even start (not found, not executable) is a
		// normal result: 127 matches the shell convention.
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		code := 1
		if errors.Is(err, exec.ErrNotFound) {
			code = 127
		}
		_ = sink.send(execEvent{Type: eventExited, ID: req.ID, ExitCode: code, Error: err.Error()})
		return
	}
	// The child owns the write ends now; close our copies so EOF is observable
	// once every holder is gone.
	_ = stdoutW.Close()
	_ = stderrW.Close()

	// Honour a cancel that raced the start.
	if ctl.recordPID(cmd.Process.Pid) {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// If the host stops reading, kill the process group: an abandoned exec must
	// not run to completion.
	kill := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	// Pump both pipes concurrently so a child that fills one pipe buffer cannot
	// block the other, and so the service cannot deadlock on output.
	var pumpWG sync.WaitGroup
	pumpWG.Add(2)
	go func() { defer pumpWG.Done(); pumpOutput(stdoutR, sink, eventStdout, kill) }()
	go func() { defer pumpWG.Done(); pumpOutput(stderrR, sink, eventStderr, kill) }()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	timedOut := false
	if d := time.Duration(clampExecTimeout(req.TimeoutSeconds)) * time.Second; d > 0 {
		timer := time.NewTimer(d)
		select {
		case <-waitCh:
			timer.Stop()
		case <-timer.C:
			timedOut = true
			// Kill the whole process group so grandchildren cannot outlive the
			// deadline.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-waitCh
		}
	} else {
		<-waitCh
	}

	// The exec child is reaped (and the gate is still held, so it is the only
	// exec child in the process): reap any unrelated children reparented to
	// this PID 1 so double-forked daemons do not accumulate as zombies.
	reapOrphans()

	// Drain the pipes for a bounded grace only. A lingering writer must not
	// hold the gate; past the grace the read ends are closed to unblock the
	// pumps and the incomplete output is reported as a failure.
	drained := make(chan struct{})
	go func() { pumpWG.Wait(); close(drained) }()
	lingering := false
	select {
	case <-drained:
	case <-time.After(pipeDrainGrace):
		lingering = true
		_ = stdoutR.Close()
		_ = stderrR.Close()
		<-drained
	}
	_ = stdoutR.Close()
	_ = stderrR.Close()

	code := exitCodeFromState(cmd.ProcessState)
	if timedOut {
		code = processTimeoutExitCode
	}
	ev := execEvent{Type: eventExited, ID: req.ID, ExitCode: code, TimedOut: timedOut}
	if lingering {
		ev.Error = "output pipe still open after the command exited; output may be incomplete"
		fmt.Fprintf(os.Stderr, "aether-env: exec %q left an output pipe open; closed after %v\n", req.ID, pipeDrainGrace)
	}
	_ = sink.send(ev)
}

// clampExecTimeout bounds a guest-side timeout to maxTimeoutSeconds so the
// Duration conversion cannot overflow into a negative, immediately-firing
// timer.
func clampExecTimeout(seconds int) int {
	if seconds <= 0 {
		return 0
	}
	if seconds > maxTimeoutSeconds {
		return maxTimeoutSeconds
	}
	return seconds
}

// reapOrphans reaps children reparented to this PID 1 without blocking. It must
// only be called while the exec gate is held, so it can never steal the status
// of a live exec child.
func reapOrphans() {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
	}
}

// pumpOutput forwards r to the sink as events of kind, capped at maxStreamBytes
// for the life of the exec. Bytes past the cap are still read (so the child is
// never blocked on a full pipe) but dropped, with a one-time truncation note.
func pumpOutput(r io.Reader, sink *eventSink, kind string, kill func()) {
	buf := make([]byte, 32*1024)
	remaining := maxStreamBytes
	noted := false
	abandoned := false
	// note copies the reused buffer: Data must own its bytes because each event
	// is encoded asynchronously (and the buffer is overwritten by the next
	// Read).
	note := func() error {
		return sink.send(execEvent{Type: kind, ID: sink.id,
			Data: []byte(fmt.Sprintf("\n[aether: %s truncated after %d bytes]\n", kind, maxStreamBytes))})
	}
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			var sendErr error
			if remaining <= 0 {
				if !noted {
					// Defensive: the note is normally emitted with the last
					// retained chunk, below.
					sendErr = note()
					noted = true
				}
			} else if len(chunk) > remaining {
				_ = sink.send(execEvent{Type: kind, ID: sink.id, Data: append([]byte(nil), chunk[:remaining]...)})
				sendErr = note()
				remaining = 0
				noted = true
			} else {
				remaining -= len(chunk)
				sendErr = sink.send(execEvent{Type: kind, ID: sink.id, Data: append([]byte(nil), chunk...)})
			}
			if sendErr != nil && !abandoned {
				// The host can no longer receive: kill the child so an
				// abandoned exec does not run to completion.
				abandoned = true
				kill()
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
