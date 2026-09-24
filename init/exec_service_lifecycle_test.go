package main

import (
	"bufio"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// runOneExec runs one command through serveExec on a private pipe while holding
// the process-wide single-exec gate, exactly as serveControlConn does, and
// returns its terminal event plus accumulated stdout. It fails the test if
// serveExec does not finish, which is precisely the "gate welded shut" bug: the
// old implementation waited for pipe EOF before reaping, so a child that kept
// stdout open never returned.
func runOneExec(t *testing.T, req execRequest) (execEvent, []byte) {
	t.Helper()
	if !execActive.CompareAndSwap(false, true) {
		t.Fatal("single-exec gate was not released by a previous exec")
	}

	r, w, err := os.Pipe()
	if err != nil {
		execActive.Store(false)
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	type result struct {
		ev     execEvent
		stdout []byte
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		br := bufio.NewReader(r)
		var stdout []byte
		for {
			var ev execEvent
			if err := readMessage(br, &ev); err != nil {
				resCh <- result{err: err}
				return
			}
			if ev.Type == eventStdout {
				stdout = append(stdout, ev.Data...)
			}
			if ev.Type == eventExited {
				resCh <- result{ev: ev, stdout: stdout}
				return
			}
		}
	}()
	go func() {
		serveExec(w, req)
		execActive.Store(false)
	}()

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("read events: %v", res.err)
		}
		return res.ev, res.stdout
	case <-time.After(10 * time.Second):
		t.Fatal("serveExec did not finish within 10s; the single-exec gate is welded shut")
		return execEvent{}, nil
	}
}

// assertNextExecSucceeds proves the gate is usable again after a case that
// leaves a lingering writer behind.
func assertNextExecSucceeds(t *testing.T) {
	t.Helper()
	ev, out := runOneExec(t, execRequest{Type: typeExec, ID: "after", Argv: []string{"echo", "ok"}})
	if ev.ExitCode != 0 || ev.Busy {
		t.Fatalf("subsequent exec = %+v, want exit 0, not busy", ev)
	}
	if string(out) != "ok\n" {
		t.Fatalf("subsequent exec stdout = %q, want %q", out, "ok\n")
	}
}

// A background child that inherits stdout must not hold the gate, and neither
// must a background child under timeout_seconds 0 (no guest deadline).
func TestServeExecReleasesGateWhenBackgroundChildHoldsStdout(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}

	// The shell exits immediately; the backgrounded sleep keeps the write end
	// of the stdout pipe open.
	ev, _ := runOneExec(t, execRequest{
		Type: typeExec, ID: "bg",
		Argv:           []string{"sh", "-c", "sleep 20 &"},
		TimeoutSeconds: 0,
	})
	if ev.ExitCode != 0 {
		t.Fatalf("background exec = %+v, want exit 0", ev)
	}
	assertNextExecSucceeds(t)
}

// A setsid-escaped child leaves the process group, so even a group kill would
// miss it; it must still not keep the gate closed.
func TestServeExecReleasesGateForSetsidEscapedChild(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not found")
	}

	ev, _ := runOneExec(t, execRequest{
		Type: typeExec, ID: "sid",
		Argv:           []string{"sh", "-c", "setsid sleep 20 &"},
		TimeoutSeconds: 0,
	})
	if ev.ExitCode != 0 {
		t.Fatalf("setsid exec = %+v, want exit 0", ev)
	}
	assertNextExecSucceeds(t)
}

// socketPair returns a connected AF_UNIX socket pair as *os.File, so the real
// serveControlConn can be driven bidirectionally.
func socketPair(t *testing.T) (client, server *os.File) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	client = os.NewFile(uintptr(fds[0]), "client")
	server = os.NewFile(uintptr(fds[1]), "server")
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}

