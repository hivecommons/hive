package hub

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestQuarantineClustersFileRenameFailure pins the best-effort contract of
// quarantineClustersFile (clusters_registry.go): when the rename itself fails,
// the function logs and returns without panicking, leaving the original bytes
// in place for the operator — the load outcome is already "refuse", so a
// failed quarantine must not make things worse.
func TestQuarantineClustersFileRenameFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("directory permissions cannot block rename for root")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "clusters.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A read-only parent makes the rename fail (both source unlink and dest
	// creation happen in this directory).
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	quarantineClustersFile(path, slog.Default())

	if _, err := os.Stat(path); err != nil {
		t.Errorf("failed quarantine must leave the original file in place: %v", err)
	}
	if _, err := os.Stat(path + clustersQuarantineSuffix); !os.IsNotExist(err) {
		t.Errorf("no quarantine copy should exist after a failed rename (stat err = %v)", err)
	}
}
