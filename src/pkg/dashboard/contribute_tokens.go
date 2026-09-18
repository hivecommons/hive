package dashboard

import (
	"context"
	"strings"
	"time"
)

// maybeRefreshToken re-mints a scoped GitHub token and pushes a token_refresh to
// the relay once wsTokenRefreshPeriod has elapsed since the current task's token
// was minted, provided a task is still active. This keeps long, human-steered
// sessions from silently losing push access when the original token expires at
// wsTokenTTL. The relay's token_refresh handler consumes github_token +
// token_expires_at (bin/contributor-relay.js). See #2393 item 2.
func (h *ContributeWSHub) maybeRefreshToken(c *ContributorConnection) {
	tier, repo, due := tokenRefreshDue(c, time.Now())
	if !due {
		return
	}

	tok, err := h.mintScopedToken(tier, repo)
	if err != nil {
		h.logger.Warn("[contribute-ws] token refresh: mint failed, will retry next heartbeat",
			"username", c.profile.GitHubUsername, "tier", tier, "error", err)
		h.sendTokenRefreshFailed(c, "mint failed, will retry on the next heartbeat")
		return
	}
	if tok == "" {
		// No new token available (no App auth / no cache): leave the relay's
		// existing token in place and try again next heartbeat.
		return
	}

	if err := h.sendTokenRefresh(c, tok); err != nil {
		h.logger.Info("[contribute-ws] token refresh: send failed", "username", c.profile.GitHubUsername, "error", err)
		return
	}

	h.logger.Info("[contribute-ws] token refreshed for active task",
		"username", c.profile.GitHubUsername, "tier", tier)
}

// resumeTaskToken re-mints a scoped GitHub token for a task that has just been
// re-asserted over a reconnect (task_progress rebuilt currentTask) and pushes it
// to the relay, which re-arms the heartbeat refresh cycle: sendTokenRefresh
// records tokenMintedAt on success, so the subsequent maybeRefreshToken calls see
// a non-zero mint time and fire again. Without this the resumed session's
// tokenMintedAt stays zero (cleared by the disconnect defer) and refresh never
// fires again for the life of the connection (#2610 finding 3). A mint failure or
// an empty token (no App auth / no cache) leaves the relay's existing token in
// place — the same lenient policy maybeRefreshToken uses — and tokenMintedAt stays
// zero, so the next reconnect (or a later mint success) can still arm it.
func (h *ContributeWSHub) resumeTaskToken(c *ContributorConnection, lease *taskLease) {
	// C4: mint for the SERVER-issued lease's tier and repository, not the client's
	// self-report, so a resumed session's credential is scoped to exactly the task the
	// hub assigned.
	tier := ""
	repo := ""
	if lease != nil {
		tier = lease.tier
		repo = lease.repo
	} else if c.profile != nil {
		tier = c.profile.TrustTier
	}
	tok, err := h.mintScopedToken(tier, repo)
	if err != nil {
		h.logger.Warn("[contribute-ws] resume token refresh: mint failed, refresh will re-arm on next resume/heartbeat",
			"username", c.profile.GitHubUsername, "tier", tier, "error", err)
		h.sendTokenRefreshFailed(c, "mint failed on task resume, refresh will re-arm on the next resume or heartbeat")
		return
	}
	if tok == "" {
		// No new token available: leave the relay's existing token in place.
		return
	}
	if err := h.sendTokenRefresh(c, tok); err != nil {
		h.logger.Info("[contribute-ws] resume token refresh: send failed", "username", c.profile.GitHubUsername, "error", err)
		return
	}
	h.logger.Info("[contribute-ws] token refreshed on task resume (re-armed refresh cycle)",
		"username", c.profile.GitHubUsername, "tier", tier)
}

// tokenRefreshDue reports whether the connection has an active task whose scoped
// token was minted at least wsTokenRefreshPeriod ago, meaning it is time to
// re-mint before wsTokenTTL. It returns the trust tier and the current task's repo
// to mint a repository-scoped token for (C4). Pure and clock-injectable so the
// timing can be tested without a real clock.
func tokenRefreshDue(c *ContributorConnection, now time.Time) (tier, repo string, due bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentTask == nil || c.tokenMintedAt.IsZero() {
		return "", "", false
	}
	if now.Sub(c.tokenMintedAt) < wsTokenRefreshPeriod {
		return "", "", false
	}
	if c.profile != nil {
		tier = c.profile.TrustTier
	}
	repo = c.currentTask.Repo
	return tier, repo, true
}

