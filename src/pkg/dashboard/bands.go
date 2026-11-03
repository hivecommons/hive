package dashboard

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const (
	defaultStaleDays = 14
	hoursPerDay      = 24
	// goodFirstIssueLabel mirrors gfi.Label (asserted equal in gfi_test.go);
	// kept local so pkg/dashboard does not import pkg/gfi.
	goodFirstIssueLabel = "good first issue"
)

// needsDecisionLabel is the issue park signal (hivecommons/hive#12683): the
// issue stage label itself, stage/045-needs-decision. prNeedsDecisionLabel
// is the PR parking stage, stage/085-needs-decision (on an issue, the
// pre-045 spelling). The retired `needs-decision` word stays recognised in
// the band and signal checks until the reconciler has migrated it.
const (
	needsDecisionLabel   = github.NeedsDecisionStageLabel
	prNeedsDecisionLabel = github.PRNeedsDecisionStageLabel
)

// labelsNeedDecision reports whether a lower-cased label set says a human
// decision is owed.
func labelsNeedDecision(labels map[string]bool) bool {
	return labels[needsDecisionLabel] || labels[prNeedsDecisionLabel] || labels["needs-decision"] || labels["2-discussing"]
}

var (
	// defaultIssueWaitingLabels are the default parking labels (the
	// stage/045 issue park signal, the pre-045 stage/085 spelling, the
	// retired needs-direction / needs-decision spellings, needs-human,
	// needs-spec, the prior-art park stage/017) plus `blocked`: the waiting band holds parked issues only
	// (hivecommons/hive#11507), and `blocked` is a gate only a person lifts,
	// which the un-park sweep answers like the needs-* labels
	// (hivecommons/hive#12376). static/index.html keeps a copy
	// (REPO_ISSUE_BAND_DEFAULTS.waitingLabels).
	defaultIssueWaitingLabels = []string{"needs-human", needsDecisionLabel, prNeedsDecisionLabel, config.PossiblyExistsStageLabel, "needs-direction", "needs-decision", "needs-spec", blockedWorkflowLabel}
	defaultIssueDoneLabels    = []string{"hive/already-done", "hive/covered-by-pr", "hive/likely-done"}
	prHumanGateLabels         = []string{"needs-human", prNeedsDecisionLabel, "needs-decision", "2-discussing"}
	issueBandOrder            = []string{"ready", "in-progress", "agent-filed", "waiting", "done"}
	prBandOrder               = []string{"waiting", "eligible", "blocked", "in-review", "open", "draft"}
	holdLabelSpellings        = []string{"hold", "on-hold", "hold/review"}
)

// issueLabelChips names the label that put an issue in the waiting band, in
// precedence order: the hard-suppress classifier's order (needs-human,
// needs-direction, needs-decision, needs-spec) first, then the remaining
// waiting labels. The parked-issue chips in index.html use the same words.
var issueLabelChips = []struct{ label, chip string }{
	{"needs-human", "human"},
	{"needs-direction", "direction"},
	{needsDecisionLabel, "decision"},
	{"needs-spec", "spec"},
	{"needs-reporter-confirmation", "reporter"},
	{"blocked", "blocked"},
	{"2-discussing", "discussing"},
	{"epic", "epic"},
	{"needs-triage", "untriaged"},
}

type Signal struct {
	Glyph string `json:"glyph,omitempty"`
	Role  string `json:"role,omitempty"`
	Label string `json:"label"`
}

type IssueBandInfo struct {
	Band         string   `json:"band"`
	Role         string   `json:"role,omitempty"`
	Acknowledged bool     `json:"acknowledged"`
	Held         bool     `json:"held"`
	HoldReason   string   `json:"hold_reason,omitempty"`
	Stale        bool     `json:"stale"`
	Signals      []Signal `json:"signals"`
	Chip         string   `json:"chip,omitempty"`
}

type PRBandInfo struct {
	Band       string   `json:"band"`
	Role       string   `json:"role,omitempty"`
	Held       bool     `json:"held"`
	HoldReason string   `json:"hold_reason,omitempty"`
	Stale      bool     `json:"stale"`
	Signals    []Signal `json:"signals"`
}

