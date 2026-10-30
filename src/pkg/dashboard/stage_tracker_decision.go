package dashboard

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/rebasemerge"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The "Needs human" popover (hivecommons/hive#14994, owner request
// 2026-10-28): hovering the
// *Needs human decision* pill or the needs-human chip must say WHAT the
// person is being asked to decide, who asked, and which of the row's
// actions advance it. The tracker payload carries that as
// `decision_needed` on every row parked on a person: the reviewer's
// actual question (the `decision_question` the verdict relay recorded in
// review-verdicts.json for the PR's current head — the same text the 085
// stage comment prints after "reviewer question:"), the blocking reason in
// plain words, and the applicable actions under the SAME ids the row
// context menu uses, so the popover's buttons and the menu share one code
// path in the SPA.

// StageTrackerDecision is the decision a person owes on a tracker row.
type StageTrackerDecision struct {
	// Question is what the person must decide, in the asker's own words
	// when the hive has them (the reviewer's decision question), else a
	// sentence derived from the park reason.
	Question string `json:"question"`
	// AskedBy names who parked the item: "reviewer swarm", "sentinel",
	// "lane rule", "GitHub App permissions", "triage gate", "hive agent",
	// "escalation ledger".
	AskedBy string `json:"asked_by"`
	// Since is when the question was asked: the verdict's recorded time
	// for a reviewer question, else when the item entered its stage.
	Since *time.Time `json:"since,omitempty"`
	// Blocker says in plain words what stops the hive from proceeding on
	// its own (and, where it applies, what a human approval cannot do);
	// BlockerReason is the lane token behind it
	// ("contributor-lane:changes-requested", "sentinel-alert", …).
	Blocker       string `json:"blocker,omitempty"`
	BlockerReason string `json:"blocker_reason,omitempty"`
	// Detail is the technical background the popover folds behind one
	// collapsed "Details" (how to trust the author, the rails, DCO): never
	// in the first read (hivecommons/hive#15394).
	Detail string `json:"detail,omitempty"`
	// RedundantOf is the PR the reviewer named as already carrying this
	// PR's change (`<!-- hive-redundant-of: #N -->`), when the question is
	// "close this as a no-op?" (hivecommons/hive#15499). The popover shows
	// it as a chip and leads with "Close as superseded".
	RedundantOf int `json:"redundant_of,omitempty"`
	// Options are the actions that can advance the item, first the one the
	// owner asked to lead ("Let hive decide"), under the row context menu's
	// action ids. A disabled option says why; the SPA further greys out
	// what the signed-in viewer's role does not allow.
	Options []StageTrackerDecisionOption `json:"options"`
}

// StageTrackerDecisionOption is one action the popover offers.
type StageTrackerDecisionOption struct {
	Action           string `json:"action"`
	Label            string `json:"label"`
	Enabled          bool   `json:"enabled"`
	ReasonIfDisabled string `json:"reason_if_disabled,omitempty"`
	// Consequence says, in one clause, what happens when the action is
	// taken ("merges #317 into main now via hive's identity, attributed to
	// you"); the ⋯ menu and the popover print it under the label
	// (hivecommons/hive#15078).
	Consequence string `json:"consequence,omitempty"`
	// URL is set on link options (open:app-settings): the SPA opens it
	// instead of posting an action (hivecommons/hive#15099).
	URL string `json:"url,omitempty"`
}

// Action ids shared with the SPA's row context menu
// (STAGE_TRACKER_ACTION_LABELS in index.html).
const (
	decisionActionDelegate        = "decide:delegate"
	decisionActionApprove         = "decide:approve"
	decisionActionDirection       = "decide:direction"
	decisionActionCloseSuperseded = "decide:close-superseded"
	decisionActionCloseWontfix    = "decide:close-wontfix"
	decisionActionMergeNow        = "decide:merge-now"
	decisionActionQueueMerge      = "decide:queue-merge"
	decisionActionReauthor        = "decide:re-author"
	decisionActionAcceptTriage    = "accept-triage"
	decisionActionHold            = "hold"
	decisionActionRelease         = "release"
	decisionActionNeedsHuman      = "needs-human"
	decisionActionRetry           = "retry"
)

// Who asked, in the plain words the popover prints (owner, 2026-10-28:
// "exceedingly clear, easy to read, and concise" — no hive jargon).
const (
	decisionAskedByReviewer   = "a reviewer"
	decisionAskedBySentinel   = "the security check"
	decisionAskedByLane       = "the hive"
	decisionAskedByApp        = "GitHub permissions"
	decisionAskedByTriage     = "triage"
	decisionAskedByHiveAgent  = "the hive"
	decisionAskedByEscalation = "the needs-human label"
)

// Blocker reason tokens the tracker coins when the lane left none. The
// token is for machines (filters, tests); the popover prints Blocker.
const (
	decisionBlockerSentinel = "sentinel-alert"
	decisionBlockerHold     = "hold"
	decisionBlockerHoldHive = "hold:hive"
)

const (
	decisionWhyHeld           = "On hold — release the hold first"
	decisionWhyNoPermission   = "Only a person with the permission can do this"
	decisionWhyApproveNoGrant = "Approving does not grant the missing permission"
	decisionHeldBlockerPlain  = "On hold — the hive does nothing until the hold is released."
)

// stageTrackerVerdictLookup returns a lookup over review-verdicts.json for
// the newest aggregate recorded for repo#number at head, mirroring the
// contributor lane's own match (cmd/hive contributorLaneVerdictFromArtifact)
// so the popover quotes the question the lane acted on. nil artifact → a
// lookup that never matches.
func stageTrackerVerdictLookup(artifact *review.Artifact) func(repo string, number int, head string) (review.Aggregate, bool) {
	return func(repo string, number int, head string) (review.Aggregate, bool) {
		if artifact == nil || number == 0 || strings.TrimSpace(head) == "" {
			return review.Aggregate{}, false
		}
		var match review.Aggregate
		var ok bool
		for _, item := range artifact.Items {
			item = item.Normalize()
			if item.Number != number || !strings.EqualFold(strings.TrimSpace(item.Repo), strings.TrimSpace(repo)) || !review.SameHeadSHA(item.HeadSHA, head) {
				continue
			}
			if !ok || item.RecordedAt.After(match.RecordedAt) {
				match, ok = item, true
			}
		}
		return match, ok
	}
}

