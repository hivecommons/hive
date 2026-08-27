package hub

// Time-limited access grants (#4150).
//
// An owner may attach an OPTIONAL expiry to any Manage Access grant. The
// expiry rides on the user record next to the role itself (SaaSUser.HiveExpiry,
// hive ID → RFC3339 instant) so a grant and its lifetime always travel — and
// are persisted — together. No expiry means permanent, which keeps every
// pre-#4150 record byte-identical and behaviorally unchanged.
//
// Enforcement is two-layered, and the layers are deliberately independent:
//
//  1. ON-ACCESS (authoritative): loadSaaSUser prunes expired grants from the
//     in-memory record at READ time, on the wall clock. Every consumer of a
//     user's role — requireAuth-gated handlers, userIsHiveOwner, accessForHive,
//     the heartbeat's authorized-users push to spokes — resolves roles through
//     loadSaaSUser/listAllSaaSUsers, so an expired grant stops working at its
//     expiry instant whether or not the sweep below ever runs.
//  2. BACKGROUND SWEEP (persistence + audit): sweepExpiredAccessIfDue, ticked
//     from the hub's SHA-poller loop, rewrites the pruned records to disk and
//     stamps a timeline event so the revocation is visible in the hive's
//     Activity Timeline. It reads the RAW json files (not loadSaaSUser, whose
//     read-time prune would hide exactly the entries this sweep must persist).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// accessExpirySweepInterval throttles the expiry persistence sweep. The SHA
// poller ticks every 2 min but expiry granularity is a calendar date, so
// persisting/auditing at most every 10 min is plenty. NOTE this throttles
// PERSISTENCE AND THE TIMELINE EVENT ONLY — an expired grant is refused at
// read time by loadSaaSUser's prune whether or not this sweep ever runs.
const accessExpirySweepInterval = 10 * time.Minute

// accessExpiryDateLayout is the bare-date form the Manage Access date picker
// submits ("2026-09-01"). A bare date means "access is valid THROUGH that
// day, UTC" — it canonicalizes to 00:00 UTC of the following day.
const accessExpiryDateLayout = "2006-01-02"

// parseAccessExpiry canonicalizes an owner-supplied expiry into a UTC instant.
// It accepts either a bare date (valid through that day, UTC) or a full
// RFC3339 timestamp. The zero time with a nil error is never returned for a
// non-empty input.
func parseAccessExpiry(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty expiry")
	}
	if d, err := time.Parse(accessExpiryDateLayout, raw); err == nil {
		return d.UTC().Add(24 * time.Hour), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("expiry must be YYYY-MM-DD or RFC3339: %w", err)
	}
	return t.UTC(), nil
}

// accessGrantExpired reports whether the stored expiry string marks a grant
// as expired at now. A missing/blank expiry is permanent; an UNPARSEABLE
// expiry is treated as permanent rather than as expired, so a corrupt field
// can never silently lock a user out — it merely stops limiting them, which
// the owner can see and fix in Manage Access.
func accessGrantExpired(expiry string, now time.Time) bool {
	expiry = strings.TrimSpace(expiry)
	if expiry == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, expiry)
	if err != nil {
		return false
	}
	return !now.Before(t)
}

// pruneExpiredHiveGrants removes every expired grant from u (both the role in
// Hives and its HiveExpiry entry) and drops orphaned expiry entries whose
// grant is already gone. It returns the hive IDs whose grants were revoked,
// so the sweep can stamp per-hive timeline events. It never writes to disk.
func pruneExpiredHiveGrants(u *SaaSUser, now time.Time) []string {
	if u == nil || len(u.HiveExpiry) == 0 {
		return nil
	}
	var revoked []string
	for hiveID, expiry := range u.HiveExpiry {
		if _, granted := u.Hives[hiveID]; !granted {
			// Orphaned expiry (grant removed through another path): clean it up
			// but do not report a revocation — there was nothing to revoke.
			delete(u.HiveExpiry, hiveID)
			continue
		}
		if accessGrantExpired(expiry, now) {
			delete(u.Hives, hiveID)
			delete(u.HiveExpiry, hiveID)
			revoked = append(revoked, hiveID)
		}
	}
	return revoked
}

// sweepExpiredAccessIfDue runs the expiry persistence sweep only if at least
// accessExpirySweepInterval has elapsed since the last run. Same throttle
// pattern (and same guarding mutex) as reconcileNetAdminIfDue: poller-loop-only
// state. Safe to call from the poller loop every tick.
func (s *HubServer) sweepExpiredAccessIfDue() {
	s.clusterUnreachableMu.Lock()
	due := s.lastAccessExpirySweep.IsZero() ||
		time.Since(s.lastAccessExpirySweep) >= accessExpirySweepInterval
	if due {
		s.lastAccessExpirySweep = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		return
	}
	s.sweepExpiredAccess(time.Now())
}

// sweepExpiredAccess persists the revocation of every expired grant and stamps
// a timeline event on each affected hive. It reads the raw user files —
// loadSaaSUser's read-time prune (layer 1) would hide exactly the expired
// entries this sweep exists to persist and audit.
func (s *HubServer) sweepExpiredAccess(now time.Time) {
	entries, err := os.ReadDir(saasUsersDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(saasUsersDir, e.Name()))
		if err != nil {
			continue
		}
		var u SaaSUser
		if json.Unmarshal(data, &u) != nil {
			continue
		}
		roles := map[string]string{}
		for hiveID := range u.HiveExpiry {
			roles[hiveID] = u.Hives[hiveID]
		}
		revoked := pruneExpiredHiveGrants(&u, now)
		if len(revoked) == 0 {
			continue
		}
		if err := saveSaaSUser(&u); err != nil {
			s.logger.Warn("access expiry sweep: save failed", "user", u.GitHubUsername, "error", err)
			continue
		}
		for _, hiveID := range revoked {
			s.logger.Info("audit: access expired", "hive", hiveID, "target", u.GitHubUsername, "role", roles[hiveID])
			s.recordTimeline(hiveID, TimelineAccess,
				fmt.Sprintf("access for %s expired (was %s)", u.GitHubUsername, roles[hiveID]), "system")
		}
	}
}
