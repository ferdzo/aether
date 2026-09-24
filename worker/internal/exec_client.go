package internal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"aether/shared/protocol"
)

// execServicePort is the guest vsock port the exec service listens on. It must
// stay in lockstep with execServicePort in init/exec_service.go.
const execServicePort = 5252

// guestBusyExitCode is the exit code the guest reports when it already has an
// exec running on another control connection (init/exec_service.go). It is not
// the busy signal: 125 is also a legitimate command exit status, so callers
// must key off ExecEvent.Busy (surfaced as ExecResult.Busy).
const guestBusyExitCode = 125

// execConnectTimeout bounds the Firecracker CONNECT acknowledgement.
const execConnectTimeout = 3 * time.Second

// execReadSlack is added to a guest-side timeout to bound the host read. The
// guest enforces the deadline itself; the slack only stops the host from
// waiting forever if the guest dies mid-exec.
const execReadSlack = 30 * time.Second

// errGuestBusy is returned when the guest rejects an exec because another
// command is running. It is distinguishable so the control API can return 409.
var errGuestBusy = errors.New("guest exec service busy")

// execClient is a host-side control connection to the guest exec service,
// reached over the Firecracker vsock Unix socket. One connection serves one
// exec: the guest's own single-exec gate still applies, and a fresh connection
// per exec keeps the client trivial (no demultiplexing, no reconnection state).
type execClient struct {
	conn net.Conn
	br   *bufio.Reader
}

// dialExecClient opens a host-initiated vsock connection, exactly as the guest
// service expects: connect to the device's Unix socket, announce the guest port
// with "CONNECT <port>\n", read Firecracker's "OK <hostside-port>\n"
// acknowledgement, then perform the Hello/Ready handshake. If nobody is
// listening in the guest, Firecracker closes the connection instead of
// acknowledging, so a successful handshake is the readiness signal.
func dialExecClient(udsPath string) (*execClient, error) {
	conn, err := net.DialTimeout("unix", udsPath, 2*time.Second)
	if err != nil {
		return nil, err
	}
	c := &execClient{conn: conn, br: bufio.NewReader(conn)}

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", execServicePort); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(execConnectTimeout))
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

func (c *execClient) close() error { return c.conn.Close() }

// exec sends one ExecRequest and drains its event stream into an ExecResult.
// A guest "busy" reply is mapped to errGuestBusy. It is execStream with no live
// callback; the synchronous result collection stays on the one streaming path.
func (c *execClient) exec(ctx context.Context, id string, req protocol.ExecRequest) (protocol.ExecResult, error) {
	return c.execStream(ctx, id, req, nil)
}

// execStream sends one ExecRequest and invokes onEvent for every event as it
// arrives, returning the aggregate result. A cancelled ctx (worker shutdown, or
// the caller's request going away) unblocks the read by expiring the connection
// deadline after asking the guest to cancel.
func (c *execClient) execStream(ctx context.Context, id string, req protocol.ExecRequest, onEvent func(protocol.ExecEvent)) (protocol.ExecResult, error) {
	req.Type = protocol.TypeExec
	req.ID = id
	if err := protocol.WriteMessage(c.conn, req); err != nil {
		return protocol.ExecResult{}, err
	}

	if req.TimeoutSeconds > 0 {
		_ = c.conn.SetReadDeadline(time.Now().Add(time.Duration(req.TimeoutSeconds)*time.Second + execReadSlack))
	}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			// Ask the guest to abandon the exec (it kills the process group),
			// then unblock the read. An abandoned exec must not run to
			// completion in the guest.
			_ = protocol.WriteMessage(c.conn, protocol.ExecRequest{Type: protocol.TypeCancel, ID: id})
			_ = c.conn.SetDeadline(time.Now())
		case <-stop:
		}
	}()

	res, err := protocol.StreamExec(c.br, id, onEvent)
	_ = c.conn.SetReadDeadline(time.Time{})
	if err != nil {
		return res, err
	}
	// Busy is signalled explicitly by the guest. Exit code 125 alone is not
	// busy: `sh -c 'exit 125'` is a normal result and its output is real.
	if res.Busy {
		return res, fmt.Errorf("%w: %s", errGuestBusy, res.Error)
	}
	return res, nil
}

// runExecOnGuest runs one command against the guest service behind udsPath,
// opening and closing its own connection. This is the production execOnGuest
// seam (see execution.go). onEvent, when non-nil, observes each event live.
func runExecOnGuest(ctx context.Context, udsPath, id string, req protocol.ExecRequest, onEvent func(protocol.ExecEvent)) (protocol.ExecResult, error) {
	c, err := dialExecClient(udsPath)
	if err != nil {
		return protocol.ExecResult{}, err
	}
	defer c.close()
	return c.execStream(ctx, id, req, onEvent)
}

// runSignalOnGuest delivers a signal to the running exec's process group by
// opening a short-lived control connection, sending a SignalRequest and reading
// the guest's ack. Signals ride their own connection so they never race the
// exec's event stream.
func runSignalOnGuest(udsPath, id, signal string) error {
	c, err := dialExecClient(udsPath)
	if err != nil {
		return err
	}
	defer c.close()

	if err := protocol.WriteMessage(c.conn, protocol.SignalRequest{
		Type:   protocol.TypeSignal,
		ID:     id,
		Signal: signal,
	}); err != nil {
		return err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(execConnectTimeout + execReadSlack))
	var ack protocol.SignalAck
	if err := protocol.ReadMessage(c.br, &ack); err != nil {
		return err
	}
	if ack.Type != protocol.TypeSignalAck {
		return fmt.Errorf("unexpected signal ack: %+v", ack)
	}
	if ack.Error != "" {
		return fmt.Errorf("%s", ack.Error)
	}
	return nil
}

// waitExecHandshake retries the full connect+Hello/Ready handshake until it
// succeeds, the timeout elapses or ctx is cancelled. It is the execution
// readiness gate: there is no HTTP probe for a vsock-only guest.
func waitExecHandshake(ctx context.Context, udsPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("exec service not reachable over vsock within %v: %w", timeout, lastErr)
		}
		c, err := dialExecClient(udsPath)
		if err == nil {
			_ = c.close()
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// shutdownExecGuest asks the guest to flush and reset, best-effort. Destroy
// does not depend on it: instance.Stop() is the real teardown.
func shutdownExecGuest(udsPath string) error {
	c, err := dialExecClient(udsPath)
	if err != nil {
		return err
	}
	defer c.close()
	return protocol.WriteMessage(c.conn, protocol.Shutdown{Type: protocol.TypeShutdown})
}
