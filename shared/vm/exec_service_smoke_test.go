package vm_test

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aether/shared/protocol"
	"aether/shared/vm"
)

// execServiceSmokePort must match execServicePort in init/exec_service.go.
const execServiceSmokePort = 5252

// TestExecServiceSmoke is the real, end-to-end test of the persistent-execution
// guest service. It boots a real Firecracker microVM with a writable workspace
// drive and a virtio-vsock device, and NO NIC, then drives the guest over the
// vsock Unix socket exactly as a worker would: host-initiated connection
// ("CONNECT <port>\n"), Hello/Ready handshake, then a stream of execs.
//
// Opt-in: set AETHER_EXEC_SMOKE=1. Requires /dev/kvm and a rootfs built with
// `AETHER_JOB_INIT=exec-service scripts/build-job-rootfs.sh` (point the test at
// it with AETHER_TEST_EXEC_ROOTFS; scripts/e2e-exec.sh does both).
//
// Deliberately no TAP/VMIP/Gateway/BootToken/MMDS: the service must be
// reachable over vsock alone, which is the whole point of persistent execs.
func TestExecServiceSmoke(t *testing.T) {
	if os.Getenv("AETHER_EXEC_SMOKE") != "1" {
		t.Skip("set AETHER_EXEC_SMOKE=1 to run the real Firecracker exec-service smoke test")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable (needed for a real boot): %v", err)
	}

	root := procRepoRoot(t)
	kernel := procEnvOr("AETHER_TEST_KERNEL", filepath.Join(root, ".assets", "vmlinux"))
	fcBin := procEnvOr("AETHER_TEST_FIRECRACKER", filepath.Join(root, ".assets", "bin", "firecracker"))
	rootfs := procEnvOr("AETHER_TEST_EXEC_ROOTFS", filepath.Join(root, ".assets", "job-rootfs-exec.ext4"))

	for _, p := range []struct{ path, what string }{
		{kernel, "kernel"},
		{fcBin, "firecracker binary"},
		{rootfs, "exec-service rootfs"},
	} {
		if _, err := os.Stat(p.path); err != nil {
			t.Skipf("%s not found at %s: %v (build it with scripts/e2e-exec.sh)", p.what, p.path, err)
		}
	}

	workDir := t.TempDir()
	wsImage := filepath.Join(workDir, "workspace.ext4")
	createExt4(t, wsImage, "64M")

	vsockPath := filepath.Join(workDir, "vsock.sock")
	console := &procSyncBuffer{}

	cfg := vm.Config{
		KernelPath: kernel,
		RootFSPath: rootfs,
		Drives:     []vm.DriveSpec{{Path: wsImage}},
		SocketPath: filepath.Join(workDir, "fc.sock"),
		Vsock:      &vm.VsockSpec{Path: vsockPath, CID: 3},
		VCPUCount:  1,
		MemSizeMB:  256,
		Stdout:     console,
		Stderr:     console,
	}

	machine, err := vm.NewManager(fcBin).Launch(cfg)
	if err != nil {
		t.Fatalf("launch Firecracker: %v\nconsole:\n%s", err, console.String())
	}
	t.Cleanup(func() {
		if machine.Machine != nil {
			_ = machine.Stop()
		}
	})

	c := waitExecClient(t, vsockPath, console)
	t.Cleanup(func() { _ = c.conn.Close() })
	t.Logf("exec service reachable over vsock at %s", vsockPath)

	// Test 1: echo hello -> exit 0, stdout contains hello, stderr empty.
	res := c.exec(t, "t1", protocol.ExecRequest{Argv: []string{"echo", "hello"}})
	t.Logf("test1 (echo hello): %+v", res)
	if res.ExitCode != 0 {
		t.Errorf("test1: exit code = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hello") {
		t.Errorf("test1: stdout = %q, want it to contain %q", res.Stdout, "hello")
	}
	if res.Stderr != "" {
		t.Errorf("test1: stderr = %q, want empty", res.Stderr)
	}

	// stdout and stderr must be reported separately and never crossed.
	sep := c.exec(t, "sep", protocol.ExecRequest{Argv: []string{"sh", "-c", "echo out; echo err 1>&2"}})
	t.Logf("stream separation: %+v", sep)
	if !strings.Contains(sep.Stdout, "out") || strings.Contains(sep.Stdout, "err") {
		t.Errorf("separation: stdout = %q, want only %q", sep.Stdout, "out")
	}
	if !strings.Contains(sep.Stderr, "err") || strings.Contains(sep.Stderr, "out") {
		t.Errorf("separation: stderr = %q, want only %q", sep.Stderr, "err")
	}

	// Test 2: a non-zero exit is a normal result and the VM keeps serving.
	res = c.exec(t, "t2", protocol.ExecRequest{Argv: []string{"sh", "-c", "exit 42"}})
	t.Logf("test2 (exit 42): %+v", res)
	if res.ExitCode != 42 {
		t.Errorf("test2: exit code = %d, want 42", res.ExitCode)
	}
	if res.TimedOut {
		t.Errorf("test2: TimedOut = true, want false")
	}

	// Test 3: two more sequential execs both succeed on the same VM.
	for _, id := range []string{"t3a", "t3b"} {
		r := c.exec(t, id, protocol.ExecRequest{Argv: []string{"echo", id}})
		if r.ExitCode != 0 || !strings.Contains(r.Stdout, id) {
			t.Errorf("test3 %s: %+v, want exit 0 and stdout containing %q", id, r, id)
		}
	}

	// Test 4: the workspace persists across execs.
	if r := c.exec(t, "t4-write", protocol.ExecRequest{Argv: []string{"sh", "-c", "echo persistent > /workspace/test"}}); r.ExitCode != 0 {
		t.Errorf("test4 write: %+v, want exit 0", r)
	}
	r := c.exec(t, "t4-read", protocol.ExecRequest{Argv: []string{"cat", "/workspace/test"}})
	t.Logf("test4 (cat /workspace/test): %+v", r)
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "persistent") {
		t.Errorf("test4 read: %+v, want exit 0 and stdout containing persistent", r)
	}

	// Test 5: Cwd is honoured.
	if r := c.exec(t, "t5-mkdir", protocol.ExecRequest{Argv: []string{"mkdir", "-p", "/workspace/foo"}}); r.ExitCode != 0 {
		t.Errorf("test5 mkdir: %+v, want exit 0", r)
	}
	r = c.exec(t, "t5-pwd", protocol.ExecRequest{Argv: []string{"pwd"}, Cwd: "/workspace/foo"})
	t.Logf("test5 (pwd in /workspace/foo): %+v", r)
	if r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != "/workspace/foo" {
		t.Errorf("test5: %+v, want exit 0 and stdout /workspace/foo", r)
	}

	// Test 6: Env overrides are applied.
	r = c.exec(t, "t6", protocol.ExecRequest{Argv: []string{"sh", "-c", "echo \"$FOO\""}, Env: map[string]string{"FOO": "bar"}})
	t.Logf("test6 (FOO=bar): %+v", r)
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "bar") {
		t.Errorf("test6: %+v, want exit 0 and stdout containing bar", r)
	}

	// Test 7: a timeout kills the process group, reports TimedOut, and leaves
	// the service available for the next exec.
	r = c.exec(t, "t7-timeout", protocol.ExecRequest{Argv: []string{"sleep", "60"}, TimeoutSeconds: 2})
	t.Logf("test7 (sleep 60, timeout 2s): %+v", r)
	if !r.TimedOut {
		t.Errorf("test7: TimedOut = false, want true")
	}
	if r.ExitCode != 124 {
		t.Errorf("test7: exit code = %d, want 124", r.ExitCode)
	}
	r = c.exec(t, "t7-alive", protocol.ExecRequest{Argv: []string{"echo", "still-alive"}})
	t.Logf("test7 (still alive): %+v", r)
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "still-alive") {
		t.Errorf("test7: service unusable after timeout: %+v", r)
	}

	// Test 8: a second Exec while one is active is rejected as busy. A second
	// control connection is opened; the service serves connections
	// concurrently but admits only one exec at a time.
	c2, err := dialExecClient(vsockPath)
	if err != nil {
		t.Fatalf("second control connection: %v", err)
	}
	defer c2.conn.Close()

	c.send(t, protocol.ExecRequest{Type: protocol.TypeExec, ID: "busy-sleep", Argv: []string{"sleep", "3"}, TimeoutSeconds: 10})
	started := c.readEvent(t)
	if started.Type != protocol.EventStarted {
		t.Fatalf("busy: first event = %q, want %q", started.Type, protocol.EventStarted)
	}
	c2.send(t, protocol.ExecRequest{Type: protocol.TypeExec, ID: "busy-probe", Argv: []string{"echo", "should-not-run"}})
	busy := c2.readEvent(t)
	t.Logf("busy rejection: %+v", busy)
	if busy.Type != protocol.EventExited || busy.ExitCode != 125 || !strings.Contains(strings.ToLower(busy.Error), "busy") {
		t.Errorf("busy: got %+v, want exited/125 with a busy error", busy)
	}
	// Drain the still-running sleep so the service is idle again.
	busyRes, err := protocol.CollectExec(c.br, "busy-sleep")
	if err != nil {
		t.Fatalf("drain sleep: %v", err)
	}
	t.Logf("busy-sleep finished: %+v", busyRes)

	// Shutdown: sync and reset; with reboot=k the VMM must exit.
	c.send(t, protocol.Shutdown{Type: protocol.TypeShutdown})
	done := make(chan error, 1)
	go func() { done <- machine.Wait() }()
	select {
	case waitErr := <-done:
		t.Logf("VM exited after Shutdown, Wait() = %v", waitErr)
		if waitErr != nil {
			t.Errorf("VM exited with error after Shutdown: %v", waitErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("VM did not exit after Shutdown; console:\n%s", console.String())
	}
}

// execClient is a host-side control connection to the guest exec service.
type execClient struct {
	conn net.Conn
	br   *bufio.Reader
}

// dialExecClient opens a host-initiated vsock connection: connect to the
// device's Unix socket, announce the guest port with "CONNECT <port>\n", read
// Firecracker's "OK <hostside-port>\n" acknowledgement, then perform the
// Hello/Ready handshake. If nobody is listening in the guest, Firecracker
// closes the connection instead of acknowledging.
func dialExecClient(udsPath string) (*execClient, error) {
	conn, err := net.DialTimeout("unix", udsPath, 2*time.Second)
	if err != nil {
		return nil, err
	}
	c := &execClient{conn: conn, br: bufio.NewReader(conn)}

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", execServiceSmokePort); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	ack, err := c.br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock connect ack: %w", err)
	}
	if !strings.HasPrefix(ack, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock connect: unexpected ack %q", ack)
	}

	if err := protocol.WriteMessage(conn, protocol.Hello{Type: protocol.TypeHello, Version: 1}); err != nil {
		conn.Close()
		return nil, err
	}
	var ready protocol.Ready
	if err := protocol.ReadMessage(c.br, &ready); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Time{})
	if ready.Type != protocol.TypeReady {
		conn.Close()
		return nil, fmt.Errorf("unexpected ready message: %+v", ready)
	}
	if ready.Error != "" {
		conn.Close()
		return nil, fmt.Errorf("guest refused handshake: %s", ready.Error)
	}
	return c, nil
}

