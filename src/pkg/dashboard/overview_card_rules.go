package dashboard

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Read-only copies of rules that live elsewhere, for the Overview card
// (overview_card.go). The card only reads what the hive already decided; it
// never decides. Each helper names its source, and a shared truth table
// (testdata/dependency_bot_rules.json) pins the dependency-bot copies to the
// cmd/hive originals, which stay where they are.

// cardBotConflictGrace and cardBotRedCIGrace copy depBotConflictGrace and
// depBotRedCIGrace (cmd/hive/dependency_bot_handoffs.go): how long a
// dependency-bot PR may conflict, or stay red, before the sweep hands it to
// a person.
const (
	cardBotConflictGrace = 24 * time.Hour
	cardBotRedCIGrace    = 4 * time.Hour
)

// cardDependabotAuthor copies depBotDependabotAuthor
// (cmd/hive/dependency_bot_rebase_request.go): Dependabot refuses commands
// from apps, so the hive never asks it to rebase.
const cardDependabotAuthor = "dependabot[bot]"

// cardDependencyBotRedCI copies dependencyBotRedCI
// (cmd/hive/dependency_bot_handoffs.go): a red required check on the current
// head that the bot PR itself must answer for.
func cardDependencyBotRedCI(pr github.PullRequest) bool {
	return pr.HasFailingRequiredCheck() && !pr.CIChecksRunning && !pr.ExternalCIDeferred && pr.SharedCIIncident == 0
}

// cardConflictMergeState copies conflictMergeState
// (cmd/hive/merge_eligibility.go).
func cardConflictMergeState(pr github.PullRequest) bool {
	if cardHasLabel(pr.Labels, "needs-rebase") {
		return true
	}
	state := strings.ToLower(strings.TrimSpace(pr.MergeableState))
	return state == "dirty" || state == "conflicting"
}

// cardBotRebaseCandidate copies staleRedBotRebaseCandidate
// (cmd/hive/stale_red_bot_rebase.go) with dependencyBotBranchCandidate
// (cmd/hive/dependency_bot_rebase_request.go): a dependency-bot branch, open
// and not a draft, not parked with needs-human, hold or do-not-merge, not
// Dependabot's and not adopted by the hive.
func cardBotRebaseCandidate(pr github.PullRequest) bool {
	if pr.Draft || (pr.State != "" && !strings.EqualFold(pr.State, "open")) {
		return false
	}
	if !github.DependencyBotBranch(pr.HeadRef, pr.Author) {
		return false
	}
	if escalation.HasNeedsHumanLabel(pr.Labels) || github.HasHoldLabel(pr.Labels) || cardHasLabel(pr.Labels, "do-not-merge") {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(pr.Author), cardDependabotAuthor) &&
		!cardHasLabel(pr.Labels, stagelabels.AdoptedByHive.Label())
}

// cardBotRebaseItem builds the write-back gate's view of a PR the way the
// client's WritebackItemForPR does (pkg/github/writeback_gate.go), from the
// snapshot row alone: the card has no client to ask about hive logins.
func cardBotRebaseItem(pr github.PullRequest) writeback.Item {
	return writeback.Item{
		Number:       pr.Number,
		IsPR:         true,
		Author:       pr.Author,
		Labels:       pr.Labels,
		CreatedAt:    pr.CreatedAt,
		HiveAuthored: pr.AppAuthored || pr.HiveAttributed,
	}
}

// cardBotRebaseAllowed is the gate check of requestPossible
// (cmd/hive/dependency_bot_rebase_request.go), read from the stash: the
// answer cardGateAllows gave at publish for this PR. With no gate installed
// it allows, as a nil gate does in cmd/hive. known is false for a candidate
// the stash has no answer for (a row added since the publish).
func cardBotRebaseAllowed(in cardInputs, repoFull string, pr github.PullRequest) (allowed, known bool) {
	if in.stash == nil || !in.stash.botRebaseGated {
		return true, true
	}
	allowed, known = in.stash.botRebase[cardStashKey(repoFull, pr.Number)]
	return allowed, known
}

// cardGatePolicy is what writeback.Gate.Decide reads for
// KindBotRebaseRequest, copied once per publish: the gate's policy
// (Gate.Config), its per-repo first-seen and writable times
// (Gate.Snapshot), and the lookups cmd/hive installs on it
// (config.Config.WritebackGateDeps). Nothing here takes the gate's mutex or
// writes its ledger.
type cardGatePolicy struct {
	cfg      writeback.Config
	repos    map[string]writeback.RepoStatus
	now      time.Time
	levelFor func(repo string) int
	optIn    func(repo string) []string
	listed   func(repo string) bool
	paused   func(repo string) bool
}

