package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/blocker"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// workflowsHandoffState validates the current fleet and installation, rather
// than relying on the potentially stale tracker snapshot.
func (s *Server) workflowsHandoffState(ctx context.Context, repo string, labels []string) (string, bool) {
	if s.deps == nil {
		return "", false
	}
	lane := ""
	if s.deps.Config != nil {
		lane = s.deps.Config.WorkflowsLaneFor(stageTrackerAgentLane(labels), repo)
	}
	info, err := installationPermissionProbe(ctx, s.deps.GHAppAuth)
	granted := err == nil && info != nil && info.Granted("workflows", "write")
	if !granted {
		lane = ""
	}
	return lane, granted
}

func (s *Server) executeWorkflowsHandoff(ctx context.Context, repo string, number int, user, lane string, labels []string, delegated bool) (RowActionResult, error) {
	gh := s.deps.GHClient
	if err := gh.AddLabels(ctx, repo, number, []string{ghpkg.WorkflowsLaneLabel}); err != nil { // writeback:solicited dashboard handoff
		return RowActionResult{}, fmt.Errorf("adding workflows routing label: %w", err)
	}
	text := "Hand the workflow change to `" + lane + "`, whose token can push workflow files."
	if delegated {
		text += " " + delegatedecision.Marker(s.delegateHiveID())
	}
	if _, err := gh.RecordOperatorIssueDecision(ctx, repo, number, user, text); err != nil {
		alreadyRouted := false
		for _, label := range labels {
			if strings.EqualFold(label, ghpkg.WorkflowsLaneLabel) {
				alreadyRouted = true
			}
		}
		if !alreadyRouted {
			if rollbackErr := gh.RemoveLabel(ctx, repo, number, ghpkg.WorkflowsLaneLabel); rollbackErr != nil {
				return RowActionResult{}, fmt.Errorf("recording workflows handoff: %w; removing routing label: %v", err, rollbackErr)
			}
		}
		return RowActionResult{}, fmt.Errorf("recording workflows handoff: %w", err)
	}
	if delegated {
		s.noteDelegatedUnpark(repo, number)
	}
	blockerLedger().ClearItem(repo, number)
	s.invalidateStageTrackerCache()
	return RowActionResult{Action: rowActionReleased, Target: rowActionTarget(repo, rowActionTargetIssue, number),
		Outcome: "Handed to `" + lane + "` and recorded the decision.",
		Next:    "The scheduler routes the issue to `" + lane + "` on its next pass."}, nil
}

func (s *Server) handleRepoItemBlockerHandoff(w http.ResponseWriter, r *http.Request) {
	owner, name, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	allowed, status, msg := s.canToggleRepoHold(r, owner, name)
	if !allowed {
		jsonError(w, msg, status)
		return
	}
	user := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if user == "" {
		jsonError(w, "GitHub user session required", http.StatusUnauthorized)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	repo := owner + "/" + name
	b := stageTrackerBlockerFor(repo, number)
	if b == nil {
		jsonError(w, "no blocker is recorded on this item", http.StatusNotFound)
		return
	}
	if blocker.Kind(b.Kind) != blocker.CapabilityWorkflowsPermission {
		jsonError(w, "handoff requires a workflows permission blocker", http.StatusConflict)
		return
	}
	ctx := r.Context()
	issue, _, err := s.deps.GHClient.GoGitHub().Issues.Get(ctx, owner, name, number)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	if issue.IsPullRequest() || !strings.EqualFold(issue.GetState(), "open") {
		jsonError(w, "handoff requires an open issue", http.StatusConflict)
		return
	}
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, label.GetName())
	}
	if len(holdLabels(labels, s.delegateHiveID())) > 0 {
		jsonError(w, "issue is held", http.StatusConflict)
		return
	}
	lane, _ := s.workflowsHandoffState(ctx, repo, labels)
	if lane == "" {
		jsonError(w, "no workflows-capable lane with installation permission serves this issue", http.StatusConflict)
		return
	}
	result, err := s.executeWorkflowsHandoff(ctx, repo, number, user, lane, labels, false)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, ghpkg.ErrIssueNotParked) || errors.Is(err, ghpkg.ErrDecisionTargetIsPR) {
			status = http.StatusConflict
		}
		jsonError(w, err.Error(), status)
		return
	}
	s.audit.LogRecord(user, "repo_item_blocker_handoff", auditDetail("repo", repo, "number", strconv.Itoa(number), "lane", lane), "", repo, number)
	jsonResponse(w, repoItemDecisionResponse{OK: true, Repo: repo, Number: number, Decision: blockerActionWorkflowsHandoff, Status: "released", Label: ghpkg.WorkflowsLaneLabel, Result: &result})
}
