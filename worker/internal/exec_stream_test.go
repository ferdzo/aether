package internal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aether/shared/protocol"
)

// newControlRequest builds a request with a body but no auth, for tests that
// need to set their own headers.
func newControlRequest(method, path, body string) *http.Request {
	return httptest.NewRequest(method, path, strings.NewReader(body))
}

func newResponseRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func timeoutAfter() <-chan time.Time { return time.After(2 * time.Second) }

// withSignalOnGuest overrides signal delivery so tests do not need a VM.
func withSignalOnGuest(t *testing.T, fn func(udsPath, id, signal string) error) {
	t.Helper()
	prev := signalOnGuest
	if fn != nil {
		signalOnGuest = fn
	}
	t.Cleanup(func() { signalOnGuest = prev })
}

func latestExecID(t *testing.T, exec *Execution) string {
	t.Helper()
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.execRecordOrder) == 0 {
		t.Fatal("no exec records")
	}
	return exec.execRecordOrder[len(exec.execRecordOrder)-1]
}

// A successful exec must produce a done record with the guest pid, exit code,
// and every streamed event in sequence order.
func TestExecExecutionRecordsLifecycle(t *testing.T) {
	w := newJobWorker(t)
	withExecOnGuest(t, func(_ context.Context, _ string, _ string, _ protocol.ExecRequest, onEvent func(protocol.ExecEvent)) (protocol.ExecResult, error) {
		onEvent(protocol.ExecEvent{Type: protocol.EventStarted, ID: "e1", PID: 4242})
		onEvent(protocol.ExecEvent{Type: protocol.EventStdout, ID: "e1", Data: []byte("hi\n")})
		onEvent(protocol.ExecEvent{Type: protocol.EventExited, ID: "e1", ExitCode: 7})
		return protocol.ExecResult{ExitCode: 7, Stdout: "hi\n"}, nil
	})
	registerTestExecution(w, "e1")

	if _, err := w.ExecExecution(context.Background(), "e1", protocol.ExecRequest{Argv: []string{"true"}}); err != nil {
		t.Fatalf("ExecExecution = %v", err)
	}

	exec := w.lookupExecution("e1")
	execID := latestExecID(t, exec)
	view, err := w.ExecRecord("e1", execID)
	if err != nil {
		t.Fatalf("ExecRecord = %v", err)
	}
	if view.State != execStateDone {
		t.Fatalf("state = %q, want done", view.State)
	}
	if view.ExitCode != 7 {
		t.Fatalf("exit_code = %d, want 7", view.ExitCode)
	}
	if view.PID != 4242 {
		t.Fatalf("pid = %d, want 4242", view.PID)
	}
	if view.StartedAt.IsZero() || view.FinishedAt.IsZero() {
		t.Fatalf("timestamps not recorded: %+v", view)
	}
	if len(view.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(view.Events))
	}
	for i, be := range view.Events {
		if be.Seq != uint64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, be.Seq, i+1)
		}
	}
	if view.Events[0].Event.Type != protocol.EventStarted || view.Events[2].Event.Type != protocol.EventExited {
		t.Fatalf("event order = %v", view.Events)
	}
}

// A transport failure with no terminal event must still leave a terminal failed
// record (with a synthesised exited event) and the gate must be released.
func TestExecExecutionRecordsFailureSynthesizesTerminal(t *testing.T) {
	w := newJobWorker(t)
	withExecOnGuest(t, func(_ context.Context, _ string, _ string, _ protocol.ExecRequest, _ func(protocol.ExecEvent)) (protocol.ExecResult, error) {
		return protocol.ExecResult{}, errors.New("connection reset")
	})
	registerTestExecution(w, "e2")

	if _, err := w.ExecExecution(context.Background(), "e2", protocol.ExecRequest{Argv: []string{"true"}}); err == nil {
		t.Fatal("ExecExecution should surface the transport error")
	}
	view, err := w.ExecRecord("e2", latestExecID(t, w.lookupExecution("e2")))
	if err != nil {
		t.Fatalf("ExecRecord = %v", err)
	}
	if view.State != execStateFailed {
		t.Fatalf("state = %q, want failed", view.State)
	}
	if len(view.Events) != 1 || view.Events[0].Event.Type != protocol.EventExited {
		t.Fatalf("terminal events = %+v, want one exited", view.Events)
	}
	if view.Events[0].Event.Error == "" {
		t.Fatal("synthesised terminal event has no error")
	}
	// The gate is free again: a later exec is admitted.
	withExecOnGuest(t, func(context.Context, string, string, protocol.ExecRequest, func(protocol.ExecEvent)) (protocol.ExecResult, error) {
		return protocol.ExecResult{ExitCode: 0}, nil
	})
	if _, err := w.ExecExecution(context.Background(), "e2", protocol.ExecRequest{Argv: []string{"true"}}); err != nil {
		t.Fatalf("exec after failure = %v, want nil", err)
	}
}