type BandSpec struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Short string `json:"short,omitempty"`
	Rule  string `json:"rule"`
	Count int    `json:"count"`
}

type normalizedIssueBandConfig struct {
	WaitingLabels []string
	DoneLabels    []string
	StaleDays     int
}

func normalizeIssueBandsConfig(cfg config.DashboardIssueBandsConfig) normalizedIssueBandConfig {
	staleDays := cfg.StaleDays
	if staleDays <= 0 {
		staleDays = defaultStaleDays
	}
	return normalizedIssueBandConfig{
		WaitingLabels: uniqueLabels(cfg.WaitingLabels, defaultIssueWaitingLabels),
		DoneLabels:    uniqueLabels(cfg.DoneLabels, defaultIssueDoneLabels),
		StaleDays:     staleDays,
	}
}

func uniqueLabels(labels, fallback []string) []string {
	source := labels
	if len(source) == 0 {
		source = fallback
	}
	out := make([]string, 0, len(source))
	seen := map[string]bool{}
	for _, label := range source {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, trimmed)
	}
	return out
}

func IssueBand(issue github.Issue, held bool, cfg config.DashboardIssueBandsConfig, now time.Time, selfAuthorizationHoldActive ...func(string) bool) IssueBandInfo {
	norm := normalizeIssueBandsConfig(cfg)
	labels := labelSet(issue.Labels)
	role := issueAgentRole(issue.Labels)
	acknowledged := issue.HumanAcknowledged || labels["approved-direction"]
	linked := issueLinkedPRState(issue)
	done := issueDoneLabel(labels, norm.DoneLabels) || linked.merged || issueStageCovered(issue.Labels)
	waiting := hasAnyLabel(labels, norm.WaitingLabels)
	inProgress := len(issue.Assignees) > 0 || issueClaimed(issue.Labels) || linked.open
	selfAuthorizationHeld := true
	if len(selfAuthorizationHoldActive) > 0 && selfAuthorizationHoldActive[0] != nil {
		selfAuthorizationHeld = selfAuthorizationHoldActive[0](issue.Repo)
	}
	agentFiled := role != "" && !acknowledged && selfAuthorizationHeld

	band := "ready"
	switch {
	case done:
		band = "done"
	case waiting:
		band = "waiting"
	case inProgress:
		band = "in-progress"
	case agentFiled:
		band = "agent-filed"
	}

	signals := make([]Signal, 0)
	// The label records no reason; GitHub's native "blocked by" links
	// (Issue.DependsOn, #9839) are the only structured one. Name the open
	// ones, and with only the label say plainly that none is recorded.
	if open := slices.Compact(issue.OpenBlockers()); len(open) > 0 {
		signals = append(signals, Signal{Glyph: "⛔", Label: "blocked by " + strings.Join(open, ", ")})
	} else if labels["blocked"] {
		signals = append(signals, Signal{Glyph: "⛔", Label: "labelled blocked — the label records no reason; see the issue"})
	}
	if labelsNeedDecision(labels) {
		signals = append(signals, Signal{Glyph: "❓", Label: "needs decision"})
	}
	if labels["needs-human"] {
		signals = append(signals, Signal{Glyph: "⚠", Label: "labelled needs-human"})
	}
	if labels["epic"] {
		signals = append(signals, Signal{Glyph: "◆", Label: "epic"})
	}
	if labels[goodFirstIssueLabel] {
		signals = append(signals, Signal{Glyph: "🌱", Label: "good first issue — curated for new contributors"})
	}
	if inProgress {
		signals = append(signals, Signal{Glyph: "👤", Label: "assigned or claimed"})
	}
	if linked.open {
		signals = append(signals, Signal{Glyph: "🔗", Label: "open PR references this issue"})
	}
	if done {
		label := "already done"
		if linked.merged {
			label = "merged PR references this issue"
		}
		signals = append(signals, Signal{Glyph: "✓", Label: label})
	}
	if cc := issue.ClaimContext; cc != nil && cc.Decision == github.ClaimDecisionReopenedAfterMerge {
		signals = append(signals, Signal{Glyph: "↺", Label: fmt.Sprintf("reopened after PR #%d merged; re-queued for work", cc.PRNumber)})
	}
	if role != "" {
		ack := ", not yet acknowledged"
		if acknowledged {
			ack = ", acknowledged by a human"
		}
		signals = append(signals, Signal{Role: role, Label: "agent-filed by " + role + ack})
	}
	stale := issueStale(issue.CreatedAt, issue.UpdatedAt, norm.StaleDays, now)
	if stale {
		signals = append(signals, Signal{Glyph: "🕒", Label: fmt.Sprintf("stale: no activity > %dd", norm.StaleDays)})
	}
	chip := ""
	if band == "waiting" {
		chip = issueLabelChip(labels)
	}
	return IssueBandInfo{Band: band, Role: role, Acknowledged: acknowledged, Held: held, HoldReason: heldReason(issue.Labels, false, ""), Stale: stale, Signals: signals, Chip: chip}
}

