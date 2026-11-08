package serve

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/hivecommons/hive/crux/agentstep"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/runs"
)

// Agent step routes (S6). A step is posted to an open agent-profile run
// and answers when the step has finished; the step runs under the
// session, not the request, so a caller that goes away leaves it in
// flight and reads it back with GET.
func (s *Server) registerSteps(mux *http.ServeMux) {
	mux.HandleFunc("POST "+api.PathRuns+"/{id}/steps", s.handleStepsRun)
	mux.HandleFunc("GET "+api.PathRuns+"/{id}/steps", s.handleStepsList)
	mux.HandleFunc("GET "+api.PathRuns+"/{id}/steps/{n}", s.handleStepsGet)
}

func (s *Server) handleStepsRun(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	run, ok := s.ownedRun(w, r)
	if !ok {
		return
	}
	var req api.StepRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	step, err := s.Runs.RunStep(r.Context(), run.ID, req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		s.writeStepsError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, step)
}

func (s *Server) handleStepsList(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	run, ok := s.ownedRun(w, r)
	if !ok {
		return
	}
	list, err := s.Runs.Steps(run.ID)
	if err != nil {
		s.writeStepsError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleStepsGet(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	run, ok := s.ownedRun(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "step number must be a positive integer")
		return
	}
	step, err := s.Runs.Step(run.ID, n)
	if err != nil {
		s.writeStepsError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, step)
}

func (s *Server) writeStepsError(w http.ResponseWriter, err error) {
	var reqErr *agentstep.RequestError
	var unsupported *agentstep.UnsupportedError
	switch {
	case errors.Is(err, agentstep.ErrNotFound):
		s.writeError(w, http.StatusNotFound, api.CodeNotFound, "step not found")
	case errors.Is(err, runs.ErrNotSession):
		s.writeError(w, http.StatusConflict, api.CodeNotSession, err.Error())
	case errors.Is(err, runs.ErrStepBusy):
		s.writeError(w, http.StatusConflict, api.CodeStepBusy, err.Error())
	case errors.As(err, &reqErr):
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, err.Error())
	case errors.As(err, &unsupported):
		// The refusal is also on the record: GET steps/{n} shows it.
		s.writeError(w, http.StatusNotImplemented, api.CodeUnsupported, err.Error())
	default:
		s.writeRunsError(w, err)
	}
}
