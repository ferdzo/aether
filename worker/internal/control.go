package internal

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aether/shared/logger"
	"aether/shared/protocol"
)

// maxControlBodyBytes bounds a control-API request body (the exec request).
const maxControlBodyBytes = 1 << 20 // 1 MiB

// controlReadHeaderTimeout bounds how long a client may take to send request
// headers, so a stuck client cannot pin a connection goroutine.
const controlReadHeaderTimeout = 10 * time.Second

// ControlServer is the worker's small HTTP control API for the gateway. It is
// deliberately minimal: no streaming, no websockets. The gateway proxies
// execution exec/destroy calls to the worker that owns the execution, which
// then talks to the guest over vsock.
//
// Authentication is an optional bearer token (WORKER_CONTROL_TOKEN). Unset
// leaves the API open, which is dev-only: a control call can run arbitrary
// commands inside a guest.
type ControlServer struct {
	worker *Worker
	token  string
	srv    *http.Server
	ln     net.Listener
}

// NewControlServer builds the control server for a worker. token == "" disables
// authentication (dev-only, see the type comment).
func NewControlServer(worker *Worker, token string) *ControlServer {
	return &ControlServer{worker: worker, token: token}
}

// Handler returns the control routes. Go 1.22 method+wildcard patterns keep
// this dependency-free (the worker does not import chi).
func (s *ControlServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /executions/{id}/exec", s.requireAuth(s.handleExec))
	mux.HandleFunc("GET /executions/{id}/exec/{exec_id}", s.requireAuth(s.handleGetExecRecord))
	mux.HandleFunc("GET /executions/{id}/exec/{exec_id}/events", s.requireAuth(s.handleExecEvents))
	mux.HandleFunc("POST /executions/{id}/exec/{exec_id}/signal", s.requireAuth(s.handleSignal))
	mux.HandleFunc("DELETE /executions/{id}", s.requireAuth(s.handleDestroy))
	return mux
}

// Start listens on addr and serves in the background. It returns once the
// listener is bound so a caller can fail fast on a port conflict.
func (s *ControlServer) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	if s.token == "" {
		logger.Warn("worker control API is UNSET and therefore unauthenticated; it binds all interfaces and can run arbitrary commands in a guest. Set WORKER_CONTROL_TOKEN to require a bearer token.", "addr", addr)
	}
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: controlReadHeaderTimeout,
	}
	go func() {
		logger.Info("worker control API listening", "addr", addr)
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("worker control API error", "error", err)
		}
	}()
	return nil
}

