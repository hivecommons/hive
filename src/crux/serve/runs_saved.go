package serve

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/runs"
)

// Saved-run-results routes (RFC crux#114 S7) beyond the record itself:
// listing, the explicit restart and the status history. Registered from
// registerRuns; the same ownership rule as the record applies.

// auditRunRestart is the audit action of every restart: who re-ran which
// run as which new one, and why.
const auditRunRestart = "run.restart"

func (s *Server) registerRunsSaved(mux *http.ServeMux) {
	mux.HandleFunc("GET "+api.PathRuns, s.handleRunsList)
	mux.HandleFunc("POST "+api.PathRuns+"/{id}/restart", s.handleRunsRestart)
	mux.HandleFunc("GET "+api.PathRuns+"/{id}/history", s.handleRunsHistory)
}

// handleRunsList is GET /v1/runs: the caller's records, every record for
// the operator, newest first; ?status= narrows to one status, ?limit= caps
// the page.
func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	f := runs.ListFilter{Status: r.URL.Query().Get("status")}
	if caller := s.runsCaller(r); !caller.Operator {
		f.Owner = caller.Name
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "limit must be a positive integer")
			return
		}
		f.Limit = n
	}
	list, err := s.Runs.List(f)
	if err != nil {
		s.writeRunsError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

// handleRunsRestart is POST /v1/runs/{id}/restart: an explicit, audited
// re-run of a finished run by its owner or the operator. A run whose saved
// state is still live is 409 conflict: the runner may be alive, and two
// drivers must not both resume one task.
func (s *Server) handleRunsRestart(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	old, ok := s.ownedRun(w, r)
	if !ok {
		return
	}
	// An empty body is a plain restart; curl -X POST needs no "{}".
	var req api.RunRestartRequest
	if r.ContentLength != 0 && !s.readJSON(w, r, &req) {
		return
	}
	caller := s.runsCaller(r)
	run, created, err := s.Runs.Restart(s.LaunchContext, caller, old.ID, req)
	if err != nil {
		s.writeRunsError(w, err)
		return
	}
	if !created {
		s.writeJSON(w, http.StatusOK, run)
		return
	}
	auditTee{s}.Record(caller.Name, auditRunRestart, "", map[string]any{
		"run": run.ID, "restarted_from": old.ID, "was": old.Status, "reason": req.Reason, "owner": run.Owner,
	})
	if req.Detach || run.Profile == api.RunProfileAgent {
		s.writeJSON(w, http.StatusAccepted, run)
		return
	}
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

// handleRunsHistory is GET /v1/runs/{id}/history: every status the record
// passed through, for its owner or the operator.
func (s *Server) handleRunsHistory(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	run, ok := s.ownedRun(w, r)
	if !ok {
		return
	}
	hist, err := s.Runs.History(run.ID)
	if err != nil {
		s.writeRunsError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, hist)
}
