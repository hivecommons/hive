package dashboard

import (
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Per-kick outcome record (hivecommons/hive#18916).
//
// GET /api/kick-outcomes answers "what did that kick come to?" from the
// durable record pkg/kickoutcome keeps. Without `agent` it lists every agent's
// latest kick as a one-line summary; with `agent` it lists that agent's last
// kicks. Any authenticated role sees the one-line summary (kind, summary, at);
// owner and read-write roles also see the detail: trigger, times, the pull
// request, branch and sha, the refusal reason code and the quoted text behind a
// stand-down or refusal. The quote is untrusted agent text: it is redacted and
// capped when stored, and the page escapes it. It is never fed back to an
// agent, and agents cannot reach this route.

// kickOutcomeRecentLimit is how many kicks the per-agent list returns.
const kickOutcomeRecentLimit = 10

// Trigger kinds the dashboard declares; they match pkg/kickoutcome's.
const (
	kickTriggerDashboard        = "dashboard"
	kickTriggerWorkflowDispatch = "workflow_dispatch"
)

// KickOutcomeSource is the durable per-kick record (pkg/kickoutcome behind an
// adapter in cmd/hive), kept behind an interface so the dashboard takes no new
// package import. Views and details are JSON-shaped values the handler passes
// through; the handler never builds or trusts quoted text itself.
type KickOutcomeSource interface {
	// Intend says who or what is about to kick agent.
	Intend(agent, kind, actor string)
	// Refused files a kick the hive refused, with a documented reason code.
	Refused(agent, kind, actor, code, reason string, at time.Time)
	// Latest is every agent's latest kick as one-line summaries.
	Latest() any
	// Recent is the agent's last n kicks as detail (kicks) and its latest
	// kick's one-line summary (latest, nil when there is none).
	Recent(agent string, n int) (latest any, kicks any)
}

func (s *Server) registerKickOutcomeRoutes() {
	s.mux.HandleFunc("GET /api/kick-outcomes", s.handleKickOutcomes)
}

// canSeeKickOutcomeDetail admits owner (verified) and read-write sessions to
// the detail of a kick; a read-only session sees the one-line summary only.
func canSeeKickOutcomeDetail(r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if isOwnerRole(role) && r.Header.Get(ownerRoleVerifiedHeader) != "true" {
		return false
	}
	return config.RoleAtLeast(role, config.RoleReadWrite)
}

func (s *Server) handleKickOutcomes(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.KickOutcomes == nil {
		jsonError(w, "kick outcomes are not available on this hive", http.StatusServiceUnavailable)
		return
	}
	detail := canSeeKickOutcomeDetail(r)
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" {
		jsonResponse(w, map[string]interface{}{"detail": detail, "agents": s.deps.KickOutcomes.Latest()})
		return
	}
	agent = s.resolveAgentParam(agent)
	latest, kicks := s.deps.KickOutcomes.Recent(agent, kickOutcomeRecentLimit)
	if detail {
		jsonResponse(w, map[string]interface{}{"detail": true, "agent": agent, "kicks": kicks})
		return
	}
	// Read-only: the latest kick's one-line summary and nothing else.
	resp := map[string]interface{}{"detail": false, "agent": agent}
	if latest != nil {
		resp["latest"] = latest
	}
	jsonResponse(w, resp)
}

// kickIntend tells the record who or what is about to kick agent.
func (s *Server) kickIntend(agent, kind, actor string) {
	if s == nil || s.deps == nil || s.deps.KickOutcomes == nil {
		return
	}
	s.deps.KickOutcomes.Intend(agent, kind, actor)
}

// kickRefusedOnError records a kick the hive refused to deliver, when err
// names a documented refusal reason (kick-refusals.md). An error that matches
// no reason is not recorded: the record never invents a code.
func (s *Server) kickRefusedOnError(agent, kind, actor string, err error) {
	if s == nil || s.deps == nil || s.deps.KickOutcomes == nil || err == nil {
		return
	}
	code := classifyKickError(err.Error())
	if code == "" {
		return
	}
	s.deps.KickOutcomes.Refused(agent, kind, actor, code, err.Error(), time.Now())
}

// ClassifyKickRefusal maps a kick error message to its kick-refusals.md
// reason code, or "" when it matches none. Exported for the governor's kick
// path in cmd/hive.
func ClassifyKickRefusal(msg string) string { return classifyKickError(msg) }
