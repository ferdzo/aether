package internal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aether/shared/protocol"

	redis "github.com/redis/go-redis/v9"
)

// withExecutionReadySeam overrides the vsock readiness gate so provisioning can
// complete without a real guest.
func withExecutionReadySeam(t *testing.T, fn func(context.Context, string, time.Duration) error) {
	t.Helper()
	prev := executionWaitReady
	if fn != nil {
		executionWaitReady = fn
	}
	t.Cleanup(func() { executionWaitReady = prev })
}

// withExecutionRecordSeams overrides durable-record reads/writes. Nil leaves the
// real (dead-registry) implementation, which is fine when a test does not care.
func withExecutionRecordSeams(t *testing.T,
	put func(*Registry, protocol.ExecutionRecord) error,
	get func(*Registry, string) (protocol.ExecutionRecord, error),
) {
	t.Helper()
	prevPut, prevGet := putExecution, getExecution
	if put != nil {
		putExecution = put
	}
	if get != nil {
		getExecution = get
	}
	t.Cleanup(func() { putExecution, getExecution = prevPut, prevGet })
}

func withExecOnGuest(t *testing.T, fn func(context.Context, string, string, protocol.ExecRequest) (protocol.ExecResult, error)) {
	t.Helper()
	prev := execOnGuest
	if fn != nil {
		execOnGuest = fn
	}
	t.Cleanup(func() { execOnGuest = prev })
}

func withShutdownGuest(t *testing.T, fn func(string) error) {
	t.Helper()
	prev := shutdownGuestFn
	if fn != nil {
		shutdownGuestFn = fn
	}
	t.Cleanup(func() { shutdownGuestFn = prev })
}

func writeFile(path string) error { return os.WriteFile(path, []byte("x"), 0o644) }

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func executionJob(execID string, workspaceMB int) []byte {
	data, _ := json.Marshal(protocol.Job{
		JobID:          execID,
		RequestID:      "req-" + execID,
		Mode:           protocol.ExecutionMode,
		Runtime:        "exec",
		TimeoutSeconds: 3600,
		WorkspaceMB:    workspaceMB,
		VCPU:           1,
		MemoryMB:       256,
	})
	return data
}

// An execution must launch a VM with the vsock device and a writable workspace
// drive, wait for the handshake, record itself ready, and stay invisible to the
// scaler.
func TestStartExecutionReadyAndUntracked(t *testing.T) {
	w := newJobWorker(t)
	w.cfg.SocketDir = t.TempDir()
	w.cfg.WorkspaceDir = t.TempDir()

	wsPath := w.cfg.WorkspaceDir + "/exec-1.ext4"
	withWorkspaceCreator(t, func(dir, id string, size int) (string, error) {
		return wsPath, nil
	})

	var gotCfg InstanceConfig
	withProvisionSeams(t, func(_ context.Context, _ *Instance, cfg InstanceConfig) error {
		gotCfg = cfg
		return nil
	}, nil, nil)

	var handshakePath string
	withExecutionReadySeam(t, func(_ context.Context, path string, _ time.Duration) error {
		handshakePath = path
		return nil
	})

	var records []protocol.ExecutionRecord
	withExecutionRecordSeams(t, func(_ *Registry, rec protocol.ExecutionRecord) error {
		records = append(records, rec)
		return nil
	}, nil)

	if err := w.handleJob(context.Background(), executionJob("exec-1", 64)); err != nil {
		t.Fatalf("handleJob = %v, want nil (readiness must ACK)", err)
	}

	if gotCfg.Vsock == nil {
		t.Fatal("execution instance has no vsock device")
	}
	if gotCfg.Vsock.CID < 3 {
		t.Fatalf("vsock CID = %d, want >= 3", gotCfg.Vsock.CID)
	}
	if !strings.HasSuffix(handshakePath, "exec-1.vsock") {
		t.Fatalf("handshake path = %q, want ...exec-1.vsock", handshakePath)
	}
	if gotCfg.Vsock.Path != handshakePath {
		t.Fatalf("vsock path %q != handshake path %q", gotCfg.Vsock.Path, handshakePath)
	}
	if len(gotCfg.Drives) != 1 || gotCfg.Drives[0].Path != wsPath || gotCfg.Drives[0].ReadOnly {
		t.Fatalf("drives = %+v, want one writable %q", gotCfg.Drives, wsPath)
	}

	if w.lookupExecution("exec-1") == nil {
		t.Fatal("execution was not registered on the worker")
	}
	if w.TotalInstances() != 0 {
		t.Fatalf("execution leaked into scaler view: TotalInstances=%d", w.TotalInstances())
	}

	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].State != protocol.ExecutionStateReady {
		t.Fatalf("record state = %q, want ready", records[0].State)
	}
	if records[0].WorkerAddr == "" {
		t.Fatal("ready record has no worker address")
	}
	if records[0].WorkspacePath != wsPath {
		t.Fatalf("record workspace = %q, want %q", records[0].WorkspacePath, wsPath)
	}
}

