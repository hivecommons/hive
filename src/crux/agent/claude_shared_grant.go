package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/sharedfile"
)

// Durable hand-off of a freshly rotated Claude grant, and the one function
// every writer of the shared login goes through (hivecommons/hive#18775,
// follow-up to #18731).
//
// The lost-write window. The provider rotates the refresh token on a
// successful refresh: the instant the token endpoint answers, the refresh
// token the hive sent is dead and the new one exists only in the response. If
// the write of the new credential then failed, and the grant were dropped, the
// next attempt would present the dead token, get invalid_grant, and the whole
// fleet would be signed out until a person signs in again. So the new grant is
//
//  1. written to a second, private file beside the shared one
//     (.credentials.pending, 0600, fsynced) BEFORE the shared file is touched;
//  2. held in memory until the shared file has it, and written with backoff
//     (claudeSharedWriteBackoffBase doubling to claudeSharedWriteBackoffMax,
//     with no overall limit while the process lives);
//  3. protected from the refresher itself: while a grant is held the token
//     endpoint is never called again, whatever the clock says.
//
// A restart finds the pending file and adopts it if it is newer than the
// shared file (settlePendingSharedClaudeGrant). If the grant cannot be written
// anywhere the hive says so loudly (log, audit, and the dashboard banner from
// ClaudeSharedLoginUnsavedAlert) while the access token it replaces still has
// life left.
//
// Closed or narrowed? Inside a running process the window is closed: nothing
// drops the grant and nothing presents the old token again. Across a crash it
// is narrowed, not closed: it is the time between the token endpoint's answer
// reaching this process and the pending file's fsync completing (milliseconds
// when the disk works), plus the case where BOTH files fail to write and the
// process dies before either recovers. A hive cannot do better than that
// without the provider's cooperation, because the old token is dead from the
// moment of the answer.

const (
	// claudeSharedPendingName is the pending grant's file name, beside the
	// shared credential. It is deliberately not ".credentials.json", so the
	// permissions watcher that widens that name to group-readable never
	// touches it.
	claudeSharedPendingName = ".credentials.pending"
	// claudeSharedPendingMode: the hive's own uid only. The pending grant holds
	// a live refresh token and agents have no business reading it.
	claudeSharedPendingMode = 0o600

	// claudeSharedWriteBackoffBase is the wait after the first failed write of a
	// held grant; it doubles per failure up to claudeSharedWriteBackoffMax.
	claudeSharedWriteBackoffBase = 5 * time.Second
	claudeSharedWriteBackoffMax  = 5 * time.Minute

	// claudeSharedReconcileTrigger labels the bridge and session sweeps run
	// before a grant is declared rejected.
	claudeSharedReconcileTrigger = "refresh-reconcile"
)

// errClaudeSharedNotNewer: the shared login already holds a usable grant at
// least as new as the candidate, so the candidate is not written.
var errClaudeSharedNotNewer = errors.New("the shared Claude login already holds a grant at least as new")

// claudeSharedWrite and claudeSharedWritePending are the write seams; tests
// substitute failures to prove the hold-and-retry behaviour.
var (
	claudeSharedWrite        = replaceSharedClaudeLoginIfNewer
	claudeSharedWritePending = writeSharedClaudePending
)

