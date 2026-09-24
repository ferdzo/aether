package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

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
type ExecEvent struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	Data     string `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Error    string `json:"error,omitempty"`
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
// event stream.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
	Error    string
}

// CollectExec drains ExecEvent messages for id from r until the terminal
// "exited" event, appending stdout and stderr separately. Events tagged with a
// different non-empty id are ignored, so the helper stays correct if the
// protocol later multiplexes execs on one connection. It returns whatever it
// collected together with any read error; io.EOF before an exited event means
// the connection closed early.
func CollectExec(r io.Reader, id string) (ExecResult, error) {
	var res ExecResult
	for {
		var ev ExecEvent
		if err := ReadMessage(r, &ev); err != nil {
			return res, err
		}
		if id != "" && ev.ID != "" && ev.ID != id {
			continue
		}
		switch ev.Type {
		case EventStdout:
			res.Stdout += ev.Data
		case EventStderr:
			res.Stderr += ev.Data
		case EventExited:
			res.ExitCode = ev.ExitCode
			res.TimedOut = ev.TimedOut
			res.Error = ev.Error
			return res, nil
		}
	}
}