// A failed handshake is a provisioning failure: no ACK, the half-built
// workspace is removed so a redelivery can recreate it, and the in-flight
// marker is cleared.
func TestStartExecutionHandshakeFailureDoesNotAckAndRemovesWorkspace(t *testing.T) {
	w := newJobWorker(t)
	w.cfg.SocketDir = t.TempDir()
	w.cfg.WorkspaceDir = t.TempDir()

	wsPath := w.cfg.WorkspaceDir + "/exec-2.ext4"
	// Create a real file so removal can be observed.
	if err := writeFile(wsPath); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	withWorkspaceCreator(t, func(string, string, int) (string, error) { return wsPath, nil })
	withProvisionSeams(t, func(context.Context, *Instance, InstanceConfig) error { return nil }, nil, nil)
	withExecutionReadySeam(t, func(context.Context, string, time.Duration) error {
		return errors.New("no handshake")
	})

	err := w.handleJob(context.Background(), executionJob("exec-2", 64))
	if err == nil {
		t.Fatal("handshake failure must return an error so the entry stays pending")
	}
	if w.lookupExecution("exec-2") != nil {
		t.Fatal("failed execution was registered")
	}
	if fileExists(wsPath) {
		t.Fatal("workspace of a failed execution was not removed")
	}
	if getErr := w.redis.Get(context.Background(), jobInflightKey("req-exec-2")).Err(); getErr != redis.Nil {
		t.Fatalf("in-flight marker not cleared after failure: %v", getErr)
	}
}

// A workspace creation failure must not launch a VM (and must not ACK).
func TestStartExecutionWorkspaceFailureDoesNotLaunch(t *testing.T) {
	w := newJobWorker(t)
	w.cfg.SocketDir = t.TempDir()
	w.cfg.WorkspaceDir = t.TempDir()

	withWorkspaceCreator(t, func(string, string, int) (string, error) {
		return "", errors.New("no space")
	})
	var launches int32
	withProvisionSeams(t, func(context.Context, *Instance, InstanceConfig) error {
		atomic.AddInt32(&launches, 1)
		return nil
	}, nil, nil)

	err := w.handleJob(context.Background(), executionJob("exec-3", 64))
	if err == nil {
		t.Fatal("workspace failure must return an error so the entry stays pending")
	}
	if got := atomic.LoadInt32(&launches); got != 0 {
		t.Fatalf("launches = %d, want 0", got)
	}
}

// A redelivery of an already-ready execution is a no-op.
func TestStartExecutionSkipsExistingReadyRecord(t *testing.T) {
	w := newJobWorker(t)
	w.cfg.SocketDir = t.TempDir()
	w.cfg.WorkspaceDir = t.TempDir()

	withExecutionRecordSeams(t, nil, func(_ *Registry, id string) (protocol.ExecutionRecord, error) {
		return protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateReady}, nil
	})

	var launches int32
	withProvisionSeams(t, func(context.Context, *Instance, InstanceConfig) error {
		atomic.AddInt32(&launches, 1)
		return nil
	}, nil, nil)

	if err := w.handleJob(context.Background(), executionJob("exec-4", 64)); err != nil {
		t.Fatalf("handleJob = %v, want nil", err)
	}
	if got := atomic.LoadInt32(&launches); got != 0 {
		t.Fatalf("launches = %d, want 0 (durable guard must skip)", got)
	}
}

// ExecExecution must admit one exec at a time and reject a concurrent one.
func TestExecutionBusyGate(t *testing.T) {
	w := newJobWorker(t)
	started := make(chan struct{})
	release := make(chan struct{})

	withExecOnGuest(t, func(context.Context, string, string, protocol.ExecRequest) (protocol.ExecResult, error) {
		close(started)
		<-release
		return protocol.ExecResult{ExitCode: 0, Stdout: "done"}, nil
	})

	w.registerExecution(&Execution{ID: "busy-1", instance: &Instance{ID: "i", FunctionID: "f"}, vsockPath: "x"})

	done := make(chan error, 1)
	go func() {
		_, err := w.ExecExecution(context.Background(), "busy-1", protocol.ExecRequest{Argv: []string{"sleep", "1"}})
		done <- err
	}()

	<-started
	if _, err := w.ExecExecution(context.Background(), "busy-1", protocol.ExecRequest{Argv: []string{"echo", "x"}}); !errors.Is(err, errExecutionBusy) {
		t.Fatalf("concurrent exec error = %v, want errExecutionBusy", err)
	}
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("first exec = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first exec never finished")
	}

	// After the first exec releases the gate, another one is admitted.
	withExecOnGuest(t, func(context.Context, string, string, protocol.ExecRequest) (protocol.ExecResult, error) {
		return protocol.ExecResult{ExitCode: 0, Stdout: "again"}, nil
	})
	if _, err := w.ExecExecution(context.Background(), "busy-1", protocol.ExecRequest{Argv: []string{"echo", "again"}}); err != nil {
		t.Fatalf("exec after release = %v, want nil", err)
	}
}

