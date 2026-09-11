package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hubbackup"
	"github.com/hivecommons/hive/pkg/spokebackup"
)

// cmdRestore is the self-service half of #6529: the verb an owner reaches for
// when a hive has to come back on a new deployment. Its happy paths run in
// process; every refusal goes through the same re-exec harness the other
// exit-code tests use, because a restore that "fails" with exit 0 would tell an
// operator their hive was restored when it was not.

// buildSpokeArchive seals a spoke backup from a synthetic /data-shaped
// directory and returns the archive path plus the hex key that opens it.
func buildSpokeArchive(t *testing.T, hiveID string) (archive, hexKey string) {
	t.Helper()
	hexKey, key := testKey(t)

	src := t.TempDir()
	if hiveID != "" {
		mustWriteFile(t, filepath.Join(src, "hive-id"), hiveID)
	}
	mustWriteFile(t, filepath.Join(src, "hive.yaml.dashboard"), "live: owner-customised\n")
	mustWriteFile(t, filepath.Join(src, "gh-app-key.pem"), "-----BEGIN PRIVATE KEY-----\nAAA\n")
	mustWriteFile(t, filepath.Join(src, "beads", "scanner", "beads.json"), `{"agent":"scanner"}`)
	t.Setenv(spokebackup.EnvDataDir, src)

	res, err := spokebackup.Build(key, hiveID, quietLogger())
	if err != nil {
		t.Fatalf("build spoke archive: %v", err)
	}
	archive = filepath.Join(t.TempDir(), "spoke.tar.gz.enc")
	if err := os.WriteFile(archive, res.Sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	return archive, hexKey
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCmdRestorePlacesSpokeArchiveIntoDataDir(t *testing.T) {
	archive, hexKey := buildSpokeArchive(t, "hive-abc123")
	t.Setenv(hubbackup.EnvBackupKey, hexKey)
	dest := t.TempDir()

	out := captureOutput(t, &os.Stdout, func() {
		cmdRestore([]string{"-file", archive, "-dest", dest}, quietLogger())
	})

	if !strings.Contains(out, "restored ") || !strings.Contains(out, dest) {
		t.Errorf("cmdRestore output = %q", out)
	}
	if !strings.Contains(out, "archive hive-id: hive-abc123") {
		t.Errorf("cmdRestore did not report the archive identity:\n%s", out)
	}
	// The next boot is part of the procedure — the entrypoint owns ownership
	// and the config hardening — so the command has to say so.
	if !strings.Contains(out, "(re)start the hive container") {
		t.Errorf("cmdRestore did not tell the operator to restart:\n%s", out)
	}

	// The two prefixes landed where the entrypoint reads them.
	for path, want := range map[string]string{
		filepath.Join(dest, "hive-id"):                        "hive-abc123",
		filepath.Join(dest, "hive.yaml.dashboard"):            "live: owner-customised\n",
		filepath.Join(dest, "gh-app-key.pem"):                 "-----BEGIN PRIVATE KEY-----\nAAA\n",
		filepath.Join(dest, "beads", "scanner", "beads.json"): `{"agent":"scanner"}`,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("restored file missing: %v", err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	// MANIFEST.json is archive metadata, not spoke state.
	if _, err := os.Stat(filepath.Join(dest, "MANIFEST.json")); !os.IsNotExist(err) {
		t.Error("cmdRestore wrote MANIFEST.json into the data dir")
	}
}

func TestCmdRestoreDryRunReportsWithoutWriting(t *testing.T) {
	archive, hexKey := buildSpokeArchive(t, "hive-abc123")
	t.Setenv(hubbackup.EnvBackupKey, hexKey)
	dest := t.TempDir()

	out := captureOutput(t, &os.Stdout, func() {
		cmdRestore([]string{"-file", archive, "-dest", dest, "-dry-run"}, quietLogger())
	})

	if !strings.Contains(out, "would restore") {
		t.Errorf("dry run did not mark itself as a plan:\n%s", out)
	}
	if strings.Contains(out, "(re)start the hive container") {
		t.Errorf("dry run told the operator to restart after writing nothing:\n%s", out)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dry run wrote %d entries into %s", len(entries), dest)
	}
}

func TestCmdRestoreForceReportsTheOverride(t *testing.T) {
	archive, hexKey := buildSpokeArchive(t, "hive-abc123")
	t.Setenv(hubbackup.EnvBackupKey, hexKey)
	dest := t.TempDir()
	mustWriteFile(t, filepath.Join(dest, "hive-id"), "hive-someone-else")

	out := captureOutput(t, &os.Stdout, func() {
		cmdRestore([]string{"-file", archive, "-dest", dest, "-force"}, quietLogger())
	})

	if !strings.Contains(out, "WARNING: identity check overridden") {
		t.Errorf("a forced restore did not warn:\n%s", out)
	}
	if !strings.Contains(out, "destination hive-id (before): hive-someone-else") {
		t.Errorf("a forced restore did not name the identity it replaced:\n%s", out)
	}
}

// --- exit-code contract ---

func TestMainRestoreMissingFlagsExitsNonzero(t *testing.T) {
	for _, args := range [][]string{
		{"restore"},
		{"restore", "-file", "x.enc"},
		{"restore", "-dest", "/data"},
	} {
		code, out := runMainHelper(t, nil, args...)
		if code != exitCodeError {
			t.Errorf("%v: exit code = %d; want %d", args, code, exitCodeError)
		}
		if !strings.Contains(out, "restore requires -file and -dest") {
			t.Errorf("%v: output did not name the missing flags:\n%s", args, out)
		}
	}
}

func TestMainRestoreWithoutKeyExitsNonzero(t *testing.T) {
	archive, _ := buildSpokeArchive(t, "hive-abc123")
	dest := t.TempDir()

	// No HIVE_BACKUP_KEY in the child env: the archive cannot be opened, and
	// failing closed is the whole contract.
	code, _ := runMainHelper(t, nil, "restore", "-file", archive, "-dest", dest)
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a keyless restore wrote %d entries into the destination", len(entries))
	}
}

func TestMainRestoreOntoDifferentHiveExitsNonzero(t *testing.T) {
	archive, hexKey := buildSpokeArchive(t, "hive-abc123")
	dest := t.TempDir()
	mustWriteFile(t, filepath.Join(dest, "hive-id"), "hive-someone-else")

	code, out := runMainHelper(t, map[string]string{hubbackup.EnvBackupKey: hexKey},
		"restore", "-file", archive, "-dest", dest)
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
	if !strings.Contains(out, "-force") {
		t.Errorf("refusal did not name the escape hatch:\n%s", out)
	}
	// A refused restore must leave the other hive alone.
	got, err := os.ReadFile(filepath.Join(dest, "hive-id"))
	if err != nil || strings.TrimSpace(string(got)) != "hive-someone-else" {
		t.Errorf("hive-id = %q (err %v) after a refused restore; want it untouched", got, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "gh-app-key.pem")); !os.IsNotExist(err) {
		t.Error("a refused restore wrote a GitHub App key")
	}
}

func TestMainRestoreMissingArchiveExitsNonzero(t *testing.T) {
	hexKey, _ := testKey(t)
	code, _ := runMainHelper(t, map[string]string{hubbackup.EnvBackupKey: hexKey},
		"restore", "-file", filepath.Join(t.TempDir(), "absent.enc"), "-dest", t.TempDir())
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
}
