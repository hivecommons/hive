package agent

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/claude"
)

// Central refresh of the shared Claude login (hivecommons/hive#18731).
//
// On a per-UID hive every Claude agent's CLI shares ONE OAuth grant through
// the ~/.claude/.credentials.json -> /data/home/.claude/.credentials.json
// bridge (claude_credential_bridge.go). Claude Code refreshes the access token
// itself, and Anthropic rotates the refresh token on every use. Because all N
// CLIs hold the same access token they all reach its expiry together and
// refresh within seconds of each other: the first rotates the grant, the rest
// present the now-dead refresh token, and the authorization server treats
// the reuse as a stolen token and revokes the WHOLE grant, the fresh one
// included. The CLIs then report "OAuth token revoked · Please run /login",
// no agent holds a token the bridge could promote, and the fleet is down
// until an operator signs in again. The bridge can only repair AFTER a
// refresh; it cannot stop N refreshes from racing.
//
// So the hive is the one refresher. It redeems the shared grant itself ahead
// of expiry (claude.RefreshTokens: the same refresh_token grant and public
// client id Claude Code sends, since the credentials file carries no client
// secret) and writes the result atomically to the shared file. Every CLI
// reads that file through its bridge, finds an access token with hours of
// life left, and never needs to refresh. Mirrors the Copilot backend's single
// durable-file refresh (copilot_token_store.go).
//
// When. The file records only expiresAt, not when the token was minted, so
// the schedule is "lead time before expiry": by default a quarter of the
// access token's lifetime (i.e. at 75% of it, claudeSharedRefreshAtFraction),
// using the lifetime the last refresh response reported and the measured
// 8-hour default until one has. HIVE_CLAUDE_SHARED_REFRESH_LEAD overrides the
// lead outright. The lead is far longer than the CLI's own margin, so the
// hive always moves first; if a CLI still refreshes first (a race at launch,
// a CLI started with a nearly-expired file) the bridge promotes its result
// exactly as before and the next pass sees a credential that is not due.
//
// Failure handling. A transient error (network, 5xx) is retried after a
// backoff. A rejected grant (claude.ErrRefreshGrantRejected: invalid_grant)
// means nothing on disk can mint a token: it is logged and audited once, with
// the operator sign-in recovery, and not retried until the shared file
// changes (a sign-in or a promoted CLI refresh). Token material is never
// logged or audited.
//
// Write rule. The HTTP round trip runs without any lock; the write re-reads
// the shared file under claudeBridgeMu and keeps whichever credential expires
// LATER, the same rule the bridge applies when it promotes an agent's
// private copy, so a fresher token that landed meanwhile is never overwritten
// by an older one. The replaced file is not preserved beside the shared
// credential: the refresh token in it is dead the moment the server rotated
// it, and the access token stays valid, in any CLI that already read it,
// until its own expiry regardless of what the file now says.
//
// Operator-token mode (HIVE_CLAUDE_OAUTH_TOKEN) leaves the shared file out of
// use, so the refresher does nothing there, like the bridge and the watchdog.

const (
	// claudeDefaultAccessTokenLifetime is the measured lifetime of a Claude
	// access token (see claude.HasUsableToken), used to size the refresh lead
	// until a refresh response has reported the real one.
	claudeDefaultAccessTokenLifetime = 8 * time.Hour

	// claudeSharedRefreshAtFraction is the point in the access token's
	// lifetime at which the hive refreshes the shared login: at 75%, i.e.
	// with a quarter of the lifetime still to run.
	claudeSharedRefreshAtFraction = 0.75

	// ClaudeSharedRefreshLeadEnv overrides how long before the access token's
	// expiry the hive refreshes, as a Go duration string ("2h", "45m"). Unset,
	// the lead is (1 - claudeSharedRefreshAtFraction) of the lifetime.
	ClaudeSharedRefreshLeadEnv = "HIVE_CLAUDE_SHARED_REFRESH_LEAD"

	// defaultClaudeSharedRefreshInterval is how often the refresher checks
	// whether the shared login is due. One parse of a small file; the HTTP
	// call happens only when it is due.
	defaultClaudeSharedRefreshInterval = time.Minute
	// ClaudeSharedRefreshIntervalEnv overrides that interval with a Go
	// duration string.
	ClaudeSharedRefreshIntervalEnv = "HIVE_CLAUDE_SHARED_REFRESH_INTERVAL"

	// ClaudeSharedRefreshEnv turns the central refresh off when set to a
	// false value ("0", "false", "off"): the CLIs then refresh for themselves
	// and only the bridge's after-the-fact repair stands between them and a
	// revoked grant. For operators who need the pre-#18731 behaviour back.
	ClaudeSharedRefreshEnv = "HIVE_CLAUDE_SHARED_REFRESH"

	// claudeSharedRefreshRetryBackoff is how long after a transient refresh
	// failure the same credential is left alone before another attempt.
	claudeSharedRefreshRetryBackoff = 5 * time.Minute
)

