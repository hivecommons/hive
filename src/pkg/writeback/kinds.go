// Package writeback is the single policy choke point for every GitHub write
// the hive makes that nobody asked for on that specific item: platform sweeps
// (sentinel, hold-gate upkeep, stage labels, supersession, duplicate-fix,
// reporter trust, …) that iterate project.repos and comment on, label, review
// or close items the hive did not open.
//
// The moment the GitHub App gained write on a repo that was listed only so
// an agent could file regression issues, the sentinel and hold-gate sweeps
// back-filled 63 comments and 40 labels onto 50 human PRs in 45 minutes.
// ACMM level 4 constrains agents, but these sweeps are not agents and ran
// unconditionally over project.repos. Every such path now asks Gate.Allowed
// before it writes; a refusal is counted, kept for the dashboard
// drill-down, and never reaches GitHub.
//
// The gate applies to UNSOLICITED writes only. It never blocks the hive's
// own PRs and issues, replies to explicit `/hive …` commands, work an
// operator triggered from the dashboard, the agent request relays, or
// agent kicks on items already labelled for the hive — those paths do not
// consult it.
package writeback

import (
	"sort"
	"strings"
)

// Kind names one class of unsolicited write-back. Each kind has a policy
// (minimum ACMM level, whether the item must carry a review opt-in label)
// with a default in DefaultKinds and an override under writeback.kinds.<kind>.
type Kind string

const (
	// KindSentinel is the sentinel alert/notice comment plus the
	// `sentinel-alert` label on a PR whose diff matches a sentinel rule.
	KindSentinel Kind = "sentinel"
	// KindHoldGateNotice is the hold-gate drift comment, the conflict
	// ("⚠️ Hold-gate mergeability") notice, the re-hold label and the
	// merge-from-base push that keeps a held PR mergeable.
	KindHoldGateNotice Kind = "hold_gate_notice"
	// KindStageLabel is every stage/*, role/* and hive-owned ownership label
	// the stage engine writes on an item.
	KindStageLabel Kind = "stage_label"
	// KindNeedsDecisionComment is the `hive:stage-needs-decision` / 085
	// park comment and label on an item a person must decide on.
	KindNeedsDecisionComment Kind = "needs_decision_comment"
	// KindSupersessionNotice is the supersession notice on a contributor PR
	// whose claimed issue a merged PR already closed.
	KindSupersessionNotice Kind = "supersession_notice"
	// KindSupersessionClose closes a superseded contributor PR after the
	// grace period.
	KindSupersessionClose Kind = "supersession_close"
	// KindDuplicateFixNotice is the `<!-- hive:duplicate-fix -->` comment on
	// the later of two PRs fixing one issue.
	KindDuplicateFixNotice Kind = "duplicate_fix_notice"
	// KindReporterTrustHold is the reporter-trust notice and needs-human
	// label on a PR whose linked issue reporter is untrusted.
	KindReporterTrustHold Kind = "reporter_trust_hold"
	// KindCoveredByPRLabel is the covered-by-pr label on an issue an open PR
	// references.
	KindCoveredByPRLabel Kind = "covered_by_pr_label"
	// KindVerdictLabel is the review-verdict label set the review lane
	// writes after a review.
	KindVerdictLabel Kind = "verdict_label"
	// KindContributorAdopt is the contributor-lane adoption/auto-promotion
	// comment on a contributor PR.
	KindContributorAdopt Kind = "contributor_adopt"
	// KindClaimComment is the claim-ledger comment and claim label on an
	// issue the hive is about to work.
	KindClaimComment Kind = "claim_comment"
	// KindIssueClose closes an issue a merged PR fixed (close-on-merge).
	KindIssueClose Kind = "issue_close"
	// KindParentIssueEdit edits a decomposed parent issue's task list.
	KindParentIssueEdit Kind = "parent_issue_edit"
	// KindGateComment is the clarity / prior-art gate comment on an issue
	// the scheduler holds back.
	KindGateComment Kind = "gate_comment"
	// KindWelcomeComment is the first-timer / good-first-issue welcome
	// comment on a contributor item.
	KindWelcomeComment Kind = "welcome_comment"
	// KindIssueFile files a NEW issue the hive authors (advisory digest,
	// tracking, fleet report, regression, shared-CI incident). There is no
	// foreign item to opt in, so RequireLabel never applies; per-repo mode
	// `off` is the only thing that stops it.
	KindIssueFile Kind = "issue_file"
	// KindCIRerun re-runs a workflow or posts `/ok-to-test` on a PR.
	KindCIRerun Kind = "ci_rerun"
)