// sendTokenRefreshFailed tells the relay that a mid-task re-mint FAILED, so the
// credential it is holding is the OLD one and will expire at the token_expires_at
// it was last given (#5447).
//
// Before this, a failed mint was recorded only in the hub's log. The relay's first
// evidence was a push that started failing roughly an hour into a long task, which
// the agent saw as a generic auth error — the same misleading-symptom class as
// #5343, where a credential problem was reported as "the branch doesn't exist on
// the remote".
//
// It deliberately carries NO token material: only a type and a human-readable
// reason. The reason is a fixed, caller-supplied string, never the mint error
// itself, because that error can quote GitHub App responses and we do not want
// hub-internal auth detail crossing to a contributor-controlled process.
//
// Advisory only, and it changes NOTHING about the refresh contract: the old token
// stays installed, tokenMintedAt is untouched (so tokenRefreshDue keeps firing),
// and the next heartbeat retries exactly as before. A send failure is swallowed —
// this is a notification about a degraded credential, and failing the refresh path
// because the notification could not be delivered would turn a warning into an
// outage. The heartbeat's own ping remains the authority on whether the socket is
// alive.
//
// Concurrency: goes through c.send, which takes writeMu, and takes no other lock.
// Both callers (maybeRefreshToken, resumeTaskToken) hold neither c.mu nor c.writeMu
// at the call site — tokenRefreshDue releases c.mu before returning — so there is
// no re-entrancy here.
func (h *ContributeWSHub) sendTokenRefreshFailed(c *ContributorConnection, reason string) {
	if c == nil {
		return
	}
	if err := c.send(WSMessage{
		Type:   "token_refresh_failed",
		Seq:    h.nextSeq(),
		Reason: reason,
	}); err != nil {
		h.logger.Debug("[contribute-ws] token refresh: could not notify relay of mint failure",
			"username", c.profile.GitHubUsername, "error", err)
	}
}

// sendTokenRefresh writes a token_refresh message carrying the new token and its
// expiry, then records the new mint time. The field names (github_token,
// token_expires_at) match exactly what the relay's token_refresh handler
// consumes in bin/contributor-relay.js. See #2393 item 2.
func (h *ContributeWSHub) sendTokenRefresh(c *ContributorConnection, tok string) error {
	msg := WSMessage{
		Type:           "token_refresh",
		Seq:            h.nextSeq(),
		GitHubToken:    tok,
		TokenExpiresAt: time.Now().Add(wsTokenTTL).UTC().Format(time.RFC3339),
	}
	if err := c.send(msg); err != nil {
		return err
	}
	c.mu.Lock()
	c.tokenMintedAt = time.Now()
	c.mu.Unlock()
	return nil
}

// requireExplicitAccept reports whether this hive is in the opt-in EXPLICIT
// (human/manual) acceptance mode (#2537). A hub without a Config (direct-in-test
// construction) or an unset toggle resolves to FALSE — the trusted-source
// auto-accept default — so existing deployments keep delivering credentials to
// admitted tasks without a wait state.
func (h *ContributeWSHub) requireExplicitAccept() bool {
	if h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return false
	}
	return h.server.deps.Config.Hub.IsContributeRequireExplicitAccept()
}

// deliverTaskCredential ships the scoped credential the hub minted for the
// connection's current task but deliberately withheld from task_assign (#2537).
// It is the single post-acceptance delivery point: both the auto-accept path (in
// the ready handler, right after task_assign is sent) and the explicit-accept path
// (the task_accepted handler) funnel through here, so the credential provably
// leaves the hub only AFTER an acceptance decision was recorded — never bundled
// with the task metadata.
//
// It reuses the token_refresh wire shape (github_token + token_expires_at) that
// every existing relay already understands, so an old client that never learned a
// new "credential" message still ends up holding a working token: it processes
// task_assign (metadata) then a token_refresh (credential) exactly as it already
// handles a mid-task re-mint. The token itself is unchanged — the same per-tier
// scoped, wsTokenTTL-expiring token selectTask minted — only its timing moved.
//
// It is idempotent and safe to call more than once: it delivers only while a
// pending token is held and not yet delivered, then sets credentialDelivered and
// clears pendingToken. A task_accepted that arrives in auto-accept mode after the
// credential already went out is therefore a no-op, and a duplicate acceptance
// cannot double-send. It NEVER logs the token value. Returns true when it actually
// delivered (an assignment→acceptance→credential ordering point for tests/audit).
func (h *ContributeWSHub) deliverTaskCredential(c *ContributorConnection, reason string) bool {
	c.mu.Lock()
	if c.currentTask == nil || c.credentialDelivered || c.pendingToken == "" {
		c.mu.Unlock()
		return false
	}
	tok := c.pendingToken
	taskID := c.currentTask.TaskID
	username := ""
	if c.profile != nil {
		username = c.profile.GitHubUsername
	}
	c.mu.Unlock()

	// sendTokenRefresh writes the github_token + token_expires_at frame and
	// re-stamps tokenMintedAt on success, anchoring the #2393 refresh cycle on when
	// the relay actually received the credential.
	if err := h.sendTokenRefresh(c, tok); err != nil {
		// Delivery failed (socket gone): leave the token PENDING and undelivered so
		// a reconnect/resume or a retried acceptance can deliver it. Never log the
		// token value.
		h.logger.Info("[contribute-ws] task credential delivery failed, will retry",
			"username", username, "task", taskID, "accept_reason", reason, "error", err)
		return false
	}

	c.mu.Lock()
	c.credentialDelivered = true
	c.pendingToken = ""
	c.mu.Unlock()

	h.logger.Info("[contribute-ws] task credential delivered after acceptance",
		"username", username, "task", taskID, "accept_reason", reason)
	return true
}

