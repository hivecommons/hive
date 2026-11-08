package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/cmdlog"
	"github.com/hivecommons/hive/crux/runs"
	"github.com/hivecommons/hive/crux/signals"
)

// runsCaller is the S0 principal behind the request, as the run service
// sees it: a run is recorded under its name, and only it (or the operator)
// may point a run at a checkout, read the run, or cancel it. A context
// that did not pass authenticate (a unix-socket server without a token)
// is the operator, which is what that transport already establishes.
func (s *Server) runsCaller(r *http.Request) runs.Caller {
	p, ok := CallerFrom(r.Context())
	if !ok {
		p = s.operator()
	}
	return runs.Caller{Name: p.Name, Operator: p.IsOperator()}
}

// registerRuns mounts the contained-run family (RFC crux#114 S2 + S7).
// The routes are always present so a server without a backend answers
// with a clear not_supported instead of a bare 404. Like checkouts, the
// family admits project principals; ownership is enforced per record.
func (s *Server) registerRuns(mux *http.ServeMux) {
	mux.HandleFunc("POST "+api.PathRuns, s.handleRunsCreate)
	mux.HandleFunc("GET "+api.PathRuns+"/{id}", s.handleRunsGet)
	mux.HandleFunc("POST "+api.PathRuns+"/{id}/cancel", s.handleRunsCancel)
	mux.HandleFunc("GET "+api.PathRuns+"/{id}/cmdlog", s.handleRunsCmdlog)
	mux.HandleFunc("GET "+api.PathRuns+"/{id}/signals", s.handleRunsSignalsList)
	mux.HandleFunc("POST "+api.PathRuns+"/{id}/signals", s.handleRunsSignalsEmit)
	s.registerRunsSaved(mux)
	s.workService(api.PathRuns)
	if s.Runs != nil && s.Runs.Signals != nil {
		s.Runs.Signals.SetObserver(s.publishRunSignal)
	}
}

// ownedRun loads {id} and enforces that the caller owns it or is the
// operator; on failure the response has been written.
func (s *Server) ownedRun(w http.ResponseWriter, r *http.Request) (api.ContainedRun, bool) {
	run, err := s.Runs.Get(r.PathValue("id"))
	if err != nil {
		s.writeRunsError(w, err)
		return api.ContainedRun{}, false
	}
	if caller := s.runsCaller(r); !caller.Operator && run.Owner != caller.Name {
		s.writeError(w, http.StatusForbidden, api.CodeForbidden, fmt.Sprintf("run %s is owned by %q, not by %q", run.ID, run.Owner, caller.Name))
		return api.ContainedRun{}, false
	}
	return run, true
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
	run, created, err := s.Runs.Create(s.LaunchContext, s.runsCaller(r), req)
	if err != nil {
		s.writeRunsError(w, err)
		return
	}
	if !created {
		// Idempotency hit: the earlier record, whatever state it is in.
		s.writeJSON(w, http.StatusOK, run)
		return
	}
	// A step session (profile agent) stays open until it is cancelled, so
	// it is always answered as accepted; its steps are what the caller waits
	// on.
	if req.Detach || run.Profile == api.RunProfileAgent {
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
	run, ok := s.ownedRun(w, r)
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleRunsCancel(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	if _, ok := s.ownedRun(w, r); !ok {
		return
	}
	run, err := s.Runs.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRunsError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, run)
}

// handleRunsCmdlog is GET /v1/runs/{id}/cmdlog (S4): the run's command
// log after ?since_seq, at most ?limit records. The caller must own the
// run or be the operator, the same rule as the record itself.
func (s *Server) handleRunsCmdlog(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	run, ok := s.ownedRun(w, r)
	if !ok {
		return
	}
	sinceSeq, limit, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	page, err := s.Runs.Commands(run.ID, sinceSeq, limit)
	if err != nil {
		if errors.Is(err, cmdlog.ErrCorrupt) {
			s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, err.Error())
			return
		}
		s.writeRunsError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, page)
}

// pageParams reads the ?since_seq and ?limit paging of the run logs; on a
// malformed value a 400 has been written.
func (s *Server) pageParams(w http.ResponseWriter, r *http.Request) (sinceSeq int64, limit int, ok bool) {
	q := r.URL.Query()
	if v := q.Get("since_seq"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "since_seq must be a non-negative integer")
			return 0, 0, false
		}
		sinceSeq = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "limit must be a positive integer")
			return 0, 0, false
		}
		limit = n
	}
	return sinceSeq, limit, true
}

// handleRunsSignalsList is GET /v1/runs/{id}/signals (S5): the run's turn
// signals after ?since_seq, at most ?limit of them, for its owner or the
// operator.
func (s *Server) handleRunsSignalsList(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	run, ok := s.ownedRun(w, r)
	if !ok {
		return
	}
	sinceSeq, limit, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	page, err := s.Runs.ReadSignals(run.ID, sinceSeq, limit)
	if err != nil {
		s.writeSignalsError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, page)
}

// handleRunsSignalsEmit is POST /v1/runs/{id}/signals (S5): the run's
// owner (or the operator) reports where the turn is. The source is always
// caller; started and done stay the runner's.
func (s *Server) handleRunsSignalsEmit(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	run, ok := s.ownedRun(w, r)
	if !ok {
		return
	}
	var in api.RunSignalRequest
	if !s.readJSON(w, r, &in) {
		return
	}
	sig, err := s.Runs.AppendSignal(run.ID, signals.Signal{Kind: in.Kind, Source: api.SignalSourceCaller, Message: in.Message, Data: in.Data})
	if err != nil {
		s.writeSignalsError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, sig)
}

// writeSignalsError maps the signal book's sentinels onto wire codes and
// falls back to the run service's mapping.
func (s *Server) writeSignalsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runs.ErrNoSignals):
		s.writeError(w, http.StatusNotImplemented, api.CodeNotSupported, err.Error())
	case errors.Is(err, signals.ErrBadRequest):
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, err.Error())
	case errors.Is(err, signals.ErrClosed):
		s.writeError(w, http.StatusConflict, api.CodeSignalsClosed, err.Error())
	case errors.Is(err, signals.ErrCorrupt):
		s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, err.Error())
	default:
		s.writeRunsError(w, err)
	}
}

// publishRunSignal relays one durable turn signal onto /v1/events.
func (s *Server) publishRunSignal(run string, sig api.RunSignal) {
	s.publishData(api.EventRunSignal, "", api.RunSignalEvent{Run: run, RunSignal: sig})
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
	case errors.Is(err, runs.ErrLive), errors.Is(err, runs.ErrNoRequest):
		s.writeError(w, http.StatusConflict, api.CodeConflict, err.Error())
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
