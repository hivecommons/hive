package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// contribute_lease_persist_test.go pins the crash-safe persist idiom that
// 8c7d6bc ported onto the task-lease registry (the #5625 standard: unique
// os.CreateTemp name, explicit 0600 chmod, fsync before rename, directory
// fsync after). The port shipped with no test delta, leaving saveLeasesLocked
// at 60.8% — the happy path was pinned by contribute_lease_restart_test.go,
// but none of the NEW invariants were: the owner-only file mode, the
// no-stale-temp guarantee on both success and failure, and the warn-and-return
// (never panic, never half-write) posture of every error branch.
//
// These matter because the registry is the C4 authorization record a resume is
// matched against after a hub restart (#5681): a world-readable copy leaks
// which contributor holds which work item, and a stale fixed-name .tmp beside
// the registry is exactly the clobber-in-flight hazard the unique temp name
// exists to remove.

// persistHub returns a minimal hub wired to persist its lease registry at
// leasesPath, with one live lease so saveLeasesLocked has something to write.
func persistHub(t *testing.T, leasesPath string) *ContributeWSHub {
	t.Helper()
	h := &ContributeWSHub{
		logger:             covBLogger(),
		persistTaskLedgers: true,
		taskLeasesFile:     leasesPath,
		leases: map[string]*taskLease{
			leaseKey("clanker-7", "task-5681"): {
				identity:  "clanker-7",
				taskID:    "task-5681",
				repo:      "hivecommons/hive",
				number:    5681,
				key:       "hivecommons/hive#5681",
				tier:      "C4",
				gen:       3,
				expiresAt: time.Now().Add(30 * time.Minute),
			},
		},
	}
	return h
}

// listStaleTemps returns every CreateTemp-style leftover ("<base>.*.tmp")
// sitting beside the registry file.
func listStaleTemps(t *testing.T, leasesPath string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(leasesPath), filepath.Base(leasesPath)+".*.tmp"))
	if err != nil {
		t.Fatalf("globbing temp files: %v", err)
	}
	return matches
}

// TestLeasePersist_FileIsOwnerOnly pins the 0600 invariant: the registry is an
// authorization record, and the explicit Chmod exists so the mode is a stated
// contract rather than an accident of CreateTemp's default.
func TestLeasePersist_FileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ws-state", "task-leases.json")
	h := persistHub(t, path)

	h.leaseMu.Lock()
	h.saveLeasesLocked()
	h.leaseMu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("registry was not written: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("lease registry mode = %04o, want 0600 (owner-only authorization record)", got)
	}
	if temps := listStaleTemps(t, path); len(temps) != 0 {
		t.Errorf("successful save left stale temp files beside the registry: %v", temps)
	}

	// The bytes that landed must be the bytes loadLeases boots from.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading registry back: %v", err)
	}
	var records []persistedLease
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatalf("registry is not valid JSON: %v", err)
	}
	if len(records) != 1 || records[0].Identity != "clanker-7" || records[0].Gen != 3 {
		t.Errorf("registry round-trip = %+v, want the single clanker-7 gen-3 lease", records)
	}
	if records[0].Stage != "" || strings.Contains(string(data), `"stage"`) {
		t.Errorf("plain lease persisted a stage field: record=%+v json=%s", records[0], data)
	}
}

// TestLeasePersist_ExpiredLeaseNeverWritten pins the skip-at-write half of the
// "a lease that can no longer be re-adopted must not come back from disk"
// contract (loadLeases pins the skip-at-read half).
func TestLeasePersist_ExpiredLeaseNeverWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-leases.json")
	h := persistHub(t, path)
	h.leases[leaseKey("expired-1", "task-old")] = &taskLease{
		identity:  "expired-1",
		taskID:    "task-old",
		repo:      "hivecommons/hive",
		number:    1,
		gen:       1,
		expiresAt: time.Now().Add(-time.Minute),
	}
	h.leases[leaseKey("zero-expiry", "task-zero")] = &taskLease{
		identity: "zero-expiry",
		taskID:   "task-zero",
		gen:      2,
		// expiresAt zero: an unbounded lease is not a thing the hub issues, so
		// a record without an expiry must not be persisted as if it were one.
	}

	h.leaseMu.Lock()
	h.saveLeasesLocked()
	h.leaseMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("registry was not written: %v", err)
	}
	if s := string(data); strings.Contains(s, "expired-1") || strings.Contains(s, "zero-expiry") {
		t.Errorf("expired/unbounded leases reached disk: %s", s)
	}
	var records []persistedLease
	if err := json.Unmarshal(data, &records); err != nil || len(records) != 1 {
		t.Fatalf("want exactly the one live lease on disk, got %s (err=%v)", data, err)
	}
}

