package hub

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// ============================================================
// server.go — channel-switch durability across hub restarts (#3771, #3783)
// ============================================================
//
// heartbeatSwitchTag is in-memory, so a hub restart mid-switch used to drop
// the delivery and strand the hive on its old tag while the version pill kept
// promising the channel (the persisted tracked_channel survives a restart;
// the in-memory directive did not). #3771 added two behaviors these tests pin
// down:
//
//  1. RE-ARM: when no switch is armed but the persisted SaaSHive record
//     tracks a release channel and the spoke's REPORTED image tag disagrees
//     with it, the hub re-arms the switch from the record — but ONLY off a
//     reported ImageRef (never roll pods on a guess) and ONLY when the spoke
//     is not mid-upgrade (re-arming mid-roll would stamp a fresh restart-at
//     annotation and re-roll the pod every beat).
//
//  2. CHANNEL LATCH: a channel-switch "Upgrading" latch (UpgradeTarget =
//     "stable") is only satisfied by the reported image actually running the
//     channel tag. The pre-existing mutable-tag+quiet-beat heuristic
//     (floatingAtLatest) must NOT clear it — a spoke still on v4-latest
//     sending one quiet beat used to evaporate the switch from the dashboard
//     while the hive never moved (observed live 2026-08-13). Non-channel
//     targets keep the old clearing behavior.
//
// Each guard test here has a positive control in the same file proving the
// mechanism it guards actually fires when the guard's condition is absent —
// a test that passes because the whole path is dead would otherwise pass for
// the wrong reason.

// seedTrackedChannelHive persists a SaaSHive record whose TrackedChannel is
// "stable", the durable state the re-arm path reads. helperSetupTempDirs must
// already have redirected saasHivesDir.
func seedTrackedChannelHive(t *testing.T, id string) {
	t.Helper()
	if err := saveSaaSHive(&SaaSHive{ID: id, TrackedChannel: ReleaseChannelStable}); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}
}

// armedSwitchTag returns the currently armed switch tag for id, "" when none.
func armedSwitchTag(s *HubServer, id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.heartbeatSwitchTag[id]
}

// TestHeartbeatReArmsChannelSwitchFromTrackedChannel is the restart-recovery
// case AND the positive control for every guard test below: nothing is armed
// in memory (the "hub just restarted" state), the persisted record tracks
// "stable", and the spoke reports a non-upgrading beat with an image still on
// v4-latest. The hub must re-arm the switch from the record, instruct the
// spoke (switch_to_tag), and keep the re-armed tag in heartbeatSwitchTag so
// the next beat does not have to re-derive it.
func TestHeartbeatReArmsChannelSwitchFromTrackedChannel(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	seedTrackedChannelHive(t, "chanhive")

	rec := postHeartbeat(t, s, `{
		"hive_id":"chanhive","primary_repo":"r",
		"image_ref":"ghcr.io/hivecommons/hive:v4-latest",
		"git_branch":"v4","git_hash":"abc1234","upgrading":false
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"switch_to_tag":"stable"`) {
		t.Errorf("expected switch_to_tag \"stable\" in response, got %s", rec.Body.String())
	}
	if got := armedSwitchTag(s, "chanhive"); got != ReleaseChannelStable {
		t.Errorf("heartbeatSwitchTag = %q, want %q (re-arm must persist in memory)", got, ReleaseChannelStable)
	}
}