// stageTrackerDecisionVerdictLookup is stageTrackerVerdictLookup with the
// fallback a parked row needs (hivecommons/hive#15499): when the current
// head has no verdict yet — the hive moved the branch onto a new base and
// the re-review is in flight — but the PR is parked at 085 on a reviewer's
// decision, the question that parked it is the newest decision-requested
// aggregate on any head of the PR. Without it the popover printed "a
// reviewer handed this PR to a person without a specific question" while
// the review on the previous head asked a specific one.
func stageTrackerDecisionVerdictLookup(artifact *review.Artifact) func(repo string, number int, head string, parkedOnDecision bool) (review.Aggregate, bool) {
	atHead := stageTrackerVerdictLookup(artifact)
	return func(repo string, number int, head string, parkedOnDecision bool) (review.Aggregate, bool) {
		if a, ok := atHead(repo, number, head); ok {
			return a, true
		}
		if !parkedOnDecision || artifact == nil || number == 0 {
			return review.Aggregate{}, false
		}
		var match review.Aggregate
		var ok bool
		for _, item := range artifact.Items {
			item = item.Normalize()
			if item.Number != number || !strings.EqualFold(strings.TrimSpace(item.Repo), strings.TrimSpace(repo)) || !item.DecisionRequested {
				continue
			}
			if !ok || item.RecordedAt.After(match.RecordedAt) {
				match, ok = item, true
			}
		}
		return match, ok
	}
}

// stageTrackerDecisionNeeded derives the decision a row asks of a person,
// or nil when nothing on the row waits on one. verdict is the review
// aggregate for the PR's current head when the artifact has one;
// sentinelLabel is the configured sentinel alert label.
func stageTrackerDecisionNeeded(it StageTrackerItem, verdict *review.Aggregate, sentinelLabel string) *StageTrackerDecision {
	return stageTrackerDecisionNeededAt(it, verdict, sentinelLabel, time.Now())
}

// stageTrackerDecisionNeededAt is stageTrackerDecisionNeeded with the clock
// the tracker build uses, so spans in questions match the row's pill.
func stageTrackerDecisionNeededAt(it StageTrackerItem, verdict *review.Aggregate, sentinelLabel string, now time.Time) *StageTrackerDecision {
	return stageTrackerDecisionNeededWith(it, verdict, sentinelLabel, now, blockerOptionsContext{})
}

// stageTrackerDecisionNeededWith is stageTrackerDecisionNeededAt with the
// server context a blocked row's options need (the App settings link).
func stageTrackerDecisionNeededWith(it StageTrackerItem, verdict *review.Aggregate, sentinelLabel string, now time.Time, bctx blockerOptionsContext) *StageTrackerDecision {
	held := slices.Contains(it.Modifiers, modHold)
	switch {
	case it.Kind == "pr" && it.MergeTrain != nil && it.MergeTrain.State == MergeGateStateRebaseExhausted:
		return decisionForMergeTrain(it, held)
	case it.Kind == "issue" && it.Blocker != nil && !it.Mirrored && (slices.Contains(it.Modifiers, modNeedsHuman) || it.Stage == stagelabels.IssueNeedsDecision.Number() || slices.Contains(it.Modifiers, modNeedsDecision)):
		// A structured blocker outranks the generic park wording: the
		// person is not being asked to decide, the hive is saying what it
		// cannot do (hivecommons/hive#15099).
		return decisionForBlocker(it, held, bctx)
	case it.Kind == "pr" && (it.Stage == stagelabels.NeedsDecision.Number() || slices.Contains(it.Modifiers, modNeedsDecision)):
		return decisionForPR(it, verdict, sentinelLabel, held)
	case it.Kind == "pr" && it.Stage == stagelabels.ReadyForHumanMerge.Number():
		return decisionForHumanMerge(it, held)
	case it.Kind == "issue" && it.Mirrored:
		// The issue rides its covering PR; the PR's own row (nested under
		// the issue) carries the decision.
		return nil
	case it.Kind == "issue" && (it.Stage == stagelabels.IssueNeedsDecision.Number() || slices.Contains(it.Modifiers, modNeedsDecision)):
		return decisionForIssue(it, held)
	case it.Kind == "issue" && (it.Stage == stagelabels.TriageNeeded.Number() || slices.Contains(it.Modifiers, modNeedsTriage)):
		return decisionForTriage(it, held)
	case slices.Contains(it.Modifiers, modNeedsHuman):
		return decisionForNeedsHuman(it, held)
	case slices.Contains(it.Modifiers, modUnauthorized):
		return decisionForUnauthorized(it, held)
	case it.Kind == "pr" && it.Adoption != nil && it.Adoption.Action == decisionActionAdopt:
		return decisionForAdoption(it, held, now)
	case it.Kind == "pr" && it.Takeover != nil && it.Takeover.GraceElapsed && it.Adoption != nil && it.Adoption.BlockedBy != stagelabels.AdoptBlockedAuthorMaintainer:
		// The nudge grace ran out and the engine has not adopted (ask is
		// the case above; never stays the policy's word): the wait is a
		// person's to end (Adopt now / Reroute) — hivecommons/hive#15038.
		return decisionForWaitingOnContributor(it, held, now)
	case it.Kind == "pr" && it.AuthorKind == authorKindHuman && slices.Contains(stageTrackerNeedsHumanAuthorStages, it.Stage):
		return decisionForWaitingOnAuthor(it, held)
	case stageTrackerNeedsHuman(it) || (held && stageTrackerNeedsHuman(stageTrackerWithoutHold(it))):
		// Flagged for a person by a stage/kind pairing none of the cases
		// above knows (an issue wearing a PR stage label by hand, a PR at
		// a triage stage): still never a pill without an action.
		return decisionForParked(it, held)
	}
	return nil
}

// stageTrackerWithoutHold is the item as it reads with the hold pill off,
// so a held row still derives the decision its underlying state asks for.
func stageTrackerWithoutHold(it StageTrackerItem) StageTrackerItem {
	it.Modifiers = slices.DeleteFunc(slices.Clone(it.Modifiers), func(m string) bool { return m == modHold })
	return it
}

// decisionDelegateOption is "Let hive decide" for a state the hive would
// not otherwise decide: the delegate answers in plain words why it won't
// act itself and lists the person's options (hivecommons/hive#15078).
func decisionDelegateOption(held bool, consequence string) StageTrackerDecisionOption {
	o := decisionOption(decisionActionDelegate, "Let hive decide", !held, decisionWhyHeld)
	o.Consequence = consequence
	return o
}

