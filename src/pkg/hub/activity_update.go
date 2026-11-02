package hub

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// Activity-only updates (#15963). A spoke sends one on a stage-tracker
// publish, at most every 15 s, so the hub-wide Activity Monitor
// (#15951) shows a change before the next 2-minute heartbeat. The owner
// decision on #15861 was to reuse the heartbeat endpoint and bearer, so the
// update is a POST /api/heartbeat marked with spoke.ActivityUpdateHeader and
// carrying only the activity block. It touches nothing but the in-memory
// activity store: the registry entry, timeline and every other heartbeat
// side effect are left to the full beat.

// activityUpdateGuard routes marked activity-only updates to
// handleActivityUpdate and passes every other request through untouched.
func (s *HubServer) activityUpdateGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/heartbeat" && r.Header.Get(spoke.ActivityUpdateHeader) != "" {
			s.handleActivityUpdate(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleActivityUpdate authenticates an activity-only update exactly like a
// heartbeat (per-hive bearer bound to the claimed hive_id) and records its
// digest for a hive already in the registry. The response is a signed
// HeartbeatResponse carrying only ok and the acknowledged activity_etag.
func (s *HubServer) handleActivityUpdate(w http.ResponseWriter, r *http.Request) {
	presentedBearer := ""
	if s.hubSecret != "" {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		presentedBearer = strings.TrimPrefix(auth, "Bearer ")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadBytes))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var payload spoke.ActivityUpdatePayload
	if err := json.Unmarshal(body, &payload); err != nil || payload.Update == nil {
		http.Error(w, "invalid activity update", http.StatusBadRequest)
		return
	}
	hiveID := sanitizeHeartbeatField(payload.Update.HiveID)
	if hiveID == "" || !isValidName(hiveID) {
		http.Error(w, "invalid hive_id", http.StatusBadRequest)
		return
	}
	if s.hubSecret != "" && !s.verifyHeartbeatBearer(presentedBearer, hiveID) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if payload.Update.Activity == nil {
		http.Error(w, "activity required", http.StatusBadRequest)
		return
	}
	// Only a hive the registry already holds: an update must not create
	// state for a hive that never beat or was removed.
	if !s.hiveRegistered(hiveID) {
		http.Error(w, "unknown hive; send a heartbeat first", http.StatusNotFound)
		return
	}
	resp := HeartbeatResponse{OK: true}
	resp.ActivityETag = s.activity.record(hiveID, payload.Update.Activity, time.Now().UTC())
	resp.ActivityEventsAck = s.activity.eventsAck(hiveID)
	w.Header().Set("Content-Type", "application/json")
	s.signHeartbeatResponse(w, &resp, hiveID)
	data, _ := json.Marshal(resp)
	_, _ = w.Write(data)
}

// hiveRegistered reports whether the registry holds hiveID. It takes s.mu
// for reading only and releases it before the caller touches the activity
// store (a leaf lock).
func (s *HubServer) hiveRegistered(hiveID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID == hiveID {
			return true
		}
	}
	return false
}
