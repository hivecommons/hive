package dashboard

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageTrackerModifierOrder is the modifier precedence the payload carries.
var stageTrackerModifierOrder = []string{modStalled, modRegressed, modHold, modUnauthorized, modNeedsHuman, modNeedsDecision, modNeedsTriage, modPossibleDupe, modNeedsRebase, modBlocked, modDraft}

// stageTrackerModifier is one health or control marker with its class.
type stageTrackerModifier struct {
	Name string
	// ActionableNow: a person must do something right now for the row to
	// move, whatever its stage (hold, draft, stalled, regressed, a step
	// the App may not take that blocks the current stage). False marks a
	// merge-gate marker — needs-human, needs-decision, a merge-time
	// unauthorized step — that only becomes actionable once the PR reaches
	// the gate band; before that the tracker holds it back.
	ActionableNow bool
}

// stageTrackerPillsDeferred reports whether a row's merge-gate markers are
// held back: a PR (or an issue riding its covering PR's stage) that has
// not reached the merge-gate band yet. The band starts at stage/080 —
// stagelabels.AwaitingMergeAdmission, the same boundary adoption yields
// at — and runs through 085/090/095/100. Issues on their own stages keep
// every pill: a needs-human on an unclaimed issue is a park the person
// owes now, not a merge-time notice.
func stageTrackerPillsDeferred(kind string, stageNumber int, mirrored bool) bool {
	if kind != "pr" && !mirrored {
		return false
	}
	return stageNumber < stagelabels.AwaitingMergeAdmission.Number()
}

// stageTrackerUnauthorizedActionableNow reports whether the step the App
// may not take blocks the row where it stands, so a person must act now:
// held workflow runs mean CI cannot even start, and waiting for the merge
// gate would waste the whole CI / review cycle. Any other refused step
// (merge, branch update, …), or a label whose reason no longer names the
// step, is a merge-time step and waits for the gate.
func stageTrackerUnauthorizedActionableNow(reason string) bool {
	base, _ := stagelabels.HumanApprovedReason(strings.TrimSpace(reason))
	rest, ok := strings.CutPrefix(base, stageengineprstage.StageReasonUnauthorizedPrefix)
	return ok && strings.TrimSpace(rest) == stageengineprstage.UnauthorizedStepApproveWorkflowRuns
}

// stageTrackerClassifyModifiers derives the row's markers from its labels,
// store record and draft flag, each with its class; reason is the lane's
// stage reason, read for the unauthorized step. Order is the pill priority.
func stageTrackerClassifyModifiers(labels []string, hiveID string, rec stagelabels.Record, hasRec bool, overrides map[string]time.Duration, now time.Time, draft bool, reason string) []stageTrackerModifier {
	set := map[string]bool{}
	for _, l := range labels {
		switch lower := strings.ToLower(strings.TrimSpace(l)); lower {
		case stagelabels.StalledLabel:
			set[modStalled] = true
		case stagelabels.RegressedLabel:
			set[modRegressed] = true
		case "needs-human":
			set[modNeedsHuman] = true
		case needsDecisionLabel, prNeedsDecisionLabel, "needs-decision":
			// The stage label is the park signal (hivecommons/hive#12683;
			// 045 on an issue, 085 on a PR); the retired spelling is read
			// until migrated.
			set[modNeedsDecision] = true
		case "needs-triage":
			// Still waiting on triage (reporter trust / maintainer look):
			// the Open row's action chip reads "triage needed".
			set[modNeedsTriage] = true
		case stagelabels.PossiblyExisting.Label():
			set[modPossibleDupe] = true
		case "needs-rebase":
			set[modNeedsRebase] = true
		case "blocked":
			set[modBlocked] = true
		case stagelabels.UnauthorizedLabel:
			set[modUnauthorized] = true
		case prowWIPLabel:
			// Prow's WIP label is the draft marker, present whether or not
			// the snapshot's draft bit made it this far.
			draft = true
		}
	}
	if len(slices.DeleteFunc(holdLabels(labels, hiveID), func(l string) bool {
		return strings.EqualFold(strings.TrimSpace(l), prowWIPLabel)
	})) > 0 {
		set[modHold] = true
	}
	if hasRec {
		if stagelabels.Stalled(rec, overrides, now) {
			set[modStalled] = true
		}
		if rec.Regressed() {
			set[modRegressed] = true
		}
	}
	if draft {
		set[modDraft] = true
	}
	out := make([]stageTrackerModifier, 0, len(set))
	for _, m := range stageTrackerModifierOrder {
		if !set[m] {
			continue
		}
		actionable := true
		switch m {
		case modNeedsHuman, modNeedsDecision:
			actionable = false
		case modUnauthorized:
			actionable = stageTrackerUnauthorizedActionableNow(reason)
		}
		out = append(out, stageTrackerModifier{Name: m, ActionableNow: actionable})
	}
	return out
}

// stageTrackerBlockedBy marks an open issue the scheduler is deliberately
// holding back for unresolved GitHub "blocked by" dependencies
// (hivecommons/hive#14765). The kick list already withholds such issues
// ("Blocked by open dependencies … do NOT start these"), but the tracker
// showed them as plain Actionable with reason `actionable`, so sequenced
// sub-issues of an RFC read as intake starvation. The row gets the same
// `blocked` modifier the `blocked` label yields, the blocker keys, and a
// caption clause naming what it waits on. Only the pre-claim stages
// qualify: once a claim or PR exists the sequencing no longer gates it.
func stageTrackerBlockedBy(it *StageTrackerItem, blockers []string) {
	if it == nil || len(blockers) == 0 || it.Stage > trackerNumberPossiblyExists {
		return
	}
	it.BlockedOn = append([]string(nil), blockers...)
	if !slices.Contains(it.Modifiers, modBlocked) {
		it.Modifiers = append(it.Modifiers, modBlocked)
		slices.SortFunc(it.Modifiers, func(a, b string) int {
			return slices.Index(stageTrackerModifierOrder, a) - slices.Index(stageTrackerModifierOrder, b)
		})
	}
	it.Caption = joinClause(it.Caption, "waiting on "+stageTrackerBlockerList(it.Repo, blockers))
	it.Actor = "its blockers"
}