// claudeRefreshTokens is the refresh grant; a seam so tests can count calls
// and shape responses without a token endpoint.
var claudeRefreshTokens = claude.RefreshTokens

// claudeSharedRefreshNow stamps the schedule; a seam for tests.
var claudeSharedRefreshNow = time.Now

// claudeSharedRefreshState is the refresher's memory between passes. Keyed on
// the shared credential's expiresAt, which changes with every new token and
// so identifies the credential version without touching token material.
type claudeSharedRefreshState struct {
	mu sync.Mutex
	// lifetime is the access-token lifetime the last refresh response
	// reported; zero until one has, meaning claudeDefaultAccessTokenLifetime.
	lifetime time.Duration
	// failedExpiresAt and notBefore record a transient failure on one
	// credential version: that version is not retried before notBefore.
	failedExpiresAt int64
	notBefore       time.Time
	// rejectedExpiresAt is the credential version whose grant the server
	// rejected; it is never retried, only logged once.
	rejectedExpiresAt int64
	// inFlight is set while one pass holds the HTTP round trip, so a second
	// tick (or a concurrent caller) does not present the same refresh token
	// a second time, which is precisely the reuse that revokes the grant.
	inFlight bool

	// A refreshed grant not yet in the shared file (claude_shared_grant.go).
	pending         *claude.OAuthTokens
	pendingLifetime time.Duration
	pendingOnDisk   bool
	pendingChecked  bool
	writeFailures   int
	writeNotBefore  time.Time
	writeErr        string
}

// claudeSharedRefreshEnabled reports whether the central refresh is on
// (ClaudeSharedRefreshEnv unset or true).
func claudeSharedRefreshEnabled() bool {
	v := strings.TrimSpace(os.Getenv(ClaudeSharedRefreshEnv))
	if v == "" {
		return true
	}
	if strings.EqualFold(v, "off") {
		return false
	}
	on, err := strconv.ParseBool(v)
	if err != nil {
		return true
	}
	return on
}

