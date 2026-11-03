package automerge

import (
	"context"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/evidence"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// Review gate modes (governor.review_gate.mode, hivecommons/hive#12592).
const (
	ReviewGateModeOff     = "off"
	ReviewGateModeShadow  = "shadow"
	ReviewGateModeEnforce = "enforce"
)

// Review gate outcomes for one head. changes-requested is reported as
// "changes-requested:<perspective,...>".
const (
	ReviewGateOutcomeNoBundle         = prstage.ReviewGateOutcomeNoBundle
	ReviewGateOutcomeAwaiting         = prstage.ReviewGateOutcomeAwaiting
	ReviewGateOutcomeChangesRequested = prstage.ReviewGateOutcomeChangesRequested
	ReviewGateOutcomeApproved         = "approved"
	ReviewGateOutcomeHumanApproved    = "human-approved"
	// ReviewGateOutcomeRequiresHuman is an aggregate requires_human /
	// decision-requested verdict: the same head the contributor lane and the
	// 085 stage report as needing a decision. Before it existed the gate
	// reclassified such a head from its perspective map, so one lane said
	// requires_human while the merge gate said changes-requested:<p>
	// (rationguard#296).
	ReviewGateOutcomeRequiresHuman = "requires-human"
)

// autoMergeReasonReviewGatePrefix prefixes the sweep skip reason when an
// enforcing review gate holds a self-merge: "review-gate:<outcome>".
const autoMergeReasonReviewGatePrefix = prstage.AutoMergeReasonReviewGatePrefix

// ReviewGateVerdict is the swarm review aggregate for one PR head, read from
// review-verdicts.json by the same loader the human merge lane uses.
type ReviewGateVerdict struct {
	// Loaded is false when review-verdicts.json could not be read.
	Loaded bool
	// Found is true when the artifact holds an aggregate for the head.
	Found bool
	// Approved is review.Artifact.HasAggregateApproval for the head.
	Approved bool
	// Verdict is the normalized aggregate verdict for the head (approve,
	// changes_requested, requires_human, reject, incomplete). It is the
	// source of truth for the outcome; "" (a verdict built without it) falls
	// back to the perspective map.
	Verdict string
	// ChangesRequested names the perspectives whose verdict on the head is
	// changes_requested or reject.
	ChangesRequested []string
}

// ReviewGateVerdictFunc resolves the swarm verdict for repo#number at
// headSHA. repo is owner/repo.
type ReviewGateVerdictFunc func(repo string, number int, headSHA string) ReviewGateVerdict

// ReviewGateEvaluation is one evaluation of the review gate on the
// self-merge lane, for the audit log. Recording only.
type ReviewGateEvaluation struct {
	Lane          string
	Repo          string
	Number        int
	HeadSHA       string
	Outcome       string
	WouldHold     bool
	Enforced      bool
	HumanApprover string
	// Overrode is the swarm outcome a trusted human's approval on the exact
	// head replaced (changes-requested:<p>, requires-human, …), so the audit
	// line records who overrode what. Empty unless Outcome is human-approved
	// and the swarm had not approved.
	Overrode string
}

// ReviewGate is the swarm-review policy the self-authored sweep evaluates on
// the App's own PRs immediately before the merge call (#12592). It mirrors
// IntentGate (#6258): the human merge lane only queues a PR whose head has an
// aggregate approve in review-verdicts.json when review.require_approval is
// on (writeMergeEligible), but the self-authored sweep lists the App's PRs on
// its own and, before this gate, never read that artifact — so an agent PR
// could merge before the swarm had reviewed its head at all, or over a
// perspective's changes_requested.
//
// Every knob is read on every evaluation so a config reload applies without
// a restart. The effective mode is the strictest of:
//
//   - governor.review_gate.mode (Mode): off | shadow | enforce, default shadow;
//   - review.require_approval (RequireApproval): true enforces, as it does on
//     the human lane;
//   - review.shadow (Shadow): true records, as it does on the human lane.
//
// Shadow records the outcome (RecordShadow, RecordEvaluation) and merges as
// before; enforce withholds the merge unless the head is approved. Off with
// both review knobs false is byte-identical to the sweep before #12592. A nil
// *ReviewGate on the Engine installs no policy.
type ReviewGate struct {
	// Mode reports governor.review_gate.mode. nil or "" means shadow.
	Mode func() string
	// RequireApproval reports review.require_approval. nil is false.
	RequireApproval func() bool
	// Shadow reports review.shadow. nil is false.
	Shadow func() bool
	// Verdict resolves the swarm verdict for a head. nil reads as no bundle.
	Verdict ReviewGateVerdictFunc
	// RecordShadow persists a would-have-held observation on the head's
	// evidence bundle. Recording only; nil disables it.
	RecordShadow func(fullRepo string, number int, head string, gate evidence.ShadowGate)
	// RecordEvaluation writes the review_evaluated audit line. Recording
	// only; nil disables it.
	RecordEvaluation func(ReviewGateEvaluation)
}

// NormalizeReviewGateMode maps a configured mode to one of the three modes;
// "" and unknown values read as shadow (the default).
func NormalizeReviewGateMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ReviewGateModeOff:
		return ReviewGateModeOff
	case ReviewGateModeEnforce:
		return ReviewGateModeEnforce
	default:
		return ReviewGateModeShadow
	}
}