// decisionForParked is the fallback for any needs-human state without a
// dedicated decision: say what the row is parked on and offer the generic
// moves — look again, hold, flag for a person (hivecommons/hive#15078).
func decisionForParked(it StageTrackerItem, held bool) *StageTrackerDecision {
	what := "issue"
	if it.Kind == "pr" {
		what = "PR"
	}
	stage := strings.TrimSpace(it.StageLabel)
	if stage == "" {
		stage = "stage " + strconv.Itoa(it.Stage)
	}
	d := &StageTrackerDecision{Since: it.StageSince, AskedBy: decisionAskedByLane, BlockerReason: strings.TrimSpace(it.Reason)}
	d.Question = "This " + what + " sits at " + stage + ", a stage the hive leaves to a person"
	if clause := decisionPlainReason(strings.TrimSpace(it.Reason)); strings.TrimSpace(it.Reason) != "" {
		d.Question += " (" + clause + ")"
	}
	d.Question += ". Act on it on GitHub, then re-evaluate so the hive picks it up."
	d.Blocker = "The hive makes no move here until a person does."
	if held {
		d.Blocker += " " + decisionHeldBlocker(it.Hold)
	}
	retry := decisionOption(decisionActionRetry, "Re-evaluate now", true, "")
	retry.Consequence = "Re-reads GitHub and recomputes the stage"
	d.Options = []StageTrackerDecisionOption{retry, decisionDelegateOption(held, "Hive explains what it can do here"), decisionHoldOption(held)}
	if !slices.Contains(it.Modifiers, modNeedsHuman) {
		d.Options = append(d.Options, decisionOption(decisionActionNeedsHuman, "Mark needs-human", true, ""))
	}
	return d
}

// decisionForHumanMerge: a PR parked at 090 Ready for human merge — the
// hive's lanes will not merge it unattended (untrusted author, workflow
// files, manual-merge path, branch protection, …), so the person is asked
// to merge. The lead options do exactly that (hivecommons/hive#15078):
// "Approve & merge as maintainer" merges now through the hive's GitHub
// App, attributed to the operator; "Approve — let hive merge" queues the
// PR for the hive's merge sweep, which merges it on its next pass once CI
// is green and the head has soaked. Both need the merger or owner role;
// the SPA greys them out for anyone else.
func decisionForHumanMerge(it StageTrackerItem, held bool) *StageTrackerDecision {
	base, approver := stagelabels.HumanApprovedReason(it.Reason)
	base = strings.TrimSpace(base)
	_, _, tip := stageTrackerHumanMergeReason(it.Reason)
	d := &StageTrackerDecision{Since: it.StageSince, AskedBy: decisionAskedByLane, BlockerReason: base}
	ref := "#" + strconv.Itoa(it.Number)
	// First read: one sentence of state, one of why (hivecommons/hive#15394).
	why := stageTrackerUntrustedAuthorWhy(base, it.Author)
	fix := stageTrackerUntrustedAuthorFix(base, it.Author)
	switch {
	case why != "":
		why = "Hive won't merge it alone — " + why + "."
	case tip != "":
		why = sentence(tip)
	default:
		why = "Hive's merge rules leave this one to a person."
	}
	d.Question = "Ready to merge. " + why
	details := []string{"Hive never merges this on its own; a maintainer merges here or on GitHub."}
	if fix != "" {
		details = append(details, fix)
	}
	if approver != "" {
		details = append(details, "Approved by @"+approver+".")
	}
	admitter := stagelabels.AdmissionLogin(it.AdmittedBy)
	breaking := strings.EqualFold(base, stageengineprstage.AutoMergeReasonHumanMergeRequired)
	if breaking {
		// No approval lets hive merge a breaking change (hivecommons/hive#15442).
		admitter = ""
	}
	if admitter != "" {
		// A person already took the trust call for this head (#15136):
		// the lane merges it as-is; nothing is adopted or re-authored.
		d.Question = "@" + admitter + " approved it. Hive merges it on its next pass."
		details = []string{"Admitted by @" + admitter + " (" + it.AdmittedBy + "). Hive merges this exact version once CI is green; a new push asks again."}
	}
	if dco := decisionDCOBlocker(it); dco != "" {
		details = append(details, dco)
	}
	if held {
		details = append(details, decisionHeldBlocker(it.Hold))
	}
	d.Detail = stageTrackerSayOnce(d.Question, details...)
	mergeNow := decisionMergeNowOption(it, held)
	queue := decisionOption(decisionActionQueueMerge, "Approve — let hive merge", !held, "Release the hold first")
	queue.Consequence = "Hive merges " + ref + " on its next pass"
	if admitter != "" && !held {
		queue.Enabled, queue.ReasonIfDisabled = false, "@"+admitter+" already approved it; hive merges it next pass"
	}
	if breaking {
		// The lanes skip a breaking-titled / needs-owner-merge PR however
		// it is queued (hivecommons/hive#15442): the merge-now click is the
		// one path, so queueing is offered greyed with the reason.
		queue.Enabled, queue.ReasonIfDisabled = false, "Hive never merges a breaking change; merge it as maintainer"
	}
	if it.DCOUnsigned > 0 {
		mergeNow.Consequence += "; " + decisionDCOConsequence(it)
		queue.Consequence += "; " + decisionDCOConsequence(it)
	}
	direction := decisionOption(decisionActionDirection, "Give direction", it.AuthorKind != authorKindHuman, "Hive did not open this PR, so it cannot carry a direction to its author; leave a review on GitHub")
	direction.Consequence = "Asks the author's agent for changes instead"
	closeSup := decisionOption(decisionActionCloseSuperseded, "Close as superseded", true, "")
	closeSup.Consequence = "Closes " + ref + ", naming the PR that replaces it"
	closeWontfix := decisionOption(decisionActionCloseWontfix, "Close as won't fix", true, "")
	closeWontfix.Consequence = "Closes " + ref + " without merging"
	delegate := decisionDelegateOption(held, "Hive explains and recommends; merges nothing")
	d.Options = []StageTrackerDecisionOption{mergeNow, queue}
	if it.DCOUnsigned > 0 {
		d.Options = append(d.Options, decisionReauthorOption(it, ref, held))
	}
	d.Options = append(d.Options, delegate, direction, closeSup, closeWontfix, decisionHoldOption(held))
	return d
}

