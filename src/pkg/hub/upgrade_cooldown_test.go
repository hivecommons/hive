package hub

import (
	"log/slog"
	"testing"
	"time"
)

// #11825: an instant-mode hive must not be rolled more than once per cooldown,
// however often its target moves — and a deferred hive must land on the NEWEST
// target once the cooldown expires, never the intermediate one.

func TestAutoUpgradeCooldownIsConfigurable(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultAutoUpgradeCooldown},
		{"0", defaultAutoUpgradeCooldown},
		{"garbage", defaultAutoUpgradeCooldown},
		{"3600", time.Hour},
		{"-1", 0},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv("HIVE_UPGRADE_COOLDOWN_SECONDS", tc.env)
			if got := autoUpgradeCooldown(); got != tc.want {
				t.Errorf("autoUpgradeCooldown() = %v, want %v", got, tc.want)
			}
		})
	}
	if defaultAutoUpgradeCooldown < 2*time.Hour {
		t.Errorf("default cooldown %v is too short to absorb back-to-back releases", defaultAutoUpgradeCooldown)
	}
}

func TestAutoUpgradeCooldownRemaining(t *testing.T) {
	now := time.Date(2026, 10, 14, 16, 27, 0, 0, time.UTC)
	cases := []struct {
		name     string
		lastAt   time.Time
		cooldown time.Duration
		want     time.Duration
	}{
		{"never upgraded", time.Time{}, 6 * time.Hour, 0},
		{"cooldown disabled", now.Add(-time.Minute), 0, 0},
		{"inside cooldown", now.Add(-35 * time.Minute), 6 * time.Hour, 6*time.Hour - 35*time.Minute},
		{"exactly expired", now.Add(-6 * time.Hour), 6 * time.Hour, 0},
		{"long expired", now.Add(-48 * time.Hour), 6 * time.Hour, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoUpgradeCooldownRemaining(tc.lastAt, tc.cooldown, now); got != tc.want {
				t.Errorf("remaining = %v, want %v", got, tc.want)
			}
		})
	}
}

// Daily/weekly hives are already bounded by AutoUpgradeLastFired; the cooldown
// must not change their behaviour.
func TestAutoUpgradeCooldownAppliesOnlyToInstant(t *testing.T) {
	cases := []struct {
		mode, channel string
		want          bool
	}{
		{AutoUpgradeModeInstant, "", true},
		{AutoUpgradeModeInstant, ReleaseChannelStable, true},
		{AutoUpgradeModeDaily, "", false},
		{AutoUpgradeModeDaily, ReleaseChannelStable, false},
		{AutoUpgradeModeWeekly, "", false},
		{AutoUpgradeModeWeekly, ReleaseChannelStable, false},
	}
	for _, tc := range cases {
		t.Run(tc.mode+"/"+tc.channel, func(t *testing.T) {
			if got := autoUpgradeCooldownApplies(tc.mode, tc.channel); got != tc.want {
				t.Errorf("autoUpgradeCooldownApplies(%q, %q) = %v, want %v", tc.mode, tc.channel, got, tc.want)
			}
		})
	}
}

// TestTriggerAutoUpgradesCooldownDefersThenFiresLatest drives the real sweep:
// fire on A, a second target B inside the cooldown is deferred, and after
// expiry the hive rolls straight to the newest target C.
func TestTriggerAutoUpgradesCooldownDefersThenFiresLatest(t *testing.T) {
	disableUpgradeDebounceForTest(t)
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetCommitOrderState(t)
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "behind", nil
	})
	t.Setenv("HIVE_UPGRADE_COOLDOWN_SECONDS", "")
	setLatestSHAForBranchForTest(t, "v4", "shaaaa1")

	const id = "cooldown-e2e"
	saveSaaSHive(&SaaSHive{ID: id, Owner: "alice", AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeInstant, Status: "running", ClusterID: "dc1"})
	s := &HubServer{
		logger:           slog.Default(),
		hubSecret:        testHubSecret,
		heartbeatUpgrade: make(map[string]string),
		clusters:         map[string]ClusterConfig{"dc1": {ID: "dc1", InCluster: true}},
	}
	beat := time.Now().UTC().Format(time.RFC3339)
	s.registry.Hives = []RegistryEntry{{ID: id, GitBranch: "v4", GitHash: "oldsha0", LastHeartbeat: beat}}

	armedTarget := func() string {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.heartbeatUpgrade[id]
	}
	landOn := func(sha string) {
		s.mu.Lock()
		s.registry.Hives[0] = RegistryEntry{ID: id, GitBranch: "v4", GitHash: sha, LastHeartbeat: beat}
		delete(s.heartbeatUpgrade, id)
		s.mu.Unlock()
	}

	s.triggerAutoUpgrades()
	if got := armedTarget(); got != "shaaaa1" {
		t.Fatalf("first upgrade armed %q, want shaaaa1", got)
	}
	stored := loadSaaSHive(id)
	if stored == nil || stored.AutoUpgradeLastAt.IsZero() {
		t.Fatalf("auto-upgrade timestamp was not persisted, got %+v", stored)
	}
	landOn("shaaaa1")

	// A second target 40 minutes later: deferred, not armed.
	setLatestSHAForBranchForTest(t, "v4", "shabbb2")
	s.triggerAutoUpgrades()
	if got := armedTarget(); got != "" {
		t.Fatalf("second target inside the cooldown armed %q, want it deferred", got)
	}

	// A third target lands, then the cooldown expires (backdated, no sleep).
	setLatestSHAForBranchForTest(t, "v4", "shaccc3")
	stored = loadSaaSHive(id)
	stored.AutoUpgradeLastAt = time.Now().Add(-defaultAutoUpgradeCooldown - time.Minute)
	if err := saveSaaSHive(stored); err != nil {
		t.Fatalf("saving backdated hive: %v", err)
	}
	s.triggerAutoUpgrades()
	if got := armedTarget(); got != "shaccc3" {
		t.Fatalf("after the cooldown armed %q, want the LATEST shaccc3 (not the intermediate shabbb2)", got)
	}
	if after := loadSaaSHive(id); time.Since(after.AutoUpgradeLastAt) > time.Minute {
		t.Errorf("timestamp not refreshed on the second fire: %v", after.AutoUpgradeLastAt)
	}
}

