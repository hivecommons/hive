package hub

// Tests for the admin upgrade kill switch (upgrade_pause.go).
//
// Every "pause blocks X" test carries a POSITIVE CONTROL — the same scenario
// with the switch off must behave normally. A pause gate that blocks
// everything forever is exactly the failure mode these switches must not have,
// and a suppression test without a control passes for the wrong reason the
// moment the underlying delivery path breaks (see the F14/F18 lesson).

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pauseSpokes / pauseHub flip the switch through the same code path the API
// uses, so By/At stamping and persistence are exercised in every test.
func pauseSpokes(t *testing.T, s *HubServer, paused bool) {
	t.Helper()
	if _, err := s.setUpgradePause(upgradePauseTargetSpokes, paused, "clubanderson"); err != nil {
		t.Fatalf("setUpgradePause(spokes, %v): %v", paused, err)
	}
}

func pauseHub(t *testing.T, s *HubServer, paused bool) {
	t.Helper()
	if _, err := s.setUpgradePause(upgradePauseTargetHub, paused, "clubanderson"); err != nil {
		t.Fatalf("setUpgradePause(hub, %v): %v", paused, err)
	}
}

// ============================================================
// Persistence: the state file must survive a hub restart
// ============================================================

func TestUpgradePausePersistenceRoundTrip(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default()}
	state, err := s.setUpgradePause(upgradePauseTargetSpokes, true, "clubanderson")
	if err != nil {
		t.Fatalf("setUpgradePause: %v", err)
	}
	if !state.Spokes.Paused || state.Spokes.By != "clubanderson" {
		t.Fatalf("in-memory state after pause = %+v", state.Spokes)
	}
	if _, err := time.Parse(time.RFC3339, state.Spokes.At); err != nil {
		t.Fatalf("At %q is not RFC3339: %v", state.Spokes.At, err)
	}

	// A FRESH HubServer models the hub restarting (the hub auto-rolls itself
	// frequently — in-memory-only pause state was the incident mode).
	s2 := &HubServer{logger: slog.Default()}
	sw, paused := s2.spokeUpgradesPaused()
	if !paused || sw.By != "clubanderson" || sw.At != state.Spokes.At {
		t.Fatalf("reloaded spoke switch = %+v paused=%v, want persisted pause by clubanderson", sw, paused)
	}
	// The two switches are independent: pausing spokes must not pause the hub.
	if _, hubPaused := s2.hubUpgradesPaused(); hubPaused {
		t.Fatal("hub switch reads paused after only the spoke switch was set")
	}

	// Resume is persisted too, with fresh who/when.
	if _, err := s2.setUpgradePause(upgradePauseTargetSpokes, false, "otheradmin"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	s3 := &HubServer{logger: slog.Default()}
	sw3, paused3 := s3.spokeUpgradesPaused()
	if paused3 {
		t.Fatal("spoke switch still paused after persisted resume")
	}
	if sw3.By != "otheradmin" {
		t.Fatalf("resume attribution = %q, want otheradmin", sw3.By)
	}
}

// ============================================================
// Delivery path 1: triggerAutoUpgrades (incl. stale-recovery re-arm)
// ============================================================

// TestSpokePauseBlocksTriggerAutoUpgradesReArm: the non-stale re-populate path
// (heartbeatUpgrade re-armed after a hub restart) must be suppressed while
// paused and work normally when resumed.
func TestSpokePauseBlocksTriggerAutoUpgradesReArm(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetCommitOrderState(t)
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "behind", nil
	})

	remote := ClusterConfig{ID: "remote", InCluster: false, KubeconfigPath: "/tmp/kc", Context: "ctx"}
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", AutoUpgrade: true, Status: "running", ClusterID: "remote"})

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, heartbeatUpgrade: make(map[string]string), clusters: map[string]ClusterConfig{"remote": remote}}
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", GitBranch: "v2", GitHash: "old", Upgrading: true,
		UpgradeTarget: "keepTarget", UpgradeStartedAt: time.Now(),
		// Collectible, so the resumed positive control genuinely re-arms.
		LastHeartbeat: rfc3339At(time.Now().Add(-time.Minute)),
	}}

	pauseSpokes(t, s, true)
	s.triggerAutoUpgrades()
	s.mu.RLock()
	got := s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()
	if got != "" {
		t.Fatalf("paused: heartbeatUpgrade re-armed to %q, want nothing delivered", got)
	}
	// The durable latch must be untouched — pause freezes, it does not cancel.
	s.mu.RLock()
	entry := s.registry.Hives[0]
	s.mu.RUnlock()
	if !entry.Upgrading || entry.UpgradeTarget != "keepTarget" {
		t.Fatalf("paused: registry latch mutated: %+v", entry)
	}

	// POSITIVE CONTROL: resumed, the same cycle re-arms the target.
	pauseSpokes(t, s, false)
	s.triggerAutoUpgrades()
	s.mu.RLock()
	got = s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()
	if got != "keepTarget" {
		t.Fatalf("resumed: heartbeatUpgrade = %q, want keepTarget (control failed — the pause test proves nothing)", got)
	}
}

