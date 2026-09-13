package hub

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================
// saas_provision.go — saveVanityMintLedgerLocked error branches
//
// The ledger save is best-effort: every failure must warn and return without
// panicking, because it runs inside the mint path holding vanityMintMu. Only
// the happy path was covered before; these tests hit the mkdir, write, and
// rename failures. (The marshal branch is unreachable for a []time.Time.)
// ============================================================

func TestSaveVanityMintLedgerErrorBranches(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions are advisory for root")
	}
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	var buf bytes.Buffer
	s := &HubServer{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	s.vanityMintTimes = []time.Time{time.Now().UTC()}

	// MkdirAll failure: a path component of the ledger dir is a regular file.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	saasHivesDir = filepath.Join(blocker, "sub", "hives")
	buf.Reset()
	s.saveVanityMintLedgerLocked()
	if !strings.Contains(buf.String(), "failed to create ledger directory") {
		t.Errorf("mkdir failure not warned: %s", buf.String())
	}

	// WriteFile failure: ledger dir exists but is read-only.
	roParent := t.TempDir()
	roDir := filepath.Join(roParent, "ro")
	if err := os.Mkdir(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(roDir, 0o755) })
	saasHivesDir = filepath.Join(roDir, "hives")
	buf.Reset()
	s.saveVanityMintLedgerLocked()
	if !strings.Contains(buf.String(), "failed to write ledger") {
		t.Errorf("write failure not warned: %s", buf.String())
	}

	// Rename failure: the ledger path itself is occupied by a directory.
	occupied := t.TempDir()
	saasHivesDir = filepath.Join(occupied, "hives")
	if err := os.Mkdir(filepath.Join(occupied, "vanity-mint-times.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	s.saveVanityMintLedgerLocked()
	if !strings.Contains(buf.String(), "failed to replace ledger") {
		t.Errorf("rename failure not warned: %s", buf.String())
	}
}