// The buffer must drop oldest events past the count bound and report the loss.
func TestExecEventBufferDropsOldestAndMarksLost(t *testing.T) {
	b := newExecEventBuffer()
	b.maxEvents = 3
	b.maxBytes = 1 << 20
	for i := 1; i <= 5; i++ {
		b.append(protocol.ExecEvent{Type: protocol.EventStdout, Data: []byte{byte(i)}})
	}
	events, lost := b.snapshot()
	if len(events) != 3 {
		t.Fatalf("retained %d events, want 3", len(events))
	}
	if events[0].Seq != 3 || events[2].Seq != 5 {
		t.Fatalf("retained seqs = [%d..%d], want [3..5]", events[0].Seq, events[2].Seq)
	}
	if !lost {
		t.Fatal("lost flag not set after dropping oldest")
	}

	// A reader from the beginning is told its position was dropped.
	got, lost, _, floor := b.waitAndRead(0, nil)
	if !lost || floor != 3 {
		t.Fatalf("resume from 0: lost=%v floor=%d, want lost=true floor=3", lost, floor)
	}
	if len(got) != 3 {
		t.Fatalf("resume from 0 returned %d events, want 3", len(got))
	}
	// A reader already at the retained floor is not considered lost.
	if _, lost, _, _ := b.waitAndRead(3, nil); lost {
		t.Fatal("reader at the retained floor should not be told it lost events")
	}
}

// The byte bound must also force eviction.
func TestExecEventBufferByteBound(t *testing.T) {
	b := newExecEventBuffer()
	b.maxBytes = 250
	b.maxEvents = 1000
	for i := 0; i < 5; i++ {
		b.append(protocol.ExecEvent{Type: protocol.EventStdout, Data: make([]byte, 40)})
	}
	events, lost := b.snapshot()
	if !lost {
		t.Fatal("byte bound did not evict")
	}
	if len(events) >= 5 {
		t.Fatalf("retained %d events, want fewer than 5", len(events))
	}
	if b.bytes > b.maxBytes {
		t.Fatalf("retained bytes = %d, want <= %d", b.bytes, b.maxBytes)
	}
}

// finish must wake a blocked reader.
func TestExecEventBufferFinishWakesReader(t *testing.T) {
	b := newExecEventBuffer()
	done := make(chan struct{})
	go func() {
		_, _, d, _ := b.waitAndRead(0, nil)
		if !d {
			t.Error("waitAndRead returned done=false after finish")
		}
		close(done)
	}()
	b.finish()
	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("finish did not wake the reader")
	}
}

// A cancelled reader (a disconnected SSE client) must unblock promptly without
// marking the stream done, so the buffer stays replayable for a reconnect.
func TestExecEventBufferCancelUnblocksReader(t *testing.T) {
	b := newExecEventBuffer()
	cancel := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_, _, d, _ := b.waitAndRead(0, cancel)
		if d {
			t.Error("waitAndRead returned done=true for a cancelled reader")
		}
		close(done)
	}()
	close(cancel)
	b.wake()
	select {
	case <-done:
	case <-timeoutAfter():
		t.Fatal("cancelled reader was not unblocked")
	}
	// The buffer is still usable: an event appended afterwards is readable.
	b.append(protocol.ExecEvent{Type: protocol.EventStdout, Data: []byte("x")})
	events, _, _, _ := b.waitAndRead(0, nil)
	if len(events) != 1 {
		t.Fatalf("buffer lost its events after a cancelled reader: %d", len(events))
	}
}

// GET /executions/{id}/exec/{exec_id} returns the record and 404 for unknown.
func TestControlGetExecRecord(t *testing.T) {
	w := newJobWorker(t)
	withExecOnGuest(t, func(_ context.Context, _ string, _ string, _ protocol.ExecRequest, onEvent func(protocol.ExecEvent)) (protocol.ExecResult, error) {
		onEvent(protocol.ExecEvent{Type: protocol.EventStarted, ID: "e1", PID: 9})
		onEvent(protocol.ExecEvent{Type: protocol.EventExited, ID: "e1"})
		return protocol.ExecResult{}, nil
	})
	registerTestExecution(w, "e1")
	if _, err := w.ExecExecution(context.Background(), "e1", protocol.ExecRequest{Argv: []string{"true"}}); err != nil {
		t.Fatalf("ExecExecution = %v", err)
	}
	execID := latestExecID(t, w.lookupExecution("e1"))

	h := NewControlServer(w, "").Handler()
	rec := controlDo(t, h, http.MethodGet, "/executions/e1/exec/"+execID, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var view execRecordView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("record not json: %v (%s)", err, rec.Body.String())
	}
	if view.ExecID != execID || view.PID != 9 || view.State != execStateDone {
		t.Fatalf("record = %+v", view)
	}

	if rec := controlDo(t, h, http.MethodGet, "/executions/e1/exec/exec-missing", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown exec status = %d, want 404", rec.Code)
	}
	if rec := controlDo(t, h, http.MethodGet, "/executions/missing/exec/"+execID, "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown execution status = %d, want 404", rec.Code)
	}
}