// TestSpokePauseBlocksStaleRecovery: the stale-upgrade recovery reconciler
// (beginUpgrade + heartbeatUpgrade[...] = recoverTarget + rolloutRestartHive)
// re-arms in-memory state aggressively — the exact machinery the kill switch
// exists to stop.
func TestSpokePauseBlocksStaleRecovery(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)
	resetCommitOrderState(t)
	// Pin the ancestry answer to "behind" so the at-or-ahead clearing path
	// cannot swallow the latch before the stale-recovery branch runs.
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "behind", nil
	})

	remote := ClusterConfig{ID: "remote", InCluster: false, KubeconfigPath: "/tmp/kc", Context: "ctx", Domain: "r.example.com"}
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", AutoUpgrade: true, Status: "running", ClusterID: "remote"})
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "newsha7"}
	latestSHAMu.Unlock()

	started := time.Now().Add(-2 * time.Hour)
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, heartbeatUpgrade: make(map[string]string), clusters: map[string]ClusterConfig{"remote": remote}}
	// SHAs deliberately share no prefix: sameCommit matches short-vs-full by
	// prefix, so "old"/"oldtarget" would read as already-at-target and clear
	// the latch before the recovery branch this test exercises.
	//
	// LastHeartbeat must be fresh (#3851): stale-recovery only re-arms a hive
	// that can actually COLLECT the instruction on its next heartbeat. A hive
	// with no LastHeartbeat is an unassigned placeholder that never checked
	// in — triggerAutoUpgrades correctly abandons its latch instead of
	// re-arming forever (see pullonly_upgrade.go:upgradeCollectible). This
	// test exercises the pause switch on a LIVE, recoverable hive, so the
	// fixture must model one: a hive that recently heartbeated.
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", GitBranch: "v2", GitHash: "aaa1111", Upgrading: true,
		UpgradeTarget: "bbb2222", UpgradeStartedAt: started,
		LastHeartbeat: rfc3339At(time.Now().Add(-30 * time.Second)),
	}}

	pauseSpokes(t, s, true)
	s.triggerAutoUpgrades()
	s.mu.RLock()
	got := s.heartbeatUpgrade["h1"]
	target := s.registry.Hives[0].UpgradeTarget
	s.mu.RUnlock()
	if got != "" {
		t.Fatalf("paused: stale recovery armed heartbeatUpgrade=%q, want no re-arm", got)
	}
	if target != "bbb2222" {
		t.Fatalf("paused: stale recovery advanced the target to %q, want bbb2222 untouched", target)
	}

	// POSITIVE CONTROL: resumed, recovery advances the target and re-arms.
	pauseSpokes(t, s, false)
	s.triggerAutoUpgrades()
	s.mu.RLock()
	got = s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()
	if got != "newsha7" {
		t.Fatalf("resumed: heartbeatUpgrade = %q, want newsha7 (control failed)", got)
	}
}

// ============================================================
// Delivery path 2: heartbeat handler (UpgradeTo, SwitchToTag, channel re-arm)
// ============================================================

