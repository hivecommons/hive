package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// contribute_ws_ledger_persist_errors_test.go pins the warn-and-return posture
// of the contributor-ledger persistence helpers in contribute_ws.go. These
// ledgers (activity feed, completed/failed task cooldowns, no-PR streaks,
// no-work verdicts, task leases) are what a restarted hub boots from; a save
// that panics — or half-writes — under a disk fault would either take the hub
// down or corrupt the very record the crash-safe idiom exists to protect.
//
// contribute_lease_persist_test.go pinned this contract for saveLeasesLocked's
// temp/rename half; this file extends it to the sibling ledgers' mkdir, write
// and rename failure branches, the *Path() default fallbacks, and
// loadCompletedTasks' legacy/corrupt-input tolerance — all of which were
// uncovered. The failure injections use directory/file blockers rather than
// permission bits, so they hold even when the test runs as root.

// ledgerHub returns a minimal hub with every ledger persisted under dir and
// one live entry in each ledger so every save has something to write.
func ledgerHub(dir string) *ContributeWSHub {
	now := time.Now()
	return &ContributeWSHub{
		logger:             covBLogger(),
		persistActivity:    true,
		persistTaskLedgers: true,
		activityFilePath:   filepath.Join(dir, "activity.json"),
		completedTasksFile: filepath.Join(dir, "completed-tasks.json"),
		failedTasksFile:    filepath.Join(dir, "failed-tasks.json"),
		noPRStreaksFile:    filepath.Join(dir, "no-pr-streaks.json"),
		noWorkVerdictsFile: filepath.Join(dir, "no-work-verdicts.json"),
		taskLeasesFile:     filepath.Join(dir, "task-leases.json"),
		activity: []ActivityEntry{
			{Username: "alice", Action: "joined", Timestamp: now.UTC().Format(time.RFC3339)},
		},
		completedTasks:        map[string]time.Time{"o/r#1": now},
		completedTaskCooldown: map[string]time.Duration{"o/r#1": 4 * time.Hour},
		completedTaskPRURL:    map[string]string{"o/r#1": "https://github.com/o/r/pull/9"},
		failedTasks:           map[string]time.Time{"o/r#2": now},
		consecutiveFailures:   map[string]int{"o/r#2": 2},
		noPRStreaks:           map[string]noPRStreakRecord{"o/r#3": {Count: 1, LastAt: now}},
		noWorkVerdicts:        map[string]noWorkVerdictRecord{"o/r#4": {RecordedAt: now, SuppressHours: 4}},
	}
}

// ledgerSaves names each sibling save function together with a getter for the
// path it writes, so the failure-injection tests below can drive all of them
// through one table. saveLeasesLocked is included via its lock-holding wrapper.
func ledgerSaves(h *ContributeWSHub) map[string]struct {
	save func()
	path string
} {
	return map[string]struct {
		save func()
		path string
	}{
		"activity":        {h.saveActivity, h.activityPath()},
		"completed-tasks": {h.saveCompletedTasks, h.completedTasksPath()},
		"failed-tasks":    {h.saveFailedTasks, h.failedTasksPath()},
		"no-pr-streaks":   {h.saveNoPRStreaks, h.noPRStreaksPath()},
		"no-work-verdicts": {h.saveNoWorkVerdicts, func() string {
			return h.noWorkVerdictsPath()
		}()},
		"task-leases": {func() {
			h.leaseMu.Lock()
			h.saveLeasesLocked()
			h.leaseMu.Unlock()
		}, h.taskLeasesPath()},
	}
}

// TestLedgerSave_MkdirFailureWarnsAndReturns points every ledger's parent
// "directory" at a regular file, so os.MkdirAll must fail. The contract is
// warn-and-return: no panic, and nothing lands at the target path.
func TestLedgerSave_MkdirFailureWarnsAndReturns(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("building blocking file: %v", err)
	}
	// Every ledger path descends THROUGH the regular file.
	h := ledgerHub(filepath.Join(blocker, "sub"))

	for name, tc := range ledgerSaves(h) {
		t.Run(name, func(t *testing.T) {
			tc.save() // must not panic
			if _, err := os.Stat(tc.path); err == nil {
				t.Errorf("mkdir failed but %s ledger reached disk anyway", name)
			}
		})
	}
}