// TestLeasePersist_RenameFailureRemovesTemp drives the rename error branch:
// the destination is occupied by a non-empty directory, so os.Rename must
// fail. The contract is warn-and-return — the unique temp file is removed
// (keep=false), nothing panics, and the previous registry state (here: the
// blocking directory) is untouched.
func TestLeasePersist_RenameFailureRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-leases.json")
	// A non-empty directory at the target path defeats rename on every POSIX
	// filesystem without needing permission tricks (which root would ignore).
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o755); err != nil {
		t.Fatalf("building blocking directory: %v", err)
	}
	h := persistHub(t, path)

	h.leaseMu.Lock()
	h.saveLeasesLocked()
	h.leaseMu.Unlock()

	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("blocking directory should have survived the failed save: %v", err)
	}
	if temps := listStaleTemps(t, path); len(temps) != 0 {
		t.Errorf("failed rename left stale temp files beside the registry: %v", temps)
	}
}

// TestLeasePersist_NilAndDisabledAreNoOps pins the guard clause: a nil hub and
// a hub with persistence off must both return without touching disk. The nil
// receiver matters because saveLeasesLocked is called from paths that can run
// before the hub is fully wired.
func TestLeasePersist_NilAndDisabledAreNoOps(t *testing.T) {
	var nilHub *ContributeWSHub
	nilHub.saveLeasesLocked() // must not panic

	path := filepath.Join(t.TempDir(), "task-leases.json")
	h := persistHub(t, path)
	h.persistTaskLedgers = false

	h.leaseMu.Lock()
	h.saveLeasesLocked()
	h.leaseMu.Unlock()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("persistence disabled but registry written anyway (stat err=%v)", err)
	}
}

// TestLeasePersist_SaveThenLoadRestoresLease closes the loop the crash-safety
// work exists for: what saveLeasesLocked commits, a fresh hub's loadLeases
// restores — same identity, same generation, marked restored — and the new
// hub's generation counter is advanced past every restored gen so post-restart
// assignments cannot alias pre-restart ones (#2568).
func TestLeasePersist_SaveThenLoadRestoresLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-leases.json")
	h := persistHub(t, path)

	h.leaseMu.Lock()
	h.saveLeasesLocked()
	h.leaseMu.Unlock()

	h2 := &ContributeWSHub{
		logger:             covBLogger(),
		persistTaskLedgers: true,
		taskLeasesFile:     path,
	}

	h2.loadLeases()

	h2.leaseMu.Lock()
	lease := h2.leaseForLocked("clanker-7", "task-5681")
	h2.leaseMu.Unlock()
	if lease == nil {
		t.Fatal("lease did not survive the save/load round trip")
	}
	if !lease.restored {
		t.Error("round-tripped lease not marked restored")
	}
	if lease.gen != 3 || lease.taskID != "task-5681" || lease.key != "hivecommons/hive#5681" {
		t.Errorf("restored lease = %+v, want gen=3 taskID=task-5681 key=hivecommons/hive#5681", lease)
	}
	if got := h2.taskGen.Load(); got < 3 {
		t.Errorf("taskGen = %d after restore, want >= 3 so new assignments cannot alias restored gens", got)
	}
}

func TestLeasePersist_StageSurvivesSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-leases.json")
	now := time.Now()
	h := &ContributeWSHub{
		logger:             covBLogger(),
		persistTaskLedgers: true,
		taskLeasesFile:     path,
	}
	if err := h.recordLeaseForKeyStage("clanker-stage", "task-stage", "hivecommons/hive", 8297,
		"hivecommons/hive#8297", "C4", StageSpec, 8, now); err != nil {
		t.Fatalf("record staged lease: %v", err)
	}
	h.leaseMu.Lock()
	if l := h.leaseForLocked("clanker-stage", "task-stage"); l != nil {
		l.mcpTokenID = "lease-token-1"
		l.mcpTokenHash = "hash-1"
		l.mcpTokenStage = StageSpec
	}
	if err := h.saveLeasesLocked(); err != nil {
		t.Fatalf("persist staged MCP token claim: %v", err)
	}
	h.leaseMu.Unlock()

	h2 := &ContributeWSHub{
		logger:             covBLogger(),
		persistTaskLedgers: true,
		taskLeasesFile:     path,
	}
	h2.loadLeases()

	got := h2.lookupLease("clanker-stage", "task-stage", "hivecommons/hive", 8297, 8, now)
	if got == nil {
		t.Fatal("staged lease did not load")
	}
	if got.stage != StageSpec {
		t.Fatalf("loaded stage = %q, want %q", got.stage, StageSpec)
	}
	if got.mcpTokenID != "lease-token-1" || got.mcpTokenHash != "hash-1" || got.mcpTokenStage != StageSpec {
		t.Fatalf("loaded MCP token claim = id=%q hash=%q stage=%q", got.mcpTokenID, got.mcpTokenHash, got.mcpTokenStage)
	}
}

