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
	"github.com/hivecommons/hive/pkg/review"
)

// Activity-monitor row decisions (hivecommons/hive#13479 follow-up). The
// stage tracker lists items parked on a person — stage/045-needs-decision on
// an issue, stage/085 on a PR — and the row's "Decide…" menu lets the
// operator answer from the dashboard. Each verb maps onto a path the hive
// already honours for a maintainer on GitHub; none of them merges anything:
//
//   - approve     → on an issue, the same effect as `/hive approve` from a
//     maintainer with write access (Client.RecordOperatorIssueDecision):
//     park labels cleared, approved-direction applied, reply posted. On a
//     PR, the `/lgtm` and `/approve` commands posted as two attributed
//     comments (Client.RecordOperatorPullRequestApproval) — the merge gate
//     decides from there; nothing is reviewed, labelled or merged here.
//   - direction   → `/hive decision <comment>`; the comment is required. On
//     a hive-authored PR it is recorded as a human change request so the
//     author agent is kicked with it (Client.RecordOperatorPullRequestDecision).
//   - close-superseded / close-wontfix → one comment naming the operator
//     (and, when given, the superseding PR), then a not_planned close (a PR
//     takes the supersession sweep's own labelled close path).
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

// repoItemDecisionChangeRequests is the store a PR direction is recorded in:
// the process-wide one the un-park sweep and review planner share. A test
// swaps it for an in-memory store.
var repoItemDecisionChangeRequests = func() review.HumanChangeRequestStore { return review.DefaultHumanChangeRequests() }

type repoItemDecisionRequest struct {
	Decision string `json:"decision"`
	Comment  string `json:"comment"`
	// Type is "issue" or "pr"; it picks the close path. Unknown reads as issue.
	Type string `json:"type"`
	// SupersededBy names the pull request that replaces a close-superseded
	// item: "458", "#458", "owner/repo#458" or a GitHub URL. Optional.
	SupersededBy string `json:"superseded_by"`
}

// parseRepoItemSupersededBy resolves a "superseded by" reference the way an
// operator types it, defaulting a bare number to the item's own repo.
func parseRepoItemSupersededBy(raw, defaultRepo string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			rest = rest[j+1:]
		}
		rest = strings.TrimSuffix(rest, "/")
		parts := strings.Split(rest, "/")
		if len(parts) == 4 && (parts[2] == "pull" || parts[2] == "issues") {
			raw = parts[0] + "/" + parts[1] + "#" + parts[3]
		} else {
			return "", fmt.Errorf("superseded_by: unrecognised pull request URL")
		}
	}
	repo := defaultRepo
	numStr := strings.TrimPrefix(raw, "#")
	if i := strings.Index(raw, "#"); i > 0 {
		repo = raw[:i]
		numStr = raw[i+1:]
	}
	n, err := strconv.Atoi(strings.TrimSpace(numStr))
	if err != nil || n <= 0 {
		return "", fmt.Errorf("superseded_by: want a pull request number, owner/repo#number or URL")
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("superseded_by: repository must be owner/repo")
	}
	return fmt.Sprintf("%s/%s#%d", owner, name, n), nil
}

type repoItemDecisionResponse struct {
	OK       bool   `json:"ok"`
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	Decision string `json:"decision"`
	Status   string `json:"status"`
	Label    string `json:"label,omitempty"`
}

func renderRepoItemDecisionCloseComment(verb, user, comment, supersededBy string) string {
	by := "an operator"
	if strings.TrimSpace(user) != "" {
		by = "@" + strings.TrimSpace(user)
	}
	if supersededBy != "" {
		verb += " by " + supersededBy
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
		if decision == repoItemDecisionDirection && comment == "" {
			jsonError(w, "a decision needs the direction text", http.StatusBadRequest)
			return
		}
		if itemType == "pr" {
			s.handlePullRequestDecision(ctx, w, repoFull, number, user, decision, comment)
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
			jsonError(w, "this is a pull request; send type \"pr\" to decide it", http.StatusBadRequest)
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
		supersededBy := ""
		if decision == repoItemDecisionCloseSuperseded {
			verb = "superseded"
			ref, err := parseRepoItemSupersededBy(body.SupersededBy, repoFull)
			if err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
			if ref == repoFull+"#"+numStr {
				jsonError(w, "superseded_by: an item cannot supersede itself", http.StatusBadRequest)
				return
			}
			supersededBy = ref
		}
		if err := gh.CreateIssueComment(ctx, repoFull, number, renderRepoItemDecisionCloseComment(verb, user, comment, supersededBy)); err != nil {
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
				// A recorded human decision may close a tracking issue;
				// only the automated paths are refused.
				AllowTrackingIssue: true,
			})
			if err != nil {
				jsonError(w, err.Error(), http.StatusBadGateway)
				return
			}
		}
		s.audit.LogRecord(user, auditRepoItemDecisionClose, auditDetail("repo", repoFull, "number", numStr, "type", itemType, "reason", verb, "superseded_by", supersededBy), "", repoFull, number)
		jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: decision, Status: "closed", Label: label})
	default:
		jsonError(w, "unknown decision; want approve, direction, close-superseded or close-wontfix", http.StatusBadRequest)
	}
}

// handlePullRequestDecision answers approve / direction on a pull request.
func (s *Server) handlePullRequestDecision(ctx context.Context, w http.ResponseWriter, repoFull string, number int, user, decision, comment string) {
	gh := s.deps.GHClient
	numStr := strconv.Itoa(number)
	var (
		head string
		err  error
	)
	if decision == repoItemDecisionApprove {
		var event ghpkg.PullRequestApprovalEvent
		event, err = gh.RecordOperatorPullRequestApproval(ctx, repoFull, number, user)
		head = event.HeadSHA
	} else {
		var event ghpkg.PullRequestDecisionEvent
		event, err = gh.RecordOperatorPullRequestDecision(ctx, repoFull, number, user, comment, repoItemDecisionChangeRequests())
		head = event.HeadSHA
	}
	switch {
	case errors.Is(err, ghpkg.ErrPullRequestNotFound):
		jsonError(w, "pull request not found", http.StatusNotFound)
		return
	case errors.Is(err, ghpkg.ErrPullRequestClosed):
		jsonError(w, "pull request is already closed", http.StatusConflict)
		return
	case errors.Is(err, ghpkg.ErrPullRequestNotHiveAuthored):
		jsonError(w, "the hive did not open this pull request, so it cannot carry a direction to its author; leave a changes-requested review instead", http.StatusConflict)
		return
	case errors.Is(err, ghpkg.ErrPullRequestHeadUnknown):
		jsonError(w, "could not read the pull request head; leave a changes-requested review instead", http.StatusBadGateway)
		return
	case err != nil:
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	action, status := auditRepoItemDecisionApprove, "approval-commented"
	if decision == repoItemDecisionDirection {
		action, status = auditRepoItemDecisionDirection, "direction-recorded"
	}
	s.audit.LogRecord(user, action, auditDetail("repo", repoFull, "number", numStr, "type", "pr", "head", head, "decision", comment), "", repoFull, number)
	jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repoFull, Number: number, Decision: decision, Status: status})
}