func TestSpokePauseBlocksHeartbeatUpgradeTo(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	s.registry.Hives = []RegistryEntry{{ID: "h1"}}
	s.heartbeatUpgrade["h1"] = "newtarget"

	pauseSpokes(t, s, true)
	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"oldhash"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("paused heartbeat status = %d, want 200 (heartbeats stay normal)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "newtarget") {
		t.Fatalf("paused: heartbeat delivered upgrade_to: %s", rec.Body.String())
	}
	// The armed target survives the pause so resuming delivers it again.
	s.mu.RLock()
	armed := s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()
	if armed != "newtarget" {
		t.Fatalf("paused: armed target = %q, want newtarget preserved", armed)
	}

	// POSITIVE CONTROL: resumed, the very next beat carries the instruction.
	pauseSpokes(t, s, false)
	rec = postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"oldhash"}`)
	if !strings.Contains(rec.Body.String(), "newtarget") {
		t.Fatalf("resumed: expected upgrade_to newtarget, got %s (control failed)", rec.Body.String())
	}
}

func TestSpokePauseBlocksSpokeManagedUpgradeTo(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "newsha7"}
	latestSHAMu.Unlock()

	// Spoke-managed hive (ConfigMap auto_upgrade=true, no SaaS record):
	// normally chases latest on every beat.
	// Assert on the upgrade_to KEY, not the SHA — the heartbeat response always
	// echoes latest_sha, so the SHA string alone appears even when nothing is
	// instructed.
	pauseSpokes(t, s, true)
	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"old","git_branch":"v2","auto_upgrade":true}`)
	if strings.Contains(rec.Body.String(), `"upgrade_to"`) {
		t.Fatalf("paused: spoke-managed upgrade_to delivered: %s", rec.Body.String())
	}

	// POSITIVE CONTROL.
	pauseSpokes(t, s, false)
	rec = postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"old","git_branch":"v2","auto_upgrade":true}`)
	if !strings.Contains(rec.Body.String(), `"upgrade_to":"newsha7"`) {
		t.Fatalf("resumed: expected upgrade_to newsha7, got %s (control failed)", rec.Body.String())
	}
}

func TestSpokePauseBlocksSwitchToTag(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	s.registry.Hives = []RegistryEntry{{ID: "h1"}}
	s.heartbeatSwitchTag["h1"] = "dev-latest"

	pauseSpokes(t, s, true)
	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_branch":"v2"}`)
	if strings.Contains(rec.Body.String(), "dev-latest") {
		t.Fatalf("paused: switch_to_tag delivered: %s", rec.Body.String())
	}
	// The armed tag survives for resume.
	s.mu.RLock()
	armed := s.heartbeatSwitchTag["h1"]
	s.mu.RUnlock()
	if armed != "dev-latest" {
		t.Fatalf("paused: armed switch tag = %q, want dev-latest preserved", armed)
	}

	// POSITIVE CONTROL.
	pauseSpokes(t, s, false)
	rec = postHeartbeat(t, s, `{"hive_id":"h1","git_branch":"v2"}`)
	if !strings.Contains(rec.Body.String(), "dev-latest") {
		t.Fatalf("resumed: expected switch_to_tag dev-latest, got %s (control failed)", rec.Body.String())
	}
}

