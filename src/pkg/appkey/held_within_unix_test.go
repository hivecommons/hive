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
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("HeldFingerprintsWithin blocked %v on a stalled scan", el)
	}
	if got != nil {
		t.Fatalf("no completed scan yet, want nil, got %v", got)
	}
	// Release the stalled scan so the goroutine exits.
	if f, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
		f.Close()
	}
}
