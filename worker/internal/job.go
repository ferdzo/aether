package internal

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"aether/shared/logger"
	"aether/shared/protocol"
)

const (
	// exitSentinelPrefix is the marker the guest supervisor prints as its final
	// stdout line: "AETHER_EXIT:<nonce>:<code>" or "AETHER_EXIT:<code>".
	exitSentinelPrefix = "AETHER_EXIT:"

	// jobModeProcess selects the guest supervisor (as opposed to the legacy
	// HTTP function path).
	jobModeProcess = "process"

	// jobTimeoutExitCode mirrors coreutils `timeout` and the guest supervisor,
	// which both report 124 when the deadline fires.
	jobTimeoutExitCode = 124

	// jobUnknownExitCode marks a failure whose real exit code could not be
	// determined because the VM exited without emitting a sentinel.
	jobUnknownExitCode = -1

	// defaultJobLogBytes bounds the in-memory console tail kept per job.
	defaultJobLogBytes = 64 * 1024

	// sentinelScanSlack is extra bytes retained beyond the marker length so a
	// sentinel split across writes is always reassembled before scanning.
	sentinelScanSlack = 32

	// jobStopGrace bounds how long Run waits for the VM process to exit after
	// Stop is called, so a wedged VMM cannot block the runner forever.
	jobStopGrace = 5 * time.Second

	// jobHeartbeatInterval is how often a running job's durable record is
	// refreshed. Heartbeats only *enable* stale detection — a record whose
	// HeartbeatAt stops advancing belongs to a dead worker. No reconciler
	// consumes them yet; this is deliberately just the observability half.
	jobHeartbeatInterval = 30 * time.Second
)

// jobLog is the bounded console sink for a job VM. It is an io.Writer that
// keeps only the last maxBytes written and scans every chunk for the exit
// sentinel.
//
// Write deliberately performs no I/O and touches no channel: it appends to an
// in-memory buffer under a short-lived mutex. This matters because os/exec
// drains the VM's stdout/stderr copy goroutines before Wait returns, so a
// blocking sink would stall VM shutdown (the known backpressure bug).
type jobLog struct {
	mu       sync.Mutex
	maxBytes int
	buf      []byte

	nonce    string
	scanTail []byte
	seen     bool
	code     int
}

// interface check: jobLog must be usable as the VM's Stdout/Stderr.
var _ io.Writer = (*jobLog)(nil)

func newJobLog(maxBytes int, nonce string) *jobLog {
	if maxBytes <= 0 {
		maxBytes = defaultJobLogBytes
	}
	return &jobLog{maxBytes: maxBytes, nonce: nonce}
}

func (l *jobLog) setNonce(nonce string) {
	l.mu.Lock()
	l.nonce = nonce
	l.mu.Unlock()
}

// Write implements io.Writer. Safe for concurrent use (the VM may pump stdout
// and stderr from separate goroutines) and never blocks on I/O.
func (l *jobLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Bounded ring: retain only the most recent maxBytes.
	l.buf = append(l.buf, p...)
	if len(l.buf) > l.maxBytes {
		l.buf = append(l.buf[:0], l.buf[len(l.buf)-l.maxBytes:]...)
	}

	// Incremental sentinel scan with enough overlap to reassemble a marker (and
	// its code) split across two writes.
	l.scanTail = append(l.scanTail, p...)
	l.scanLocked()
	limit := len(l.markerLocked()) + sentinelScanSlack
	if len(l.scanTail) > limit {
		l.scanTail = append(l.scanTail[:0], l.scanTail[len(l.scanTail)-limit:]...)
	}

	return len(p), nil
}

// Tail returns the captured console tail.
func (l *jobLog) Tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.buf)
}

// SentinelSeen reports whether a well-formed AETHER_EXIT sentinel was found.
func (l *jobLog) SentinelSeen() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen
}

// ExitCode returns the code carried by the last sentinel found. It is only
// meaningful when SentinelSeen reports true.
func (l *jobLog) ExitCode() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.code
}

