package automerge

// The trusted-bot lane merges PRs authored by the logins the operator enabled
// under auto_merge.trusted_bot_authors (Settings → bot toggles, any custom
// login). Its contract is "safety is established by CI": the review swarm's
// verdict is advisory unless the operator turns
// auto_merge.trusted_bot_skip_review off, and a required check that
// auto_merge.required_checks names but the repo has never produced is
// surfaced by name instead of parking the PR at a bare "needs a person".

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

const (
	// laneTrustedBot is the sweepLaneForAuthor lane for TrustedBotAuthors.
	laneTrustedBot = "trusted-bot"
	// requiredCheckNeverReportedPrefix: see prstage.RequiredCheckNeverReportedPrefix.
	requiredCheckNeverReportedPrefix = prstage.RequiredCheckNeverReportedPrefix
	// requiredCheckNeverReportedWarnEvery bounds the operator warning to one
	// line per repo per check per hour.
	requiredCheckNeverReportedWarnEvery = time.Hour
	// trustedBotAdvisoryVerdict is what the stage tracker records in place of
	// a swarm verdict the trusted-bot lane treats as advisory.
	trustedBotAdvisoryVerdict = "advisory"
)

// trustedBotReviewAdvisory reports whether the review swarm's verdict is
// advisory on lane: only the trusted-bot lane, and only while
// auto_merge.trusted_bot_skip_review is on (the default).
func (c *Engine) trustedBotReviewAdvisory(lane string) bool {
	if c == nil || lane != laneTrustedBot {
		return false
	}
	return c.trustedBotSkipReview == nil || c.trustedBotSkipReview()
}

// trustedBotLaneVerdict downgrades a swarm verdict that would hold or route
// a trusted-bot PR to a person (requires_human, decision-requested,
// changes_requested, reject) to an advisory one. The verdict itself stays
// visible: the review comment is on the PR and the downgrade is logged once
// per evaluation. Verdicts a person already answered are left alone.
func (c *Engine) trustedBotLaneVerdict(displayRepo string, number int, head, lane string, v ContributorLaneVerdict) ContributorLaneVerdict {
	if !c.trustedBotReviewAdvisory(lane) || !v.Configured || v.Approved || v.DecisionAnswered {
		return v
	}
	d := stageDetail{Verdict: v, VerdictKnown: true}
	if !verdictNeedsHuman(d) && !verdictBlocks(d) {
		return v
	}
	original := strings.ToLower(strings.TrimSpace(v.Verdict))
	if original == "" && v.DecisionRequested {
		original = "decision-requested"
	}
	c.info("trusted-bot lane: review verdict advisory", "tier", lane, "repo", displayRepo, "pr", number, "head", head, "verdict", original, "reason", "trusted_bot_skip_review")
	v.Verdict = trustedBotAdvisoryVerdict
	v.DecisionRequested = false
	v.DecisionQuestion = ""
	v.DecisionFinding = ""
	v.FixCycleCapReached = false
	return v
}

// reviewVerdictGateForLane is reviewVerdictGate with the trusted-bot lane's
// advisory contract applied: when the swarm's verdict holds a PR authored by
// an auto_merge.trusted_bot_authors login and trusted_bot_skip_review is on
// (the default), the hold is logged with the lane tier and the merge
// proceeds; safety is the repo's CI. Every other lane, and the trusted-bot
// lane with the knob off, is gated exactly as before.
func (c *Engine) reviewVerdictGateForLane(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, lane string) (string, error) {
	if c == nil || !c.trustedBotReviewAdvisory(lane) || c.reviewVerdictHold == nil {
		return c.reviewVerdictGate(ctx, displayRepo, owner, repo, pr)
	}
	number := pr.GetNumber()
	hold := c.reviewVerdictHold(owner+"/"+repo, number)
	if !hold.Held {
		return "", nil
	}
	c.info("trusted-bot lane: review verdict advisory", "tier", lane, "repo", displayRepo, "pr", number, "head", pr.GetHead().GetSHA(), "verdict", hold.Verdict, "verdict_head", hold.HeadSHA, "reason", "trusted_bot_skip_review")
	return "", nil
}

