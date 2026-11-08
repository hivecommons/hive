package serve

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/signals"
)

// Turn signals (hivecommons/crux#114 S5). The per-run half lives with the
// runs family (runs.go); this is the RFC's S5 proper: the last kick outcome
// of each agent, saved before it is published so a driver that missed the
// agent.kick_outcome event learns from the record.

// registerSignals mounts GET /v1/agents/{name}/kick-outcome. Like every
// /v1/agents route it is the operator's; project principals are refused.
func (s *Server) registerSignals(mux *http.ServeMux) {
	mux.HandleFunc("GET "+api.PathAgents+"/{name}/kick-outcome", s.handleKickOutcome)
}

// recordKickOutcome is the runtime's kick-outcome observer: save the
// outcome (when the server keeps them), then publish it. The record is
// written first because it is the proof; the event is only a notification.
func (s *Server) recordKickOutcome(agent string, o crux.KickOutcome) {
	wire := api.KickOutcome{Kind: o.Kind, Reason: o.Reason, At: o.At, KickAt: o.KickAt}
	if s.KickOutcomes != nil {
		if err := s.KickOutcomes.Save(agent, wire); err != nil {
			s.Logger.Warn("crux serve: saving kick outcome", "agent", agent, "err", err)
		}
	}
	s.publishData(api.EventKickOutcome, agent, wire)
}

// handleKickOutcome answers the saved outcome of the agent's last kicked
// turn and whether it belongs to the runtime's latest kick, by
// crux.KickOutcome.Settled — never by comparing timestamps loosely. An
// agent the runtime does not hold is still answered from its record; one
// with neither is 404.
func (s *Server) handleKickOutcome(w http.ResponseWriter, r *http.Request) {
	if s.KickOutcomes == nil {
		s.writeError(w, http.StatusNotImplemented, api.CodeNotSupported, "this server keeps no kick outcomes")
		return
	}
	name := s.Runtime.ResolveAgent(r.PathValue("name"))
	out := api.AgentKickOutcome{Agent: name}
	p, err := s.Runtime.GetStatusFast(name)
	known := err == nil && p != nil
	if known {
		out.LastKick = p.LastKick
	}
	saved, err := s.KickOutcomes.Load(name)
	switch {
	case err == nil:
		o := saved.Outcome
		out.Outcome = &o
		out.SettledForLastKick = crux.KickOutcome{Kind: o.Kind, Reason: o.Reason, At: o.At, KickAt: o.KickAt}.Settled(out.LastKick)
	case errors.Is(err, signals.ErrNotFound), errors.Is(err, signals.ErrBadRequest):
		// No record (or a name no agent can have): only an agent the
		// runtime holds is answered, with no outcome yet.
		if !known {
			s.writeError(w, http.StatusNotFound, api.CodeNotFound, fmt.Sprintf("agent %q not found", name))
			return
		}
	default:
		s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}
