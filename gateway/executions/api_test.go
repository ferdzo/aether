package executions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aether/gateway/internal"
	"aether/shared/protocol"

	"github.com/alicebob/miniredis/v2"
)

// stubStore stands in for the etcd-backed record store.
type stubStore struct {
	fn func(id string) (*protocol.ExecutionRecord, error)
}

func (s *stubStore) GetExecution(_ context.Context, id string) (*protocol.ExecutionRecord, error) {
	return s.fn(id)
}

func newTestAPI(t *testing.T) (*ExecutionsAPI, *internal.RedisClient) {
	t.Helper()

	mr := miniredis.RunT(t)
	rc, err := internal.NewRedisClient(mr.Addr())
	if err != nil {
		t.Fatalf("NewRedisClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	api := NewExecutionsAPI(rc, nil, "", "")
	api.pollInterval = 5 * time.Millisecond
	api.readyTimeout = 500 * time.Millisecond
	return api, rc
}

func do(t *testing.T, api *ExecutionsAPI, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	api.Routes().ServeHTTP(rec, req)
	return rec
}

func readyStore(id string) *stubStore {
	return &stubStore{fn: func(gotID string) (*protocol.ExecutionRecord, error) {
		if gotID != id {
			return nil, ErrExecutionNotFound
		}
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateReady, WorkerAddr: "127.0.0.1:1"}, nil
	}}
}

func TestCreateValidation(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = &stubStore{fn: func(string) (*protocol.ExecutionRecord, error) { return nil, ErrExecutionNotFound }}

	cases := []struct {
		name string
		body string
	}{
		{"missing runtime", `{"workspace_mb":64,"timeout_seconds":30}`},
		{"missing workspace", `{"runtime":"exec","timeout_seconds":30}`},
		{"zero workspace", `{"runtime":"exec","workspace_mb":0,"timeout_seconds":30}`},
		{"negative workspace", `{"runtime":"exec","workspace_mb":-1,"timeout_seconds":30}`},
		{"oversized workspace", fmt.Sprintf(`{"runtime":"exec","workspace_mb":%d,"timeout_seconds":30}`, protocol.MaxWorkspaceMB+1)},
		{"negative vcpu", `{"runtime":"exec","workspace_mb":64,"timeout_seconds":30,"vcpu":-1}`},
		{"negative memory", `{"runtime":"exec","workspace_mb":64,"timeout_seconds":30,"memory_mb":-1}`},
		{"zero timeout", `{"runtime":"exec","workspace_mb":64,"timeout_seconds":0}`},
		{"negative timeout", `{"runtime":"exec","workspace_mb":64,"timeout_seconds":-1}`},
		{"id with slash", `{"runtime":"exec","workspace_mb":64,"timeout_seconds":30,"id":"a/b"}`},
		{"invalid json", `{"runtime":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, api, http.MethodPost, "/", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCreatePublishesExecutionJobAndWaitsReady(t *testing.T) {
	api, rc := newTestAPI(t)

	var calls int32
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		// First poll: worker has not written the record yet; then ready.
		if atomic.AddInt32(&calls, 1) < 2 {
			return nil, ErrExecutionNotFound
		}
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateReady, WorkerAddr: "127.0.0.1:9091"}, nil
	}}

	body := `{"runtime":"exec","workspace_mb":64,"timeout_seconds":60,"vcpu":1,"memory_mb":256}`
	rec := do(t, api, http.MethodPost, "/", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d want 201: %s", rec.Code, rec.Body.String())
	}
	var got protocol.ExecutionRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not a record: %v", err)
	}
	if got.State != protocol.ExecutionStateReady {
		t.Fatalf("state = %q, want ready", got.State)
	}

	msgs, err := rc.Client().XRange(context.Background(), protocol.StreamProvision, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("stream entries = %d, want 1", len(msgs))
	}
	raw, _ := msgs[0].Values["job"].(string)
	var job protocol.Job
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		t.Fatalf("payload not a job: %v", err)
	}
	if job.Mode != protocol.ExecutionMode {
		t.Fatalf("mode = %q, want %q", job.Mode, protocol.ExecutionMode)
	}
	if job.WorkspaceMB != 64 || job.TimeoutSeconds != 60 {
		t.Fatalf("job = %+v, want workspace 64 timeout 60", job)
	}
}

func TestCreatePublishFailureIs502(t *testing.T) {
	api, rc := newTestAPI(t)
	api.records = &stubStore{fn: func(string) (*protocol.ExecutionRecord, error) { return nil, ErrExecutionNotFound }}

	// Close the redis client so PushJob fails deterministically.
	if err := rc.Close(); err != nil {
		t.Fatalf("close redis: %v", err)
	}

	if rec := do(t, api, http.MethodPost, "/", `{"runtime":"exec","workspace_mb":64,"timeout_seconds":30}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d want 502: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateTimeoutIs504(t *testing.T) {
	api, _ := newTestAPI(t)
	api.readyTimeout = 40 * time.Millisecond
	api.pollInterval = 5 * time.Millisecond
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateCreating}, nil
	}}

	rec := do(t, api, http.MethodPost, "/", `{"runtime":"exec","workspace_mb":64,"timeout_seconds":30}`)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("got %d want 504: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateRejectsOversizedTimeout(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = readyStore("e1")
	body := fmt.Sprintf(`{"runtime":"exec","workspace_mb":64,"timeout_seconds":%d}`, protocol.MaxTimeoutSeconds+1)
	if rec := do(t, api, http.MethodPost, "/", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateRejectsInvalidID(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = readyStore("e1")
	if rec := do(t, api, http.MethodPost, "/", `{"runtime":"exec","workspace_mb":64,"timeout_seconds":30,"id":"a/b"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestTimeoutErrorBodyIncludesGeneratedID(t *testing.T) {
	api, _ := newTestAPI(t)
	api.readyTimeout = 30 * time.Millisecond
	api.pollInterval = 5 * time.Millisecond
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateCreating}, nil
	}}

	rec := do(t, api, http.MethodPost, "/", `{"runtime":"exec","workspace_mb":64,"timeout_seconds":30}`)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("got %d want 504", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v (%s)", err, rec.Body.String())
	}
	if body["id"] == "" {
		t.Fatalf("timeout body has no id: %v", body)
	}
}

func TestGetRejectsInvalidID(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = readyStore("e1")
	if rec := do(t, api, http.MethodGet, "/a%20b", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestProxyMapsWorker401To502(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer worker.Close()

	api, _ := newTestAPI(t)
	api.controlToken = "wrong"
	addr := strings.TrimPrefix(worker.URL, "http://")
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateReady, WorkerAddr: addr}, nil
	}}

	if rec := do(t, api, http.MethodPost, "/e1/exec", `{"argv":["true"]}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d want 502 for a worker 401: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateFailedRecordIs502(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateFailed, Error: "boom"}, nil
	}}

	rec := do(t, api, http.MethodPost, "/", `{"runtime":"exec","workspace_mb":64,"timeout_seconds":30}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d want 502: %s", rec.Code, rec.Body.String())
	}
}

func TestGet(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		if id != "e1" {
			return nil, ErrExecutionNotFound
		}
		return &protocol.ExecutionRecord{ID: "e1", State: protocol.ExecutionStateReady}, nil
	}}

	if rec := do(t, api, http.MethodGet, "/e1", ""); rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, api, http.MethodGet, "/missing", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d want 404", rec.Code)
	}
}

func TestExecProxiesToWorker(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	var gotMethod string
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"exit_code":7,"stdout":"hi","stderr":"","timed_out":false}`)
	}))
	defer worker.Close()

	api, _ := newTestAPI(t)
	api.controlToken = "ctrl-secret"
	addr := strings.TrimPrefix(worker.URL, "http://")
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateReady, WorkerAddr: addr}, nil
	}}

	rec := do(t, api, http.MethodPost, "/e1/exec", `{"argv":["sh","-c","echo hi"],"timeout_seconds":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200: %s", rec.Code, rec.Body.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/executions/e1/exec" {
		t.Fatalf("worker saw %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer ctrl-secret" {
		t.Fatalf("worker auth = %q, want bearer ctrl-secret", gotAuth)
	}
	var req protocol.ExecRequest
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("proxied body not an ExecRequest: %v (%s)", err, gotBody)
	}
	if len(req.Argv) != 3 || req.Argv[2] != "echo hi" {
		t.Fatalf("argv = %v", req.Argv)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["exit_code"].(float64) != 7 {
		t.Fatalf("exit_code = %v, want 7", resp["exit_code"])
	}
}

func TestExecPropagatesWorkerStatus(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "execution busy", http.StatusConflict)
	}))
	defer worker.Close()

	api, _ := newTestAPI(t)
	addr := strings.TrimPrefix(worker.URL, "http://")
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateReady, WorkerAddr: addr}, nil
	}}

	if rec := do(t, api, http.MethodPost, "/e1/exec", `{"argv":["true"]}`); rec.Code != http.StatusConflict {
		t.Fatalf("got %d want 409", rec.Code)
	}
}

func TestExecRejectsNonReady(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateStopped, WorkerAddr: "127.0.0.1:1"}, nil
	}}
	if rec := do(t, api, http.MethodPost, "/e1/exec", `{"argv":["true"]}`); rec.Code != http.StatusConflict {
		t.Fatalf("got %d want 409", rec.Code)
	}
}

func TestExecEmptyArgvIs400(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = readyStore("e1")
	if rec := do(t, api, http.MethodPost, "/e1/exec", `{"argv":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d want 400", rec.Code)
	}
}

