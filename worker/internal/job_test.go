package internal

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"aether/shared/protocol"
)

func TestJobLogKeepsOnlyTail(t *testing.T) {
	l := newJobLog(8, "")
	if _, err := l.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := l.Tail(); got != "89ABCDEF" {
		t.Fatalf("tail = %q, want %q", got, "89ABCDEF")
	}

	// A second write must slide the window, not reset it.
	if _, err := l.Write([]byte("ZZ")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := l.Tail(); got != "ABCDEFZZ" {
		t.Fatalf("tail = %q, want %q", got, "ABCDEFZZ")
	}
}

// Write must return promptly with no reader and no downstream I/O; a blocking
// sink would stall the VM because os/exec drains the copy goroutines on Wait.
func TestJobLogWriteIsNonBlocking(t *testing.T) {
	l := newJobLog(1024, "")
	payload := make([]byte, 1<<20)

	done := make(chan struct{})
	go func() {
		_, _ = l.Write(payload)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("jobLog.Write blocked")
	}
	if got := len(l.Tail()); got != 1024 {
		t.Fatalf("tail = %d bytes, want 1024", got)
	}
}

// Concurrent stdout/stderr pumping must be safe; run with -race.
func TestJobLogConcurrentWrites(t *testing.T) {
	l := newJobLog(4096, "")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				if _, err := l.Write([]byte("chunk")); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	_ = l.Tail()
}

func TestJobLogSentinelWithoutNonce(t *testing.T) {
	l := newJobLog(4096, "")
	if _, err := l.Write([]byte("hello\nAETHER_EXIT:42\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !l.SentinelSeen() {
		t.Fatal("sentinel not detected")
	}
	if got := l.ExitCode(); got != 42 {
		t.Fatalf("exit code = %d, want 42", got)
	}
}

// A sentinel split across chunk boundaries must still be assembled.
func TestJobLogSentinelSplitAcrossWrites(t *testing.T) {
	l := newJobLog(4096, "")
	for i, chunk := range []string{"boot\nAETHER_EX", "IT:4", "2"} {
		if _, err := l.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		if l.SentinelSeen() {
			t.Fatalf("sentinel matched after chunk %d %q, before the code was terminated", i, chunk)
		}
	}

	if _, err := l.Write([]byte("\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !l.SentinelSeen() {
		t.Fatal("split sentinel not detected after the terminating newline")
	}
	if got := l.ExitCode(); got != 42 {
		t.Fatalf("exit code = %d, want 42", got)
	}
}

func TestJobLogSentinelWithNonce(t *testing.T) {
	l := newJobLog(4096, "n0nce")
	if _, err := l.Write([]byte("AETHER_EXIT:n0nce:7\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !l.SentinelSeen() {
		t.Fatal("nonce sentinel not detected")
	}
	if got := l.ExitCode(); got != 7 {
		t.Fatalf("exit code = %d, want 7", got)
	}
}

func TestJobLogSentinelWithNonceSplit(t *testing.T) {
	l := newJobLog(64, "abc")
	if _, err := l.Write([]byte("AETHER_EXIT:ab")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if l.SentinelSeen() {
		t.Fatal("partial nonce matched")
	}
	if _, err := l.Write([]byte("c:0\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !l.SentinelSeen() {
		t.Fatal("nonce sentinel split across writes not detected")
	}
	if got := l.ExitCode(); got != 0 {
		t.Fatalf("exit code = %d, want 0", got)
	}
}

func TestJobLogIgnoresWrongNonce(t *testing.T) {
	l := newJobLog(4096, "expected")
	if _, err := l.Write([]byte("AETHER_EXIT:other:7\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if l.SentinelSeen() {
		t.Fatal("sentinel with a mismatched nonce must be ignored")
	}
}

func TestClassifyJobMatrix(t *testing.T) {
	cases := []struct {
		name  string
		in    jobOutcome
		state string
		code  int
	}{
		{"sentinel done", jobOutcome{SentinelSeen: true, SentinelCode: 42}, protocol.JobStateDone, 42},
		{"sentinel zero", jobOutcome{SentinelSeen: true, SentinelCode: 0}, protocol.JobStateDone, 0},
		{"timeout no sentinel", jobOutcome{TimedOut: true}, protocol.JobStateTimeout, 124},
		{"crash no sentinel", jobOutcome{}, protocol.JobStateFailed, -1},
		{"crash with wait error", jobOutcome{WaitErr: errors.New("boom")}, protocol.JobStateFailed, -1},
		// A sentinel is authoritative even if the deadline also fired.
		{"sentinel wins over timeout", jobOutcome{SentinelSeen: true, SentinelCode: 7, TimedOut: true}, protocol.JobStateDone, 7},
		// A job that already finished before the cancel stays done.
		{"sentinel wins over cancel", jobOutcome{SentinelSeen: true, SentinelCode: 3, Cancelled: true}, protocol.JobStateDone, 3},
		{"sentinel wins over cancel and timeout", jobOutcome{SentinelSeen: true, SentinelCode: 1, Cancelled: true, TimedOut: true}, protocol.JobStateDone, 1},
		{"cancel no sentinel", jobOutcome{Cancelled: true}, protocol.JobStateCancelled, -1},
		{"cancel with wait error", jobOutcome{Cancelled: true, WaitErr: errors.New("killed")}, protocol.JobStateCancelled, -1},
		// An explicit cancel beats a deadline that fired in the same instant.
		{"cancel wins over timeout", jobOutcome{Cancelled: true, TimedOut: true}, protocol.JobStateCancelled, -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, code, msg := classifyJob(tc.in)
			if state != tc.state {
				t.Fatalf("state = %q, want %q", state, tc.state)
			}
			if code != tc.code {
				t.Fatalf("exit code = %d, want %d", code, tc.code)
			}
			switch state {
			case protocol.JobStateDone:
				if msg != "" {
					t.Fatalf("done record must have no error, got %q", msg)
				}
			default:
				if msg == "" {
					t.Fatalf("state %q must carry an error message", state)
				}
			}
		})
	}
}

func TestJobRunnerRecordsSentinelOutcome(t *testing.T) {
	log := newJobLog(4096, "")

	var recorded []protocol.JobRecord
	r := NewJobRunner(JobRunnerConfig{
		JobID:     "job-1",
		RequestID: "req-1",
		WorkerID:  "worker-1",
		Log:       log,
		Wait: func() error {
			_, _ = log.Write([]byte("hello\nAETHER_EXIT:42\n"))
			return nil
		},
		Record: func(rec protocol.JobRecord) error {
			recorded = append(recorded, rec)
			return nil
		},
	})

	rec := r.Run(context.Background())

	if rec.State != protocol.JobStateDone || rec.ExitCode != 42 {
		t.Fatalf("record = %+v, want done/42", rec)
	}
	if rec.Error != "" {
		t.Fatalf("done record error = %q, want empty", rec.Error)
	}
	if rec.JobID != "job-1" || rec.RequestID != "req-1" || rec.WorkerID != "worker-1" {
		t.Fatalf("identifier fields lost: %+v", rec)
	}
	if rec.FinishedAt.Before(rec.StartedAt) {
		t.Fatalf("finished_at %v precedes started_at %v", rec.FinishedAt, rec.StartedAt)
	}
	if len(recorded) != 1 {
		t.Fatalf("record callback called %d times, want 1", len(recorded))
	}
	if recorded[0].State != protocol.JobStateDone || recorded[0].ExitCode != 42 {
		t.Fatalf("recorded = %+v, want done/42", recorded[0])
	}
	if !strings.Contains(log.Tail(), "hello") {
		t.Fatalf("console tail %q missing workload output", log.Tail())
	}
}

func TestJobRunnerTimeoutStopsVMAndClassifies(t *testing.T) {
	log := newJobLog(4096, "")

	stopCalled := false
	release := make(chan struct{})
	r := NewJobRunner(JobRunnerConfig{
		JobID:   "job-timeout",
		Timeout: 20 * time.Millisecond,
		Log:     log,
		Wait: func() error {
			<-release
			return errors.New("killed")
		},
		Stop: func() error {
			stopCalled = true
			close(release)
			return nil
		},
	})

	rec := r.Run(context.Background())

	if !stopCalled {
		t.Fatal("Stop was not called on timeout")
	}
	if rec.State != protocol.JobStateTimeout || rec.ExitCode != 124 {
		t.Fatalf("record = %+v, want timeout/124", rec)
	}
	if rec.Error == "" {
		t.Fatal("timeout record must carry an error message")
	}
}

// A sentinel already on the console wins even when the deadline fires, because
// classification is post-hoc rather than flag-driven.
func TestJobRunnerSentinelWinsOverTimeout(t *testing.T) {
	log := newJobLog(4096, "")
	if _, err := log.Write([]byte("done\nAETHER_EXIT:9\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	release := make(chan struct{})
	r := NewJobRunner(JobRunnerConfig{
		JobID:   "job-race",
		Timeout: 10 * time.Millisecond,
		Log:     log,
		Wait: func() error {
			<-release
			return nil
		},
		Stop: func() error {
			close(release)
			return nil
		},
	})

	rec := r.Run(context.Background())
	if rec.State != protocol.JobStateDone || rec.ExitCode != 9 {
		t.Fatalf("record = %+v, want done/9", rec)
	}
}

// While a job runs, the runner must periodically refresh a running record with
// an advancing HeartbeatAt, and the terminal record must still be written.
func TestJobRunnerHeartbeatsWhileRunning(t *testing.T) {
	log := newJobLog(4096, "")

	var mu sync.Mutex
	var beats []protocol.JobRecord
	release := make(chan struct{})

	r := NewJobRunner(JobRunnerConfig{
		JobID:             "job-hb",
		RequestID:         "req-hb",
		WorkerID:          "worker-hb",
		HeartbeatInterval: 5 * time.Millisecond,
		Log:               log,
		Wait: func() error {
			<-release
			_, _ = log.Write([]byte("AETHER_EXIT:0\n"))
			return nil
		},
		Heartbeat: func(rec protocol.JobRecord) error {
			mu.Lock()
			beats = append(beats, rec)
			mu.Unlock()
			return nil
		},
		Record: func(protocol.JobRecord) error { return nil },
	})

	done := make(chan protocol.JobRecord, 1)
	go func() { done <- r.Run(context.Background()) }()

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(beats)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("runner never heartbeated while the job was running")
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}

	close(release)
	rec := <-done

	if rec.State != protocol.JobStateDone || rec.ExitCode != 0 {
		t.Fatalf("terminal record = %+v, want done/0", rec)
	}
	if rec.HeartbeatAt.IsZero() || rec.HeartbeatAt.Before(rec.StartedAt) {
		t.Fatalf("terminal heartbeat %v must be at or after start %v", rec.HeartbeatAt, rec.StartedAt)
	}

	mu.Lock()
	first := beats[0]
	count := len(beats)
	mu.Unlock()

	if first.State != protocol.JobStateRunning {
		t.Fatalf("heartbeat state = %q, want running", first.State)
	}
	if first.JobID != "job-hb" || first.RequestID != "req-hb" || first.WorkerID != "worker-hb" {
		t.Fatalf("heartbeat lost identifiers: %+v", first)
	}
	if first.HeartbeatAt.IsZero() || first.HeartbeatAt.Before(first.StartedAt) {
		t.Fatalf("heartbeat timestamp %v precedes start %v", first.HeartbeatAt, first.StartedAt)
	}

	// The ticker must stop once Run returns.
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	after := len(beats)
	mu.Unlock()
	if after != count {
		t.Fatalf("heartbeats continued after Run returned: %d -> %d", count, after)
	}
}

// A failing heartbeat write is best-effort and must not change the outcome.
func TestJobRunnerHeartbeatFailureDoesNotChangeOutcome(t *testing.T) {
	log := newJobLog(4096, "")

	r := NewJobRunner(JobRunnerConfig{
		JobID:             "job-hb-fail",
		HeartbeatInterval: 2 * time.Millisecond,
		Log:               log,
		Wait: func() error {
			time.Sleep(20 * time.Millisecond)
			_, _ = log.Write([]byte("AETHER_EXIT:5\n"))
			return nil
		},
		Heartbeat: func(protocol.JobRecord) error { return errors.New("etcd down") },
		Record:    func(protocol.JobRecord) error { return nil },
	})

	rec := r.Run(context.Background())
	if rec.State != protocol.JobStateDone || rec.ExitCode != 5 {
		t.Fatalf("record = %+v, want done/5 despite heartbeat failure", rec)
	}
}

func TestJobRunnerRecordingFailureDoesNotChangeOutcome(t *testing.T) {
	log := newJobLog(4096, "")
	r := NewJobRunner(JobRunnerConfig{
		JobID: "job-rec-fail",
		Log:   log,
		Wait: func() error {
			_, _ = log.Write([]byte("AETHER_EXIT:3\n"))
			return nil
		},
		Record: func(protocol.JobRecord) error {
			return errors.New("etcd unavailable")
		},
	})

	rec := r.Run(context.Background())
	if rec.State != protocol.JobStateDone || rec.ExitCode != 3 {
		t.Fatalf("record = %+v, want done/3 despite recording failure", rec)
	}
}

// Cancel must stop the VM through the existing Stop path and produce exactly
// one cancelled terminal record.
func TestJobRunnerCancelStopsVMAndRecordsOnce(t *testing.T) {
	log := newJobLog(4096, "")

	var mu sync.Mutex
	var stopCalls int
	var recorded []protocol.JobRecord
	release := make(chan struct{})

	r := NewJobRunner(JobRunnerConfig{
		JobID:     "job-cancel",
		RequestID: "req-cancel",
		WorkerID:  "worker-cancel",
		Log:       log,
		Wait: func() error {
			<-release
			return errors.New("killed")
		},
		Stop: func() error {
			mu.Lock()
			stopCalls++
			mu.Unlock()
			select {
			case <-release:
			default:
				close(release)
			}
			return nil
		},
		Record: func(rec protocol.JobRecord) error {
			mu.Lock()
			recorded = append(recorded, rec)
			mu.Unlock()
			return nil
		},
	})

	done := make(chan protocol.JobRecord, 1)
	go func() { done <- r.Run(context.Background()) }()

	// Cancel after Run has started waiting; the signal is buffered by the
	// closed channel, so an early cancel is safe too.
	time.Sleep(20 * time.Millisecond)
	r.Cancel()
	r.Cancel() // idempotent

	var rec protocol.JobRecord
	select {
	case rec = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Cancel")
	}

	if rec.State != protocol.JobStateCancelled {
		t.Fatalf("record = %+v, want cancelled", rec)
	}
	if rec.ExitCode != jobUnknownExitCode {
		t.Fatalf("cancel exit code = %d, want %d", rec.ExitCode, jobUnknownExitCode)
	}
	if !strings.Contains(rec.Error, "cancel") {
		t.Fatalf("cancel error = %q, want it to mention cancellation", rec.Error)
	}

	mu.Lock()
	defer mu.Unlock()
	if stopCalls != 1 {
		t.Fatalf("Stop called %d times, want exactly 1", stopCalls)
	}
	if len(recorded) != 1 {
		t.Fatalf("record callback called %d times, want exactly 1", len(recorded))
	}
	if recorded[0].State != protocol.JobStateCancelled {
		t.Fatalf("recorded state = %q, want cancelled", recorded[0].State)
	}
}

// A cancel arriving after the job already finished must not rewrite the
// outcome: the already-written terminal record stands and no new one appears.
func TestJobRunnerCancelAfterCompletionIsNoop(t *testing.T) {
	log := newJobLog(4096, "")

	var recorded []protocol.JobRecord
	r := NewJobRunner(JobRunnerConfig{
		JobID: "job-late-cancel",
		Log:   log,
		Wait: func() error {
			_, _ = log.Write([]byte("AETHER_EXIT:0\n"))
			return nil
		},
		Record: func(rec protocol.JobRecord) error {
			recorded = append(recorded, rec)
			return nil
		},
	})

	rec := r.Run(context.Background())
	if rec.State != protocol.JobStateDone || rec.ExitCode != 0 {
		t.Fatalf("record = %+v, want done/0", rec)
	}

	r.Cancel()

	if len(recorded) != 1 {
		t.Fatalf("late Cancel wrote %d records, want the original 1", len(recorded))
	}
	if recorded[0].State != protocol.JobStateDone {
		t.Fatalf("late Cancel rewrote outcome to %q, want done", recorded[0].State)
	}
}

// Cancel racing a natural exit must still produce exactly one terminal record.
// Run with -race.
func TestJobRunnerCancelRacesNaturalExit(t *testing.T) {
	for i := 0; i < 50; i++ {
		log := newJobLog(64, "")
		var mu sync.Mutex
		var recorded []protocol.JobRecord
		r := NewJobRunner(JobRunnerConfig{
			JobID: "job-race-cancel",
			Log:   log,
			Wait: func() error {
				_, _ = log.Write([]byte("AETHER_EXIT:7\n"))
				return nil
			},
			Record: func(rec protocol.JobRecord) error {
				mu.Lock()
				recorded = append(recorded, rec)
				mu.Unlock()
				return nil
			},
		})

		done := make(chan struct{})
		go func() { r.Run(context.Background()); close(done) }()
		r.Cancel()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return")
		}

		mu.Lock()
		n := len(recorded)
		state := ""
		if n == 1 {
			state = recorded[0].State
		}
		mu.Unlock()

		if n != 1 {
			t.Fatalf("iteration %d: wrote %d records, want exactly 1", i, n)
		}
		// A sentinel is authoritative: whichever branch won the select, the
		// record must be done/7, never cancelled.
		if state != protocol.JobStateDone {
			t.Fatalf("iteration %d: state = %q, want done (sentinel wins)", i, state)
		}
	}
}