func (l *jobLog) markerLocked() string {
	if l.nonce != "" {
		return exitSentinelPrefix + l.nonce + ":"
	}
	return exitSentinelPrefix
}

// scanLocked searches the retained window for the newest sentinel. It is
// idempotent: re-scanning only ever rediscovers an already-recorded sentinel,
// so a later occurrence still wins.
func (l *jobLog) scanLocked() {
	marker := []byte(l.markerLocked())
	idx := bytes.LastIndex(l.scanTail, marker)
	if idx < 0 {
		return
	}
	code, ok := parseSentinelCode(l.scanTail[idx+len(marker):])
	if !ok {
		return
	}
	l.seen = true
	l.code = code
}

// parseSentinelCode parses the decimal exit code immediately following a
// sentinel marker. It requires the digits to be terminated (the guest always
// ends the sentinel with a newline) so that a code split across two writes,
// e.g. "4" then "2", is not mistaken for the final value in the first write.
func parseSentinelCode(rest []byte) (int, bool) {
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 || end == len(rest) {
		return 0, false
	}
	code, err := strconv.Atoi(string(rest[:end]))
	if err != nil {
		return 0, false
	}
	return code, true
}

// jobOutcome is the pure input to classifyJob: everything the post-hoc
// classifier is allowed to look at.
type jobOutcome struct {
	SentinelSeen bool
	SentinelCode int
	TimedOut     bool
	Cancelled    bool
	WaitErr      error
}

// classifyJob is the pure post-hoc decision function. It never pre-sets state:
// the outcome is derived from what actually happened, with precedence
// sentinel > cancelled > timeout > crash.
//
//   - a sentinel always means the supervisor reported a real exit code -> done.
//     This is checked first so a job that finished just before an external
//     cancel is recorded as done, not cancelled;
//   - otherwise a cancel request -> cancelled (an explicit stop wins over a
//     deadline that may have fired in the same instant);
//   - otherwise a fired deadline -> timeout, 124;
//   - otherwise the VM exited without a sentinel -> failed.
func classifyJob(o jobOutcome) (state string, exitCode int, errMsg string) {
	switch {
	case o.SentinelSeen:
		return protocol.JobStateDone, o.SentinelCode, ""
	case o.Cancelled:
		return protocol.JobStateCancelled, jobUnknownExitCode, "job cancelled by request"
	case o.TimedOut:
		return protocol.JobStateTimeout, jobTimeoutExitCode, "job exceeded timeout"
	default:
		msg := "VM exited without an exit sentinel"
		if o.WaitErr != nil {
			msg = fmt.Sprintf("%s: %v", msg, o.WaitErr)
		}
		return protocol.JobStateFailed, jobUnknownExitCode, msg
	}
}

// JobRunnerConfig parameterises a JobRunner. JobRunner depends on neither
// *Instance nor any networking: Wait and Stop are injected so a real
// FirecrackerVM (or a hermetic fake) can drive it.
type JobRunnerConfig struct {
	JobID     string
	RequestID string
	WorkerID  string
	Nonce     string
	Timeout   time.Duration

	// WorkspacePath is the host path to the job's workspace image, if any. It
	// is copied onto the running and terminal records so callers can find the
	// persisted results after the VM is gone.
	WorkspacePath string

	// Log is the bounded console sink attached to the VM. When nil a default
	// sink is created; both are reachable via (*JobRunner).Log.
	Log *jobLog

	// Wait blocks until the VM exits. Required.
	Wait func() error
	// Stop kills/cleans up the VM when the deadline expires or the context is
	// cancelled. Optional.
	Stop func() error
	// Record persists the final record. Optional; a failure is logged and never
	// changes the run's outcome.
	Record func(protocol.JobRecord) error

	// HeartbeatInterval is how often the running record is refreshed while the
	// job runs. <= 0 disables heartbeats. It is injected so tests can shorten
	// it; production passes jobHeartbeatInterval.
	HeartbeatInterval time.Duration
	// Heartbeat persists a running record with a refreshed HeartbeatAt.
	// Optional and best-effort: a failure is logged and never changes the run's
	// outcome nor the stream ACK.
	Heartbeat func(protocol.JobRecord) error
}

