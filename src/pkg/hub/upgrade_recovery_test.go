package hub

// Tests for #2476: a hub restart must never orphan an armed upgrade.
//
// The registry latch (Upgrading/UpgradeTarget/UpgradeStartedAt) is durable,
// but heartbeatUpgrade — the map the heartbeat path reads to actually deliver
// an upgrade instruction — is in-memory. The two recovery paths under test:
//
//  1. recoverArmedUpgrades() rebuilds the map from the registry at startup.
//  2. triggerAutoUpgrades() recovers stale latched hives regardless of
//     AutoUpgrade — the recovery used to live behind the AutoUpgrade gate, so
//     manually upgraded hives were never recovered at all.

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRecoverArmedUpgradesRebuildsHeartbeatMap constructs a HubServer whose
// registry carries a latched entry (exactly what a restarted hub loads from
// disk) and asserts the startup-recovery path re-arms delivery for it — and
// only for it.
func TestRecoverArmedUpgradesRebuildsHeartbeatMap(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	// These entries carry a recent LastHeartbeat because recovery only re-arms
	// hives that can COLLECT the instruction (upgradeCollectible); a real
	// latched hive always has one. The uncollectible case is covered in
	// rearm_collectible_test.go.
	beat := rfc3339At(time.Now().Add(-time.Minute))
	s.registry.Hives = []RegistryEntry{
		// The incident shape: durably latched Upgrading with a target, armed by
		// handleUpgradeHive before the hub restarted.
		{ID: "latched", Upgrading: true, UpgradeTarget: "fc32ae4", LastHeartbeat: beat},
		// Not upgrading — a leftover target alone must not arm delivery.
		{ID: "idle", Upgrading: false, UpgradeTarget: "deadbee", LastHeartbeat: beat},
		// Latched but with no target — nothing a heartbeat could deliver.
		{ID: "no-target", Upgrading: true, LastHeartbeat: beat},
		// Terminal failure recorded by the orphan sweep — must stay terminal.
		{ID: "failed", Upgrading: true, UpgradeTarget: "fc32ae4", UpgradeFailed: true, LastHeartbeat: beat},
	}

	// heartbeatUpgrade is deliberately left nil: at this point in NewHubServer
	// the recovery must work regardless, like every other writer of the map.
	s.recoverArmedUpgrades()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if got := s.heartbeatUpgrade["latched"]; got != "fc32ae4" {
		t.Errorf("latched upgrade must be re-armed at startup, heartbeatUpgrade = %q, want %q", got, "fc32ae4")
	}
	for _, id := range []string{"idle", "no-target", "failed"} {
		if target, armed := s.heartbeatUpgrade[id]; armed {
			t.Errorf("hive %q must NOT be re-armed at startup, got target %q", id, target)
		}
	}
}

// TestNewHubServerRecoversArmedUpgradesFromDisk goes through the real startup
// path: a registry file on disk with a latched entry, loaded by NewHubServer,
// must leave the hub with delivery armed. This is the end-to-end guard — it
// fails if the recovery call is ever dropped from server construction, not
// just if the method's logic regresses.
func TestNewHubServerRecoversArmedUpgradesFromDisk(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	oldRegistry := registryPath
	registryPath = filepath.Join(t.TempDir(), "hub-registry.json")
	defer func() { registryPath = oldRegistry }()

	reg := Registry{Hives: []RegistryEntry{
		{ID: "latched-disk", Upgrading: true, UpgradeTarget: "fc32ae4",
			LastHeartbeat: rfc3339At(time.Now().Add(-time.Minute))},
	}}
	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatalf("marshal registry: %v", err)
	}
	if err := os.WriteFile(registryPath, data, 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	s := NewHubServer(0, slog.Default(), "test", "v2")
	// Join the save loop before t.TempDir cleanup: recovery arms a requestSave,
	// and an unjoined loop would recreate the registry inside the TempDir while
	// the framework removes it (#4774).
	t.Cleanup(s.StopSaveLoop)

	s.mu.RLock()
	defer s.mu.RUnlock()
	if got := s.heartbeatUpgrade["latched-disk"]; got != "fc32ae4" {
		t.Errorf("hub restart orphaned an armed upgrade: heartbeatUpgrade = %q, want %q", got, "fc32ae4")
	}
}

