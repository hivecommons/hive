package automerge

// Merge-request relay gate.
//
// Agents land PRs through the merge-request relay (hive-merge → the
// merge-request watcher → MergePR as the App). The relay's own gates are the
// agent's authority, the governor's merge-eligible list at a pinned head,
// labels and positive CI confirmation. None of those is the CONTRIBUTOR-LANE
// policy: who the author is, which tier the operator granted them, whether
// the head soaked. Live on hivecommons/docs#401 (2026-10-20) an
// untrusted-tier contributor's PR, green at stage/060 with
// contributor-lane:claim-tier-untrusted on it, was merged within minutes
// because a scanner asked the relay — the lane that would have refused it
// never got a say. RelayMergeRefusal gives it one: the relay asks this
// engine, and the engine answers with the same evaluation the sweep runs.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

// RelayMergeRefusalPrefix starts every contributor-lane refusal the relay
// records, so the result file, the audit trail and the log can be grepped
// for one thing.
const RelayMergeRefusalPrefix = "contributor-lane policy refuses the merge relay"

// relayLaneReasonDisabled / relayLaneReasonRepo are the relay-only skip
// reasons for a lane the operator turned off or scoped to other repos: the
// sweep never evaluates such a PR, so it has no reason of its own.
const (
	relayLaneReasonDisabled = contributorLaneReasonPrefix + "disabled"
	relayLaneReasonRepo     = contributorLaneReasonPrefix + "repo-not-allowed"
	relayLaneReasonHead     = "head-changed-since-request"
)

// RelayMergeRefusal decides whether the merge-request relay may land
// displayRepo#number at expectSHA under the lane policy the auto-merge
// sweeps apply. It returns "" when the merge may proceed, a refusal naming
// the gate otherwise, and an error when the evidence could not be read (the
// relay fails closed on it through its bounded retry path).
//
// The merge may proceed when the PR author is the App itself or an
// operator-listed trusted bot (hive-authored work keeps today's relay
// behaviour), or holds the trusted-author role in the dashboard allowlist.
// Any other author is a contributor, and the request is allowed only when
// evaluateContributorLanePR — tier admission, author association, diff
// fences, labels, review verdict, CI with min_head_age, soak, human-merge
// paths — finds nothing outstanding, exactly as the contributor-lane sweep
// would have merged it on its next tick.
func (c *Engine) RelayMergeRefusal(ctx context.Context, displayRepo string, number int, expectSHA string) (string, error) {
	if !c.ready() {
		return "", hgithub.ErrNoGitHubClient
	}
	owner, repo := c.transport.SplitRepo(displayRepo)
	caller := hgithub.WithRESTCaller(ctx, "hive:merge_relay_lane_gate")
	pr, _, err := c.gh.PullRequests.Get(caller, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("%s#%d could not be fetched to check the lane policy (fail closed): %w", displayRepo, number, err)
	}
	if c.integrationExecution != nil {
		decision, routeErr := c.integrationExecution(ctx, owner+"/"+repo, pr, nil)
		if routeErr != nil {
			return decision.Reason, routeErr
		}
		if decision.Authority != "" {
			reason := decision.Reason
			if reason == "" {
				reason = "selected execution authority handles admission; relay cannot merge or jump the queue"
			}
			return reason, nil
		}
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	if _, hiveAuthored := c.sweepLaneForAuthor(author); hiveAuthored {
		return "", nil
	}
	trusted := c.currentTrustedAuthorPolicy()
	if decision := c.trustedAuthorDecision(author, trusted.RequireRole); decision.Allowed {
		c.info("merge relay lane gate: trusted-author role admits PR", "repo", displayRepo, "pr", number, "author", author, "role", decision.Role)
		return "", nil
	}
	policy := c.currentContributorLanePolicy()
	switch {
	case !policy.Enabled:
		return relayMergeRefusal(author, "", policy, relayLaneReasonDisabled, 0), nil
	case !policy.repoAllowed(displayRepo):
		return relayMergeRefusal(author, "", policy, relayLaneReasonRepo, 0), nil
	}
	ctx = contextWithExpectedCheckCache(ctx, newExpectedCheckCache())
	eval, reason, err := c.evaluateContributorLanePR(ctx, displayRepo, owner, repo, number, policy, true)
	if err != nil {
		return "", fmt.Errorf("%s#%d lane evidence unavailable (%s; fail closed): %w", displayRepo, number, reason, err)
	}
	if reason == "" && strings.TrimSpace(expectSHA) != "" && eval.headSHA != expectSHA {
		reason = relayLaneReasonHead
	}
	if reason == "" {
		c.info("merge relay lane gate: contributor lane admits PR", "repo", displayRepo, "pr", number, "author", author, "admitted_by", contributorLaneAdmitPath(eval.claim))
		return "", nil
	}
	return relayMergeRefusal(author, eval.claim.ObservedTier, policy, reason, eval.soakRemaining), nil
}

// relayMergeRefusal renders the refusal the relay records: the lane's own
// skip reason first (so it matches the sweep log and the stage label), then
// what the operator can act on — the author, the tier the lane saw against
// require_tiers, and the soak — and that the lane merges the PR itself once
// every gate passes, so nobody re-requests it. soakLeft is the soak time
// left when reason is the soaking skip (the reason itself carries no
// countdown), 0 otherwise.
func relayMergeRefusal(author, observedTier string, policy ContributorLanePolicy, reason string, soakLeft time.Duration) string {
	var b strings.Builder
	b.WriteString(RelayMergeRefusalPrefix)
	if author != "" {
		b.WriteString(" for @" + author + "'s PR")
	}
	b.WriteString(": " + reason)
	switch {
	case reason == relayLaneReasonDisabled:
		b.WriteString(" (auto_merge.contributor_lane.enabled is false; a person must merge contributor PRs)")
	case reason == relayLaneReasonRepo:
		b.WriteString(" (this repo is outside auto_merge.contributor_lane.repos)")
	case strings.HasPrefix(reason, contributorLaneReasonSoaking):
		fmt.Fprintf(&b, " (auto_merge.contributor_lane soak is %s", policy.Soak)
		if soakLeft > 0 {
			fmt.Fprintf(&b, ", %s remaining", soakLeft.Round(time.Minute))
		}
		b.WriteString("; the lane merges it itself once the soak elapses)")
	case strings.HasPrefix(reason, "pending: head pushed "):
		b.WriteString(" (auto_merge.min_head_age; the lane merges it itself once the head is old enough)")
	default:
		tier := strings.TrimSpace(observedTier)
		if tier == "" {
			tier = "none"
		}
		fmt.Fprintf(&b, " (author tier %q, require_tiers %s)", tier, strings.Join(sortedTierList(policy.RequireTiers), ", "))
	}
	return b.String()
}

func sortedTierList(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for tier, ok := range set {
		if ok {
			out = append(out, tier)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"(none)"}
	}
	return out
}