// replaceSharedClaudeLoginIfNewer is THE writer of the shared Claude login: the
// central refresher, the agents' credential bridge (promote-on-race) and the
// session adopt-back all end here. The caller holds claudeBridgeMu, so the
// read of the current login and the write that replaces it are one step.
//
// cand replaces the shared login only if it is complete and usable
// (claudeBridgePromotable) and the shared login is not usable or expires
// strictly earlier than cand; otherwise nothing is written
// (errClaudeBridgeNotPromotable, errClaudeSharedNotNewer). Only the typed
// token block is written, atomically, fsynced, at claudeSharedCredentialMode
// and owned like the file it replaces. With preserveOld the replaced file is
// kept beside it first and its path returned (the refresher passes false: its
// refresh token is dead the moment the server rotated it).
func replaceSharedClaudeLoginIfNewer(cand *claude.OAuthTokens, target string, now time.Time, preserveOld bool) (string, error) {
	if !claudeBridgePromotable(cand, now) {
		return "", errClaudeBridgeNotPromotable
	}
	if cur := claude.LoadTokens(target); cur != nil && claude.TokensUsable(cur, now) && cand.ExpiresAt <= cur.ExpiresAt {
		return "", errClaudeSharedNotNewer
	}
	data, err := json.Marshal(claude.Credentials{ClaudeAIOAuth: cand})
	if err != nil {
		return "", errClaudeBridgeNotPromotable
	}
	// Refuse before any backup copy, not only at the write: the copy lands in
	// the same directory, and the directory is part of what is being checked.
	info, err := sharedfile.CheckLocation(target)
	if err != nil {
		return "", err
	}
	staleShared := ""
	if preserveOld && info != nil {
		if staleShared, err = preserveClaudeCredential(target, now); err != nil {
			return "", fmt.Errorf("preserve shared credential: %w", err)
		}
	}
	if err := sharedfile.Replace(target, data, claudeSharedCredentialMode, sharedfile.Options{PreserveOwner: true}); err != nil {
		return staleShared, err
	}
	return staleShared, nil
}

// sharedClaudePendingPath is the pending grant file beside the shared login.
func sharedClaudePendingPath(target string) string {
	return filepath.Join(filepath.Dir(target), claudeSharedPendingName)
}

// writeSharedClaudePending durably stores tok as the pending grant: temp file,
// fsync, rename, directory fsync, mode 0600.
func writeSharedClaudePending(tok *claude.OAuthTokens, target string) error {
	data, err := json.Marshal(claude.Credentials{ClaudeAIOAuth: tok})
	if err != nil {
		return err
	}
	return sharedfile.Replace(sharedClaudePendingPath(target), data, claudeSharedPendingMode, sharedfile.Options{})
}

// readSharedClaudePending returns the pending grant, nil when absent or not a
// credential.
func readSharedClaudePending(target string) *claude.OAuthTokens {
	data, err := sharedfile.ReadNoFollow(sharedClaudePendingPath(target), claudeBridgeMaxCredentialBytes)
	if err != nil {
		return nil
	}
	return claude.ParseTokens(data)
}

// removeSharedClaudePending deletes the pending file; absent is fine.
func removeSharedClaudePending(target string) {
	_ = os.Remove(sharedClaudePendingPath(target))
}

// holdSharedClaudeGrant records fresh as the grant that must reach the shared
// file before anything else happens. The pending file is written first; if
// that fails the grant is still held in memory and the failure is logged.
func (m *Manager) holdSharedClaudeGrant(fresh *claude.OAuthTokens, target string, lifetime time.Duration) {
	st := &m.claudeSharedRefresh
	st.mu.Lock()
	st.pending, st.pendingLifetime, st.pendingOnDisk = fresh, lifetime, false
	st.writeFailures, st.writeNotBefore, st.writeErr = 0, time.Time{}, ""
	st.mu.Unlock()
	m.ensureSharedClaudePendingOnDisk(fresh, target)
}

// ensureSharedClaudePendingOnDisk writes the pending file if it is not there.
func (m *Manager) ensureSharedClaudePendingOnDisk(p *claude.OAuthTokens, target string) {
	st := &m.claudeSharedRefresh
	st.mu.Lock()
	done := st.pendingOnDisk
	st.mu.Unlock()
	if done {
		return
	}
	if err := claudeSharedWritePending(p, target); err != nil {
		m.logger.Error("claude shared login: could not save the new refresh grant to its pending file; it is held in memory only until the shared file is written",
			"pending", sharedClaudePendingPath(target), "error", err)
		return
	}
	st.mu.Lock()
	st.pendingOnDisk = true
	st.mu.Unlock()
}