// cardGatePolicyFor builds the copy from the gate's policy and repo
// records and the hive config's gate lookups (nil cfg: none, as a gate built
// without deps).
func cardGatePolicyFor(policy writeback.Config, repos map[string]writeback.RepoStatus, cfg *config.Config, now time.Time) cardGatePolicy {
	p := cardGatePolicy{cfg: policy, repos: repos, now: now}
	if cfg != nil {
		deps := cfg.WritebackGateDeps()
		p.levelFor, p.optIn, p.listed = deps.LevelFor, deps.OptInLabels, deps.RepoListed
		if deps.RepoPause != nil {
			p.paused = func(repo string) bool {
				_, paused := deps.RepoPause(repo)
				return paused
			}
		}
	}
	return p
}

// cardGateAllows copies writeback.Gate.decideLocked (pkg/writeback/gate.go)
// for KindBotRebaseRequest on a pull request, read-only: the operator
// pause, the per-repo mode, the hive's own items, the backfill guard (an
// unlabelled PR it would hold counts as not asked; the gate would also
// record it, the copy does not), mode full, the listed repos, the kind's
// policy and the ACMM level with the opt-in label rules. The branches for
// other kinds (issue files, stage labels, blocked repairs, parks, adoption)
// cannot apply to this kind. TestCardGateAllowsMatchesDecide pins the copy
// to Decide across the policy matrix.
func cardGateAllows(p cardGatePolicy, repo string, item writeback.Item) bool {
	const kind = writeback.KindBotRebaseRequest
	if p.paused != nil && p.paused(repo) {
		return false
	}
	mode := p.cfg.RepoModeFor(repo)
	switch {
	case mode == writeback.RepoModeOff, mode == writeback.RepoModeIssuesOnly:
		return false
	case mode == writeback.RepoModeLabelsOnly && !kind.IsLabelKind():
		return false
	}
	if item.HiveAuthored {
		return true
	}
	optedIn := cardGateOptedIn(p, repo, item.Labels)
	if !optedIn && cardGateBackfillHolds(p, repo, item) {
		return false
	}
	if mode == writeback.RepoModeFull {
		return true
	}
	if p.listed != nil && !p.listed(repo) {
		return false
	}
	policy := p.cfg.Policy(kind)
	if policy.OwnOnly {
		return false
	}
	level := 0
	if p.levelFor != nil {
		level = p.levelFor(repo)
	}
	if level <= 0 {
		return false
	}
	if !policy.RequireLabel || optedIn {
		return level >= policy.MinLevel
	}
	return policy.UnlabeledMinLevel > 0 && level >= policy.UnlabeledMinLevel
}

// cardGateOptedIn copies Gate.optedInLocked: the item carries one of the
// repo's opt-in labels (default writeback.DefaultOptInLabel).
func cardGateOptedIn(p cardGatePolicy, repo string, labels []string) bool {
	want := []string{writeback.DefaultOptInLabel}
	if p.optIn != nil {
		if ls := p.optIn(repo); len(ls) > 0 {
			want = ls
		}
	}
	for _, w := range want {
		if cardHasLabel(labels, strings.TrimSpace(w)) {
			return true
		}
	}
	return false
}

// cardGateBackfillHolds copies Gate.backfillHoldsLocked: backfill is off
// and the item was created before the repo's cutoff (the later of
// first_seen and writable_at). A repo the gate has not seen gets first_seen
// now, as recordLocked would stamp it.
func cardGateBackfillHolds(p cardGatePolicy, repo string, item writeback.Item) bool {
	if p.cfg.BackfillEnabled() || item.CreatedAt.IsZero() {
		return false
	}
	cut := p.now.UTC()
	if rec, ok := p.repos[strings.ToLower(strings.TrimSpace(repo))]; ok {
		cut = rec.FirstSeen
		if rec.WritableAt.After(cut) {
			cut = rec.WritableAt
		}
	}
	return !cut.IsZero() && item.CreatedAt.Before(cut)
}

// cardFixerExcluded copies stageTrackerApplyFixerStatus
// (stage_tracker_fixer.go) over review.FixDispatchStatusFor: a hive PR at
// 070 the fix dispatcher refuses to work and no agent is on. why is the
// dispatcher's reason in plain words.
func cardFixerExcluded(state review.DispatchState, workers map[string]stageTrackerWorker, repoFull string, pr github.PullRequest) (string, bool) {
	fixer := review.FixDispatchStatusFor(state, repoFull, pr.Number, pr.HeadSHA)
	if !fixer.Excluded() {
		return "", false
	}
	if _, busy := stageTrackerWorkerOn(workers, repoFull, pr.Number); busy {
		return "", false
	}
	why := review.FixDispatchExclusionText(fixer)
	if why == "" {
		return "", false
	}
	return why, true
}

