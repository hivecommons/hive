package hub

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// handleSpokeBreaker queues a whole-hive pause or resume for one spoke:
// POST /api/saas/hives/{id}/breaker  {"engaged": true|false}
//
// It is the hub-side face of the spoke's own top-bar "running ⏸" button, so an
// operator managing several spokes can pause one from the hub's per-hive menu
// without opening its dashboard. No new pause mechanism: the spoke applies the
// instruction through the same EngageBreaker/ReleaseBreaker path its
// /api/breaker/{engage,release} endpoints use. The heartbeat is the channel
// because it is the only one that reaches every spoke, including those on
// clusters the hub cannot touch.
//
// Owner-or-admin, mirroring the spoke endpoints' owner-role gate.
func (s *HubServer) handleSpokeBreaker(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	hiveID := r.PathValue("id")
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) && !isHubAdmin(username) {
		http.Error(w, `{"error":"only the owner can pause or resume this hive"}`, http.StatusForbidden)
		return
	}
	var body struct {
		Engaged *bool `json:"engaged"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil || body.Engaged == nil {
		http.Error(w, `{"error":"body must be {\"engaged\": true|false}"}`, http.StatusBadRequest)
		return
	}
	action := spoke.BreakerRelease
	if *body.Engaged {
		action = spoke.BreakerEngage
	}
	// A newer request replaces an undelivered older one: the operator's last
	// click is the state they want.
	h.PendingBreaker = action
	h.PendingBreakerBy = username
	if err := saveSaaSHive(h); err != nil {
		s.logger.Error("failed to queue spoke breaker", "hive_id", hiveID, "error", err)
		http.Error(w, `{"error":"could not queue the request"}`, http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: spoke breaker queued", "hive_id", hiveID, "action", action, "by", username)
	verb := "pause"
	if action == spoke.BreakerRelease {
		verb = "resume"
	}
	s.recordTimeline(hiveID, TimelineAgentsChanged, fmt.Sprintf("hive %s requested from hub", verb), username)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "queued", "action": action})
}

// pendingBreakerForHeartbeat returns the queued breaker instruction for this
// hive ("" when none) and clears it, so it is delivered exactly once — the
// same one-shot contract as pendingAgentRestartResetsForHeartbeat. Engage and
// release are idempotent on the spoke, so a duplicate instance hearing it too
// is harmless.
func (s *HubServer) pendingBreakerForHeartbeat(hiveID string) string {
	h := loadSaaSHive(hiveID)
	if h == nil || h.PendingBreaker == "" {
		return ""
	}
	action, by := h.PendingBreaker, h.PendingBreakerBy
	h.PendingBreaker = ""
	h.PendingBreakerBy = ""
	if err := saveSaaSHive(h); err != nil {
		// Not clearing would redeliver on every beat; withholding is safer than
		// toggling forever, and the operator can click again.
		s.logger.Warn("spoke breaker: failed to clear queued request; not delivering", "hive_id", hiveID, "error", err)
		return ""
	}
	if action != spoke.BreakerEngage && action != spoke.BreakerRelease {
		return ""
	}
	s.logger.Info("heartbeat: delivering spoke breaker", "hive_id", hiveID, "action", action, "requested_by", by)
	return action
}
