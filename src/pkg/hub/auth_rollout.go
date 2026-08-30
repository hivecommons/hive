package hub

import (
	"encoding/json"
	"net/http"
	"time"
)

// Rollout readiness telemetry for the N1/N2 compatibility lanes (issue #3234).
//
// N1 (per-hive heartbeat bearer) and N2 (Ed25519 session cookie) both shipped
// as verify-both / mint-new: the hub accepts the new credential OR the legacy
// one, and issues only the new one. That was required — a flag-day cutover
// would have 401'd every spoke in the fleet at once, the failure mode #2773
// documented for the v2-hub/v4-spoke split.
//
// But each legacy lane IS the vulnerability it replaced. The fleet-wide
// heartbeat bearer does not bind identity, so an un-rolled spoke can still
// claim any hive_id; the symmetric session key that verifies a cookie can also
// mint one, including an admin cookie. They must be removed.
//
// The blocker was observability: "has every spoke re-provisioned?" had no
// answer short of inspecting each Deployment by hand, so the lanes would have
// lingered indefinitely — which is how a temporary compatibility path becomes
// permanent. This records which format each spoke actually presents, so the
// decision is made on evidence.
//
// STATUS: the N1/F2 heartbeat lane is DELETED — verifyHeartbeatBearer now
// accepts only the per-hive bearer, so PerHiveBearer is necessarily true for
// every hive that authenticates at all. This file is retained because (a) the
// N2 session-cookie lane is still mid-rollout and (b) GET
// /api/saas/admin/auth-rollout remains the operator's post-cutover view: a hive
// that vanishes from the heartbeat totals is one that is now failing auth.

// authRolloutEntry is the last-seen credential format for one hive.
type authRolloutEntry struct {
	// PerHiveBearer is true when this hive's most recent heartbeat used the
	// identity-bound bearer (N1) rather than the fleet-wide legacy one.
	PerHiveBearer bool `json:"per_hive_bearer"`
	// LastSeen is when that observation was made.
	LastSeen time.Time `json:"last_seen"`
}

// noteHeartbeatAuthPath records which bearer format hiveID just authenticated
// with. Called on every accepted heartbeat; cheap and lock-scoped.
//
// Deliberately records the CURRENT observation rather than latching "has ever
// used per-hive": a spoke can roll back, and a stale true would make the fleet
// look ready when it is not. Readiness must mean "every hive is per-hive NOW".
func (s *HubServer) noteHeartbeatAuthPath(hiveID string, perHive bool) {
	if s == nil || hiveID == "" {
		return
	}
	s.authRolloutMu.Lock()
	defer s.authRolloutMu.Unlock()
	if s.authRolloutSeen == nil {
		s.authRolloutSeen = make(map[string]authRolloutEntry)
	}
	s.authRolloutSeen[hiveID] = authRolloutEntry{PerHiveBearer: perHive, LastSeen: time.Now()}
}