// effectiveMode folds the gate's own mode with review.require_approval and
// review.shadow: the strictest wins, so neither review knob is ever weakened
// by governor.review_gate.mode.
func (g *ReviewGate) effectiveMode() string {
	if g == nil {
		return ReviewGateModeOff
	}
	mode := ReviewGateModeShadow
	if g.Mode != nil {
		mode = NormalizeReviewGateMode(g.Mode())
	}
	if g.RequireApproval != nil && g.RequireApproval() {
		return ReviewGateModeEnforce
	}
	if mode == ReviewGateModeOff && g.Shadow != nil && g.Shadow() {
		return ReviewGateModeShadow
	}
	return mode
}

// reviewGateOutcome classifies a verdict into one of the swarm outcomes.
// An unreadable artifact counts as no bundle: the gate cannot show the
// swarm reviewed the head, so it fails closed when enforcing.
//
// The aggregate verdict decides the class when it is known: the contributor
// lane (contributorLaneVerdictFor) and the 085 stage read the same aggregate,
// and a head must not be requires_human to one lane and changes-requested to
// the merge gate. The perspective list only names who asked for changes.
func reviewGateOutcome(v ReviewGateVerdict) string {
	if !v.Loaded || !v.Found {
		return ReviewGateOutcomeNoBundle
	}
	changes := func() string {
		if len(v.ChangesRequested) == 0 {
			return ReviewGateOutcomeChangesRequested
		}
		persp := append([]string(nil), v.ChangesRequested...)
		sort.Strings(persp)
		return ReviewGateOutcomeChangesRequested + ":" + strings.Join(persp, ",")
	}
	switch strings.ToLower(strings.TrimSpace(v.Verdict)) {
	case "requires_human":
		return ReviewGateOutcomeRequiresHuman
	case "changes_requested", "reject", "rejected", "request_changes":
		return changes()
	case "approve", "approved":
		if v.Approved {
			return ReviewGateOutcomeApproved
		}
		return ReviewGateOutcomeAwaiting
	}
	switch {
	case len(v.ChangesRequested) > 0:
		return changes()
	case v.Approved:
		return ReviewGateOutcomeApproved
	default:
		return ReviewGateOutcomeAwaiting
	}
}

// SetReviewGate installs (or, with nil, removes) the review gate consulted
// by trySweepSelfAuthoredPR.
func (c *Engine) SetReviewGate(gate *ReviewGate) {
	if c == nil {
		return
	}
	c.reviewGateMu.Lock()
	defer c.reviewGateMu.Unlock()
	c.reviewGate = gate
}

func (c *Engine) currentReviewGate() *ReviewGate {
	if c == nil {
		return nil
	}
	c.reviewGateMu.RLock()
	defer c.reviewGateMu.RUnlock()
	return c.reviewGate
}

// reviewEvidenceStarter is implemented by a transport that can start the
// evidence bundle for a head from the PR snapshot (hgithub.Client), so a
// shadow observation, the CI snapshot and the merge always have a bundle to
// land on even when the swarm has not written one yet.
type reviewEvidenceStarter interface {
	StartReviewEvidence(repo string, number int, head string, pr *gh.PullRequest)
}