// claudeSharedRefreshInterval resolves the check interval from
// ClaudeSharedRefreshIntervalEnv, falling back to the default.
func claudeSharedRefreshInterval() time.Duration {
	if v := os.Getenv(ClaudeSharedRefreshIntervalEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultClaudeSharedRefreshInterval
}

// claudeSharedRefreshLead is how long before expiry the shared login is
// refreshed: the env override when set and positive, otherwise a quarter of
// the given access-token lifetime (zero lifetime means the default).
func claudeSharedRefreshLead(lifetime time.Duration) time.Duration {
	if v := os.Getenv(ClaudeSharedRefreshLeadEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	if lifetime <= 0 {
		lifetime = claudeDefaultAccessTokenLifetime
	}
	return time.Duration(float64(lifetime) * (1 - claudeSharedRefreshAtFraction))
}

// claudeSharedRefreshDue reports whether the shared credential should be
// refreshed now: it has a refresh grant that has not itself expired, a
// recorded access-token expiry, and that expiry is within lead of now (or
// already past).
func claudeSharedRefreshDue(tok *claude.OAuthTokens, now time.Time, lead time.Duration) bool {
	if tok == nil || tok.RefreshToken == "" || tok.ExpiresAt <= 0 {
		return false
	}
	if tok.RefreshTokenExpiresAt > 0 && tok.RefreshTokenExpiresAt < now.UnixMilli() {
		return false
	}
	return time.UnixMilli(tok.ExpiresAt).Add(-lead).Before(now) || time.UnixMilli(tok.ExpiresAt).Add(-lead).Equal(now)
}

// claudeSharedRefreshOutcome says what one pass did, for callers and tests.
type claudeSharedRefreshOutcome int

const (
	// claudeSharedRefreshSkipped: not enabled, not in use, not due, or held
	// off by an earlier failure or an in-flight pass.
	claudeSharedRefreshSkipped claudeSharedRefreshOutcome = iota
	// claudeSharedRefreshWritten: a new token was minted and written.
	claudeSharedRefreshWritten
	// claudeSharedRefreshSuperseded: a new token was minted, but the shared
	// file already held one that expires later, which was kept.
	claudeSharedRefreshSuperseded
	// claudeSharedRefreshFailed: the refresh request failed.
	claudeSharedRefreshFailed
)

// refreshSharedClaudeCredential runs one pass of the central refresh against
// the shared credential. Safe to call from any goroutine; concurrent passes
// collapse to one request.
func (m *Manager) refreshSharedClaudeCredential() claudeSharedRefreshOutcome {
	if !claudeSharedRefreshEnabled() || m.claudeOperatorTokenMode() {
		return claudeSharedRefreshSkipped
	}
	target := sharedClaudeBridgeTarget()
	now := claudeSharedRefreshNow()
	// A grant already redeemed but not yet written comes first: the token
	// endpoint is never called while one is held (claude_shared_grant.go).
	if out, held := m.settlePendingSharedClaudeGrant(now); held {
		return out
	}
	tok := claude.LoadTokens(target)

	st := &m.claudeSharedRefresh
	st.mu.Lock()
	lead := claudeSharedRefreshLead(st.lifetime)
	if !claudeSharedRefreshDue(tok, now, lead) || st.inFlight ||
		st.rejectedExpiresAt == tok.ExpiresAt ||
		(st.failedExpiresAt == tok.ExpiresAt && now.Before(st.notBefore)) {
		st.mu.Unlock()
		return claudeSharedRefreshSkipped
	}
	st.inFlight = true
	st.mu.Unlock()
	defer func() {
		st.mu.Lock()
		st.inFlight = false
		st.mu.Unlock()
	}()

	fresh, err := claudeRefreshTokens(tok)
	if err != nil {
		if errors.Is(err, claude.ErrRefreshGrantRejected) && m.reconcileBeforeRejectingSharedClaude(tok, target, now) {
			return claudeSharedRefreshSuperseded
		}
		m.recordSharedClaudeRefreshFailure(tok.ExpiresAt, now, err)
		return claudeSharedRefreshFailed
	}
	if !claudeBridgePromotable(fresh, now) {
		m.recordSharedClaudeRefreshFailure(tok.ExpiresAt, now, errors.New("refresh response did not yield a complete, usable credential"))
		return claudeSharedRefreshFailed
	}

	// The old refresh token is dead from here on: keep the new grant on disk
	// and in memory until the shared file has it, then write it under the
	// bridge lock with the same "strictly newer wins" rule as every writer.
	m.holdSharedClaudeGrant(fresh, target, time.Duration(fresh.ExpiresAt-now.UnixMilli())*time.Millisecond)
	return m.flushPendingSharedClaudeGrant(now)
}

// recordSharedClaudeRefreshFailure logs and audits one failed pass and arms
// the hold-off: a rejected grant is never retried on this credential
// version; anything else is retried after claudeSharedRefreshRetryBackoff.
func (m *Manager) recordSharedClaudeRefreshFailure(expiresAt int64, now time.Time, err error) {
	st := &m.claudeSharedRefresh
	st.mu.Lock()
	rejected := errors.Is(err, claude.ErrRefreshGrantRejected)
	if rejected {
		st.rejectedExpiresAt = expiresAt
	} else {
		st.failedExpiresAt = expiresAt
		st.notBefore = now.Add(claudeSharedRefreshRetryBackoff)
	}
	st.mu.Unlock()

	if rejected {
		m.logger.Error("claude shared login: the authorization server rejected the shared refresh grant, so no token can be minted from the credential on disk; not retrying this credential. Sign in again from the dashboard",
			"shared", sharedClaudeBridgeTarget(), "error", err,
			"recovery", defaultCredentialRecovery,
			"cause", "the grant was revoked (a refresh token presented twice, or a sign-out) or it expired")
		m.audit(AuditClaudeCredentialRefreshed, "", auditFields(
			"outcome", "failed",
			"trigger", "timer",
			"reason", "refresh grant rejected",
			"recovery", defaultCredentialRecovery,
		))
		return
	}
	m.logger.Warn("claude shared login: refreshing the shared Claude credential failed; retrying later",
		"shared", sharedClaudeBridgeTarget(), "error", err, "retry_after", claudeSharedRefreshRetryBackoff.String())
	m.audit(AuditClaudeCredentialRefreshed, "", auditFields(
		"outcome", "failed",
		"trigger", "timer",
		"reason", "transient",
	))
}
