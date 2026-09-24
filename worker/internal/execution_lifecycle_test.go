package internal

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"aether/shared/protocol"
)

func withExecutionsList(t *testing.T, fn func(*Registry, context.Context) ([]protocol.ExecutionRecord, error)) {
	t.Helper()
	prev := listExecutions
	if fn != nil {
		listExecutions = fn
	}
	t.Cleanup(func() { listExecutions = prev })
}

func minimalExecution(id string) *Execution {
	return &Execution{
		ID:            id,
		instance:      &Instance{ID: "inst-" + id, FunctionID: "f"},
		workspacePath: "/tmp/" + id + ".ext4",
		vsockPath:     "/tmp/" + id + ".vsock",
		workerAddr:    "127.0.0.1:9091",
		startedAt:     time.Now().UTC(),
	}
}

// registerExecution must refuse an id that is already registered and stop the
// newcomer's VM so it cannot become an orphan no record points at.
func TestRegisterExecutionRefusesDuplicateAndStopsNewcomer(t *testing.T) {
	w := newJobWorker(t)

	first := minimalExecution("dup")
	if !w.registerExecution(first) {
		t.Fatal("first registration was refused")
	}
	second := minimalExecution("dup")
	if w.registerExecution(second) {
		t.Fatal("duplicate registration was accepted")
	}
	if !second.instance.isStopped() {
		t.Fatal("duplicate newcomer's VM was not stopped")
	}
	if w.lookupExecution("dup") != first {
		t.Fatal("duplicate overwrote the registered execution")
	}
	if first.instance.isStopped() {
		t.Fatal("winner's VM must not be stopped")
	}
}

// Once shutdown starts, registration must refuse and stop the newcomer so no VM
// outlives the worker.
func TestRegisterExecutionRefusedAfterShutdown(t *testing.T) {
	w := newJobWorker(t)
	w.registerExecution(minimalExecution("live"))
	w.destroyAllExecutions()

	late := minimalExecution("late")
	if w.registerExecution(late) {
		t.Fatal("registration succeeded after shutdown began")
	}
	if !late.instance.isStopped() {
		t.Fatal("late VM was not stopped")
	}
	if w.lookupExecution("live") != nil {
		t.Fatal("shutdown did not destroy the live execution")
	}
}

// A stale lifetime timer / death callback must not destroy a later execution
// that reused the id.
func TestDestroyExecutionForIgnoresReusedID(t *testing.T) {
	w := newJobWorker(t)
	withShutdownGuest(t, func(string) error { return nil })
	withExecutionRecordSeams(t, func(*Registry, protocol.ExecutionRecord) error { return nil }, nil)

	old := minimalExecution("reuse")
	w.registerExecution(old)
	if err := w.destroyExecution("reuse"); err != nil {
		t.Fatalf("destroy old: %v", err)
	}

	newExec := minimalExecution("reuse")
	w.registerExecution(newExec)
	// A stale timer for the old execution must be a no-op.
	if err := w.destroyExecutionFor("reuse", old); err != nil {
		t.Fatalf("stale destroyExecutionFor: %v", err)
	}
	if w.lookupExecution("reuse") != newExec {
		t.Fatal("stale timer destroyed the reused id")
	}
	// The current execution's own timer still works.
	if err := w.destroyExecutionFor("reuse", newExec); err != nil {
		t.Fatalf("current destroyExecutionFor: %v", err)
	}
	if w.lookupExecution("reuse") != nil {
		t.Fatal("current execution was not destroyed")
	}
}

// A provisioning failure must leave a terminal failed record (so the gateway's
// waitReady returns 502 instead of only timing out) and must still clear the
// in-flight marker.
func TestStartExecutionFailureRecordsFailedState(t *testing.T) {
	w := newJobWorker(t)
	w.cfg.SocketDir = t.TempDir()
	w.cfg.WorkspaceDir = t.TempDir()

	withWorkspaceCreator(t, func(string, string, int) (string, error) {
		return "", errors.New("no space")
	})

	var states []string
	withExecutionRecordSeams(t, func(_ *Registry, rec protocol.ExecutionRecord) error {
		states = append(states, rec.State)
		return nil
	}, nil)

	err := w.handleJob(context.Background(), executionJob("exec-fail", 64))
	if err == nil {
		t.Fatal("provisioning failure must return an error")
	}
	if len(states) != 2 || states[0] != protocol.ExecutionStateCreating || states[1] != protocol.ExecutionStateFailed {
		t.Fatalf("record states = %v, want [creating failed]", states)
	}
}

// A duplicate id reaching startExecution (a client retry racing the in-flight
// marker's expiry) must not launch a second long-lived VM: registerExecution
// refuses it and stops the newcomer.
func TestStartExecutionDuplicateIDDoesNotLaunchSecondVM(t *testing.T) {
	w := newJobWorker(t)
	w.cfg.SocketDir = t.TempDir()
	w.cfg.WorkspaceDir = t.TempDir()

	withWorkspaceCreator(t, func(dir, id string, size int) (string, error) {
		return dir + "/" + id + ".ext4", nil
	})
	withExecutionReadySeam(t, func(context.Context, string, time.Duration) error { return nil })

	var launched *Instance
	withProvisionSeams(t, func(_ context.Context, inst *Instance, _ InstanceConfig) error {
		launched = inst
		return nil
	}, nil, nil)

	// The winner is already registered for this id.
	winner := minimalExecution("dup-exec")
	w.registerExecution(winner)

	if err := w.handleJob(context.Background(), executionJob("dup-exec", 64)); err != nil {
		t.Fatalf("handleJob = %v, want nil (duplicate must ACK, not loop)", err)
	}
	if launched == nil {
		t.Fatal("duplicate never reached provisioning")
	}
	if !launched.isStopped() {
		t.Fatal("duplicate's VM was not stopped; a second VM would be orphaned")
	}
	if w.lookupExecution("dup-exec") != winner {
		t.Fatal("duplicate replaced the registered winner")
	}
}

