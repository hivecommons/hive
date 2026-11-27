package dashboard

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Open change requests on a dashboard decision (hivecommons/hive#20067).
//
// Every hive path that approves, labels or merges a PR refuses while a
// writer's change request is open. The operator's explicit Approve, Merge
// now and Queue merge decisions are the one place that may proceed anyway,
// and only like this: the first click is answered 409 with the open change
// requests named (reviewer and review ID), and the decision goes through
// only when it is sent again with override_reason and override_review_ids
// naming every change request still open (a newer one answers 409 again). The override covers
// exactly the change requests the operator was shown, is audited with the
// reason, and every gated path that honours it audits it again. "Let hive
// decide" is the hive's decision, not the operator's, so it never overrides.

const (
	// auditRepoItemDecisionChangesRequestedOverride records an operator's
	// override of open change requests, with the reason.
	auditRepoItemDecisionChangesRequestedOverride = "repo_item_decision_changes_requested_override"
	// auditRepoItemDecisionChangesRequestedBlocked records a decision
	// refused because it named no override reason.
	auditRepoItemDecisionChangesRequestedBlocked = "repo_item_decision_changes_requested_blocked"

	repoItemStatusChangesRequested = "changes-requested"
	repoItemMaxOverrideReason      = 1000
)

// repoItemChangeRequest is one open change request in the 409 answer.
type repoItemChangeRequest struct {
	Reviewer string `json:"reviewer"`
	ReviewID int64  `json:"review_id"`
	CommitID string `json:"commit_id,omitempty"`
	URL      string `json:"url,omitempty"`
}

// repoItemChangesRequestedResponse is the 409 a decision gets while a
// writer's change request is open and no override reason was sent.
type repoItemChangesRequestedResponse struct {
	OK             bool                    `json:"ok"`
	Status         string                  `json:"status"`
	Error          string                  `json:"error"`
	ChangeRequests []repoItemChangeRequest `json:"change_requests"`
	// NeedsOverrideReason tells the client to ask the operator for a reason
	// and resend the same decision with override_reason.
	NeedsOverrideReason bool `json:"needs_override_reason"`
}

// changeRequestsShown reports whether every open change request is one the
// operator was shown (its review ID is in shown).
func changeRequestsShown(blocks []ghpkg.ChangeRequestBlock, shown []int64) bool {
	seen := make(map[int64]bool, len(shown))
	for _, id := range shown {
		seen[id] = true
	}
	for _, b := range blocks {
		if !seen[b.ReviewID] {
			return false
		}
	}
	return true
}

// repoItemDecisionGatesChangeRequests reports whether decision on a PR ends
// in an approval or a merge, and so must clear the change-request gate.
func repoItemDecisionGatesChangeRequests(decision string) bool {
	switch decision {
	case repoItemDecisionApprove, repoItemDecisionMergeNow, repoItemDecisionQueueMerge:
		return true
	}
	return false
}

// clearChangeRequestsForDecision runs before an operator's approve or merge
// decision on a PR. It returns true when the decision may proceed: no open
// change request, or the operator sent an override reason, which is recorded
// and audited. Otherwise it has answered the request (409 naming the open
// change requests, or 502 when they could not be read) and returns false.
func (s *Server) clearChangeRequestsForDecision(ctx context.Context, w http.ResponseWriter, r *http.Request, repoFull string, number int, user, decision, reason string, shown []int64) bool {
	gh := s.deps.GHClient
	blocks, err := gh.OpenChangeRequests(ctx, repoFull, number)
	if err != nil {
		jsonError(w, "reading the pull request's reviews: "+err.Error(), http.StatusBadGateway)
		return false
	}
	if len(blocks) == 0 {
		return true
	}
	numStr := strconv.Itoa(number)
	reason = strings.TrimSpace(reason)
	if reason == "" || !changeRequestsShown(blocks, shown) {
		s.audit.LogRecord(user, auditRepoItemDecisionChangesRequestedBlocked, auditDetail("repo", repoFull, "number", numStr, "decision", decision,
			"reviewers", ghpkg.ChangeRequestReviewers(blocks), "review_ids", ghpkg.ChangeRequestReviewIDs(blocks)), "", repoFull, number)
		out := repoItemChangesRequestedResponse{
			Status: repoItemStatusChangesRequested,
			Error: "This pull request has an open change request from " + ghpkg.DescribeChangeRequests(blocks) +
				". The hive will not approve or merge it until that reviewer approves or the review is dismissed. To proceed anyway, give a reason; it is recorded in the audit log.",
			NeedsOverrideReason: true,
		}
		for _, b := range blocks {
			out.ChangeRequests = append(out.ChangeRequests, repoItemChangeRequest{Reviewer: b.Reviewer, ReviewID: b.ReviewID, CommitID: b.CommitID, URL: b.URL})
		}
		jsonStatusResponse(w, http.StatusConflict, out)
		return false
	}
	// An override lets the merge sweep merge over the change request, so it
	// takes the merge bar (a verified owner or a merger) whatever decision
	// carries it, Approve included.
	if !repoItemMergeDecisionAllowed(r) {
		jsonError(w, "merger or owner access required to override an open change request", http.StatusForbidden)
		return false
	}
	if len(reason) > repoItemMaxOverrideReason {
		jsonError(w, "override_reason exceeds "+strconv.Itoa(repoItemMaxOverrideReason)+" characters", http.StatusBadRequest)
		return false
	}
	if err := ghpkg.RecordChangesRequestedOverride(repoFull, number, user, reason, blocks); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return false
	}
	s.audit.LogRecord(user, auditRepoItemDecisionChangesRequestedOverride, auditDetail("repo", repoFull, "number", numStr, "decision", decision,
		"reviewers", ghpkg.ChangeRequestReviewers(blocks), "review_ids", ghpkg.ChangeRequestReviewIDs(blocks), "reason", reason), "", repoFull, number)
	if s.logger != nil {
		s.logger.Warn("operator overrode open change request", "repo", repoFull, "pr", number, "actor", user, "decision", decision,
			"reviewers", ghpkg.ChangeRequestReviewers(blocks), "review_ids", ghpkg.ChangeRequestReviewIDs(blocks), "reason", reason)
	}
	return true
}