// StageTrackerBlockedOnPR is the pull request a blocked issue waits on.
type StageTrackerBlockedOnPR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	// State is "open", "merged" or "closed" when a cache knows it, else "".
	State string `json:"state,omitempty"`
}

// Blocked-on PR states as the caches report them.
const (
	blockedOnPRStateOpen   = "open"
	blockedOnPRStateMerged = "merged"
	blockedOnPRStateClosed = "closed"
)

// stageTrackerBlockedOnPR fills the row's BlockedOnPR from the issue's
// marker (or reason) and rewrites the default caption to name the blocker:
// "Blocked on PR #N" (same repo) / "Blocked on PR owner/repo#N", with what
// a merged or closed-unmerged blocker means for the row. It never adds the
// `blocked` modifier: the label does that (finish), and an issue whose
// label was lifted but whose body still carries the marker is not blocked.
func stageTrackerBlockedOnPR(it *StageTrackerItem, body, reason string, baseURL string, state func(repo string, number int) string) {
	if it == nil || !slices.Contains(it.Modifiers, modBlocked) {
		return
	}
	repo, number, ok := github.BlockedOnTarget(body, reason)
	if !ok {
		return
	}
	ref := &StageTrackerBlockedOnPR{Repo: repo, Number: number, URL: overviewItemURL("", repo, number, "pr", baseURL)}
	if state != nil {
		ref.State = state(repo, number)
	}
	it.BlockedOnPR = ref
	short := stageTrackerBlockerList(it.Repo, []string{repo + "#" + strconv.Itoa(number)})
	clause := "Blocked on PR " + short
	switch ref.State {
	case blockedOnPRStateMerged:
		clause += " — merged; the blocked sweep lifts the label next"
	case blockedOnPRStateClosed:
		clause += " — closed unmerged; the blocked sweep re-triages it next"
	default:
		clause += " — lifts when it merges"
	}
	if it.Stage == 0 {
		it.Caption = clause
		it.Actor = "PR " + short
		return
	}
	it.Caption = joinClause(it.Caption, clause)
}

// stageTrackerPRStateResolver answers a blocked-on PR's state from what the
// hive already holds, cheapest first: the status snapshot's open PRs, the
// forgestate cache (when attached), the recent-merge audit ring. Unknown
// is "" — never a GitHub call from the tracker build.
func (s *Server) stageTrackerPRStateResolver(status *StatusPayload, merged []StageTrackerMerged) func(repo string, number int) string {
	var open map[string]bool
	openKey := func(repo string, number int) string { return strings.ToLower(repo) + "#" + strconv.Itoa(number) }
	return func(repo string, number int) string {
		if open == nil {
			open = map[string]bool{}
			if status != nil {
				for _, r := range status.Repos {
					name := overviewRepoName(r)
					for _, it := range repoOpenItems(r) {
						if it.Kind == "pr" {
							open[openKey(name, it.Number)] = true
						}
					}
				}
			}
		}
		if open[openKey(repo, number)] {
			return blockedOnPRStateOpen
		}
		if s != nil && s.deps != nil && s.deps.GHClient != nil {
			if fs := s.deps.GHClient.ForgeState(); fs != nil {
				if item, ok := fs.PR(repo, number); ok {
					switch {
					case item.Merged:
						return blockedOnPRStateMerged
					case item.Closed || strings.EqualFold(item.State, "closed"):
						return blockedOnPRStateClosed
					case strings.EqualFold(item.State, "open"):
						return blockedOnPRStateOpen
					}
				}
			}
		}
		for _, m := range merged {
			if m.Number == number && strings.EqualFold(m.Repo, repo) {
				return blockedOnPRStateMerged
			}
		}
		return ""
	}
}

// stageTrackerBlockerList renders blocker keys ("repo#N") for a caption,
// shortening a same-repo blocker to "#N".
func stageTrackerBlockerList(repo string, blockers []string) string {
	parts := make([]string, 0, len(blockers))
	for _, b := range blockers {
		b = strings.TrimSpace(b)
		if r, n, ok := strings.Cut(b, "#"); ok && (r == "" || strings.EqualFold(r, repo) || strings.HasSuffix(strings.ToLower(repo), "/"+strings.ToLower(r))) {
			b = "#" + n
		}
		parts = append(parts, b)
	}
	return strings.Join(parts, ", ")
}

// stageTrackerModifiers splits a row's markers into the pills it draws
// (shown) and the merge-gate pills it holds back (deferred) until the PR
// reaches the gate band. Everything actionable now is always shown; a
// merge-gate marker is shown once stageTrackerPillsDeferred says the row
// is at or past the gate, or the row is an issue on its own stage.
func stageTrackerModifiers(labels []string, hiveID string, rec stagelabels.Record, hasRec bool, overrides map[string]time.Duration, now time.Time, draft bool, reason string, kind string, stage stagelabels.Stage, mirrored bool) (shown, deferred []string) {
	held := stageTrackerPillsDeferred(kind, stage.Number(), mirrored)
	for _, m := range stageTrackerClassifyModifiers(labels, hiveID, rec, hasRec, overrides, now, draft, reason) {
		if m.ActionableNow || !held {
			shown = append(shown, m.Name)
		} else {
			deferred = append(deferred, m.Name)
		}
	}
	if shown == nil {
		shown = []string{}
	}
	return shown, deferred
}

// noVerdictClause is the bare no-verdict clause; stageTrackerCaption swaps
// in "awaiting review of head <sha7>" when the item's head is known.
const noVerdictClause = "no review verdict yet"

// shortHeadLen is the abbreviated SHA length the captions print.
const shortHeadLen = 7