// TestTriggerAutoUpgradesRecoversStaleManualUpgrade is the sweep half of
// #2476: a hive with AutoUpgrade FALSE, latched Upgrading past
// staleUpgradeTimeout, must have its heartbeat delivery re-armed — with the
// target it originally requested, not advanced to the branch's latest SHA
// (target advancement is auto-upgrade behaviour).
func TestTriggerAutoUpgradesRecoversStaleManualUpgrade(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	remote := ClusterConfig{ID: "remote", InCluster: false, KubeconfigPath: "/tmp/kc", Context: "ctx"}
	saveSaaSHive(&SaaSHive{ID: "manual-1", Owner: "alice", AutoUpgrade: false, Status: "running", ClusterID: "remote"})
	resetSHACaches(t)
	// A newer SHA exists on the branch; a manual upgrade must NOT be advanced
	// to it during recovery.
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "newer99"}
	latestSHAMu.Unlock()

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, heartbeatUpgrade: make(map[string]string), clusters: map[string]ClusterConfig{"remote": remote}}
	s.registry.Hives = []RegistryEntry{{
		ID: "manual-1", GitBranch: "v2", GitHash: "old1234", Upgrading: true,
		UpgradeTarget: "fc32ae4", UpgradeStartedAt: time.Now().Add(-30 * time.Minute),
		// Alive: recovery re-arms delivery for a spoke that will COLLECT the
		// instruction on its next beat. A hive that never heartbeated is
		// abandoned instead, which is a different case (see
		// pullonly_upgrade_test.go).
		LastHeartbeat: time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339),
	}}

	s.triggerAutoUpgrades()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if got := s.heartbeatUpgrade["manual-1"]; got != "fc32ae4" {
		t.Errorf("stale manual upgrade must be re-armed for delivery, heartbeatUpgrade = %q, want %q", got, "fc32ae4")
	}
	if got := s.registry.Hives[0].UpgradeTarget; got != "fc32ae4" {
		t.Errorf("a manual upgrade's target must not be advanced to latest, got %q", got)
	}
	if !s.registry.Hives[0].Upgrading {
		t.Error("recovery must keep the durable Upgrading latch until the spoke reports the target")
	}
	// Re-arming delivery for the SAME target must PRESERVE the original start
	// time, not refresh it. A crash-looping self-upgrade re-enters this recovery
	// every staleUpgradeTimeout with the same target; refreshing the clock reset
	// the displayed "Upgrading Ns" each cycle, so the true elapsed never crossed
	// the stuck threshold and AlertTypeStuckUpgrade never fired. The 30-minute
	// start this hive began with must survive the re-arm so the elapsed is honest.
	if age := time.Since(s.registry.Hives[0].UpgradeStartedAt); age < 29*time.Minute {
		t.Errorf("recovery re-arming the same target must PRESERVE UpgradeStartedAt so the elapsed is honest, but it was refreshed to %v old", age)
	}
}

// TestTriggerAutoUpgradesRecoversStaleDailyModeHiveHeldBySchedule pins the
// #2496 half of the restructure: a daily-mode hive that already fired today is
// held by the schedule gate ("already upgraded today") — but staleness
// recovery is NOT a scheduled upgrade and must never be held by the daily
// window. Before the restructure the schedule gate sat ahead of the
// alreadyUpgrading branch, so a stuck upgrade on such a hive was not re-armed
// until the next ET day.
func TestTriggerAutoUpgradesRecoversStaleDailyModeHiveHeldBySchedule(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	loc, err := autoUpgradeLocation()
	if err != nil {
		t.Fatalf("autoUpgradeLocation: %v", err)
	}
	today := time.Now().In(loc).Format(autoUpgradeDateFormat)

	remote := ClusterConfig{ID: "remote", InCluster: false, KubeconfigPath: "/tmp/kc", Context: "ctx"}
	saveSaaSHive(&SaaSHive{
		ID: "daily-1", Owner: "alice", AutoUpgrade: true,
		AutoUpgradeMode: AutoUpgradeModeDaily, AutoUpgradeLastFired: today,
		Status: "running", ClusterID: "remote",
	})
	// No latest SHA cached: recovery keeps the latched target rather than
	// advancing, which keeps the assertion on the re-arm itself.
	resetSHACaches(t)

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, heartbeatUpgrade: make(map[string]string), clusters: map[string]ClusterConfig{"remote": remote}}
	s.registry.Hives = []RegistryEntry{{
		ID: "daily-1", GitBranch: "v2", GitHash: "old1234", Upgrading: true,
		UpgradeTarget: "fc32ae4", UpgradeStartedAt: time.Now().Add(-30 * time.Minute),
		// Alive, so the assertion stays on the schedule-gate behaviour rather
		// than on heartbeat collectability.
		LastHeartbeat: time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339),
	}}

	s.triggerAutoUpgrades()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if got := s.heartbeatUpgrade["daily-1"]; got != "fc32ae4" {
		t.Errorf("stale upgrade on a daily-mode hive that already fired today must still be re-armed, heartbeatUpgrade = %q, want %q", got, "fc32ae4")
	}
	if !s.registry.Hives[0].Upgrading {
		t.Error("recovery must keep the durable Upgrading latch until the spoke reports the target")
	}
}