// stageTrackerSayOnce joins parts into one text, dropping any sentence
// that already appeared in lead or earlier in the parts, so a hover never
// says the same thing twice (hivecommons/hive#15394).
func stageTrackerSayOnce(lead string, parts ...string) string {
	seen := map[string]bool{}
	for _, sent := range stageTrackerSentences(lead) {
		seen[sent] = true
	}
	var out []string
	for _, part := range parts {
		var keep []string
		for _, sent := range stageTrackerSentences(part) {
			if seen[sent] {
				continue
			}
			seen[sent] = true
			keep = append(keep, sentence(sent))
		}
		if len(keep) > 0 {
			out = append(out, strings.Join(keep, " "))
		}
	}
	return strings.Join(out, " ")
}

// stageTrackerSentences splits text on sentence ends and normalises each
// sentence (trimmed, trailing punctuation dropped, lower-cased first rune)
// for say-it-once comparison.
func stageTrackerSentences(text string) []string {
	var out []string
	for _, raw := range strings.FieldsFunc(text, func(r rune) bool { return r == '.' || r == '?' || r == '!' }) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		r := []rune(raw)
		r[0] = unicode.ToLower(r[0])
		out = append(out, string(r))
	}
	return out
}

// sentence capitalises a clause and ends it with a full stop.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	s = string(r)
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "?") && !strings.HasSuffix(s, "!") {
		s += "."
	}
	return s
}

// decisionDCOBlocker says, before anyone presses Approve, that the head
// cannot merge as-is where DCO is required: the stage record's commit check
// found commits without a Signed-off-by (hivecommons/hive#15136). "" when
// the check is not cached for this head or every commit is signed.
func decisionDCOBlocker(it StageTrackerItem) string {
	if it.DCOUnsigned <= 0 {
		return ""
	}
	return fmt.Sprintf("%d of %s no Signed-off-by, so a required DCO check refuses the merge. Re-author as hive PR is the separate choice for that; an Approve never re-authors by itself.",
		it.DCOUnsigned, pluralCommitsHave(it.DCOTotal))
}

// decisionDCOConsequence is the short DCO caveat under the merge options.
func decisionDCOConsequence(it StageTrackerItem) string {
	return fmt.Sprintf("%d of %s no Signed-off-by (DCO)", it.DCOUnsigned, pluralCommitsHave(it.DCOTotal))
}

// pluralCommitsHave renders "3 commits have" / "1 commit has" for the DCO
// sentences; a total the record does not know reads "its commits have".
func pluralCommitsHave(total int) string {
	switch {
	case total == 1:
		return "1 commit has"
	case total > 1:
		return strconv.Itoa(total) + " commits have"
	}
	return "its commits have"
}

// decisionReauthorOption is the operator's explicit "re-author as hive PR"
// choice (hivecommons/hive#15136): the hive re-homes the commits on a
// hive-owned, signed-off branch and opens a superseding PR. A person's PR
// is never re-authored by the hive, so for a human author the option is
// shown disabled with the step that does unblock it.
func decisionReauthorOption(it StageTrackerItem, ref string, held bool) StageTrackerDecisionOption {
	o := decisionOption(decisionActionReauthor, "Re-author as hive PR", !held && it.AuthorKind != authorKindHuman, "Release the hold first")
	if it.AuthorKind == authorKindHuman {
		who := "the author"
		if it.Author != "" {
			who = "@" + it.Author
		}
		o.ReasonIfDisabled = "Hive does not re-author a person's PR; ask " + who + " to sign off and push"
	}
	o.Consequence = "Hive reopens " + ref + " as a signed-off hive PR; nothing is merged as-is"
	return o
}

// decisionMergeNowOption is "Approve & merge as maintainer", or — when the
// row wears needs-rebase, so a plain merge would be a GitHub 405 — "Rebase,
// then merge as maintainer": the same click records the approval, has the
// hive rebase the PR, and merges it on green (hivecommons/hive#14822).
func decisionMergeNowOption(it StageTrackerItem, held bool) StageTrackerDecisionOption {
	ref := "#" + strconv.Itoa(it.Number)
	into := strings.TrimSpace(it.BaseRef)
	if into == "" {
		into = "its base branch"
	}
	if stageTrackerNeedsRebase(it) {
		opt := decisionOption(decisionActionMergeNow, rebaseMergeFollowThroughLabel, !held, "Release the hold first")
		opt.Consequence = rebaseMergeFollowThroughConsequence(it.Number, strings.TrimSpace(it.BaseRef))
		return opt
	}
	opt := decisionOption(decisionActionMergeNow, "Approve & merge as maintainer", !held, "Release the hold first")
	opt.Consequence = "Merges " + ref + " into " + into + " now, credited to you"
	return opt
}

// stageTrackerNeedsRebase: the row wears needs-rebase (modifier or label),
// or a rebase-then-merge follow-through is already running on it.
func stageTrackerNeedsRebase(it StageTrackerItem) bool {
	if slices.Contains(it.Modifiers, modNeedsRebase) {
		return true
	}
	for _, l := range it.Labels {
		if strings.EqualFold(strings.TrimSpace(l), "needs-rebase") {
			return true
		}
	}
	return it.RebaseMerge != nil && it.RebaseMerge.State != "" && it.RebaseMerge.State != "merged" && it.RebaseMerge.State != "failed"
}

// decisionForUnauthorized: a row wearing hive:unauthorized outside 085 —
// the hive's GitHub App lacks the permission for its next step, so a
// person takes that step on GitHub (approve the held workflow runs, merge
// the workflow-file PR, …) and then asks the hive to look again.
func decisionForUnauthorized(it StageTrackerItem, held bool) *StageTrackerDecision {
	d := &StageTrackerDecision{Since: it.StageSince, AskedBy: decisionAskedByApp, BlockerReason: strings.TrimSpace(it.Reason)}
	step := strings.TrimSpace(it.UnauthorizedStep)
	if step == "" {
		_, tip := stageTrackerUnauthorized(it.Reason)
		d.Question = "The hive's GitHub App lacks a permission for the next step. " + tip
	} else {
		d.Question = "The hive's GitHub App may not " + unauthorizedStepWords(step) + " here. Do that step on GitHub, then re-evaluate so the hive picks it up."
	}
	d.Blocker = strings.TrimSpace(it.Unauthorized)
	if d.Blocker == "" {
		d.Blocker = decisionWhyNoPermission + "."
	}
	if held {
		d.Blocker += " " + decisionHeldBlocker(it.Hold)
	}
	retry := decisionOption(decisionActionRetry, "Re-evaluate now", true, "")
	retry.Consequence = "Re-reads GitHub; hive resumes once the step is done"
	d.Options = []StageTrackerDecisionOption{retry, decisionDelegateOption(held, "Hive explains which step it may not take"), decisionHoldOption(held)}
	if !slices.Contains(it.Modifiers, modNeedsHuman) {
		d.Options = append(d.Options, decisionOption(decisionActionNeedsHuman, "Mark needs-human", true, ""))
	}
	return d
}