func TestExecUnknownIs404(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = &stubStore{fn: func(string) (*protocol.ExecutionRecord, error) { return nil, ErrExecutionNotFound }}
	if rec := do(t, api, http.MethodPost, "/missing/exec", `{"argv":["true"]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d want 404", rec.Code)
	}
}

func TestDeleteProxiesToWorker(t *testing.T) {
	var gotMethod, gotPath string
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"e1","state":"stopped"}`)
	}))
	defer worker.Close()

	api, _ := newTestAPI(t)
	addr := strings.TrimPrefix(worker.URL, "http://")
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateReady, WorkerAddr: addr}, nil
	}}

	rec := do(t, api, http.MethodDelete, "/e1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200: %s", rec.Code, rec.Body.String())
	}
	if gotMethod != http.MethodDelete || gotPath != "/executions/e1" {
		t.Fatalf("worker saw %s %s", gotMethod, gotPath)
	}
}

func TestDeleteUnknownIs404(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = &stubStore{fn: func(string) (*protocol.ExecutionRecord, error) { return nil, ErrExecutionNotFound }}
	if rec := do(t, api, http.MethodDelete, "/missing", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d want 404", rec.Code)
	}
}

func TestGetExecRecordProxiesToWorker(t *testing.T) {
	var gotMethod, gotPath string
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"exec_id":"exec-1","state":"done","exit_code":7}`)
	}))
	defer worker.Close()

	api, _ := newTestAPI(t)
	addr := strings.TrimPrefix(worker.URL, "http://")
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateReady, WorkerAddr: addr}, nil
	}}

	rec := do(t, api, http.MethodGet, "/e1/exec/exec-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200: %s", rec.Code, rec.Body.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/executions/e1/exec/exec-1" {
		t.Fatalf("worker saw %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(rec.Body.String(), `"exit_code":7`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestGetExecRecordUnknownIs404(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = &stubStore{fn: func(string) (*protocol.ExecutionRecord, error) { return nil, ErrExecutionNotFound }}
	if rec := do(t, api, http.MethodGet, "/missing/exec/exec-1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d want 404", rec.Code)
	}
}

func TestGetExecRecordRejectsInvalidScopedID(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = readyStore("e1")
	if rec := do(t, api, http.MethodGet, "/e1/exec/a%20b", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d want 400", rec.Code)
	}
}

func TestExecEventsProxiesSSEWithLastEventID(t *testing.T) {
	var gotLastEventID string
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLastEventID = r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: stdout\nid: 5\ndata: {\"type\":\"stdout\"}\n\nevent: exited\nid: 6\ndata: {\"type\":\"exited\"}\n\n")
	}))
	defer worker.Close()

	api, _ := newTestAPI(t)
	addr := strings.TrimPrefix(worker.URL, "http://")
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateReady, WorkerAddr: addr}, nil
	}}

	req := httptest.NewRequest(http.MethodGet, "/e1/exec/exec-1/events", nil)
	req.Header.Set("Last-Event-ID", "4")
	rec := httptest.NewRecorder()
	api.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	if gotLastEventID != "4" {
		t.Fatalf("worker saw Last-Event-ID = %q, want 4", gotLastEventID)
	}
	if !strings.Contains(rec.Body.String(), "id: 5\n") || !strings.Contains(rec.Body.String(), "event: exited\n") {
		t.Fatalf("stream body = %q", rec.Body.String())
	}
}

func TestExecStreamTrueProxiesSSE(t *testing.T) {
	var gotBody string
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: stdout\nid: 1\ndata: {\"type\":\"stdout\"}\n\n")
	}))
	defer worker.Close()

	api, _ := newTestAPI(t)
	addr := strings.TrimPrefix(worker.URL, "http://")
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateReady, WorkerAddr: addr}, nil
	}}

	rec := do(t, api, http.MethodPost, "/e1/exec", `{"argv":["sh","-c","echo tick"],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	var proxied map[string]interface{}
	if err := json.Unmarshal([]byte(gotBody), &proxied); err != nil {
		t.Fatalf("proxied body not json: %v (%s)", err, gotBody)
	}
	if proxied["stream"] != true {
		t.Fatalf("proxied body dropped stream flag: %s", gotBody)
	}
	if !strings.Contains(rec.Body.String(), "event: stdout\n") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestSignalProxiesStatus(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "exec is not running", http.StatusConflict)
	}))
	defer worker.Close()

	api, _ := newTestAPI(t)
	addr := strings.TrimPrefix(worker.URL, "http://")
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateReady, WorkerAddr: addr}, nil
	}}

	if rec := do(t, api, http.MethodPost, "/e1/exec/exec-1/signal", `{"signal":"SIGTERM"}`); rec.Code != http.StatusConflict {
		t.Fatalf("got %d want 409: %s", rec.Code, rec.Body.String())
	}
}