func shortHead(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > shortHeadLen {
		return sha[:shortHeadLen]
	}
	return sha
}

// humanizeStageReason turns a lane reason token into a short clause with
// no leading capital and no trailing period, or "" when the token adds
// nothing to the stage description.
func humanizeStageReason(reason string) string {
	r := strings.TrimSpace(reason)
	if r == "" {
		return ""
	}
	if base, approver := stagelabels.HumanApprovedReason(r); approver != "" {
		clause := humanizeStageReason(base)
		if clause == "" {
			return "approved by @" + approver
		}
		return clause + " — approved by @" + approver
	}
	lower := strings.ToLower(r)
	if front, ok := strings.CutPrefix(r, "base-refresh:queued-behind#"); ok {
		if front == "0" {
			return "waiting for admission to final base preparation"
		}
		return "queued for base refresh behind PR #" + front
	}
	if r == "base-refresh:preparing" {
		return "preparing the current base; current-head CI and review must complete"
	}
	switch lower {
	case "actionable", "live agent claim", "live issue claim", "open pr covers it":
		return ""
	}
	if stageengine.ReasonIsPartialCover(r) {
		return "the PR references it without closing it (partial)"
	}
	if clause, ok := humanizeCoverReason(r); ok {
		return clause
	}
	if clause, ok := stagelabels.AlreadyLandedCaption(r); ok {
		return clause
	}
	switch lower {
	case stagelabels.LaneReasonAdoptionChildClosedTwice:
		return "the hive's copies of this PR were closed without merging twice within an hour"
	case stagelabels.LaneReasonAdoptionCopyFailed:
		return "the hive's copy of this PR failed and this PR was reopened"
	case "held":
		return "held by a hold label"
	case "label:needs-rebase":
		return "needs a rebase"
	case "untrusted-author-role":
		return "author's role is not trusted for unattended merge"
	case "contributor-lane:no-verdict":
		return noVerdictClause
	case "contributor-lane:no-claim":
		return "no verified claim links this PR to an issue"
	case "contributor-lane:claim-tier-untrusted":
		return "author's claim tier is below the auto-merge tiers"
	case "ci-red":
		return "CI failed"
	case "check-runs", "status-check":
		return "CI checks could not be read"
	}
	if words, ok := stageTrackerCIStateWords(r); ok {
		return words
	}
	if strings.HasPrefix(lower, "mirrors pr #") {
		return "mirrors PR " + r[len("mirrors PR "):]
	}
	if rest, ok := strings.CutPrefix(r, "required-check-pending:"); ok {
		return "check " + rest + " still running"
	}
	if rest, ok := strings.CutPrefix(r, "required-check-failing:"); ok {
		return "check " + rest + " failing"
	}
	if rest, ok := strings.CutPrefix(r, "optional-check-failing:"); ok {
		return "optional check " + rest + " failing"
	}
	if rest, ok := strings.CutPrefix(r, "base-branch-red:"); ok {
		return "waiting for the base branch's CI to recover (" + strings.ReplaceAll(rest, ",", ", ") + ") — not this PR's fault"
	}
	if rest, ok := strings.CutPrefix(r, stageengineprstage.StageReasonUnauthorizedPrefix); ok {
		return "the hive's GitHub App is not permitted to " + unauthorizedStepWords(rest) + "; a maintainer must"
	}
	if rest, ok := strings.CutPrefix(r, "fork-guard:"); ok {
		return "check " + strings.ReplaceAll(rest, ",", ", ") + " refuses fork PRs — a maintainer must push this branch to the repository"
	}
	if rest, ok := strings.CutPrefix(r, "shared-ci-incident:"); ok {
		return "deferred to shared-CI incident #" + rest + " — not this PR's fault"
	}
	if lower == stageengineprstage.AutoMergeReasonHumanChangesRequested {
		return "a maintainer requested changes"
	}
	if rest, ok := strings.CutPrefix(lower, "reviewer-verdict:"); ok {
		switch rest {
		case "changes_requested", "request_changes":
			return "review swarm requested changes"
		case "reject", "rejected":
			return "review swarm rejected the head"
		}
		return "review verdict " + strings.ReplaceAll(rest, "_", " ")
	}
	if rest, ok := strings.CutPrefix(r, "excluded-label:"); ok {
		return "label " + rest + " excludes it"
	}
	if rest, ok := strings.CutPrefix(r, "label:"); ok {
		return "label " + rest
	}
	if rest, ok := strings.CutPrefix(lower, "pending: "); ok {
		if i := strings.Index(rest, " (< min_head_age)"); i >= 0 {
			return rest[:i] + "; waiting for the minimum head age"
		}
		return rest
	}
	if strings.HasPrefix(lower, "contributor-lane:soaking") {
		if left, ok := soakRemainingFromReason(r); ok {
			return "soaking, " + left.Round(time.Minute).String() + " left"
		}
		return "soaking"
	}
	if strings.HasPrefix(lower, "release-hold") {
		return "release hold"
	}
	switch lower {
	case stageengineprstage.ContributorLaneReasonWorkflowPath:
		return "workflow path"
	case stageengineprstage.ContributorLaneReasonTestsRemoved:
		return "tests removed"
	case stageengineprstage.AutoMergeReasonHumanMergePath:
		return "human-merge path"
	case stageengineprstage.AutoMergeReasonHumanMergeRequired:
		return "human merge"
	case stageengineprstage.AutoMergeReasonForkNotEditable:
		// The App cannot update a fork the author did not open to
		// maintainer edits; only adoption (hive re-authoring on its own
		// branch) moves it (hivecommons/hive#15312).
		return "fork not editable by hive"
	}
	if rest, ok := strings.CutPrefix(lower, "contributor-lane:"); ok {
		return strings.ReplaceAll(strings.ReplaceAll(rest, "-", " "), "_", " ")
	}
	return stageTrackerPlainStages(strings.NewReplacer("-", " ", "_", " ", ":", ": ").Replace(stageTrackerPlainStages(r)))
}