// decisionForWaitingOnAuthor: a human-authored PR at 071 Needs author /
// 072 Returned to contributor — the hive waits for its author to push.
// The hive never adopts a human's PR, so the person's choices are to wait,
// hold, or close it (hivecommons/hive#15078).
func decisionForWaitingOnAuthor(it StageTrackerItem, held bool) *StageTrackerDecision {
	who := "its author"
	if a := strings.TrimSpace(it.Author); a != "" {
		who = "@" + a
	}
	ref := "#" + strconv.Itoa(it.Number)
	d := &StageTrackerDecision{Since: it.StageSince, AskedBy: decisionAskedByLane, BlockerReason: strings.TrimSpace(it.Reason)}
	what := "a new commit"
	if clause := decisionPlainReason(strings.TrimSpace(it.Reason)); strings.TrimSpace(it.Reason) != "" && clause != "the hive parked it" {
		what = "a new commit (" + clause + ")"
	}
	d.Question = "This PR waits on " + who + " for " + what + ". The hive never rewrites a person's PR. Wait for them, or close it?"
	d.Blocker = "Nothing for the hive to do until " + who + " pushes."
	if held {
		d.Blocker += " " + decisionHeldBlocker(it.Hold)
	}
	closeSup := decisionOption(decisionActionCloseSuperseded, "Close as superseded", true, "")
	closeSup.Consequence = "Closes " + ref + ", naming the PR that replaces it"
	closeWontfix := decisionOption(decisionActionCloseWontfix, "Close as won't fix", true, "")
	closeWontfix.Consequence = "Closes " + ref + " without merging"
	d.Options = []StageTrackerDecisionOption{decisionDelegateOption(held, "Hive weighs waiting against closing; never rewrites the PR"), closeSup, closeWontfix, decisionHoldOption(held)}
	return d
}

// decisionForWaitingOnContributor: a 072 clanker PR whose adoption came due
// and is held (hivecommons/hive#15038). The options are the takeover's.
func decisionForWaitingOnContributor(it StageTrackerItem, held bool, now time.Time) *StageTrackerDecision {
	a := it.Adoption
	who := "the author"
	if a.Author != "" {
		who = "@" + a.Author
	}
	d := &StageTrackerDecision{Since: a.Since, AskedBy: decisionAskedByLane, BlockerReason: "adoption:" + a.BlockerReason()}
	gone := "has been offline since the nudge"
	if off := strings.TrimSpace(stageTrackerAdoptionOfflineFor(*a, now)); off != "" {
		gone = "has been offline for " + off
	}
	if a.AuthorOnline {
		gone = "is online but has not answered " + plural(a.Nudges, "nudge")
	}
	d.Question = who + " " + gone + " and the hive is not adopting on its own; take the PR over now, or reroute its follow-up work to another clanker?"
	d.Blocker = "The PR waits on " + who + "'s hive; adoption is held (" + a.BlockerReason() + ")."
	if held {
		d.Blocker += " " + decisionHeldBlocker(it.Hold)
	}
	d.Options = append(takeoverDecisionOptions(it.Takeover, held), decisionHoldOption(held))
	return d
}

// decisionForAdoption: a nudged 072 PR whose author maintains the
// repository, held for an owner under
// governor.labels.stage.adoption.maintainer_policy: ask
// (hivecommons/hive#14524). The question names the author and how long
// their clanker has been gone; the lead option is Adopt.
func decisionForAdoption(it StageTrackerItem, held bool, now time.Time) *StageTrackerDecision {
	a := it.Adoption
	who := "the author"
	if a.Author != "" {
		who = "@" + a.Author
	}
	d := &StageTrackerDecision{Since: a.Since, AskedBy: decisionAskedByLane, BlockerReason: "adoption:" + a.BlockedBy}
	gone := "has been offline since the nudge"
	if off := strings.TrimSpace(stageTrackerAdoptionOfflineFor(*a, now)); off != "" {
		gone = "has been offline for " + off
	}
	if a.AuthorOnline {
		gone = "is online but has not answered " + plural(a.Nudges, "nudge")
	}
	d.Question = who + " maintains this repository and " + gone + "; adopt the PR? The hive would take it over as it does any contributor's (the author's commits and sign-off are kept)."
	d.Blocker = "maintainer_policy is ask: the hive never adopts a maintainer's PR on its own; a person decides."
	if held {
		d.Blocker += " " + decisionHeldBlocker(it.Hold)
	}
	d.Options = []StageTrackerDecisionOption{
		decisionOption(decisionActionAdopt, "Adopt (hive takes over)", !held, "Release the hold first"),
	}
	d.Options = append(d.Options, takeoverDecisionOptions(it.Takeover, held)...)
	d.Options = append(d.Options, decisionHoldOption(held))
	return d
}

func decisionHoldOption(held bool) StageTrackerDecisionOption {
	if held {
		return StageTrackerDecisionOption{Action: decisionActionRelease, Label: "Release hold", Enabled: true}
	}
	return StageTrackerDecisionOption{Action: decisionActionHold, Label: "Hold", Enabled: true}
}

func decisionOption(action, label string, enabled bool, why string) StageTrackerDecisionOption {
	if enabled {
		why = ""
	}
	return StageTrackerDecisionOption{Action: action, Label: label, Enabled: enabled, ReasonIfDisabled: why}
}

