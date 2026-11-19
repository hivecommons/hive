package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/claims"
)

// POST /api/workflows/dispatch: the owner's "Send to <profile>" on a
// stage-tracker row of a model-bound repository (cncf/techdocs#395).
//
// The automatic pass (cmd/hive/workflow_dispatch.go) hands an item to its
// model profile once per state. This is the same hand-off on demand: the
// restart after a discarded draft, a verifier pass without a `/verify`, or a
// profile turned off for automatic dispatch. cmd/hive prepares it through
// Dependencies.WorkflowDispatchPrepare (fresh item read, model state, agent,
// the agent claim, the prompt); the handler kicks the agent and, once the
// kick lands, records it in the dispatch log so the automatic pass does not
// repeat it.

// WorkflowDispatchPlan is one prepared hand-off.
type WorkflowDispatchPlan struct {
	Ref     string
	Model   string
	State   string
	Profile string
	Agent   string
	Message string
	// Record writes the delivered hand-off to the dispatch log.
	Record func(at time.Time)
}

// Errors WorkflowDispatchPrepare reports; the handler maps them to status
// codes.
var (
	// ErrWorkflowDispatchNotOwned: the repository is not bound to a model
	// that hands the item's current state to a profile.
	ErrWorkflowDispatchNotOwned = errors.New("the bound workflow model does not hand this item's current state to an agent profile")
	// ErrWorkflowDispatchNoAgent: no enabled hive agent fills the profile.
	// cmd/hive wraps it with the reason and the fix
	// (WorkflowDispatchNoAgentError).
	ErrWorkflowDispatchNoAgent = errors.New("not sent")
	// ErrWorkflowDispatchHeld: someone else holds the issue's claim.
	ErrWorkflowDispatchHeld = errors.New("the issue is claimed by someone else")
)

func (s *Server) handleWorkflowDispatch(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.WorkflowDispatchPrepare == nil || s.deps.AgentMgr == nil {
		jsonError(w, "workflow dispatch is not available on this hive", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Repo   string `json:"repo"`
		Number int    `json:"number"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	body.Repo = strings.TrimSpace(body.Repo)
	if body.Repo == "" || body.Number <= 0 {
		jsonError(w, "body must be {\"repo\": \"owner/repo\", \"number\": N}", http.StatusBadRequest)
		return
	}
	by := requestUser(r)
	plan, err := s.deps.WorkflowDispatchPrepare(r.Context(), body.Repo, body.Number, by, s.IssueClaims())
	if err != nil {
		jsonError(w, err.Error(), workflowDispatchStatus(err))
		return
	}
	s.kickIntend(plan.Agent, kickTriggerWorkflowDispatch, by)
	if err := s.deps.AgentMgr.SendKick(plan.Agent, plan.Message); err != nil {
		s.kickRefusedOnError(plan.Agent, kickTriggerWorkflowDispatch, by, err)
		status := http.StatusBadGateway
		if errors.Is(err, crux.ErrAgentBusy) {
			status = http.StatusConflict
		}
		jsonError(w, fmt.Sprintf("kick %s: %v", plan.Agent, err), status)
		return
	}
	if s.deps.Governor != nil {
		s.deps.Governor.RecordKick(plan.Agent)
	}
	if plan.Record != nil {
		plan.Record(time.Now())
	}
	s.auditFromRequest(r, "workflow_dispatch", fmt.Sprintf("%s profile=%s state=%s model=%s", plan.Ref, plan.Profile, plan.State, plan.Model), plan.Agent)
	jsonResponse(w, map[string]interface{}{
		"status":  "sent",
		"ref":     plan.Ref,
		"model":   plan.Model,
		"state":   plan.State,
		"profile": plan.Profile,
		"agent":   plan.Agent,
		// result is the tracker's toast: the hand-off is done when the kick
		// lands, so the row does not wait for a state change (hours away).
		"result": map[string]interface{}{
			"action":  workflowDispatchResultAction,
			"outcome": fmt.Sprintf("Sent to %s (%s).", plan.Agent, plan.Profile),
			"next":    workflowDispatchNext(plan),
		},
	})
}

// workflowDispatchResultAction is the tracker result action of a sent
// hand-off.
const workflowDispatchResultAction = "dispatched"

func workflowDispatchNext(plan WorkflowDispatchPlan) string {
	if plan.Profile == "" {
		return ""
	}
	return "The " + plan.Profile + " agent works the item now; the row moves when its deliverable lands."
}

func workflowDispatchStatus(err error) int {
	switch {
	case errors.Is(err, ErrWorkflowDispatchNotOwned):
		return http.StatusNotFound
	case errors.Is(err, ErrWorkflowDispatchNoAgent), errors.Is(err, ErrWorkflowDispatchHeld):
		return http.StatusConflict
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

// workflowDispatchPrepareFunc is Dependencies.WorkflowDispatchPrepare.
type workflowDispatchPrepareFunc = func(ctx context.Context, repo string, number int, by string, ledger *claims.Ledger) (WorkflowDispatchPlan, error)