// unauthorizedStepWords renders an `unauthorized:<step>` step as words.
func unauthorizedStepWords(step string) string {
	return strings.ReplaceAll(strings.TrimSpace(step), "-", " ")
}

// Tooltip for the unauthorized pill, per step; the generic form serves a
// label whose stage reason does not (or no longer) names the step.
const (
	stageTrackerUnauthorizedApproveRuns = "The hive's GitHub App lacks `actions: write`, so it cannot approve the workflow runs GitHub is holding for this head. A maintainer must click Approve and run on the PR's checks banner (or the Actions tab); see the hive:unauthorized comment for the run ids. Hive resumes, and clears the label, once the required checks report."
	stageTrackerUnauthorizedGeneric     = "The hive's GitHub App lacks a permission for the next step, so only a person can take it; the hive:unauthorized comment on the PR names the permission and the step. Hive resumes, and clears the label, once the step is done."
	stageTrackerUnauthorizedMergeWF     = "The hive's GitHub App lacks `workflows: write`, so it cannot merge this owner-approved PR that edits .github/workflows files. A maintainer must click Merge on GitHub (the approval already stands), or grant the App Workflows: Read and write so the hive can merge such PRs itself."
)

// stageTrackerUnauthorized derives the gate word and the tooltip for an
// item wearing hive:unauthorized from its stage reason.
func stageTrackerUnauthorized(reason string) (step, tip string) {
	base, _ := stagelabels.HumanApprovedReason(strings.TrimSpace(reason))
	rest, ok := strings.CutPrefix(base, stageengineprstage.StageReasonUnauthorizedPrefix)
	if !ok || strings.TrimSpace(rest) == "" {
		return "", stageTrackerUnauthorizedGeneric
	}
	step = unauthorizedStepWords(rest)
	if rest == stageengineprstage.UnauthorizedStepApproveWorkflowRuns {
		return step, stageTrackerUnauthorizedApproveRuns
	}
	if rest == stageengineprstage.UnauthorizedStepMergeWorkflowPath {
		return step, stageTrackerUnauthorizedMergeWF
	}
	return step, "The hive's GitHub App lacks the permission to " + step + ", so only a person can take that step; the hive:unauthorized comment on the PR names the permission and the action. Hive resumes, and clears the label, once the step is done."
}

// stageTrackerManualMergeReason reports whether a bare stage reason is a
// lane rule the hive never merges unattended, so a 090 under it is waiting
// on a maintainer's merge click rather than on /lgtm /approve.
func stageTrackerManualMergeReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case stageengineprstage.ContributorLaneReasonWorkflowPath, stageengineprstage.ContributorLaneReasonTestsRemoved, stageengineprstage.AutoMergeReasonHumanMergePath,
		stageengineprstage.AutoMergeReasonHumanMergeRequired:
		return true
	}
	return false
}

// Gate-cell words and tooltips per prstage.HumanMergeReason* code: why a
// person, not the hive, merges a PR parked at 090 (hivecommons/hive#14658).
// Plain words, one short sentence each, no config keys (hivecommons/hive#15394).
var stageTrackerHumanMergeReasonWords = map[string][2]string{
	stageengineprstage.HumanMergeReasonWorkflowFiles:     {"workflow files", "It changes GitHub workflow files, which hive never merges on its own"},
	stageengineprstage.HumanMergeReasonTestsRemoved:      {"tests removed", "It removes tests, which hive never merges on its own"},
	stageengineprstage.HumanMergeReasonManualMergePolicy: {"manual-merge policy", "It touches files this repo reserves for a person to merge"},
	stageengineprstage.HumanMergeReasonBranchProtection:  {"branch protection", "Branch protection wants a review hive cannot give; approve on GitHub or merge it"},
	stageengineprstage.HumanMergeReasonMergeMethod:       {"merge method", "The repo does not allow the merge method this PR needs"},
	stageengineprstage.HumanMergeReasonAutoMergeOff:      {"auto-merge off", "Auto-merge is off for this repo, so a person merges"},
	stageengineprstage.HumanMergeReasonUntrustedAuthor:   {"untrusted author", "Hive won't merge it alone — the author isn't a trusted author yet"},
	stageengineprstage.HumanMergeReasonUnauthorized:      {"unauthorized", "GitHub does not let hive merge here; a person merges"},
	stageengineprstage.HumanMergeReasonReviewerDeferred:  {"reviewer deferred", "A reviewer asked for a person to merge this one"},
	stageengineprstage.HumanMergeReasonBreakingChange:    {"human merge", "Breaking change — owner merges; hive never merges a ⚠️-titled or needs-owner-merge PR"},
}

// stageTrackerHumanMergeReason derives, from a 090 stage reason, the
// compact code, the gate-cell word and the tooltip saying why a person
// must merge. An unrecognised lane reason falls back to the "policy" code
// with the raw reason in the tooltip; an empty reason yields "".
func stageTrackerHumanMergeReason(reason string) (code, word, tip string) {
	code = stageengineprstage.HumanMergeReason(reason)
	if code == "" {
		return "", "", ""
	}
	if w, ok := stageTrackerHumanMergeReasonWords[code]; ok {
		return code, w[0], w[1]
	}
	base, _ := stagelabels.HumanApprovedReason(reason)
	return stageengineprstage.HumanMergeReasonPolicy, "policy", "Hive's merge rules leave this one to a person (`" + strings.TrimSpace(base) + "`)"
}

// Where an operator makes an author trusted, in the words the ⚙ and 👥
// sections use. One line each, no config keys: the keys live on those
// pages (hivecommons/hive#15394).
const (
	stageTrackerTrustFixTier   = "give them the trusted tier under 👥 Contributors"
	stageTrackerTrustFixRole   = "add them as a merger under ⚙ Security → Authorized users"
	stageTrackerTrustFixCollab = "make them a collaborator on GitHub"
)