// TestTriggerAutoUpgradesClearsFloatingTagAtLatest is the triggerAutoUpgrades
// half of the floating-tag fix. A floating-tag hive that is already on its
// branch-latest build but latched Upgrading toward an older specific target
// must have the latch CLEARED — not advanced to a newer target and re-rolled.
// This is the branch of the arm→stale→advance→rollout loop that kept restarting
// the spoke pod: the target advance and rolloutRestartHive lived here.
func TestTriggerAutoUpgradesClearsFloatingTagAtLatest(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	remote := ClusterConfig{ID: "remote", InCluster: false, KubeconfigPath: "/tmp/kc", Context: "ctx"}
	saveSaaSHive(&SaaSHive{ID: "floating-1", Owner: "alice", AutoUpgrade: true, Status: "running", ClusterID: "remote"})
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v4"] = branchSHAInfo{SHA: "9999abc"}
	latestSHAMu.Unlock()

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, heartbeatUpgrade: make(map[string]string), clusters: map[string]ClusterConfig{"remote": remote}}
	s.registry.Hives = []RegistryEntry{{
		ID: "floating-1", GitBranch: "v4", GitHash: "9999abc", // already at branch-latest
		ImageRef:      "ghcr.io/hivecommons/hive:v4-latest",
		Upgrading:     true,
		UpgradeTarget: "fc32ae4", // older armed target the floating tag never lands on
		// Stale on purpose: without the fix this triggers the advance+rollout.
		UpgradeStartedAt: time.Now().Add(-30 * time.Minute),
	}}

	s.triggerAutoUpgrades()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.registry.Hives[0].Upgrading {
		t.Error("floating-tag hive at latest must have its Upgrading latch cleared")
	}
	if s.registry.Hives[0].UpgradeTarget != "" {
		t.Errorf("cleared floating-tag hive must drop its stale target, got %q", s.registry.Hives[0].UpgradeTarget)
	}
	if got, armed := s.heartbeatUpgrade["floating-1"]; armed {
		t.Errorf("floating-tag hive at latest must NOT be re-armed for a rollout, got %q", got)
	}
}

// TestTriggerAutoUpgradesNotStaleManualRepopulates covers the hub-restart case
// before the stale threshold: a latched manual upgrade on a remote cluster is
// re-populated into heartbeatUpgrade even when it is not yet stale, exactly as
// the auto-upgrade path always did.
func TestTriggerAutoUpgradesNotStaleManualRepopulates(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	remote := ClusterConfig{ID: "remote", InCluster: false, KubeconfigPath: "/tmp/kc", Context: "ctx"}
	saveSaaSHive(&SaaSHive{ID: "manual-2", Owner: "alice", AutoUpgrade: false, Status: "running", ClusterID: "remote"})

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, heartbeatUpgrade: make(map[string]string), clusters: map[string]ClusterConfig{"remote": remote}}
	s.registry.Hives = []RegistryEntry{{
		ID: "manual-2", GitBranch: "v2", GitHash: "old1234", Upgrading: true,
		UpgradeTarget: "keepTarget", UpgradeStartedAt: time.Now(),
		LastHeartbeat: rfc3339At(time.Now().Add(-time.Minute)),
	}}

	s.triggerAutoUpgrades()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if got := s.heartbeatUpgrade["manual-2"]; got != "keepTarget" {
		t.Errorf("in-flight manual upgrade must be re-populated after a hub restart, got %q", got)
	}
}