// TestSpokePauseBlocksTrackedChannelReArm: the durable-channel self-heal
// (#3771) re-arms heartbeatSwitchTag from SaaSHive.TrackedChannel whenever the
// reported image tag disagrees — a delivery decision that must respect the
// pause.
func TestSpokePauseBlocksTrackedChannelReArm(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", TrackedChannel: ReleaseChannelStable})
	s.registry.Hives = []RegistryEntry{{ID: "h1"}}

	pauseSpokes(t, s, true)
	rec := postHeartbeat(t, s, `{"hive_id":"h1","image_ref":"ghcr.io/kubestellar/hive:v4-latest"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	s.mu.RLock()
	armed := s.heartbeatSwitchTag["h1"]
	s.mu.RUnlock()
	if armed != "" {
		t.Fatalf("paused: tracked-channel re-armed heartbeatSwitchTag=%q, want no re-arm", armed)
	}
	if strings.Contains(rec.Body.String(), "switch_to_tag") {
		t.Fatalf("paused: switch instruction delivered: %s", rec.Body.String())
	}

	// POSITIVE CONTROL: resumed, the drift re-arms and instructs the channel.
	pauseSpokes(t, s, false)
	rec = postHeartbeat(t, s, `{"hive_id":"h1","image_ref":"ghcr.io/kubestellar/hive:v4-latest"}`)
	s.mu.RLock()
	armed = s.heartbeatSwitchTag["h1"]
	s.mu.RUnlock()
	if armed != ReleaseChannelStable {
		t.Fatalf("resumed: re-armed tag = %q, want %q (control failed)", armed, ReleaseChannelStable)
	}
	if !strings.Contains(rec.Body.String(), ReleaseChannelStable) {
		t.Fatalf("resumed: expected switch_to_tag %q, got %s", ReleaseChannelStable, rec.Body.String())
	}
}

// ============================================================
// Delivery path 3: orphaned-upgrade sweep re-arm
// ============================================================

func TestSpokePauseBlocksOrphanSweep(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetSHACaches(t)
	resetCommitOrderState(t)
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "behind", nil
	})

	mkOrphan := func() *HubServer {
		s := &HubServer{logger: slog.Default(), saveCh: make(chan struct{}, 1), heartbeatUpgrade: make(map[string]string)}
		s.registry.Hives = []RegistryEntry{{
			ID: "h1", Upgrading: true, UpgradeTarget: "tgt7777", GitHash: "old7777",
			UpgradeStartedAt: time.Now().Add(-3 * staleUpgradeTimeout),
			LastHeartbeat:    time.Now().Format(time.RFC3339),
		}}
		return s
	}

	s := mkOrphan()
	pauseSpokes(t, s, true)
	s.sweepOrphanedUpgrades()
	s.mu.RLock()
	entry := s.registry.Hives[0]
	armed := s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()
	if !entry.Upgrading || entry.OrphanedUpgradeSweeps != 0 {
		t.Fatalf("paused: sweep mutated the latch (%+v) — must be a no-op; retry budget burned while paused would fake UpgradeFailed", entry)
	}
	if armed != "" {
		t.Fatalf("paused: sweep re-armed heartbeatUpgrade=%q", armed)
	}

	// POSITIVE CONTROL on a fresh, unpaused server: same orphan is swept and
	// the target is re-armed for delivery.
	s2 := mkOrphan()
	pauseSpokes(t, s2, false)
	s2.sweepOrphanedUpgrades()
	s2.mu.RLock()
	entry = s2.registry.Hives[0]
	armed = s2.heartbeatUpgrade["h1"]
	s2.mu.RUnlock()
	if entry.Upgrading || entry.OrphanedUpgradeSweeps != 1 {
		t.Fatalf("unpaused control: sweep did not clear the orphan (%+v) — the pause test proves nothing", entry)
	}
	if armed != "tgt7777" {
		t.Fatalf("unpaused control: re-armed target = %q, want tgt7777", armed)
	}
}

// ============================================================
// Hub self-upgrade kill switch
// ============================================================

func TestHubPauseRefusesManualHubUpgrade(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetSHACaches(t)
	mkUser(t, hubAdminUsername)
	s := newHandlerHub()

	pauseHub(t, s, true)
	rec := httptest.NewRecorder()
	s.handleHubSelfUpgrade(rec, reqWithUser(http.MethodPost, "/api/saas/hub/upgrade", "", hubAdminUsername))
	if rec.Code != http.StatusConflict {
		t.Fatalf("paused: hub self-upgrade status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hub upgrades are paused by clubanderson since ") {
		t.Fatalf("paused: refusal must name who/when, got %s", rec.Body.String())
	}

	// POSITIVE CONTROL: resumed, the request passes the gate. With no latest
	// hub SHA cached, rolloutHubToSHA fails -> 500, which proves the handler
	// advanced PAST the 409 pause gate.
	pauseHub(t, s, false)
	rec = httptest.NewRecorder()
	s.handleHubSelfUpgrade(rec, reqWithUser(http.MethodPost, "/api/saas/hub/upgrade", "", hubAdminUsername))
	if rec.Code == http.StatusConflict {
		t.Fatalf("resumed: still 409 — the kill switch is stuck on; body=%s", rec.Body.String())
	}
}

// ============================================================
// Manual spoke upgrade / branch switch refusals (409, never a silent queue)
// ============================================================

func TestSpokePauseRefusesManualUpgradeHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)
	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", ClusterID: "hive-oke"})
	s := newHandlerHub()
	s.clusters = map[string]ClusterConfig{"hive-oke": {ID: "hive-oke", InCluster: true}}
	s.registry.Hives = []RegistryEntry{{ID: "h1", GitBranch: "v2", GitHash: "old"}}
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "newsha7"}
	latestSHAMu.Unlock()

	pauseSpokes(t, s, true)
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/h1/upgrade", "", "alice"), "id", "h1")
	s.handleUpgradeHive(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("paused: upgrade status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "spoke upgrades are paused by clubanderson since ") {
		t.Fatalf("paused: refusal must name who/when, got %s", rec.Body.String())
	}
	// Refused means NOTHING was armed or latched — no silent queue.
	s.mu.RLock()
	armed := s.heartbeatUpgrade["h1"]
	upgrading := s.registry.Hives[0].Upgrading
	s.mu.RUnlock()
	if armed != "" || upgrading {
		t.Fatalf("paused: refused request still armed state (armed=%q upgrading=%v)", armed, upgrading)
	}

	// POSITIVE CONTROL: resumed, the same request upgrades (scripted kubectl
	// delivers the restart).
	pauseSpokes(t, s, false)
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/h1/upgrade", "", "alice"), "id", "h1")
	s.handleUpgradeHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resumed: upgrade status = %d, want 200 (control failed); body=%s", rec.Code, rec.Body.String())
	}
}

func TestSpokePauseRefusesManualSwitchBranch(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", ClusterID: "hive-oke"})
	// GitHub returns 404 for every branch, so the unpaused control below stops
	// at "unknown branch" (400) without touching a cluster — far enough to
	// prove the request advanced past the pause gate.
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	resetSHACaches(t)
	s := newHandlerHub()
	s.clusters = map[string]ClusterConfig{"hive-oke": {ID: "hive-oke", InCluster: true}}

	pauseSpokes(t, s, true)
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/h1/switch-branch", `{"branch":"v4"}`, "alice"), "id", "h1")
	s.handleSwitchBranch(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("paused: switch status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "spoke upgrades are paused by clubanderson since ") {
		t.Fatalf("paused: refusal must name who/when, got %s", rec.Body.String())
	}
	s.mu.RLock()
	armed := s.heartbeatSwitchTag["h1"]
	s.mu.RUnlock()
	if armed != "" {
		t.Fatalf("paused: refused switch still queued heartbeatSwitchTag=%q", armed)
	}

	// POSITIVE CONTROL: resumed, the handler reaches branch validation (400
	// here, because the faked GitHub knows no branches) instead of 409.
	pauseSpokes(t, s, false)
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/api/saas/hives/h1/switch-branch", `{"branch":"no-such-branch"}`, "alice"), "id", "h1")
	s.handleSwitchBranch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("resumed: switch status = %d, want 400 unknown-branch (control failed); body=%s", rec.Code, rec.Body.String())
	}
}

func TestSpokePauseRefusesBulkUpgradeAndSwitch(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: hubAdminUsername, ClusterID: "hive-oke"})
	s := newHandlerHub()

	pauseSpokes(t, s, true)
	for _, action := range []string{bulkActionUpgrade, bulkActionSwitchBranch} {
		body := `{"action":"` + action + `","hive_ids":["h1"],"branch":"v4"}`
		rec := httptest.NewRecorder()
		s.handleBulkHiveAction(rec, reqWithUser(http.MethodPost, "/api/saas/hives/bulk", body, hubAdminUsername))
		if rec.Code != http.StatusConflict {
			t.Errorf("paused bulk %s: status = %d, want 409; body=%s", action, rec.Code, rec.Body.String())
		}
	}
	// Preference-only bulk actions are configuration, not delivery — allowed.
	rec := httptest.NewRecorder()
	s.handleBulkHiveAction(rec, reqWithUser(http.MethodPost, "/api/saas/hives/bulk",
		`{"action":"disable-auto-upgrade","hive_ids":["h1"]}`, hubAdminUsername))
	if rec.Code != http.StatusOK {
		t.Errorf("paused bulk disable-auto-upgrade: status = %d, want 200 (preference changes stay allowed); body=%s", rec.Code, rec.Body.String())
	}
}

// ============================================================
// API: admin-only enforcement + state shape
// ============================================================

func TestUpgradePauseEndpointsAdminOnly(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")
	mkUser(t, hubAdminUsername)
	s := newHandlerHub()

	getH := s.requireAdmin(s.handleGetUpgradePause)
	postH := s.requireAdmin(s.handleSetUpgradePause)

	// Non-admin: both verbs 403.
	rec := httptest.NewRecorder()
	getH(rec, reqWithUser(http.MethodGet, "/api/saas/upgrade-pause", "", "alice"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET status = %d, want 403", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := reqWithUser(http.MethodPost, "/api/saas/upgrade-pause", `{"target":"spokes","paused":true}`, "alice")
	req.Header.Set("Origin", "https://"+hubCanonicalHost)
	postH(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin POST status = %d, want 403", rec.Code)
	}
	// The refused flip must not have engaged the switch.
	if _, paused := s.spokeUpgradesPaused(); paused {
		t.Fatal("non-admin POST engaged the kill switch")
	}

	// POSITIVE CONTROL: admin flips it (same-origin, so the CSRF gate passes),
	// and GET reflects the persisted state with who/when.
	rec = httptest.NewRecorder()
	req = reqWithUser(http.MethodPost, "/api/saas/upgrade-pause", `{"target":"spokes","paused":true}`, hubAdminUsername)
	req.Header.Set("Origin", "https://"+hubCanonicalHost)
	postH(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin POST status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	getH(rec, reqWithUser(http.MethodGet, "/api/saas/upgrade-pause", "", hubAdminUsername))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"paused":true`) || !strings.Contains(body, hubAdminUsername) {
		t.Fatalf("GET state missing paused/by fields: %s", body)
	}

	// Unknown target is rejected.
	rec = httptest.NewRecorder()
	req = reqWithUser(http.MethodPost, "/api/saas/upgrade-pause", `{"target":"everything","paused":true}`, hubAdminUsername)
	req.Header.Set("Origin", "https://"+hubCanonicalHost)
	postH(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad target status = %d, want 400", rec.Code)
	}
}