// waitExecClient retries dialExecClient until the guest service is listening.
// Firecracker's host-initiated connect fails until the guest has bound the
// port, so boot readiness is observed as a successful handshake.
func waitExecClient(t *testing.T, udsPath string, console *procSyncBuffer) *execClient {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := dialExecClient(udsPath)
		if err == nil {
			return c
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("exec service not reachable over vsock within 45s: %v\nconsole:\n%s", lastErr, console.String())
	return nil
}

func (c *execClient) send(t *testing.T, v any) {
	t.Helper()
	if err := protocol.WriteMessage(c.conn, v); err != nil {
		t.Fatalf("send %T: %v", v, err)
	}
}

func (c *execClient) readEvent(t *testing.T) protocol.ExecEvent {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	defer c.conn.SetReadDeadline(time.Time{})
	var ev protocol.ExecEvent
	if err := protocol.ReadMessage(c.br, &ev); err != nil {
		t.Fatalf("read event: %v", err)
	}
	return ev
}

// exec runs one command to completion and returns its aggregated result.
func (c *execClient) exec(t *testing.T, id string, req protocol.ExecRequest) protocol.ExecResult {
	t.Helper()
	req.Type = protocol.TypeExec
	req.ID = id
	c.send(t, req)
	_ = c.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	res, err := protocol.CollectExec(c.br, id)
	_ = c.conn.SetReadDeadline(time.Time{})
	if err != nil {
		t.Fatalf("collect exec %s: %v", id, err)
	}
	return res
}

// createExt4 makes a fresh ext4 image at path with the given mke2fs size
// (for example "64M"), without mounting or root.
func createExt4(t *testing.T, path, size string) {
	t.Helper()
	mke2fs, err := exec.LookPath("mke2fs")
	if err != nil {
		t.Skip("mke2fs not found (install e2fsprogs)")
	}
	out, err := exec.Command(mke2fs, "-q", "-F", "-t", "ext4", "-m", "0", path, size).CombinedOutput()
	if err != nil {
		t.Fatalf("mke2fs %s: %v: %s", path, err, out)
	}
}