// AuthRolloutStatus summarizes fleet readiness for removing the compat lanes.
type AuthRolloutStatus struct {
	// TotalHives is how many distinct hives have been observed heartbeating.
	TotalHives int `json:"total_hives"`
	// PerHiveBearer is how many of those last used the N1 identity-bound bearer.
	PerHiveBearer int `json:"per_hive_bearer"`
	// LegacyBearer is how many last used the fleet-wide legacy bearer. While
	// this is non-zero, removing the N1 compat lane WILL 401 those spokes.
	LegacyBearer int `json:"legacy_bearer"`
	// LegacyHives names the laggards, so an operator can go re-provision them
	// rather than guessing which ones are holding up the removal.
	LegacyHives []string `json:"legacy_hives,omitempty"`
	// StaleAfter is the cutoff beyond which an observation is ignored: a hive
	// that has not beaten recently is not evidence of anything.
	StaleAfter string `json:"stale_after"`
	// HeartbeatLaneReady is true when every recently-seen hive is on the
	// per-hive bearer — i.e. the N1 legacy lane can be deleted.
	//
	// NOTE this covers the HEARTBEAT lane only. The N2 session-cookie lane has a
	// second precondition this cannot observe: existing browser cookies must
	// also have aged out (cookieMaxAgeDays, currently 7). Removing that lane
	// early logs every active user out rather than breaking a spoke.
	HeartbeatLaneReady bool `json:"heartbeat_lane_ready"`

	// PerHiveEnv is the Deployment-SOURCED convergence view for the five
	// per-hive security env vars (perhive_env_reconcile.go).
	//
	// It is embedded here rather than served from a new endpoint so an operator
	// has ONE readiness page — but the two halves of this response are sourced
	// differently on purpose, and the difference matters for the master-secret
	// cutover this gates.
	//
	// Everything above comes from heartbeat observations and is subject to
	// authRolloutStaleAfter: a hive that has not beaten in 24h simply leaves
	// the totals, and as the constant's own comment records, this signal cannot
	// distinguish "hive absent" from "hive never existed". A paused spoke
	// therefore drops silently out of the denominator and the fleet reads ready
	// while that spoke is unconverged.
	//
	// PerHiveEnv is immune to that: its counts come from the hub reading each
	// spoke's Deployment itself. A paused hive still has a Deployment, so it is
	// still read, still counted, and still blocks convergence. Gate the
	// master-secret removal on THESE numbers, not on the heartbeat totals.
	PerHiveEnv PerHiveEnvStatus `json:"per_hive_env"`
}

// authRolloutStaleAfter bounds how old an observation may be and still count.
// A hive that has not heartbeated within this window is treated as absent, not
// as legacy — otherwise a long-deleted hive would block the removal forever.
//
// 24h is chosen against the two ways this can be wrong, which fail in opposite
// directions:
//
//   - TOO SHORT and a hive that is merely paused, mid-upgrade, or on a
//     temporarily unreachable cluster drops out of the denominator. The fleet
//     then reads ready while that spoke is still on the legacy bearer, and
//     removing the lane 401s it the moment it comes back.
//   - TOO LONG and every hive ever deleted keeps counting as a legacy laggard,
//     so readiness never arrives and the compat lane becomes permanent — the
//     precise failure this whole signal exists to prevent.
//
// Spokes beat on the order of a minute, so 24h is ~1000x the cadence: far past
// any transient blip, but short enough that a decommissioned hive clears within
// a day. If the fleet ever adopts long-lived paused hives that stop beating
// entirely, revisit this — a paused hive should ideally be excluded explicitly
// rather than by timeout.
const authRolloutStaleAfter = 24 * time.Hour

// AuthRolloutReadiness reports whether the N1 heartbeat compat lane can be
// removed. Fails CLOSED: with no observations at all it reports not-ready,
// because "no evidence" must never read as "safe to remove".
func (s *HubServer) AuthRolloutReadiness() AuthRolloutStatus {
	out := AuthRolloutStatus{StaleAfter: authRolloutStaleAfter.String()}
	if s == nil {
		return out
	}
	cutoff := time.Now().Add(-authRolloutStaleAfter)
	// Scoped, NOT deferred: PerHiveEnvSnapshot below takes a different mutex
	// (perHiveEnvMu), and holding two hub locks at once — even in a consistent
	// order today — is how a future caller acquires them the other way round
	// and deadlocks the readiness endpoint. Release this one first.
	func() {
		s.authRolloutMu.RLock()
		defer s.authRolloutMu.RUnlock()
		for id, e := range s.authRolloutSeen {
			if e.LastSeen.Before(cutoff) {
				continue
			}
			out.TotalHives++
			if e.PerHiveBearer {
				out.PerHiveBearer++
			} else {
				out.LegacyBearer++
				out.LegacyHives = append(out.LegacyHives, id)
			}
		}
	}()
	out.HeartbeatLaneReady = out.TotalHives > 0 && out.LegacyBearer == 0
	out.PerHiveEnv = s.PerHiveEnvSnapshot()
	return out
}

// handleAuthRollout serves the readiness summary. Admin-only: it enumerates
// hive IDs, and while that is not secret it is fleet-shaped information with no
// reason to be public.
func (s *HubServer) handleAuthRollout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.AuthRolloutReadiness())
}
