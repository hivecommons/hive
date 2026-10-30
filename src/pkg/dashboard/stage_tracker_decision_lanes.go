package dashboard

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

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
	d.Question = "This PR waits on " + who + " to push " + what + ". The hive never rewrites a person's PR; it waits unless you say otherwise."
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