// decisionChangesRequested reports whether the base reason says a swarm
// reviewer asked for changes — directly, or as the lane reason an adoption
// stalled on.
func decisionChangesRequested(base string) bool {
	lower := strings.ToLower(strings.TrimSpace(base))
	lower = strings.TrimPrefix(lower, stagelabels.LaneReasonAdoptionStalledPrefix)
	switch lower {
	case stageengineprstage.ContributorLaneReasonChangesRequested, stagelabels.LaneReasonChangesRequested,
		"reviewer-verdict:request_changes", "changes-requested", stageengineprstage.AutoMergeReasonHumanChangesRequested:
		return true
	}
	return false
}

// decisionLaneRule reports whether the base reason is a lane rule that
// never merges unattended whoever approves (workflow files, removed tests,
// human-merge paths, no reviewer, refused merges, …).
func decisionLaneRule(base string) bool {
	lower := strings.ToLower(strings.TrimSpace(base))
	switch lower {
	case stageengineprstage.ContributorLaneReasonWorkflowPath, stageengineprstage.ContributorLaneReasonTestsRemoved,
		stageengineprstage.ContributorLaneReasonNoReviewer, stageengineprstage.ContributorLaneReasonReporterTrustHold,
		stageengineprstage.ContributorLaneReasonNoAppBotLogin, stageengineprstage.ContributorLaneReasonFilesEvidence,
		stageengineprstage.AutoMergeReasonHumanMergePath, stageengineprstage.AutoMergeReasonHumanMergePathEvidence,
		stageengineprstage.AutoMergeReasonIntentUnauthorized, stageengineprstage.AutoMergeReasonIntentEvidence,
		stageengineprstage.AutoMergeReasonMergeRefusedReview, stageengineprstage.AutoMergeReasonMergeRefusedMethod,
		stageengineprstage.AutoMergeReasonNoAppBotLogin, stageengineprstage.AutoMergeReasonHumanMergeRequired:
		return true
	}
	return false
}

// decisionPlainReason renders a lane rule in plain words for the popover
// (the owner's bar: no reason slugs, no stage codes, no hive jargon).
func decisionPlainReason(base string) string {
	switch strings.ToLower(strings.TrimSpace(base)) {
	case stageengineprstage.ContributorLaneReasonWorkflowPath:
		return "it edits CI workflow files"
	case stageengineprstage.ContributorLaneReasonTestsRemoved:
		return "it removes tests"
	case stageengineprstage.ContributorLaneReasonNoReviewer:
		return "no reviewer is configured for this repo"
	case stageengineprstage.ContributorLaneReasonReporterTrustHold:
		return "the hive held it for the trust level of the linked issue's reporter"
	case stageengineprstage.ContributorLaneReasonNoAppBotLogin, stageengineprstage.AutoMergeReasonNoAppBotLogin:
		return "the hive's GitHub App login is not known"
	case stageengineprstage.ContributorLaneReasonFilesEvidence:
		return "the hive could not read its changed files"
	case stageengineprstage.AutoMergeReasonHumanMergePath, stageengineprstage.AutoMergeReasonHumanMergePathEvidence:
		return "it touches files a person must merge"
	case stageengineprstage.AutoMergeReasonHumanMergeRequired:
		return "it is a breaking change (⚠️ title or needs-owner-merge label) — the owner merges it"
	case stageengineprstage.AutoMergeReasonIntentUnauthorized, stageengineprstage.AutoMergeReasonIntentEvidence:
		return "the hive could not confirm the author's merge intent"
	case stageengineprstage.AutoMergeReasonMergeRefusedReview:
		return "GitHub requires a review the hive cannot give"
	case stageengineprstage.AutoMergeReasonMergeRefusedMethod:
		return "GitHub refused the hive's merge method"
	}
	// The shared decision glossary (pkg/delegatedecision) covers the
	// reasons the lane shares with the delegate — changes requested,
	// sentinel, conflicts, red CI, DCO — in the same words the toast and
	// the PR comment use (hivecommons/hive#14822).
	if e, ok := delegatedecision.Explain(base); ok && e.Kind == delegatedecision.GlossaryReason {
		return e.Plain
	}
	if text := humanizeStageReason(base); text != "" {
		return text
	}
	return "the hive parked it"
}

