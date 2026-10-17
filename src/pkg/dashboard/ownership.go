package dashboard

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// The "your turn" decision (hivecommons/hive#11507, part A #11565): for every
// open issue and pull request on a watched repository the server decides,
// once per status refresh, whether the item waits on a person, which kind of
// action resolves it (the verb), why (the reason code and a one-line reason),
// and its place in the inbox. Consumers read the stored decision and never
// re-derive it from labels or bands.

const (
	OwnershipVerbInspect = "INSPECT"
	OwnershipVerbDecide  = "DECIDE"
	OwnershipVerbSteer   = "STEER"
	OwnershipVerbSpec    = "SPEC"
	OwnershipVerbReview  = "REVIEW"
	OwnershipVerbTriage  = "TRIAGE"
	OwnershipVerbAdmit   = "ADMIT"
	OwnershipVerbClose   = "CLOSE"
)

// ownershipVerbOrder is the inbox's group order; CLOSE always sorts last.
var ownershipVerbOrder = []string{
	OwnershipVerbInspect,
	OwnershipVerbDecide,
	OwnershipVerbSteer,
	OwnershipVerbSpec,
	OwnershipVerbReview,
	OwnershipVerbTriage,
	OwnershipVerbAdmit,
	OwnershipVerbClose,
}

const (
	OwnershipReasonSentinelFlagged             = "sentinel_flagged"
	OwnershipReasonParkedNeedsHuman            = "parked_needs_human"
	OwnershipReasonParkedNeedsDirection        = "parked_needs_direction"
	OwnershipReasonParkedNeedsDecision         = "parked_needs_decision"
	OwnershipReasonParkedNeedsSpec             = "parked_needs_spec"
	OwnershipReasonUntrustedReporter           = "untrusted_reporter"
	OwnershipReasonReporterConfirmationStale   = "reporter_confirmation_stale"
	OwnershipReasonVerifiedDone                = "verified_done"
	OwnershipReasonAgentFiledUnacknowledged    = "agent_filed_unacknowledged"
	OwnershipReasonLevelGateHold               = "level_gate_hold"
	OwnershipReasonOutreachHold                = "outreach_hold"
	OwnershipReasonPolicyHold                  = "policy_hold"
	OwnershipReasonHoldSourceUnknown           = "hold_source_unknown"
	OwnershipReasonHivePRNeedsHuman            = "hive_pr_needs_human"
	OwnershipReasonAdjudicatorWaitElapsed      = "adjudicator_wait_elapsed"
	OwnershipReasonReviewerUnavailable         = "reviewer_unavailable"
	OwnershipReasonAdjudicatorRecommendedClose = "adjudicator_recommended_close"
	OwnershipReasonAdjudicatorReescalated      = "adjudicator_reescalated"
	OwnershipReasonPersonPRNeedsHuman          = "person_pr_needs_human"
	OwnershipReasonReviewRequiresHuman         = "review_requires_human"
	OwnershipReasonStaleSafetyNet              = "stale_safety_net"
)

// ownershipReasonVerbs is the fixed reason-code set with the one verb each
// code is produced with.
var ownershipReasonVerbs = map[string]string{
	OwnershipReasonSentinelFlagged:             OwnershipVerbInspect,
	OwnershipReasonParkedNeedsHuman:            OwnershipVerbReview,
	OwnershipReasonParkedNeedsDirection:        OwnershipVerbSteer,
	OwnershipReasonParkedNeedsDecision:         OwnershipVerbDecide,
	OwnershipReasonParkedNeedsSpec:             OwnershipVerbSpec,
	OwnershipReasonUntrustedReporter:           OwnershipVerbAdmit,
	OwnershipReasonReporterConfirmationStale:   OwnershipVerbClose,
	OwnershipReasonVerifiedDone:                OwnershipVerbClose,
	OwnershipReasonAgentFiledUnacknowledged:    OwnershipVerbTriage,
	OwnershipReasonLevelGateHold:               OwnershipVerbReview,
	OwnershipReasonOutreachHold:                OwnershipVerbReview,
	OwnershipReasonPolicyHold:                  OwnershipVerbReview,
	OwnershipReasonHoldSourceUnknown:           OwnershipVerbReview,
	OwnershipReasonHivePRNeedsHuman:            OwnershipVerbReview,
	OwnershipReasonAdjudicatorWaitElapsed:      OwnershipVerbReview,
	OwnershipReasonReviewerUnavailable:         OwnershipVerbReview,
	OwnershipReasonAdjudicatorRecommendedClose: OwnershipVerbClose,
	OwnershipReasonAdjudicatorReescalated:      OwnershipVerbReview,
	OwnershipReasonPersonPRNeedsHuman:          OwnershipVerbReview,
	OwnershipReasonReviewRequiresHuman:         OwnershipVerbReview,
	OwnershipReasonStaleSafetyNet:              OwnershipVerbReview,
}

// OwnershipActionReleaseHold is the dashboard's existing release-hold action.
// It is offered only when the level gate is a hold's only source.
const OwnershipActionReleaseHold = "release_hold"

// OwnershipActionRelease is the hub-owned typed-release action. ReleaseTag
// and ReleaseTarget carry the exact proposal the server already trusted.
const OwnershipActionRelease = "release"

