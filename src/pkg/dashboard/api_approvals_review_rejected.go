package dashboard

import (
	"context"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/hooks"
)

// emitApprovalDenied fires review_rejected for ONE denied approval. item is
// the pending row captured BEFORE Resolve removed it from the queue; operator
// and rationale are what the journal recorded.
//
// Nil-safe on deps and HookFire: a hive with no hooks configured, or a test
// server without the seam wired, records the denial and emits nothing.
func (s *Server) emitApprovalDenied(r *http.Request, item toolapprove.PendingItem, operator, rationale string) {
	if s == nil || s.deps == nil || s.deps.HookFire == nil {
		return
	}
	agentName := strings.TrimSpace(item.Request.Agent.Name)
	backend, model, pin := s.producingModel(agentName)

	hooks.EmitReviewRejected(context.Background(), s.deps.HookFire, hooks.ReviewRejection{
		Agent:            agentName,
		Repo:             item.Request.Repo,
		PRNumber:         item.Request.Number,
		Actor:            operator,
		Reason:           rationale,
		Backend:          backend,
		Model:            model,
		Pin:              pin,
		ACMMLevel:        s.currentACMMLevel(),
		DashboardBaseURL: s.oauthPublicOrigin(r),
	})
}

// producingModel resolves the backend/model/pin that PRODUCED the named
// agent's output, in the same precedence the launcher applies: a runtime
// override wins, then the operator's model pin, then static config. The
// agent manager is consulted first because it holds the runtime overrides
// and the pin; the static config is the fallback when the manager does not
// know the agent (or is not wired), so the notification still names what it
// can rather than going silent.
//
// pin is the operator's pinned model when one is set, which is exactly the
// knob the notification deep-links (a stale pin is the RFC's motivating
// failure).
func (s *Server) producingModel(agentName string) (backend, model, pin string) {
	if agentName == "" || s.deps == nil {
		return "", "", ""
	}
	if s.deps.Config != nil {
		if ac, ok := s.deps.Config.Agents[agentName]; ok {
			backend, model = ac.Backend, ac.Model
		}
	}
	if s.deps.AgentMgr == nil {
		return backend, model, pin
	}
	proc, err := s.deps.AgentMgr.GetStatus(agentName)
	if err != nil || proc == nil {
		return backend, model, pin
	}
	if proc.Config.Backend != "" {
		backend = proc.Config.Backend
	}
	if proc.BackendOverride != "" {
		backend = proc.BackendOverride
	}
	if proc.Config.Model != "" {
		model = proc.Config.Model
	}
	pin = proc.PinnedModel
	switch {
	case proc.ModelOverride != "":
		model = proc.ModelOverride
	case pin != "":
		model = pin
	}
	return backend, model, pin
}
