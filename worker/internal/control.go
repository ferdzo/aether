package internal

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"aether/shared/logger"
	"aether/shared/protocol"
)

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
	s.srv = &http.Server{Handler: s.Handler()}
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

// POST /executions/{id}/exec
//
// 200 {exit_code, stdout, stderr, timed_out}; 404 unknown; 409 busy; 502 when
// the guest call fails.
func (s *ControlServer) handleExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "execution id is required", http.StatusBadRequest)
		return
	}

	var req struct {
		Argv           []string          `json:"argv"`
		Cwd            string            `json:"cwd"`
		Env            map[string]string `json:"env"`
		TimeoutSeconds int               `json:"timeout_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if len(req.Argv) == 0 {
		http.Error(w, "argv is required and must not be empty", http.StatusBadRequest)
		return
	}

	res, err := s.worker.ExecExecution(r.Context(), id, protocol.ExecRequest{
		Argv:           req.Argv,
		Cwd:            req.Cwd,
		Env:            req.Env,
		TimeoutSeconds: req.TimeoutSeconds,
	})
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"exit_code": res.ExitCode,
		"stdout":    res.Stdout,
		"stderr":    res.Stderr,
		"timed_out": res.TimedOut,
	})
}

// DELETE /executions/{id} → destroy; idempotent.
func (s *ControlServer) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "execution id is required", http.StatusBadRequest)
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