// ExecExecution on an unknown execution must be a distinct not-found error.
func TestExecExecutionUnknown(t *testing.T) {
	w := newJobWorker(t)
	if _, err := w.ExecExecution(context.Background(), "nope", protocol.ExecRequest{Argv: []string{"true"}}); !errors.Is(err, errExecutionNotFound) {
		t.Fatalf("error = %v, want errExecutionNotFound", err)
	}
}

// Destroy must be idempotent, record stopped once, ask the guest to reset once
// and drop the execution.
func TestDestroyExecutionIdempotentRecordsStopped(t *testing.T) {
	w := newJobWorker(t)

	var shutdowns int32
	withShutdownGuest(t, func(string) error {
		atomic.AddInt32(&shutdowns, 1)
		return nil
	})
	var stopped []protocol.ExecutionRecord
	withExecutionRecordSeams(t, func(_ *Registry, rec protocol.ExecutionRecord) error {
		if rec.State == protocol.ExecutionStateStopped {
			stopped = append(stopped, rec)
		}
		return nil
	}, nil)

	w.registerExecution(&Execution{
		ID:            "destroy-1",
		instance:      &Instance{ID: "i", FunctionID: "f"},
		workspacePath: "/tmp/ws.ext4",
		vsockPath:     "x",
		workerAddr:    "127.0.0.1:9091",
		startedAt:     time.Now().UTC(),
	})

	if err := w.DestroyExecution("destroy-1"); err != nil {
		t.Fatalf("destroy = %v, want nil", err)
	}
	// Second destroy is a no-op.
	if err := w.DestroyExecution("destroy-1"); err != nil {
		t.Fatalf("second destroy = %v, want nil", err)
	}

	if got := atomic.LoadInt32(&shutdowns); got != 1 {
		t.Fatalf("guest shutdowns = %d, want 1", got)
	}
	if len(stopped) != 1 {
		t.Fatalf("stopped records = %d, want 1", len(stopped))
	}
	if w.lookupExecution("destroy-1") != nil {
		t.Fatal("destroyed execution still registered")
	}
	// A later exec is rejected.
	withExecOnGuest(t, func(context.Context, string, string, protocol.ExecRequest) (protocol.ExecResult, error) {
		t.Fatal("exec must not reach the guest after destroy")
		return protocol.ExecResult{}, nil
	})
	if _, err := w.ExecExecution(context.Background(), "destroy-1", protocol.ExecRequest{Argv: []string{"true"}}); !errors.Is(err, errExecutionNotFound) {
		t.Fatalf("exec after destroy = %v, want errExecutionNotFound", err)
	}
}

// The lifetime timer must destroy an execution when it expires, independently
// of the context that created it.
func TestExecutionLifetimeExpiry(t *testing.T) {
	w := newJobWorker(t)
	w.cfg.SocketDir = t.TempDir()
	w.cfg.WorkspaceDir = t.TempDir()

	withWorkspaceCreator(t, func(string, string, int) (string, error) { return "/tmp/ws.ext4", nil })
	withProvisionSeams(t, func(context.Context, *Instance, InstanceConfig) error { return nil }, nil, nil)
	withExecutionReadySeam(t, func(context.Context, string, time.Duration) error { return nil })
	withShutdownGuest(t, func(string) error { return nil })

	// Signal when the stopped record is written, then wait for it: the lifetime
	// timer runs on its own goroutine, and letting the test return while it
	// still reads these seams would race with their cleanup.
	stoppedCh := make(chan struct{})
	var once sync.Once
	withExecutionRecordSeams(t, func(_ *Registry, rec protocol.ExecutionRecord) error {
		if rec.State == protocol.ExecutionStateStopped {
			once.Do(func() { close(stoppedCh) })
		}
		return nil
	}, nil)

	job := protocol.Job{
		JobID:          "exec-life",
		RequestID:      "req-exec-life",
		Mode:           protocol.ExecutionMode,
		Runtime:        "exec",
		TimeoutSeconds: 1,
		WorkspaceMB:    64,
	}
	data, _ := json.Marshal(job)
	if err := w.handleJob(context.Background(), data); err != nil {
		t.Fatalf("handleJob = %v, want nil", err)
	}
	if w.lookupExecution("exec-life") == nil {
		t.Fatal("execution not registered")
	}

	select {
	case <-stoppedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("execution was not destroyed when its lifetime expired")
	}
	if w.lookupExecution("exec-life") != nil {
		t.Fatal("expired execution still registered")
	}
}
