package github

import (
	"context"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// EnsurePRAttribution reconciles the invocation-attribution trailer onto a PR
// the hive could NOT stamp at creation time (kubestellar/hive#4085): a
// local-mode contributor relay runs the contributor's own `gh` (no wrapper),
// and a GitHub-MCP `create_pull_request` never touches `gh` at all — so their
// PR bodies reach GitHub exactly as the agent wrote them. This closes that gap
// hub-side, after the fact, using the hub's OWN record of the connection's
// backend/model/effort (from the auth handshake) rather than the agent's
// self-report.
//
// It fetches the PR body and, when no AttributionTrailerPrefix line is present,
// appends the trailer via AppendTrailer (idempotent — a body the agent already
// stamped, or one a previous reconciliation stamped, is left untouched, so a PR
// touched by two mechanisms ends up with exactly one line). The visible edit is
// gated by the same governor.attribution_trailer toggle as the creation-time
// path; the audit entry is recorded whenever an edit is actually made.
//
// Returns updated=true only when the PR body was actually edited. All failures
// are returned for the caller to log — a reconciliation failure must never
// affect task-completion handling.
func (c *Client) EnsurePRAttribution(ctx context.Context, prURL string, m InvocationMeta) (updated bool, err error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	ref, err := ParsePRURL(prURL)
	if err != nil {
		return false, err
	}
	trailer := m.Trailer()
	if trailer == "" {
		// All-unknown metadata: there is nothing honest to stamp.
		return false, nil
	}
	if !c.attributionTrailerOn() {
		// Same operator toggle as the creation-time trailer: when the visible
		// line is suppressed there, do not retro-stamp it here either.
		return false, nil
	}
	pr, _, err := c.client.PullRequests.Get(ctx, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return false, err
	}
	body := pr.GetBody()
	if strings.Contains(body, AttributionTrailerPrefix) {
		// Already stamped (by the agent following its instruction, by the
		// creation-time path, or by an earlier reconciliation) — never stack.
		return false, nil
	}
	newBody := AppendTrailer(body, m)
	if newBody == body {
		return false, nil
	}
	if _, _, err := c.client.PullRequests.Edit(ctx, ref.Owner, ref.Repo, ref.Number, &gh.PullRequest{Body: gh.Ptr(newBody)}); err != nil {
		return false, err
	}
	c.recordCreationAudit(AuditActionPRAttributionReconciled, m,
		"repo", ref.FullName(), "number", strconv.Itoa(ref.Number), "url", prURL)
	return true, nil
}