// recheckRequiredCheckNeverReported re-evaluates a `required-check-missing:`
// head on the trusted-bot lane when the required set came from
// auto_merge.required_checks and the named check has never reported on this
// head: the config names a check this repo does not run (e.g. "build-gate"
// declared for console, absent on docs). With at least one other check
// reporting and none red, the head is gated on "every reported check green"
// instead; with nothing else to vouch for it, the hold is surfaced as
// required-check-never-reported:<name> so the human gate names the cause.
// Both outcomes log once per repo per check per hour. The verdict is
// returned unchanged when the shape does not apply.
func (c *Engine) recheckRequiredCheckNeverReported(ctx context.Context, displayRepo, owner, repo, branch string, pr *gh.PullRequest, number int, headSHA string, headPushedAt time.Time, ci hgithub.CommitCIState, reason string) (bool, string, error) {
	if c == nil || !strings.HasPrefix(reason, requiredCheckMissingPrefix) || !ci.RequiredFromConfig || len(ci.MissingRequired) == 0 {
		return false, reason, nil
	}
	if lane, ok := c.sweepLaneForAuthor(hgithub.SafeGetLogin(pr.GetUser())); !ok || lane != laneTrustedBot {
		return false, reason, nil
	}
	missing := make(map[string]bool, len(ci.MissingRequired))
	for _, name := range ci.MissingRequired {
		missing[name] = true
	}
	observedOther := 0
	for name := range ci.Observed {
		if !missing[name] {
			observedOther++
		}
	}
	first := ci.MissingRequired[0]
	if observedOther == 0 {
		c.warnRequiredCheckNeverReported(owner, repo, first, "no other check reports on the head")
		return false, requiredCheckNeverReportedPrefix + first, nil
	}
	st, _, err := c.commitCIForPRSkipping(ctx, owner, repo, branch, headSHA, number, headPushedAt, expectedCheckCacheFromContext(ctx), missing)
	if err != nil {
		return false, st.Reason, err
	}
	if !st.Green {
		return false, st.Reason, nil
	}
	// The required set is now effectively empty, so the walk skipped every
	// other red check as non-required; "all reported checks green" means
	// those block here.
	if len(st.FailingNonRequired) > 0 {
		return false, "required-check-failing:" + st.FailingNonRequired[0], nil
	}
	if st.InFlight > 0 {
		return false, "check-pending", nil
	}
	c.warnRequiredCheckNeverReported(owner, repo, first, "gating on every reported check instead")
	c.info("trusted-bot lane: required check never reported; every reported check green", "tier", laneTrustedBot, "repo", displayRepo, "pr", number, "sha", headSHA, "check", first, "observed", observedOther)
	return true, "", nil
}

// warnRequiredCheckNeverReported logs
// `trusted-bot lane: required check "<name>" never reported on <repo>` at
// most once per repo per check per requiredCheckNeverReportedWarnEvery.
func (c *Engine) warnRequiredCheckNeverReported(owner, repo, name, action string) {
	if c == nil {
		return
	}
	key := strings.ToLower(owner + "/" + repo + ":" + name)
	now := c.now()
	c.requiredChecksFallbackWarnedMu.Lock()
	last, seen := c.requiredCheckNeverReportedWarned[key]
	if seen && now.Sub(last) < requiredCheckNeverReportedWarnEvery {
		c.requiredChecksFallbackWarnedMu.Unlock()
		return
	}
	c.requiredCheckNeverReportedWarned[key] = now
	c.requiredChecksFallbackWarnedMu.Unlock()
	c.warn(`trusted-bot lane: required check "`+name+`" never reported on `+owner+"/"+repo, "repo", owner+"/"+repo, "check", name, "action", action, "hint", "set auto_merge.repos."+repo+".required_checks to this repo's real required checks")
}

// selfAuthoredSweepTier names the self-authored sweep's lane for pr on its
// log lines: "trusted-bot" for an operator-enabled bot login, "self-authored"
// for the App's own PRs, "" when neither (the prefilter then skips it).
func (c *Engine) selfAuthoredSweepTier(pr *gh.PullRequest) string {
	if c == nil || pr == nil {
		return ""
	}
	lane, ok := c.sweepLaneForAuthor(hgithub.SafeGetLogin(pr.GetUser()))
	if !ok {
		return ""
	}
	return lane
}