func handshake(t *testing.T, c *os.File, br *bufio.Reader) {
	t.Helper()
	if err := writeMessage(c, hello{Type: typeHello, Version: 1}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	var ready ready
	if err := readMessage(br, &ready); err != nil {
		t.Fatalf("read ready: %v", err)
	}
	if ready.Type != typeReady {
		t.Fatalf("ready = %+v", ready)
	}
}

func readEvent(t *testing.T, br *bufio.Reader, timeout time.Duration) execEvent {
	t.Helper()
	type result struct {
		ev  execEvent
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var ev execEvent
		err := readMessage(br, &ev)
		ch <- result{ev: ev, err: err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read event: %v", res.err)
		}
		return res.ev
	case <-time.After(timeout):
		t.Fatal("timed out reading an event")
		return execEvent{}
	}
}

// waitGateFree blocks until no exec holds the process-wide gate, so a test
// cannot observe a previous test's in-flight exec (the gate is global state).
func waitGateFree(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for execActive.Load() {
		if time.Now().After(deadline) {
			t.Fatal("single-exec gate was not released by a previous test")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A second exec on another connection while one is running is rejected with an
// explicit Busy signal; exit code 125 alone is not the signal.
func TestServeControlConnConcurrentExecIsBusy(t *testing.T) {
	waitGateFree(t)

	c1, s1 := socketPair(t)
	go serveControlConn(s1)
	br1 := bufio.NewReader(c1)
	handshake(t, c1, br1)

	if err := writeMessage(c1, execRequest{Type: typeExec, ID: "long", Argv: []string{"sleep", "2"}, TimeoutSeconds: 10}); err != nil {
		t.Fatalf("write long exec: %v", err)
	}
	if ev := readEvent(t, br1, 5*time.Second); ev.Type != eventStarted {
		t.Fatalf("first event = %+v, want started", ev)
	}

	c2, s2 := socketPair(t)
	go serveControlConn(s2)
	br2 := bufio.NewReader(c2)
	handshake(t, c2, br2)
	if err := writeMessage(c2, execRequest{Type: typeExec, ID: "probe", Argv: []string{"echo", "x"}}); err != nil {
		t.Fatalf("write probe exec: %v", err)
	}
	busy := readEvent(t, br2, 5*time.Second)
	if busy.Type != eventExited || !busy.Busy || busy.ExitCode != execBusyExitCode {
		t.Fatalf("busy reply = %+v, want exited, busy, exit %d", busy, execBusyExitCode)
	}
	if busy.ID != "probe" {
		t.Fatalf("busy reply id = %q, want probe", busy.ID)
	}
	// Drain the still-running sleep so the service is idle again.
	for {
		ev := readEvent(t, br1, 10*time.Second)
		if ev.Type == eventExited {
			break
		}
	}
	_ = c1.Close()
	_ = c2.Close()
}

// A cancel message must kill the running exec's process group so it does not run
// to completion, and the service must remain usable afterwards.
func TestServeControlConnHonoursCancel(t *testing.T) {
	waitGateFree(t)

	c, s := socketPair(t)
	go serveControlConn(s)
	br := bufio.NewReader(c)
	handshake(t, c, br)

	if err := writeMessage(c, execRequest{Type: typeExec, ID: "cancel-me", Argv: []string{"sleep", "30"}}); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	if ev := readEvent(t, br, 5*time.Second); ev.Type != eventStarted {
		t.Fatalf("first event = %+v, want started", ev)
	}
	if err := writeMessage(c, execRequest{Type: typeCancel, ID: "cancel-me"}); err != nil {
		t.Fatalf("write cancel: %v", err)
	}
	// The exec must terminate promptly rather than run its full 30s.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ev := readEvent(t, br, 6*time.Second)
		if ev.Type == eventExited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled exec did not produce a terminal event in time")
		}
	}

	// The service is still usable.
	if err := writeMessage(c, execRequest{Type: typeExec, ID: "after", Argv: []string{"echo", "ok"}}); err != nil {
		t.Fatalf("write after exec: %v", err)
	}
	for {
		ev := readEvent(t, br, 10*time.Second)
		if ev.Type == eventExited {
			if ev.ExitCode != 0 {
				t.Fatalf("exec after cancel = %+v, want exit 0", ev)
			}
			break
		}
	}
	_ = c.Close()
}
