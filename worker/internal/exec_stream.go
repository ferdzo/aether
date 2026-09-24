package internal

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"aether/shared/id"
	"aether/shared/protocol"
)

// This file holds the per-exec record and the bounded, sequence-numbered event
// buffer that backs both the synchronous exec result and the live SSE stream.
//
// The exec goroutine is the only writer. It never touches an HTTP response:
// it appends to the buffer, which is bounded by both event count and total
// bytes. Readers (the SSE handlers, or a record lookup) take the buffer's lock
// and never block the writer. That is what makes a slow client unable to stall
// an exec: the client's handler reads independently, and if it falls far enough
// behind the oldest events are dropped and a loss marker is recorded.

const (
	// maxExecRecords bounds how many recent per-exec records an execution
	// retains. The most recent win; older records are evicted whole.
	maxExecRecords = 16

	// maxExecBufferEvents bounds the event count retained per exec, in addition
	// to the byte budget (protocol.MaxExecStreamBytes).
	maxExecBufferEvents = 1024
)

// Per-exec record states.
const (
	execStateRunning   = "running"
	execStateDone      = "done"
	execStateFailed    = "failed"
	execStateTimeout   = "timeout"
	execStateCancelled = "cancelled"
)

// Errors the control API maps to distinct status codes for exec-scoped calls.
var (
	errExecNotFound   = errors.New("exec not found")
	errExecNotRunning = errors.New("exec is not running")
	errSignalInvalid  = errors.New("invalid signal")
)

// execSignalWhitelist is the set of signals a caller may deliver. It mirrors
// the guest's whitelist; the worker rejects anything else before opening a
// guest connection.
var execSignalWhitelist = map[string]struct{}{
	"SIGTERM": {},
	"SIGINT":  {},
	"SIGHUP":  {},
	"SIGKILL": {},
}

func validExecSignal(s string) bool {
	_, ok := execSignalWhitelist[s]
	return ok
}

// newExecID mints a fresh per-exec id. It is a helper so call sites whose
// parameter is also named id do not shadow the id package.
func newExecID() string { return id.GenerateExecID() }

// bufferedExecEvent is one event plus its monotonically increasing sequence
// number. The sequence is the SSE "id" so a reconnecting client can resume.
type bufferedExecEvent struct {
	Seq   uint64             `json:"seq"`
	Event protocol.ExecEvent `json:"event"`
}

// execEventBuffer is a bounded append-only log of sequenced events with a
// condition variable for readers. Writers never block: when the log exceeds
// either bound the oldest events are dropped and a loss flag is set.
type execEventBuffer struct {
	mu        sync.Mutex
	cond      *sync.Cond
	events    []bufferedExecEvent
	nextSeq   uint64 // sequence that will be assigned to the next append
	minSeq    uint64 // smallest sequence still retained (== nextSeq when empty)
	bytes     int
	maxEvents int
	maxBytes  int
	lost      bool
	done      bool
}

func newExecEventBuffer() *execEventBuffer {
	b := &execEventBuffer{
		nextSeq:   1,
		minSeq:    1,
		maxEvents: maxExecBufferEvents,
		maxBytes:  protocol.MaxExecStreamBytes,
	}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// execEventSize approximates an event's retained footprint. Data dominates, so
// a small fixed overhead for the encoded envelope is enough for the byte bound.
func execEventSize(ev protocol.ExecEvent) int {
	return len(ev.Data) + len(ev.Error) + len(ev.ID) + len(ev.Type) + 64
}

// append assigns the next sequence number to ev and stores it, evicting the
// oldest events while either bound is exceeded. It always keeps the newest
// event even if that single event is larger than the byte budget.
func (b *execEventBuffer) append(ev protocol.ExecEvent) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	seq := b.nextSeq
	b.nextSeq++
	b.events = append(b.events, bufferedExecEvent{Seq: seq, Event: ev})
	b.bytes += execEventSize(ev)
	for len(b.events) > 1 && (len(b.events) > b.maxEvents || b.bytes > b.maxBytes) {
		b.dropOldestLocked()
	}
	b.cond.Broadcast()
	return seq
}

