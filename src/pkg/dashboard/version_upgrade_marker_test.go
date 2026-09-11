package dashboard

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// #6765: /api/version must expose the spoke's persisted self-upgrade marker so
// the dashboard can say whether auto-update is retrying or has terminally
// failed, instead of the operator being unable to tell "updated as configured"
// from "updating has a problem".

func TestReadUpgradeMarkerAbsent(t *testing.T) {
	orig := upgradeMarkerPath
	upgradeMarkerPath = filepath.Join(t.TempDir(), "no-such-marker")
	t.Cleanup(func() { upgradeMarkerPath = orig })

	if m := readUpgradeMarker(); m != nil {
		t.Fatalf("readUpgradeMarker() = %v, want nil when no marker exists", m)
	}
}

func TestReadUpgradeMarkerRetrying(t *testing.T) {
	orig := upgradeMarkerPath
	dir := t.TempDir()
	upgradeMarkerPath = filepath.Join(dir, "upgrade-requested")
	t.Cleanup(func() { upgradeMarkerPath = orig })

	requested := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	marker := `{"target_sha":"abc1234","current_sha":"old1111","requested_at":"` +
		requested.Format(time.RFC3339) + `","attempts":2,"last_error":"patch own deployment: forbidden"}`
	if err := os.WriteFile(upgradeMarkerPath, []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}

	m := readUpgradeMarker()
	if m == nil {
		t.Fatal("readUpgradeMarker() = nil, want marker")
	}
	if m["target"] != "abc1234" || m["current"] != "old1111" {
		t.Fatalf("target/current = %v/%v", m["target"], m["current"])
	}
	if m["attempts"] != 2 || m["maxAttempts"] != dashboardSelfUpgradeMaxAttempts {
		t.Fatalf("attempts/maxAttempts = %v/%v", m["attempts"], m["maxAttempts"])
	}
	if m["failed"] != false {
		t.Fatalf("failed = %v, want false below the attempt budget", m["failed"])
	}
	if m["lastError"] != "patch own deployment: forbidden" {
		t.Fatalf("lastError = %v", m["lastError"])
	}
	if m["requestedAt"] != requested.Format(time.RFC3339) {
		t.Fatalf("requestedAt = %v, want %s", m["requestedAt"], requested.Format(time.RFC3339))
	}
}

func TestReadUpgradeMarkerTerminalFailureAndLegacy(t *testing.T) {
	orig := upgradeMarkerPath
	dir := t.TempDir()
	upgradeMarkerPath = filepath.Join(dir, "upgrade-requested")
	t.Cleanup(func() { upgradeMarkerPath = orig })

	// Budget exhausted → failed.
	if err := os.WriteFile(upgradeMarkerPath,
		[]byte(`{"target_sha":"abc1234","current_sha":"old1111","attempts":5}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := readUpgradeMarker()
	if m == nil || m["failed"] != true {
		t.Fatalf("failed = %v, want true at the attempt budget", m)
	}

	// Legacy marker without attempts counts as one prior attempt (mirrors
	// cmd/hive parseUpgradeMarker).
	if err := os.WriteFile(upgradeMarkerPath,
		[]byte(`{"target_sha":"abc1234","current_sha":"old1111"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m = readUpgradeMarker()
	if m == nil || m["attempts"] != 1 || m["failed"] != false {
		t.Fatalf("legacy marker = %v, want attempts=1 failed=false", m)
	}

	// Garbage and empty-target markers must be ignored, not rendered.
	if err := os.WriteFile(upgradeMarkerPath, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if m := readUpgradeMarker(); m != nil {
		t.Fatalf("garbage marker = %v, want nil", m)
	}
	if err := os.WriteFile(upgradeMarkerPath, []byte(`{"attempts":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if m := readUpgradeMarker(); m != nil {
		t.Fatalf("empty-target marker = %v, want nil", m)
	}
}