// SSE replay of a finished exec: frames carry event/id/data and the stream ends
// after the exited event.
func TestControlExecEventsReplaysFinished(t *testing.T) {
	w := newJobWorker(t)
	withExecOnGuest(t, func(_ context.Context, _ string, _ string, _ protocol.ExecRequest, onEvent func(protocol.ExecEvent)) (protocol.ExecResult, error) {
		onEvent(protocol.ExecEvent{Type: protocol.EventStarted, ID: "e1", PID: 9})
		onEvent(protocol.ExecEvent{Type: protocol.EventStdout, ID: "e1", Data: []byte("hi")})
		onEvent(protocol.ExecEvent{Type: protocol.EventExited, ID: "e1"})
		return protocol.ExecResult{}, nil
	})
	registerTestExecution(w, "e1")
	if _, err := w.ExecExecution(context.Background(), "e1", protocol.ExecRequest{Argv: []string{"true"}}); err != nil {
		t.Fatalf("ExecExecution = %v", err)
	}
	execID := latestExecID(t, w.lookupExecution("e1"))

	rec := controlDo(t, NewControlServer(w, "").Handler(), http.MethodGet, "/executions/e1/exec/"+execID+"/events", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"event: started\nid: 1\n", "event: stdout\nid: 2\n", "event: exited\nid: 3\n"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	// The stdout frame's data is the base64-encoded ExecEvent.
	if !strings.Contains(body, `"data":"aGk="`) {
		t.Fatalf("stdout data not byte-exact base64:\n%s", body)
	}
}

// Last-Event-ID must resume strictly after the given sequence.
func TestControlExecEventsResumesFromLastEventID(t *testing.T) {
	w := newJobWorker(t)
	withExecOnGuest(t, func(_ context.Context, _ string, _ string, _ protocol.ExecRequest, onEvent func(protocol.ExecEvent)) (protocol.ExecResult, error) {
		onEvent(protocol.ExecEvent{Type: protocol.EventStarted, ID: "e1"})
		onEvent(protocol.ExecEvent{Type: protocol.EventStdout, ID: "e1", Data: []byte("one")})
		onEvent(protocol.ExecEvent{Type: protocol.EventStdout, ID: "e1", Data: []byte("two")})
		onEvent(protocol.ExecEvent{Type: protocol.EventExited, ID: "e1"})
		return protocol.ExecResult{}, nil
	})
	registerTestExecution(w, "e1")
	if _, err := w.ExecExecution(context.Background(), "e1", protocol.ExecRequest{Argv: []string{"true"}}); err != nil {
		t.Fatalf("ExecExecution = %v", err)
	}
	execID := latestExecID(t, w.lookupExecution("e1"))

	req := newControlRequest(http.MethodGet, "/executions/e1/exec/"+execID+"/events", "")
	req.Header.Set("Last-Event-ID", "2")
	rec := newResponseRecorder()
	NewControlServer(w, "").Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "id: 1\n") || strings.Contains(body, "id: 2\n") {
		t.Fatalf("resume replayed already-seen events:\n%s", body)
	}
	if !strings.Contains(body, "event: stdout\nid: 3\n") || !strings.Contains(body, "event: exited\nid: 4\n") {
		t.Fatalf("resume did not return later events:\n%s", body)
	}
}

// When the requested position has been dropped, the stream emits a lost marker
// and continues with the retained events rather than failing.
func TestControlExecEventsEmitsLostMarker(t *testing.T) {
	w := newJobWorker(t)
	registerTestExecution(w, "e1")
	exec := w.lookupExecution("e1")
	rec := newExecRecordState("exec-lost")
	rec.buf.maxEvents = 2
	exec.addExecRecord(rec)
	for i := 1; i <= 5; i++ {
		rec.buf.append(protocol.ExecEvent{Type: protocol.EventStdout, ID: "e1", Data: []byte{byte('a' + i)}})
	}
	rec.buf.finish()

	req := newControlRequest(http.MethodGet, "/executions/e1/exec/exec-lost/events", "")
	req.Header.Set("Last-Event-ID", "1")
	rr := newResponseRecorder()
	NewControlServer(w, "").Handler().ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "event: lost\n") {
		t.Fatalf("no lost marker:\n%s", body)
	}
	// Retained events are seq 4 and 5.
	if !strings.Contains(body, "id: 4\n") || !strings.Contains(body, "id: 5\n") {
		t.Fatalf("retained events missing:\n%s", body)
	}
	if strings.Contains(body, "id: 2\n") || strings.Contains(body, "id: 3\n") {
		t.Fatalf("dropped events replayed:\n%s", body)
	}
}