// Unexpected guest death must destroy the execution and record it stopped.
func TestExecutionVMDeathDestroysExecution(t *testing.T) {
	w := newJobWorker(t)
	w.cfg.SocketDir = t.TempDir()
	w.cfg.WorkspaceDir = t.TempDir()

	withWorkspaceCreator(t, func(dir, id string, size int) (string, error) {
		return dir + "/" + id + ".ext4", nil
	})
	withProvisionSeams(t, func(context.Context, *Instance, InstanceConfig) error { return nil }, nil, nil)
	withExecutionReadySeam(t, func(context.Context, string, time.Duration) error { return nil })
	withShutdownGuest(t, func(string) error { return nil })

	stoppedCh := make(chan struct{})
	var once atomic.Bool
	withExecutionRecordSeams(t, func(_ *Registry, rec protocol.ExecutionRecord) error {
		if rec.State == protocol.ExecutionStateStopped && once.CompareAndSwap(false, true) {
			close(stoppedCh)
		}
		return nil
	}, nil)

	if err := w.handleJob(context.Background(), executionJob("exec-death", 64)); err != nil {
		t.Fatalf("handleJob = %v", err)
	}
	exec := w.lookupExecution("exec-death")
	if exec == nil {
		t.Fatal("execution not registered")
	}
	exec.instance.mu.Lock()
	cb := exec.instance.onVMDeath
	exec.instance.mu.Unlock()
	if cb == nil {
		t.Fatal("execution instance has no VM-death callback")
	}
	cb("f", exec.instance.ID)

	select {
	case <-stoppedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("VM death did not produce a stopped record")
	}
	if w.lookupExecution("exec-death") != nil {
		t.Fatal("execution still registered after VM death")
	}
}

// Startup reconcile must mark this worker's non-terminal records failed and
// leave other workers' records and terminal records alone.
func TestReconcileExecutionsMarksOwnNonTerminalFailed(t *testing.T) {
	w := newJobWorker(t)
	w.cfg.SocketDir = t.TempDir()

	me := w.workerID()
	withExecutionsList(t, func(*Registry, context.Context) ([]protocol.ExecutionRecord, error) {
		return []protocol.ExecutionRecord{
			{ID: "mine-ready", State: protocol.ExecutionStateReady, WorkerID: me},
			{ID: "mine-creating", State: protocol.ExecutionStateCreating, WorkerID: me},
			{ID: "other-ready", State: protocol.ExecutionStateReady, WorkerID: "someone-else"},
			{ID: "mine-done", State: protocol.ExecutionStateStopped, WorkerID: me},
		}, nil
	})

	var reconciled []string
	withExecutionRecordSeams(t, func(_ *Registry, rec protocol.ExecutionRecord) error {
		if rec.State == protocol.ExecutionStateFailed {
			reconciled = append(reconciled, rec.ID)
		}
		return nil
	}, nil)

	w.reconcileExecutions(context.Background())

	want := map[string]bool{"mine-ready": true, "mine-creating": true}
	if len(reconciled) != len(want) {
		t.Fatalf("reconciled = %v, want %v", reconciled, want)
	}
	for _, id := range reconciled {
		if !want[id] {
			t.Fatalf("reconciled unexpected record %q", id)
		}
	}
}

// Re-creating a terminal id must remove the retained workspace image first so
// CreateWorkspace's reuse guard does not wedge it.
func TestStartExecutionRemovesStaleWorkspaceForTerminalRecord(t *testing.T) {
	w := newJobWorker(t)
	w.cfg.SocketDir = t.TempDir()
	w.cfg.WorkspaceDir = t.TempDir()

	wsPath := w.cfg.WorkspaceDir + "/exec-again.ext4"
	if err := os.WriteFile(wsPath, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed stale workspace: %v", err)
	}
	// A stateful store, because the creating record this attempt writes must be
	// visible to the reclaim check. A constant stub returns "stopped" forever
	// and hides the ordering bug this test exists to catch: with the reclaim
	// running after the creating write it would see "creating" and bail.
	store := map[string]protocol.ExecutionRecord{
		"exec-again": {ID: "exec-again", State: protocol.ExecutionStateStopped, WorkspacePath: wsPath},
	}
	withExecutionRecordSeams(t,
		func(_ *Registry, rec protocol.ExecutionRecord) error {
			store[rec.ID] = rec
			return nil
		},
		func(_ *Registry, id string) (protocol.ExecutionRecord, error) {
			rec, ok := store[id]
			if !ok {
				return protocol.ExecutionRecord{}, os.ErrNotExist
			}
			return rec, nil
		},
	)

	// The creator asserts the stale image is gone when it is invoked.
	created := false
	withWorkspaceCreator(t, func(dir, id string, size int) (string, error) {
		if fileExists(wsPath) {
			t.Fatal("stale workspace was not removed before CreateWorkspace")
		}
		created = true
		return wsPath, nil
	})
	withProvisionSeams(t, func(context.Context, *Instance, InstanceConfig) error { return nil }, nil, nil)
	withExecutionReadySeam(t, func(context.Context, string, time.Duration) error { return nil })

	if err := w.handleJob(context.Background(), executionJob("exec-again", 64)); err != nil {
		t.Fatalf("handleJob = %v", err)
	}
	if !created {
		t.Fatal("workspace was not recreated")
	}
}