// TestLedgerSave_TempWriteFailureWarnsAndReturns occupies each fixed-name
// "<path>.tmp" slot with a DIRECTORY, so the ledgers using the
// WriteFile-then-Rename idiom fail at the write. The final path must stay
// absent — a write failure must never dirty the committed file.
// (task-leases is excluded: its #5625 CreateTemp idiom has no fixed temp name
// to occupy, and its temp/rename failures are pinned in
// contribute_lease_persist_test.go.)
func TestLedgerSave_TempWriteFailureWarnsAndReturns(t *testing.T) {
	dir := t.TempDir()
	h := ledgerHub(dir)

	saves := ledgerSaves(h)
	delete(saves, "task-leases")
	for name, tc := range saves {
		t.Run(name, func(t *testing.T) {
			if err := os.MkdirAll(tc.path+".tmp", 0o755); err != nil {
				t.Fatalf("building blocking temp dir: %v", err)
			}
			tc.save() // must not panic
			if _, err := os.Stat(tc.path); !os.IsNotExist(err) {
				t.Errorf("temp write failed but %s ledger committed anyway (stat err=%v)", name, err)
			}
		})
	}
}

// TestLedgerSave_RenameFailureWarnsAndReturns occupies each final path with a
// non-empty directory (rename onto a non-empty dir fails on every POSIX
// filesystem, root included). The blocking directory — standing in for
// whatever the previous committed state was — must survive untouched.
func TestLedgerSave_RenameFailureWarnsAndReturns(t *testing.T) {
	dir := t.TempDir()
	h := ledgerHub(dir)

	saves := ledgerSaves(h)
	delete(saves, "task-leases") // rename branch pinned in contribute_lease_persist_test.go
	for name, tc := range saves {
		t.Run(name, func(t *testing.T) {
			if err := os.MkdirAll(filepath.Join(tc.path, "occupied"), 0o755); err != nil {
				t.Fatalf("building blocking directory: %v", err)
			}
			tc.save() // must not panic
			info, err := os.Stat(tc.path)
			if err != nil || !info.IsDir() {
				t.Fatalf("blocking directory should have survived the failed %s save: %v", name, err)
			}
		})
	}
}

// TestLedgerPathDefaults pins the fallback half of every *Path() accessor: a
// hub whose per-hub override is empty (production wiring) must resolve to the
// package-level location, and a nil hub must not panic. The overrides exist
// solely so tests can redirect disk I/O — if the fallback ever broke, every
// production hub would silently persist to "" and lose its ledgers on restart.
func TestLedgerPathDefaults(t *testing.T) {
	h := &ContributeWSHub{logger: covBLogger()}

	if got := h.taskLeasesPath(); got != taskLeasesFile {
		t.Errorf("taskLeasesPath() = %q, want package default %q", got, taskLeasesFile)
	}
	if got := h.activityPath(); got != activityFilePath {
		t.Errorf("activityPath() = %q, want package default %q", got, activityFilePath)
	}
	if got := h.completedTasksPath(); got != completedTasksFile {
		t.Errorf("completedTasksPath() = %q, want package default %q", got, completedTasksFile)
	}
	if got := h.failedTasksPath(); got != failedTasksFile {
		t.Errorf("failedTasksPath() = %q, want package default %q", got, failedTasksFile)
	}
	if got := h.noPRStreaksPath(); got != noPRStreaksFile {
		t.Errorf("noPRStreaksPath() = %q, want package default %q", got, noPRStreaksFile)
	}

	// The no-work-verdicts default routes through getContributorsDir(), which
	// honours HIVE_CONTRIBUTORS_DIR — the same contract the contributor
	// profiles use (#3987).
	contribDir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", contribDir)
	want := filepath.Join(contribDir, noWorkVerdictsFileName)
	if got := h.noWorkVerdictsPath(); got != want {
		t.Errorf("noWorkVerdictsPath() = %q, want %q", got, want)
	}
}

// TestLoadCompletedTasks_ToleratesCorruptFile: a ledger that no longer parses
// as either the current object form or the legacy map form must be ignored —
// boot continues with an empty cooldown map instead of crashing or
// half-loading.
func TestLoadCompletedTasks_ToleratesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completed-tasks.json")
	if err := os.WriteFile(path, []byte(`{"broken":`), 0o644); err != nil {
		t.Fatalf("writing corrupt ledger: %v", err)
	}
	h := &ContributeWSHub{
		logger:             covBLogger(),
		persistTaskLedgers: true,
		completedTasksFile: path,
		completedTasks:     map[string]time.Time{},
	}

	h.loadCompletedTasks() // must not panic

	if n := len(h.completedTasks); n != 0 {
		t.Errorf("corrupt ledger loaded %d entries, want 0", n)
	}
}