// KindPolicy is the per-kind rule. A write of this kind on an item the hive
// did not author is allowed when the repo's effective ACMM level is at
// least MinLevel AND (RequireLabel is false OR the item carries a review
// opt-in label). UnlabeledMinLevel, when > 0, additionally admits an item
// WITHOUT the opt-in label once the level reaches it (the sentinel's "L5
// with label, L6 without"). OwnOnly kinds never write on foreign items at
// any level.
type KindPolicy struct {
	MinLevel          int  `yaml:"min_level" json:"min_level"`
	RequireLabel      bool `yaml:"require_label" json:"require_label"`
	UnlabeledMinLevel int  `yaml:"unlabeled_min_level,omitempty" json:"unlabeled_min_level,omitempty"`
	OwnOnly           bool `yaml:"own_only,omitempty" json:"own_only,omitempty"`
}

// KindPolicyOverride is the yaml shape of writeback.kinds.<kind>: every
// field optional so an operator can move one knob.
type KindPolicyOverride struct {
	MinLevel          *int  `yaml:"min_level,omitempty" json:"min_level,omitempty"`
	RequireLabel      *bool `yaml:"require_label,omitempty" json:"require_label,omitempty"`
	UnlabeledMinLevel *int  `yaml:"unlabeled_min_level,omitempty" json:"unlabeled_min_level,omitempty"`
	OwnOnly           *bool `yaml:"own_only,omitempty" json:"own_only,omitempty"`
}

// DefaultKinds is the default matrix: "does the hive have the power this
// write-back exists to govern at that level?" (see
// docs/acmm-policy-matrix.md, "What hive will say on GitHub at each level").
//
//   - L4 is the first level the hive holds its OWN PRs; hold-gate upkeep on
//     those is allowed from L4 and never touches a foreign PR.
//   - L5 is the first level the hive proposes on other people's items —
//     but only items a maintainer opted in with the review label.
//   - L6 is the only level a sentinel alert or a close lands on an item
//     nobody opted in, because L6 is where the hive merges autonomously and
//     a missed finding would merge.
func DefaultKinds() map[Kind]KindPolicy {
	return map[Kind]KindPolicy{
		KindSentinel:             {MinLevel: 5, RequireLabel: true, UnlabeledMinLevel: 6},
		KindHoldGateNotice:       {MinLevel: 4, OwnOnly: true},
		KindStageLabel:           {MinLevel: 5, RequireLabel: true},
		KindNeedsDecisionComment: {MinLevel: 5, RequireLabel: true},
		KindSupersessionNotice:   {MinLevel: 5, RequireLabel: true},
		KindSupersessionClose:    {MinLevel: 6, RequireLabel: true},
		KindDuplicateFixNotice:   {MinLevel: 5, RequireLabel: true},
		KindReporterTrustHold:    {MinLevel: 5, RequireLabel: true},
		KindCoveredByPRLabel:     {MinLevel: 5, RequireLabel: true},
		KindVerdictLabel:         {MinLevel: 3, RequireLabel: true},
		KindContributorAdopt:     {MinLevel: 6, RequireLabel: true},
		KindClaimComment:         {MinLevel: 4, RequireLabel: true},
		KindIssueClose:           {MinLevel: 5, RequireLabel: true},
		KindParentIssueEdit:      {MinLevel: 5, RequireLabel: true},
		KindGateComment:          {MinLevel: 5, RequireLabel: true},
		KindWelcomeComment:       {MinLevel: 5, RequireLabel: true, UnlabeledMinLevel: 6},
		KindIssueFile:            {MinLevel: 3},
		KindCIRerun:              {MinLevel: 6, RequireLabel: true},
	}
}

// Kinds returns every known kind in a stable order; config validation and
// the docs matrix are checked against it.
func Kinds() []Kind {
	m := DefaultKinds()
	out := make([]Kind, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// IsKind reports whether s names a known kind.
func IsKind(s string) bool {
	_, ok := DefaultKinds()[Kind(strings.ToLower(strings.TrimSpace(s)))]
	return ok
}

// IsLabelKind reports whether k writes labels only (per-repo mode
// labels-only admits it; issues-only does not).
func (k Kind) IsLabelKind() bool {
	switch k {
	case KindStageLabel, KindCoveredByPRLabel, KindVerdictLabel:
		return true
	}
	return false
}

// IsIssueFileKind reports whether k files a new issue (per-repo mode
// issues-only admits it).
func (k Kind) IsIssueFileKind() bool { return k == KindIssueFile }