// issueLabelChip returns the chip word for the highest-precedence waiting
// label in labels (a lower-cased label set), or "" when none matches so the
// frontend falls back to the band's short label.
func issueLabelChip(labels map[string]bool) string {
	for _, entry := range issueLabelChips {
		if labels[entry.label] || (entry.label == needsDecisionLabel && (labels[prNeedsDecisionLabel] || labels["needs-decision"])) {
			return entry.chip
		}
	}
	return ""
}

type linkedPRState struct{ open, merged bool }

// issueDoneLabel reports whether a configured done label is set. The agent's
// hive/verified-open verdict (the referenced PR did not finish the
// issue) cancels hive/likely-done; every other done label still counts
// (hivecommons/hive#11722).
func issueDoneLabel(labels map[string]bool, doneLabels []string) bool {
	if !labels[github.VerifiedOpenLabel] {
		return hasAnyLabel(labels, doneLabels)
	}
	for _, label := range doneLabels {
		lower := strings.ToLower(label)
		if lower != github.LikelyDoneLabel && labels[lower] {
			return true
		}
	}
	return false
}

func issueLinkedPRState(github.Issue) linkedPRState { return linkedPRState{} }

// prGateLabels are the labels that put a pull request in the waiting
// ("Gated") band: the human-gate labels plus the configured waiting labels.
// The your-turn decision reads the same set (hivecommons/hive#12378).
func prGateLabels(norm normalizedIssueBandConfig) []string {
	return append(append([]string{}, prHumanGateLabels...), norm.WaitingLabels...)
}

func PRBand(pr github.PullRequest, held bool, cfg config.DashboardIssueBandsConfig, now time.Time) PRBandInfo {
	return prBand(pr, nil, held, cfg, now, github.AutoMergeQueuedLabel, "")
}