func decisionForPR(it StageTrackerItem, verdict *review.Aggregate, sentinelLabel string, held bool) *StageTrackerDecision {
	base, _ := stagelabels.HumanApprovedReason(it.Reason)
	base = strings.TrimSpace(base)
	lower := strings.ToLower(base)
	sha := shortHead(it.Head)
	at := ""
	if sha != "" {
		at = " on " + sha
	}
	d := &StageTrackerDecision{Since: it.StageSince, AskedBy: decisionAskedByLane, BlockerReason: base}
	approveOK, approveWhy := true, ""
	delegateOK, delegateWhy := !held, decisionWhyHeld
	var selfHold *StageTrackerHold
	if held && it.Hold != nil && strings.EqualFold(it.Hold.By, stagelabels.HoldByHive) && it.Hold.Source != "" && it.Hold.Source != stagelabels.HoldSourceLabel {
		selfHold = it.Hold
	}

	sentinel := it.SentinelAlert || lower == "sentinel" || lower == stageengineprstage.ContributorLaneReasonSentinelBlocked
	changes := decisionChangesRequested(base)
	laneRule := decisionLaneRule(base)
	adoptionLane, adoptionStalled := strings.CutPrefix(base, stagelabels.LaneReasonAdoptionStalledPrefix)
	forkGuard, forkGuarded := strings.CutPrefix(base, stageengineprstage.StageReasonForkGuard)
	// hivecommons/hive#15307: the hive's adoption copy (or the original it
	// supersedes) after the fixer's two rebase-resolve attempts failed.
	rebaseFailedTwice := lower == stagelabels.LaneReasonRebaseFailedTwice || (adoptionStalled && adoptionLane == stagelabels.LaneReasonRebaseFailedTwice)
	reviewerDeferred := lower == stageengine.StageReasonVerdictRequiresHuman || lower == stageengine.StageReasonDecisionRequested

	// WHAT it is waiting on: the question, most specific source first. A
	// reviewer's own question is quoted verbatim — it is the text the
	// hive's "Needs a decision" comment on the PR prints after "reviewer
	// question:", read from the same verdict record.
	switch {
	case it.UnauthorizedStep != "":
		d.AskedBy = decisionAskedByApp
		d.Question = "The hive's GitHub App may not " + unauthorizedStepWords(it.UnauthorizedStep) + " here. A maintainer has to do that step on GitHub."
		d.Blocker = strings.TrimSpace(it.Unauthorized)
		delegateOK, delegateWhy = false, decisionWhyNoPermission
		approveOK, approveWhy = false, decisionWhyApproveNoGrant
	case verdict != nil && strings.TrimSpace(verdict.DecisionQuestion) != "":
		d.AskedBy = decisionAskedByReviewer
		d.Question = strings.TrimSpace(verdict.DecisionQuestion)
		if !verdict.RecordedAt.IsZero() {
			t := verdict.RecordedAt
			d.Since = &t
		}
	case verdict != nil && verdict.RedundantOf > 0:
		d.AskedBy = decisionAskedByReviewer
		d.Question = "A reviewer found this PR's change already on the base through #" + strconv.Itoa(verdict.RedundantOf) + ". Close it as superseded, or give direction?"
		if !verdict.RecordedAt.IsZero() {
			t := verdict.RecordedAt
			d.Since = &t
		}
	case selfHold != nil:
		// The hive held the PR itself (hivecommons/hive#15020): say so,
		// name the gate and the issue, and offer release / keep.
		d.AskedBy = decisionAskedByLane
		d.Question = "The hive held this PR itself for " + decisionHoldReason(selfHold) + ". Release the hold (the lane then merges it on its next pass once the other gates are clear), or keep it?"
		if selfHold.Since != nil {
			t := *selfHold.Since
			d.Since = &t
		}
		d.Blocker = "The hive set `hold` on its own: " + decisionHoldReason(selfHold) + ". It does nothing more until the hold is released."
		d.BlockerReason = decisionBlockerHoldHive
	case verdict != nil && decisionFirstNonEmpty(verdict.HumanSummaries) != "":
		d.AskedBy = decisionAskedByReviewer
		d.Question = "A reviewer handed this to a person: " + decisionFirstNonEmpty(verdict.HumanSummaries)
		if !verdict.RecordedAt.IsZero() {
			t := verdict.RecordedAt
			d.Since = &t
		}
	case reviewerDeferred:
		d.AskedBy = decisionAskedByReviewer
		d.Question = "A reviewer handed this PR to a person without a specific question. Merge it as is, give direction, or close it?"
	case sentinel:
		d.AskedBy = decisionAskedBySentinel
		reason := strings.TrimSpace(it.SentinelReason)
		if reason == "" {
			reason = "security-relevant changes"
		}
		d.Question = "The security check flagged " + reason + at + ". Accept it as is, give direction, or close it?"
	case lower == stagelabels.LaneReasonAdoptionChildClosedTwice:
		d.Question = "The hive's copies of this PR were closed without merging twice within an hour, so the hive stopped opening copies. Reopen the last copy (#" + strconv.Itoa(it.AdoptedBy) + "), give direction, or close it?"
	case rebaseFailedTwice:
		d.Question = "The hive adopted this PR but its fixer could not resolve the conflicts with the base in two attempts. Adopt the commits into a fresh branch, give direction, or close it as superseded?"
	case lower == stagelabels.LaneReasonAdoptionCopyFailed:
		d.Question = "The hive's copy of this PR (#" + strconv.Itoa(it.AdoptedBy) + ") failed, so this PR was reopened. Ask the hive to supersede it again, give direction, or close it?"
	case adoptionStalled:
		d.Question = "The hive took this PR over but could not move it (" + decisionPlainReason(adoptionLane) + "). Give direction, or close it?"
	case forkGuarded:
		d.Question = "CI will not run for this fork PR (" + strings.ReplaceAll(forkGuard, ",", ", ") + "). A maintainer must push the branch to this repo — or close it?"
	case changes:
		d.AskedBy = decisionAskedByReviewer
		d.Question = "A reviewer requested changes" + at + ". Give direction for a new commit, or close it?"
	case laneRule:
		d.Question = "The hive never merges this by itself (" + decisionPlainReason(base) + "). Approve it for a maintainer to merge, give direction, or close it?"
	default:
		clause := decisionPlainReason(base)
		if base == "" {
			clause = strings.TrimSpace(it.Caption)
		}
		if clause == "" {
			clause = "the hive parked it"
		}
		d.Question = "A person must decide (" + clause + "). Approve, give direction, or close?"
	}

	// The blocker in plain words, and what it disables.
	switch {
	case it.UnauthorizedStep != "":
		// set above
	case changes:
		d.Blocker = "A reviewer requested changes" + at + " — needs a new commit. Approving cannot override that."
		approveOK, approveWhy = false, d.Blocker
	case sentinel:
		d.Blocker = "The security check has an open alert. The hive will not merge until a person removes the `" + sentinelLabel + "` label."
		if base == "" {
			d.BlockerReason = decisionBlockerSentinel
		}
	case laneRule:
		d.Blocker = "The hive never merges this by itself. After approval, a maintainer merges."
	case forkGuarded:
		d.Blocker = "Neither the author nor the hive can make CI run for this fork PR."
	case reviewerDeferred:
		d.Blocker = "The hive will not merge until a person decides."
	}
	if held && selfHold == nil {
		if d.Blocker != "" {
			d.Blocker += " "
		}
		d.Blocker += decisionHeldBlocker(it.Hold)
		if d.BlockerReason == "" {
			d.BlockerReason = decisionBlockerHold
		}
	}

	d.Options = []StageTrackerDecisionOption{
		decisionOption(decisionActionDelegate, "Let hive decide", delegateOK, delegateWhy),
		decisionOption(decisionActionApprove, "Approve", approveOK, approveWhy),
		decisionOption(decisionActionDirection, "Give direction", true, ""),
		decisionOption(decisionActionCloseSuperseded, "Close as superseded", true, ""),
		decisionOption(decisionActionCloseWontfix, "Close as won't fix", true, ""),
		decisionHoldOption(held),
	}
	// A PR parked here that also needs a rebase has no lane rebasing it
	// until a person speaks (hivecommons/hive#14822): offer the maintainer
	// merge with the rebase folded in, right after Approve.
	if stageTrackerNeedsRebase(it) && !sentinel {
		d.Options = slices.Insert(d.Options, 2, decisionMergeNowOption(it, held))
	}
	if rebaseFailedTwice {
		// The two answers to a rebase nobody could finish lead, as they do
		// for merge-now (rebaseMergeFailureOptions).
		d.Options = slices.Insert(d.Options, 0, decisionOption(decisionActionAdopt, rebasemerge.OptionAdoptFreshLabel, true, ""))
	}
	if selfHold != nil {
		// Release leads: it is the one action that answers the question.
		d.Options = append([]StageTrackerDecisionOption{decisionHoldOption(true)}, d.Options[:len(d.Options)-1]...)
	}
	if verdict != nil && verdict.RedundantOf > 0 && selfHold == nil {
		// The reviewer named the PR that already carries this change
		// (hivecommons/hive#15499): closing as superseded is the answer
		// to that question, so it leads and says which PR.
		d.RedundantOf = verdict.RedundantOf
		d.Options = decisionLeadWithCloseSuperseded(d.Options, verdict.RedundantOf)
	}
	return d
}

