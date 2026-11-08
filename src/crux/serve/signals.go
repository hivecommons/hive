package serve

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/signals"
)

// Turn signals (hivecommons/crux#114 S5). The per-run log is read and
// written under the run's ownership rule; the per-agent kick outcome is
// an agent RPC, so the operator alone reads it.

// handleRunsSignals is GET /v1/runs/{id}/signals: the run's signals after
// ?since_seq, at most ?limit of them.
func (s *Server) handleRunsSignals(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	run, ok := s.ownedRun(w, r)
	if !ok {
		return
	}
	sinceSeq, limit, ok := s.seqPaging(w, r)
	if !ok {
		return
	}
	page, err := s.Runs.Signals(run.ID, sinceSeq, limit)
	if err != nil {
		s.writeSignalError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, page)
}

// handleRunsSignalEmit is POST /v1/runs/{id}/signals: the run's owner (or
// the operator) records a caller signal. started and done are refused —
// the runner alone says when a turn began and ended.
func (s *Server) handleRunsSignalEmit(w http.ResponseWriter, r *http.Request) {
	if !s.runsReady(w) {
		return
	}
	run, ok := s.ownedRun(w, r)
	if !ok {
		return
	}
	var in api.SignalRequest
	if !s.readJSON(w, r, &in) {
		return
	}
	p, ok := CallerFrom(r.Context())
	if !ok {
		p = s.operator()
	}
	rec, err := s.Runs.Emit(run.ID, signals.Signal{Kind: in.Kind, Source: api.SignalSourceCaller, By: p.String(), Message: in.Message, Data: in.Data})
	if err != nil {
		s.writeSignalError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, rec)
}

// writeSignalError maps the signal log's typed errors onto wire codes and
// falls back to the run service's mapping for the rest.
func (s *Server) writeSignalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, signals.ErrKindNotAllowed):
		s.writeError(w, http.StatusForbidden, api.CodeForbidden, err.Error())
	case errors.Is(err, signals.ErrDataTooLarge):
		s.writeError(w, http.StatusRequestEntityTooLarge, api.CodeBadRequest, err.Error())
	case errors.Is(err, signals.ErrBadRequest):
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, err.Error())
	case errors.Is(err, signals.ErrCorrupt):
		s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, err.Error())
	default:
		s.writeRunsError(w, err)
	}
}

// publishSignal relays one durable signal onto /v1/events.
func (s *Server) publishSignal(runID string, sig api.Signal) {
	s.publishData(api.EventRunSignal, "", api.RunSignalEvent{Run: runID, Signal: sig})
}

// handleAgentKickOutcome is GET /v1/agents/{name}/kick-outcome: the
// agent's last KickOutcome and whether it settles the last kick. The live
// runtime wins when it has classified a turn; otherwise the record
// KickOutcomes saved before the restart answers, marked saved.
func (s *Server) handleAgentKickOutcome(w http.ResponseWriter, r *http.Request) {
	name := s.Runtime.ResolveAgent(r.PathValue("name"))
	p, err := s.Runtime.GetStatus(name)
	if err != nil || p == nil {
		s.writeError(w, http.StatusNotFound, api.CodeNotFound, fmt.Sprintf("agent %q not found", name))
		return
	}
	out := api.AgentKickOutcome{Agent: name, LastKick: p.LastKick}
	if p.KickOutcome.Kind != "" {
		o := p.KickOutcome
		out.Outcome = &api.KickOutcome{Kind: o.Kind, Reason: o.Reason, At: o.At, KickAt: o.KickAt}
	} else if s.KickOutcomes != nil {
		saved, ok, err := s.KickOutcomes.Get(name)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, err.Error())
			return
		}
		if ok {
			out.Outcome, out.Saved = &saved, true
		}
	}
	// Settled semantics, not timestamps: the outcome belongs to the last
	// kick only when its kick_at is that kick's delivery time.
	out.SettledForLastKick = out.Outcome != nil && p.LastKick != nil && out.Outcome.KickAt.Equal(*p.LastKick)
	s.writeJSON(w, http.StatusOK, out)
}

// saveKickOutcome is the host-observer tee that persists each settled
// outcome (installHostObservers); a store that is unset or fails never
// blocks the event.
func (s *Server) saveKickOutcome(agent string, o api.KickOutcome) {
	if s.KickOutcomes == nil {
		return
	}
	if err := s.KickOutcomes.Put(agent, o); err != nil {
		s.Logger.Warn("crux serve: saving kick outcome", "agent", agent, "err", err)
	}
}