// TestHeartbeatNoReArmWithoutImageRef: an unknown image means the hub cannot
// tell drift from a stale cache — it must NOT roll pods on a guess. Same
// state as the positive control above except the beat omits image_ref.
func TestHeartbeatNoReArmWithoutImageRef(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	seedTrackedChannelHive(t, "chanhive")

	rec := postHeartbeat(t, s, `{
		"hive_id":"chanhive","primary_repo":"r",
		"git_branch":"v4","git_hash":"abc1234","upgrading":false
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "switch_to_tag") {
		t.Errorf("empty ImageRef must not re-arm a switch, got %s", rec.Body.String())
	}
	if got := armedSwitchTag(s, "chanhive"); got != "" {
		t.Errorf("heartbeatSwitchTag = %q, want empty (no re-arm on a guess)", got)
	}
}

// TestHeartbeatNoReArmWhileUpgrading: mid-roll, the OLD pod still reports the
// old tag; re-arming would stamp a fresh restart-at annotation and re-roll
// the pod on every beat forever. Same state as the positive control except
// the spoke reports upgrading=true.
func TestHeartbeatNoReArmWhileUpgrading(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	seedTrackedChannelHive(t, "chanhive")

	rec := postHeartbeat(t, s, `{
		"hive_id":"chanhive","primary_repo":"r",
		"image_ref":"ghcr.io/hivecommons/hive:v4-latest",
		"git_branch":"v4","git_hash":"abc1234","upgrading":true
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "switch_to_tag") {
		t.Errorf("mid-upgrade beat must not re-arm a switch, got %s", rec.Body.String())
	}
	if got := armedSwitchTag(s, "chanhive"); got != "" {
		t.Errorf("heartbeatSwitchTag = %q, want empty (no re-arm mid-roll)", got)
	}
}

// TestHeartbeatNoReArmWhenImageMatchesChannel: the spoke already runs the
// tracked channel's tag — there is no drift to heal, so re-arming would roll
// a healthy pod for nothing.
func TestHeartbeatNoReArmWhenImageMatchesChannel(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	seedTrackedChannelHive(t, "chanhive")

	rec := postHeartbeat(t, s, `{
		"hive_id":"chanhive","primary_repo":"r",
		"image_ref":"ghcr.io/hivecommons/hive:stable",
		"git_branch":"v4","git_hash":"abc1234","upgrading":false
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "switch_to_tag") {
		t.Errorf("image already on the channel must not re-arm, got %s", rec.Body.String())
	}
	if got := armedSwitchTag(s, "chanhive"); got != "" {
		t.Errorf("heartbeatSwitchTag = %q, want empty (already on channel)", got)
	}
}

// TestHeartbeatNoReArmWithoutTrackedChannel: a hive whose record tracks no
// channel (plain-branch hive) must never be switched, whatever image it
// reports. This pins the isReleaseChannel gate on the persisted record.
func TestHeartbeatNoReArmWithoutTrackedChannel(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	if err := saveSaaSHive(&SaaSHive{ID: "plainhive"}); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}

	rec := postHeartbeat(t, s, `{
		"hive_id":"plainhive","primary_repo":"r",
		"image_ref":"ghcr.io/hivecommons/hive:v4-latest",
		"git_branch":"v4","git_hash":"abc1234","upgrading":false
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "switch_to_tag") {
		t.Errorf("hive without a tracked channel must not be switched, got %s", rec.Body.String())
	}
	if got := armedSwitchTag(s, "plainhive"); got != "" {
		t.Errorf("heartbeatSwitchTag = %q, want empty (no tracked channel)", got)
	}
}

// TestChannelLatchSurvivesQuietMutableBeat: a channel-switch latch
// (UpgradeTarget = "stable") must NOT be cleared by the floatingAtLatest
// heuristic when the spoke's quiet beat still reports the OLD mutable tag
// (v4-latest ≠ stable). Before #3771 this exact beat evaporated the switch
// from the dashboard while the hive never moved.
//
// The git branch and hashes are deliberately unique to this test so the
// package-level latestSHAByBranch cache (shared across the test binary)
// cannot satisfy the SHA-equality clearing branch by accident — the latch
// must survive because of the channel guard, not because every other
// clearing condition happened to be false for cache-timing reasons.
func TestChannelLatchSurvivesQuietMutableBeat(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	started := time.Now().Add(-time.Minute)
	s.registry.Hives = []RegistryEntry{{
		ID: "latchhive", Upgrading: true, UpgradeTarget: ReleaseChannelStable,
		GitHash: "aaa9999", UpgradeStartedAt: started,
	}}
	rec := postHeartbeat(t, s, `{
		"hive_id":"latchhive","primary_repo":"r",
		"image_ref":"ghcr.io/hivecommons/hive:v4-latest",
		"git_branch":"chandurbranch","git_hash":"aaa9999","upgrading":false
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	entry := s.registry.Hives[0]
	s.mu.RUnlock()
	if !entry.Upgrading || entry.UpgradeTarget != ReleaseChannelStable {
		t.Errorf("channel latch must survive a quiet beat still on the old mutable tag, got Upgrading=%v UpgradeTarget=%q",
			entry.Upgrading, entry.UpgradeTarget)
	}
}

// TestChannelLatchClearsWhenImageReportsChannelTag is the completion path and
// the positive control for the survival test above: the spoke's reported
// image now RUNS the channel tag, so both the armed switch (switchDone) and
// the registry latch must clear, and the hub must stop instructing the spoke.
func TestChannelLatchClearsWhenImageReportsChannelTag(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	started := time.Now().Add(-time.Minute)
	s.registry.Hives = []RegistryEntry{{
		ID: "latchhive", Upgrading: true, UpgradeTarget: ReleaseChannelStable,
		GitHash: "aaa9999", UpgradeStartedAt: started,
	}}
	s.heartbeatSwitchTag["latchhive"] = ReleaseChannelStable

	// A "stable" retag of a v4 build heartbeats its baked-in branch ("v4"),
	// which maps to "v4-latest", never "stable" — so ONLY the reported
	// ImageRef can prove completion. That is exactly what this beat carries.
	rec := postHeartbeat(t, s, `{
		"hive_id":"latchhive","primary_repo":"r",
		"image_ref":"ghcr.io/hivecommons/hive:stable",
		"git_branch":"v4","git_hash":"aaa9999","upgrading":false
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "switch_to_tag") {
		t.Errorf("completed switch must not be re-instructed, got %s", rec.Body.String())
	}
	if got := armedSwitchTag(s, "latchhive"); got != "" {
		t.Errorf("heartbeatSwitchTag = %q, want cleared after completion", got)
	}
	s.mu.RLock()
	entry := s.registry.Hives[0]
	s.mu.RUnlock()
	if entry.Upgrading || entry.UpgradeTarget != "" {
		t.Errorf("registry latch must clear once the image runs the channel tag, got Upgrading=%v UpgradeTarget=%q",
			entry.Upgrading, entry.UpgradeTarget)
	}
}

// TestNonChannelTargetKeepsFloatingLatestClear is the regression control for
// the pre-#3771 behavior: a NON-channel target on a floating-tag hive must
// still be cleared by a quiet mutable-tag beat (the spoke cannot land on a
// specific historical commit, so a non-upgrading beat is itself proof the
// rollout finished). If the channel guard were over-broad and latched every
// target, this hive would stay "Upgrading" forever and the stale-upgrade
// sweep would re-roll its pod every timeout.
func TestNonChannelTargetKeepsFloatingLatestClear(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	started := time.Now().Add(-time.Minute)
	s.registry.Hives = []RegistryEntry{{
		ID: "shahive", Upgrading: true, UpgradeTarget: "bbb1111",
		GitHash: "aaa9999", UpgradeStartedAt: started,
	}}
	// Quiet beat on a mutable tag, reporting a hash that matches NEITHER the
	// target nor any cached latest — only the floatingAtLatest heuristic can
	// clear this, which is exactly the branch under test.
	rec := postHeartbeat(t, s, `{
		"hive_id":"shahive","primary_repo":"r",
		"image_ref":"ghcr.io/hivecommons/hive:v4-latest",
		"git_branch":"chandurbranch","git_hash":"ccc2222","upgrading":false
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	entry := s.registry.Hives[0]
	s.mu.RUnlock()
	if entry.Upgrading || entry.UpgradeTarget != "" {
		t.Errorf("non-channel target on a floating tag must still clear on a quiet beat, got Upgrading=%v UpgradeTarget=%q",
			entry.Upgrading, entry.UpgradeTarget)
	}
}
