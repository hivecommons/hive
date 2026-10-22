package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestApplyDeliveredPrimaryKey pins the heartbeat primary-key write that
// replaced a plain os.WriteFile: the hosted hive logged "failed to write
// github app key from heartbeat: permission denied" on EVERY beat because
// /data is root_squash NFS and the key file was a root-owned 0600 leftover.
func TestApplyDeliveredPrimaryKey(t *testing.T) {
	dir := t.TempDir()
	origDir, origGeneric := appKeys.DataDir, appKeys.DataKeyPath
	appKeys.DataDir = dir
	appKeys.DataKeyPath = filepath.Join(dir, "gh-app-key.pem")
	t.Cleanup(func() { appKeys.DataDir, appKeys.DataKeyPath = origDir, origGeneric })

	keyPath := filepath.Join(dir, "gh-app-key-3568013.pem")
	tmp := filepath.Join(t.TempDir(), "delivered.pem")
	wantFP := writeTestKey(t, tmp)
	delivered, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// First delivery: no file yet → written atomically, reported as changed.
	changed, err := applyDeliveredPrimaryKey(logger, keyPath, string(delivered))
	if err != nil || !changed {
		t.Fatalf("first delivery: changed=%v err=%v, want changed with no error", changed, err)
	}
	if fp, _ := config.AppKeyFingerprintFromFile(keyPath); fp != wantFP {
		t.Fatalf("written fingerprint %q, want %q", fp, wantFP)
	}
	if st, err := os.Stat(keyPath); err != nil || st.Mode().Perm() != appKeys.FileMode {
		t.Fatalf("mode = %v (err %v), want %v", st.Mode().Perm(), err, appKeys.FileMode)
	}
	if !strings.Contains(buf.String(), "key_changed=true") {
		t.Fatalf("first delivery log = %q, want key_changed=true", buf.String())
	}

	// Redelivery of the SAME key (every beat): nothing written, not changed.
	before, _ := os.Stat(keyPath)
	buf.Reset()
	changed, err = applyDeliveredPrimaryKey(logger, keyPath, string(delivered))
	if err != nil || changed {
		t.Fatalf("redelivery: changed=%v err=%v, want unchanged with no error", changed, err)
	}
	after, _ := os.Stat(keyPath)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("redelivery of an identical key must not rewrite the file")
	}
	if !strings.Contains(buf.String(), "unchanged; not rewritten") || strings.Contains(buf.String(), "written via heartbeat") {
		t.Fatalf("redelivery log = %q, want the unchanged debug line only", buf.String())
	}

	// A different key replaces the file (via rename, so an unwritable but
	// replaceable file is handled) and is reported as changed.
	tmp2 := filepath.Join(t.TempDir(), "rotated.pem")
	rotatedFP := writeTestKey(t, tmp2)
	rotated, _ := os.ReadFile(tmp2)
	if err := os.Chmod(keyPath, 0o400); err != nil {
		t.Fatal(err)
	}
	changed, err = applyDeliveredPrimaryKey(logger, keyPath, string(rotated))
	if err != nil || !changed {
		t.Fatalf("rotation: changed=%v err=%v, want changed with no error", changed, err)
	}
	if fp, _ := config.AppKeyFingerprintFromFile(keyPath); fp != rotatedFP {
		t.Fatalf("rotated fingerprint %q, want %q", fp, rotatedFP)
	}

	// Garbage is refused without touching the file.
	if _, err := applyDeliveredPrimaryKey(logger, keyPath, "not a key"); err == nil {
		t.Fatal("non-PEM delivery must error")
	}
	if fp, _ := config.AppKeyFingerprintFromFile(keyPath); fp != rotatedFP {
		t.Fatal("failed delivery must leave the existing key intact")
	}
}