func (b *execEventBuffer) dropOldestLocked() {
	dropped := b.events[0]
	b.events = b.events[1:]
	b.bytes -= execEventSize(dropped.Event)
	if len(b.events) > 0 {
		b.minSeq = b.events[0].Seq
	} else {
		b.minSeq = b.nextSeq
	}
	b.lost = true
}

// finish marks the stream terminal and wakes every waiter. It is idempotent.
func (b *execEventBuffer) finish() {
	b.mu.Lock()
	b.done = true
	b.cond.Broadcast()
	b.mu.Unlock()
}

// sliceLocked returns retained events with Seq > afterSeq and whether any
// event after afterSeq was dropped. Callers hold b.mu.
func (b *execEventBuffer) sliceLocked(afterSeq uint64) ([]bufferedExecEvent, bool) {
	start := sort.Search(len(b.events), func(i int) bool { return b.events[i].Seq > afterSeq })
	out := make([]bufferedExecEvent, len(b.events)-start)
	copy(out, b.events[start:])
	lost := b.lost && afterSeq+1 < b.minSeq
	return out, lost
}

// wake broadcasts to every waiter without changing the stream state. It is
// used to unblock a reader whose client has gone away.
func (b *execEventBuffer) wake() {
	b.mu.Lock()
	b.cond.Broadcast()
	b.mu.Unlock()
}

// channelClosed reports whether ch is already closed. A nil channel is never
// closed, so a background context keeps a reader waiting as usual.
func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// waitAndRead blocks until there is an event after afterSeq, the stream is
// terminal, or cancel is closed, then returns the retained tail. A caller that
// was away long enough for its position to be dropped gets lost=true and must
// emit a loss marker before the returned events; floor is the oldest retained
// sequence, which the caller uses to advance past the gap so the marker is not
// repeated.
func (b *execEventBuffer) waitAndRead(afterSeq uint64, cancel <-chan struct{}) (events []bufferedExecEvent, lost, done bool, floor uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.nextSeq-1 <= afterSeq && !b.done && !channelClosed(cancel) {
		b.cond.Wait()
	}
	events, lost = b.sliceLocked(afterSeq)
	return events, lost, b.done, b.minSeq
}

// snapshot returns every retained event and the loss flag without blocking. It
// is used to render a finished (or running) record.
func (b *execEventBuffer) snapshot() ([]bufferedExecEvent, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sliceLocked(0)
}

// execRecord is one exec's host-side state: identity, lifecycle, and the
// buffered event log. It is created before the guest is contacted and finished
// after the result returns.
type execRecord struct {
	ID  string
	buf *execEventBuffer

	mu         sync.Mutex
	state      string
	pid        int
	startedAt  time.Time
	finishedAt time.Time
	exitCode   int
	timedOut   bool
	terminal   bool
}

func newExecRecordState(id string) *execRecord {
	return &execRecord{
		ID:        id,
		buf:       newExecEventBuffer(),
		state:     execStateRunning,
		startedAt: time.Now().UTC(),
	}
}

// observe records one live event. It is the exec-path sink callback.
func (r *execRecord) observe(ev protocol.ExecEvent) {
	r.buf.append(ev)
	switch ev.Type {
	case protocol.EventStarted:
		if ev.PID > 0 {
			r.mu.Lock()
			if r.pid == 0 {
				r.pid = ev.PID
			}
			r.mu.Unlock()
		}
	case protocol.EventExited:
		r.mu.Lock()
		r.terminal = true
		r.exitCode = ev.ExitCode
		r.timedOut = ev.TimedOut
		r.finishedAt = time.Now().UTC()
		switch {
		case ev.Busy:
			r.state = execStateFailed
		case ev.TimedOut:
			r.state = execStateTimeout
		default:
			r.state = execStateDone
		}
		r.mu.Unlock()
		r.buf.finish()
	}
}

// complete finalises the record after the exec path returns. If no terminal
// event was observed (a transport failure, a cancellation, or the guest dying
// mid-exec) it synthesises one so every consumer can end on an exited event.
func (r *execRecord) complete(ctx context.Context, res protocol.ExecResult, err error) {
	r.mu.Lock()
	if r.terminal {
		r.mu.Unlock()
		r.buf.finish()
		return
	}
	r.terminal = true
	r.finishedAt = time.Now().UTC()
	r.exitCode = res.ExitCode
	r.timedOut = res.TimedOut
	switch {
	case err != nil && ctx.Err() != nil:
		r.state = execStateCancelled
	case err != nil:
		r.state = execStateFailed
	case res.TimedOut:
		r.state = execStateTimeout
	default:
		r.state = execStateDone
	}
	exitCode := r.exitCode
	timedOut := r.timedOut
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	r.mu.Unlock()

	r.buf.append(protocol.ExecEvent{
		Type:     protocol.EventExited,
		ID:       r.ID,
		ExitCode: exitCode,
		TimedOut: timedOut,
		Error:    errMsg,
	})
	r.buf.finish()
}