// TestRecoverArmedUpgradesStampsZeroClockPreservesLiveClock covers the restart
// half of the badge-without-timer bug: the registry persists UpgradeStartedAt
// (json "upgradeStartedAt"), so recovery must NEVER re-stamp a rehydrated
// non-zero clock — the hub rolls often, and a re-stamp per roll is the #2725
// reset bug all over again. Only a latch that never had a clock (armed before
// the field existed, or a persisted spoke-reported upgrade) gets stamped, so
// the dashboard badge always carries its elapsed counter and the
// stuck-upgrade alert is never blind after a restart.
func TestRecoverArmedUpgradesStampsZeroClockPreservesLiveClock(t *testing.T) {
	persisted := time.Now().Add(-42 * time.Minute)
	s := &HubServer{logger: slog.Default()}
	// Recent heartbeats: clock stamping is what this test is about, and re-arming
	// is gated on collectibility (see rearm_collectible_test.go).
	beat := rfc3339At(time.Now().Add(-time.Minute))
	s.registry.Hives = []RegistryEntry{
		// Rehydrated from disk with its real clock — must survive untouched.
		{ID: "with-clock", Upgrading: true, UpgradeTarget: "fc32ae4", UpgradeStartedAt: persisted, LastHeartbeat: beat},
		// Latched with a lost/never-set clock — must be stamped now.
		{ID: "zero-clock", Upgrading: true, UpgradeTarget: "fc32ae4", LastHeartbeat: beat},
		// Spoke-reported upgrading (no target): nothing to arm, but the badge
		// still renders, so it must get a clock too.
		{ID: "spoke-reported", Upgrading: true, LastHeartbeat: beat},
		// Not upgrading — must keep a zero clock.
		{ID: "idle"},
	}

	before := time.Now()
	s.recoverArmedUpgrades()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if got := s.registry.Hives[0].UpgradeStartedAt; !got.Equal(persisted) {
		t.Errorf("recovery must not re-stamp a persisted clock (that resets every in-flight timer on each hub roll), got %v want %v", got, persisted)
	}
	if got := s.registry.Hives[1].UpgradeStartedAt; got.IsZero() || got.Before(before) {
		t.Errorf("a recovered latch with a zero clock must be stamped now, got %v", got)
	}
	if got := s.registry.Hives[2].UpgradeStartedAt; got.IsZero() {
		t.Error("a persisted spoke-reported upgrade must get a clock at recovery")
	}
	if got := s.registry.Hives[3].UpgradeStartedAt; !got.IsZero() {
		t.Errorf("a non-upgrading entry must keep a zero clock, got %v", got)
	}
	// Delivery re-arming is unchanged by the stamping.
	if got := s.heartbeatUpgrade["with-clock"]; got != "fc32ae4" {
		t.Errorf("with-clock must still be re-armed, got %q", got)
	}
	if got := s.heartbeatUpgrade["zero-clock"]; got != "fc32ae4" {
		t.Errorf("zero-clock must still be re-armed, got %q", got)
	}
	if _, armed := s.heartbeatUpgrade["spoke-reported"]; armed {
		t.Error("a targetless latch must not be armed for delivery")
	}
}

// TestUpgradeStartedAtRoundTripsThroughRegistryAndRecovery pins the full
// restart story: the clock serializes to the registry JSON, deserializes
// intact, and survives recoverArmedUpgrades — so a hub restart never resets
// an in-flight upgrade's elapsed time.
func TestUpgradeStartedAtRoundTripsThroughRegistryAndRecovery(t *testing.T) {
	started := time.Now().Add(-13 * time.Minute).UTC().Truncate(time.Second)
	reg := Registry{Hives: []RegistryEntry{{
		ID: "h1", Upgrading: true, UpgradeTarget: "fc32ae4", UpgradeStartedAt: started,
	}}}
	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatalf("marshal registry: %v", err)
	}
	var loaded Registry
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal registry: %v", err)
	}
	if !loaded.Hives[0].UpgradeStartedAt.Equal(started) {
		t.Fatalf("UpgradeStartedAt must round-trip through the registry JSON, got %v want %v", loaded.Hives[0].UpgradeStartedAt, started)
	}

	s := &HubServer{logger: slog.Default()}
	s.registry = loaded
	s.recoverArmedUpgrades()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.registry.Hives[0].UpgradeStartedAt.Equal(started) {
		t.Fatalf("recovery must keep the rehydrated clock, got %v want %v", s.registry.Hives[0].UpgradeStartedAt, started)
	}
}
