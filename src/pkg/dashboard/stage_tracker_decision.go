package dashboard

import (
	"slices"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
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
}

// Action ids shared with the SPA's row context menu
// (STAGE_TRACKER_ACTION_LABELS in index.html).
const (
	decisionActionDelegate        = "decide:delegate"
	decisionActionApprove         = "decide:approve"
	decisionActionDirection       = "decide:direction"
	decisionActionCloseSuperseded = "decide:close-superseded"
	decisionActionCloseWontfix    = "decide:close-wontfix"
	decisionActionAcceptTriage    = "accept-triage"
	decisionActionHold            = "hold"
	decisionActionRelease         = "release"
	decisionActionNeedsHuman      = "needs-human"
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
	held := slices.Contains(it.Modifiers, modHold)
	switch {
	case it.Kind == "pr" && (it.Stage == stagelabels.NeedsDecision.Number() || slices.Contains(it.Modifiers, modNeedsDecision)):
		return decisionForPR(it, verdict, sentinelLabel, held)
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
	case it.Kind == "pr" && it.Adoption != nil && it.Adoption.Action == decisionActionAdopt:
		return decisionForAdoption(it, held, now)
	}
	return nil
}

// decisionForAdoption: a nudged 072 PR whose author maintains the
// repository, held for an owner under
// governor.labels.stage.adoption.maintainer_policy: ask
// (hivecommons/hive#14524). The question names the author and how long
// their hive has been gone; the lead option is Adopt.
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
		decisionHoldOption(held),
	}
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
		stageengineprstage.AutoMergeReasonNoAppBotLogin:
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
	if selfHold != nil {
		// Release leads: it is the one action that answers the question.
		d.Options = append([]StageTrackerDecisionOption{decisionHoldOption(true)}, d.Options[:len(d.Options)-1]...)
	}
	return d
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
	d.Question = "This " + what + " is marked needs-human: automated fixes ran out, or a person flagged it. Review it on GitHub and remove the label when the hive may continue."
	d.Blocker = "The hive does nothing with a needs-human item until a person removes the label."
	if held {
		d.Blocker += " " + decisionHeldBlocker(it.Hold)
	}
	d.Options = []StageTrackerDecisionOption{decisionHoldOption(held)}
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