func prBand(pr github.PullRequest, verdict *github.MergeVerdict, held bool, cfg config.DashboardIssueBandsConfig, now time.Time, autoMergeLabel, hiveID string) PRBandInfo {
	norm := normalizeIssueBandsConfig(cfg)
	labels := labelSet(pr.Labels)
	role := prAgentRole(pr)
	review := prGitHubReview(pr)
	waiting := held || hasAnyLabel(labels, prGateLabels(norm)) || len(holdLabels(pr.Labels, hiveID)) > 0
	eligible, blocked, inReview := prBandFlags(pr, verdict, labels, autoMergeLabel, review)

	band := prMergeStateBand(pr, eligible, blocked, inReview)
	if waiting {
		band = "waiting"
	}

	signals := make([]Signal, 0)
	if role != "" {
		signals = append(signals, Signal{Role: role, Label: "agent-authored by " + role})
	}
	if labels["needs-human"] {
		reason := "labelled needs-human"
		if pr.ReporterTrustReason != "" {
			reason = pr.ReporterTrustReason
		}
		signals = append(signals, Signal{Glyph: "⚠", Label: reason})
	}
	if labelsNeedDecision(labels) {
		signals = append(signals, Signal{Glyph: "❓", Label: "needs decision"})
	}
	if held || len(holdLabels(pr.Labels, hiveID)) > 0 {
		signals = append(signals, Signal{Glyph: "⏸", Label: "held"})
	}
	if eligible {
		label := "merge-eligible"
		if prQueued(labels, autoMergeLabel) {
			label = "queued or merge-eligible"
		}
		signals = append(signals, Signal{Glyph: "✓", Label: label})
	}
	if inReview {
		signals = append(signals, Signal{Glyph: "◐", Label: "in review or outstanding"})
	}
	if review != nil {
		signals = append(signals, Signal{Glyph: review.glyph, Label: review.label})
	}
	if requested := prRequestedReviews(pr); len(requested) > 0 {
		signals = append(signals, Signal{Glyph: "👥", Label: "review requested from " + strings.Join(requested, ", ")})
	}
	if total, label := prConversation(pr); total > 0 {
		signals = append(signals, Signal{Glyph: fmt.Sprintf("🗨 %d", total), Label: label})
	}
	if prCIFailing(pr) {
		checks := strings.Join(pr.FailingChecks, ", ")
		if checks == "" {
			checks = "check failure"
		}
		signals = append(signals, Signal{Glyph: "✗ CI", Label: "failing CI: " + checks})
	}
	if pr.Mergeable == github.MergeableNo {
		label := "not mergeable"
		if pr.MergeableState != "" {
			label += ": " + pr.MergeableState
		}
		signals = append(signals, Signal{Glyph: "⑂", Label: label})
	}
	if prQueued(labels, autoMergeLabel) {
		signals = append(signals, Signal{Glyph: "🔀", Label: "queued for auto-merge"})
	}
	stale := issueStale(pr.CreatedAt, pr.UpdatedAt, norm.StaleDays, now)
	if stale {
		signals = append(signals, Signal{Glyph: "🕒", Label: fmt.Sprintf("stale: no activity > %dd", norm.StaleDays)})
	}
	return PRBandInfo{Band: band, Role: role, Held: held, HoldReason: heldReason(pr.Labels, pr.HiveAttributed, hiveID, pr.ReporterTrustReason), Stale: stale, Signals: signals}
}

// prBandFlags reports the merge-state facts every PR band is chosen from.
func prBandFlags(pr github.PullRequest, verdict *github.MergeVerdict, labels map[string]bool, autoMergeLabel string, review *prReviewInfo) (eligible, blocked, inReview bool) {
	// A hand-merge verdict (the repo's auto-merge is off) is never eligible,
	// even with the auto-merge label: every sweep, the queued one included,
	// skips that repo.
	eligible = (verdict == nil || !verdict.HandMerge) &&
		((verdict != nil && verdict.State == github.MergeVerdictEligible) || prQueued(labels, autoMergeLabel))
	blocked = (verdict != nil && verdict.State == github.MergeVerdictBlocked) || pr.Mergeable == github.MergeableNo || prCIFailing(pr)
	// A hand-merge verdict is outstanding because the repo's switch is off,
	// not because anyone is reviewing it; it does not earn the in-review band.
	inReview = (verdict != nil && verdict.State == github.MergeVerdictOutstanding && !verdict.HandMerge) || pr.ReviewURL != "" || review != nil
	return eligible, blocked, inReview
}

// prMergeStateBand is the PR band once the waiting rule is out of the way.
func prMergeStateBand(pr github.PullRequest, eligible, blocked, inReview bool) string {
	switch {
	case eligible:
		return "eligible"
	case blocked:
		return "blocked"
	case inReview:
		return "in-review"
	case pr.Draft:
		return "draft"
	}
	return "open"
}

// prChartBand is the band a PR gets with the waiting rule skipped. The
// Overview charts and the Actionable-now equation use it for PRs the
// ownership decision leaves in no slice of its own (hivecommons/hive#11507).
func prChartBand(pr github.PullRequest, verdict *github.MergeVerdict, autoMergeLabel string) string {
	eligible, blocked, inReview := prBandFlags(pr, verdict, labelSet(pr.Labels), autoMergeLabel, prGitHubReview(pr))
	return prMergeStateBand(pr, eligible, blocked, inReview)
}

