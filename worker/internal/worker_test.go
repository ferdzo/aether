package internal

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	return NewRegistry(client, "10.0.0.1")
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
	if err := w.handleJob([]byte("{not json")); err == nil {
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
	go func() { done <- w.handleJob(data) }()

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
		Status:      StatusReady,
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
	if inst.Status != StatusStopped {
		t.Fatalf("status = %q, want stopped", inst.Status)
	}
}
