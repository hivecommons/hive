package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

func useTempUpgradeHistoryDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := upgradeHistoryDir
	upgradeHistoryDir = dir
	t.Cleanup(func() { upgradeHistoryDir = orig })
	return dir
}

func TestUpgradeHistoryStartEndRoundTrip(t *testing.T) {
	useTempUpgradeHistoryDir(t)
	u := newUpgradeHistoryStore()
	t0 := time.Date(2026, 10, 26, 17, 0, 0, 0, time.UTC)

	u.start("hive-a", "551ffc34f1234", "abcdef0123456", spoke.UpgradeTriggerScheduled, t0)
	open, ok := u.open("hive-a")
	if !ok || !open.Open() || open.Target != "abcdef0123456" || open.Trigger != spoke.UpgradeTriggerScheduled {
		t.Fatalf("open attempt = %+v, ok=%v", open, ok)
	}
	// A retry of the same open attempt re-targets rather than appending.
	u.start("hive-a", "551ffc34f1234", "fedcba9876543", spoke.UpgradeTriggerScheduled, t0.Add(time.Minute))
	if got := u.recent("hive-a"); len(got) != 1 || got[0].Target != "fedcba9876543" {
		t.Fatalf("retry should update the open row, got %+v", got)
	}
	u.end("hive-a", spoke.UpgradeOutcomeDone, "", "", "", t0.Add(10*time.Minute))
	got := u.recent("hive-a")
	if len(got) != 1 || got[0].Outcome != spoke.UpgradeOutcomeDone || got[0].EndedAt.IsZero() {
		t.Fatalf("closed row = %+v", got)
	}
	// A missed outcome with nothing open has no window to blame: dropped.
	u.end("hive-a", spoke.UpgradeOutcomeMissed, "drain lapsed", "", "", t0.Add(time.Hour))
	if got := u.recent("hive-a"); len(got) != 1 {
		t.Fatalf("missed with nothing open must be dropped, got %d rows", len(got))
	}
	// A done outcome with nothing open is an observed rollout: one row.
	u.end("hive-a", spoke.UpgradeOutcomeDone, "", "fedcba9876543", "0123456789abc", t0.Add(2*time.Hour))
	got = u.recent("hive-a")
	if len(got) != 2 || got[1].Trigger != spoke.UpgradeTriggerObserved || got[1].Target != "0123456789abc" {
		t.Fatalf("observed row = %+v", got)
	}
}

func TestUpgradeHistoryStaleOpenRowIsSuperseded(t *testing.T) {
	useTempUpgradeHistoryDir(t)
	u := newUpgradeHistoryStore()
	t0 := time.Date(2026, 10, 25, 17, 0, 0, 0, time.UTC)
	u.start("h", "aaa", "bbb", spoke.UpgradeTriggerManual, t0)
	u.start("h", "aaa", "ccc", spoke.UpgradeTriggerScheduled, t0.Add(upgradeHistoryRecentFor+time.Hour))
	got := u.recent("h")
	if len(got) != 2 {
		t.Fatalf("want stale row closed + new row, got %+v", got)
	}
	if got[0].Outcome != spoke.UpgradeOutcomeMissed || !strings.Contains(got[0].Reason, "superseded") {
		t.Fatalf("stale row = %+v", got[0])
	}
	if !got[1].Open() || got[1].Target != "ccc" {
		t.Fatalf("new row = %+v", got[1])
	}
}

func TestUpgradeHistoryWindowMissedOneRowPerDay(t *testing.T) {
	useTempUpgradeHistoryDir(t)
	u := newUpgradeHistoryStore()
	loc, err := autoUpgradeLocation()
	if err != nil {
		t.Fatal(err)
	}
	day1 := time.Date(2026, 10, 26, 13, 0, 0, 0, loc)
	u.windowMissed("h", "551ffc3", "hub restarted; latch recovering", day1)
	u.windowMissed("h", "551ffc3", "spoke still draining", day1.Add(30*time.Minute))
	got := u.recent("h")
	if len(got) != 1 {
		t.Fatalf("same window must dedupe to one row, got %d", len(got))
	}
	if got[0].Trigger != spoke.UpgradeTriggerWindow || got[0].Outcome != spoke.UpgradeOutcomeDelayed || got[0].Reason != "spoke still draining" {
		t.Fatalf("window row = %+v", got[0])
	}
	u.windowMissed("h", "551ffc3", "spoke still draining", day1.Add(24*time.Hour))
	if got := u.recent("h"); len(got) != 2 {
		t.Fatalf("next day's window must be its own row, got %d", len(got))
	}
	// A window that fired has an open attempt — nothing to record.
	u.start("h", "551ffc3", "abc1234", spoke.UpgradeTriggerScheduled, day1.Add(48*time.Hour))
	u.windowMissed("h", "551ffc3", "late reason", day1.Add(48*time.Hour+time.Minute))
	if got := u.recent("h"); len(got) != 3 || !got[2].Open() {
		t.Fatalf("open attempt must not be shadowed by a window row, got %+v", got)
	}
}

