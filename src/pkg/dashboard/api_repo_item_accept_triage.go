package dashboard

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Accept triage from the activity monitor's row menu: the one-click form of
// what a maintainer does on GitHub when a "Triage needed" (015) issue is
// theirs — add the reporter-trust triage label (`triage/accepted` or the
// first of `issue_filter.reporter_trust.untrusted_require_labels`) and drop
// the awaiting label (`needs-triage` / `reporter_trust.awaiting_label`). The
// labels are the whole signal: the reporter-trust gate admits the issue on
// its next poll, the stage engine moves 015 → 010 (never a regression), and
// a label-driven repository's own sweep reads the approval label exactly as
// if the maintainer had applied it by hand. A Hive-written
// `stage/015-triage-needed` label is removed with the awaiting label so the
// chevron does not keep reading "Triage needed" until the engine's next
// cycle. Authorised like Hold (write permission on the repo or the verified
// owner role); attributed to the signed-in dashboard user in the audit log.

const (
	acceptTriageAuditAction      = "repo_item_accept_triage"
	acceptTriageLabelColor       = "0e8a16"
	acceptTriageLabelDescription = "Triage accepted by a maintainer: the hive may act on this issue."
)

type repoAcceptTriageResponse struct {
	OK              bool     `json:"ok"`
	Repo            string   `json:"repo"`
	Number          int      `json:"number"`
	AcceptLabel     string   `json:"acceptLabel"`
	Added           []string `json:"added"`
	Removed         []string `json:"removed"`
	AlreadyAccepted bool     `json:"alreadyAccepted"`
	MinStatusSeq    uint64   `json:"minStatusSeq"`
	// Result is the shared row-action outcome for the dashboard's toast.
	Result *RowActionResult `json:"result,omitempty"`
}

// acceptTriageLabels returns the triage label to add and the awaiting labels
// to remove, from the reporter-trust config or its defaults.
func (s *Server) acceptTriageLabels() (accept string, awaiting []string) {
	var rt config.ReporterTrustConfig
	if s != nil && s.deps != nil && s.deps.Config != nil {
		rt = s.deps.Config.Project.IssueFilter.ReporterTrust
	}
	for _, l := range rt.EffectiveUntrustedRequireLabels() {
		if l = strings.TrimSpace(l); l != "" {
			accept = l
			break
		}
	}
	if accept == "" {
		accept = config.DefaultUntrustedRequireLabels[0]
	}
	for _, l := range []string{rt.EffectiveAwaitingLabel(), config.DefaultReporterTrustAwaitingLabel} {
		if l = strings.TrimSpace(l); l != "" && !acceptTriageHasLabel(awaiting, l) {
			awaiting = append(awaiting, l)
		}
	}
	return accept, awaiting
}

func acceptTriageHasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

// acceptTriagePlan is pure: which labels to add and remove so that labels
// read "triage accepted". Nothing to do when the accept label is already on
// and no awaiting / 015 label remains.
func acceptTriagePlan(labels []string, accept string, awaiting []string) (add, remove []string) {
	if !acceptTriageHasLabel(labels, accept) {
		add = append(add, accept)
	}
	for _, l := range labels {
		trimmed := strings.TrimSpace(l)
		if acceptTriageHasLabel(awaiting, trimmed) || strings.EqualFold(trimmed, stagelabels.TriageNeeded.Label()) {
			if !acceptTriageHasLabel(remove, trimmed) {
				remove = append(remove, trimmed)
			}
		}
	}
	return add, remove
}

