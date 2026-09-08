package proclock

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// A contender whose lock file holds garbage (not a PID) must get the generic
// "held by another process" error, not one naming a bogus holder.
func TestIllegibleHolderFallsBackToGenericError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString("not-a-pid\n"); err != nil {
		t.Fatalf("seed garbage holder: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("take external flock: %v", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	_, err = Acquire(path)
	if err == nil {
		t.Fatal("Acquire succeeded against an externally held lock")
	}
	if !strings.Contains(err.Error(), "held by another process") {
		t.Errorf("error = %q, want the generic 'held by another process' form", err)
	}
	if strings.Contains(err.Error(), "not-a-pid") {
		t.Errorf("error %q leaks the illegible holder content", err)
	}
}

// An empty lock file (holder never recorded a PID) also gets the generic error.
func TestEmptyHolderFallsBackToGenericError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("take external flock: %v", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	_, err = Acquire(path)
	if err == nil {
		t.Fatal("Acquire succeeded against an externally held lock")
	}
	if !strings.Contains(err.Error(), "held by another process") {
		t.Errorf("error = %q, want the generic 'held by another process' form", err)
	}
}

// readHolder must return "" when the descriptor cannot seek (e.g. closed).
func TestReadHolderSeekErrorReturnsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive.lock")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create lock file: %v", err)
	}
	if _, err := f.WriteString("1234\n"); err != nil {
		t.Fatalf("seed holder: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := readHolder(f); got != "" {
		t.Errorf("readHolder on closed file = %q, want empty", got)
	}
}
