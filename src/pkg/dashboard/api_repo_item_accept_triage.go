package dashboard

import (
	"context"
	"fmt"
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
// labels are the signal the reporter-trust gate admits the issue on at its
// next poll, and a label-driven repository's own sweep reads the approval
// label exactly as if the maintainer had applied it by hand. The handler
// also moves the stage in the same request: an issue with no stage label
// (parked 000-new by the gate) or wearing a Hive-written
// `stage/015-triage-needed` gets `stage/010-actionable` (015 removed with
// the awaiting label), so the row leaves "New" / "Triage needed" on the next
// poll instead of waiting for the engine's cycle (hivecommons/hive#16652
// sat at "New" after the click). A higher stage is never regressed; the
// engine's next pass may refine 010. Authorised like Hold (write permission
// on the repo or the verified owner role); attributed to the signed-in
// dashboard user in the audit log.

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

// acceptTriageStageAdvance is pure: the stage label to add so the accepted
// issue reads 010 Actionable, or "" when it already carries a stage label
// other than 015 (a claim or a covering PR is further along and is never
// regressed) or is on hold (the reconciler strips stage labels from a held
// item; the row reads "on hold" whatever the stage). stageOf reads the
// item's current stage from its labels.
func acceptTriageStageAdvance(labels []string, hiveID string, stageOf func([]string) stagelabels.Stage) string {
	if len(holdLabels(labels, hiveID)) > 0 {
		return ""
	}
	switch stageOf(labels) {
	case stagelabels.None, stagelabels.TriageNeeded:
		return stagelabels.Actionable.Label()
	}
	return ""
}

// acceptTriageLabelsAccepted reports whether labels say triage was accepted:
// the accept label is on and no awaiting label and no 015 stage label remain.
func (s *Server) acceptTriageLabelsAccepted(labels []string) bool {
	accept, awaiting := s.acceptTriageLabels()
	if !acceptTriageHasLabel(labels, accept) {
		return false
	}
	_, remove := acceptTriagePlan(labels, accept, awaiting)
	return len(remove) == 0
}

// stageTrackerAcceptedTriageStage is the stage the tracker shows for an
// issue the engine has no stage for (no record, or a record at None) whose
// labels already say triage was accepted: 010 Actionable rather than
// 000-new, until the engine's next pass records it. "New" is never derived
// from the mere absence of a stage label when the accept label is present
// (hivecommons/hive#16652).
func (s *Server) stageTrackerAcceptedTriageStage(stage stagelabels.Stage, rec stagelabels.Record, hasRec bool, labels []string) stagelabels.Stage {
	if stage != stagelabels.None || (hasRec && rec.Stage != stagelabels.None) {
		return stage
	}
	if s.acceptTriageLabelsAccepted(labels) {
		return stagelabels.Actionable
	}
	return stage
}

// stageTrackerAcceptedTriageRecord reports whether rec parks the issue at
// 015 while its labels already say triage was accepted (the accept label is
// on, no awaiting label and no 015 stage label remain). The labels are the
// newer fact; the record is waiting on the engine's next reconcile, and the
// row reads 010 Actionable meanwhile.
func (s *Server) stageTrackerAcceptedTriageRecord(rec stagelabels.Record, labels []string) bool {
	return rec.Stage == stagelabels.TriageNeeded && s.acceptTriageLabelsAccepted(labels)
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
	if !s.watchesRepo(repoFull) {
		jsonError(w, "repository is not configured for this hive", http.StatusForbidden)
		return
	}
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
	hiveID := ""
	if s.deps.Config != nil {
		hiveID = s.deps.Config.HiveID
	}
	stageLabel := acceptTriageStageAdvance(labels, hiveID, labelStage)
	out := repoAcceptTriageResponse{OK: true, Repo: repoFull, Number: number, AcceptLabel: accept, Added: []string{}, Removed: []string{}}
	if len(add) == 0 && len(remove) == 0 && stageLabel == "" {
		out.MinStatusSeq, err = s.refreshAcceptedTriage(ctx, repoFull, number)
		if err != nil {
			jsonError(w, "Triage is already accepted on GitHub, but the work handoff could not be refreshed: "+err.Error(), http.StatusBadGateway)
			return
		}
		out.AlreadyAccepted = true
		res := rowActionNoopResult(rowActionTarget(repoFull, rowActionTargetIssue, number), "triage was already accepted (`"+accept+"` is on)", time.Time{}, "")
		res.Next = "The reporter-trust gate admits the issue on its next poll."
		out.Result = &res
		jsonResponse(w, out)
		return
	}
	alreadyAccepted := len(add) == 0
	if len(add) > 0 {
		if err := s.deps.GHClient.EnsureIssueLabel(ctx, repoFull, accept, acceptTriageLabelColor, acceptTriageLabelDescription); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	if stageLabel != "" {
		if err := s.deps.GHClient.EnsureIssueLabel(ctx, repoFull, stageLabel, stagelabels.StageColor, stagelabels.Actionable.Description()); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	// Removes before adds so the issue never carries two stage labels
	// (015 off before 010 on), matching stagelabels.Enforce.
	for _, label := range remove {
		if err := s.deps.GHClient.RemoveLabel(ctx, repoFull, number, label); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	out.Removed = remove
	if stageLabel != "" {
		add = append(add, stageLabel)
	}
	if len(add) > 0 {
		if err := s.deps.GHClient.AddLabels(ctx, repoFull, number, add); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Added = add
	}
	s.auditFromRequest(r, acceptTriageAuditAction, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "added", strings.Join(add, ","), "removed", strings.Join(remove, ",")), "")
	out.MinStatusSeq, err = s.refreshAcceptedTriage(ctx, repoFull, number)
	if err != nil {
		jsonError(w, "Triage labels were saved on GitHub, but the work handoff could not be refreshed: "+err.Error(), http.StatusBadGateway)
		return
	}
	outcome := "Triage accepted: `" + strings.Join(add, "`, `") + "` added"
	if alreadyAccepted {
		outcome = "Triage already accepted: `" + strings.Join(add, "`, `") + "` added"
		if len(add) == 0 {
			outcome = "Triage already accepted"
		}
	}
	if len(remove) > 0 {
		outcome += ", `" + strings.Join(remove, "`, `") + "` removed"
	}
	out.Result = &RowActionResult{Action: rowActionTriaged, Target: rowActionTarget(repoFull, rowActionTargetIssue, number), Outcome: outcome + ".",
		Next: acceptTriageNextText(s.governorNextPassIn(time.Now()))}
	jsonResponse(w, out)
}

// Re-read and classify the persisted labels, rather than only painting them in
// the dashboard. This also repairs a retry after a restart or failed refresh,
// without treating the operator-action ledger as lasting admission authority.
func (s *Server) refreshAcceptedTriage(ctx context.Context, repo string, number int) (uint64, error) {
	// ActionableResult keys use project.repos entries (often bare names),
	// whereas dashboard routes use owner/repo.
	if cfg := s.deps.Config; cfg != nil {
		for _, watched := range cfg.Project.Repos {
			if strings.EqualFold(config.QualifyRepo(cfg.Project.Org, watched), repo) {
				repo = watched
				break
			}
		}
	}
	snap, err := s.deps.GHClient.FetchWebhookItemSnapshot(ctx, repo, number, false)
	if err != nil {
		return 0, err
	}
	accept, awaiting := s.acceptTriageLabels()
	_, remove := acceptTriagePlan(snap.Labels, accept, awaiting)
	if snap.IsPR || !snap.Open() || !acceptTriageHasLabel(snap.Labels, accept) || len(remove) > 0 {
		return 0, fmt.Errorf("issue changed during acceptance; reload its current state before retrying")
	}
	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{snap})
	s.statusMu.Lock()
	s.statusMutationEpoch++
	s.statusSeq++
	seq := s.statusSeq
	if s.status != nil {
		next := *s.status
		next.StatusSeq = seq
		s.status = &next
	}
	s.statusMu.Unlock()
	s.invalidateStageTrackerForWebhook()
	return seq, nil
}

// acceptTriageNextText is the result's "next" line: the row reads 010
// actionable now; the hive picks the issue up on its next scanner pass,
// with the governor's ETA when known.
func acceptTriageNextText(eta string) string {
	next := "Hive picks it up on its next scanner pass"
	if eta != "" {
		next += " (≈" + eta + ")"
	}
	return next + "; the row reads 010 actionable now and the reporter-trust gate admits the issue on that poll."
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
