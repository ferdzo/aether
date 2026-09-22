package internal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"aether/shared/protocol"
)

// startFakeGuest listens on a unix socket and serves the Firecracker
// CONNECT/OK + Hello/Ready handshake, then runs respond for the single exec it
// receives. It returns the socket path.
func startFakeGuest(t *testing.T, respond func(conn net.Conn, req protocol.ExecRequest)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fakeguest")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "vsock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		if _, err := br.ReadString('\n'); err != nil { // CONNECT <port>
			return
		}
		if _, err := fmt.Fprintf(conn, "OK 1\n"); err != nil {
			return
		}
		var h protocol.Hello
		if err := protocol.ReadMessage(br, &h); err != nil {
			return
		}
		if err := protocol.WriteMessage(conn, protocol.Ready{Type: protocol.TypeReady}); err != nil {
			return
		}
		var req protocol.ExecRequest
		if err := protocol.ReadMessage(br, &req); err != nil {
			return
		}
		respond(conn, req)
	}()
	return path
}

// An explicit Busy event maps to errGuestBusy.
func TestExecClientBusyEventIsBusyError(t *testing.T) {
	path := startFakeGuest(t, func(conn net.Conn, req protocol.ExecRequest) {
		_ = protocol.WriteMessage(conn, protocol.ExecEvent{
			Type: protocol.EventExited, ID: req.ID, ExitCode: guestBusyExitCode,
			Busy: true, Error: "exec service busy",
		})
	})

	c, err := dialExecClient(path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()
	if _, err := c.exec(context.Background(), "e1", protocol.ExecRequest{Argv: []string{"true"}}); !errors.Is(err, errGuestBusy) {
		t.Fatalf("err = %v, want errGuestBusy", err)
	}
}

// A plain exit 125 is a normal result: it must not be mapped to busy and its
// output must be preserved.
func TestExecClientExit125IsNotBusy(t *testing.T) {
	path := startFakeGuest(t, func(conn net.Conn, req protocol.ExecRequest) {
		_ = protocol.WriteMessage(conn, protocol.ExecEvent{Type: protocol.EventStdout, ID: req.ID, Data: []byte("real output")})
		_ = protocol.WriteMessage(conn, protocol.ExecEvent{Type: protocol.EventExited, ID: req.ID, ExitCode: 125})
	})

	c, err := dialExecClient(path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()
	res, err := c.exec(context.Background(), "e1", protocol.ExecRequest{Argv: []string{"sh", "-c", "exit 125"}})
	if err != nil {
		t.Fatalf("err = %v, want nil for a plain exit 125", err)
	}
	if res.ExitCode != 125 || res.Busy {
		t.Fatalf("result = %+v, want exit 125, not busy", res)
	}
	if res.Stdout != "real output" {
		t.Fatalf("stdout = %q, want %q", res.Stdout, "real output")
	}
}