func (r *execRecord) isRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == execStateRunning
}

// execRecordView is the JSON shape of GET /executions/{id}/exec/{exec_id}.
type execRecordView struct {
	ExecID     string              `json:"exec_id"`
	State      string              `json:"state"`
	PID        int                 `json:"pid"`
	StartedAt  time.Time           `json:"started_at"`
	FinishedAt time.Time           `json:"finished_at"`
	ExitCode   int                 `json:"exit_code"`
	TimedOut   bool                `json:"timed_out"`
	Lost       bool                `json:"lost"`
	Events     []bufferedExecEvent `json:"events"`
}

func (r *execRecord) view() execRecordView {
	events, lost := r.buf.snapshot()
	r.mu.Lock()
	defer r.mu.Unlock()
	return execRecordView{
		ExecID:     r.ID,
		State:      r.state,
		PID:        r.pid,
		StartedAt:  r.startedAt,
		FinishedAt: r.finishedAt,
		ExitCode:   r.exitCode,
		TimedOut:   r.timedOut,
		Lost:       lost,
		Events:     events,
	}
}

// addExecRecord stores rec, evicting the oldest records past maxExecRecords.
func (e *Execution) addExecRecord(rec *execRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.execRecords == nil {
		e.execRecords = make(map[string]*execRecord)
	}
	e.execRecords[rec.ID] = rec
	e.execRecordOrder = append(e.execRecordOrder, rec.ID)
	for len(e.execRecordOrder) > maxExecRecords {
		oldest := e.execRecordOrder[0]
		e.execRecordOrder = e.execRecordOrder[1:]
		delete(e.execRecords, oldest)
	}
}

func (e *Execution) getExecRecord(execID string) (*execRecord, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, ok := e.execRecords[execID]
	return rec, ok
}

// StartExecStream admits an exec, creates its record, and runs the exec on a
// background goroutine. It returns as soon as the gate is held and the record
// exists, so an SSE handler can attach and stream while the command runs. The
// gate is released when the exec path returns.
func (w *Worker) StartExecStream(ctx context.Context, id string, req protocol.ExecRequest) (*execRecord, error) {
	exec := w.lookupExecution(id)
	if exec == nil {
		return nil, errExecutionNotFound
	}
	if !exec.tryAcquire() {
		return nil, errExecutionBusy
	}
	rec := newExecRecordState(newExecID())
	exec.addExecRecord(rec)
	go func() {
		defer exec.release()
		res, err := execOnGuest(ctx, exec.vsockPath, id, req, rec.observe)
		rec.complete(ctx, res, err)
	}()
	return rec, nil
}

// ExecRecord returns the stored record for one exec of an execution.
func (w *Worker) ExecRecord(id, execID string) (execRecordView, error) {
	exec := w.lookupExecution(id)
	if exec == nil {
		return execRecordView{}, errExecutionNotFound
	}
	rec, ok := exec.getExecRecord(execID)
	if !ok {
		return execRecordView{}, errExecNotFound
	}
	return rec.view(), nil
}

// SignalExec delivers signalName to the running exec's process group. It
// rejects an unknown execution (404), an unknown exec (404), an exec that is no
// longer running (409) and an invalid signal (400); a transport failure is a
// plain error the control API maps to 502.
func (w *Worker) SignalExec(id, execID, signalName string) error {
	if !validExecSignal(signalName) {
		return errSignalInvalid
	}
	exec := w.lookupExecution(id)
	if exec == nil {
		return errExecutionNotFound
	}
	rec, ok := exec.getExecRecord(execID)
	if !ok {
		return errExecNotFound
	}
	if !rec.isRunning() {
		return errExecNotRunning
	}
	return signalOnGuest(exec.vsockPath, execID, signalName)
}
