//go:build unix

package appkey

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestHeldFingerprintsWithinBoundsSlowScan: a scan that stalls (slow PVC) must
// not hold the heartbeat collect past the bound (#18484).
func TestHeldFingerprintsWithinBoundsSlowScan(t *testing.T) {
	dir := t.TempDir()
	// A FIFO named like a key file blocks the scan's read until opened for write.
	fifo := filepath.Join(dir, "gh-app-key-42.pem")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	r := &Resolver{DataDir: dir}
	start := time.Now()
	got := r.HeldFingerprintsWithin(100 * time.Millisecond)
	elapsed := time.Since(start)
	r.boundMu.Lock()
	done := r.inflight
	r.boundMu.Unlock()
	if done == nil {
		t.Fatal("stalled scan is not in flight")
	}
	t.Cleanup(func() {
		// Opening the FIFO releases the reader; wait for the worker before
		// another test can replace the package's fingerprint function.
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("release stalled scan: %v", err)
			return
		}
		if err := f.Close(); err != nil {
			t.Errorf("close FIFO: %v", err)
		}
		<-done
	})
	if elapsed > 2*time.Second {
		t.Fatalf("HeldFingerprintsWithin blocked %v on a stalled scan", elapsed)
	}
	if got != nil {
		t.Fatalf("no completed scan yet, want nil, got %v", got)
	}
}
