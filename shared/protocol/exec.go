package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// maxMessageBytes bounds one newline-delimited protocol message. A peer that
// sends an unterminated line must not be able to grow the heap without bound;
// the cap is far above any legitimate message (requests are small and output is
// chunked), so only a buggy or hostile peer can hit it.
const maxMessageBytes = 4 << 20 // 4 MiB

// MaxExecStreamBytes bounds how much output CollectExec retains per stream
// (stdout and stderr separately) on the host side. The guest already caps each
// stream at 1 MiB (see maxStreamBytes in init/exec_service.go); this host-side
// cap is a second line of defence so a buggy or hostile guest cannot make the
// worker allocate without bound.
const MaxExecStreamBytes = 4 << 20 // 4 MiB per stream

// ErrExecOutputTooLarge is returned by CollectExec when a guest exceeds
// MaxExecStreamBytes on one stream. It is distinct so callers can map it to a
// guest failure rather than a transport error.
var ErrExecOutputTooLarge = errors.New("exec output exceeded host cap")

// The exec protocol is newline-delimited JSON: one JSON object per line, in
// both directions. It is deliberately small and streaming-shaped so richer
// streaming (stdin, attach, more event kinds) can be layered on later without
// changing the framing or the handshake.
//
// Messages are exchanged over a Full-duplex byte stream. The host is the
// client and the guest is the server:
//
//	host -> guest : Hello, then ExecRequest* / Shutdown
//	guest -> host : Ready, then ExecEvent*
//
// Every message carries a "type" discriminator. ExecEvent.Type is the event
// kind (started|stdout|stderr|exited), which doubles as the discriminator for
// guest->host traffic.
const (
	TypeHello    = "hello"
	TypeReady    = "ready"
	TypeExec     = "exec"
	TypeShutdown = "shutdown"
	// TypeCancel asks the guest to abandon a running exec. It may arrive on the
	// same control connection while the exec is still streaming events; the
	// guest kills the exec's process group and reports a terminal event.
	TypeCancel = "cancel"
	// TypeSignal asks the guest to deliver a signal to the running exec's
	// process group. It is sent on its own control connection (the exec itself
	// owns the connection it streams on) so the guest can acknowledge it
	// without racing the event stream.
	TypeSignal = "signal"
	// TypeSignalAck is the guest's reply to TypeSignal.
	TypeSignalAck = "signal_ack"

	EventStarted = "started"
	EventStdout  = "stdout"
	EventStderr  = "stderr"
	EventExited  = "exited"
)

// Hello is the first message the host sends on a control connection. Version
// lets a guest reject an incompatible peer; only version 1 exists today.
type Hello struct {
	Type    string `json:"type"`
	Version int    `json:"version,omitempty"`
}

// Ready is the guest's reply to Hello. A non-empty Error means the handshake
// failed and the host should close the connection.
type Ready struct {
	Type  string `json:"type"`
	Error string `json:"error,omitempty"`
}

// ExecRequest asks the guest service to run one command. Argv is used
// verbatim: the service never wraps it in "sh -c". An empty ID is allowed;
// non-empty IDs let a caller correlate events when execs are multiplexed.
type ExecRequest struct {
	Type           string            `json:"type"`
	ID             string            `json:"id,omitempty"`
	Argv           []string          `json:"argv"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

// Shutdown asks the guest to flush and reset (see the guest implementation:
// a reset, not a poweroff, is what makes Firecracker exit on x86).
type Shutdown struct {
	Type string `json:"type"`
}

// ExecEvent is one event in a single exec's stream. Type is the event kind:
// started, stdout, stderr or exited. Data carries output for stdout/stderr and
// may be split across events at arbitrary boundaries. ExitCode and TimedOut
// are only meaningful on the terminal exited event. Error is set when the
// service could not run the command at all (for example it was busy).
//
// Data is a []byte rather than a string so the wire representation is
// byte-exact: encoding/json base64-encodes byte slices and decodes them back
// without loss, whereas a JSON string replaces invalid UTF-8 and cannot carry a
// chunk boundary that splits a multi-byte rune. Callers that want text can
// convert with string(ev.Data); callers that need exact bytes use it directly.
//
// Busy is set on the terminal event when the guest rejected the exec because
// another command was already running. It is the authoritative busy signal;
// callers must not infer busy from ExitCode (125 is also a legitimate command
// exit status).
//
// PID is set on the started event: it is the guest-side process-group id of the
// running command. It is informational (used by the per-exec record) and is 0
// on every other event.
type ExecEvent struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	Data     []byte `json:"data,omitempty"`
	PID      int    `json:"pid,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Busy     bool   `json:"busy,omitempty"`
	Error    string `json:"error,omitempty"`
}