// A streamed POST must return text/event-stream and the live frames.
func TestControlExecStreamTrueReturnsSSE(t *testing.T) {
	w := newJobWorker(t)
	withExecOnGuest(t, func(_ context.Context, _ string, _ string, _ protocol.ExecRequest, onEvent func(protocol.ExecEvent)) (protocol.ExecResult, error) {
		onEvent(protocol.ExecEvent{Type: protocol.EventStarted, ID: "e1", PID: 5})
		onEvent(protocol.ExecEvent{Type: protocol.EventStdout, ID: "e1", Data: []byte("tick")})
		onEvent(protocol.ExecEvent{Type: protocol.EventExited, ID: "e1"})
		return protocol.ExecResult{}, nil
	})
	registerTestExecution(w, "e1")

	rec := controlDo(t, NewControlServer(w, "").Handler(), http.MethodPost, "/executions/e1/exec",
		`{"argv":["echo","tick"],"stream":true}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "event: stdout\n") || !strings.Contains(body, "event: exited\n") {
		t.Fatalf("stream body = %q", body)
	}
}

// Signal routing: 200 delivered, 400 invalid, 404 unknown exec/execution,
// 409 not running, 502 transport.
func TestControlSignalMapping(t *testing.T) {
	w := newJobWorker(t)
	registerTestExecution(w, "s1")
	exec := w.lookupExecution("s1")
	rec := newExecRecordState("exec-1")
	exec.addExecRecord(rec)

	var gotPath, gotID, gotSignal string
	withSignalOnGuest(t, func(udsPath, id, signal string) error {
		gotPath, gotID, gotSignal = udsPath, id, signal
		return nil
	})
	h := NewControlServer(w, "").Handler()

	r := controlDo(t, h, http.MethodPost, "/executions/s1/exec/exec-1/signal", `{"signal":"SIGTERM"}`, "")
	if r.Code != http.StatusOK {
		t.Fatalf("deliver status = %d, want 200: %s", r.Code, r.Body.String())
	}
	if gotID != "exec-1" || gotSignal != "SIGTERM" || gotPath != "vsock" {
		t.Fatalf("guest saw path=%q id=%q signal=%q", gotPath, gotID, gotSignal)
	}

	if r := controlDo(t, h, http.MethodPost, "/executions/s1/exec/exec-1/signal", `{"signal":"SIGUSR1"}`, ""); r.Code != http.StatusBadRequest {
		t.Fatalf("invalid signal status = %d, want 400", r.Code)
	}
	if r := controlDo(t, h, http.MethodPost, "/executions/s1/exec/nope/signal", `{"signal":"SIGTERM"}`, ""); r.Code != http.StatusNotFound {
		t.Fatalf("unknown exec status = %d, want 404", r.Code)
	}
	if r := controlDo(t, h, http.MethodPost, "/executions/missing/exec/exec-1/signal", `{"signal":"SIGTERM"}`, ""); r.Code != http.StatusNotFound {
		t.Fatalf("unknown execution status = %d, want 404", r.Code)
	}

	// A finished exec is not running → 409.
	rec.complete(context.Background(), protocol.ExecResult{}, nil)
	if r := controlDo(t, h, http.MethodPost, "/executions/s1/exec/exec-1/signal", `{"signal":"SIGTERM"}`, ""); r.Code != http.StatusConflict {
		t.Fatalf("not-running status = %d, want 409", r.Code)
	}

	// Transport failure → 502.
	rec2 := newExecRecordState("exec-2")
	exec.addExecRecord(rec2)
	withSignalOnGuest(t, func(string, string, string) error { return errors.New("dial failed") })
	if r := controlDo(t, h, http.MethodPost, "/executions/s1/exec/exec-2/signal", `{"signal":"SIGKILL"}`, ""); r.Code != http.StatusBadGateway {
		t.Fatalf("transport failure status = %d, want 502", r.Code)
	}
}

// A concurrent streamed exec must be rejected before any SSE headers are sent.
func TestControlExecStreamTrueBusyIs409(t *testing.T) {
	w := newJobWorker(t)
	registerTestExecution(w, "b1")
	if !w.lookupExecution("b1").tryAcquire() {
		t.Fatal("could not acquire gate")
	}
	rec := controlDo(t, NewControlServer(w, "").Handler(), http.MethodPost, "/executions/b1/exec",
		`{"argv":["true"],"stream":true}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatal("busy response must not start an SSE stream")
	}
}