func TestLeasePersist_AdvanceFailureRestoresMemory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-leases.json")
	now := time.Now()
	h := &ContributeWSHub{
		logger:             covBLogger(),
		persistTaskLedgers: true,
		taskLeasesFile:     path,
	}
	if err := h.recordLeaseForKeyStage("clanker-stage", "task-stage", "hivecommons/hive", 8297,
		"hivecommons/hive#8297", "C4", StageSpec, 8, now); err != nil {
		t.Fatalf("record staged lease: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove registry before injecting rename failure: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o755); err != nil {
		t.Fatalf("occupy registry path: %v", err)
	}

	if _, err := h.advanceLeaseStage("clanker-stage", "task-stage", StagePlan, now.Add(time.Minute)); err == nil {
		t.Fatal("advance reported success despite persist failure")
	}
	h.leaseMu.Lock()
	live := h.leaseForLocked("clanker-stage", "task-stage")
	h.leaseMu.Unlock()
	if live == nil {
		t.Fatal("failed advance removed the live lease")
	}
	if live.stage != StageSpec || live.gen != 8 {
		t.Fatalf("failed advance left live lease mutated: stage=%q gen=%d", live.stage, live.gen)
	}
}

// readOnlyLeaseDir returns a lease-registry path inside a directory the test
// cannot create files in (0500), so os.CreateTemp fails and every save is a
// write failure. The mode is restored on cleanup so t.TempDir can remove it.
// Root ignores directory modes, so the caller is skipped under uid 0.
func readOnlyLeaseDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("directory modes do not stop root; cannot inject a write failure")
	}
	dir := filepath.Join(t.TempDir(), "ws-state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("building leases directory: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("making leases directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return filepath.Join(dir, "task-leases.json")
}

// TestLeasePersist_WriteFailureRefusesGrant pins #8287: a lease that did not
// reach disk is not a lease. saveLeasesLocked used to log every failure and
// return nothing, so recordLeaseForKey reported success for a grant that would
// be gone on the next restart. Now the save's error propagates, the grant is
// withdrawn from the live registry, and lookupLease finds nothing for the
// identity — the same answer a restarted hub would have given.
func TestLeasePersist_WriteFailureRefusesGrant(t *testing.T) {
	path := readOnlyLeaseDir(t)
	now := time.Now()
	h := &ContributeWSHub{
		logger:             covBLogger(),
		persistTaskLedgers: true,
		taskLeasesFile:     path,
	}

	err := h.recordLeaseForKey("clanker-8287", "task-8287", "hivecommons/hive", 8287,
		"hivecommons/hive#8287", "C4", 5, now)
	if err == nil {
		t.Fatal("#8287: recordLeaseForKey reported success for a lease that never reached disk")
	}
	if got := h.lookupLease("clanker-8287", "task-8287", "hivecommons/hive", 8287, 5, now); got != nil {
		t.Errorf("#8287: refused grant is still re-adoptable from the live registry: %+v", got)
	}
	h.leaseMu.Lock()
	live := h.leaseForLocked("clanker-8287", "task-8287")
	h.leaseMu.Unlock()
	if live != nil {
		t.Errorf("#8287: refused grant left in the live registry: %+v", live)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("registry written despite the read-only directory (stat err=%v)", statErr)
	}
	if temps := listStaleTemps(t, path); len(temps) != 0 {
		t.Errorf("failed save left stale temp files beside the registry: %v", temps)
	}

	// The error is the save's own, not something recordLeaseForKey invented: a
	// direct save into the same directory fails the same way.
	h.leaseMu.Lock()
	saveErr := h.saveLeasesLocked()
	h.leaseMu.Unlock()
	if saveErr == nil {
		t.Error("#8287: saveLeasesLocked returned nil for a write into a read-only directory")
	}
}

// TestLeasePersist_WriteFailureKeepsPreviousLease pins the re-record half of
// the withdrawal: when re-recording a task the identity already holds fails to
// persist, the task's PREVIOUS (persisted) lease is put back rather than the
// task being dropped, so the live registry keeps describing what is on disk.
func TestLeasePersist_WriteFailureKeepsPreviousLease(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory modes do not stop root; cannot inject a write failure")
	}
	dir := filepath.Join(t.TempDir(), "ws-state")
	path := filepath.Join(dir, "task-leases.json")
	now := time.Now()
	h := &ContributeWSHub{
		logger:             covBLogger(),
		persistTaskLedgers: true,
		taskLeasesFile:     path,
	}
	if err := h.recordLease("clanker-7", "task-5681", "hivecommons/hive", 5681, "C4", 3, now); err != nil {
		t.Fatalf("persisting the first lease: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("making leases directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := h.recordLease("clanker-7", "task-5681", "hivecommons/hive", 5681, "C4", 9, now); err == nil {
		t.Fatal("#8287: re-record reported success for a lease that never reached disk")
	}
	got := h.lookupLease("clanker-7", "task-5681", "hivecommons/hive", 5681, 3, now)
	if got == nil || got.gen != 3 {
		t.Fatalf("#8287: the persisted gen-3 lease was not restored after the failed re-record: %+v", got)
	}
	if h.lookupLease("clanker-7", "task-5681", "hivecommons/hive", 5681, 9, now) != nil {
		t.Error("#8287: the unpersisted gen-9 lease is re-adoptable from the live registry")
	}
}