// SignalRequest asks the guest to deliver Signal to the process group of the
// currently running exec. ID correlates the request with an exec but the guest
// routes by its own single-exec gate; the ack reports whether delivery
// succeeded.
type SignalRequest struct {
	Type   string `json:"type"`
	ID     string `json:"id,omitempty"`
	Signal string `json:"signal"`
}

// SignalAck is the guest's reply to a SignalRequest. A non-empty Error means
// the signal was not delivered (unknown name, or no exec running).
type SignalAck struct {
	Type  string `json:"type"`
	Error string `json:"error,omitempty"`
}

// WriteMessage encodes v as a single JSON line. json.Marshal never emits a
// literal newline, so the newline terminator is unambiguous.
func WriteMessage(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// ReadMessage reads exactly one newline-terminated JSON object from r into v.
// It buffers across the bytes of a single message, so a message may be split
// across any number of Reads and multiple messages may arrive in one Read
// without being lost. It reads one byte at a time from r, so a caller wrapping
// a socket should pass a buffered reader (for example
// bufio.NewReader(conn)) so refills happen in bulk; byte-at-a-time costs are
// then served from that buffer's memory.
//
// It returns io.EOF when r is exhausted before any byte of a new message is
// seen, so callers can detect a cleanly closed connection.
func ReadMessage(r io.Reader, v any) error {
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
				break // tolerate a final line with no trailing newline
			}
			return err
		}
	}
	if len(line) == 0 {
		return fmt.Errorf("exec protocol: empty message")
	}
	return json.Unmarshal(line, v)
}

// ExecResult is the aggregate outcome of a single exec, assembled from its
// event stream. Stdout and Stderr are raw bytes held in strings (a Go string
// can hold arbitrary bytes); converting them to text may substitute invalid
// UTF-8, so callers that need exact bytes must consume the event stream rather
// than this aggregate.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
	Busy     bool
	Error    string
}

// StreamExec drains ExecEvent messages for id from r until the terminal
// "exited" event, appending stdout and stderr separately. Events tagged with a
// different non-empty id are ignored, so the helper stays correct if the
// protocol later multiplexes execs on one connection. onEvent, when non-nil, is
// invoked for every event belonging to id before it is folded into the
// aggregate; it is the hook a streaming caller uses to observe events live
// without duplicating the read loop.
//
// It returns whatever it collected together with any read error; io.EOF before
// an exited event means the connection closed early.
func StreamExec(r io.Reader, id string, onEvent func(ExecEvent)) (ExecResult, error) {
	var res ExecResult
	var stdout, stderr bytes.Buffer
	over := false
	for {
		var ev ExecEvent
		if err := ReadMessage(r, &ev); err != nil {
			res.Stdout = stdout.String()
			res.Stderr = stderr.String()
			return res, err
		}
		if id != "" && ev.ID != "" && ev.ID != id {
			continue
		}
		if onEvent != nil {
			onEvent(ev)
		}
		switch ev.Type {
		case EventStdout:
			if stdout.Len()+len(ev.Data) > MaxExecStreamBytes {
				over = true
			} else {
				stdout.Write(ev.Data)
			}
		case EventStderr:
			if stderr.Len()+len(ev.Data) > MaxExecStreamBytes {
				over = true
			} else {
				stderr.Write(ev.Data)
			}
		case EventExited:
			res.Stdout = stdout.String()
			res.Stderr = stderr.String()
			res.ExitCode = ev.ExitCode
			res.TimedOut = ev.TimedOut
			res.Busy = ev.Busy
			res.Error = ev.Error
			if over {
				return res, ErrExecOutputTooLarge
			}
			return res, nil
		}
	}
}

// CollectExec drains the event stream into an aggregate result. It is
// StreamExec with no live callback, kept as the simple synchronous entry point.
func CollectExec(r io.Reader, id string) (ExecResult, error) {
	return StreamExec(r, id, nil)
}
