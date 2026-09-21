package internal

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"aether/shared/protocol"
)

// jobRegistered reports whether the worker currently tracks a runner for
// jobID. It reads the map directly (same package) so tests can assert
// registration without triggering a cancel.
func jobRegistered(w *Worker, jobID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.jobs[jobID]
	return ok
}

// dispatchProcessJob sends a process job through the same path the stream
// consumer uses, so startJob's registration and launch seams are exercised.
func dispatchProcessJob(t *testing.T, w *Worker, jobID string) {
	t.Helper()

	job := protocol.Job{
		JobID:          jobID,
		RequestID:      "req-" + jobID,
		FunctionID:     "fn-" + jobID,
		Mode:           jobModeProcess,
		Command:        []string{"sh", "-c", "sleep 300"},
		TimeoutSeconds: 600,
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := w.handleJob(context.Background(), data); err != nil {
		t.Fatalf("handleJob(%s) = %v, want nil (spawn must ACK)", jobID, err)
	}
}

// CancelJob must reach exactly the runner that owns the id, leave other jobs
// alone, return false for an unknown id, and stay clear of the scaler's
// instance map. Deregistration is idempotent.
func TestCancelJobRoutesToOwningRunner(t *testing.T) {
	w := newJobWorker(t)
	withProvisionSeams(t, func(context.Context, *Instance, InstanceConfig) error {
		return nil
	}, nil, nil)

	var mu sync.Mutex
	captured := map[string]*JobRunner{}
	withJobRunnerSeam(t, func(_ context.Context, r *JobRunner) {
		mu.Lock()
		captured[r.jobID] = r
		mu.Unlock()
	})

	dispatchProcessJob(t, w, "job-a")
	dispatchProcessJob(t, w, "job-b")

	mu.Lock()
	runnerA, runnerB := captured["job-a"], captured["job-b"]
	mu.Unlock()
	if runnerA == nil || runnerB == nil {
		t.Fatalf("runners not captured (got %d entries)", len(captured))
	}

	if !jobRegistered(w, "job-a") || !jobRegistered(w, "job-b") {
		t.Fatal("process jobs were not registered")
	}

	// Jobs must stay invisible to the scaler, which only reads w.instances.
	if got := w.TotalInstances(); got != 0 {
		t.Fatalf("jobs leaked into the scaler view: TotalInstances=%d", got)
	}
	if _, ok := w.GetInstances("fn-job-a"); ok {
		t.Fatal("job instance appeared under a function id")
	}

	if !w.CancelJob("job-a") {
		t.Fatal("CancelJob(job-a) = false, want true")
	}
	if !runnerA.cancelRequested() {
		t.Fatal("runner for job-a was not cancelled")
	}
	if runnerB.cancelRequested() {
		t.Fatal("a cancel for job-a also cancelled job-b")
	}
	if w.CancelJob("job-does-not-exist") {
		t.Fatal("CancelJob(unknown) = true, want false")
	}

	// Cleanup deregisters only its own runner and is safe to repeat.
	runnerA.runCleanup()
	runnerA.runCleanup()
	if jobRegistered(w, "job-a") {
		t.Fatal("job-a still registered after cleanup")
	}
	if !jobRegistered(w, "job-b") {
		t.Fatal("job-a cleanup deregistered the unrelated job-b")
	}
	if !w.CancelJob("job-b") {
		t.Fatal("CancelJob(job-b) = false, want true")
	}
}

// When Run returns, the default startJobRunner wrapper must deregister the job
// so a finished job does not linger in the registry.
func TestStartJobDeregistersWhenRunReturns(t *testing.T) {
	w := newJobWorker(t)

	var gotInst *Instance
	withProvisionSeams(t, func(_ context.Context, inst *Instance, _ InstanceConfig) error {
		gotInst = inst
		return nil
	}, nil, nil)

	// The default seam is used on purpose: it runs Run in a goroutine wrapped
	// with the deregistration defer.
	dispatchProcessJob(t, w, "job-natural")

	if gotInst == nil {
		t.Fatal("provision seam never ran")
	}
	if !jobRegistered(w, "job-natural") {
		t.Fatal("job was not registered immediately after launch")
	}
	if got := w.TotalInstances(); got != 0 {
		t.Fatalf("job leaked into the scaler view: TotalInstances=%d", got)
	}

	// Simulate the VM exiting naturally.
	gotInst.signalExit(nil)

	deadline := time.After(5 * time.Second)
	for jobRegistered(w, "job-natural") {
		select {
		case <-deadline:
			t.Fatal("job was not deregistered after Run returned")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// A cancel for a finished (deregistered) job is a no-op.
	if w.CancelJob("job-natural") {
		t.Fatal("CancelJob on a finished job = true, want false")
	}
}