// settlePendingSharedClaudeGrant writes a held grant to the shared file, or
// adopts one a previous process left in the pending file. handled is true when
// a grant is held (even while it waits out a backoff): the caller must not
// call the token endpoint.
func (m *Manager) settlePendingSharedClaudeGrant(now time.Time) (claudeSharedRefreshOutcome, bool) {
	target := sharedClaudeBridgeTarget()
	st := &m.claudeSharedRefresh
	st.mu.Lock()
	if !st.pendingChecked {
		st.pendingChecked = true
		if st.pending == nil {
			st.mu.Unlock()
			p := readSharedClaudePending(target)
			st.mu.Lock()
			if p != nil && st.pending == nil {
				st.pending, st.pendingOnDisk = p, true
				m.logger.Warn("claude shared login: found a pending refresh grant from an earlier run; writing it to the shared login if it is newer", "pending", sharedClaudePendingPath(target))
			}
		}
	}
	held, wait := st.pending != nil, now.Before(st.writeNotBefore)
	st.mu.Unlock()
	if !held {
		return claudeSharedRefreshSkipped, false
	}
	if wait {
		return claudeSharedRefreshSkipped, true
	}
	return m.flushPendingSharedClaudeGrant(now), true
}

// flushPendingSharedClaudeGrant makes one attempt to write the held grant.
func (m *Manager) flushPendingSharedClaudeGrant(now time.Time) claudeSharedRefreshOutcome {
	target := sharedClaudeBridgeTarget()
	st := &m.claudeSharedRefresh
	claudeBridgeMu.Lock()
	st.mu.Lock()
	p, lifetime := st.pending, st.pendingLifetime
	st.mu.Unlock()
	if p == nil {
		claudeBridgeMu.Unlock()
		return claudeSharedRefreshSkipped
	}
	m.ensureSharedClaudePendingOnDisk(p, target)
	_, err := claudeSharedWrite(p, target, now, false)
	claudeBridgeMu.Unlock()

	switch {
	case err == nil:
		m.clearSharedClaudePending(target)
		m.mu.Lock()
		m.claudeAuthToken = p.AccessToken
		m.mu.Unlock()
		st.mu.Lock()
		if lifetime > 0 {
			st.lifetime = lifetime
		}
		st.failedExpiresAt, st.notBefore, st.rejectedExpiresAt = 0, time.Time{}, 0
		st.mu.Unlock()
		expires := time.UnixMilli(p.ExpiresAt).UTC().Format(time.RFC3339)
		m.logger.Info("claude shared login: refreshed the shared Claude credential ahead of expiry; agents read the new token through their credential bridges and do not refresh it themselves",
			"shared", target, "expires_at", expires)
		m.audit(AuditClaudeCredentialRefreshed, "", auditFields("outcome", "success", "trigger", "timer", "expires_at", expires))
		return claudeSharedRefreshWritten
	case errors.Is(err, errClaudeSharedNotNewer):
		m.clearSharedClaudePending(target)
		m.logger.Info("claude shared login: a fresher credential reached the shared file while the hive was refreshing it; keeping the fresher one", "shared", target)
		return claudeSharedRefreshSuperseded
	case errors.Is(err, errClaudeBridgeNotPromotable):
		m.clearSharedClaudePending(target)
		m.logger.Error("claude shared login: the held refresh grant is no longer usable and was dropped; sign in again if the fleet reports a revoked login", "shared", target)
		return claudeSharedRefreshFailed
	}
	m.recordSharedClaudeWriteFailure(err, now, target)
	return claudeSharedRefreshFailed
}

// clearSharedClaudePending forgets the held grant: the file first, then the
// memory, so a new hold can never be deleted by an old clear.
func (m *Manager) clearSharedClaudePending(target string) {
	removeSharedClaudePending(target)
	st := &m.claudeSharedRefresh
	st.mu.Lock()
	st.pending, st.pendingOnDisk, st.pendingLifetime = nil, false, 0
	st.writeFailures, st.writeNotBefore, st.writeErr = 0, time.Time{}, ""
	st.mu.Unlock()
}

