package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AUDIT F10, part 2: server-side session revocation.
//
// Signing the expiry (hub_cookie.go) bounds how long a copied cookie survives.
// It does not make logout mean anything before that bound: a value captured
// while it was valid keeps working until its own `exp` passes. Revocation is
// what closes that window on demand — logout, a compromised account, an admin
// pulling a session — by recording the session ID and rejecting it at verify
// time regardless of what the signature says.
//
// WHY THIS IS ON DISK AND NOT IN A MAP.
//
// The hub restarts frequently (auto-upgrades roll it several times a day). An
// in-memory-only revocation set is therefore not merely lossy, it is a
// vulnerability with a published schedule: revoke a session, wait for the next
// roll, and the cookie you revoked works again. /data is a real RWX PVC
// (hive-hub-data-rwx) that already holds alert acks, banners and registry
// state, so the revocation set persists exactly as those do.
//
// The stored shape is deliberately minimal — {sid: exp} and nothing else. There
// is no reason for the hub to retain who a revoked session belonged to, and an
// entry is useless once the cookie it names would fail the expiry check anyway,
// so entries are evicted at their own `exp`. That eviction is what keeps the
// file bounded: the set can only ever be as large as the sessions revoked
// within one cookie lifetime, not the sessions revoked ever.

// revokedSessionsPath is the on-disk file holding revoked session IDs so a
// logout survives a hub restart. A var (not a const) so tests can point it at a
// temp dir — same shape as alertAcksPath.
var revokedSessionsPath = "/data/saas/hub-revoked-sessions.json"

// revokedSessionsQuarantineSuffix is appended to revokedSessionsPath when the
// file on disk will not parse.
//
// NOTE the difference from alert acks, and it is deliberate: a corrupt ack file
// is quarantined and the system continues with an empty set, because losing an
// ack merely un-silences an alert. Losing revocations UN-REVOKES SESSIONS, which
// is the unsafe direction. So a corrupt file is quarantined for inspection and
// the load FAILS LOUDLY rather than pretending the set is empty; see
// loadRevokedSessions.
const revokedSessionsQuarantineSuffix = ".corrupt"

// revokedSessions is the in-memory view of the persisted revocation set, keyed
// by session ID with the value being the revoked cookie's own expiry (Unix
// seconds) — the point after which the entry can be dropped.
type revokedSessions struct {
	mu  sync.RWMutex
	sid map[string]int64
	// failedLoad records that the persisted set could not be read. While true
	// the store fails CLOSED: see isRevoked.
	failedLoad bool
}

func newRevokedSessions() *revokedSessions {
	return &revokedSessions{sid: make(map[string]int64)}
}

// isRevoked reports whether a session ID must be rejected.
//
// Fails CLOSED on a load failure. If the hub could not read its revocation set,
// it does not know which sessions are revoked, and answering "false" there is
// precisely the F10 bug re-introduced by an I/O error. Refusing every v3 cookie
// degrades to "everyone re-authenticates", which is recoverable; honouring a
// revoked admin cookie is not.
//
// An entry whose own expiry has passed is treated as not-revoked: the cookie it
// names now fails the signed-expiry check on its own, so the entry is dead
// weight. Eviction from the map happens in pruneExpired, off the read path.
func (r *revokedSessions) isRevoked(sid string, now time.Time) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.failedLoad {
		return true
	}
	exp, ok := r.sid[sid]
	if !ok {
		return false
	}
	return exp >= now.Unix()
}

// revoke records a session ID as revoked until exp. Returns false when the entry
// was already present and nothing changed, so callers can skip a pointless disk
// write.
//
// A non-positive exp is ignored rather than stored: an entry that is already
// expired would be evicted on the next prune without ever having rejected
// anything, and storing it would let a caller grow the file with dead keys.
func (r *revokedSessions) revoke(sid string, exp int64, now time.Time) bool {
	if r == nil || sid == "" || exp <= now.Unix() {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.sid[sid]; ok && existing >= exp {
		return false
	}
	r.sid[sid] = exp
	return true
}

// pruneExpired drops entries whose expiry has passed, reporting whether anything
// was removed so the caller knows if the file needs rewriting. This is the only
// thing keeping the persisted set bounded.
func (r *revokedSessions) pruneExpired(now time.Time) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	nowUnix := now.Unix()
	changed := false
	for sid, exp := range r.sid {
		if exp < nowUnix {
			delete(r.sid, sid)
			changed = true
		}
	}
	return changed
}

// snapshot returns a copy of the revocation set for persistence.
func (r *revokedSessions) snapshot() map[string]int64 {
	if r == nil {
		return map[string]int64{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int64, len(r.sid))
	for sid, exp := range r.sid {
		out[sid] = exp
	}
	return out
}

// load replaces the set from persisted data, dropping already-expired entries.
// Reports whether anything was pruned, so the caller can rewrite the file.
func (r *revokedSessions) load(stored map[string]int64, now time.Time) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	nowUnix := now.Unix()
	r.sid = make(map[string]int64, len(stored))
	pruned := false
	for sid, exp := range stored {
		if sid == "" {
			continue
		}
		if exp < nowUnix {
			pruned = true
			continue
		}
		r.sid[sid] = exp
	}
	r.failedLoad = false
	return pruned
}

// markLoadFailed puts the store into fail-closed mode.
func (r *revokedSessions) markLoadFailed() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failedLoad = true
}

