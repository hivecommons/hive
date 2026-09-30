package github

import (
	"context"
	"time"
)

// SetRequiredChecks installs the config-declared required-status-check set
// (config.AutoMergeConfig.RequiredCheckSet) consulted by commitGreen before
// it ever calls GitHub's branch-protection API. nil/empty clears it, meaning
// "not config-declared" — commitGreen then falls back to the API and, if that
// also fails, to the isMetaCheck/isIgnorableCICheck allowlist. Safe to call
// repeatedly (e.g. on every config reload); the sweep goroutine reads the
// installed value through requiredChecksMu.
func (c *Client) SetRequiredChecks(set map[string]bool) {
	if c == nil {
		return
	}
	c.requiredChecksMu.Lock()
	defer c.requiredChecksMu.Unlock()
	c.requiredChecks = set
}

// configRequiredChecks returns the currently installed config-declared
// required-check set and whether one is installed. Mirrors isTrustedMerger's
// nil-safe read pattern for c.mergerAuthz.
func (c *Client) configRequiredChecks() (map[string]bool, bool) {
	if c == nil {
		return nil, false
	}
	c.requiredChecksMu.RLock()
	defer c.requiredChecksMu.RUnlock()
	if len(c.requiredChecks) == 0 {
		return nil, false
	}
	return c.requiredChecks, true
}

func (c *Client) SetAutoMergeMinHeadAge(d time.Duration) {
	if c == nil {
		return
	}
	c.mergePolicyMu.Lock()
	defer c.mergePolicyMu.Unlock()
	c.autoMergeMinHeadAge = d
}

func (c *Client) configuredAutoMergeMinHeadAge() time.Duration {
	if c == nil {
		return 0
	}
	c.mergePolicyMu.RLock()
	defer c.mergePolicyMu.RUnlock()
	return c.autoMergeMinHeadAge
}

// StartSelfAuthoredAutoMergeSweep runs a loop that periodically calls
// SweepSelfAuthoredAutoMerges. It returns immediately; the loop runs until ctx
// is cancelled. A nil client is a no-op. maxMerges is passed straight through
// to AutoMergeSweepOptions.MaxMerges (<=0 falls back to
// DefaultAutoMergeSweepMaxMerges there). Mirrors
// StartMergeRequestWatcher/StartPRRequestWatcher's own-ticker-goroutine
// pattern so all three App-identity-dependent watchers share one shape.
//
// acmmAllowed is the caller-computed
// config.AutoMergeConfig.SelfAuthoredAutoMergeAllowed(acmmLevel) result (both
// the auto_merge.self_authored flag AND the hive's ACMM level gate self-merge
// authority — see config.SelfMergeMinACMMLevel). When false the loop is never
// started at all: an ACMM L4/L5 hive (l4.md/l5.md both forbid the App
// merging its own PRs) must not self-merge, matching console's L6 hive which
// is unaffected and keeps self-merging as before.
// refreshRateLimitCache re-reads GitHub's real rate limits and writes them back
// into the go-github client's cache.
//
// WHY THIS IS NEEDED. go-github refuses requests PRE-EMPTIVELY: once it has seen
// Remaining==0 it returns a synthetic 403 ("not making remote request") for
// every call until the cached Reset time passes, without contacting GitHub.
// That cache lives on the Client and is only updated by responses the Client
// itself receives.
//
// Hive's App client is created ONCE (NewClientFromApp) while appTransport
// injects a freshly minted INSTALLATION TOKEN per request, and installation
// tokens rotate roughly hourly. A new token gets a new allowance — but the
// Client's cache still says Remaining==0 with the old token's reset, so
// go-github keeps refusing requests the new token could happily serve.
//
// Observed live: the dashboard reported core remaining=6613 of 6900 while every
// sweep tick failed with "API rate limit of 6900 still exceeded", and the fleet
// consumed ZERO requests over six minutes — not rate-limited, just refusing.
// Merges stalled for the remainder of the window each time.
//
// GET /rate_limit does not count against any limit, and RateLimitService.Get
// writes the result back into the client's cache, so this is a cheap, exact
// correction rather than a guess.
func (c *Client) refreshRateLimitCache(ctx context.Context) {
	if c == nil || c.client == nil {
		return
	}
	if _, _, err := c.client.RateLimit.Get(ctx); err != nil {
		c.warn("could not refresh rate-limit cache", "error", err)
		return
	}
	c.info("refreshed rate-limit cache after a pre-emptive rate-limit refusal")
}

// isGitHubStatus reports whether err is a GitHub API error with the given HTTP
// status. Kept as a package-local shim for task-list sweep tests and older v5
// callers; production paths use githubStatusError directly.
func isGitHubStatus(err error, status int) bool {
	return githubStatusError(err, status)
}

func (c *Client) warn(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Warn(msg, args...)
	}
}

func (c *Client) info(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Info(msg, args...)
	}
}

// requiredStatusCheckContexts returns the set of status-check contexts /
// check-run names the base branch requires, and whether that set is known.
// See RequiredStatusCheckContexts for the resolution order; on v5 the merge
// sweep itself lives in pkg/github/automerge, so this Client-level shim is
// used only by the protection-facts collector (#7515).
func (c *Client) requiredStatusCheckContexts(ctx context.Context, owner, repo, branch string) (map[string]bool, bool) {
	set, ok := c.configRequiredChecks()
	return RequiredStatusCheckContexts(ctx, c.client, owner, repo, branch, set, ok)
}