// stageTrackerUntrustedAuthorWhy says, in one short plain sentence, why
// the hive will not merge the PR's author's work under a trust-family 090
// reason (hivecommons/hive#15078, #15394). "" for any other reason. The
// how-to-fix line is stageTrackerUntrustedAuthorFix.
func stageTrackerUntrustedAuthorWhy(base, author string) string {
	who := "the author"
	if a := strings.TrimSpace(author); a != "" {
		who = "@" + a
	}
	switch strings.ToLower(strings.TrimSpace(base)) {
	case stageengineprstage.ContributorLaneReasonAuthorAssociation, "untrusted-author-role", stageengineprstage.ContributorLaneReasonClaimTier,
		stageengineprstage.ContributorLaneReasonNoClaim, stageengineprstage.ContributorLaneReasonNoLinkedIssue:
		return who + " isn't a trusted author yet"
	case "author-permission-missing":
		return who + " has no write access to this repo"
	case "fork-non-member":
		return "it comes from a fork and " + who + " isn't a member of the org"
	case "untrusted-merger":
		return "the person who queued it isn't a merger"
	case stageengineprstage.ContributorLaneReasonClaimUnverified:
		return who + " hasn't verified their GitHub login yet"
	case stageengineprstage.ContributorLaneReasonClaimAuthor:
		return "the linked issue was claimed by someone other than " + who
	case stageengineprstage.ContributorLaneReasonClaimOtherHive:
		return "another hive owns the linked issue"
	}
	return ""
}

// stageTrackerUntrustedAuthorFix is the one-line "how to trust them" for
// the Details fold under a trust-family 090 reason; "" when none applies.
func stageTrackerUntrustedAuthorFix(base, author string) string {
	who := "the author"
	if a := strings.TrimSpace(author); a != "" {
		who = "@" + a
	}
	switch strings.ToLower(strings.TrimSpace(base)) {
	case stageengineprstage.ContributorLaneReasonAuthorAssociation:
		return "To trust " + who + ": " + stageTrackerTrustFixTier + ", or " + stageTrackerTrustFixCollab + "."
	case "untrusted-author-role":
		return "To trust " + who + ": " + stageTrackerTrustFixRole + "."
	case "author-permission-missing":
		return "To trust " + who + ": grant them write access on GitHub."
	case "fork-non-member":
		return "To trust " + who + ": " + stageTrackerTrustFixCollab + "."
	case "untrusted-merger":
		return "A merger or owner queues or merges it."
	case stageengineprstage.ContributorLaneReasonClaimTier, stageengineprstage.ContributorLaneReasonNoClaim, stageengineprstage.ContributorLaneReasonNoLinkedIssue:
		return "To trust " + who + ": " + stageTrackerTrustFixTier + "."
	case stageengineprstage.ContributorLaneReasonClaimUnverified:
		return "They verify it under Contributors → Verify GitHub, or " + stageTrackerTrustFixTier + "."
	case stageengineprstage.ContributorLaneReasonClaimAuthor:
		return "A maintainer merges, or " + stageTrackerTrustFixTier + "."
	case stageengineprstage.ContributorLaneReasonClaimOtherHive:
		return "The owning hive merges it, or a maintainer merges here."
	}
	return ""
}

// trackerSoakRemainingRE pulls the time left out of the
// `contributor-lane:soaking(remaining=12m0s)` reason the lane wrote before
// the soak deadline lived on the record (hivecommons/hive#15729). Records
// of that shape may still be on disk; nothing writes it any more.
var trackerSoakRemainingRE = regexp.MustCompile(`remaining=([0-9hms.]+)`)

// soakRemainingFromReason parses the soak time left from a soaking reason
// of the pre-deadline shape; false for the bare `contributor-lane:soaking`.
func soakRemainingFromReason(reason string) (time.Duration, bool) {
	m := trackerSoakRemainingRE.FindStringSubmatch(reason)
	if m == nil {
		return 0, false
	}
	d, err := time.ParseDuration(m[1])
	if err != nil || d < 0 {
		return 0, false
	}
	return d, true
}

// stageTrackerSoakCaption is the 095 sentence for a record carrying the
// lane's soak clock: "soaking — 12m left" while the window runs, computed
// at request time from the record's deadline so a soak that has run out
// never says "0s left"; past it, "soak complete — merges on the next
// pass". "" when the record is not at 095 or carries no clock (the
// generic caption stands). The words move each minute, but the note's
// age (NoteChangedAt) does not: the SPA refreshes them in place.
func stageTrackerSoakCaption(rec stagelabels.Record, now time.Time) string {
	const admitted = "Admitted by the automerge lane; "
	if rec.Stage != stagelabels.PreMergeSoak {
		return ""
	}
	until, ok := stageTrackerSoakUntil(rec)
	if !ok {
		return ""
	}
	if left := until.Sub(now); left > 0 {
		return admitted + "soaking — " + shortDuration(left) + " left"
	}
	return admitted + "soak complete — merges on the next pass"
}

// stageTrackerSoakUntil is the soak deadline a 095 record carries: the
// recorded soak's start plus its window (NoteSoak), else — for a record
// written before the deadline existed — the lane's `soaking(remaining=…)`
// reason aged from the sweep that wrote it. ok is false when the record
// is not at 095 or carries no clock.
func stageTrackerSoakUntil(rec stagelabels.Record) (time.Time, bool) {
	if rec.Stage != stagelabels.PreMergeSoak {
		return time.Time{}, false
	}
	if _, startedAt, window, ok := rec.Soak(); ok {
		return startedAt.Add(window), true
	}
	left, ok := soakRemainingFromReason(rec.Reason)
	if !ok || rec.LastSeen.IsZero() {
		return time.Time{}, false
	}
	return rec.LastSeen.Add(left), true
}

// stageTrackerSoakRemaining is the soak time left now for a record at 095,
// 0 once the deadline has passed or when the record carries no clock.
func stageTrackerSoakRemaining(rec stagelabels.Record, now time.Time) time.Duration {
	until, ok := stageTrackerSoakUntil(rec)
	if !ok {
		return 0
	}
	if left := until.Sub(now); left > 0 {
		return left
	}
	return 0
}