// JobRunner runs one process job to completion and classifies its outcome.
type JobRunner struct {
	jobID     string
	requestID string
	workerID  string
	nonce     string
	timeout   time.Duration
	log       *jobLog

	workspacePath string

	wait func() error
	stop func() error

	record            func(protocol.JobRecord) error
	heartbeat         func(protocol.JobRecord) error
	heartbeatInterval time.Duration

	// cancelCh is closed exactly once by Cancel to request cancellation. A nil
	// channel (a runner built without NewJobRunner) is simply never ready, so
	// waitForExit degrades to its pre-cancellation behaviour.
	cancelCh   chan struct{}
	cancelOnce sync.Once

	// cleanup is invoked once after Run returns so the owning worker can
	// deregister the job. It is set by the worker; standalone runners leave it
	// nil. cleanupOnce makes the deregistration idempotent across the seam
	// wrapper's defer and any explicit caller.
	cleanup     func()
	cleanupOnce sync.Once
}

// NewJobRunner builds a runner. The provided sink's nonce is aligned with the
// config so the sentinel format matches the guest's.
func NewJobRunner(cfg JobRunnerConfig) *JobRunner {
	log := cfg.Log
	if log == nil {
		log = newJobLog(defaultJobLogBytes, cfg.Nonce)
	} else {
		log.setNonce(cfg.Nonce)
	}
	return &JobRunner{
		jobID:     cfg.JobID,
		requestID: cfg.RequestID,
		workerID:  cfg.WorkerID,
		nonce:     cfg.Nonce,
		timeout:   cfg.Timeout,
		log:       log,
		wait:      cfg.Wait,
		stop:      cfg.Stop,
		record:    cfg.Record,

		workspacePath: cfg.WorkspacePath,

		heartbeat:         cfg.Heartbeat,
		heartbeatInterval: cfg.HeartbeatInterval,

		cancelCh: make(chan struct{}),
	}
}

// Cancel requests cancellation of the running job. It is idempotent and safe to
// call from any goroutine, including after Run has returned (in which case it is
// a no-op because classification is post-hoc). Cancel only signals; the single
// terminal record is always written by Run.
func (r *JobRunner) Cancel() {
	if r.cancelCh == nil {
		return
	}
	r.cancelOnce.Do(func() { close(r.cancelCh) })
}

// cancelRequested reports whether Cancel has already been called, without
// blocking. It lets Run let an explicit cancel win over a deadline that fired
// in the same instant.
func (r *JobRunner) cancelRequested() bool {
	if r.cancelCh == nil {
		return false
	}
	select {
	case <-r.cancelCh:
		return true
	default:
		return false
	}
}

// runCleanup invokes the worker-registered cleanup at most once. It is called
// from the startJobRunner wrapper's deferred call, so it runs whether Run
// returns normally or panics.
func (r *JobRunner) runCleanup() {
	r.cleanupOnce.Do(func() {
		if r.cleanup != nil {
			r.cleanup()
		}
	})
}

// Log exposes the console sink, mainly so callers (and tests) can inspect the
// captured tail and the parsed sentinel.
func (r *JobRunner) Log() *jobLog {
	return r.log
}

