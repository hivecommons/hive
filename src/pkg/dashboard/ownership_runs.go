package dashboard

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/worksource"
)

// Operator waits outside the GitHub snapshot (hivecommons/hive#12380): a run
// the Runs tab shows as waiting_on: human, and a request the tool-approval
// desk parked for the operator. Neither carries a label the ownership rules
// read, and a run's issue carries the lease or claim that switches off the
// safety net, so without this input the item never became your turn.

const (
	OwnershipReasonRunCheckpoint     = "run_checkpoint"
	OwnershipReasonRunStalePlan      = "run_stale_plan"
	OwnershipReasonRunStageExhausted = "run_stage_exhausted"
	OwnershipReasonApprovalPending   = "approval_pending"
)

// operatorWaitReasonVerbs are the reason codes this input produces, merged
// into ownershipReasonVerbs.
var operatorWaitReasonVerbs = map[string]string{
	OwnershipReasonRunCheckpoint:     OwnershipVerbReview,
	OwnershipReasonRunStalePlan:      OwnershipVerbReview,
	OwnershipReasonRunStageExhausted: OwnershipVerbReview,
	OwnershipReasonApprovalPending:   OwnershipVerbReview,
}

func init() {
	for code, verb := range operatorWaitReasonVerbs {
		ownershipReasonVerbs[code] = verb
	}
}

// Dashboard sections the rows link to for the action itself.
const (
	ownershipLinkRuns      = "#section-runs-section"
	ownershipLinkApprovals = "#section-approvals-section"
)

// operatorWaits is the your-turn decision of every item a run or the approval
// desk is waiting on the operator for, keyed by item kind and repo#number.
type operatorWaits struct {
	org   string
	items map[string]Ownership
}

func newOperatorWaits(org string) *operatorWaits {
	return &operatorWaits{org: org, items: map[string]Ownership{}}
}

func (w *operatorWaits) key(kind, repo string, number int) string {
	return kind + "|" + leaseItemKey(w.org, repo, number)
}

// add records a wait; the first one recorded for an item wins.
func (w *operatorWaits) add(kind, repo string, number int, o Ownership) {
	if w == nil || number <= 0 || strings.TrimSpace(repo) == "" {
		return
	}
	key := w.key(kind, repo, number)
	if _, ok := w.items[key]; !ok {
		w.items[key] = o
	}
}

// apply returns the decision for an item given the decision the label rules
// reached. A run or approval wait outranks a not-your-turn decision — a
// claim, a lease, a hold, an open PR — and the stale safety net, because no
// lane will move the item until the operator acts; every other your-turn
// decision is kept.
func (w *operatorWaits) apply(kind, repo string, number int, decided Ownership) Ownership {
	if w == nil || number <= 0 {
		return decided
	}
	wait, ok := w.items[w.key(kind, repo, number)]
	if !ok {
		return decided
	}
	if decided.YourTurn && decided.ReasonCode != OwnershipReasonStaleSafetyNet {
		return decided
	}
	return wait
}

// runWaitOwnership is the your-turn decision for a run waiting on a person,
// with the next action the Runs tab offers for the wait.
func runWaitOwnership(run Run) Ownership {
	stage := strings.TrimSpace(run.Stage)
	var o Ownership
	switch run.WaitingReason {
	case runWaitingReasonStageBudgetExhausted:
		o = yourTurn(OwnershipReasonRunStageExhausted, fmt.Sprintf("Run stage %s used up its retries — reset the stage or abandon the run", stage))
	case planning.WaitingReasonStalePlan:
		o = yourTurn(OwnershipReasonRunStalePlan, "Run plan is stale — re-plan, approve or abandon the run")
	case worksource.RunWaitingReasonInterviewQuestions:
		o = yourTurn(OwnershipReasonRunCheckpoint, "Run is waiting for answers to its interview questions")
	default:
		o = yourTurn(OwnershipReasonRunCheckpoint, fmt.Sprintf("Run is at the %s checkpoint — approve or reject it", stage))
	}
	o.Link = ownershipLinkRuns
	return o
}

// addRuns records every active run waiting on a person against the issue it
// works on. A run on a work item with no issue number has no row to mark.
func (w *operatorWaits) addRuns(runs []Run) {
	for _, run := range runs {
		if run.WaitingOn != RunWaitingOnHuman || (run.State != "" && run.State != "active") {
			continue
		}
		ref, ok := worksource.ParseKey(run.Key)
		if !ok || !ref.IsGitHubIssue() {
			continue
		}
		w.add(ownershipKindIssue, ref.Repo, ref.Number, runWaitOwnership(run))
	}
}

// approvalWaitKind is the row kind a pending approval marks: merges are pull
// requests, plan approvals are issues. Agent tool calls have no row.
func approvalWaitKind(kind string) string {
	switch kind {
	case toolapprove.KindSelfMerge, toolapprove.KindQueuedMerge:
		return ownershipKindPR
	case toolapprove.KindPlanApproval, toolapprove.KindPlanFromLabel:
		return ownershipKindIssue
	}
	return ""
}

// addApprovals records every pending approval-desk item. Resolved items leave
// the inbox's pending list, so they clear on the next refresh.
func (w *operatorWaits) addApprovals(pending []toolapprove.PendingItem) {
	for _, p := range pending {
		kind := approvalWaitKind(p.Request.Kind)
		if kind == "" {
			continue
		}
		reason := "Approval desk is waiting for your approval"
		if p.Request.Kind == toolapprove.KindSelfMerge {
			reason = "Self-merge is waiting for your approval"
		}
		if rule := strings.TrimSpace(p.Verdict.Rule); rule != "" {
			reason += " (rule " + rule + ")"
		}
		o := yourTurn(OwnershipReasonApprovalPending, reason+" — approve or reject it in Approvals")
		o.Link = ownershipLinkApprovals
		w.add(kind, p.Request.Repo, p.Request.Number, o)
	}
}

// collectOperatorWaits reads the waits from the server behind the contribute
// hub: the active runs the Runs tab lists and the approval desk's pending
// items. It returns nil when there is no server.
func collectOperatorWaits(hub *ContributeWSHub, org string) *operatorWaits {
	if hub == nil || hub.server == nil {
		return nil
	}
	s := hub.server
	w := newOperatorWaits(org)
	if s.contributeHub != nil {
		if runs, err := s.activeRuns(false); err == nil {
			w.addRuns(runs)
		} else {
			slog.Debug("ownership: runs unavailable", "error", err)
		}
	}
	if inbox := s.approvalInbox(); inbox != nil {
		w.addApprovals(inbox.List())
	}
	return w
}