// stageTrackerNoteChangedAt is when the row's hive note last changed
// meaning: the record's reason-class change (or stage entry), or — for a
// 095 row whose soak has run out — the deadline, since "soak complete"
// is a different note from "soaking". Never later than now.
func stageTrackerNoteChangedAt(rec stagelabels.Record, now time.Time) time.Time {
	at := rec.NoteChangedAt()
	if until, ok := stageTrackerSoakUntil(rec); ok && !until.After(now) && until.After(at) {
		at = until
	}
	if at.After(now) {
		at = now
	}
	return at
}

// adoptedWorkCaption names what the hive is doing on a PR it adopted in
// place (stage 074) from the lane's reason token: the 074 label stays
// whether the head is conflicted, red or awaiting rework, so the caption is
// where "hive rebasing" vs "hive fixing CI" shows. Unknown reasons fall back
// to the bare ownership sentence.
func adoptedWorkCaption(reason string) string {
	const owned = "Hive adopted this PR"
	lower := strings.ToLower(strings.TrimSpace(reason))
	switch {
	case lower == "":
		return owned
	case lower == "conflicting" || lower == "not-mergeable" || strings.Contains(lower, "rebase"):
		return owned + "; hive is rebasing it onto the base branch"
	case strings.HasPrefix(lower, "check-failing:") || strings.HasPrefix(lower, "required-check-failing:") ||
		strings.HasPrefix(lower, "optional-check-failing:") || lower == "ci-red" || lower == "check-runs":
		return owned + "; hive is fixing CI"
	case strings.HasPrefix(lower, "reviewer-verdict:") || strings.Contains(lower, "changes-requested") || strings.Contains(lower, "changes_requested"):
		return owned + "; hive is addressing the review findings"
	}
	return owned
}

// stageTrackerReturnedToHiveCaption rewrites a 072 caption for a
// clanker-authored PR so it names whose clanker the PR went back to and
// who sent it: "Returned to @thatguywob's clanker — @clubanderson
// requested changes" (hivecommons/hive#15159). The human has already acted
// and nothing is owed by a maintainer, so the "@x must act" tail goes; the
// contributor's clanker is the actor. author is the person the clanker
// acts for (the PR's GitHub author), never the hive App's bot login.
func stageTrackerReturnedToHiveCaption(caption, author string, requesters []string) string {
	who := strings.TrimSpace(author)
	if who == "" {
		return caption
	}
	who = "@" + strings.TrimPrefix(who, "@")
	head := "Returned to " + who
	if !strings.HasPrefix(caption, head) {
		return caption
	}
	rest := strings.TrimPrefix(caption, head)
	rest = strings.Replace(rest, " — "+who+" must act", "", 1)
	if by := mentionLogins(requesters); by != "" {
		rest = strings.Replace(rest, " — a maintainer requested changes", " — "+by+" requested changes", 1)
	}
	return head + "'s clanker" + rest
}

// mentionLogins is "@a", "@a and @b" or "@a, @b and @c" for the given
// logins; "" when there are none.
func mentionLogins(logins []string) string {
	out := make([]string, 0, len(logins))
	for _, l := range logins {
		if l = strings.TrimPrefix(strings.TrimSpace(l), "@"); l != "" {
			out = append(out, "@"+l)
		}
	}
	switch len(out) {
	case 0:
		return ""
	case 1:
		return out[0]
	}
	return strings.Join(out[:len(out)-1], ", ") + " and " + out[len(out)-1]
}

// stageTrackerTriageNeededCaption is the caption of every reporter-trust
// held issue — 015 and a 000-new hold alike. The chevron already reads
// "Triage needed", so the caption carries only the maintainer's two moves.
const stageTrackerTriageNeededCaption = "Triage needed — accept or close"

func joinClause(head, clause string) string {
	if clause == "" {
		return head
	}
	return head + " — " + clause
}

