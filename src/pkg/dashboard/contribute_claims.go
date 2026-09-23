package dashboard

import (
	"context"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Issue claims on the contribute hub (hivecommons/hive#8380).
//
// Reading: the enumerator decorates each actionable issue with any LIVE claim
// (claimed_by / claim_expires_at / claim_source, pkg/github issue_claims.go).
// The admission ladder reads those fields — through claimFromIssueMap, the one
// reader — and withholds a claimed issue exactly as it withholds one an open
// PR claims. Nothing here fetches from GitHub to decide admission.
//
// Writing: when a relay contributor takes a lease, the hub posts the claim
// comment on the issue through the existing forge seam, but ONLY when the
// contributor's tier sits at a mode that may write issue comments
// (agentmode.CanComment, reached through the pkg/github seam). Below that the claim is recorded on the lease alone,
// so the runs API still shows it while the forge stays untouched.
//
// Everything is gated on governor.claims.enabled; with it off every function
// here is a no-op and no listing carries a claim field.

// claimCommentTimeout bounds the claim comment post so a slow forge cannot
// hold the assignment hostage — the lease is already committed and the
// task_assign is what the relay is waiting for. Same bound as the plan mirror.
const claimCommentTimeout = 15 * time.Second

// claimsEnabled reports governor.claims.enabled for this hub.
func (h *ContributeWSHub) claimsEnabled() bool {
	return h != nil && h.server != nil && h.server.deps != nil && h.server.deps.Config != nil &&
		h.server.deps.Config.Governor.Claims.Enabled
}

// claimTTL is governor.claims.ttl_s with the default applied.
func (h *ContributeWSHub) claimTTL() time.Duration {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return ghpkg.IssueClaimDefaultTTL
	}
	return h.server.deps.Config.Governor.Claims.EffectiveTTL()
}

// claimFromIssueMap reads the enumerator's claim fields off one actionable
// issue and reports the claim if it is live at now. It is the single reader
// for the contribute queue and selectTask, so both surfaces judge liveness
// identically. With claims disabled it never reports a claim, whatever the
// map carries, so turning the feature off releases everything at once.
func (h *ContributeWSHub) claimFromIssueMap(issue map[string]any, now time.Time) (ghpkg.IssueClaimMark, bool) {
	if !h.claimsEnabled() {
		return ghpkg.IssueClaimMark{}, false
	}
	identity, _ := issue["claimed_by"].(string)
	raw, _ := issue["claim_expires_at"].(string)
	if identity == "" || raw == "" {
		return ghpkg.IssueClaimMark{}, false
	}
	expires, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return ghpkg.IssueClaimMark{}, false
	}
	source, _ := issue["claim_source"].(string)
	claim := ghpkg.IssueClaimMark{Identity: identity, ExpiresAt: expires, Source: source}
	if !claim.Live(now) {
		return ghpkg.IssueClaimMark{}, false
	}
	return claim, true
}

// claimCommenterFor returns the forge seam the hub posts claims through: the
// injected recorder (tests) or the wired GitHub client. Nil when neither is
// present, in which case a claim is lease-only.
func (h *ContributeWSHub) claimCommenterFor() planIssueCommenter {
	if h == nil {
		return nil
	}
	if h.claimCommenter != nil {
		return h.claimCommenter
	}
	if h.server != nil && h.server.deps != nil && h.server.deps.GHClient != nil {
		return h.server.deps.GHClient
	}
	return nil
}

// claimIdentityFor names the claimant for a relay assignment: the
// contributor's GitHub login when known, else its hub identity.
func claimIdentityFor(c *ContributorConnection) string {
	if c != nil && c.profile != nil && c.profile.GitHubUsername != "" {
		return c.profile.GitHubUsername
	}
	return identityOf(c)
}

// recordAgentClaim asserts the claim for a task the hub just leased to a
// relay contributor. It always records the claim on the lease (so the runs
// API shows it); it posts the claim comment on the issue only when the
// contributor's tier maps to a mode that may write issue comments and a
// commenter is wired. A failed post is logged and leaves the lease claim
// standing — the assignment is never refused over a comment. It returns the
// recorded claim and whether the comment was posted; the zero claim and false
// when claims are off or the task is not a GitHub issue.
func (h *ContributeWSHub) recordAgentClaim(ctx context.Context, c *ContributorConnection, taskID, repoFull string, number int, now time.Time) (ghpkg.IssueClaimMark, bool) {
	if !h.claimsEnabled() || c == nil || repoFull == "" || number <= 0 {
		return ghpkg.IssueClaimMark{}, false
	}
	claim := ghpkg.IssueClaimMark{
		Identity:  claimIdentityFor(c),
		StartedAt: now,
		ExpiresAt: now.Add(h.claimTTL()),
		Source:    ghpkg.IssueClaimSourceLease,
	}
	tier := ""
	if c.profile != nil {
		tier = c.profile.TrustTier
	}
	canComment, mode := ghpkg.IssueClaimTierCanComment(tier)
	posted := false
	if commenter := h.claimCommenterFor(); commenter != nil && canComment {
		postCtx, cancel := context.WithTimeout(ctx, claimCommentTimeout)
		defer cancel()
		body := ghpkg.IssueClaimCommentBody(claim.Identity, claim.StartedAt, claim.ExpiresAt)
		if err := commenter.CreateIssueComment(postCtx, repoFull, number, body); err != nil {
			h.logger.Warn("[contribute-ws] issue claim comment failed; claim recorded on the lease only",
				"username", identityOf(c), "task", taskID, "repo", repoFull, "number", number, "error", err)
		} else {
			posted = true
			claim.Source = ghpkg.IssueClaimSourceMarker
			h.logger.Info("[contribute-ws] issue claim posted",
				"username", identityOf(c), "task", taskID, "repo", repoFull, "number", number,
				"claim_expires_at", claim.ExpiresAt.UTC().Format(time.RFC3339))
		}
	} else {
		h.logger.Info("[contribute-ws] issue claim recorded on the lease only",
			"username", identityOf(c), "task", taskID, "repo", repoFull, "number", number,
			"tier", tier, "mode", mode, "can_comment", canComment)
	}
	h.setLeaseClaim(identityOf(c), taskID, claim, posted)
	return claim, posted
}