func TestUpgradeHistoryRotationAndEnvCap(t *testing.T) {
	useTempUpgradeHistoryDir(t)
	t.Setenv("HIVE_HUB_UPGRADE_HISTORY_MAX", "3")
	u := newUpgradeHistoryStore()
	t0 := time.Date(2026, 10, 1, 17, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		at := t0.Add(time.Duration(i) * 24 * time.Hour)
		u.start("h", "aaa", "bbb", spoke.UpgradeTriggerScheduled, at)
		u.end("h", spoke.UpgradeOutcomeDone, "", "", "", at.Add(5*time.Minute))
	}
	got := u.recent("h")
	if len(got) != 3 {
		t.Fatalf("cap 3, got %d rows", len(got))
	}
	if !got[0].StartedAt.Equal(t0.Add(3 * 24 * time.Hour)) {
		t.Fatalf("oldest surviving row should be day 3, got %s", got[0].StartedAt)
	}
	if upgradeHistoryMax() != 3 {
		t.Fatalf("env cap not honoured: %d", upgradeHistoryMax())
	}
}

func TestUpgradeHistoryPersistsAndLoads(t *testing.T) {
	dir := useTempUpgradeHistoryDir(t)
	u := newUpgradeHistoryStore()
	t0 := time.Date(2026, 10, 26, 17, 0, 0, 0, time.UTC)
	u.start("fma", "551ffc3", "abc1234", spoke.UpgradeTriggerScheduled, t0)
	u.end("fma", spoke.UpgradeOutcomeMissed, "drain lapsed", "", "", t0.Add(time.Hour))
	u.windowMissed("fma", "551ffc3", "hub restarted", t0.Add(24*time.Hour))
	if _, err := os.Stat(filepath.Join(dir, "fma.json")); err != nil {
		t.Fatalf("history file not written: %v", err)
	}

	reloaded := newUpgradeHistoryStore()
	reloaded.load()
	got := reloaded.recent("fma")
	if len(got) != 2 {
		t.Fatalf("reload: want 2 rows, got %+v", got)
	}
	if got[0].Reason != "drain lapsed" || got[1].Trigger != spoke.UpgradeTriggerWindow {
		t.Fatalf("reload rows = %+v", got)
	}

	reloaded.forget("fma")
	if reloaded.recent("fma") != nil {
		t.Fatal("forget must drop in-memory rows")
	}
	if _, err := os.Stat(filepath.Join(dir, "fma.json")); !os.IsNotExist(err) {
		t.Fatalf("forget must remove the file, stat err=%v", err)
	}
	// Path traversal in a hive id never escapes the directory.
	if p := upgradeHistoryPath("../etc"); p != "" {
		t.Fatalf("traversal id produced a path: %q", p)
	}
}

func TestHistoryRefTrimsCommitsOnly(t *testing.T) {
	if got := historyRef("551ffc34f0123456789"); got != "551ffc34f012" && len(got) != StandardSHALen {
		t.Fatalf("historyRef(long sha) = %q", got)
	}
	if got := historyRef("candidate"); got != "candidate" {
		t.Fatalf("channel name must pass through, got %q", got)
	}
	if got := historyRef(""); got != "" {
		t.Fatalf("empty must stay empty, got %q", got)
	}
}

func TestUpgradeTriggerForSchedule(t *testing.T) {
	if got := upgradeTriggerForSchedule(AutoUpgradeModeDaily, ReleaseChannelStable); got != spoke.UpgradeTriggerScheduled {
		t.Fatalf("daily stable = %q", got)
	}
	if got := upgradeTriggerForSchedule(AutoUpgradeModeInstant, ""); got != spoke.UpgradeTriggerInstant {
		t.Fatalf("instant = %q", got)
	}
}
