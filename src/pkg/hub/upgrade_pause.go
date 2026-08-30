package hub

// Admin-only upgrade kill switch.
//
// Two independent pauses, both admin-only and both DURABLE across hub restarts
// (the hub auto-rolls itself frequently, so any in-memory-only flag would be
// silently wiped by the very machinery it is meant to stop):
//
//   - hub:    freezes the hub's own self-upgrade machinery. While set, the
//     poll-loop auto-upgrade never triggers and the manual
//     /api/saas/hub/upgrade endpoint refuses with 409. The hub stays on its
//     current build regardless of new tags.
//
//   - spokes: freezes EVERY automatic image-change delivery path to spokes,
//     fleet-wide. The pause must be honoured by ALL of them or it is a lie:
//     triggerAutoUpgrades (kubectl restarts + arming heartbeatUpgrade), the
//     heartbeat handler's UpgradeTo / SwitchToTag sends, the tracked-channel
//     re-arm inside the heartbeat handler, and the orphaned-upgrade sweep's
//     heartbeatUpgrade re-arm. Heartbeats are otherwise answered normally, and
//     armed in-memory targets are left in place — pause suppresses DELIVERY,
//     it does not destroy state, so resuming picks up exactly where the fleet
//     left off. Manual upgrade/switch API requests while paused get an
//     explicit 409 naming who paused and when, never a silent queue.
//
// State is one small JSON file on the hub's durable data dir, next to the
// other hub-owned SaaS state (hub-auto-upgrade, hub-generations.json), loaded
// lazily on first use so a fresh post-restart process starts from the
// persisted truth.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// upgradePausePath is the durable home of the kill-switch state. A var (not a
// const) so tests can redirect it at a temp path, exactly like
// hubAutoUpgradePath and hubGenerationsPath.
var upgradePausePath = "/data/saas/upgrade-pause.json"

// upgradePauseFileMode matches the other non-secret /data/saas JSON files
// (registry, hive records): the state is operational, not key material.
const upgradePauseFileMode = 0o644

// Pause targets accepted by the API and used as the audit-log discriminator.
const (
	upgradePauseTargetHub    = "hub"
	upgradePauseTargetSpokes = "spokes"
)

// UpgradePauseSwitch is one of the two toggles: whether it is engaged, and who
// flipped it last, when (RFC3339). By/At record the LAST flip in either
// direction so a resume is attributable too.
type UpgradePauseSwitch struct {
	Paused bool   `json:"paused"`
	By     string `json:"by,omitempty"`
	At     string `json:"at,omitempty"`
}

// UpgradePauseState is the full persisted kill-switch state.
type UpgradePauseState struct {
	Hub    UpgradePauseSwitch `json:"hub"`
	Spokes UpgradePauseSwitch `json:"spokes"`
}

type UpgradePauseEvent struct {
	Target string
	Paused bool
	By     string
	At     string
}

func (s *HubServer) SetUpgradePauseObserver(fn func(UpgradePauseEvent)) {
	s.upgradePauseMu.Lock()
	defer s.upgradePauseMu.Unlock()
	s.upgradePauseObserver = fn
}

func (s *HubServer) emitUpgradePause(event UpgradePauseEvent) {
	s.upgradePauseMu.Lock()
	fn := s.upgradePauseObserver
	s.upgradePauseMu.Unlock()
	if fn != nil {
		go fn(event)
	}
}

// ensureUpgradePauseLoadedLocked loads the persisted state exactly once per
// process. Callers must hold s.upgradePauseMu. A missing file means "nothing
// paused" — the safe default and the pre-feature behaviour. An unreadable or
// corrupt file is logged and treated the same way rather than wedging the hub.
func (s *HubServer) ensureUpgradePauseLoadedLocked() {
	if s.upgradePauseLoaded {
		return
	}
	s.upgradePauseLoaded = true
	data, err := os.ReadFile(upgradePausePath)
	if err != nil {
		return
	}
	var st UpgradePauseState
	if err := json.Unmarshal(data, &st); err != nil {
		if s.logger != nil {
			s.logger.Warn("upgrade-pause state file is corrupt — treating as not paused",
				"path", upgradePausePath, "error", err)
		}
		return
	}
	s.upgradePause = st
}

