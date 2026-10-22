package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Activity-monitor row decisions (hivecommons/hive#13479 follow-up). The
// stage tracker lists items parked on a person — stage/045-needs-decision on
// an issue, stage/085 on a PR — and the row's "Decide…" menu lets the
// operator answer from the dashboard. Each verb maps onto a path the hive
// already honours for a maintainer on GitHub; none of them merges anything:
//
//   - approve     → the same effect as `/hive approve` from a maintainer with
//     write access (Client.RecordOperatorIssueDecision): park
//     labels cleared, approved-direction applied, reply posted.
//   - direction   → `/hive decision <comment>`; the comment is required.
//   - close-superseded / close-wontfix → one comment naming the operator,
//     then a not_planned close (a PR takes the supersession
//     sweep's own labelled close path).
//
// Authorisation is the repo-hold bar (canToggleRepoHold): a verified owner,
// or a signed-in GitHub user with write on the repository — which is exactly
// the permission the un-park sweep checks on a commenter.
const (
	repoItemDecisionApprove         = "approve"
	repoItemDecisionDirection       = "direction"
	repoItemDecisionCloseSuperseded = "close-superseded"
	repoItemDecisionCloseWontFix    = "close-wontfix"

	repoItemDecisionMaxBody    = 16 << 10
	repoItemDecisionMaxComment = 4000

	auditRepoItemDecisionApprove   = "repo_item_decision_approve"
	auditRepoItemDecisionDirection = "repo_item_decision_direction"
	auditRepoItemDecisionClose     = "repo_item_decision_close"

	issueStateReasonNotPlanned = "not_planned"
)

type repoItemDecisionRequest struct {
	Decision string `json:"decision"`
	Comment  string `json:"comment"`
	// Type is "issue" or "pr"; it picks the close path. Unknown reads as issue.
	Type string `json:"type"`
}

type repoItemDecisionResponse struct {
	OK       bool   `json:"ok"`
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	Decision string `json:"decision"`
	Status   string `json:"status"`
	Label    string `json:"label,omitempty"`
}

func renderRepoItemDecisionCloseComment(verb, user, comment string) string {
	by := "an operator"
	if strings.TrimSpace(user) != "" {
		by = "@" + strings.TrimSpace(user)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Closed as %s by %s from the Hive dashboard.", verb, by)
	if c := strings.TrimSpace(comment); c != "" {
		fmt.Fprintf(&b, "\n\n%s", c)
	}
	return b.String()
}

func (s *Server) handleRepoItemDecision(w http.ResponseWriter, r *http.Request) {
	owner, repoName, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	allowed, status, msg := s.canToggleRepoHold(r, owner, repoName)
	if !allowed {
		jsonError(w, msg, status)
		return
	}
	user := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if user == "" {
		jsonError(w, "GitHub user session required to record a decision", http.StatusUnauthorized)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	var body repoItemDecisionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, repoItemDecisionMaxBody)).Decode(&body); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	decision := strings.ToLower(strings.TrimSpace(body.Decision))
	comment := strings.TrimSpace(body.Comment)
	if len(comment) > repoItemDecisionMaxComment {
		jsonError(w, fmt.Sprintf("comment exceeds %d characters", repoItemDecisionMaxComment), http.StatusBadRequest)
		return
	}
	itemType := strings.ToLower(strings.TrimSpace(body.Type))
	if itemType != "pr" {
		itemType = "issue"
	}
	repoFull := owner + "/" + repoName
	ctx := context.Background()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	gh := s.deps.GHClient
	numStr := strconv.Itoa(number)

	switch decision {
	case repoItemDecisionApprove, repoItemDecisionDirection:
		if itemType == "pr" {
			jsonError(w, "a pull request decision is answered by review, not from the activity monitor", http.StatusBadRequest)
			return
		}
		if decision == repoItemDecisionDirection && comment == "" {
			jsonError(w, "a decision needs the direction text", http.StatusBadRequest)
			return
		}
		text := ""
		if decision == repoItemDecisionDirection {
			text = comment
		}
		event, err := gh.RecordOperatorIssueDecision(ctx, repoFull, number, user, text)
		switch {
		case errors.Is(err, ghpkg.ErrIssueNotParked):
			jsonError(w, "issue is not waiting on a decision", http.StatusConflict)
			return
		case errors.Is(err, ghpkg.ErrDecisionTargetIsPR):
			jsonError(w, "a pull request decision is answered by review, not from the activity monitor", http.StatusBadRequest)
			return
		case err != nil:
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		action := auditRepoItemDecisionApprove
		if decision == repoItemDecisionDirection {
			action = auditRepoItemDecisionDirection
		}
		s.audit.LogRecord(user, action, auditDetail("repo", repoFull, "number", numStr, "decision", event.Decision), "", repoFull, number)
		jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: decision, Status: "unparked", Label: ghpkg.HumanAckLabel})
	case repoItemDecisionCloseSuperseded, repoItemDecisionCloseWontFix:
		verb := "won't fix"
		if decision == repoItemDecisionCloseSuperseded {
			verb = "superseded"
		}
		if err := gh.CreateIssueComment(ctx, repoFull, number, renderRepoItemDecisionCloseComment(verb, user, comment)); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		label := ""
		if itemType == "pr" && decision == repoItemDecisionCloseSuperseded {
			if err := gh.CloseSupersededPR(ctx, repoFull, number); err != nil {
				jsonError(w, err.Error(), http.StatusBadGateway)
				return
			}
			label = ghpkg.SupersededLabel
		} else {
			err := gh.CloseIssue(ctx, repoFull, number, ghpkg.IssueCloseOptions{
				OverrideReason:          "closed as " + verb + " by @" + user + " from the Hive dashboard",
				SuppressOverrideComment: true,
				StateReason:             issueStateReasonNotPlanned,
			})
			if err != nil {
				jsonError(w, err.Error(), http.StatusBadGateway)
				return
			}
		}
		s.audit.LogRecord(user, auditRepoItemDecisionClose, auditDetail("repo", repoFull, "number", numStr, "type", itemType, "reason", verb), "", repoFull, number)
		jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: decision, Status: "closed", Label: label})
	default:
		jsonError(w, "unknown decision; want approve, direction, close-superseded or close-wontfix", http.StatusBadRequest)
	}
}
