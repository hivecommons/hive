package hub

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// #11196: a daily/weekly hive whose window opened but was turned away by a
// later gate must record WHY, and the heartbeat must relay it, so the spoke
// dashboard does not just show the previous upgrade as if the schedule never ran.

func scheduledHoldET(t *testing.T, hour int) time.Time {
	t.Helper()
	loc, err := autoUpgradeLocation()
	if err != nil {
		t.Fatalf("loading %s: %v", autoUpgradeTimezone, err)
	}
	return time.Date(2026, 10, 14, hour, 0, 0, 0, loc)
}

func stubAutoUpgradeHoldNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := autoUpgradeHoldNow
	autoUpgradeHoldNow = func() time.Time { return at }
	t.Cleanup(func() { autoUpgradeHoldNow = prev })
}

func TestScheduledUpgradeWindowOpen(t *testing.T) {
	afternoon := scheduledHoldET(t, 14)
	cases := []struct {
		name      string
		mode      string
		lastFired string
		now       time.Time
		want      bool
	}{
		{"daily after the window", AutoUpgradeModeDaily, "", afternoon, true},
		{"daily before the window", AutoUpgradeModeDaily, "", scheduledHoldET(t, 12), false},
		{"daily already fired today", AutoUpgradeModeDaily, "2026-10-14", afternoon, false},
		{"daily fired yesterday", AutoUpgradeModeDaily, "2026-10-13", afternoon, true},
		{"empty mode is daily", "", "", afternoon, true},
		{"instant has no window", AutoUpgradeModeInstant, "", afternoon, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scheduledUpgradeWindowOpen(tc.mode, tc.lastFired, tc.now); got != tc.want {
				t.Errorf("scheduledUpgradeWindowOpen(%q, %q, %s) = %v, want %v", tc.mode, tc.lastFired, tc.now, got, tc.want)
			}
		})
	}
}

func TestTriggerAutoUpgradesRecordsScheduledHold(t *testing.T) {
	disableUpgradeDebounceForTest(t)
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	newServer := func(id string) *HubServer {
		s := &HubServer{
			logger:           slog.Default(),
			hubSecret:        testHubSecret,
			heartbeatUpgrade: make(map[string]string),
			clusters:         map[string]ClusterConfig{"dc1": {ID: "dc1", InCluster: true}},
		}
		s.registry.Hives = []RegistryEntry{{ID: id, GitBranch: "v5", GitHash: "d09cc5f", LastHeartbeat: time.Now().UTC().Format(time.RFC3339)}}
		return s
	}

	t.Run("window open records the reason", func(t *testing.T) {
		at := scheduledHoldET(t, 14)
		stubAutoUpgradeHoldNow(t, at)
		const id = "hold-claim"
		saveSaaSHive(&SaaSHive{ID: id, Owner: "alice", AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeDaily,
			Status: statusAssigned, ClusterID: "dc1"})
		s := newServer(id)

		s.triggerAutoUpgrades()

		stored := loadSaaSHive(id)
		if stored == nil {
			t.Fatal("hive record vanished")
		}
		if !strings.Contains(stored.AutoUpgradeHoldReason, "claim") {
			t.Errorf("AutoUpgradeHoldReason = %q, want the claim-in-flight reason", stored.AutoUpgradeHoldReason)
		}
		if !stored.AutoUpgradeHoldAt.Equal(at) {
			t.Errorf("AutoUpgradeHoldAt = %v, want %v", stored.AutoUpgradeHoldAt, at)
		}
		if got := s.heartbeatUpgrade[id]; got != "" {
			t.Errorf("armed %q for a held hive, want nothing armed", got)
		}
	})

	t.Run("window not open records nothing", func(t *testing.T) {
		stubAutoUpgradeHoldNow(t, scheduledHoldET(t, 10))
		const id = "hold-morning"
		saveSaaSHive(&SaaSHive{ID: id, Owner: "alice", AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeDaily,
			Status: statusAssigned, ClusterID: "dc1"})
		newServer(id).triggerAutoUpgrades()
		if got := loadSaaSHive(id).AutoUpgradeHoldReason; got != "" {
			t.Errorf("AutoUpgradeHoldReason = %q before the window, want empty", got)
		}
	})

	t.Run("auto-upgrade off records nothing", func(t *testing.T) {
		stubAutoUpgradeHoldNow(t, scheduledHoldET(t, 14))
		const id = "hold-manual"
		saveSaaSHive(&SaaSHive{ID: id, Owner: "alice", AutoUpgradeMode: AutoUpgradeModeDaily,
			Status: statusAssigned, ClusterID: "dc1"})
		newServer(id).triggerAutoUpgrades()
		if got := loadSaaSHive(id).AutoUpgradeHoldReason; got != "" {
			t.Errorf("AutoUpgradeHoldReason = %q for a manual hive, want empty", got)
		}
	})
}