// upgradePauseSnapshot returns a copy of the current kill-switch state.
func (s *HubServer) upgradePauseSnapshot() UpgradePauseState {
	s.upgradePauseMu.Lock()
	defer s.upgradePauseMu.Unlock()
	s.ensureUpgradePauseLoadedLocked()
	return s.upgradePause
}

// hubUpgradesPaused reports whether the hub self-upgrade kill switch is on,
// with the switch metadata for messages/logs.
func (s *HubServer) hubUpgradesPaused() (UpgradePauseSwitch, bool) {
	st := s.upgradePauseSnapshot()
	return st.Hub, st.Hub.Paused
}

// spokeUpgradesPaused reports whether the fleet-wide spoke kill switch is on,
// with the switch metadata for messages/logs.
func (s *HubServer) spokeUpgradesPaused() (UpgradePauseSwitch, bool) {
	st := s.upgradePauseSnapshot()
	return st.Spokes, st.Spokes.Paused
}

// upgradePauseRefusal is the human-readable 409 message for a manual request
// refused by an engaged kill switch: "<kind> upgrades are paused by <who>
// since <when>".
func upgradePauseRefusal(kind string, sw UpgradePauseSwitch) string {
	msg := kind + " upgrades are paused"
	if sw.By != "" {
		msg += " by " + sw.By
	}
	if sw.At != "" {
		msg += " since " + sw.At
	}
	return msg
}

// setUpgradePause flips one switch, stamps who/when, persists, and returns the
// new full state. The write happens under the mutex so two admins flipping
// different switches concurrently cannot lose each other's update.
func (s *HubServer) setUpgradePause(target string, paused bool, by string) (UpgradePauseState, error) {
	s.upgradePauseMu.Lock()
	defer s.upgradePauseMu.Unlock()
	s.ensureUpgradePauseLoadedLocked()
	sw := UpgradePauseSwitch{Paused: paused, By: by, At: time.Now().UTC().Format(time.RFC3339)}
	switch target {
	case upgradePauseTargetHub:
		s.upgradePause.Hub = sw
	case upgradePauseTargetSpokes:
		s.upgradePause.Spokes = sw
	}
	data, err := json.MarshalIndent(s.upgradePause, "", "  ")
	if err != nil {
		return s.upgradePause, err
	}
	if err := os.MkdirAll(filepath.Dir(upgradePausePath), 0o755); err != nil {
		return s.upgradePause, err
	}
	if err := os.WriteFile(upgradePausePath, data, upgradePauseFileMode); err != nil {
		return s.upgradePause, err
	}
	return s.upgradePause, nil
}

// handleGetUpgradePause returns the current kill-switch state. Admin-only
// (wrapped in requireAdmin at registration) — the same gate as the toggles it
// describes, and the who/when metadata names admins.
func (s *HubServer) handleGetUpgradePause(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.upgradePauseSnapshot())
}

// handleSetUpgradePause flips one switch. Admin-only (requireAdmin). Every
// flip — pause AND resume — is audit-logged with the acting admin.
func (s *HubServer) handleSetUpgradePause(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target string `json:"target"`
		Paused bool   `json:"paused"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	if body.Target != upgradePauseTargetHub && body.Target != upgradePauseTargetSpokes {
		http.Error(w, `{"error":"target must be \"hub\" or \"spokes\""}`, http.StatusBadRequest)
		return
	}
	username := s.getAuthUser(r)
	state, err := s.setUpgradePause(body.Target, body.Paused, username)
	if err != nil {
		s.logger.Error("failed to persist upgrade-pause state", "target", body.Target, "error", err)
		http.Error(w, `{"error":"failed to persist upgrade-pause state"}`, http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: upgrade pause toggled",
		"target", body.Target, "paused", body.Paused, "by", username)
	sw := state.Spokes
	if body.Target == upgradePauseTargetHub {
		sw = state.Hub
	}
	s.emitUpgradePause(UpgradePauseEvent{Target: body.Target, Paused: body.Paused, By: username, At: sw.At})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "state": state})
}