// recordSharedClaudeWriteFailure arms the backoff and says, loudly and
// specifically, that the new grant is not yet saved. The error carries paths
// and errno text only, never token material.
func (m *Manager) recordSharedClaudeWriteFailure(err error, now time.Time, target string) {
	st := &m.claudeSharedRefresh
	st.mu.Lock()
	st.writeFailures++
	n := st.writeFailures
	delay := claudeSharedWriteBackoffBase
	for i := 1; i < n && delay < claudeSharedWriteBackoffMax; i++ {
		delay *= 2
	}
	if delay > claudeSharedWriteBackoffMax {
		delay = claudeSharedWriteBackoffMax
	}
	st.writeNotBefore, st.writeErr = now.Add(delay), err.Error()
	onDisk := st.pendingOnDisk
	st.mu.Unlock()

	m.logger.Error("claude shared login: the hive redeemed the shared Claude grant but could NOT save the new one; it is held and retried, and the token endpoint will not be called again until it is saved. Fix the credentials directory (disk space, ownership), or sign in again from the dashboard before the current access token expires",
		"shared", target, "error", err, "attempt", n, "retry_in", delay.String(), "pending_file_saved", onDisk)
	if n == 1 {
		m.audit(AuditClaudeCredentialRefreshed, "", auditFields(
			"outcome", "failed", "trigger", "timer", "reason", "new grant could not be saved",
			"pending_file_saved", onDisk, "recovery", defaultCredentialRecovery))
	}
}

// ClaudeSharedLoginUnsavedAlert is the dashboard banner text while a refreshed
// grant is held but not yet written to the shared login, empty otherwise. The
// host polls it (the dashboard cannot be imported here) and raises or clears
// its system alert. Never carries token material.
func (m *Manager) ClaudeSharedLoginUnsavedAlert() string {
	st := &m.claudeSharedRefresh
	st.mu.Lock()
	failures, errText, onDisk, held := st.writeFailures, st.writeErr, st.pendingOnDisk, st.pending != nil
	st.mu.Unlock()
	if !held || failures == 0 {
		return ""
	}
	deadline := "unknown"
	if cur := claude.LoadTokens(sharedClaudeBridgeTarget()); cur != nil && cur.ExpiresAt > 0 {
		deadline = time.UnixMilli(cur.ExpiresAt).UTC().Format(time.RFC3339)
	}
	backup := "NOT saved to its backup file either, so a restart now would lose it"
	if onDisk {
		backup = "saved to a backup file (.credentials.pending) and adopted on restart"
	}
	return fmt.Sprintf("The hive refreshed the shared Claude login but cannot write the new one (%s, %d attempts). The new grant is held in memory and %s. "+
		"The login the agents read expires at %s: fix the credentials directory (disk space, ownership) before then, or sign in to Claude from the dashboard.",
		errText, failures, backup, deadline)
}

// reconcileBeforeRejectingSharedClaude runs when the token endpoint refused
// the shared refresh token. A refusal is final only if no newer grant exists:
// the bridge and the session adopt-back are asked to publish any newer
// credential from an agent or session home first, and a shared login that now
// holds a different refresh token is a newer grant, so the refused one is not
// marked rejected. Called with no lock held.
func (m *Manager) reconcileBeforeRejectingSharedClaude(rejected *claude.OAuthTokens, target string, now time.Time) bool {
	st := &m.claudeSharedRefresh
	st.mu.Lock()
	held := st.pending != nil
	st.mu.Unlock()
	if held {
		return true
	}
	m.RepairClaudeCredentialBridges(claudeSharedReconcileTrigger)
	m.syncUserSessionClaudeLogins(claudeSharedReconcileTrigger)
	cur := claude.LoadTokens(target)
	if cur == nil || !claudeBridgePromotable(cur, now) || cur.RefreshToken == rejected.RefreshToken {
		return false
	}
	m.logger.Info("claude shared login: the refresh grant was refused, but a different, newer grant has reached the shared file; not marking the login rejected", "shared", target)
	return true
}
