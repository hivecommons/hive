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
// AUDIT F15: two hard bounds on the persisted store, both of which hold even if
// the verify-before-revoke check in handleLogout is ever weakened or bypassed.
// Defence in depth: the store's own invariants must not depend on its caller.
const (
	// maxRevokedSessionExpiry clamps how far in the future a stored entry may
	// sit. An entry's expiry comes from the cookie's SIGNED claims, so it is not
	// attacker-chosen once signatures are verified — but a bug that let an
	// unverified value through, or a future minting change with a longer TTL,
	// would otherwise pin entries indefinitely. Sized off the longest session the
	// hub will ever mint (cookieSessionTTL) with slack for clock skew: nothing
	// legitimate can exceed it, and anything that claims to is lying.
	maxRevokedSessionExpiry = cookieSessionTTL + 24*time.Hour

	// maxRevokedSessions hard-caps the entry count so the file, the startup parse
	// and the memory footprint stay bounded no matter what. The natural bound is
	// "sessions revoked within one cookie lifetime", which for this fleet is
	// orders of magnitude below this number; reaching it means something is
	// wrong, so it is logged rather than silently absorbed.
	maxRevokedSessions = 100_000
)

func (r *revokedSessions) revoke(sid string, exp int64, now time.Time) bool {
	if r == nil || sid == "" || exp <= now.Unix() {
		return false
	}
	// Clamp to the mint horizon. An entry is only useful until the cookie it
	// names fails its own expiry check; anything beyond that is dead weight that
	// merely occupies the store for longer.
	if maxExp := now.Add(maxRevokedSessionExpiry).Unix(); exp > maxExp {
		exp = maxExp
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.sid[sid]; ok && existing >= exp {
		return false
	}
	// The cap applies only to NEW session IDs — extending an entry that already
	// exists adds nothing to the store's size, and refusing it would be the
	// unsafe direction (it would leave a session un-revoked).
	if _, exists := r.sid[sid]; !exists && len(r.sid) >= maxRevokedSessions {
		return false
	}
	r.sid[sid] = exp
	return true
}

// atCapacity reports whether the store has hit its hard cap, so the caller can
// log it. Separate from revoke so the lock discipline stays simple.
func (r *revokedSessions) atCapacity() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sid) >= maxRevokedSessions
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
		if s.revokedSessions.atCapacity() {
			// AUDIT F15: the hard cap refused an entry. This should be
			// unreachable in normal operation, so it is an operational signal,
			// not a routine outcome — a session the user asked to revoke has NOT
			// been revoked.
			s.logger.Error("revocation store at capacity — session NOT revoked",
				"cap", maxRevokedSessions)
		}
		return false
	}
	s.revokedSessions.pruneExpired(now)
	// AUDIT F15: coalesce the disk write, but stay DURABLE.
	//
	// The tempting version of this is "mark dirty, flush on a timer, return" —
	// which is wrong here, and the F10 restart test is what says so. Revocation's
	// entire value proposition is that it survives a roll: the hub restarts
	// several times a day, so a revocation that is only in memory un-revokes
	// itself on a schedule an attacker can simply wait for. A purely deferred
	// flush reintroduces exactly the bug F10 was filed for, just with a 2-second
	// window instead of an unbounded one.
	//
	// So the write still happens before this returns. What coalescing buys is
	// that CONCURRENT revocations collapse into one write rather than one each:
	// the flush is idempotent and drains whatever is pending, so a burst of N
	// logouts costs far fewer than N full-file rewrites without any of them
	// returning before their own entry is on disk.
	s.markRevokedSessionsDirty()
	s.flushRevokedSessions()
	s.logger.Info("audit: hub session revoked", "sid", sid)
	return true
}

// markRevokedSessionsDirty records that the in-memory set has changes not yet on
// disk. Separate from the flush so several revocations can accumulate into one
// pending write.
func (s *HubServer) markRevokedSessionsDirty() {
	if s == nil {
		return
	}
	s.revokedSaveMu.Lock()
	s.revokedSaveDirty = true
	s.revokedSaveMu.Unlock()
}

// flushRevokedSessions persists the set if anything is pending, and is a no-op
// otherwise. Safe to call unconditionally and from any goroutine.
//
// The dirty flag is cleared BEFORE the write, under the lock, and the write
// snapshots the whole set. That ordering is what makes concurrent revocations
// coalesce: a revocation that lands while a flush is in progress re-marks the
// set dirty, and either it was already captured by the in-flight snapshot
// (harmless — the next flush is a cheap no-op re-write) or it is picked up by
// the next flush. The one thing that must never happen is a change being
// dropped, and clearing-before-writing cannot drop one; clearing after could.
func (s *HubServer) flushRevokedSessions() {
	if s == nil || s.revokedSessions == nil {
		return
	}
	s.revokedSaveMu.Lock()
	dirty := s.revokedSaveDirty
	s.revokedSaveDirty = false
	s.revokedSaveMu.Unlock()
	if dirty {
		s.saveRevokedSessions()
	}
}

// hubSessionRevokedLookup adapts the store to the hubSessionRevokedFunc the
// cookie verifier takes.
func (s *HubServer) hubSessionRevokedLookup(now time.Time) hubSessionRevokedFunc {
	if s == nil || s.revokedSessions == nil {
		return nil
	}
	return func(sid string) bool { return s.revokedSessions.isRevoked(sid, now) }
}

// verifyHubUserCookie is the hub-side entry point: it accepts v3 or v2 cookies
// and, for v3, additionally enforces signed expiry AND revocation.
//
// Spokes deliberately get less: their proxy enforces the signature and the
// signed expiry, but has no revocation store, so a revoked session can still
// reach a spoke until its expiry. Closing that needs the spoke to ask the hub,
// which is a network dependency on the terminal path and out of scope here.
//
// ROTATION (master-key-rotation.md, follow-on PR #1): the cookie is checked
// against every master GENERATION the hub still accepts, not just the current
// one. Without this, rotating the master logs every user out at the instant of
// rotation — the session cookie is the longest-lived artifact bound to a
// generation, which is why defaultVerifyWindow matches cookieMaxAgeDays.
//
// On a hub that has never rotated the set holds exactly one generation whose
// secret IS s.hubSecret, so this is byte-identical to the single-master call it
// replaces. keyGenerations is nil only in hand-built test servers; the fallback
// below keeps those on the old path rather than failing them closed, which
// would be an authentication change disguised as a nil check.
func (s *HubServer) verifyHubUserCookie(value string) (string, bool) {
	if s == nil {
		return "", false
	}
	now := time.Now()
	revoked := s.hubSessionRevokedLookup(now)
	if s.keyGenerations != nil {
		u, _, ok := verifyHubUserCookieAcrossGenerations(s.keyGenerations, value, now, revoked)
		return u, ok
	}
	return verifyHubUserCookieEitherAt(s.sessionPublicKey(), s.sessionKey(), value, now, revoked)
}
