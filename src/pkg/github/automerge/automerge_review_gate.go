package automerge

import (
	"context"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/evidence"
	hgithub "github.com/hivecommons/hive/pkg/github"
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
	ReviewGateOutcomeNoBundle         = "no-review-bundle"
	ReviewGateOutcomeAwaiting         = "awaiting-review-approval"
	ReviewGateOutcomeChangesRequested = "changes-requested"
	ReviewGateOutcomeApproved         = "approved"
	ReviewGateOutcomeHumanApproved    = "human-approved"
)

// autoMergeReasonReviewGatePrefix prefixes the sweep skip reason when an
// enforcing review gate holds a self-merge: "review-gate:<outcome>".
const autoMergeReasonReviewGatePrefix = "review-gate:"

// ReviewGateVerdict is the swarm review aggregate for one PR head, read from
// review-verdicts.json by the same loader the human merge lane uses.
type ReviewGateVerdict struct {
	// Loaded is false when review-verdicts.json could not be read.
	Loaded bool
	// Found is true when the artifact holds an aggregate for the head.
	Found bool
	// Approved is review.Artifact.HasAggregateApproval for the head.
	Approved bool
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
func reviewGateOutcome(v ReviewGateVerdict) string {
	switch {
	case !v.Loaded || !v.Found:
		return ReviewGateOutcomeNoBundle
	case len(v.ChangesRequested) > 0:
		persp := append([]string(nil), v.ChangesRequested...)
		sort.Strings(persp)
		return ReviewGateOutcomeChangesRequested + ":" + strings.Join(persp, ",")
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

// selfMergeReviewGate evaluates the review gate for an App-authored PR head
// immediately before the merge call. It returns "" when the merge may
// proceed and a skip reason ("review-gate:<outcome>") when an enforcing gate
// holds it. Only the self-authored lane is gated; trusted-bot PRs are not
// reviewed by the swarm and pass untouched.
func (c *Engine) selfMergeReviewGate(ctx context.Context, displayRepo, owner, repo, lane string, number int, pr *gh.PullRequest, head string) string {
	gate := c.currentReviewGate()
	if gate == nil || lane != stageLaneSelf {
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
	humanApprover := ""
	if enforce && outcome != ReviewGateOutcomeApproved && !strings.HasPrefix(outcome, ReviewGateOutcomeChangesRequested) {
		if approver, ok := c.latestTrustedHumanApproval(ctx, owner, repo, number, head); ok {
			humanApprover = approver
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
	if gate.RecordEvaluation != nil {
		gate.RecordEvaluation(ReviewGateEvaluation{
			Lane: intentLaneSelfMerge, Repo: fullRepo, Number: number, HeadSHA: head,
			Outcome: outcome, WouldHold: wouldHold, Enforced: enforce, HumanApprover: humanApprover,
		})
	}
	if enforce {
		if wouldHold {
			c.info("self-authored automerge sweep review gate holding merge", "repo", displayRepo, "pr", number, "head", head, "outcome", outcome)
			return autoMergeReasonReviewGatePrefix + outcome
		}
		if humanApprover != "" {
			c.info("self-authored automerge sweep review gate cleared by trusted human approval", "repo", displayRepo, "pr", number, "head", head, "approver", humanApprover)
		}
		return ""
	}
	if gate.RecordShadow != nil {
		gate.RecordShadow(fullRepo, number, head, evidence.ShadowGate{
			WouldHaveHeld: &wouldHold,
			Reason:        outcome,
			Lane:          intentLaneSelfMerge,
			Confidence:    1,
			EvaluatedAt:   time.Now(),
		})
	}
	return ""
}

func (c *Engine) latestTrustedHumanApproval(ctx context.Context, owner, repo string, number int, head string) (string, bool) {
	if c == nil || c.gh == nil {
		return "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	opts := &gh.ListOptions{PerPage: 100}
	var seen []*gh.PullRequestReview
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			c.warn("self-merge review gate could not read PR reviews for human approval", "owner", owner, "repo", repo, "pr", number, "error", err)
			return "", false
		}
		seen = append(seen, reviews...)
		for _, review := range reviews {
			if !strings.EqualFold(review.GetState(), "APPROVED") || strings.TrimSpace(review.GetCommitID()) != head {
				continue
			}
			login := strings.TrimSpace(hgithub.SafeGetLogin(review.GetUser()))
			if login == "" || c.isHiveAppReviewAuthor(review) || strings.HasSuffix(strings.ToLower(login), "[bot]") {
				continue
			}
			trusted, configured := c.isTrustedMerger(login)
			if configured && trusted {
				c.noteStageReviews(owner, repo, number, seen)
				return login, true
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	c.noteStageReviews(owner, repo, number, seen)
	return "", false
}
