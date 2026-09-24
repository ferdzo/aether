package main

import (
	"bufio"
	"os/exec"
	"testing"
	"time"
)

func readSignalAck(t *testing.T, br *bufio.Reader, timeout time.Duration) signalAck {
	t.Helper()
	type result struct {
		ack signalAck
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var ack signalAck
		err := readMessage(br, &ack)
		ch <- result{ack: ack, err: err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read signal ack: %v", res.err)
		}
		return res.ack
	case <-time.After(timeout):
		t.Fatal("timed out reading a signal ack")
		return signalAck{}
	}
}

// The whitelist must reject anything not on it, and no exec means no delivery.
func TestSignalRunningExecValidation(t *testing.T) {
	waitGateFree(t)

	for _, name := range []string{"SIGUSR1", "SIGSTOP", "9", ""} {
		if err := signalRunningExec(name); err == nil {
			t.Fatalf("signalRunningExec(%q) = nil, want an error", name)
		}
	}

	// No exec is running: even a whitelisted signal is rejected.
	setRunningExec(nil)
	if err := signalRunningExec("SIGTERM"); err == nil {
		t.Fatal("signalRunningExec with no exec = nil, want an error")
	}
}

// A SIGTERM delivered on a second control connection must reach the running
// exec's process group, terminate it promptly, and leave the service usable.
func TestServeControlConnDeliversSignal(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not found")
	}
	if _, err := exec.LookPath("echo"); err != nil {
		t.Skip("echo not found")
	}
	waitGateFree(t)

	c, s := socketPair(t)
	go serveControlConn(s)
	br := bufio.NewReader(c)
	handshake(t, c, br)

	if err := writeMessage(c, execRequest{Type: typeExec, ID: "long", Argv: []string{"sleep", "30"}}); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	started := readEvent(t, br, 5*time.Second)
	if started.Type != eventStarted {
		t.Fatalf("first event = %+v, want started", started)
	}
	if started.PID <= 0 {
		t.Fatalf("started event pid = %d, want > 0", started.PID)
	}

	// Signal on its own connection, exactly as the worker does.
	c2, s2 := socketPair(t)
	go serveControlConn(s2)
	br2 := bufio.NewReader(c2)
	handshake(t, c2, br2)
	if err := writeMessage(c2, execRequest{Type: typeSignal, ID: "long", Signal: "SIGTERM"}); err != nil {
		t.Fatalf("write signal: %v", err)
	}
	if ack := readSignalAck(t, br2, 5*time.Second); ack.Type != signalAckType || ack.Error != "" {
		t.Fatalf("signal ack = %+v, want clean %s", ack, signalAckType)
	}

	// The exec must terminate promptly rather than run its full 30s.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ev := readEvent(t, br, 6*time.Second)
		if ev.Type == eventExited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("signalled exec did not produce a terminal event in time")
		}
	}

	// The service is still usable.
	if err := writeMessage(c, execRequest{Type: typeExec, ID: "after", Argv: []string{"echo", "ok"}}); err != nil {
		t.Fatalf("write after exec: %v", err)
	}
	for {
		ev := readEvent(t, br, 10*time.Second)
		if ev.Type == eventExited {
			if ev.ExitCode != 0 || ev.Busy {
				t.Fatalf("exec after signal = %+v, want exit 0, not busy", ev)
			}
			break
		}
	}
	_ = c.Close()
	_ = c2.Close()
}

// An unsupported signal must be rejected with a clear ack error and must not
// disturb the running exec or the single-exec gate.
func TestServeControlConnRejectsInvalidSignal(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not found")
	}
	waitGateFree(t)

	c, s := socketPair(t)
	go serveControlConn(s)
	br := bufio.NewReader(c)
	handshake(t, c, br)
	if err := writeMessage(c, execRequest{Type: typeExec, ID: "long", Argv: []string{"sleep", "30"}}); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	if ev := readEvent(t, br, 5*time.Second); ev.Type != eventStarted {
		t.Fatalf("first event = %+v, want started", ev)
	}

	c2, s2 := socketPair(t)
	go serveControlConn(s2)
	br2 := bufio.NewReader(c2)
	handshake(t, c2, br2)
	if err := writeMessage(c2, execRequest{Type: typeSignal, ID: "long", Signal: "SIGUSR1"}); err != nil {
		t.Fatalf("write signal: %v", err)
	}
	ack := readSignalAck(t, br2, 5*time.Second)
	if ack.Error == "" {
		t.Fatalf("invalid signal ack = %+v, want a clear error", ack)
	}

	// The original exec is still alive and can still be signalled: prove it by
	// delivering SIGTERM now (which would fail with "no exec is running" if the
	// invalid signal had disturbed the gate).
	if err := writeMessage(c2, execRequest{Type: typeSignal, ID: "long", Signal: "SIGTERM"}); err != nil {
		t.Fatalf("write valid signal: %v", err)
	}
	if ack := readSignalAck(t, br2, 5*time.Second); ack.Error != "" {
		t.Fatalf("valid signal after invalid one = %+v, want clean ack", ack)
	}
	for {
		ev := readEvent(t, br, 6*time.Second)
		if ev.Type == eventExited {
			break
		}
	}
	_ = c.Close()
	_ = c2.Close()
}
