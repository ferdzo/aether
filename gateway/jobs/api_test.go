package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aether/gateway/internal"
	"aether/shared/protocol"

	"github.com/alicebob/miniredis/v2"
)

// newTestAPI wires a JobsAPI to a throwaway miniredis so the publish path is
// exercised for real. The record store is intentionally nil: POST never reads
// it, and GET tests install themselves.
func newTestAPI(t *testing.T) (*JobsAPI, *internal.RedisClient) {
	t.Helper()

	mr := miniredis.RunT(t)
	rc, err := internal.NewRedisClient(mr.Addr())
	if err != nil {
		t.Fatalf("NewRedisClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	return NewJobsAPI(rc, nil, ""), rc
}

func do(t *testing.T, api *JobsAPI, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	api.Routes().ServeHTTP(rec, req)
	return rec
}

func TestSubmitValidation(t *testing.T) {
	api, _ := newTestAPI(t)

	cases := []struct {
		name string
		body string
	}{
		{"missing runtime", `{"command":["true"]}`},
		{"empty command", `{"runtime":"job","command":[]}`},
		{"command omitted", `{"runtime":"job"}`},
		{"unsupported mode", `{"runtime":"job","command":["true"],"mode":"session"}`},
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

func TestSubmitPublishesStreamEntry(t *testing.T) {
	api, rc := newTestAPI(t)

	body := `{"runtime":"job","command":["sh","-c","echo hi"],"timeout_seconds":30,"vcpu":2,"memory_mb":256}`
	rec := do(t, api, http.MethodPost, "/", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d want 202: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	if resp["state"] != protocol.JobStateProvisioning {
		t.Fatalf("response state = %q, want %q", resp["state"], protocol.JobStateProvisioning)
	}

	msgs, err := rc.Client().XRange(context.Background(), protocol.StreamProvision, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 stream entry, got %d", len(msgs))
	}

	raw, ok := msgs[0].Values["job"].(string)
	if !ok {
		t.Fatalf("stream entry missing job field: %v", msgs[0].Values)
	}
	var job protocol.Job
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		t.Fatalf("job payload is not valid JSON: %v", err)
	}

	if job.Mode != "process" {
		t.Fatalf("mode = %q, want process", job.Mode)
	}
	if job.Runtime != "job" {
		t.Fatalf("runtime = %q, want job", job.Runtime)
	}
	if len(job.Command) != 3 || job.Command[0] != "sh" || job.Command[2] != "echo hi" {
		t.Fatalf("command not preserved: %v", job.Command)
	}
	if job.TimeoutSeconds != 30 || job.VCPU != 2 || job.MemoryMB != 256 {
		t.Fatalf("resource fields not preserved: %+v", job)
	}
	if !strings.HasPrefix(job.JobID, "job-") {
		t.Fatalf("job id = %q, want job- prefix", job.JobID)
	}
	if job.JobID != resp["job_id"] {
		t.Fatalf("published job id %q != response job id %q", job.JobID, resp["job_id"])
	}
	if !strings.HasPrefix(job.RequestID, "req-") {
		t.Fatalf("request id = %q, want req- prefix", job.RequestID)
	}
	if job.RequestID != resp["request_id"] {
		t.Fatalf("published request id %q != response request id %q", job.RequestID, resp["request_id"])
	}
}

func TestSubmitHonorsCallerID(t *testing.T) {
	api, rc := newTestAPI(t)

	rec := do(t, api, http.MethodPost, "/", `{"id":"job-caller","runtime":"job","command":["true"]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d want 202: %s", rec.Code, rec.Body.String())
	}

	msgs, err := rc.Client().XRange(context.Background(), protocol.StreamProvision, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 stream entry, got %d", len(msgs))
	}
	var job protocol.Job
	if err := json.Unmarshal([]byte(msgs[0].Values["job"].(string)), &job); err != nil {
		t.Fatalf("unmarshal job: %v", err)
	}
	if job.JobID != "job-caller" {
		t.Fatalf("job id = %q, want caller-supplied job-caller", job.JobID)
	}
}

// stubStore is an in-memory JobRecordStore for GET tests.
type stubStore struct {
	rec *protocol.JobRecord
	err error
}

func (s stubStore) GetJob(context.Context, string) (*protocol.JobRecord, error) {
	return s.rec, s.err
}

func TestGetNotFound(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = stubStore{err: ErrJobNotFound}

	rec := do(t, api, http.MethodGet, "/job-missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestGetReturnsRecord(t *testing.T) {
	api, _ := newTestAPI(t)
	api.records = stubStore{rec: &protocol.JobRecord{
		JobID:    "job-1",
		State:    protocol.JobStateDone,
		ExitCode: 42,
	}}

	rec := do(t, api, http.MethodGet, "/job-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200: %s", rec.Code, rec.Body.String())
	}

	var got protocol.JobRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a job record: %v", err)
	}
	if got.JobID != "job-1" || got.State != protocol.JobStateDone || got.ExitCode != 42 {
		t.Fatalf("unexpected record: %+v", got)
	}
}

func TestAuthRequired(t *testing.T) {
	api, _ := newTestAPI(t)
	api.authToken = "secret"

	body := `{"runtime":"job","command":["true"]}`
	rec := do(t, api, http.MethodPost, "/", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without token: got %d want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	authed := httptest.NewRecorder()
	api.Routes().ServeHTTP(authed, req)
	if authed.Code != http.StatusAccepted {
		t.Fatalf("with token: got %d want 202: %s", authed.Code, authed.Body.String())
	}
}