// acceptTaskCredential is the explicit-acceptance entry point (#2537): a client
// task_accepted for taskID accepts the assigned task, releasing the credential the
// hub withheld from task_assign. It delivers only when taskID matches the task this
// connection currently holds — a stale or mismatched task_id (e.g. a late
// task_accepted for a task that already ended) is ignored, so a credential is never
// delivered for work this connection is not actually on. It returns whether a
// credential was delivered (an assignment→acceptance→credential ordering point).
func (h *ContributeWSHub) acceptTaskCredential(c *ContributorConnection, taskID string) bool {
	if c == nil || taskID == "" {
		return false
	}
	c.mu.Lock()
	match := c.currentTask != nil && c.currentTask.TaskID == taskID
	c.mu.Unlock()
	if !match {
		return false
	}
	return h.deliverTaskCredential(c, "explicit_accept")
}

// mintScopedToken produces a scoped GitHub token for the given trust tier via the
// GitHub App auth path, scoped to a single repository when repo is non-empty
// (hivecommons/hive C4). This is the single mint path shared by task_assign and the
// heartbeat/resume token-refresh, so all three advertise tokens minted the same way.
// See #2393 item 2.
//
// C4 (CWE-862/639): the previous full-cache FALLBACK — when no App auth was
// configured, returning the hive's own FULL cached installation token — has been
// DELETED. Handing a contributor relay the hive's installation-wide credential gave
// it push access to every repo the installation covers, wildly beyond the single
// issue the contributor was assigned, and it was reachable from the forgeable resume
// path. When there is no App auth to mint a properly scoped token, mint NOTHING: the
// caller treats an empty token as "leave the relay's current token in place", and
// selectTask surfaces token_mint_failed rather than leaking the full credential.
func (h *ContributeWSHub) mintScopedToken(tier, repo string) (string, error) {
	if h.server != nil && h.server.deps != nil && h.server.deps.GHAppAuth != nil {
		ctx := h.server.deps.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		// C4: restrict the installation token to the assignment's repository so the
		// credential cannot touch any other repo the installation covers. repoNameOnly
		// yields the bare repo name the GitHub API's Repositories option expects; an
		// empty repo (synthetic/pr-review tasks with no single repo) falls back to the
		// tier-scoped installation token — still permission-scoped, unchanged behavior.
		if name := repoNameOnly(repo); name != "" {
			return h.server.deps.GHAppAuth.ScopedTokenForRepos(ctx, tier, []string{name})
		}
		return h.server.deps.GHAppAuth.ScopedToken(ctx, tier)
	}
	// No App auth: mint nothing rather than leak a full credential (see the note
	// above — the cache fallback is deliberately gone).
	return "", nil
}

// repoNameOnly returns the bare repository name from an "owner/repo" (or already
// bare) value, for the GitHub App installation-token Repositories option, which is
// keyed on the repo name within the installation's org (hivecommons/hive C4). An
// empty input yields "".
func repoNameOnly(repo string) string {
	repo = strings.TrimSpace(repo)
	if idx := strings.LastIndex(repo, "/"); idx >= 0 {
		return repo[idx+1:]
	}
	return repo
}

// contributorCanPush reports whether the connected contributor can create a
// branch directly in repoFull. A personal repository cannot be forked back into
// the same account, so owner equality is both authoritative and deliberately
// independent of API availability. For organization repositories, GitHub's
// permission endpoint folds direct, team, organization, and enterprise grants
// into one effective permission. Any missing dependency, malformed identity,
// timeout, or API error fails closed to the universally safe fork workflow.
func (h *ContributeWSHub) contributorCanPush(repoFull, username string) bool {
	owner, repo, ok := strings.Cut(strings.TrimSpace(repoFull), "/")
	username = strings.TrimSpace(username)
	if !ok || owner == "" || repo == "" || username == "" {
		return false
	}
	if strings.EqualFold(owner, username) {
		return true
	}
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return false
	}
	client := h.server.deps.GHClient.GoGitHub()
	if client == nil {
		return false
	}
	ctx := h.server.deps.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, repoPermissionTimeout)
	defer cancel()
	level, _, err := client.Repositories.GetPermissionLevel(ctx, owner, repo, username)
	if err != nil {
		if h.logger != nil {
			h.logger.Debug("[contribute-ws] repository permission lookup failed; using fork workflow",
				"repo", repoFull, "username", username, "error", err)
		}
		return false
	}
	switch strings.ToLower(level.GetPermission()) {
	case "admin", "write":
		return true
	default:
		return false
	}
}