// issueChartBand is the band an issue gets with the waiting, agent-filed and
// done rules skipped: Claimed when someone has it, otherwise Unclaimed.
func issueChartBand(issue github.Issue) string {
	if len(issue.Assignees) > 0 || issueClaimed(issue.Labels) || issueLinkedPRState(issue).open {
		return "in-progress"
	}
	return "ready"
}

type prReviewInfo struct{ glyph, label string }

func prGitHubReview(pr github.PullRequest) *prReviewInfo {
	if pr.Protection == nil {
		return nil
	}
	changes := pr.Protection.ChangesRequestedBy
	approvals := pr.Protection.ApprovalsGiven
	who := ""
	if len(changes) > 0 {
		prefixed := make([]string, 0, len(changes))
		for _, login := range changes {
			prefixed = append(prefixed, "@"+login)
		}
		who = " by " + strings.Join(prefixed, ", ")
	}
	given := ""
	if approvals > 0 {
		given = fmt.Sprintf("%d approval", approvals)
		if approvals != 1 {
			given += "s"
		}
	}
	switch strings.ToUpper(string(pr.Protection.ReviewDecision)) {
	case "APPROVED":
		label := "approved on GitHub"
		if given != "" {
			label += " (" + given + ")"
		}
		return &prReviewInfo{glyph: "👍", label: label}
	case "CHANGES_REQUESTED":
		return &prReviewInfo{glyph: "👎", label: "changes requested" + who}
	case "REVIEW_REQUIRED":
		label := "approving review required by branch protection"
		if given != "" {
			label += " (" + given + " given)"
		}
		return &prReviewInfo{glyph: "👀", label: label}
	}
	if len(changes) > 0 {
		return &prReviewInfo{glyph: "👎", label: "changes requested" + who + " — no review decision from GitHub"}
	}
	if approvals > 0 {
		return &prReviewInfo{glyph: "👍", label: given + " on GitHub — no review decision"}
	}
	return nil
}

func IssueBandSpecs(cfg config.DashboardIssueBandsConfig) []BandSpec {
	out := make([]BandSpec, 0, len(issueBandOrder))
	for _, key := range issueBandOrder {
		out = append(out, issueBandSpec(key, cfg))
	}
	return out
}

func PRBandSpecs(cfg config.DashboardIssueBandsConfig) []BandSpec {
	out := make([]BandSpec, 0, len(prBandOrder))
	for _, key := range prBandOrder {
		out = append(out, prBandSpec(key, cfg))
	}
	return out
}

func issueBandSpec(key string, cfg config.DashboardIssueBandsConfig) BandSpec {
	norm := normalizeIssueBandsConfig(cfg)
	switch key {
	case "in-progress":
		return BandSpec{Key: key, Label: "Claimed", Short: "claimed", Rule: "assigned or claimed by an agent — nothing needed unless it stalls"}
	case "agent-filed":
		return BandSpec{Key: key, Label: "Needs triage", Short: "triage", Rule: "filed by an agent (agent/<role> label) with no approved-direction label, no human assignee, and self-authorization hold is on for the repo (ACMM < 6 or github.self_authorization_hold=true) — agents do not pick it up until it is labelled, assigned to a human, or closed. A human comment also acknowledges for #5117 but is not in the snapshot, so a commented-on proposal still shows here"}
	case "waiting":
		return BandSpec{Key: key, Label: "Parked", Short: "parked", Rule: "labelled " + strings.Join(norm.WaitingLabels, ", ") + " — parked: agents do not continue until the label is removed or the question it names is answered"}
	case "done":
		return BandSpec{Key: key, Label: "Confirm & close", Short: "close?", Rule: "an agent applied " + strings.Join(norm.DoneLabels, ", ") + " — the agent reports the work as landed, and the issue is still open"}
	default:
		return BandSpec{Key: "ready", Label: "Unclaimed", Short: "unclaimed", Rule: "no other band matched — nobody is assigned, nothing claimed it, and no human gate applies; this does not by itself mean agents will pick it up"}
	}
}