// Shutdown drains the control server.
func (s *ControlServer) Shutdown(ctx context.Context) error {
	if s == nil || s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *ControlServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// validControlID applies the same id rules the gateway does, so a path id is
// always a safe etcd key suffix and file-name element.
func validControlID(w http.ResponseWriter, id string) bool {
	if id == "" {
		http.Error(w, "execution id is required", http.StatusBadRequest)
		return false
	}
	if err := protocol.ValidJobID(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// validExecScopedID validates the second path id in exec-scoped routes. An exec
// id is host-generated but still bounds-checked so a crafted path cannot reach
// a map or a worker path unchecked.
func validExecScopedID(w http.ResponseWriter, id string) bool {
	if id == "" {
		http.Error(w, "exec id is required", http.StatusBadRequest)
		return false
	}
	if err := protocol.ValidJobID(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// writeExecPathError maps the execution/exec error set to the control status
// codes used by the exec-scoped routes.
func writeExecPathError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errExecutionNotFound), errors.Is(err, errExecNotFound):
		http.Error(w, "exec not found", http.StatusNotFound)
	case errors.Is(err, errExecutionBusy), errors.Is(err, errGuestBusy):
		http.Error(w, "execution busy", http.StatusConflict)
	case errors.Is(err, errExecNotRunning):
		http.Error(w, "exec is not running", http.StatusConflict)
	case errors.Is(err, errSignalInvalid):
		http.Error(w, "unsupported signal", http.StatusBadRequest)
	default:
		http.Error(w, "exec failed", http.StatusBadGateway)
	}
}

// POST /executions/{id}/exec
//
// Without "stream" it blocks and returns 200 {exit_code, stdout, stderr,
// timed_out}; 404 unknown; 409 busy; 502 when the guest call fails. With
// "stream": true it responds text/event-stream and streams the events until the
// terminal exited event.
func (s *ControlServer) handleExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validControlID(w, id) {
		return
	}

	var req struct {
		Argv           []string          `json:"argv"`
		Cwd            string            `json:"cwd"`
		Env            map[string]string `json:"env"`
		TimeoutSeconds int               `json:"timeout_seconds"`
		Stream         bool              `json:"stream"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxControlBodyBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if len(req.Argv) == 0 {
		http.Error(w, "argv is required and must not be empty", http.StatusBadRequest)
		return
	}
	if req.TimeoutSeconds < 0 {
		http.Error(w, "timeout_seconds must not be negative", http.StatusBadRequest)
		return
	}
	if req.TimeoutSeconds > protocol.MaxTimeoutSeconds {
		http.Error(w, fmt.Sprintf("timeout_seconds must be at most %d", protocol.MaxTimeoutSeconds), http.StatusBadRequest)
		return
	}

	execReq := protocol.ExecRequest{
		Argv:           req.Argv,
		Cwd:            req.Cwd,
		Env:            req.Env,
		TimeoutSeconds: req.TimeoutSeconds,
	}

	if req.Stream {
		rec, err := s.worker.StartExecStream(r.Context(), id, execReq)
		if err != nil {
			writeExecPathError(w, err)
			return
		}
		// The exec id is host-generated; surface it before the SSE headers so a
		// caller can address the record and the event buffer afterwards.
		w.Header().Set("X-Exec-ID", rec.ID)
		// A reconnect header on a brand-new exec is meaningless; start at 0.
		streamExecRecord(w, r, rec, 0)
		return
	}

	res, err := s.worker.ExecExecution(r.Context(), id, execReq)
	switch {
	case errors.Is(err, errExecutionNotFound):
		http.Error(w, "execution not found", http.StatusNotFound)
		return
	case errors.Is(err, errExecutionBusy), errors.Is(err, errGuestBusy):
		http.Error(w, "execution busy", http.StatusConflict)
		return
	case err != nil:
		logger.Warn("exec failed", "execution_id", id, "error", err)
		http.Error(w, "exec failed", http.StatusBadGateway)
		return
	}

	// stdout/stderr are JSON strings, which substitute invalid UTF-8. Callers
	// that need the exact bytes of a non-UTF-8 output use stdout_b64/stderr_b64
	// (standard base64), which are byte-exact.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"exit_code":  res.ExitCode,
		"stdout":     res.Stdout,
		"stderr":     res.Stderr,
		"stdout_b64": base64.StdEncoding.EncodeToString([]byte(res.Stdout)),
		"stderr_b64": base64.StdEncoding.EncodeToString([]byte(res.Stderr)),
		"timed_out":  res.TimedOut,
	})
}

// GET /executions/{id}/exec/{exec_id} → the stored record as JSON, 404 unknown.
func (s *ControlServer) handleGetExecRecord(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	execID := r.PathValue("exec_id")
	if !validControlID(w, id) || !validExecScopedID(w, execID) {
		return
	}
	view, err := s.worker.ExecRecord(id, execID)
	if err != nil {
		writeExecPathError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(view)
}

// GET /executions/{id}/exec/{exec_id}/events → SSE: attach to a running exec, or
// replay a finished one's buffer, then close. 404 for an unknown exec.
func (s *ControlServer) handleExecEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	execID := r.PathValue("exec_id")
	if !validControlID(w, id) || !validExecScopedID(w, execID) {
		return
	}
	exec := s.worker.lookupExecution(id)
	if exec == nil {
		http.Error(w, "exec not found", http.StatusNotFound)
		return
	}
	rec, ok := exec.getExecRecord(execID)
	if !ok {
		http.Error(w, "exec not found", http.StatusNotFound)
		return
	}
	after := parseLastEventID(r.Header.Get("Last-Event-ID"))
	streamExecRecord(w, r, rec, after)
}

// POST /executions/{id}/exec/{exec_id}/signal
//
// 200 on delivery, 404 unknown exec, 409 when not running, 400 invalid signal,
// 502 on a transport failure.
func (s *ControlServer) handleSignal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	execID := r.PathValue("exec_id")
	if !validControlID(w, id) || !validExecScopedID(w, execID) {
		return
	}

	var req struct {
		Signal string `json:"signal"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxControlBodyBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := s.worker.SignalExec(id, execID, req.Signal); err != nil {
		if !errors.Is(err, errSignalInvalid) {
			logger.Warn("signal delivery failed", "execution_id", id, "exec_id", execID, "signal", req.Signal, "error", err)
		}
		writeExecPathError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"id":      id,
		"exec_id": execID,
		"signal":  req.Signal,
		"state":   "delivered",
	})
}

// sseWriteTimeout bounds a single SSE write to a slow client. The exec is never
// affected by this: a write past the deadline fails and only this handler
// unwinds.
const sseWriteTimeout = 30 * time.Second

// parseLastEventID parses a Last-Event-ID header. Anything malformed, absent or
// empty means "from the beginning" (0). It never fails the request: a resume is
// best-effort and the buffer's loss marker reports a dropped position.
func parseLastEventID(h string) uint64 {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	n, err := strconv.ParseUint(h, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// writeSSEEvent frames one event as event/id/data. seq == 0 omits the id (used
// for the loss marker, which must not advance a client's Last-Event-ID).
func writeSSEEvent(w http.ResponseWriter, seq uint64, ev protocol.ExecEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if seq == 0 {
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", ev.Type, seq, b)
	return err
}

// streamExecRecord streams buffered events to w, following the exec live until
// its terminal event. It is the one SSE renderer used by both the initial
// streamed exec and a reconnect; after afterSeq it resumes from the buffer.
//
// The writer (the exec goroutine) never touches w, so a slow client cannot
// block the exec. Deadlines bound each write so a stalled client unwinds.
func streamExecRecord(w http.ResponseWriter, r *http.Request, rec *execRecord, afterSeq uint64) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	last := afterSeq

	// A disconnected client must unblock this handler promptly even if the exec
	// is silent: wake the buffer when the request context is cancelled. The
	// watcher is scoped to this reader and does not touch the exec or the
	// buffer's shared state.
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-r.Context().Done():
			rec.buf.wake()
		case <-stopWatch:
		}
	}()

	for {
		events, lost, done, floor := rec.buf.waitAndRead(last, r.Context().Done())
		if lost {
			// Deadlines are best-effort: a ResponseWriter that does not support
			// them (or a test recorder) must not abort the stream.
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
			_, _ = fmt.Fprintf(w, "event: lost\ndata: {\"lost\":true}\n\n")
			_ = rc.Flush()
			if floor > 0 {
				last = floor - 1
			}
		}
		for _, be := range events {
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
			if err := writeSSEEvent(w, be.Seq, be.Event); err != nil {
				return
			}
			last = be.Seq
			if err := rc.Flush(); err != nil {
				return
			}
		}
		if done {
			return
		}
		if r.Context().Err() != nil {
			return
		}
	}
}

// DELETE /executions/{id} → destroy; idempotent.
func (s *ControlServer) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validControlID(w, id) {
		return
	}
	if err := s.worker.DestroyExecution(id); err != nil {
		logger.Error("failed to destroy execution", "execution_id", id, "error", err)
		http.Error(w, "failed to destroy execution", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"id":    id,
		"state": protocol.ExecutionStateStopped,
	})
}