const (
	// ownershipLanePRMinLevel is the lowest ACMM level at which a hive pull
	// request is covered by the safety net (no agent opens PRs at L1–L2).
	// Issues use their routed lane's real push rights instead.
	ownershipLanePRMinLevel = 3
	// ownershipAdjudicatorMinLevel is the lowest hive-wide ACMM level with an
	// adjudicator lane (L5/L6 packs).
	ownershipAdjudicatorMinLevel = 5
	// ownershipAdjudicatorWait is how long a hive needs-human PR waits for the
	// adjudicator before it becomes your turn.
	ownershipAdjudicatorWait = 4 * time.Hour

	ownershipReviewerRole = "reviewer"
)

const (
	ownershipLabelLikelyDone           = "hive/likely-done"
	ownershipLabelVerifiedOpen         = "hive/verified-open"
	ownershipLabelAlreadyDone          = "hive/already-done"
	ownershipLabelCoveredByPR          = "hive/covered-by-pr"
	ownershipLabelApprovedDirection    = "approved-direction"
	ownershipLabelReporterConfirmation = "needs-reporter-confirmation"
	ownershipLabelNeedsHuman           = config.DefaultNeedsHumanLabel
	ownershipLabelReviewerPassed       = escalation.ReviewerPassedLabel
	// ownershipLabelReviewerRecommendClose mirrors
	// scheduler.ReviewerRecommendCloseLabel; pkg/dashboard does not import the
	// scheduler.
	ownershipLabelReviewerRecommendClose = "reviewer-recommend-close"
)

// Non-actionable issue buckets recorded by enumeration (github.RepoWorkIssue).
const (
	repoWorkBucketFiltered       = "filtered"
	repoWorkBucketReporterTriage = "reporter_triage"
	repoWorkBucketClaimedByPR    = "claimed_by_pr"
)