func prBandSpec(key string, cfg config.DashboardIssueBandsConfig) BandSpec {
	norm := normalizeIssueBandsConfig(cfg)
	switch key {
	case "waiting":
		labels := uniqueLowerLabels(append(append([]string{}, prHumanGateLabels...), norm.WaitingLabels...))
		return BandSpec{Key: key, Label: "Gated", Rule: "held, or labelled " + strings.Join(labels, ", ") + " — gated: automation does not continue until the hold is released or the label is removed"}
	case "eligible":
		return BandSpec{Key: key, Label: "Merge-eligible", Rule: "sweep verdict eligible, or queued for Hive auto-merge — nothing needed; it merges on its own"}
	case "blocked":
		return BandSpec{Key: key, Label: "Blocked", Rule: "blocked sweep verdict, merge conflicts, or failing CI"}
	case "in-review":
		return BandSpec{Key: key, Label: "In review", Rule: "outstanding sweep verdict, a Hive review posted, or a GitHub review decision — nothing needed until the review resolves"}
	case "draft":
		return BandSpec{Key: key, Label: "Draft", Rule: "draft on GitHub — nothing needed until it is marked ready for review"}
	default:
		return BandSpec{Key: "open", Label: "Open", Rule: "no verdict, review, hold, or draft flag yet — an ordinary open PR"}
	}
}

func uniqueLowerLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	seen := map[string]bool{}
	for _, label := range labels {
		lower := strings.ToLower(strings.TrimSpace(label))
		if lower == "" || seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, lower)
	}
	return out
}

func labelSet(labels []string) map[string]bool {
	out := make(map[string]bool, len(labels))
	for _, label := range labels {
		out[strings.ToLower(label)] = true
	}
	return out
}

func hasAnyLabel(set map[string]bool, labels []string) bool {
	for _, label := range labels {
		if set[strings.ToLower(label)] {
			return true
		}
	}
	return false
}