// Waiting causes, in the order the your-turn decision tries its waiting
// returns. The helpers below only tell those returns apart; the stored
// decision already said the row waits.
const (
	cardWaitHold            = "hold"
	cardWaitCoveredByPR     = "covered"
	cardWaitHiveClosingPR   = "hive_pr_closes"
	cardWaitDecisionBlocker = "decision_blocker"
	cardWaitReporter        = "reporter"
	cardWaitClosingPR       = "closing_pr"
	cardWaitBlockedBy       = "blocked_by"
	cardWaitAdjudicator     = "adjudicator"
	cardWaitAnswered        = "answered"
)

// cardIssueWaitingCause copies the order of the waiting returns in
// decideIssueOwnership (ownership.go) and blockedIssueOwnership
// (ownership_gates.go): a person's hold or the dashboard pause, an open PR
// that covers the issue, a decision park a hive PR closes, a decision park
// another decision blocks, the reporter's confirmation, an issue-filter row
// a PR closes, and a blocked issue with an open blocked-by link. "" means
// none of them matches.
func cardIssueWaitingCause(row cardIssueRow, parking string) string {
	labels := labelSet(row.labels())
	if row.held && holdSourcesHide(row.holdSources) {
		return cardWaitHold
	}
	if row.bucket() == repoWorkBucketClaimedByPR || (!row.nonActionable && issueHasOpenLinkedPR(row.issue)) {
		return cardWaitCoveredByPR
	}
	if parking != "" {
		if parking == cardDecisionBucket && row.hiveClosingPR > 0 {
			return cardWaitHiveClosingPR
		}
		if parking == cardDecisionBucket {
			return cardWaitDecisionBlocker
		}
		return ""
	}
	if labels[ownershipLabelReporterConfirmation] || (row.bucket() == repoWorkBucketFiltered && row.bucketReason() == ownershipLabelReporterConfirmation) ||
		(row.reporterGate() && ownershipLikelyDone(labels)) {
		return cardWaitReporter
	}
	if row.bucket() == repoWorkBucketFiltered && row.bucketReason() == github.IssueFilterReason && row.closingPR > 0 {
		return cardWaitClosingPR
	}
	if labels[blockedWorkflowLabel] && len(row.openBlockers()) > 0 {
		return cardWaitBlockedBy
	}
	return ""
}

// cardPRWaitingCause copies the waiting returns of decidePROwnership
// (ownership.go) and reviewHoldOwnership (ownership_review_holds.go): a
// person's hold or the dashboard pause, a hive PR inside the adjudicator's
// needs-human wait, and otherwise a review a person already answered.
// reviewHoldOwnership returns early inside the adjudicator wait, so the last
// two never overlap.
func cardPRWaitingCause(pr github.PullRequest, held bool) string {
	if held && holdSourcesHide(pr.HoldSources) {
		return cardWaitHold
	}
	if labelSet(pr.Labels)[ownershipLabelNeedsHuman] && prAgentRole(pr) != "" {
		return cardWaitAdjudicator
	}
	return cardWaitAnswered
}

// cardLaneCanOpenPR copies laneCanOpenPRAt (ownership.go) over the agent
// rows of the snapshot instead of the live processes, which the card cannot
// read: the lane's agent is running in a mode that pushes, and its default
// mode at the repository's effective level pushes too. It is an
// approximation: the row's Mode is the configured mode, not the launched
// one, so an agent awaiting a restart (NeedsRestart) counts as not
// pushing; names match case-insensitively.
func cardLaneCanOpenPR(agents []FrontendAgent, level func(repo string) int) func(lane, repo string) bool {
	return func(lane, repo string) bool {
		name := strings.ToLower(strings.TrimSpace(lane))
		if name == "" {
			return false
		}
		for _, a := range agents {
			if !strings.EqualFold(strings.TrimSpace(a.Name), name) {
				continue
			}
			if a.State != string(crux.StateRunning) || a.NeedsRestart {
				return false
			}
			mode, ok := crux.ParseAgentMode(a.Mode)
			if !ok || !mode.CanPush() {
				return false
			}
			return crux.DefaultAgentMode(name, level(repo)).CanPush()
		}
		return false
	}
}

func cardHasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

// cardText makes borrowed reason text safe for the card: no figure, en, em
// or horizontal-bar dashes and no mentions. A spaced dash becomes "; ", any
// other dash a comma, and a login loses its leading at sign.
func cardText(s string) string {
	for _, dash := range []string{"\u2012", "\u2013", "\u2014", "\u2015"} {
		s = strings.ReplaceAll(s, " "+dash+" ", "; ")
		s = strings.ReplaceAll(s, dash, ",")
	}
	if !strings.ContainsRune(s, '@') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '@' && i+1 < len(s) && cardLoginByte(s[i+1]) && (i == 0 || !cardLoginByte(s[i-1])) {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func cardLoginByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
}