func TestTriggerAutoUpgradesCooldownDisabled(t *testing.T) {
	disableUpgradeDebounceForTest(t)
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetCommitOrderState(t)
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "behind", nil
	})
	t.Setenv("HIVE_UPGRADE_COOLDOWN_SECONDS", "-1")
	setLatestSHAForBranchForTest(t, "v4", "shabbb2")

	const id = "cooldown-off"
	saveSaaSHive(&SaaSHive{ID: id, Owner: "alice", AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeInstant, Status: "running", ClusterID: "dc1",
		AutoUpgradeLastAt: time.Now().Add(-10 * time.Minute)})
	s := &HubServer{
		logger:           slog.Default(),
		hubSecret:        testHubSecret,
		heartbeatUpgrade: make(map[string]string),
		clusters:         map[string]ClusterConfig{"dc1": {ID: "dc1", InCluster: true}},
	}
	s.registry.Hives = []RegistryEntry{{ID: id, GitBranch: "v4", GitHash: "shaaaa1", LastHeartbeat: time.Now().UTC().Format(time.RFC3339)}}

	s.triggerAutoUpgrades()
	s.mu.RLock()
	got := s.heartbeatUpgrade[id]
	s.mu.RUnlock()
	if got != "shabbb2" {
		t.Fatalf("with the cooldown disabled armed %q, want shabbb2 immediately", got)
	}
}

func TestHeartbeatUpgradePolicySurfacesCooldown(t *testing.T) {
	stubBranchHead(t, "v4", "526ef71")
	t.Setenv("HIVE_UPGRADE_COOLDOWN_SECONDS", "")
	s := &HubServer{logger: targetingLogger()}
	payload := &HeartbeatPayload{HiveID: "h", GitBranch: "v4", GitHash: "abc1234"}
	lastAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	cases := []struct {
		name         string
		rec          *SaaSHive
		wantSeconds  int
		wantEligible string
	}{
		{
			name:         "instant inside cooldown",
			rec:          &SaaSHive{AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeInstant, AutoUpgradeLastAt: lastAt},
			wantSeconds:  int(defaultAutoUpgradeCooldown / time.Second),
			wantEligible: lastAt.Add(defaultAutoUpgradeCooldown).Format(time.RFC3339),
		},
		{
			name:        "instant never upgraded",
			rec:         &SaaSHive{AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeInstant},
			wantSeconds: int(defaultAutoUpgradeCooldown / time.Second),
		},
		{
			name: "instant past cooldown",
			rec: &SaaSHive{AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeInstant,
				AutoUpgradeLastAt: time.Now().Add(-defaultAutoUpgradeCooldown - time.Hour)},
			wantSeconds: int(defaultAutoUpgradeCooldown / time.Second),
		},
		{
			name: "daily unaffected",
			rec:  &SaaSHive{AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeDaily, AutoUpgradeLastAt: lastAt},
		},
		{
			name: "auto-upgrade off",
			rec:  &SaaSHive{AutoUpgradeMode: AutoUpgradeModeInstant, AutoUpgradeLastAt: lastAt},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.heartbeatUpgradePolicy(payload, tc.rec, false, false, "v4", "")
			if got == nil {
				t.Fatal("policy = nil")
			}
			if got.CooldownSeconds != tc.wantSeconds {
				t.Errorf("CooldownSeconds = %d, want %d", got.CooldownSeconds, tc.wantSeconds)
			}
			if got.NextEligibleAt != tc.wantEligible {
				t.Errorf("NextEligibleAt = %q, want %q", got.NextEligibleAt, tc.wantEligible)
			}
		})
	}
}
