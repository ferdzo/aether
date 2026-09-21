package protocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// chunkReader returns its data in fixed-size chunks, emulating a stream whose
// message boundaries do not line up with Read boundaries.
type chunkReader struct {
	data []byte
	n    int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := c.n
	if n > len(c.data) {
		n = len(c.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

// A message split across many Reads must be reassembled.
func TestReadMessageSplitAcrossReads(t *testing.T) {
	want := ExecRequest{
		Type:           TypeExec,
		ID:             "abc",
		Argv:           []string{"sh", "-c", "echo hello"},
		Cwd:            "/workspace",
		Env:            map[string]string{"FOO": "bar"},
		TimeoutSeconds: 7,
	}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, want); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	wire := buf.Bytes()

	for _, chunk := range []int{1, 2, 3, 7, len(wire)} {
		t.Run(string(rune('0'+chunk%10))+"-byte chunks", func(t *testing.T) {
			r := &chunkReader{data: append([]byte(nil), wire...), n: chunk}
			var got ExecRequest
			if err := ReadMessage(r, &got); err != nil {
				t.Fatalf("ReadMessage (chunk=%d): %v", chunk, err)
			}
			if got.ID != want.ID || got.Cwd != want.Cwd || got.TimeoutSeconds != want.TimeoutSeconds {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			if strings.Join(got.Argv, "\x00") != strings.Join(want.Argv, "\x00") {
				t.Fatalf("argv = %v, want %v", got.Argv, want.Argv)
			}
			if got.Env["FOO"] != "bar" {
				t.Fatalf("env = %v, want FOO=bar", got.Env)
			}
		})
	}
}

// Several messages arriving in a single Read must all be decoded in order.
func TestReadMessageMultipleInOneRead(t *testing.T) {
	var buf bytes.Buffer
	for _, ev := range []ExecEvent{
		{Type: EventStarted, ID: "1"},
		{Type: EventStdout, ID: "1", Data: "hello\n"},
		{Type: EventStderr, ID: "1", Data: "oops\n"},
		{Type: EventExited, ID: "1", ExitCode: 3, TimedOut: true},
	} {
		if err := WriteMessage(&buf, ev); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	r := bytes.NewReader(buf.Bytes())
	wantTypes := []string{EventStarted, EventStdout, EventStderr, EventExited}
	for i, wantType := range wantTypes {
		var ev ExecEvent
		if err := ReadMessage(r, &ev); err != nil {
			t.Fatalf("ReadMessage #%d: %v", i, err)
		}
		if ev.Type != wantType {
			t.Fatalf("event #%d type = %q, want %q", i, ev.Type, wantType)
		}
	}
	if err := ReadMessage(r, &ExecEvent{}); !errors.Is(err, io.EOF) {
		t.Fatalf("after last message: err = %v, want io.EOF", err)
	}
}

// A final message without a trailing newline is still decoded.
func TestReadMessageFinalLineWithoutNewline(t *testing.T) {
	r := strings.NewReader(`{"type":"ready"}`)
	var ready Ready
	if err := ReadMessage(r, &ready); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if ready.Type != TypeReady {
		t.Fatalf("type = %q, want %q", ready.Type, TypeReady)
	}
}

func TestReadMessageEOF(t *testing.T) {
	if err := ReadMessage(strings.NewReader(""), &Ready{}); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

// CollectExec must keep stdout and stderr separate and stop at exited.
func TestCollectExecSeparatesStreams(t *testing.T) {
	// Two messages in one write, and one split write, to exercise framing and
	// the collector together.
	first := bytes.Buffer{}
	_ = WriteMessage(&first, ExecEvent{Type: EventStarted, ID: "e1"})
	_ = WriteMessage(&first, ExecEvent{Type: EventStdout, ID: "e1", Data: "out-1"})
	_ = WriteMessage(&first, ExecEvent{Type: EventStderr, ID: "e1", Data: "err-1"})

	var second bytes.Buffer
	_ = WriteMessage(&second, ExecEvent{Type: EventStdout, ID: "e1", Data: "out-2"})
	_ = WriteMessage(&second, ExecEvent{Type: EventExited, ID: "e1", ExitCode: 42, TimedOut: true})

	r := &chunkReader{data: append(append([]byte(nil), first.Bytes()...), second.Bytes()...), n: 5}
	got, err := CollectExec(r, "e1")
	if err != nil {
		t.Fatalf("CollectExec: %v", err)
	}
	if got.Stdout != "out-1out-2" {
		t.Fatalf("Stdout = %q, want %q", got.Stdout, "out-1out-2")
	}
	if got.Stderr != "err-1" {
		t.Fatalf("Stderr = %q, want %q", got.Stderr, "err-1")
	}
	if got.ExitCode != 42 || !got.TimedOut {
		t.Fatalf("result = %+v, want ExitCode=42 TimedOut=true", got)
	}
}

// CollectExec must ignore events belonging to another exec.
func TestCollectExecIgnoresOtherIDs(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteMessage(&buf, ExecEvent{Type: EventStdout, ID: "other", Data: "noise"})
	_ = WriteMessage(&buf, ExecEvent{Type: EventStdout, ID: "mine", Data: "mine"})
	_ = WriteMessage(&buf, ExecEvent{Type: EventExited, ID: "mine", ExitCode: 0})

	got, err := CollectExec(bytes.NewReader(buf.Bytes()), "mine")
	if err != nil {
		t.Fatalf("CollectExec: %v", err)
	}
	if got.Stdout != "mine" {
		t.Fatalf("Stdout = %q, want %q", got.Stdout, "mine")
	}
}
