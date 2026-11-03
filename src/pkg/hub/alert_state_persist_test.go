package hub

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func withTempAlertStatePath(t *testing.T) string {
	t.Helper()
	orig := alertStatePath
	alertStatePath = filepath.Join(t.TempDir(), "hub-alert-state.json")
	t.Cleanup(func() { alertStatePath = orig })
	return alertStatePath
}

func TestAlertStatePersistRoundTrip(t *testing.T) {
	withTempAlertStatePath(t)
	now := time.Now().UTC().Truncate(time.Second)

	s := newTestAlertServer()
	s.alerts.noteUpgrade("h1", now.Add(-time.Minute))
	s.alerts.observeStart("h1", rfc3339(now.Add(-10*time.Minute)), now.Add(-9*time.Minute))
	s.alerts.observeStart("h1", rfc3339(now.Add(-5*time.Minute)), now.Add(-4*time.Minute))
	fs := s.alerts.markCondition("h1", AlertTypeOffline, now.Add(-3*time.Minute))
	s.saveAlertState()

	if _, err := os.Stat(alertStatePath); err != nil {
		t.Fatalf("alert state not written: %v", err)
	}

	restarted := newTestAlertServer()
	restarted.loadAlertState()
	got := restarted.alerts.snapshotBookkeeping()
	if n := len(got.StartTimes["h1"]); n != 2 {
		t.Fatalf("start times after reload = %d, want 2", n)
	}
	if n := len(got.UpgradeStarts["h1"]); n != 1 {
		t.Fatalf("upgrade marks after reload = %d, want 1", n)
	}
	if !got.FirstSeen[alertAckKey("h1", AlertTypeOffline)].Equal(fs) {
		t.Fatalf("firstSeen not restored: %+v", got.FirstSeen)
	}
}

func TestAlertStateLoadPrunesAgedAndKeepsAckFirstSeen(t *testing.T) {
	now := time.Now()
	key := alertAckKey("h1", AlertTypeOffline)
	state := newAlertState()
	ackFS := now.Add(-time.Hour)
	state.firstSeen[key] = ackFS
	state.loadBookkeeping(persistedAlertState{
		StartTimes: map[string][]alertStartObservation{
			"h1": {{StartedAt: now.Add(-5 * time.Hour), ObservedAt: now.Add(-5 * time.Hour)}},
		},
		UpgradeStarts: map[string][]time.Time{"h1": {now.Add(-5 * time.Hour)}},
		FirstSeen:     map[string]time.Time{key: now.Add(-2 * time.Hour)},
	}, now)
	if len(state.startTimes["h1"]) != 0 || len(state.upgradeStarts["h1"]) != 0 {
		t.Fatalf("aged bookkeeping not pruned: %+v %+v", state.startTimes, state.upgradeStarts)
	}
	if !state.firstSeen[key].Equal(ackFS) {
		t.Fatalf("ack FirstSeen overwritten: %v", state.firstSeen[key])
	}
}

func TestAlertStateLoadQuarantinesCorruptFile(t *testing.T) {
	path := withTempAlertStatePath(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestAlertServer()
	s.loadAlertState()
	if _, err := os.Stat(path + alertAcksQuarantineSuffix); err != nil {
		t.Fatalf("corrupt file not quarantined: %v", err)
	}
}

func TestSaveAlertStateSkipsEmptyStateWithoutFile(t *testing.T) {
	path := withTempAlertStatePath(t)
	newTestAlertServer().saveAlertState()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty state should not create a file, stat err = %v", err)
	}
}

func TestHubRestartGrace_SuppressesThenResumesOffline(t *testing.T) {
	state := newAlertState()
	state.markHubStart(fixedNow)
	h := baseHive("h1")
	h.Online = false
	h.LastHeartbeat = rfc3339(fixedNow.Add(-time.Hour))

	got := evaluateAlerts(state, []alertHive{h}, nil, fixedNow.Add(time.Minute))
	if hasAlert(got.Alerts, "h1", AlertTypeOffline) {
		t.Fatalf("offline alert raised inside hub restart grace: %+v", got.Alerts)
	}
	got = evaluateAlerts(state, []alertHive{h}, nil, fixedNow.Add(alertHubRestartGrace))
	if !hasAlert(got.Alerts, "h1", AlertTypeOffline) {
		t.Fatalf("offline alert did not resume after the grace window: %+v", got.Alerts)
	}
}

func TestHubRestartGrace_SuppressesThenResumesCrashLoop(t *testing.T) {
	state := newAlertState()
	state.markHubStart(fixedNow)
	h := baseHive("h1")

	// Three distinct starts, all inside the grace window: enough to trip the
	// rule, but the hub has no pre-restart record so it must stay quiet.
	var got AlertSummary
	for i := 0; i < alertCrashLoopMinRestarts; i++ {
		now := fixedNow.Add(time.Duration(i) * 3 * time.Minute)
		h.StartedAt = rfc3339(now.Add(-30 * time.Second))
		got = evaluateAlerts(state, []alertHive{h}, nil, now)
	}
	if hasAlert(got.Alerts, "h1", AlertTypeCrashLoop) {
		t.Fatalf("crash-loop alert raised inside hub restart grace: %+v", got.Alerts)
	}

	// Once the window closes the observed history (which was still recorded)
	// fires as normal.
	later := fixedNow.Add(alertHubRestartGrace)
	h.StartedAt = rfc3339(later.Add(-30 * time.Second))
	got = evaluateAlerts(state, []alertHive{h}, nil, later)
	if !hasAlert(got.Alerts, "h1", AlertTypeCrashLoop) {
		t.Fatalf("crash-loop alert did not resume after the grace window: %+v", got.Alerts)
	}
}

func TestHubRestartGrace_KeepsConditionKnownBeforeRestart(t *testing.T) {
	state := newAlertState()
	h := baseHive("h1")
	h.Online = false
	h.LastHeartbeat = rfc3339(fixedNow.Add(-time.Hour))
	before := fixedNow.Add(-5 * time.Minute)
	state.firstSeen[alertAckKey("h1", AlertTypeOffline)] = before
	state.markHubStart(fixedNow)

	got := evaluateAlerts(state, []alertHive{h}, nil, fixedNow.Add(time.Minute))
	a, ok := findAlert(got.Alerts, "h1", AlertTypeOffline)
	if !ok {
		t.Fatalf("pre-restart offline alert was suppressed: %+v", got.Alerts)
	}
	if !a.FirstSeen.Equal(before) {
		t.Fatalf("FirstSeen = %v, want persisted %v", a.FirstSeen, before)
	}
}

func TestStartAlertStatePersistence_MarksStartAndFlushesOnCancel(t *testing.T) {
	path := withTempAlertStatePath(t)
	s := newTestAlertServer()
	s.alerts.markCondition("h1", AlertTypeOffline, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.startAlertStatePersistence(ctx)

	if s.alerts.hubStartedAt.IsZero() {
		t.Fatal("hub start not recorded")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state not flushed on cancel: %v", err)
	}
}
