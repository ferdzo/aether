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
		{Type: EventStdout, ID: "1", Data: []byte("hello\n")},
		{Type: EventStderr, ID: "1", Data: []byte("oops\n")},
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
	_ = WriteMessage(&first, ExecEvent{Type: EventStdout, ID: "e1", Data: []byte("out-1")})
	_ = WriteMessage(&first, ExecEvent{Type: EventStderr, ID: "e1", Data: []byte("err-1")})

	var second bytes.Buffer
	_ = WriteMessage(&second, ExecEvent{Type: EventStdout, ID: "e1", Data: []byte("out-2")})
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

// Data must survive the wire byte-for-byte, including invalid UTF-8, and a
// valid multi-byte rune split across two events must not be corrupted.
func TestExecEventDataIsByteExact(t *testing.T) {
	invalid := []byte{0xff, 0xfe, 0x00, 0x41, 0x80}
	// "é" is 0xC3 0xA9; split it between two events.
	splitRune := []byte{'x', 0xc3, 0xa9, 'y'}

	var buf bytes.Buffer
	_ = WriteMessage(&buf, ExecEvent{Type: EventStdout, ID: "b", Data: invalid})
	_ = WriteMessage(&buf, ExecEvent{Type: EventStdout, ID: "b", Data: splitRune[:2]})
	_ = WriteMessage(&buf, ExecEvent{Type: EventStdout, ID: "b", Data: splitRune[2:]})
	_ = WriteMessage(&buf, ExecEvent{Type: EventExited, ID: "b", ExitCode: 0})

	got, err := CollectExec(bytes.NewReader(buf.Bytes()), "b")
	if err != nil {
		t.Fatalf("CollectExec: %v", err)
	}
	want := string(invalid) + string(splitRune)
	if got.Stdout != want {
		t.Fatalf("Stdout = %q (% x), want %q (% x)", got.Stdout, []byte(got.Stdout), want, []byte(want))
	}
}

// The busy flag must round-trip and is the only busy signal CollectExec reports.
func TestExecEventBusyRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteMessage(&buf, ExecEvent{Type: EventExited, ID: "z", ExitCode: 125, Busy: true, Error: "exec service busy"})

	got, err := CollectExec(bytes.NewReader(buf.Bytes()), "z")
	if err != nil {
		t.Fatalf("CollectExec: %v", err)
	}
	if !got.Busy {
		t.Fatalf("Busy = false, want true (ExitCode=%d)", got.ExitCode)
	}
}

// A plain exit 125 must not be reported as busy.
func TestExecEventExit125IsNotBusy(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteMessage(&buf, ExecEvent{Type: EventExited, ID: "z", ExitCode: 125})

	got, err := CollectExec(bytes.NewReader(buf.Bytes()), "z")
	if err != nil {
		t.Fatalf("CollectExec: %v", err)
	}
	if got.Busy {
		t.Fatal("Busy = true for a plain exit 125, want false")
	}
}

// CollectExec must stop retaining output past the host cap and say so.
func TestCollectExecCapsOutput(t *testing.T) {
	var buf bytes.Buffer
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = 'a'
	}
	for i := 0; i < 6; i++ { // 6 MiB, over the 4 MiB cap
		_ = WriteMessage(&buf, ExecEvent{Type: EventStdout, ID: "big", Data: chunk})
	}
	_ = WriteMessage(&buf, ExecEvent{Type: EventExited, ID: "big"})

	got, err := CollectExec(bytes.NewReader(buf.Bytes()), "big")
	if !errors.Is(err, ErrExecOutputTooLarge) {
		t.Fatalf("err = %v, want ErrExecOutputTooLarge", err)
	}
	if len(got.Stdout) > MaxExecStreamBytes {
		t.Fatalf("retained %d bytes, want <= %d", len(got.Stdout), MaxExecStreamBytes)
	}
}

// An unterminated line beyond the message cap must be rejected, not buffered.
func TestReadMessageRejectsOversizedLine(t *testing.T) {
	r := io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("a"), maxMessageBytes+1)), strings.NewReader(""))
	if err := ReadMessage(r, &Ready{}); err == nil {
		t.Fatal("oversized unterminated message must be rejected")
	}
}

// CollectExec must ignore events belonging to another exec.
func TestCollectExecIgnoresOtherIDs(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteMessage(&buf, ExecEvent{Type: EventStdout, ID: "other", Data: []byte("noise")})
	_ = WriteMessage(&buf, ExecEvent{Type: EventStdout, ID: "mine", Data: []byte("mine")})
	_ = WriteMessage(&buf, ExecEvent{Type: EventExited, ID: "mine", ExitCode: 0})

	got, err := CollectExec(bytes.NewReader(buf.Bytes()), "mine")
	if err != nil {
		t.Fatalf("CollectExec: %v", err)
	}
	if got.Stdout != "mine" {
		t.Fatalf("Stdout = %q, want %q", got.Stdout, "mine")
	}
}