// stageTrackerCaption builds the human sentence and the actor for an item.
func stageTrackerCaption(stage stagelabels.Stage, reason, author string, kind string, mirrored bool, rec stagelabels.Record, hasRec bool, linkedPR int, head string) (caption, actor string) {
	clause := humanizeStageReason(reason)
	if clause == noVerdictClause {
		if sha := shortHead(head); sha != "" {
			clause = "awaiting review of head " + sha
		}
	}
	who := strings.TrimSpace(author)
	if who == "" {
		who = "the author"
	} else {
		who = "@" + who
	}
	prRef := ""
	if linkedPR > 0 {
		prRef = " #" + strconv.Itoa(linkedPR)
	}
	if kind == "issue" && mirrored {
		return "Covered by PR" + prRef + " — " + strings.ToLower(stageTrackerShort(stage)) + " on the PR side", "its PR"
	}
	switch stage {
	case stagelabels.Actionable:
		return joinClause("Triaged and eligible; waiting for the scanner to pick it up", clause), "scanner"
	case stagelabels.TriageNeeded:
		// The lane reason ("triage needed: carries needs-triage") would say
		// triage a third time after the chevron word; the caption is the ask.
		return stageTrackerTriageNeededCaption, "maintainer"
	case stagelabels.PossiblyExisting:
		return joinClause("Prior art found; the author must confirm the gap (`/hive decision proceed — <gap>`) or close it (`/hive decision exists`)", clause), strings.TrimPrefix(who, "@")
	case stagelabels.Claimed:
		return joinClause("Claimed; work is about to start", clause), "claimant"
	case stagelabels.PRInProgress:
		return joinClause("An agent has a PR under way; not yet opened", clause), "hive"
	case stagelabels.CoveredByPR:
		if stageengine.ReasonIsPartialCover(reason) {
			return "An open PR" + prRef + " addresses part of this issue — it references it without a closing keyword, so the remainder stays open when it merges", "its PR"
		}
		return joinClause("An open PR"+prRef+" covers this issue", clause), "its PR"
	case stagelabels.ReviewPending:
		return joinClause("Review swarm requested; waiting for a verdict on the current head", clause), "review swarm"
	case stagelabels.Reviewed:
		return joinClause("Review passed; waiting for merge admission", clause), "CI"
	case stagelabels.Fixing:
		return joinClause("Red or changes requested; a hive agent is fixing it", clause), "hive"
	case stagelabels.NeedsAuthor:
		return joinClause("Needs "+who, clause) + " — " + who + " must act", strings.TrimPrefix(who, "@")
	case stagelabels.ReturnedToContributor:
		c := joinClause("Returned to "+who, clause) + " — " + who + " must act"
		if hasRec && rec.NudgeCount > 0 {
			c += " (" + plural(rec.NudgeCount, "nudge") + " sent)"
		}
		return c, strings.TrimPrefix(who, "@")
	case stagelabels.AdoptedByHive:
		c := adoptedWorkCaption(reason)
		if hasRec && rec.AdoptedBy > 0 {
			switch stageTrackerAdoptedByState(rec) {
			case stageTrackerAdoptedClosed:
				return joinClause("The hive's copy #"+strconv.Itoa(rec.AdoptedBy)+" was closed without merging; the hive is opening a fresh copy from the same head", clause), "hive"
			case stageTrackerAdoptedResolving:
				return joinClause("Adopting: the hive's copy #"+strconv.Itoa(rec.AdoptedBy)+" conflicts with the base and the fixer is resolving the conflicts on it", clause), "hive"
			}
			c += " (superseded by #" + strconv.Itoa(rec.AdoptedBy) + ")"
		}
		return joinClause(c, clause), "hive"
	case stagelabels.AwaitingMergeAdmission:
		if c := stageTrackerMergeTrainCaption(reason); c != "" {
			return c, "automerge lane"
		}
		return joinClause(stageTrackerAwaitingAdmissionCaption, clause), "automerge lane"
	case stagelabels.NeedsDecision, stagelabels.IssueNeedsDecision:
		switch stageTrackerAdoptedByState(rec) {
		case stageTrackerAdoptedClosedTwice:
			return stageTrackerAdoptionLoopCaption(rec), "maintainer"
		case stageTrackerAdoptedCopyFailed:
			return "The hive's copy #" + strconv.Itoa(rec.AdoptedBy) + " failed; this PR was reopened and needs a person", "maintainer"
		case stageTrackerAdoptedResolving:
			return "Rebase failed twice: the hive's copy #" + strconv.Itoa(rec.AdoptedBy) + " still conflicts with the base after two fixer attempts; a person decides", "maintainer"
		}
		if strings.TrimSpace(strings.ToLower(reason)) == stagelabels.LaneReasonRebaseFailedTwice {
			return "Rebase failed twice: the fixer could not resolve the conflicts with the base; a person decides", "maintainer"
		}
		if step, _ := stageTrackerUnauthorized(reason); step != "" {
			// hivecommons/hive#14589: not a decision but a step only a
			// person may take; the gate cell says the same.
			return joinClause("Awaiting human: "+step, clause), "maintainer"
		}
		if c := stageTrackerMergeTrainCaption(reason); c != "" {
			return c, "maintainer"
		}
		return joinClause("A human decision is owed", clause), "maintainer"
	case stagelabels.ReadyForHumanMerge:
		if base, approver := stagelabels.HumanApprovedReason(reason); stageTrackerManualMergeReason(base) {
			if strings.EqualFold(strings.TrimSpace(base), stageengineprstage.AutoMergeReasonHumanMergeRequired) {
				// The breaking-change rule (hivecommons/hive#15442): no
				// approval releases it; the owner's merge click does.
				c := "Ready for human merge (" + stageengineprstage.CauseHumanMergeRequired + ")"
				if approver != "" {
					c += " — approved by @" + approver
				}
				return c, "maintainer"
			}
			c := "Ready to merge (" + humanizeStageReason(base) + ")"
			if approver != "" {
				c += " — approved by @" + approver
			}
			return c + " — a person merges it", "maintainer"
		}
		return joinClause("Ready to merge — a maintainer merges it", clause), "maintainer"
	case stagelabels.PreMergeSoak:
		return joinClause("Admitted by the automerge lane; the pre-merge soak window is running", clause), "automerge lane"
	case stagelabels.MergeQueue:
		return joinClause("In the merge queue; soak complete, waiting for tide or the merge window", clause), "tide"
	}
	return joinClause("No stage recorded yet", clause), ""
}

// stageTrackerReviewWaitClause explains a review-pending head the swarm has
// not finished: how many perspectives have reported, and, when the planner
// has been passing the head over for lack of a reviewer, for how many
// cycles and since when (hivecommons/hive#15229). "" when neither is known.
func stageTrackerReviewWaitClause(agg *review.Aggregate, state review.DispatchState, fullRepo, repo string, number int, head string, now time.Time) string {
	var parts []string
	if agg != nil && agg.Verdict == review.VerdictIncomplete {
		coverage := fmt.Sprintf("%s reported so far", plural(len(agg.Perspectives), "perspective"))
		for _, r := range agg.Reasons {
			if strings.HasPrefix(r, "review coverage incomplete") {
				coverage = r
				break
			}
		}
		parts = append(parts, coverage)
	}
	wait, ok := state.WaitFor(fullRepo, number, head)
	if !ok && fullRepo != repo {
		wait, ok = state.WaitFor(repo, number, head)
	}
	if ok {
		w := fmt.Sprintf("waiting for a free reviewer for %s", plural(wait.Cycles, "cycle"))
		if !wait.FirstAt.IsZero() {
			w += " since " + wait.FirstAt.UTC().Format("15:04") + " UTC"
		}
		if wait.Cycles >= review.ReviewWaitStarvationCycles {
			w += "; promoted ahead of the queue"
		}
		parts = append(parts, w)
	}
	return strings.Join(parts, "; ")
}

