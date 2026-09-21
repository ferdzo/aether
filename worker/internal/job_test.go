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
