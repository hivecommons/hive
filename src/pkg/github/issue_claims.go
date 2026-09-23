package github

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/agentmode"
	"github.com/hivecommons/hive/pkg/issueclaim"
)

// Issue claims at enumeration time (hivecommons/hive#8380).
//
// The claim's source of truth is the issue on GitHub — a `hive-claim` marker
// comment, or an assignee — so this is where the hive reads it: once per
// enumeration, for the actionable issues only, decorating each Issue with the
// live claim (if any). Every downstream listing (contribute queue, kick
// prompts, runs API) then reads the SAME fields rather than each fetching
// comments on its own.
//
// Cost: one ListComments call per actionable issue on first sight, then none
// until the issue's updated_at moves — a new comment, assignee change or any
// other activity bumps that timestamp, so a cache keyed on it is exact. With
// claims off (the default) nothing here runs.

// issueClaimCacheMax bounds the updated_at-keyed cache. Past it the cache is
// dropped wholesale rather than evicted piecemeal: the next enumeration
// refills it for the live actionable set, which is the only set it serves.
const issueClaimCacheMax = 4096

type issueClaimCacheEntry struct {
	updatedAt time.Time
	claim     issueclaim.Claim
	found     bool
}

// SetIssueClaims installs the live claims setting: whether claims are
// recognised and the TTL an assignee-inferred claim runs for. Nil disables.
func (c *Client) SetIssueClaims(fn func() (enabled bool, ttl time.Duration)) {
	if c == nil {
		return
	}
	c.issueClaims = fn
}

func (c *Client) issueClaimsSetting() (bool, time.Duration) {
	if c == nil || c.issueClaims == nil {
		return false, 0
	}
	enabled, ttl := c.issueClaims()
	if ttl <= 0 {
		ttl = issueclaim.DefaultTTL
	}
	return enabled, ttl
}

// annotateIssueClaims sets ClaimedBy / ClaimExpiresAt / ClaimSource on every
// issue in the slice that carries a LIVE claim at now. A fetch failure for one
// issue leaves that issue unannotated (fail open: an unreadable claim must
// not withhold work) and is logged; the rest of the slice is still decorated.
func (c *Client) annotateIssueClaims(ctx context.Context, owner, repo string, issues []Issue, now time.Time) {
	enabled, ttl := c.issueClaimsSetting()
	if !enabled || len(issues) == 0 {
		return
	}
	for i := range issues {
		issue := &issues[i]
		claim, found, err := c.issueClaimFor(ctx, owner, repo, issue, ttl, now)
		if err != nil {
			if c.logger != nil {
				c.logger.Warn("issue claim: comment fetch failed; treating the issue as unclaimed",
					"repo", issue.Repo, "number", issue.Number, "error", err)
			}
			continue
		}
		if !found {
			continue
		}
		expires := claim.ExpiresAt
		issue.ClaimedBy = claim.Identity
		issue.ClaimExpiresAt = &expires
		issue.ClaimSource = claim.Source
	}
}

// issueClaimFor resolves the live claim for one issue, through the
// updated_at-keyed cache. The cache stores the claim as READ (found or not,
// regardless of liveness) and liveness is re-judged against now on every
// call, so an expiring claim releases the issue on the next enumeration
// without a fetch.
func (c *Client) issueClaimFor(ctx context.Context, owner, repo string, issue *Issue, ttl time.Duration, now time.Time) (issueclaim.Claim, bool, error) {
	key := issue.Repo + "#" + strconv.Itoa(issue.Number)

	c.issueClaimMu.Lock()
	entry, hit := c.issueClaimCache[key]
	c.issueClaimMu.Unlock()
	if hit && !issue.UpdatedAt.IsZero() && entry.updatedAt.Equal(issue.UpdatedAt) {
		return resolveCachedClaim(entry, issue, ttl, now)
	}

	comments, err := c.listIssueComments(ctx, owner, repo, issue.Number)
	if err != nil {
		return issueclaim.Claim{}, false, err
	}
	bodies := make([]string, 0, len(comments))
	for _, comment := range comments {
		if !c.isTrustedAppBotCommentAuthor(comment) {
			continue
		}
		bodies = append(bodies, comment.GetBody())
	}
	entry = issueClaimCacheEntry{updatedAt: issue.UpdatedAt}
	entry.claim, entry.found = issueclaim.Latest(bodies, ttl)

	c.issueClaimMu.Lock()
	if c.issueClaimCache == nil || len(c.issueClaimCache) >= issueClaimCacheMax {
		c.issueClaimCache = make(map[string]issueClaimCacheEntry)
	}
	c.issueClaimCache[key] = entry
	c.issueClaimMu.Unlock()

	return resolveCachedClaim(entry, issue, ttl, now)
}