// Run waits for the VM to exit (racing the optional timeout and the context),
// classifies the outcome from the console, and records it. It returns only
// after the record has been handed to the callback.
func (r *JobRunner) Run(ctx context.Context) protocol.JobRecord {
	if ctx == nil {
		ctx = context.Background()
	}

	started := time.Now().UTC()
	stopHeartbeat := r.startHeartbeat(ctx, started)
	waitErr, timedOut, cancelled := r.waitForExit(ctx)
	// Stop (and wait for) the heartbeat before writing the terminal record, so a
	// late heartbeat can never clobber it back to running.
	stopHeartbeat()

	// An explicit cancel beats a deadline that fired in the same instant. A
	// natural exit (including one carrying a sentinel) is left untouched, so a
	// job that finished before the cancel stays done/failed rather than being
	// rewritten to cancelled.
	if timedOut && r.cancelRequested() {
		cancelled = true
	}

	state, exitCode, errMsg := classifyJob(jobOutcome{
		SentinelSeen: r.log.SentinelSeen(),
		SentinelCode: r.log.ExitCode(),
		TimedOut:     timedOut,
		Cancelled:    cancelled,
		WaitErr:      waitErr,
	})

	finished := time.Now().UTC()
	rec := protocol.JobRecord{
		JobID:         r.jobID,
		RequestID:     r.requestID,
		Mode:          jobModeProcess,
		State:         state,
		ExitCode:      exitCode,
		WorkerID:      r.workerID,
		Error:         errMsg,
		StartedAt:     started,
		HeartbeatAt:   finished,
		FinishedAt:    finished,
		WorkspacePath: r.workspacePath,
	}

	if r.record != nil {
		if err := r.record(rec); err != nil {
			logger.Error("failed to record job outcome", "job_id", r.jobID, "state", state, "error", err)
		}
	}

	return rec
}

// startHeartbeat refreshes the running record on a ticker until the returned
// stop function is called. The stop function waits for the goroutine to exit,
// so no heartbeat write can land after the terminal record. Heartbeats are
// best-effort: a failed write is logged and never affects the job or its ACK.
//
// This only *enables* stale detection — a running record whose HeartbeatAt
// stops advancing is a dead worker's. No reconciler consumes it yet.
func (r *JobRunner) startHeartbeat(ctx context.Context, started time.Time) func() {
	if r.heartbeat == nil || r.heartbeatInterval <= 0 {
		return func() {}
	}

	ticker := time.NewTicker(r.heartbeatInterval)
	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				rec := protocol.JobRecord{
					JobID:         r.jobID,
					RequestID:     r.requestID,
					Mode:          jobModeProcess,
					State:         protocol.JobStateRunning,
					WorkerID:      r.workerID,
					StartedAt:     started,
					HeartbeatAt:   time.Now().UTC(),
					WorkspacePath: r.workspacePath,
				}
				if err := r.heartbeat(rec); err != nil {
					logger.Warn("failed to record job heartbeat", "job_id", r.jobID, "error", err)
				}
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

// waitForExit races Wait against the timeout, a cancel request and the context.
//
// A fired deadline, a cancel request or a cancelled context stops the VM and
// then waits a bounded grace period for Wait to return, so Run cannot hang on a
// wedged VMM. Exactly one of the branches is taken, but classification is
// post-hoc: a sentinel already on the console still wins (see Run/classifyJob).
func (r *JobRunner) waitForExit(ctx context.Context) (err error, timedOut bool, cancelled bool) {
	waitCh := make(chan error, 1)
	go func() {
		var e error
		if r.wait != nil {
			e = r.wait()
		}
		waitCh <- e
	}()

	var timeoutC <-chan time.Time
	if r.timeout > 0 {
		timer := time.NewTimer(r.timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}

	select {
	case err = <-waitCh:
		return err, false, false
	case <-timeoutC:
		r.forceStop()
		r.drain(waitCh)
		return nil, true, false
	case <-r.cancelCh:
		r.forceStop()
		r.drain(waitCh)
		return nil, false, true
	case <-ctx.Done():
		r.forceStop()
		r.drain(waitCh)
		return ctx.Err(), false, false
	}
}

func (r *JobRunner) forceStop() {
	if r.stop == nil {
		return
	}
	if err := r.stop(); err != nil {
		logger.Warn("failed to stop job VM", "job_id", r.jobID, "error", err)
	}
}

func (r *JobRunner) drain(waitCh <-chan error) {
	select {
	case <-waitCh:
	case <-time.After(jobStopGrace):
		logger.Warn("job VM did not exit after stop", "job_id", r.jobID)
	}
}
