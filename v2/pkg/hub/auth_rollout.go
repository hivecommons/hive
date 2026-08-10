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
}

// authRolloutStaleAfter bounds how old an observation may be and still count.
// A hive that has not heartbeated within this window is treated as absent, not
// as legacy — otherwise a long-deleted hive would block the removal forever.
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
	out.HeartbeatLaneReady = out.TotalHives > 0 && out.LegacyBearer == 0
	return out
}

// handleAuthRollout serves the readiness summary. Admin-only: it enumerates
// hive IDs, and while that is not secret it is fleet-shaped information with no
// reason to be public.
func (s *HubServer) handleAuthRollout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.AuthRolloutReadiness())
}