// resolveCachedClaim applies issueclaim.Resolve's precedence to a cached
// marker read: a live marker claims; an expired marker releases; no marker
// falls back to the assignee inference.
func resolveCachedClaim(entry issueClaimCacheEntry, issue *Issue, ttl time.Duration, now time.Time) (issueclaim.Claim, bool, error) {
	if entry.found {
		entry.claim = issueclaim.ClampToTTL(entry.claim, ttl)
		if entry.claim.Live(now) {
			return entry.claim, true, nil
		}
		return issueclaim.Claim{}, false, nil
	}
	claim, ok := issueclaim.FromAssignees(issue.Assignees, issue.UpdatedAt, ttl)
	if ok && claim.Live(now) {
		return claim, true, nil
	}
	return issueclaim.Claim{}, false, nil
}

// LiveClaim returns the claim carried on the issue if it is still live at
// now. It is the one reader every consumer of the ClaimedBy/ClaimExpiresAt
// pair should use, so "live" has a single definition.
func (i Issue) LiveClaim(now time.Time) (issueclaim.Claim, bool) {
	if i.ClaimedBy == "" || i.ClaimExpiresAt == nil {
		return issueclaim.Claim{}, false
	}
	claim := issueclaim.Claim{Identity: i.ClaimedBy, ExpiresAt: *i.ClaimExpiresAt, Source: i.ClaimSource}
	if !claim.Live(now) {
		return issueclaim.Claim{}, false
	}
	return claim, true
}

// FilterLiveIssueClaims removes from result every issue that carries a live
// issue claim at now, so the kick prompts (scanner included) treat a claimed
// issue as covered exactly as they treat one an open PR claims. It mutates
// result in place, mirroring FilterClaimedIssues, and returns the number of
// issues withheld. Issues carry claim fields only while claims are enabled,
// so with the feature off this is a pure no-op over an unchanged slice.
func FilterLiveIssueClaims(result *ActionableResult, now time.Time, logger *slog.Logger) int {
	if result == nil || len(result.Issues.Items) == 0 {
		return 0
	}
	kept := make([]Issue, 0, len(result.Issues.Items))
	withheld := 0
	for _, issue := range result.Issues.Items {
		claim, live := issue.LiveClaim(now)
		if !live {
			kept = append(kept, issue)
			continue
		}
		withheld++
		if logger != nil {
			logger.Info("withholding issue: live issue claim",
				"repo", issue.Repo, "issue", issue.Number,
				"claimed_by", claim.Identity, "claim_expires_at", claim.ExpiresAt,
				"claim_source", claim.Source)
		}
	}
	if withheld == 0 {
		return 0
	}
	result.Issues = IssueResultFromItems(kept)
	return withheld
}

// The dashboard-facing seam for issue claims (hivecommons/hive#8380).
//
// pkg/dashboard already imports this package for the Issue envelope that
// carries the claim fields, and its internal-import count is ratcheted
// (import_count_test.go). So the claim vocabulary it needs — the claim record,
// the comment body and the "may this tier comment?" predicate — is exposed
// from here rather than pulling pkg/issueclaim and pkg/agentmode into the
// dashboard. Both underlying packages are stdlib-only leaves, so importing
// them here adds no depth to the graph.

// IssueClaimMark is one recognised issue claim (issueclaim.Claim).
type IssueClaimMark = issueclaim.Claim

const (
	// IssueClaimDefaultTTL is the claim TTL when none is configured.
	IssueClaimDefaultTTL = issueclaim.DefaultTTL
	// IssueClaimSourceMarker / IssueClaimSourceAssignee / IssueClaimSourceLease
	// name where a claim was read from or recorded.
	IssueClaimSourceMarker   = issueclaim.SourceMarker
	IssueClaimSourceAssignee = issueclaim.SourceAssignee
	IssueClaimSourceLease    = issueclaim.SourceLease
)

// IssueClaimCommentBody renders the claim comment (marker plus human line).
func IssueClaimCommentBody(identity string, started, expires time.Time) string {
	return issueclaim.CommentBody(identity, started, expires)
}

// IssueClaimTierCanComment reports whether a scoped-token tier maps to an
// agent mode that may write issue comments, and names that mode for logs.
// An unknown tier fails closed (ADVISORY, cannot comment).
func IssueClaimTierCanComment(tier string) (canComment bool, mode string) {
	m, _ := agentmode.ModeForTokenTier(tier)
	return m.CanComment(), m.String()
}
