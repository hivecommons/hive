package serve

import (
	"context"
	"errors"
	"net/http"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/runs"
)

// runsOwner is the principal a contained run is recorded under.
//
// TODO(S0): switch to CallerFrom(r.Context()) once the caller-principal
// change lands; until then every authenticated caller is the operator,
// which is exactly what the bearer-token check already establishes.
func runsOwner(r *http.Request) string {
	return "operator"
}

// registerRuns mounts the contained-run family (RFC crux#114 S2 + S7).
// The routes are always present so a server without a backend answers
// with a clear not_supported instead of a bare 404.
func (s *Server) registerRuns(mux *http.ServeMux) {
	mux.HandleFunc("POST "+api.PathRuns, s.handleRunsCreate)
	mux.HandleFunc("GET "+api.PathRuns+"/{id}", s.handleRunsGet)
	mux.HandleFunc("POST "+api.PathRuns+"/{id}/cancel", s.handleRunsCancel)
}

// runsReady reports whether the run service exists; when it does not, a
// 501 has been written. A service without a backend still serves saved
// records, so only Create checks Configured separately.
func (s *Server) runsReady(w http.ResponseWriter) bool {
	if s.Runs == nil {
		s.writeError(w, http.StatusNotImplemented, api.CodeNotSupported, "contained runs are not enabled on this server")
		return false
	}
	return true
}

func (s *Server) handleRunsCreate(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	var req api.ContainedRunRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	run, created, err := s.Runs.Create(s.LaunchContext, runsOwner(r), req)
	if err != nil {
		s.writeRunsError(w, err)
		return
	}
	if !created {
		// Idempotency hit: the earlier record, whatever state it is in.
		s.writeJSON(w, http.StatusOK, run)
		return
	}
	if req.Detach {
		s.writeJSON(w, http.StatusAccepted, run)
		return
	}
	// The run itself is bound to LaunchContext; only the wait is bound to
	// the request, so a caller that goes away leaves the run in flight.
	run, err = s.Runs.Wait(r.Context(), run.ID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		s.writeRunsError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleRunsGet(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	run, err := s.Runs.Get(r.PathValue("id"))
	if err != nil {
		s.writeRunsError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleRunsCancel(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	run, err := s.Runs.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRunsError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, run)
}

// writeRunsError maps the run service's typed errors onto wire codes. The
// refusals keep their own codes so a caller can tell "you asked for
// something wrong" from "this backend cannot enforce that".
func (s *Server) writeRunsError(w http.ResponseWriter, err error) {
	var reqErr *runs.RequestError
	var forbidden *runs.ForbiddenError
	var unsupported *runs.UnsupportedError
	switch {
	case errors.Is(err, runs.ErrNotConfigured):
		s.writeError(w, http.StatusNotImplemented, api.CodeNotSupported, err.Error())
	case errors.Is(err, runs.ErrNotFound):
		s.writeError(w, http.StatusNotFound, api.CodeNotFound, "run not found")
	case errors.As(err, &reqErr):
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, err.Error())
	case errors.As(err, &forbidden):
		s.writeError(w, http.StatusForbidden, api.CodeForbidden, err.Error())
	case errors.As(err, &unsupported):
		s.writeError(w, http.StatusNotImplemented, api.CodeUnsupported, err.Error())
	default:
		s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, err.Error())
	}
}
