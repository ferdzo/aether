package internal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aether/shared/protocol"
)

func controlDo(t *testing.T, h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func registerTestExecution(w *Worker, id string) {
	w.registerExecution(&Execution{
		ID:            id,
		instance:      &Instance{ID: "i", FunctionID: "f"},
		workspacePath: "/tmp/ws.ext4",
		vsockPath:     "vsock",
		workerAddr:    "127.0.0.1:9091",
	})
}

func TestControlExecSuccess(t *testing.T) {
	w := newJobWorker(t)
	withExecOnGuest(t, func(_ context.Context, _ string, id string, req protocol.ExecRequest, _ func(protocol.ExecEvent)) (protocol.ExecResult, error) {
		if id != "c1" {
			t.Fatalf("guest exec id = %q, want c1", id)
		}
		if len(req.Argv) != 2 || req.Argv[0] != "echo" {
			t.Fatalf("argv = %v, want [echo hello]", req.Argv)
		}
		return protocol.ExecResult{ExitCode: 0, Stdout: "hello\n"}, nil
	})
	registerTestExecution(w, "c1")

	rec := controlDo(t, NewControlServer(w, "").Handler(), http.MethodPost, "/executions/c1/exec",
		`{"argv":["echo","hello"]}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		TimedOut bool   `json:"timed_out"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body.String())
	}
	if got.ExitCode != 0 || got.Stdout != "hello\n" {
		t.Fatalf("response = %+v, want exit 0 stdout hello", got)
	}
}

func TestControlExecUnknownIs404(t *testing.T) {
	w := newJobWorker(t)
	rec := controlDo(t, NewControlServer(w, "").Handler(), http.MethodPost, "/executions/missing/exec",
		`{"argv":["true"]}`, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestControlExecBusyIs409(t *testing.T) {
	w := newJobWorker(t)
	registerTestExecution(w, "c2")
	// Hold the gate as an in-flight exec would.
	if !w.lookupExecution("c2").tryAcquire() {
		t.Fatal("could not acquire gate")
	}
	rec := controlDo(t, NewControlServer(w, "").Handler(), http.MethodPost, "/executions/c2/exec",
		`{"argv":["true"]}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestControlExecGuestBusyIs409(t *testing.T) {
	w := newJobWorker(t)
	withExecOnGuest(t, func(context.Context, string, string, protocol.ExecRequest, func(protocol.ExecEvent)) (protocol.ExecResult, error) {
		return protocol.ExecResult{ExitCode: guestBusyExitCode}, errGuestBusy
	})
	registerTestExecution(w, "c3")
	rec := controlDo(t, NewControlServer(w, "").Handler(), http.MethodPost, "/executions/c3/exec",
		`{"argv":["true"]}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestControlExecGuestFailureIs502(t *testing.T) {
	w := newJobWorker(t)
	withExecOnGuest(t, func(context.Context, string, string, protocol.ExecRequest, func(protocol.ExecEvent)) (protocol.ExecResult, error) {
		return protocol.ExecResult{}, context.DeadlineExceeded
	})
	registerTestExecution(w, "c4")
	rec := controlDo(t, NewControlServer(w, "").Handler(), http.MethodPost, "/executions/c4/exec",
		`{"argv":["true"]}`, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestControlExecEmptyArgvIs400(t *testing.T) {
	w := newJobWorker(t)
	registerTestExecution(w, "c5")
	rec := controlDo(t, NewControlServer(w, "").Handler(), http.MethodPost, "/executions/c5/exec",
		`{"argv":[]}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestControlDestroyIsIdempotent(t *testing.T) {
	w := newJobWorker(t)
	withShutdownGuest(t, func(string) error { return nil })
	withExecutionRecordSeams(t, func(*Registry, protocol.ExecutionRecord) error { return nil }, nil)
	registerTestExecution(w, "c6")

	h := NewControlServer(w, "").Handler()
	if rec := controlDo(t, h, http.MethodDelete, "/executions/c6", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", rec.Code)
	}
	// Second delete is still 200 (idempotent).
	if rec := controlDo(t, h, http.MethodDelete, "/executions/c6", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("second delete status = %d, want 200", rec.Code)
	}
	// A later exec is rejected.
	if rec := controlDo(t, h, http.MethodPost, "/executions/c6/exec", `{"argv":["true"]}`, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("exec after destroy status = %d, want 404", rec.Code)
	}
}

func TestControlExecRejectsOversizedTimeout(t *testing.T) {
	w := newJobWorker(t)
	registerTestExecution(w, "c8")
	rec := controlDo(t, NewControlServer(w, "").Handler(), http.MethodPost, "/executions/c8/exec",
		`{"argv":["true"],"timeout_seconds":99999999999}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestControlRejectsInvalidID(t *testing.T) {
	w := newJobWorker(t)
	h := NewControlServer(w, "").Handler()
	if rec := controlDo(t, h, http.MethodPost, "/executions/a%20b/exec", `{"argv":["true"]}`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("exec invalid id status = %d, want 400", rec.Code)
	}
	if rec := controlDo(t, h, http.MethodDelete, "/executions/a%20b", "", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("destroy invalid id status = %d, want 400", rec.Code)
	}
}

func TestControlAuth(t *testing.T) {
	w := newJobWorker(t)
	withExecOnGuest(t, func(context.Context, string, string, protocol.ExecRequest, func(protocol.ExecEvent)) (protocol.ExecResult, error) {
		return protocol.ExecResult{ExitCode: 0, Stdout: "ok"}, nil
	})
	registerTestExecution(w, "c7")
	h := NewControlServer(w, "secret-token").Handler()

	if rec := controlDo(t, h, http.MethodPost, "/executions/c7/exec", `{"argv":["true"]}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", rec.Code)
	}
	if rec := controlDo(t, h, http.MethodPost, "/executions/c7/exec", `{"argv":["true"]}`, "secret-token"); rec.Code != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}
