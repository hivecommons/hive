package dashboard

import (
	"slices"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/rebasemerge"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

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
	hiveSupersederStop := stagelabels.HiveSupersederHardStopReason(base)

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
	case hiveSupersederStop != "":
		// The owner's superseder rule (hive-authored superseding PRs
		// auto-merge with no decision) was overridden by its one exception
		// class; the caption names which stop, never "needs your decision".
		d.AskedBy = decisionAskedByLane
		d.Question = "The hive's superseding PR would auto-merge (owner rule), but " + stagelabels.HiveSupersederHardStopProse(hiveSupersederStop) + ". Clear that stop and the lane merges it on its next pass, or close it?"
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
		if verdict != nil && strings.TrimSpace(verdict.DecisionFinding) != "" {
			d.Blocker = "The review found: " + strings.TrimSpace(verdict.DecisionFinding) + ". " + d.Blocker
		}
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
	lead.Lead = true
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
