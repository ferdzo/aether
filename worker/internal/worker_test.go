package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aether/shared/logger"
	"aether/shared/protocol"
	"aether/shared/storage"

	redis "github.com/redis/go-redis/v9"
	etcd "go.etcd.io/etcd/client/v3"
)

func TestMain(m *testing.M) {
	logger.Init(slog.LevelError, false)
	os.Exit(m.Run())
}

func newDeadMinio(t *testing.T) *storage.Minio {
	t.Helper()
	m, err := storage.NewMinio(storage.MinioConfig{
		Endpoint:  "127.0.0.1:1",
		AccessKey: "test",
		SecretKey: "test",
	})
	if err != nil {
		t.Fatalf("failed to build minio client: %v", err)
	}
	return m
}

func newDeadRegistry(t *testing.T) *Registry {
	t.Helper()
	client, err := etcd.New(etcd.Config{
		Endpoints:   []string{"http://127.0.0.1:1"},
		DialTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("failed to build etcd client: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	// NewRegistry leaves leaseID at 0, so RegisterInstance always fails with
	// "worker not registered, no lease" — the seam we use to force a
	// registration failure without etcd.
	return NewRegistry(client, "10.0.0.1")
}

// newProvisionWorker builds a worker whose code cache is warm on disk, so
// SpawnInstance gets past EnsureCode without object storage. The registry is
// dead, so registration always fails.
func newProvisionWorker(t *testing.T, functionID string) *Worker {
	t.Helper()
	cfg := &Config{WorkerID: "test-worker"}
	dir := t.TempDir()
	codeCache := NewCodeCache(newDeadMinio(t), "bucket", dir)
	if err := os.WriteFile(filepath.Join(dir, functionID+".ext4"), []byte("code"), 0o644); err != nil {
		t.Fatalf("seed code cache: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { client.Close() })
	return NewWorker(cfg, newDeadRegistry(t), codeCache, client)
}

// withProvisionSeams overrides the provisioning stages for one test. A nil
// argument keeps the real implementation.
func withProvisionSeams(t *testing.T,
	start func(context.Context, *Instance, InstanceConfig) error,
	waitReady func(context.Context, *Instance, int, time.Duration) error,
	startProxy func(*Instance, int, int) error,
) {
	t.Helper()
	prevStart, prevWait, prevProxy := provisionStart, provisionWaitReady, provisionStartProxy
	if start != nil {
		provisionStart = start
	}
	if waitReady != nil {
		provisionWaitReady = waitReady
	}
	if startProxy != nil {
		provisionStartProxy = startProxy
	}
	t.Cleanup(func() {
		provisionStart, provisionWaitReady, provisionStartProxy = prevStart, prevWait, prevProxy
	})
}

func TestAllocatePortSequentialAndRelease(t *testing.T) {
	w := &Worker{usedPorts: make(map[int]bool), nextPort: 30000}

	p1 := w.allocatePort()
	p2 := w.allocatePort()
	if p1 != 30000 || p2 != 30001 {
		t.Fatalf("expected sequential allocation 30000,30001; got %d,%d", p1, p2)
	}

	w.releasePort(p1)
	if w.usedPorts[30000] {
		t.Fatal("released port still marked as used")
	}

	if got := w.allocatePort(); got != 30000 {
		t.Fatalf("expected released port to be reused; got %d", got)
	}
}

func TestHandleJobUnmarshalError(t *testing.T) {
	w := &Worker{}
	if err := w.handleJob(context.Background(), []byte("{not json")); err == nil {
		t.Fatal("expected unmarshal error, got nil")
	}
}

func TestHandleJobSpawnFailurePropagates(t *testing.T) {
	cfg := &Config{WorkerID: "test-worker"}
	codeCache := NewCodeCache(newDeadMinio(t), "bucket", t.TempDir())
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond})
	defer client.Close()
	w := NewWorker(cfg, newDeadRegistry(t), codeCache, client)

	job := protocol.Job{RequestID: "req-1", FunctionID: "fn-x", Count: 1}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- w.handleJob(context.Background(), data) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected spawn failure to propagate, got nil")
		}
		if !strings.Contains(err.Error(), "instance spawn failed") {
			t.Fatalf("unexpected error shape: %v", err)
		}
		if len(w.instances["fn-x"]) != 0 {
			t.Fatalf("failed spawn must not register instance; got %d", len(w.instances["fn-x"]))
		}
	case <-time.After(8 * time.Second):
		t.Fatal("handleJob did not return in time")
	}
}

// A spawn that reaches registration but fails there must return an error (so
// the stream entry stays pending and is reclaimed) and must not leave the
// half-provisioned instance tracked or its proxy port allocated.
func TestHandleJobRegistrationFailureReturnsErrorAndCleansUp(t *testing.T) {
	const functionID = "fn-reg"
	w := newProvisionWorker(t, functionID)

	withProvisionSeams(t,
		func(context.Context, *Instance, InstanceConfig) error { return nil },
		func(context.Context, *Instance, int, time.Duration) error { return nil },
		func(*Instance, int, int) error { return nil },
	)

	job := protocol.Job{RequestID: "req-reg", FunctionID: functionID, Count: 1}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	err = w.handleJob(context.Background(), data)
	if err == nil {
		t.Fatal("expected registration failure to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "failed to register instance") {
		t.Fatalf("unexpected error shape: %v", err)
	}

	if got := len(w.instances[functionID]); got != 0 {
		t.Fatalf("failed registration must not leave a tracked instance; got %d", got)
	}
	if got := len(w.usedPorts); got != 0 {
		t.Fatalf("failed registration must release the proxy port; used=%v", w.usedPorts)
	}
}

func TestSpawnInstanceContextCancellationAbortsProvisioning(t *testing.T) {
	const functionID = "fn-ctx"
	w := newProvisionWorker(t, functionID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	withProvisionSeams(t,
		func(ctx context.Context, _ *Instance, _ InstanceConfig) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
		nil, nil,
	)

	type result struct {
		inst *Instance
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		inst, err := w.SpawnInstanceContext(ctx, functionID)
		resCh <- result{inst, err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provisioning never reached the start stage")
	}

	cancel()

	select {
	case r := <-resCh:
		if r.inst != nil {
			t.Fatalf("cancelled provisioning returned an instance: %s", r.inst.ID)
		}
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled provisioning did not return promptly")
	}

	if got := len(w.instances[functionID]); got != 0 {
		t.Fatalf("cancelled provisioning must not leave an instance; got %d", got)
	}
	if got := len(w.usedPorts); got != 0 {
		t.Fatalf("cancelled provisioning must not leak a proxy port; used=%v", w.usedPorts)
	}
}

// WaitReady must observe the provisioning context so shutdown aborts an
// in-flight readiness wait instead of waiting out the full timeout.
func TestWaitReadyHonoursContextCancellation(t *testing.T) {
	inst := &Instance{
		ID:         "inst-wr",
		FunctionID: "fn-wr",
		status:     StatusReady,
		StartedAt:  time.Now(),
		vmIP:       "192.0.2.1", // TEST-NET-1, non-routable
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := inst.WaitReady(ctx, 3000, 30*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitReady err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("WaitReady ignored cancellation, took %v", elapsed)
	}
}

// The MMDS payload must carry the single resolved port, not the raw (0) value.
func TestBuildMMDSDataResolvesDefaultPort(t *testing.T) {
	data := buildMMDSData("tok", FunctionConfig{Entrypoint: "handler.js"}, nil)
	if got := data["port"]; got != defaultFunctionPort {
		t.Fatalf("MMDS port = %v, want %d", got, defaultFunctionPort)
	}

	data = buildMMDSData("tok", FunctionConfig{Port: 8080}, []string{"1.1.1.1", "8.8.8.8"})
	if got := data["port"]; got != 8080 {
		t.Fatalf("MMDS port = %v, want 8080", got)
	}
	dns, ok := data["dns"].([]string)
	if !ok || len(dns) != 2 || dns[0] != "1.1.1.1" {
		t.Fatalf("MMDS dns = %v, want [1.1.1.1 8.8.8.8]", data["dns"])
	}

	// No DNS configured must not emit the key at all.
	if _, present := buildMMDSData("tok", FunctionConfig{}, nil)["dns"]; present {
		t.Fatal("empty GuestDNS must not be added to the MMDS payload")
	}
}

func TestInstanceStopDrainsInFlightAndRemovesSocket(t *testing.T) {
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "inst-1.sock")
	if err := os.WriteFile(sockPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed socket: %v", err)
	}

	inHandler := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inHandler)
		time.Sleep(300 * time.Millisecond)
		fmt.Fprintln(w, "ok")
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)

	inst := &Instance{
		ID:          "inst-1",
		FunctionID:  "fn-d",
		status:      StatusReady,
		StartedAt:   time.Now(),
		proxyServer: srv,
		socketPath:  sockPath,
	}

	type result struct {
		code int
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		resCh <- result{code: resp.StatusCode}
	}()

	<-inHandler
	time.Sleep(50 * time.Millisecond) // handler mid-flight when Stop lands

	if err := inst.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("in-flight request severed by Stop: %v", r.err)
		}
		if r.code != http.StatusOK {
			t.Fatalf("in-flight request got status %d", r.code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	if _, statErr := os.Stat(sockPath); !os.IsNotExist(statErr) {
		t.Fatalf("socket file not removed: %v", statErr)
	}
	if inst.GetStatus() != StatusStopped {
		t.Fatalf("status = %q, want stopped", inst.GetStatus())
	}
}

// Stop must be safe to call repeatedly and concurrently with status access.
// Run with -race.
func TestInstanceStatusConcurrentAccess(t *testing.T) {
	inst := &Instance{
		ID:         "inst-race",
		FunctionID: "fn-race",
		status:     StatusStarting,
		StartedAt:  time.Now(),
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = inst.GetStatus()
				inst.SetStatus(StatusReady)
				_ = inst.GetProxyPort()
				_ = inst.IdleDuration()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := inst.Stop(); err != nil {
				t.Errorf("Stop: %v", err)
				return
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	// The final status is intentionally not asserted: the setters and Stop race
	// by design. The point of this test is that -race reports no data race and
	// that no call panics.
	_ = inst.GetStatus()
}