// TestLoadCompletedTasks_LegacyMapForm pins the upgrade path: the pre-object
// ledger was map[key]RFC3339-string, and "an upgrade never drops cooldowns"
// means a fresh entry restores, an unparseable timestamp is skipped (not
// fatal), and an entry past the default cooldown stays dead.
func TestLoadCompletedTasks_LegacyMapForm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completed-tasks.json")
	legacy := map[string]string{
		"o/r#1": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		"o/r#2": "not-a-timestamp",
		"o/r#3": time.Now().Add(-time.Duration(completedTaskCooldownHours+1) * time.Hour).UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshalling legacy ledger: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing legacy ledger: %v", err)
	}
	h := &ContributeWSHub{
		logger:             covBLogger(),
		persistTaskLedgers: true,
		completedTasksFile: path,
		completedTasks:     map[string]time.Time{},
	}

	h.loadCompletedTasks()

	if _, ok := h.completedTasks["o/r#1"]; !ok {
		t.Error("fresh legacy entry o/r#1 did not survive the upgrade load")
	}
	if _, ok := h.completedTasks["o/r#2"]; ok {
		t.Error("unparseable legacy timestamp was loaded as a cooldown")
	}
	if _, ok := h.completedTasks["o/r#3"]; ok {
		t.Error("legacy entry past its cooldown was resurrected")
	}
}

// TestLoadCompletedTasks_ObjectFormHonoursPerRecordCooldown pins the object
// form's semantics: a zero CompletedAt is skipped, a record's own
// CooldownHours (not the default) decides whether it is still live, and a
// restored record carries its cooldown override and PR URL into the maps a
// running hub consults.
func TestLoadCompletedTasks_ObjectFormHonoursPerRecordCooldown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completed-tasks.json")
	records := map[string]completedTaskRecord{
		// 10h old with a 100h override: dead under the default cooldown,
		// alive under its own — must restore.
		"o/r#long": {
			CompletedAt:   time.Now().Add(-10 * time.Hour),
			CooldownHours: 100,
			PRURL:         "https://github.com/o/r/pull/42",
		},
		// 10h old with a 1h override: must stay dead.
		"o/r#short": {
			CompletedAt:   time.Now().Add(-10 * time.Hour),
			CooldownHours: 1,
		},
		// Zero CompletedAt: malformed, skipped.
		"o/r#zero": {},
	}
	data, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("marshalling ledger: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing ledger: %v", err)
	}
	h := &ContributeWSHub{
		logger:                covBLogger(),
		persistTaskLedgers:    true,
		completedTasksFile:    path,
		completedTasks:        map[string]time.Time{},
		completedTaskCooldown: map[string]time.Duration{},
		completedTaskPRURL:    map[string]string{},
	}

	h.loadCompletedTasks()

	if _, ok := h.completedTasks["o/r#long"]; !ok {
		t.Fatal("record alive under its own CooldownHours override was not restored")
	}
	if got := h.completedTaskCooldown["o/r#long"]; got != 100*time.Hour {
		t.Errorf("restored cooldown override = %v, want 100h", got)
	}
	if got := h.completedTaskPRURL["o/r#long"]; got != "https://github.com/o/r/pull/42" {
		t.Errorf("restored PR URL = %q, want the persisted one", got)
	}
	if _, ok := h.completedTasks["o/r#short"]; ok {
		t.Error("record past its own CooldownHours override was resurrected")
	}
	if _, ok := h.completedTasks["o/r#zero"]; ok {
		t.Error("record with zero CompletedAt was loaded")
	}
}

// TestSendTokenRefreshFailed pins both halves of the mint-failure notifier:
// a nil connection is a no-op (the refresh loop can race a teardown), and a
// dead socket downgrades to a debug log — the relay simply never learns of
// the failure, which must not take the heartbeat down with it.
func TestSendTokenRefreshFailed(t *testing.T) {
	h := &ContributeWSHub{logger: covBLogger()}

	h.sendTokenRefreshFailed(nil, "mint failed") // must not panic

	serverConn, _ := wsPair(t)
	c := refreshConn(serverConn, time.Now())
	if err := serverConn.Close(); err != nil {
		t.Fatalf("closing server side: %v", err)
	}

	h.sendTokenRefreshFailed(c, "mint failed") // send fails; must not panic
}