// decisionLeadWithCloseSuperseded moves "Close as superseded" to the front
// of opts with the superseding PR named in its consequence.
func decisionLeadWithCloseSuperseded(opts []StageTrackerDecisionOption, redundantOf int) []StageTrackerDecisionOption {
	lead := decisionOption(decisionActionCloseSuperseded, "Close as superseded", true, "")
	lead.Consequence = "Closes this PR as superseded by #" + strconv.Itoa(redundantOf) + ", which already carries its change."
	out := make([]StageTrackerDecisionOption, 0, len(opts)+1)
	out = append(out, lead)
	for _, o := range opts {
		if o.Action == decisionActionCloseSuperseded {
			continue
		}
		out = append(out, o)
	}
	return out
}

// decisionHoldReason is the hold's plain-words reason for the popover.
func decisionHoldReason(h *StageTrackerHold) string {
	if h != nil {
		if reason := strings.TrimSpace(h.Reason); reason != "" {
			return reason
		}
	}
	return "its own gate"
}

// decisionHeldBlocker is the "On hold" blocker sentence, naming who set the
// hold when the row knows (hivecommons/hive#15020).
func decisionHeldBlocker(h *StageTrackerHold) string {
	if h == nil || strings.TrimSpace(h.By) == "" {
		return decisionHeldBlockerPlain
	}
	who := "@" + strings.TrimSpace(h.By)
	if strings.EqualFold(h.By, stagelabels.HoldByHive) {
		who = "the hive"
	}
	s := "On hold (set by " + who
	if reason := strings.TrimSpace(h.Reason); reason != "" {
		s += ": " + reason
	}
	return s + ") — the hive does nothing until the hold is released."
}

func decisionForIssue(it StageTrackerItem, held bool) *StageTrackerDecision {
	d := &StageTrackerDecision{Since: it.StageSince, AskedBy: decisionAskedByHiveAgent, BlockerReason: strings.TrimSpace(it.Reason)}
	d.Question = "The hive parked this issue for a person. Approve the direction, send it back with direction, or close it?"
	d.Blocker = "No agent picks this up until a person answers. The hive's question is in its latest comment on the issue."
	if held {
		d.Blocker += " " + decisionHeldBlocker(it.Hold)
		if d.BlockerReason == "" {
			d.BlockerReason = decisionBlockerHold
		}
	}
	d.Options = []StageTrackerDecisionOption{
		decisionOption(decisionActionDelegate, "Let hive decide", !held, decisionWhyHeld),
		decisionOption(decisionActionApprove, "Approve direction", true, ""),
		decisionOption(decisionActionDirection, "Send back", true, ""),
		decisionOption(decisionActionCloseSuperseded, "Close as superseded", true, ""),
		decisionOption(decisionActionCloseWontfix, "Close as won't fix", true, ""),
		decisionHoldOption(held),
	}
	return d
}

func decisionForTriage(it StageTrackerItem, held bool) *StageTrackerDecision {
	d := &StageTrackerDecision{Since: it.StageSince, AskedBy: decisionAskedByTriage, BlockerReason: strings.TrimSpace(it.Reason)}
	d.Question = "Should the hive work on this issue? Accept triage so an agent can take it, hold it, or mark it needs-human."
	d.Blocker = "No agent picks up an issue until a maintainer accepts triage."
	if held {
		d.Blocker += " " + decisionHeldBlocker(it.Hold)
	}
	d.Options = []StageTrackerDecisionOption{
		decisionOption(decisionActionAcceptTriage, "Accept triage", true, ""),
		decisionDelegateOption(held, "Hive triages it for you, or explains why not"),
		decisionHoldOption(held),
	}
	if !slices.Contains(it.Modifiers, modNeedsHuman) {
		d.Options = append(d.Options, decisionOption(decisionActionNeedsHuman, "Mark needs-human", true, ""))
	}
	return d
}

func decisionForNeedsHuman(it StageTrackerItem, held bool) *StageTrackerDecision {
	d := &StageTrackerDecision{Since: it.StageSince, AskedBy: decisionAskedByEscalation, BlockerReason: "label:" + modNeedsHuman}
	what := "issue"
	if it.Kind == "pr" {
		what = "PR"
	}
	d.Question = "This " + what + " is marked needs-human: automated fixes ran out, or a person flagged it. Review it, then resolve it here (lifts the label) or remove the label on GitHub when the hive may continue."
	d.Blocker = "The hive does nothing with a needs-human item until a person removes the label."
	if held {
		d.Blocker += " " + decisionHeldBlocker(it.Hold)
	}
	// The matching action for the ask (hivecommons/hive#15078): Approve
	// lifts needs-human — on a PR it also posts /lgtm + /approve as the
	// operator, on an issue it records /hive approve and un-parks it.
	resolve := decisionOption(decisionActionApprove, "Resolve — lift needs-human", !held, "Release the hold first")
	if it.Kind == "pr" {
		resolve.Consequence = "Lifts needs-human and approves as you; hive merges"
	} else {
		resolve.Consequence = "Lifts needs-human and approves as you; hive carries on"
	}
	d.Options = []StageTrackerDecisionOption{resolve}
	// A needs-human PR still waiting on its contributor's clanker offers
	// the takeover actions too (hivecommons/hive#15038).
	d.Options = append(d.Options, takeoverDecisionOptions(it.Takeover, held)...)
	d.Options = append(d.Options, decisionDelegateOption(held, "Hive reviews it and lifts needs-human, or explains why not"), decisionHoldOption(held))
	return d
}

func decisionFirstNonEmpty(ss []string) string {
	for _, s := range ss {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	return ""
}