func TestNoteScheduledUpgradeHoldClearsAndPersists(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default()}
	at := scheduledHoldET(t, 14)
	const id = "hold-clear"
	h := &SaaSHive{ID: id, Owner: "alice", AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeDaily}
	if err := saveSaaSHive(h); err != nil {
		t.Fatalf("saving hive: %v", err)
	}

	s.noteScheduledUpgradeHold(h, "the cluster's upgrade wave is full", at)
	if got := loadSaaSHive(id); got.AutoUpgradeHoldReason == "" || !got.AutoUpgradeHoldAt.Equal(at) {
		t.Fatalf("hold not persisted: reason=%q at=%v", got.AutoUpgradeHoldReason, got.AutoUpgradeHoldAt)
	}

	// The same reason a cycle later keeps the original timestamp.
	s.noteScheduledUpgradeHold(h, "the cluster's upgrade wave is full", at.Add(2*time.Minute))
	if got := loadSaaSHive(id); !got.AutoUpgradeHoldAt.Equal(at) {
		t.Errorf("AutoUpgradeHoldAt = %v after a repeated hold, want the first time %v", got.AutoUpgradeHoldAt, at)
	}

	s.noteScheduledUpgradeHold(h, "", at.Add(time.Hour))
	if got := loadSaaSHive(id); got.AutoUpgradeHoldReason != "" || !got.AutoUpgradeHoldAt.IsZero() {
		t.Errorf("hold not cleared: reason=%q at=%v", got.AutoUpgradeHoldReason, got.AutoUpgradeHoldAt)
	}
}

func TestHeartbeatUpgradePolicyRelaysScheduledHold(t *testing.T) {
	stubBranchHead(t, "v5", "5193426")
	s := &HubServer{logger: targetingLogger()}
	payload := &HeartbeatPayload{HiveID: "h", GitBranch: "v5", GitHash: "d09cc5f"}
	at := time.Date(2026, 10, 14, 17, 0, 0, 0, time.UTC)
	const reason = "the cluster's upgrade wave is full; the hive boards a later wave"

	cases := []struct {
		name       string
		rec        *SaaSHive
		wantReason string
		wantAt     string
	}{
		{
			name:       "daily hub-managed",
			rec:        &SaaSHive{AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeDaily, AutoUpgradeHoldReason: reason, AutoUpgradeHoldAt: at},
			wantReason: reason,
			wantAt:     "2026-10-14T17:00:00Z",
		},
		{
			name:       "weekly hub-managed",
			rec:        &SaaSHive{AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeWeekly, AutoUpgradeHoldReason: reason, AutoUpgradeHoldAt: at},
			wantReason: reason,
			wantAt:     "2026-10-14T17:00:00Z",
		},
		{
			name: "instant has no window to hold",
			rec:  &SaaSHive{AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeInstant, AutoUpgradeHoldReason: reason, AutoUpgradeHoldAt: at},
		},
		{
			name: "auto-upgrade off",
			rec:  &SaaSHive{AutoUpgradeMode: AutoUpgradeModeDaily, AutoUpgradeHoldReason: reason, AutoUpgradeHoldAt: at},
		},
		{
			name: "nothing held",
			rec:  &SaaSHive{AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeDaily},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.heartbeatUpgradePolicy(payload, tc.rec, false, false, "v5", "")
			if got == nil {
				t.Fatal("policy = nil")
			}
			if got.ScheduledHoldReason != tc.wantReason {
				t.Errorf("ScheduledHoldReason = %q, want %q", got.ScheduledHoldReason, tc.wantReason)
			}
			if got.ScheduledHoldAt != tc.wantAt {
				t.Errorf("ScheduledHoldAt = %q, want %q", got.ScheduledHoldAt, tc.wantAt)
			}
		})
	}
}