// revokedSessionsFileMu serialises writers of the revocation file, for the same
// reason alertAcksFileMu exists: two concurrent logouts writing the SAME tmp
// path with os.WriteFile truncate each other mid-write, and the interleaved
// bytes that get renamed into place are not JSON. For acks that cost a silenced
// alert; here it would cost the entire revocation set on the next restart.
var revokedSessionsFileMu sync.Mutex

// saveRevokedSessions persists the revocation set to the PVC. Mirrors
// saveAlertAcks exactly: atomic tmp+rename, failures logged not fatal.
func (s *HubServer) saveRevokedSessions() {
	if s == nil || s.revokedSessions == nil {
		return
	}
	revokedSessionsFileMu.Lock()
	defer revokedSessionsFileMu.Unlock()
	data, err := json.MarshalIndent(s.revokedSessions.snapshot(), "", "  ")
	if err != nil {
		s.logger.Error("failed to marshal revoked sessions for persistence", "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(revokedSessionsPath), 0o755); err != nil {
		s.logger.Error("failed to create revoked sessions dir", "error", err)
		return
	}
	tmpPath := revokedSessionsPath + ".tmp"
	// 0o600, not 0o644: the set of live-but-revoked session IDs is not
	// something any other process on the PVC needs to read.
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		s.logger.Error("failed to write revoked sessions tmp file", "error", err)
		return
	}
	if err := os.Rename(tmpPath, revokedSessionsPath); err != nil {
		s.logger.Error("failed to rename revoked sessions file", "error", err)
	}
}

// loadRevokedSessions reads the persisted revocation set at startup. A missing
// file is normal (nothing has been revoked yet) and yields an empty set.
//
// Any OTHER failure — unreadable file, unparseable JSON — marks the store
// fail-closed, so every v3 cookie is rejected until an operator resolves it.
// That is a blunt outcome and it is the intended one: the alternative is a hub
// that has silently forgotten which sessions were revoked while continuing to
// honour them.
func (s *HubServer) loadRevokedSessions() {
	if s == nil || s.revokedSessions == nil {
		return
	}
	data, err := os.ReadFile(revokedSessionsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		s.logger.Error("failed to read revoked sessions — rejecting v3 sessions until resolved", "error", err)
		s.revokedSessions.markLoadFailed()
		return
	}
	var stored map[string]int64
	if err := json.Unmarshal(data, &stored); err != nil {
		quarantine := revokedSessionsPath + revokedSessionsQuarantineSuffix
		if renameErr := os.Rename(revokedSessionsPath, quarantine); renameErr != nil {
			s.logger.Error("failed to quarantine corrupt revoked sessions file",
				"error", renameErr, "parseError", err)
		} else {
			s.logger.Error("persisted revoked sessions were corrupt — quarantined; rejecting v3 sessions until resolved",
				"error", err, "quarantined", quarantine)
		}
		s.revokedSessions.markLoadFailed()
		return
	}
	if s.revokedSessions.load(stored, time.Now()) {
		s.saveRevokedSessions()
	}
}

// revokeHubSessionCookie revokes the session carried by a cookie value, if that
// value is a v3 cookie. Returns whether anything was revoked.
//
// A v2 or legacy cookie carries no session ID, so there is nothing to revoke and
// this is a no-op — which is exactly why F10 is not fixed until minting flips to
// v3. Logout on a v2 cookie still deletes the browser copy and still leaves a
// captured value working; that residual is called out in the PR body rather than
// papered over here.
func (s *HubServer) revokeHubSessionCookie(value string) bool {
	return s.revokeHubSessionCookieAt(value, time.Now())
}

// revokeHubSessionCookieAt is revokeHubSessionCookie with the clock made
// explicit. Revocation is entirely a statement about time — "reject this until
// its expiry" — so a hidden time.Now() makes the whole thing untestable except
// against the wall clock.
func (s *HubServer) revokeHubSessionCookieAt(value string, now time.Time) bool {
	if s == nil || s.revokedSessions == nil || value == "" {
		return false
	}
	sid := hubCookieSessionID(value)
	if sid == "" {
		return false
	}
	// Revoke until the cookie's own signed expiry: past that point the expiry
	// check rejects it anyway and the entry is dead weight.
	exp := hubCookieExpiry(value)
	if exp <= 0 {
		return false
	}
	if !s.revokedSessions.revoke(sid, exp, now) {
		return false
	}
	s.revokedSessions.pruneExpired(now)
	s.saveRevokedSessions()
	s.logger.Info("audit: hub session revoked", "sid", sid)
	return true
}

// hubSessionRevokedLookup adapts the store to the hubSessionRevokedFunc the
// cookie verifier takes.
func (s *HubServer) hubSessionRevokedLookup(now time.Time) hubSessionRevokedFunc {
	if s == nil || s.revokedSessions == nil {
		return nil
	}
	return func(sid string) bool { return s.revokedSessions.isRevoked(sid, now) }
}

// verifyHubUserCookie is the hub-side entry point: it accepts v3, v2 or legacy
// cookies and, for v3, additionally enforces signed expiry AND revocation.
//
// Spokes deliberately get less: their proxy enforces the signature and the
// signed expiry, but has no revocation store, so a revoked session can still
// reach a spoke until its expiry. Closing that needs the spoke to ask the hub,
// which is a network dependency on the terminal path and out of scope here.
func (s *HubServer) verifyHubUserCookie(value string) (string, bool) {
	if s == nil {
		return "", false
	}
	now := time.Now()
	return verifyHubUserCookieEitherAt(s.sessionPublicKey(), s.sessionKey(), value, now, s.hubSessionRevokedLookup(now))
}