func issueAgentRole(labels []string) string {
	for _, label := range labels {
		if strings.HasPrefix(strings.ToLower(label), "agent/") {
			parts := strings.SplitN(label, "/", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

// Stage-family ordinals the claimed check needs (hivecommons/hive#12436). The
// numbers are the `stage/NNN-` prefix of the labels pkg/stagelabels emits;
// bands_inprogress_test.go pins them to that table. Coverage is NOT an ordinal
// test: see issueStageCovered.
const (
	stageNumberClaimed      = 20
	stageNumberPRInProgress = 30
	stageLabelPrefix        = "stage/"
)

// stageNumber returns the ordinal of a `stage/NNN-slug` label, or 0 for any
// other label (including the health markers stage/stalled|regressed).
func stageNumber(label string) int {
	rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(label)), stageLabelPrefix)
	if !ok {
		return 0
	}
	digits, _, found := strings.Cut(rest, "-")
	if !found || digits == "" {
		return 0
	}
	n := 0
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// issueStageCovered reports whether the issue carries stage/040-covered-by-pr
// (or, until the reconciler's next pass, a PR stage an older build mirrored
// past it) — the stage successor of the retired covered-by-PR labels, so the
// issue files under "done" the same way. The parking stages are numbered past
// 040 but mean the opposite — 045 needs-decision (and 085, its pre-045 issue
// spelling) is an issue waiting on a person, not one a PR covers — so this
// asks the stage table (stagelabels.IssueCoveredByPR) instead of comparing
// ordinals, which marked every parked issue "already done".
func issueStageCovered(labels []string) bool {
	return stagelabels.IssueCoveredByPR(labels)
}

// issueClaimed reports whether labels say someone holds the issue: the
// stage/020-claimed and stage/030-pr-in-progress stages, the
// `hive/claimed-by-<who>` modifier, or (migration cycle only) the retired
// `claimed` / `hive/in-progress` labels.
func issueClaimed(labels []string) bool {
	for _, label := range labels {
		lower := strings.ToLower(label)
		if lower == claims.LegacyLabelClaimed || lower == claims.LegacyLabelInProgress || strings.HasPrefix(lower, "hive/claimed-by-") {
			return true
		}
		if n := stageNumber(lower); n == stageNumberClaimed || n == stageNumberPRInProgress {
			return true
		}
	}
	return false
}

func prAgentRole(pr github.PullRequest) string {
	if strings.TrimSpace(pr.HiveAgent) != "" {
		return strings.TrimSpace(pr.HiveAgent)
	}
	if role := issueAgentRole(pr.Labels); role != "" {
		return role
	}
	if pr.AppAuthored {
		return "app"
	}
	return ""
}

func prQueued(labels map[string]bool, autoMergeLabel string) bool {
	want := strings.ToLower(strings.TrimSpace(autoMergeLabel))
	if want == "" {
		want = github.AutoMergeQueuedLabel
	}
	return labels[want]
}

func prCIFailing(pr github.PullRequest) bool {
	return strings.ToLower(pr.CIStatus) == "failing" && len(pr.FailingChecks) > 0
}

func prRequestedReviews(pr github.PullRequest) []string {
	out := make([]string, 0, len(pr.RequestedReviewers)+len(pr.RequestedTeams))
	for _, login := range pr.RequestedReviewers {
		out = append(out, "@"+login)
	}
	for _, team := range pr.RequestedTeams {
		out = append(out, "team "+team)
	}
	return out
}

func prConversation(pr github.PullRequest) (int, string) {
	comments := pr.CommentCount
	threads := pr.ReviewThreadCount
	if comments == 0 && threads == 0 {
		return 0, ""
	}
	parts := make([]string, 0, 2)
	if comments > 0 {
		parts = append(parts, plural(comments, "comment"))
	}
	if threads > 0 {
		parts = append(parts, plural(threads, "review thread"))
	}
	return comments + threads, strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func issueStale(createdAt, updatedAt time.Time, staleDays int, now time.Time) bool {
	activity := updatedAt
	if activity.IsZero() {
		activity = createdAt
	}
	if activity.IsZero() {
		return false
	}
	return now.Sub(activity) > time.Duration(staleDays*hoursPerDay)*time.Hour
}

func holdLabels(labels []string, hiveID string) []string {
	canonical := "hold"
	if trimmed := strings.TrimSpace(hiveID); trimmed != "" {
		canonical = "hive-pause/" + trimmed
	}
	canonical = strings.ToLower(canonical)
	out := make([]string, 0)
	for _, label := range labels {
		lower := strings.ToLower(label)
		if lower == canonical || lower == "do-not-merge" || strings.HasPrefix(lower, "do-not-merge/") || containsAnyHoldLabel(lower, holdLabelSpellings) {
			out = append(out, label)
		}
	}
	return out
}

func containsAnyHoldLabel(s string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func heldReason(labels []string, hiveAttributed bool, hiveID string, evidence ...string) string {
	held := holdLabels(labels, hiveID)
	parts := make([]string, 0, 3)
	labelText := ""
	if len(held) > 0 {
		quoted := make([]string, 0, len(held))
		for _, label := range held {
			quoted = append(quoted, "`"+label+"`")
		}
		labelText = " — label " + strings.Join(quoted, ", ")
	}
	parts = append(parts, "On hold"+labelText+": agents will not act on this until the hold label is removed")
	if len(evidence) > 0 && evidence[0] != "" {
		parts = append(parts, evidence[0]+", a maintainer must review this")
		return strings.Join(parts, "; ")
	}
	if hiveAttributed {
		parts = append(parts, "opened by a hive agent — a generic `hold` here is usually the ACMM level gate; the dashboard hive-pause hold is only removed by an operator")
	}
	for _, label := range labels {
		if label == "needs-human" {
			parts = append(parts, "needs-human: automated fix attempts exhausted, a human must review this")
			break
		}
	}
	return strings.Join(parts, "; ")
}

func issueBandRank(band string) int { return bandRank(issueBandOrder, band) }
func prBandRank(band string) int    { return bandRank(prBandOrder, band) }

func bandRank(order []string, band string) int {
	for i, key := range order {
		if key == band {
			return i
		}
	}
	return len(order)
}