// acceptTriageLabelsAfter is pure: the labels an item carries once the plan
// has been applied.
func acceptTriageLabelsAfter(labels, add, remove []string) []string {
	out := make([]string, 0, len(labels)+len(add))
	for _, l := range labels {
		if !acceptTriageHasLabel(remove, l) {
			out = append(out, l)
		}
	}
	for _, l := range add {
		if !acceptTriageHasLabel(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// stageTrackerAcceptedTriageRecord reports whether rec parks the issue at
// 015 while its labels already say triage was accepted (the accept label is
// on, no awaiting label and no 015 stage label remain). The labels are the
// newer fact; the record is waiting on the engine's next reconcile, and the
// row reads 010 Actionable meanwhile.
func (s *Server) stageTrackerAcceptedTriageRecord(rec stagelabels.Record, labels []string) bool {
	if rec.Stage != stagelabels.TriageNeeded {
		return false
	}
	accept, awaiting := s.acceptTriageLabels()
	if !acceptTriageHasLabel(labels, accept) {
		return false
	}
	_, remove := acceptTriagePlan(labels, accept, awaiting)
	return len(remove) == 0
}

func (s *Server) handleRepoItemAcceptTriage(w http.ResponseWriter, r *http.Request) {
	owner, repoName, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	allowed, status, msg := s.canToggleRepoHold(r, owner, repoName)
	if !allowed {
		jsonError(w, msg, status)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	ctx := context.Background()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	repoFull := owner + "/" + repoName
	issue, resp, err := s.deps.GHClient.GoGitHub().Issues.Get(ctx, owner, repoName, number)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			jsonError(w, "issue not found", http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	if issue.IsPullRequest() {
		jsonError(w, "only issues are triaged; pull requests have no triage state", http.StatusNotFound)
		return
	}
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, label.GetName())
	}
	accept, awaiting := s.acceptTriageLabels()
	add, remove := acceptTriagePlan(labels, accept, awaiting)
	out := repoAcceptTriageResponse{OK: true, Repo: repoFull, Number: number, AcceptLabel: accept, Added: []string{}, Removed: []string{}}
	if len(add) == 0 && len(remove) == 0 {
		out.AlreadyAccepted = true
		res := rowActionNoopResult(rowActionTarget(repoFull, rowActionTargetIssue, number), "triage was already accepted (`"+accept+"` is on)", time.Time{}, "")
		res.Next = "The reporter-trust gate admits the issue on its next poll."
		out.Result = &res
		jsonResponse(w, out)
		return
	}
	if len(add) > 0 {
		if err := s.deps.GHClient.EnsureIssueLabel(ctx, repoFull, accept, acceptTriageLabelColor, acceptTriageLabelDescription); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		if err := s.deps.GHClient.AddLabels(ctx, repoFull, number, add); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Added = add
	}
	for _, label := range remove {
		if err := s.deps.GHClient.RemoveLabel(ctx, repoFull, number, label); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	out.Removed = remove
	s.auditFromRequest(r, acceptTriageAuditAction, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "added", strings.Join(add, ","), "removed", strings.Join(remove, ",")), "")
	out.MinStatusSeq = s.applyAcceptTriageToStatus(repoFull, number, add, remove)
	outcome := "Triage accepted: `" + accept + "` added"
	if len(remove) > 0 {
		outcome += ", `" + strings.Join(remove, "`, `") + "` removed"
	}
	out.Result = &RowActionResult{Action: rowActionTriaged, Target: rowActionTarget(repoFull, rowActionTargetIssue, number), Outcome: outcome + ".",
		Next: acceptTriageNextText(s.governorNextPassIn(time.Now()))}
	jsonResponse(w, out)
}

// acceptTriageNextText is the result's "next" line: the hive picks the
// issue up on its next scanner pass, with the governor's ETA when known.
func acceptTriageNextText(eta string) string {
	next := "Hive picks it up on its next scanner pass"
	if eta != "" {
		next += " (≈" + eta + ")"
	}
	return next + "; the reporter-trust gate admits the issue on that poll and the stage engine records it 010 actionable."
}

// governorNextPassIn is the governor's published next kick as an ETA
// ("13m", "due now"), or "" when none is published.
func (s *Server) governorNextPassIn(now time.Time) string {
	if s == nil {
		return ""
	}
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	if s.status == nil {
		return ""
	}
	t, ok := parseGovernorNextKick(s.status.Governor.NextKickAt)
	if !ok {
		return ""
	}
	return formatETA(t.Sub(now))
}

// applyAcceptTriageToStatus patches the issue's labels in the status
// snapshot so the activity monitor's next reconcile (and the stage tracker
// it is built from) reads the accepted state before the next GitHub poll.
func (s *Server) applyAcceptTriageToStatus(repoFull string, number int, add, remove []string) uint64 {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.statusMutationEpoch++
	minStatusSeq := s.statusSeq + 1
	status := s.status
	if status == nil {
		return minStatusSeq
	}
	patch := func(items []any) {
		for i, raw := range items {
			if repoHoldItemNumber(raw) != number && acceptTriageWorkIssueNumber(raw) != number {
				continue
			}
			items[i] = acceptTriageSetItemLabels(raw, acceptTriageLabelsAfter(acceptTriageItemLabels(raw), add, remove))
		}
	}
	for ri := range status.Repos {
		repo := &status.Repos[ri]
		if !repoHoldStatusRepoMatches(*repo, repoFull) {
			continue
		}
		patch(repo.ActionableIssues)
		patch(repo.NonActionableIssues)
		patch(repo.HeldIssues)
		break
	}
	s.statusSeq++
	status.StatusSeq = s.statusSeq
	return minStatusSeq
}

func acceptTriageWorkIssueNumber(raw any) int {
	switch v := raw.(type) {
	case github.RepoWorkIssue:
		return v.Number
	case *github.RepoWorkIssue:
		if v != nil {
			return v.Number
		}
	}
	return 0
}

func acceptTriageItemLabels(raw any) []string {
	switch v := raw.(type) {
	case github.RepoWorkIssue:
		return append([]string(nil), v.Labels...)
	case *github.RepoWorkIssue:
		if v != nil {
			return append([]string(nil), v.Labels...)
		}
	}
	return repoHoldItemLabels(raw)
}

func acceptTriageSetItemLabels(raw any, labels []string) any {
	switch v := raw.(type) {
	case github.RepoWorkIssue:
		v.Labels = append([]string(nil), labels...)
		return v
	case *github.RepoWorkIssue:
		if v != nil {
			cp := *v
			cp.Labels = append([]string(nil), labels...)
			return cp
		}
	}
	return repoHoldSetItemLabels(raw, labels)
}