func TestSignalMapsWorker401To502(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer worker.Close()

	api, _ := newTestAPI(t)
	api.controlToken = "wrong"
	addr := strings.TrimPrefix(worker.URL, "http://")
	api.records = &stubStore{fn: func(id string) (*protocol.ExecutionRecord, error) {
		return &protocol.ExecutionRecord{ID: id, State: protocol.ExecutionStateReady, WorkerAddr: addr}, nil
	}}

	if rec := do(t, api, http.MethodPost, "/e1/exec/exec-1/signal", `{"signal":"SIGTERM"}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d want 502: %s", rec.Code, rec.Body.String())
	}
}

func TestSignalUnknownExecutionIs404(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = &stubStore{fn: func(string) (*protocol.ExecutionRecord, error) { return nil, ErrExecutionNotFound }}
	if rec := do(t, api, http.MethodPost, "/missing/exec/exec-1/signal", `{"signal":"SIGTERM"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d want 404", rec.Code)
	}
}

func TestAuthRequiredWhenConfigured(t *testing.T) {
	mr := miniredis.RunT(t)
	rc, err := internal.NewRedisClient(mr.Addr())
	if err != nil {
		t.Fatalf("NewRedisClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	api := NewExecutionsAPI(rc, nil, "api-secret", "")
	api.records = readyStore("e1")

	req := httptest.NewRequest(http.MethodGet, "/e1", nil)
	rec := httptest.NewRecorder()
	api.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token got %d want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/e1", nil)
	req.Header.Set("Authorization", "Bearer api-secret")
	rec = httptest.NewRecorder()
	api.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with token got %d want 200", rec.Code)
	}
}
