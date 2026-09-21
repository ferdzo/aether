package internal

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aether/shared/protocol"

	"github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"
)

// newJobWorker builds a worker with a live miniredis (for the in-flight SetNX
// guard) and a dead etcd registry (job records fail to persist, which is fine:
// the ACK decision never depends on them). The launch seam is stubbed in each
// test so no VM, rootfs or root privileges are needed.
func newJobWorker(t *testing.T) *Worker {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })

	cfg := &Config{WorkerID: "job-worker"}
	codeCache := NewCodeCache(newDeadMinio(t), "bucket", t.TempDir())
	return NewWorker(cfg, newDeadRegistry(t), codeCache, client)
}

// withJobRunnerSeam overrides how a process job's runner is started, so tests
// can observe the runner without waiting on a real VM exit.
func withJobRunnerSeam(t *testing.T, fn func(context.Context, *JobRunner)) {
	t.Helper()
	prev := startJobRunner
	if fn != nil {
		startJobRunner = fn
	}
	t.Cleanup(func() { startJobRunner = prev })
}

// A process job must ACK (handleJob returns nil) as soon as it is spawned, stay
// out of the scaler's instance map, and have a runner started.
func TestStartJobSpawnsAcksAndIsUntracked(t *testing.T) {
	w := newJobWorker(t)

	var gotInstance *Instance
	var gotCfg InstanceConfig
	withProvisionSeams(t, func(_ context.Context, inst *Instance, cfg InstanceConfig) error {
		gotInstance = inst
		gotCfg = cfg
		return nil
	}, nil, nil)

	runnerCh := make(chan *JobRunner, 1)
	withJobRunnerSeam(t, func(_ context.Context, r *JobRunner) {
		runnerCh <- r
	})

	job := protocol.Job{
		JobID:          "job-1",
		RequestID:      "req-1",
		FunctionID:     "fn-job",
		Mode:           jobModeProcess,
		Command:        []string{"sh", "-c", "echo hi"},
		TimeoutSeconds: 30,
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := w.handleJob(context.Background(), data); err != nil {
		t.Fatalf("handleJob = %v, want nil (spawn must ACK)", err)
	}

	if gotInstance == nil {
		t.Fatal("launch seam was never invoked")
	}
	if w.TotalInstances() != 0 {
		t.Fatalf("job instance leaked into scaler view: TotalInstances=%d", w.TotalInstances())
	}
	if _, ok := w.GetInstances(job.FunctionID); ok {
		t.Fatal("job instance appeared under a function id")
	}
	if gotCfg.ConsoleWriter == nil {
		t.Fatal("job instance must be given the job log as its console writer")
	}
	if len(gotCfg.Drives) != 0 {
		t.Fatalf("a job without WorkspaceMB must attach no drives, got %+v", gotCfg.Drives)
	}
	if got := gotCfg.MMDSData["mode"]; got != jobModeProcess {
		t.Fatalf("mmds mode = %v, want %q", got, jobModeProcess)
	}

	select {
	case r := <-runnerCh:
		if r == nil {
			t.Fatal("started a nil runner")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner was never started")
	}
}

// The same RequestID delivered twice must launch exactly one VM: the second
// delivery is absorbed by the in-flight marker (or the durable record).
func TestStartJobIdempotentPerRequest(t *testing.T) {
	w := newJobWorker(t)

	var launches int32
	withProvisionSeams(t, func(context.Context, *Instance, InstanceConfig) error {
		atomic.AddInt32(&launches, 1)
		return nil
	}, nil, nil)
	withJobRunnerSeam(t, func(context.Context, *JobRunner) {})

	job := protocol.Job{
		JobID:     "job-2",
		RequestID: "req-2",
		Mode:      jobModeProcess,
		Command:   []string{"true"},
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := w.handleJob(context.Background(), data); err != nil {
			t.Fatalf("delivery %d: handleJob = %v, want nil", i, err)
		}
	}

	if got := atomic.LoadInt32(&launches); got != 1 {
		t.Fatalf("launches = %d, want 1", got)
	}
}

// A failure before spawn must return the error (so the entry is not ACKed) and
// clear the in-flight marker so the redelivery can retry.
func TestStartJobProvisioningFailureClearsInflightMarker(t *testing.T) {
	w := newJobWorker(t)

	withProvisionSeams(t, func(context.Context, *Instance, InstanceConfig) error {
		return errors.New("boom")
	}, nil, nil)
	withJobRunnerSeam(t, func(context.Context, *JobRunner) {})

	job := protocol.Job{
		JobID:     "job-3",
		RequestID: "req-3",
		Mode:      jobModeProcess,
		Command:   []string{"true"},
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	err = w.handleJob(context.Background(), data)
	if err == nil {
		t.Fatal("provisioning failure must return an error so the entry stays pending")
	}
	if !strings.Contains(err.Error(), "failed to start job instance") {
		t.Fatalf("unexpected error shape: %v", err)
	}

	if getErr := w.redis.Get(context.Background(), jobInflightKey(job.RequestID)).Err(); getErr != redis.Nil {
		t.Fatalf("in-flight marker not cleared after failure: %v", getErr)
	}

	// With the marker gone, the redelivery must be able to launch.
	withProvisionSeams(t, func(context.Context, *Instance, InstanceConfig) error {
		return nil
	}, nil, nil)
	if err := w.handleJob(context.Background(), data); err != nil {
		t.Fatalf("retry after cleared marker = %v, want nil", err)
	}
}

// The process MMDS payload must carry the guest supervisor contract.
func TestBuildJobMMDSData(t *testing.T) {
	data := buildJobMMDSData("tok-1", protocol.Job{
		Command:        []string{"sh", "-c", "echo hi"},
		TimeoutSeconds: 45,
		EnvVars:        map[string]string{"A": "B"},
	}, []string{"1.1.1.1"}, "nonce-1")

	if got := data["token"]; got != "tok-1" {
		t.Fatalf("token = %v, want tok-1", got)
	}
	if got := data["mode"]; got != jobModeProcess {
		t.Fatalf("mode = %v, want %q", got, jobModeProcess)
	}
	cmd, ok := data["command"].([]string)
	if !ok || len(cmd) != 3 || cmd[0] != "sh" || cmd[2] != "echo hi" {
		t.Fatalf("command = %v, want [sh -c echo hi]", data["command"])
	}
	if got := data["timeout_s"]; got != 45 {
		t.Fatalf("timeout_s = %v, want 45", got)
	}
	if got := data["exit_nonce"]; got != "nonce-1" {
		t.Fatalf("exit_nonce = %v, want nonce-1", got)
	}
	env, ok := data["env"].(map[string]string)
	if !ok || env["A"] != "B" {
		t.Fatalf("env = %v, want map[A:B]", data["env"])
	}
	dns, ok := data["dns"].([]string)
	if !ok || len(dns) != 1 || dns[0] != "1.1.1.1" {
		t.Fatalf("dns = %v, want [1.1.1.1]", data["dns"])
	}

	// No DNS configured must not emit the key at all (mirrors the function path).
	if _, present := buildJobMMDSData("tok", protocol.Job{}, nil, "n")["dns"]; present {
		t.Fatal("empty GuestDNS must not be added to the job MMDS payload")
	}
}

// monitorVM must signal the exit channel even when there is no VM, so a runner
// waiting on ExitCh cannot hang.
func TestInstanceExitChSignalsWithoutVM(t *testing.T) {
	inst := &Instance{ID: "inst-exit", FunctionID: "fn-exit"}

	go inst.monitorVM()

	select {
	case <-inst.ExitCh():
	case <-time.After(2 * time.Second):
		t.Fatal("ExitCh was never signalled")
	}
}

// ExitCh must be safe to obtain before any VM exists and must deliver at most
// one value even if signalExit is called repeatedly.
func TestInstanceExitChSignalsOnce(t *testing.T) {
	inst := &Instance{ID: "inst-once", FunctionID: "fn-once"}

	inst.signalExit(errors.New("first"))
	inst.signalExit(errors.New("second"))

	select {
	case err := <-inst.ExitCh():
		if err == nil || err.Error() != "first" {
			t.Fatalf("first exit = %v, want first", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ExitCh was never signalled")
	}

	select {
	case err := <-inst.ExitCh():
		t.Fatalf("ExitCh delivered a second value: %v", err)
	default:
	}
}

// withWorkspaceCreator overrides how a job's workspace image is built, so the
// dispatch tests exercise attachment without invoking mke2fs.
func withWorkspaceCreator(t *testing.T, fn func(dir, jobID string, sizeMB int) (string, error)) {
	t.Helper()
	prev := createJobWorkspace
	if fn != nil {
		createJobWorkspace = fn
	}
	t.Cleanup(func() { createJobWorkspace = prev })
}

// A job with WorkspaceMB must create the image before launch, attach it as the
// first writable drive, and carry the path on both the running record and the
// runner config so the terminal record inherits it.
func TestStartJobWithWorkspaceAttachesDriveAndRecordsPath(t *testing.T) {
	w := newJobWorker(t)
	wsDir := t.TempDir()
	w.cfg.WorkspaceDir = wsDir

	wantPath := filepath.Join(wsDir, "job-ws.ext4")
	var gotDir, gotJobID string
	var gotSize int
	withWorkspaceCreator(t, func(dir, jobID string, sizeMB int) (string, error) {
		gotDir, gotJobID, gotSize = dir, jobID, sizeMB
		return wantPath, nil
	})

	var gotCfg InstanceConfig
	withProvisionSeams(t, func(_ context.Context, _ *Instance, cfg InstanceConfig) error {
		gotCfg = cfg
		return nil
	}, nil, nil)

	var gotRunner *JobRunner
	withJobRunnerSeam(t, func(_ context.Context, r *JobRunner) {
		gotRunner = r
	})

	job := protocol.Job{
		JobID:       "job-ws",
		RequestID:   "req-ws",
		Mode:        jobModeProcess,
		Command:     []string{"true"},
		WorkspaceMB: 64,
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := w.handleJob(context.Background(), data); err != nil {
		t.Fatalf("handleJob = %v, want nil (spawn must ACK)", err)
	}

	if gotDir != wsDir || gotJobID != "job-ws" || gotSize != 64 {
		t.Fatalf("workspace request = (%q, %q, %d), want (%q, job-ws, 64)", gotDir, gotJobID, gotSize, wsDir)
	}
	if len(gotCfg.Drives) != 1 {
		t.Fatalf("drives = %+v, want exactly one workspace drive", gotCfg.Drives)
	}
	if d := gotCfg.Drives[0]; d.Path != wantPath || d.ReadOnly {
		t.Fatalf("workspace drive = %+v, want a writable %q", d, wantPath)
	}
	if gotRunner == nil {
		t.Fatal("runner was never started")
	}
	if gotRunner.workspacePath != wantPath {
		t.Fatalf("runner workspace path = %q, want %q", gotRunner.workspacePath, wantPath)
	}
}

// A workspace creation failure is a provisioning failure: no VM may launch, the
// error must propagate so the entry stays pending, and the in-flight marker
// must be cleared for a retry.
func TestStartJobWorkspaceCreationFailureClearsInflightMarker(t *testing.T) {
	w := newJobWorker(t)
	w.cfg.WorkspaceDir = t.TempDir()

	withWorkspaceCreator(t, func(string, string, int) (string, error) {
		return "", errors.New("no space left on device")
	})

	var launches int32
	withProvisionSeams(t, func(context.Context, *Instance, InstanceConfig) error {
		atomic.AddInt32(&launches, 1)
		return nil
	}, nil, nil)
	withJobRunnerSeam(t, func(context.Context, *JobRunner) {})

	job := protocol.Job{
		JobID:       "job-ws-fail",
		RequestID:   "req-ws-fail",
		Mode:        jobModeProcess,
		Command:     []string{"true"},
		WorkspaceMB: 64,
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	err = w.handleJob(context.Background(), data)
	if err == nil {
		t.Fatal("workspace failure must return an error so the entry stays pending")
	}
	if !strings.Contains(err.Error(), "failed to create job workspace") {
		t.Fatalf("unexpected error shape: %v", err)
	}
	if got := atomic.LoadInt32(&launches); got != 0 {
		t.Fatalf("launches = %d, want 0 (no VM without a workspace)", got)
	}
	if getErr := w.redis.Get(context.Background(), jobInflightKey(job.RequestID)).Err(); getErr != redis.Nil {
		t.Fatalf("in-flight marker not cleared after failure: %v", getErr)
	}
}