// Ownership is the server's your-turn decision for one issue or pull
// request. YourTurn, Verb and ReasonCode are the item's ownership identity;
// Reason and Action are display only. InboxRank is the item's 1-based
// position in the "Your turn" inbox across every repository in the
// snapshot, and is zero when the item is not your turn.
//
// WaitingElsewhere marks a not-your-turn item that is gated on someone other
// than the operator: a hold whose sources include a person or the dashboard's
// pause, a hive needs-human PR still within the adjudicator wait, a pending
// reporter confirmation, an issue an open PR covers, or a blocked issue with
// an open GitHub blocked-by link. The Overview equation
// and charts read it; it is never set on a your-turn item and is not part of
// the ownership identity.
type Ownership struct {
	YourTurn         bool   `json:"your_turn"`
	Verb             string `json:"verb,omitempty"`
	ReasonCode       string `json:"reason_code,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Action           string `json:"action,omitempty"`
	ReleaseTag       string `json:"release_tag,omitempty"`
	ReleaseTarget    string `json:"release_target,omitempty"`
	InboxRank        int    `json:"inbox_rank,omitempty"`
	WaitingElsewhere bool   `json:"waiting_elsewhere,omitempty"`
	// Link points at the dashboard section that acts on the item (a run or
	// the approval desk; hivecommons/hive#12380). Display only.
	Link string `json:"link,omitempty"`
	// LabelDriven marks a your-turn issue on a repo whose un-park sweep
	// refuses `/hive approve` and `/hive decision` (label-driven triage
	// without label approval); the menu offers the triage labels instead.
	// It is not part of the ownership identity.
	LabelDriven bool `json:"label_driven,omitempty"`
}

func notYourTurn() Ownership { return Ownership{} }

// waitingElsewhere is the not-your-turn decision for an item gated on someone
// other than the operator.
func waitingElsewhere() Ownership { return Ownership{WaitingElsewhere: true} }

func yourTurn(code, reason string) Ownership {
	return Ownership{YourTurn: true, Verb: ownershipReasonVerbs[code], ReasonCode: code, Reason: reason}
}

// ownershipParking is one hard-suppress bucket and the labels that place an
// issue in it (owners can rename them; the bucket decides).
type ownershipParking struct {
	bucket string
	labels []string
}

// ownershipEnv carries everything the decision reads besides the item. It is
// built once per status refresh; every func field may be nil.
type ownershipEnv struct {
	now           time.Time
	hiveLevel     int
	staleDays     int
	sentinelLabel string
	parking       []ownershipParking

	repoLevel             func(repo string) int
	selfAuthorizationHold func(repo string) bool
	reviewerLaneAvailable func(repo string) bool
	escalatedAt           func(repo string, number int) time.Time
	// laneCanOpenPR reports whether the running agent serving a routed lane
	// may open pull requests in repo: its launched mode pushes and its default
	// mode at the repository's effective level pushes, as processing's
	// prRepoPolicyGate requires.
	laneCanOpenPR func(lane, repo string) bool
	// leased reports an unexpired contribute task lease on repo#number.
	leased func(repo string, number int) bool
	// routeLane classifies an issue whose Lane is not yet set (the scheduler
	// writes it as a side effect of building kicks). Nil leaves it empty.
	routeLane func(issue github.Issue) string
	// reviewVerdicts are the review swarm's verdict artifact entries
	// (review.LoadArtifact), the record /api/review/pipeline reads.
	reviewVerdicts []review.Aggregate
	// operatorWaits are the runs and approval-desk items waiting on the
	// operator (ownership_runs.go). Nil adds none.
	operatorWaits *operatorWaits
	// reviewSwarm is the rest of what the review pipeline reads.
	reviewSwarm reviewSwarmState
	// unparkCommandsRefused reports a repo whose sweep refuses the `/hive`
	// un-park commands (labelDrivenRefusesUnparkCommands). Nil means none.
	unparkCommandsRefused func(repo string) bool
	// releaseOnApprove reports whether typed release actions are enabled for
	// the repository. Nil keeps the dashboard action absent.
	releaseOnApprove func(repo string) bool
	// mergeLaneTakesAuthor reports whether a Hive merge lane admits PRs by
	// author in repo (automerge.Engine.MergeLaneTakesAuthor); nil turns the
	// hand-merge rule off.
	mergeLaneTakesAuthor func(repo, author string) bool
}

func (e ownershipEnv) laneCanOpenPRFor(lane, repo string) bool {
	return lane != "" && e.laneCanOpenPR != nil && e.laneCanOpenPR(lane, repo)
}

func (e ownershipEnv) leasedFor(repo string, number int) bool {
	return number > 0 && e.leased != nil && e.leased(repo, number)
}

func (e ownershipEnv) levelFor(repo string) int {
	if e.repoLevel != nil {
		if level := e.repoLevel(repo); level > 0 {
			return level
		}
	}
	return e.hiveLevel
}

func (e ownershipEnv) selfAuthorizationHoldFor(repo string) bool {
	if e.selfAuthorizationHold == nil {
		return e.hiveLevel < config.MaxACMMLevel
	}
	return e.selfAuthorizationHold(repo)
}

func (e ownershipEnv) releaseOnApproveFor(repo string) bool {
	return e.releaseOnApprove != nil && e.releaseOnApprove(repo)
}

func withReleaseAction(decision Ownership, step *github.IssueOwnerStep, enabled bool) Ownership {
	if !enabled || step == nil || step.Kind != "release" {
		return decision
	}
	if !decision.YourTurn || decision.ReasonCode != OwnershipReasonParkedNeedsDecision {
		return decision
	}
	decision.Action = OwnershipActionRelease
	decision.ReleaseTag = step.Tag
	decision.ReleaseTarget = step.Target
	return decision
}

func (e ownershipEnv) reviewerLaneAvailableFor(repo string) bool {
	return e.reviewerLaneAvailable != nil && e.reviewerLaneAvailable(repo)
}

func (e ownershipEnv) escalatedAtFor(repo string, number int) time.Time {
	if e.escalatedAt == nil {
		return time.Time{}
	}
	return e.escalatedAt(repo, number)
}

func (e ownershipEnv) staleDaysOrDefault() int {
	if e.staleDays <= 0 {
		return defaultStaleDays
	}
	return e.staleDays
}

func (e ownershipEnv) stale(createdAt, updatedAt time.Time) bool {
	return issueStale(createdAt, updatedAt, e.staleDaysOrDefault(), e.now)
}

// untouchedDays is the whole number of days since the item's last activity.
func (e ownershipEnv) untouchedDays(createdAt, updatedAt time.Time) int {
	activity := ownershipActivity(createdAt, updatedAt)
	if activity.IsZero() || e.now.Before(activity) {
		return 0
	}
	return int(e.now.Sub(activity) / (hoursPerDay * time.Hour))
}

// parkingBucket returns the hard-suppress bucket (its canonical label) the
// labels place an issue in, checked in the order needs_human,
// needs_direction, needs_decision, needs_spec.
func (e ownershipEnv) parkingBucket(labels []string) string {
	parking := e.parking
	if len(parking) == 0 {
		parking = defaultOwnershipParking()
	}
	set := labelSet(labels)
	for _, p := range parking {
		if hasAnyLabel(set, p.labels) {
			return p.bucket
		}
	}
	return ""
}

func defaultOwnershipParking() []ownershipParking {
	return ownershipParkingFor(config.HardSuppressLabelsConfig{})
}

func ownershipParkingFor(h config.HardSuppressLabelsConfig) []ownershipParking {
	return []ownershipParking{
		{bucket: config.DefaultNeedsHumanLabel, labels: h.EffectiveNeedsHuman()},
		{bucket: config.DefaultNeedsDirectionLabel, labels: h.EffectiveNeedsDirection()},
		{bucket: config.DefaultNeedsDecisionLabel, labels: h.EffectiveNeedsDecision()},
		{bucket: config.DefaultNeedsSpecLabel, labels: h.EffectiveNeedsSpec()},
	}
}

// isParkingBucket reports whether bucket names one of the four hard-suppress
// buckets (by canonical label).
func isParkingBucket(bucket string) bool {
	switch bucket {
	case config.DefaultNeedsHumanLabel, config.DefaultNeedsDirectionLabel, config.DefaultNeedsDecisionLabel, config.DefaultNeedsSpecLabel:
		return true
	}
	return false
}

// repoWorkIssueParkingBucket returns the parking bucket enumeration recorded
// on a non-actionable issue, or "" when it is not parked.
func repoWorkIssueParkingBucket(item github.RepoWorkIssue) string {
	if item.Bucket == repoWorkBucketFiltered && isParkingBucket(item.Reason) {
		return item.Reason
	}
	return ""
}

func parkedOwnership(bucket string) Ownership {
	switch bucket {
	case config.DefaultNeedsHumanLabel:
		return yourTurn(OwnershipReasonParkedNeedsHuman, "Parked for human review")
	case config.DefaultNeedsDirectionLabel:
		return yourTurn(OwnershipReasonParkedNeedsDirection, "Parked until a maintainer gives direction")
	case config.DefaultNeedsDecisionLabel:
		return yourTurn(OwnershipReasonParkedNeedsDecision, "Parked until a maintainer decides")
	case config.DefaultNeedsSpecLabel:
		return yourTurn(OwnershipReasonParkedNeedsSpec, "Parked until it has a spec")
	}
	return notYourTurn()
}

func ownershipActivity(createdAt, updatedAt time.Time) time.Time {
	if !updatedAt.IsZero() {
		return updatedAt
	}
	return createdAt
}

// holdSourcesHide reports whether a hold with these sources belongs to a
// person (their own parking) or to the dashboard's pause: never your turn.
func holdSourcesHide(sources []string) bool {
	for _, source := range sources {
		if source == github.HoldSourcePerson || source == github.HoldSourceDashboardPause {
			return true
		}
	}
	return false
}

// holdSourcesOwnership maps a held pull request's hold sources to ownership
// ("Hold sources" in #11507).
func holdSourcesOwnership(sources []string) Ownership {
	if holdSourcesHide(sources) {
		return notYourTurn()
	}
	unique := uniqueLowerLabels(sources)
	if len(unique) == 1 {
		switch unique[0] {
		case github.HoldSourceLevelGate:
			out := yourTurn(OwnershipReasonLevelGateHold, "Held by the ACMM level gate — review it, then release the hold")
			out.Action = OwnershipActionReleaseHold
			return out
		case github.HoldSourceOutreach:
			return yourTurn(OwnershipReasonOutreachHold, "Held by the outreach lane for human review")
		}
	}
	for _, source := range unique {
		if source == github.HoldSourceUnknown {
			return yourTurn(OwnershipReasonHoldSourceUnknown, "Held, and the hold's source is unknown — review it")
		}
	}
	return yourTurn(OwnershipReasonPolicyHold, "Held by Hive policy ("+strings.Join(unique, ", ")+") for human review")
}

// issueOwnershipInput is one issue row of the status snapshot: an actionable
// issue, a held issue, or a non-actionable record with its enumeration
// bucket and reason.
type issueOwnershipInput struct {
	repo         string
	number       int
	labels       []string
	acknowledged bool
	// openLinkedPR is set when the issue has an open (unmerged) linked PR or
	// open PR claim context.
	openLinkedPR bool
	// openBlockers are the issue's open GitHub blocked-by links
	// (Issue.OpenBlockers).
	openBlockers []string
	createdAt    time.Time
	updatedAt    time.Time

	// lane is the lane the classifier routed the issue to.
	lane string
	// liveClaim is true while the issue carries an unexpired ClaimedBy claim.
	liveClaim bool
	// reporterGate is true when the human-filed-bug reporter-confirmation
	// gate applies to the issue.
	reporterGate bool

	actionable  bool
	held        bool
	holdSources []string
	// band is the IssueBand band for actionable and held rows.
	band string
	// bucket and bucketReason are the enumeration's record for a
	// non-actionable row.
	bucket       string
	bucketReason string
}

// issueHasOpenLinkedPR reports whether an actionable issue carries an open
// linked PR or an open PR claim context (the duplicate-PR guard's
// weak/stale claims keep the issue actionable but still covered).
func issueHasOpenLinkedPR(issue github.Issue) bool {
	for _, link := range issue.LinkedPRs {
		if !link.Merged && strings.EqualFold(strings.TrimSpace(link.State), "open") {
			return true
		}
	}
	return issue.ClaimContext != nil && !issue.ClaimContext.MergedPR
}

// decideIssueOwnership applies the precedence rules of #11507 to an issue;
// the first matching rule decides, then the safety net applies to items that
// came out not your turn.
func decideIssueOwnership(in issueOwnershipInput, env ownershipEnv) Ownership {
	labels := labelSet(in.labels)
	// (1) a person's hold or the dashboard's pause.
	if in.held && holdSourcesHide(in.holdSources) {
		return waitingElsewhere()
	}
	// (2) sentinel-flagged.
	if env.sentinelLabel != "" && labels[strings.ToLower(env.sentinelLabel)] {
		return yourTurn(OwnershipReasonSentinelFlagged, "Flagged by the sentinel — inspect it")
	}
	// (3) covered by an open PR: the PR closes it on merge. Someone else's
	// move, and the safety net does not cover it.
	if in.bucket == repoWorkBucketClaimedByPR || in.openLinkedPR {
		return waitingElsewhere()
	}
	// (4) parking buckets.
	var parking string
	if !in.actionable && !in.held {
		// The enumeration recorded one bucket per issue by label order;
		// the ordered label check decides the verb, and the recorded
		// bucket is the fallback when the labels no longer say.
		parking = repoWorkIssueParkingBucket(github.RepoWorkIssue{Bucket: in.bucket, Reason: in.bucketReason})
		if parking != "" {
			if ordered := env.parkingBucket(in.labels); ordered != "" {
				parking = ordered
			}
		}
	} else {
		parking = env.parkingBucket(in.labels)
	}
	if parking != "" {
		return parkedOwnership(parking)
	}
	// (5) untrusted reporter awaiting an admission label.
	if in.bucket == repoWorkBucketReporterTriage {
		return yourTurn(OwnershipReasonUntrustedReporter, "Reporter is not trusted — add an admission label to let agents work on it")
	}
	// (6) awaiting the reporter's confirmation: the label or bucket, or a
	// likely-done issue the reporter-confirmation gate applies to.
	// hive/verified-open cancels likely-done: the agent found the work not
	// finished, so nothing awaits the reporter's confirmation.
	if labels[ownershipLabelReporterConfirmation] || (in.bucket == repoWorkBucketFiltered && in.bucketReason == ownershipLabelReporterConfirmation) ||
		(in.reporterGate && ownershipLikelyDone(labels)) {
		if env.stale(in.createdAt, in.updatedAt) {
			return yourTurn(OwnershipReasonReporterConfirmationStale, fmt.Sprintf("Reporter has not confirmed for %d days — close it", env.untouchedDays(in.createdAt, in.updatedAt)))
		}
		return waitingElsewhere()
	}
	// (7) verified done but not closed.
	if ownershipVerifiedDone(in, labels, env) {
		return yourTurn(OwnershipReasonVerifiedDone, "Hive verified this is done but could not close it")
	}
	// (8) agent-filed and unacknowledged while acknowledgment is required.
	if role := issueAgentRole(in.labels); role != "" && !in.acknowledged && !labels[ownershipLabelApprovedDirection] && env.selfAuthorizationHoldFor(in.repo) {
		return yourTurn(OwnershipReasonAgentFiledUnacknowledged, "Filed by agent "+role+" — acknowledge it or close it")
	}
	// (10) otherwise not your turn; the safety net may still apply.
	if issueSafetyNetCovers(in, labels, env) && env.stale(in.createdAt, in.updatedAt) {
		return safetyNetOwnership(env.untouchedDays(in.createdAt, in.updatedAt))
	}
	return notYourTurn()
}

// ownershipLikelyDone is the effective hive/likely-done: the label without
// hive/verified-open, the agent's verdict that the referenced PR did not
// finish the issue and which overrides likely-done.
func ownershipLikelyDone(labels map[string]bool) bool {
	return labels[ownershipLabelLikelyDone] && !labels[ownershipLabelVerifiedOpen]
}

func ownershipVerifiedDone(in issueOwnershipInput, labels map[string]bool, env ownershipEnv) bool {
	if labels[ownershipLabelAlreadyDone] {
		return true
	}
	if ownershipLikelyDone(labels) && (in.held || !in.actionable || !env.laneCanOpenPRFor(in.lane, in.repo)) {
		return true
	}
	return labels[ownershipLabelCoveredByPR] && !in.openLinkedPR
}

// issueSafetyNetCovers reports whether the safety net applies to an issue: an
// unheld actionable issue in the Unclaimed or Claimed band, or an actionable
// hive/likely-done issue (hive/verified-open cancels likely-done), whose
// routed lane may open PRs and which no lane has claimed (label, live claim or
// unexpired task lease).
func issueSafetyNetCovers(in issueOwnershipInput, labels map[string]bool, env ownershipEnv) bool {
	if !in.actionable || in.held || !env.laneCanOpenPRFor(in.lane, in.repo) {
		return false
	}
	if issueClaimed(in.labels) || in.liveClaim || env.leasedFor(in.repo, in.number) {
		return false
	}
	return in.band == "ready" || in.band == "in-progress" || ownershipLikelyDone(labels)
}

func safetyNetOwnership(days int) Ownership {
	return yourTurn(OwnershipReasonStaleSafetyNet, fmt.Sprintf("untouched for %d days", days))
}

// decidePROwnership applies the precedence rules of #11507 to a pull request.
func decidePROwnership(pr github.PullRequest, held bool, env ownershipEnv) Ownership {
	labels := labelSet(pr.Labels)
	var sources []string
	if held {
		sources = pr.HoldSources
		// (1) a person's hold or the dashboard's pause.
		if holdSourcesHide(sources) {
			return waitingElsewhere()
		}
	}
	// (2) sentinel-flagged.
	if env.sentinelLabel != "" && labels[strings.ToLower(env.sentinelLabel)] {
		return yourTurn(OwnershipReasonSentinelFlagged, "Flagged by the sentinel — inspect it before it merges")
	}
	// (9) a held PR is decided by its hold sources and nothing later.
	if held {
		return holdSourcesOwnership(sources)
	}
	hive := prAgentRole(pr) != ""
	adjudicated := env.hiveLevel >= ownershipAdjudicatorMinLevel
	if hive && adjudicated && labels[ownershipLabelReviewerRecommendClose] {
		return yourTurn(OwnershipReasonAdjudicatorRecommendedClose, "The adjudicator recommended closing this pull request")
	}
	withinAdjudicatorWait := false
	if labels[ownershipLabelNeedsHuman] {
		switch {
		case !hive:
			return yourTurn(OwnershipReasonPersonPRNeedsHuman, "Needs human review")
		case !adjudicated:
			return yourTurn(OwnershipReasonHivePRNeedsHuman, "Hive escalated this pull request for human review")
		case labels[ownershipLabelReviewerPassed]:
			return yourTurn(OwnershipReasonAdjudicatorReescalated, "Re-escalated after the adjudicator's pass")
		case !env.reviewerLaneAvailableFor(pr.Repo):
			return yourTurn(OwnershipReasonReviewerUnavailable, "No adjudicator is available to review this escalation")
		}
		if at := env.escalatedAtFor(pr.Repo, pr.Number); !at.IsZero() && env.now.Sub(at) >= ownershipAdjudicatorWait {
			return yourTurn(OwnershipReasonAdjudicatorWaitElapsed, fmt.Sprintf("The adjudicator has not acted within %d hours of escalation", int(ownershipAdjudicatorWait/time.Hour)))
		}
		withinAdjudicatorWait = true
	}
	// A PR the review pipeline holds for a person, for any author: no label
	// records it unless review.human_decision_label is set (#12329, #12377).
	if o, ok := env.reviewHoldOwnership(pr, withinAdjudicatorWait); ok {
		return o
	}
	// (10) otherwise not your turn; the safety net covers non-draft, unheld
	// hive PRs at L3 and above that no lane has claimed.
	if hive && !pr.Draft && env.levelFor(pr.Repo) >= ownershipLanePRMinLevel && !prClaimed(pr, env) && env.stale(pr.CreatedAt, pr.UpdatedAt) {
		return safetyNetOwnership(env.untouchedDays(pr.CreatedAt, pr.UpdatedAt))
	}
	if withinAdjudicatorWait {
		return waitingElsewhere()
	}
	return notYourTurn()
}

// prClaimed reports whether a lane holds a claim on a pull request: a claim
// label, or an unexpired task lease on the PR or on an issue it claims.
func prClaimed(pr github.PullRequest, env ownershipEnv) bool {
	if issueClaimed(pr.Labels) || env.leasedFor(pr.Repo, pr.Number) {
		return true
	}
	for _, linked := range pr.LinkedIssues {
		if env.leasedFor(nonEmpty(linked.Repo, pr.Repo), linked.Number) {
			return true
		}
	}
	return false
}

const (
	ownershipKindIssue = "issue"
	ownershipKindPR    = "pr"
)

func ownershipKey(kind string, number int) string {
	return kind + "#" + strconv.Itoa(number)
}

// ownershipFor returns the stored decision for a row, or nil when the
// snapshot carries none.
func ownershipFor(decisions map[string]Ownership, kind string, number int) *Ownership {
	if decisions == nil || number <= 0 {
		return nil
	}
	decision, ok := decisions[ownershipKey(kind, number)]
	if !ok {
		return nil
	}
	return &decision
}

// decideRepoOwnership decides every issue and pull request row of the
// snapshot's repositories, stores the decisions on each repository, and
// assigns inbox ranks across all of them.
func decideRepoOwnership(repos []FrontendRepo, cfg config.DashboardIssueBandsConfig, env ownershipEnv) {
	type ranked struct {
		repo     int
		key      string
		verb     int
		activity time.Time
		repoName string
		number   int
		kind     string
	}
	var inbox []ranked
	for i := range repos {
		repo := &repos[i]
		repoName := overviewRepoName(*repo)
		decisions := map[string]Ownership{}
		activity := map[string]time.Time{}
		urls := map[string]string{}
		record := func(kind string, number int, url string, decision Ownership, created, updated time.Time) {
			if number <= 0 {
				return
			}
			key := ownershipKey(kind, number)
			decisions[key] = env.operatorWaits.apply(kind, repoName, number, decision)
			urls[key] = url
			activity[key] = ownershipActivity(created, updated)
		}
		issueRows := func(items []any, held bool) {
			for _, raw := range items {
				issue, ok := frontendIssue(raw)
				if !ok {
					continue
				}
				issue.Repo = nonEmpty(issue.Repo, repoName)
				var sources []string
				switch v := raw.(type) {
				case github.HoldItem:
					sources = v.HoldSources
				case *github.HoldItem:
					sources = v.HoldSources
				}
				lane := issue.Lane
				if lane == "" && !held && env.routeLane != nil {
					lane = env.routeLane(issue)
				}
				_, liveClaim := issue.LiveClaim(env.now)
				in := issueOwnershipInput{
					repo:         issue.Repo,
					number:       issue.Number,
					labels:       issue.Labels,
					acknowledged: issue.HumanAcknowledged,
					openLinkedPR: issueHasOpenLinkedPR(issue),
					openBlockers: issue.OpenBlockers(),
					createdAt:    issue.CreatedAt,
					updatedAt:    issue.UpdatedAt,
					lane:         lane,
					liveClaim:    liveClaim,
					reporterGate: issue.ReporterConfirmationGate,
					actionable:   !held,
					held:         held,
					holdSources:  sources,
					band:         IssueBand(issue, held, cfg, env.now, env.selfAuthorizationHoldFor).Band,
				}
				decision := withReleaseAction(decideIssueOwnershipGated(in, env), issue.OwnerStep, env.releaseOnApproveFor(issue.Repo))
				record(ownershipKindIssue, issue.Number, issue.URL, decision, issue.CreatedAt, issue.UpdatedAt)
			}
		}
		issueRows(repo.ActionableIssues, false)
		issueRows(repo.HeldIssues, true)
		for _, raw := range repo.NonActionableIssues {
			item, ok := repoWorkIssueRow(raw)
			if !ok {
				continue
			}
			in := issueOwnershipInput{
				repo:         nonEmpty(item.Repo, repoName),
				number:       item.Number,
				labels:       item.Labels,
				acknowledged: item.HumanAcknowledged,
				createdAt:    item.CreatedAt,
				updatedAt:    item.UpdatedAt,
				reporterGate: item.ReporterConfirmationGate,
				bucket:       item.Bucket,
				bucketReason: item.Reason,
			}
			decision := withReleaseAction(decideIssueOwnershipGated(in, env), item.OwnerStep, env.releaseOnApproveFor(in.repo))
			record(ownershipKindIssue, item.Number, item.URL, decision, item.CreatedAt, item.UpdatedAt)
		}
		prRows := func(items []any, held bool) {
			for _, raw := range items {
				entry, ok := frontendPullRequest(raw)
				if !ok {
					continue
				}
				pr := entry.pr
				pr.Repo = nonEmpty(pr.Repo, repoName)
				record(ownershipKindPR, pr.Number, pr.URL, decidePROwnershipGated(pr, entry.verdict, held, cfg, env), pr.CreatedAt, pr.UpdatedAt)
			}
		}
		prRows(repo.OpenPrs, false)
		prRows(repo.HeldPrs, true)

		for key, decision := range decisions {
			if !decision.YourTurn {
				continue
			}
			kind, num, _ := strings.Cut(key, "#")
			number, _ := strconv.Atoi(num)
			inbox = append(inbox, ranked{repo: i, key: key, verb: bandRank(ownershipVerbOrder, decision.Verb), activity: activity[key], repoName: repoName, number: number, kind: kind})
		}
		markLabelDrivenReplies(decisions, repoName, env)
		repo.Ownership = decisions
		repo.ownershipURLs = urls
		repo.ACMMLevel = env.levelFor(repoName)
	}
	// Grouped by verb; within a group least-recently-active first, ties
	// broken by repository name and then item number.
	sort.Slice(inbox, func(a, b int) bool {
		x, y := inbox[a], inbox[b]
		if x.verb != y.verb {
			return x.verb < y.verb
		}
		if !x.activity.Equal(y.activity) {
			return x.activity.Before(y.activity)
		}
		if x.repoName != y.repoName {
			return x.repoName < y.repoName
		}
		if x.number != y.number {
			return x.number < y.number
		}
		return x.kind < y.kind
	})
	for rank, entry := range inbox {
		decision := repos[entry.repo].Ownership[entry.key]
		decision.InboxRank = rank + 1
		repos[entry.repo].Ownership[entry.key] = decision
	}
}

func repoWorkIssueRow(raw any) (github.RepoWorkIssue, bool) {
	switch v := raw.(type) {
	case github.RepoWorkIssue:
		return v, true
	case *github.RepoWorkIssue:
		if v == nil {
			return github.RepoWorkIssue{}, false
		}
		return *v, true
	}
	return github.RepoWorkIssue{}, false
}

// newOwnershipEnv resolves the decision's inputs from configuration and the
// agent runtime once per status refresh.
func newOwnershipEnv(cfg *config.Config, agentStatuses map[string]*agent.AgentProcess, currentMode string, now time.Time, hub *ContributeWSHub) ownershipEnv {
	env := ownershipEnv{now: now, hiveLevel: 1}
	org := ""
	if cfg != nil {
		org = cfg.Project.Org
	}
	if leased := hub.leasedWorkItems(now, org); len(leased) > 0 {
		env.leased = func(repo string, number int) bool { return leased[leaseItemKey(org, repo, number)] }
	}
	if cfg == nil {
		env.laneCanOpenPR = laneCanOpenPRAt(env, agentStatuses)
		return env
	}
	env.hiveLevel = detectACMMLevel(cfg)
	env.staleDays = cfg.Dashboard.IssueBands.StaleDays
	env.sentinelLabel = cfg.Sentinel.LabelOrDefault()
	env.parking = ownershipParkingFor(cfg.Project.IssueFilter.HardSuppressLabels)
	env.repoLevel = cfg.EffectiveACMMLevelForRepo
	env.selfAuthorizationHold = func(repo string) bool { return selfAuthorizationHoldActive(cfg, repo) }
	env.reviewerLaneAvailable = func(repo string) bool {
		return reviewerLaneAvailable(cfg, agentStatuses, currentMode, repo)
	}
	env.unparkCommandsRefused = func(repo string) bool { return labelDrivenRefusesUnparkCommands(cfg, repo) }
	env.releaseOnApprove = cfg.RepoReleaseOnApprove
	env.laneCanOpenPR = laneCanOpenPRAt(env, agentStatuses)
	return env
}

// laneCanOpenPRAt builds the lane push-rights check: the lane's agent is
// running in a mode that pushes, and its default mode at the repository's
// effective level (which a repository policy can only lower) pushes too.
func laneCanOpenPRAt(env ownershipEnv, agentStatuses map[string]*agent.AgentProcess) func(lane, repo string) bool {
	return func(lane, repo string) bool {
		name := strings.ToLower(strings.TrimSpace(lane))
		proc, ok := agentStatuses[name]
		return ok && proc != nil && proc.State == agent.StateRunning && proc.LaunchedMode.CanPush() &&
			agent.DefaultAgentMode(name, env.levelFor(repo)).CanPush()
	}
}

// selfAuthorizationHoldActive resolves the self-authorization hold as
// processing does: at the hive-wide ACMM level, honouring a repository's own
// override.
func selfAuthorizationHoldActive(cfg *config.Config, repo string) bool {
	if cfg == nil {
		return true
	}
	return cfg.SelfAuthorizationHoldEnabledForRepoAtLevel(repo, cfg.ACMMLevelOrZero())
}

// reviewerLaneAvailable reports whether an agent that runs the reviewer lane
// (role reviewer, no kick_template — the adjudicator in the L5/L6 packs)
// serves repo and is neither paused nor unscheduled in the current mode. The
// ADVISORY reviewer, which has a kick_template, does not count.
func reviewerLaneAvailable(cfg *config.Config, agentStatuses map[string]*agent.AgentProcess, currentMode, repo string) bool {
	if cfg == nil {
		return false
	}
	for name, agentCfg := range cfg.Agents {
		if !agentCfg.Enabled || !strings.EqualFold(strings.TrimSpace(agentCfg.Role), ownershipReviewerRole) || strings.TrimSpace(agentCfg.KickTemplate) != "" {
			continue
		}
		if !cfg.AgentServesRepo(name, repo) {
			continue
		}
		if agentCfg.Paused {
			continue
		}
		if proc, ok := agentStatuses[name]; ok && proc != nil && proc.Paused {
			continue
		}
		// An explicit pause/off cadence in the current mode means the
		// governor never kicks the agent (the status builder's offByCadence).
		if cadence := cfg.CadenceValueForMode(name, currentMode); cadence != "" && cadence.IsPaused() && !agentCfg.OnDemand {
			continue
		}
		if unscheduled, _ := agentUnscheduledInMode(cfg, name, currentMode, agentCfg.Enabled, agentCfg.OnDemand, agentCfg.UsesGovernorKick()); unscheduled {
			continue
		}
		return true
	}
	return false
}

// ownershipStatePath is Hive's local record for the your-turn decision: the
// first status refresh that saw each hive PR with needs-human, so the
// adjudicator wait survives a restart when the escalation ledger has no
// label-applied time for the PR.
var ownershipStatePath = "/data/ownership-state.json"

// ownershipEscalationLedgerPath is the escalation ledger the escalation sweep
// writes; read only.
var ownershipEscalationLedgerPath = escalation.DefaultLedgerPath

var ownershipStateMu sync.Mutex

type ownershipState struct {
	EscalationFirstSeen map[string]time.Time `json:"escalation_first_seen,omitempty"`
	// Identities is each item's last ownership identity; SeededRepos is the
	// set of repositories whose identities have been stored by a refresh
	// that had complete data for them.
	Identities  map[string]ownershipIdentity `json:"identities,omitempty"`
	SeededRepos map[string]bool              `json:"seeded_repos,omitempty"`
}

func loadOwnershipState(path string) ownershipState {
	state := ownershipState{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	if state.EscalationFirstSeen == nil {
		state.EscalationFirstSeen = map[string]time.Time{}
	}
	return state
}

func saveOwnershipState(path string, state ownershipState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// recordEscalationFirstSeen updates the local record of when each hive
// needs-human PR was first seen, and returns the escalation time lookup: the
// ledger's label-applied time when it has one, else the first-seen time. It
// runs only in the status refresh, never in a request handler.
func recordEscalationFirstSeen(repos []FrontendRepo, now time.Time) func(repo string, number int) time.Time {
	ownershipStateMu.Lock()
	defer ownershipStateMu.Unlock()
	state := loadOwnershipState(ownershipStatePath)
	seen := map[string]bool{}
	complete := map[string]bool{}
	changed := false
	for _, repo := range repos {
		repoName := overviewRepoName(repo)
		if ownershipRowsComplete(repo) {
			complete[repo.Full] = true
			complete[repo.Name] = true
		}
		for _, raw := range repo.OpenPrs {
			entry, ok := frontendPullRequest(raw)
			if !ok || prAgentRole(entry.pr) == "" || !labelSet(entry.pr.Labels)[ownershipLabelNeedsHuman] {
				continue
			}
			key := escalation.Key(nonEmpty(entry.pr.Repo, repoName), entry.pr.Number)
			seen[key] = true
			if _, ok := state.EscalationFirstSeen[key]; !ok {
				state.EscalationFirstSeen[key] = now
				changed = true
			}
		}
	}
	for key := range state.EscalationFirstSeen {
		i := strings.LastIndex(key, "#")
		if seen[key] || i < 0 || !complete[key[:i]] {
			continue
		}
		delete(state.EscalationFirstSeen, key)
		changed = true
	}
	if changed {
		if err := saveOwnershipState(ownershipStatePath, state); err != nil {
			slog.Warn("could not save ownership state", "path", ownershipStatePath, "error", err)
		}
	}
	firstSeen := state.EscalationFirstSeen
	ledger := escalation.Load(ownershipEscalationLedgerPath)
	return func(repo string, number int) time.Time {
		if at, ok := ledger.LabelAppliedAt(repo, number); ok {
			return at
		}
		return firstSeen[escalation.Key(repo, number)]
	}
}

// applyOwnershipDecisions computes the your-turn decision for every row of a
// freshly built snapshot, using the snapshot's generated time as now.
func applyOwnershipDecisions(cfg *config.Config, repos []FrontendRepo, agentStatuses map[string]*agent.AgentProcess, currentMode string, now time.Time, hub *ContributeWSHub, adjust ...func(*ownershipEnv)) {
	env := newOwnershipEnv(cfg, agentStatuses, currentMode, now, hub)
	for _, fn := range adjust {
		fn(&env)
	}
	env.routeLane = classify.KeywordLane
	env.loadReviewSwarm()
	if env.hiveLevel >= ownershipAdjudicatorMinLevel {
		env.escalatedAt = recordEscalationFirstSeen(repos, now)
	}
	bandsCfg := config.DashboardIssueBandsConfig{}
	if cfg != nil {
		bandsCfg = cfg.Dashboard.IssueBands
	}
	org := ""
	if cfg != nil {
		org = cfg.Project.Org
	}
	env.operatorWaits = collectOperatorWaits(hub, org)
	decideRepoOwnership(repos, bandsCfg, env)
	recordOwnershipTransitions(repos, now)
}
