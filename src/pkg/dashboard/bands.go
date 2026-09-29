package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	defaultStaleDays = 14
	hoursPerDay      = 24
)

var (
	defaultIssueWaitingLabels = []string{"blocked", "needs-decision", "2-discussing", "Epic", "needs-human", "needs-triage"}
	defaultIssueDoneLabels    = []string{"hive/already-done", "hive/covered-by-pr", "hive/likely-done"}
	prHumanGateLabels         = []string{"needs-human", "needs-decision", "2-discussing"}
	issueBandOrder            = []string{"ready", "in-progress", "agent-filed", "waiting", "done"}
	prBandOrder               = []string{"waiting", "eligible", "blocked", "in-review", "open", "draft"}
	holdLabelSpellings        = []string{"hold", "on-hold", "hold/review"}
)

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
	done := hasAnyLabel(labels, norm.DoneLabels) || linked.merged
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
	if labels["blocked"] {
		signals = append(signals, Signal{Glyph: "⛔", Label: "blocked"})
	}
	if labels["needs-decision"] || labels["2-discussing"] {
		signals = append(signals, Signal{Glyph: "❓", Label: "needs decision"})
	}
	if labels["epic"] {
		signals = append(signals, Signal{Glyph: "◆", Label: "epic"})
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
	return IssueBandInfo{Band: band, Role: role, Acknowledged: acknowledged, Held: held, HoldReason: heldReason(issue.Labels, false, ""), Stale: stale, Signals: signals}
}

type linkedPRState struct{ open, merged bool }

func issueLinkedPRState(github.Issue) linkedPRState { return linkedPRState{} }

func PRBand(pr github.PullRequest, held bool, cfg config.DashboardIssueBandsConfig, now time.Time) PRBandInfo {
	return prBand(pr, nil, held, cfg, now, github.AutoMergeQueuedLabel, "")
}

func prBand(pr github.PullRequest, verdict *github.MergeVerdict, held bool, cfg config.DashboardIssueBandsConfig, now time.Time, autoMergeLabel, hiveID string) PRBandInfo {
	norm := normalizeIssueBandsConfig(cfg)
	labels := labelSet(pr.Labels)
	role := prAgentRole(pr)
	review := prGitHubReview(pr)
	waiting := held || hasAnyLabel(labels, append(append([]string{}, prHumanGateLabels...), norm.WaitingLabels...)) || len(holdLabels(pr.Labels, hiveID)) > 0
	eligible := (verdict != nil && verdict.State == github.MergeVerdictEligible) || prQueued(labels, autoMergeLabel)
	blocked := (verdict != nil && verdict.State == github.MergeVerdictBlocked) || pr.Mergeable == github.MergeableNo || prCIFailing(pr)
	inReview := (verdict != nil && verdict.State == github.MergeVerdictOutstanding) || pr.ReviewURL != "" || review != nil

	band := "open"
	switch {
	case waiting:
		band = "waiting"
	case eligible:
		band = "eligible"
	case blocked:
		band = "blocked"
	case inReview:
		band = "in-review"
	case pr.Draft:
		band = "draft"
	}

	signals := make([]Signal, 0)
	if role != "" {
		signals = append(signals, Signal{Role: role, Label: "agent-authored by " + role})
	}
	if labels["needs-human"] {
		signals = append(signals, Signal{Glyph: "⚠", Label: "needs human review"})
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
	return PRBandInfo{Band: band, Role: role, Held: held, HoldReason: heldReason(pr.Labels, pr.HiveAttributed, hiveID), Stale: stale, Signals: signals}
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
		return BandSpec{Key: key, Label: "Claimed", Short: "claimed", Rule: "assigned, claimed by an agent, or an open PR references it — nothing needed unless it stalls"}
	case "agent-filed":
		return BandSpec{Key: key, Label: "Needs triage", Short: "triage", Rule: "filed by an agent (agent/<role> label) with no approved-direction label, no human assignee, and self-authorization hold is on for the repo (ACMM < 6 or github.self_authorization_hold=true) — add the label, assign a human, or close it. A human comment also acknowledges for #5117 but is not in the snapshot, so a commented-on proposal still shows here"}
	case "waiting":
		return BandSpec{Key: key, Label: "Needs human", Short: "needs human", Rule: "labelled " + strings.Join(norm.WaitingLabels, ", ") + " — a human must unblock or decide before agents continue"}
	case "done":
		return BandSpec{Key: key, Label: "Confirm & close", Short: "close?", Rule: "an agent applied " + strings.Join(norm.DoneLabels, ", ") + " or a merged PR references it — verify the work landed and close the issue"}
	default:
		return BandSpec{Key: "ready", Label: "Unclaimed", Short: "unclaimed", Rule: "no other band matched — nobody is assigned, nothing claimed it, and no human gate applies; this does not by itself mean agents will pick it up"}
	}
}

func prBandSpec(key string, cfg config.DashboardIssueBandsConfig) BandSpec {
	norm := normalizeIssueBandsConfig(cfg)
	switch key {
	case "waiting":
		labels := uniqueLowerLabels(append(append([]string{}, prHumanGateLabels...), norm.WaitingLabels...))
		return BandSpec{Key: key, Label: "Needs human", Rule: "held, or labelled " + strings.Join(labels, ", ") + " — a human must review, decide, or release the hold before automation continues"}
	case "eligible":
		return BandSpec{Key: key, Label: "Merge-eligible", Rule: "sweep verdict eligible, or queued for Hive auto-merge — nothing needed; it merges on its own"}
	case "blocked":
		return BandSpec{Key: key, Label: "Blocked", Rule: "blocked sweep verdict, merge conflicts, or failing CI — read the verdict, rebase, or fix the checks"}
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

func issueClaimed(labels []string) bool {
	for _, label := range labels {
		lower := strings.ToLower(label)
		if lower == "claimed" || strings.HasPrefix(lower, "hive/claimed-by-") {
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
		if lower == canonical || containsAnyHoldLabel(lower, holdLabelSpellings) {
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

func heldReason(labels []string, hiveAttributed bool, hiveID string) string {
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
