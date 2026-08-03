package hub

import (
	"net/http"
	"time"
)

// spokeRestartDeliveryWindow is how long an armed restart keeps being
// delivered after RequestedRestartAt. It must be long enough that EVERY
// instance reporting as the hive gets at least one beat inside it (beats are
// 1–2 minutes apart), because the whole point is reaching a duplicate
// instance whose existence the hub can't otherwise act on. The spoke-side
// uptime guard (spokeRestartMinUptime in cmd/hive) keeps a process that
// already restarted from acting on the same window twice.
const spokeRestartDeliveryWindow = 5 * time.Minute

// reporterConflictWindow is how recently two reporters must have alternated
// for the conflict to still be considered live. A rollout leaves exactly one
// reporter, so the alternation stops and the conflict ages out on its own.
const reporterConflictWindow = 15 * time.Minute

// reporterFlipsToConfirm is how many A→B→A alternations are required before
// two reporters are declared in conflict. One change of reporter is a normal
// rollout (old pod's last beat, new pod's first); a return to a PREVIOUS
// reporter is the signature of two live instances.
const reporterFlipsToConfirm = 2

// reporterFlipState is the per-hive memory behind noteReporter.
type reporterFlipState struct {
	last     string // reporter of the most recent beat
	prev     string // reporter seen immediately before last
	flips    int    // times the reporter returned to a previously seen one
	lastFlip time.Time
}

// noteReporter records which spoke instance sent this beat and returns a
// non-empty "a ↔ b" description while two instances are alternating as the
// same hive. Empty reporter (a spoke too old to report one) is UNKNOWN and
// never counts for or against a conflict — the state is left untouched.
func (s *HubServer) noteReporter(hiveID, reporter string) string {
	if reporter == "" {
		return ""
	}
	s.reporterMu.Lock()
	defer s.reporterMu.Unlock()
	if s.reporterSeen == nil {
		s.reporterSeen = make(map[string]*reporterFlipState)
	}
	st := s.reporterSeen[hiveID]
	if st == nil {
		st = &reporterFlipState{last: reporter}
		s.reporterSeen[hiveID] = st
		return ""
	}
	if reporter != st.last {
		if reporter == st.prev {
			// The reporter came BACK — both instances are alive.
			st.flips++
		} else {
			// A reporter never seen adjacent to this one: treat as a fresh
			// handover (rollout), not a conflict.
			st.flips = 0
		}
		st.prev, st.last = st.last, reporter
		st.lastFlip = time.Now()
	}
	if st.flips >= reporterFlipsToConfirm && time.Since(st.lastFlip) < reporterConflictWindow {
		a, b := st.prev, st.last
		if a > b {
			a, b = b, a
		}
		return a + " ↔ " + b
	}
	if time.Since(st.lastFlip) >= reporterConflictWindow {
		st.flips = 0
	}
	return ""
}

// restartSpokeForHeartbeat reports whether an armed restart should ride this
// beat. It delivers to EVERY beat inside the window — all instances reporting
// as this hive must hear it — and clears the armed record on the first beat
// after the window so the file does not stay armed forever.
func (s *HubServer) restartSpokeForHeartbeat(hiveID string) bool {
	h := loadSaaSHive(hiveID)
	if h == nil || h.RequestedRestartAt == "" {
		return false
	}
	armedAt, err := time.Parse(time.RFC3339, h.RequestedRestartAt)
	if err != nil {
		// Unparseable is unrecoverable — clear it rather than deliver forever.
		h.RequestedRestartAt = ""
		if saveErr := saveSaaSHive(h); saveErr != nil {
			s.logger.Warn("spoke restart: failed to clear bad timestamp", "hive_id", hiveID, "error", saveErr)
		}
		return false
	}
	if time.Since(armedAt) >= spokeRestartDeliveryWindow {
		h.RequestedRestartAt = ""
		if err := saveSaaSHive(h); err != nil {
			s.logger.Warn("spoke restart: failed to clear expired request", "hive_id", hiveID, "error", err)
		}
		s.logger.Info("spoke restart window closed", "hive_id", hiveID, "armed_at", armedAt.Format(time.RFC3339))
		return false
	}
	return true
}

// handleRestartSpoke arms a rolling restart for one hive's spoke:
// POST /api/saas/hives/{id}/restart-spoke
//
// WHY AN OPERATOR NEEDS THIS. When two spoke instances report as one hive
// (ConflictingReporters — a stale ReplicaSet pod, an orphaned duplicate
// deployment), the dashboard flips state every beat and one instance is
// usually broken in a way that pollutes the registry. The hub often cannot
// reach the cluster to delete anything; the heartbeat is the only channel.
// A restart delivered to every instance makes each patch its own Deployment
// (RolloutRestartSelf): a stale pod is replaced and never comes back, while
// the healthy instance suffers one rolling restart.
func (s *HubServer) handleRestartSpoke(w http.ResponseWriter, r *http.Request) {
	if s.getAuthUser(r) != hubAdminUsername {
		http.Error(w, `{"error":"admin access required"}`, http.StatusForbidden)
		return
	}
	hiveID := r.PathValue("id")
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	h.RequestedRestartAt = time.Now().UTC().Format(time.RFC3339)
	if err := saveSaaSHive(h); err != nil {
		s.logger.Error("failed to arm spoke restart", "hive_id", hiveID, "error", err)
		http.Error(w, `{"error":"could not arm the restart"}`, http.StatusInternalServerError)
		return
	}
	s.logger.Info("spoke restart armed by operator", "hive_id", hiveID)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"armed","detail":"every spoke instance reporting as this hive will rolling-restart on its next heartbeat within the delivery window"}`))
}
