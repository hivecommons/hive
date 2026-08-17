package proclock

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The whole point of the lock (#2453/#2496): a second acquisition while the
// first is held MUST fail. flock contention is per open file description, so
// two Acquire calls in one process exercise the same kernel path a second
// hive process in the same container would.
func TestSecondAcquisitionFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive.singleton.lock")

	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquisition failed: %v", err)
	}
	defer first.Release()

	second, err := Acquire(path)
	if err == nil {
		second.Release()
		t.Fatal("second concurrent acquisition succeeded — a duplicate hive process would be allowed to run")
	}
	// The refusal must name the holder so the container log points at the
	// culprit PID, not just "locked".
	wantPID := fmt.Sprintf("%d", os.Getpid())
	if !strings.Contains(err.Error(), wantPID) {
		t.Errorf("refusal does not name the holder PID %s: %v", wantPID, err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("refusal does not name the lock path: %v", err)
	}
}

// Releasing the lock (as process death does implicitly) must make the lock
// acquirable again — a legitimate restart is never blocked by the previous
// process's leftover lock FILE, only by a still-LIVE holder.
func TestReleaseAllowsReacquisition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive.singleton.lock")

	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquisition failed: %v", err)
	}
	first.Release()

	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("re-acquisition after release failed — a stale lock file blocked a clean restart: %v", err)
	}
	second.Release()
}

// The holder records its PID in the file for diagnosability.
func TestLockFileRecordsHolderPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive.singleton.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquisition failed: %v", err)
	}
	defer l.Release()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("lock file unreadable: %v", err)
	}
	want := fmt.Sprintf("%d", os.Getpid())
	if strings.TrimSpace(string(data)) != want {
		t.Errorf("lock file records %q, want holder PID %q", strings.TrimSpace(string(data)), want)
	}
	if l.Path() != path {
		t.Errorf("Path() = %q, want %q", l.Path(), path)
	}
}

// An unwritable location must fail loudly, not silently skip the guard.
func TestUnwritableLocationErrors(t *testing.T) {
	if _, err := Acquire(filepath.Join(t.TempDir(), "no-such-dir", "hive.lock")); err == nil {
		t.Fatal("acquiring in a nonexistent directory succeeded")
	}
}

// Nil-safety for the accessors (defensive: Release on a failed acquisition).
func TestNilLockSafe(t *testing.T) {
	var l *Lock
	l.Release() // must not panic
	if l.Path() != "" {
		t.Error("nil lock has a path")
	}
}
