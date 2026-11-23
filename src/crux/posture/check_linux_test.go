//go:build linux

package posture

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestCheckStatfsFailureIsAGap checks the root filesystem probe fails closed
// (#19479): when statfs on / fails the read-only state is unknown, so it is
// reported as a gap and strict mode refuses to start, rather than passing as
// if the root filesystem were read-only.
func TestCheckStatfsFailureIsAGap(t *testing.T) {
	status := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(status, []byte("NoNewPrivs:\t1\nCapEff:\t0000000000000000\nSeccomp:\t2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failing := func(string, *syscall.Statfs_t) error { return errors.New("statfs boom") }
	gaps := check(status, failing)
	if len(gaps) != 1 || !strings.Contains(gaps[0], "root filesystem") || !strings.Contains(gaps[0], "statfs boom") {
		t.Fatalf("gaps with a failing statfs = %q, want one root filesystem gap", gaps)
	}

	readOnly := func(_ string, fs *syscall.Statfs_t) error { fs.Flags = 1; return nil }
	if gaps := check(status, readOnly); len(gaps) != 0 {
		t.Errorf("gaps with a read-only root = %q, want none", gaps)
	}
	writable := func(_ string, fs *syscall.Statfs_t) error { fs.Flags = 0; return nil }
	if gaps := check(status, writable); len(gaps) != 1 || !strings.Contains(gaps[0], "--read-only") {
		t.Errorf("gaps with a writable root = %q, want the --read-only gap", gaps)
	}
}