// stageTrackerRegression fills the item's regression narration from the
// record: the hop that opened the regression when History still has it,
// else the record's fall time and current stage.
func stageTrackerRegression(it *StageTrackerItem, rec stagelabels.Record) {
	if !rec.Regressed() {
		return
	}
	it.RegressedFrom = rec.RegressedFrom.Number()
	if hop, ok := rec.RegressionHop(); ok {
		it.RegressedTo = hop.Stage.Number()
		it.RegressedReason = hop.Reason
		at := hop.At
		it.RegressedAt = &at
		return
	}
	it.RegressedTo = rec.Stage.Number()
	it.RegressedReason = rec.Reason
	if !rec.RegressedAt.IsZero() {
		at := rec.RegressedAt
		it.RegressedAt = &at
	}
}

func stageTrackerHistory(rec stagelabels.Record) []StageTrackerHop {
	out := make([]StageTrackerHop, 0, len(rec.History))
	for _, h := range rec.History {
		out = append(out, StageTrackerHop{Number: h.Stage.Number(), Slug: h.Stage.Slug(), At: h.At, Reason: h.Reason, From: h.From.Number(), Regressed: h.Regressed})
	}
	if len(out) > stageTrackerHistoryMaxItems {
		out = out[len(out)-stageTrackerHistoryMaxItems:]
	}
	return out
}

// Enumeration buckets (pkg/github work details) and the stage each implies
// for an issue that carries no stage label and has no store record yet.
const (
	trackerBucketClaimed     = "claimed"
	trackerBucketInProgress  = "in_progress"
	trackerBucketClaimedByPR = "claimed_by_pr"
)

func bucketStage(bucket string) stagelabels.Stage {
	switch bucket {
	case trackerBucketClaimed:
		return stagelabels.Claimed
	case trackerBucketInProgress:
		return stagelabels.PRInProgress
	case trackerBucketClaimedByPR:
		return stagelabels.CoveredByPR
	}
	return stagelabels.None
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t
	return &u
}

// AdoptedByState values (StageTrackerItem.AdoptedByState).
const (
	stageTrackerAdoptedClosed      = "closed"
	stageTrackerAdoptedClosedTwice = "closed-twice"
	// stageTrackerAdoptedCopyFailed: a close-first supersession whose copy
	// failed; the original was reopened for a person (hivecommons/hive#15135).
	stageTrackerAdoptedCopyFailed = "copy-failed"
	// stageTrackerAdoptedResolving: the copy opened conflicting with its
	// base and the hive's fixer is on its rebase-resolve episode
	// (hivecommons/hive#15307).
	stageTrackerAdoptedResolving = "resolving-conflicts"
)

// stageTrackerAdoptedByState reads the copy's state from the record
// (hivecommons/hive#15052): the loop-guard park, else a copy the hive noted
// closed and has not yet replaced.
func stageTrackerAdoptedByState(rec stagelabels.Record) string {
	switch {
	case rec.AdoptedBy <= 0:
		return ""
	case rec.AdoptionChildClosedTwice():
		return stageTrackerAdoptedClosedTwice
	case rec.AdoptionCopyFailed():
		return stageTrackerAdoptedCopyFailed
	case rec.AdoptOrphanedBy == rec.AdoptedBy:
		return stageTrackerAdoptedClosed
	case rec.AdoptCopyConflicting:
		return stageTrackerAdoptedResolving
	}
	return ""
}

// stageTrackerSupersededBy is the live copy that supersedes an adopted
// original, 0 when the adoption was in place or the copy is closed, failed
// or being replaced (then the original is live work again). A copy still
// resolving conflicts is live: the pair stays one logical PR.
func stageTrackerSupersededBy(rec stagelabels.Record) int {
	if rec.AdoptedBy <= 0 || rec.AdoptMode == stagelabels.AdoptModeInPlace {
		return 0
	}
	switch stageTrackerAdoptedByState(rec) {
	case "", stageTrackerAdoptedResolving:
		return rec.AdoptedBy
	}
	return 0
}

// stageTrackerAdoptionLoopCaption is the 085 caption for the loop guard.
func stageTrackerAdoptionLoopCaption(rec stagelabels.Record) string {
	return "The hive's copies of this PR (last #" + strconv.Itoa(rec.AdoptedBy) + ") were closed without merging twice within an hour; the hive stopped opening copies and a person must decide"
}

// stageStoreOrShared returns the injected stage store (tests) or the
// process-wide one the reconcilers write.
func (s *Server) stageStoreOrShared() *stagelabels.Store {
	if s != nil && s.stageStore != nil {
		return s.stageStore
	}
	return stagelabels.Shared()
}

func (s *Server) stageDwellOverrides() map[string]time.Duration {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return nil
	}
	return s.deps.Config.Governor.Labels.Stage.DwellOverrides()
}

// stageTrackerStaleDecisionRecord reports whether rec parks the item at
// 045/085 while its current labels carry no park signal at all — no stage
// label for the bay, no `needs-human`, no retired needs-decision spelling.
// The labels are the newer fact: the park lifted and the record is waiting
// on the engine's next reconcile. Not so when the write-back gate kept the
// labels off the item (a contributor PR without the opt-in label): the
// labels never carried the park, the record is the only stage there is,
// and dropping it read the row as an unclassified "PR in progress" for
// hours while the engine held it at 085.
func stageTrackerStaleDecisionRecord(rec stagelabels.Record, labels []string) bool {
	if rec.Stage != stagelabels.NeedsDecision && rec.Stage != stagelabels.IssueNeedsDecision {
		return false
	}
	if rec.LabelsSuppressed {
		return false
	}
	for _, l := range labels {
		switch strings.ToLower(strings.TrimSpace(l)) {
		case needsDecisionLabel, prNeedsDecisionLabel, "needs-decision", "needs-human", "needs-direction":
			return false
		}
	}
	return true
}