// mergeReviewGate evaluates the review gate for a PR head immediately before
// an automerge lane's merge call. It returns "" when the merge may proceed
// and a skip reason ("review-gate:<outcome>") when an enforcing gate holds
// it. Every sweep lane is gated — self-authored, trusted-bot and
// trusted-author — because the swarm reviews every open PR regardless of
// author whenever review is on (hivecommons/hive#12974); before that, the
// gate covered the self-authored lane only and a human's PR could merge
// through the trusted-author tier with no swarm verdict at all (#12966).
// priorChecked reports that the caller already looked up the trusted human
// approval for head (priorApprover is its result), so the reviews are not
// listed again.
func (c *Engine) mergeReviewGate(ctx context.Context, displayRepo, owner, repo, lane string, number int, pr *gh.PullRequest, head string, priorApprover string, priorChecked bool) string {
	gate := c.currentReviewGate()
	if gate == nil {
		return ""
	}
	mode := gate.effectiveMode()
	if mode == ReviewGateModeOff {
		return ""
	}
	fullRepo := owner + "/" + repo
	var verdict ReviewGateVerdict
	if gate.Verdict != nil {
		verdict = gate.Verdict(fullRepo, number, head)
	}
	outcome := reviewGateOutcome(verdict)
	enforce := mode == ReviewGateModeEnforce
	humanApprover, overrode := "", ""
	if enforce && outcome != ReviewGateOutcomeApproved {
		// A trusted human's APPROVED review on exactly this head outranks
		// every swarm outcome, changes-requested included: the person has
		// read the reviewer's objection and decided the head lands as-is.
		// Who overrode what is recorded on the evaluation.
		approver, ok := priorApprover, priorApprover != ""
		if !priorChecked {
			approver, ok = c.latestTrustedHumanApproval(ctx, owner, repo, number, head)
		}
		if ok {
			humanApprover = approver
			overrode = outcome
			outcome = ReviewGateOutcomeHumanApproved
		}
	}
	wouldHold := outcome != ReviewGateOutcomeApproved
	if outcome == ReviewGateOutcomeHumanApproved {
		wouldHold = false
	}
	if starter, ok := c.transport.(reviewEvidenceStarter); ok {
		starter.StartReviewEvidence(fullRepo, number, head, pr)
	}
	recordLane := reviewGateRecordLane(lane)
	if gate.RecordEvaluation != nil {
		gate.RecordEvaluation(ReviewGateEvaluation{
			Lane: recordLane, Repo: fullRepo, Number: number, HeadSHA: head,
			Outcome: outcome, WouldHold: wouldHold, Enforced: enforce, HumanApprover: humanApprover, Overrode: overrode,
		})
	}
	if enforce {
		if wouldHold {
			c.info("automerge sweep review gate holding merge", "lane", lane, "repo", displayRepo, "pr", number, "head", head, "outcome", outcome)
			return autoMergeReasonReviewGatePrefix + outcome
		}
		if humanApprover != "" {
			c.info("automerge sweep review gate cleared by trusted human approval", "lane", lane, "repo", displayRepo, "pr", number, "head", head, "approver", humanApprover, "overrode", overrode)
		}
		return ""
	}
	if gate.RecordShadow != nil {
		gate.RecordShadow(fullRepo, number, head, evidence.ShadowGate{
			WouldHaveHeld: &wouldHold,
			Reason:        outcome,
			Lane:          recordLane,
			Confidence:    1,
			EvaluatedAt:   time.Now(),
		})
	}
	return ""
}

// reviewGateRecordLane names the lane on the audit line and the evidence
// bundle. The App's own PRs keep the historical "self-merge" so existing
// shadow-gate rollups (review_gate_by_lane.self_merge) stay continuous; the
// other sweep lanes record under their own name.
func reviewGateRecordLane(lane string) string {
	if lane == stageLaneSelf {
		return intentLaneSelfMerge
	}
	return lane
}

func (c *Engine) latestTrustedHumanApproval(ctx context.Context, owner, repo string, number int, head string) (string, bool) {
	rv, ok := c.latestTrustedHumanReview(ctx, owner, repo, number, head, "APPROVED")
	if !ok {
		return "", false
	}
	login := strings.TrimSpace(hgithub.SafeGetLogin(rv.GetUser()))
	c.noteStageTrustedHumanApproval(owner, repo, number, head, login)
	return login, true
}

func (c *Engine) trustedHumanApprovalForRequiresHuman(ctx context.Context, owner, repo string, number int, head string, verdict ContributorLaneVerdict) (string, bool) {
	if !verdictNeedsHuman(stageDetail{Verdict: verdict, VerdictKnown: true}) {
		return "", false
	}
	if login, ok := c.latestTrustedHumanApproval(ctx, owner, repo, number, head); ok {
		return login, true
	}
	// No live trusted approval on head: a persisted review-backed approval
	// is revoked, so drop it before reading the recorded decision.
	c.reconcileReviewBackedApproval(ctx, owner, repo, number, head)
	return c.recordedHumanApprovalForHead(ctx, owner, repo, number, head)
}

// recordedHumanApprovalForHead is a bare `/hive decision approve` the un-park
// sweep recorded against head (pr_unpark_decision.go): the owner answered the
// swarm's question with "yes" in a comment rather than a GitHub review. It
// releases requires_human the way an APPROVED review on the head does
// (hivecommons/hive#14679). An approval recorded on the previous head
// follows the PR when the hive's own base refresh is all that moved it
// (hivecommons/hive#15649).
func (c *Engine) recordedHumanApprovalForHead(ctx context.Context, owner, repo string, number int, head string) (string, bool) {
	if c == nil || c.humanChangeRequests == nil || strings.TrimSpace(head) == "" {
		return "", false
	}
	req, ok := c.recordedDecisionFor(ctx, owner, repo, number, "", head)
	if !ok || !req.IsApproval() {
		return "", false
	}
	login := strings.ToLower(strings.TrimSpace(req.Actor))
	c.noteStageTrustedHumanApproval(owner, repo, number, head, login)
	return login, true
}
