package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hubbackup"
	"github.com/hivecommons/hive/pkg/spokebackup"
)

// buildSpokeArchive assembles and seals a real spokebackup archive (using the
// package's own builder, per AGENT-COMMON's "share code, don't duplicate")
// for hiveID, containing a hive-id file, one other spoke/ file and one bead
// file, returning the sealed bytes and the raw key used.
func buildSpokeArchive(t *testing.T, hiveID string) (sealed, key []byte) {
	t.Helper()
	_, key = testKey(t)

	b := hubbackup.NewBuilder()
	if err := b.AddBytes(filepath.Join(spokebackup.SpokePrefix, spokebackup.HiveIDFile), 0o600, []byte(hiveID)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddBytes(filepath.Join(spokebackup.SpokePrefix, "hive.yaml.runtime"), 0o600, []byte("policies: {}\n")); err != nil {
		t.Fatal(err)
	}
	if err := b.AddBytes(filepath.Join(spokebackup.BeadsPrefix, "agentA", "bead-0001.json"), 0o640, []byte(`{"agent":"agentA","bead":1}`)); err != nil {
		t.Fatal(err)
	}

	man := &hubbackup.Manifest{
		FormatVersion: hubbackup.FormatVersion(),
		SpokeIDs:      []string{hiveID},
		SpokeErrors:   map[string]string{},
	}
	sealed, err := b.Finish(key, man, 0)
	if err != nil {
		t.Fatal(err)
	}
	return sealed, key
}

// writeArchiveFile seals sealed to a temp file and sets HIVE_BACKUP_KEY, and
// returns the archive path.
func writeArchiveFile(t *testing.T, sealed, key []byte) string {
	t.Helper()
	t.Setenv(hubbackup.EnvBackupKey, hex.EncodeToString(key))
	path := filepath.Join(t.TempDir(), "spoke-backup.enc")
	if err := os.WriteFile(path, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCmdRestorePlacesFilesAtMappedPaths(t *testing.T) {
	sealed, key := buildSpokeArchive(t, "hive-abc")
	archive := writeArchiveFile(t, sealed, key)
	dest := filepath.Join(t.TempDir(), "data")

	out := captureOutput(t, &os.Stdout, func() {
		cmdRestore([]string{"-file", archive, "-dest", dest}, quietLogger())
	})
	if !strings.Contains(out, "restored 3 file(s)") || !strings.Contains(out, "hive-id: hive-abc") {
		t.Errorf("cmdRestore summary = %q", out)
	}

	assertFileContent(t, filepath.Join(dest, "hive-id"), "hive-abc")
	assertFileContent(t, filepath.Join(dest, "hive.yaml.runtime"), "policies: {}\n")
	assertFileContent(t, filepath.Join(dest, "beads", "agentA", "bead-0001.json"), `{"agent":"agentA","bead":1}`)

	info, err := os.Stat(filepath.Join(dest, "beads", "agentA", "bead-0001.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("bead file mode = %o; want 0640", info.Mode().Perm())
	}

	// The extraction scratch directory must not leak into the destination.
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "hive-id" && e.Name() != "hive.yaml.runtime" && e.Name() != "beads" {
			t.Errorf("unexpected entry in dest: %s", e.Name())
		}
	}
}

func TestCmdRestoreDryRunWritesNothing(t *testing.T) {
	sealed, key := buildSpokeArchive(t, "hive-abc")
	archive := writeArchiveFile(t, sealed, key)
	dest := filepath.Join(t.TempDir(), "data")

	out := captureOutput(t, &os.Stdout, func() {
		cmdRestore([]string{"-file", archive, "-dest", dest, "-dry-run"}, quietLogger())
	})
	if !strings.Contains(out, "dry run") || !strings.Contains(out, "hive-id -> ") {
		t.Errorf("cmdRestore -dry-run output = %q", out)
	}

	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dry-run wrote to dest: %v", entries)
	}
}

func TestCmdRestoreRefusesDifferentHiveIDWithoutForce(t *testing.T) {
	sealed, key := buildSpokeArchive(t, "hive-new")
	archive := writeArchiveFile(t, sealed, key)
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "hive-id"), []byte("hive-old"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out := runMainHelper(t,
		map[string]string{hubbackup.EnvBackupKey: hex.EncodeToString(key)},
		"restore", "-file", archive, "-dest", dest)
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
	if !strings.Contains(out, "restore failed") || !strings.Contains(out, "hive-old") || !strings.Contains(out, "hive-new") {
		t.Errorf("mismatched hive-id restore did not report the conflict:\n%s", out)
	}

	restored, err := os.ReadFile(filepath.Join(dest, "hive-id"))
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != "hive-old" {
		t.Errorf("dest hive-id was overwritten to %q; want it left as hive-old", restored)
	}
}

func TestCmdRestoreAllowsDifferentHiveIDWithForce(t *testing.T) {
	sealed, key := buildSpokeArchive(t, "hive-new")
	archive := writeArchiveFile(t, sealed, key)
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "hive-id"), []byte("hive-old"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureOutput(t, &os.Stdout, func() {
		cmdRestore([]string{"-file", archive, "-dest", dest, "-force"}, quietLogger())
	})
	if !strings.Contains(out, "restored") {
		t.Errorf("forced restore output = %q", out)
	}
	assertFileContent(t, filepath.Join(dest, "hive-id"), "hive-new")
}

func TestCmdRestoreAllowsMatchingHiveID(t *testing.T) {
	sealed, key := buildSpokeArchive(t, "hive-same")
	archive := writeArchiveFile(t, sealed, key)
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "hive-id"), []byte("hive-same"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureOutput(t, &os.Stdout, func() {
		cmdRestore([]string{"-file", archive, "-dest", dest}, quietLogger())
	})
	if !strings.Contains(out, "restored") {
		t.Errorf("same-identity restore output = %q", out)
	}
}

func TestMainRestoreRequiresFileAndDest(t *testing.T) {
	for _, args := range [][]string{
		{"restore"},
		{"restore", "-file", "a.enc"},
		{"restore", "-dest", "out"},
	} {
		code, out := runMainHelper(t, nil, args...)
		if code != exitCodeError {
			t.Errorf("%v: exit code = %d; want %d", args, code, exitCodeError)
		}
		if !strings.Contains(out, "restore requires -file and -dest") {
			t.Errorf("%v: missing flag guard not reported:\n%s", args, out)
		}
	}
}

func TestMainRestoreWithoutKeyExitsNonzero(t *testing.T) {
	code, out := runMainHelper(t, nil, "restore", "-file", "a.enc", "-dest", t.TempDir())
	if code != exitCodeError {
		t.Errorf("exit code = %d; want %d", code, exitCodeError)
	}
	if !strings.Contains(out, "restore failed") {
		t.Errorf("missing-key restore did not report failure:\n%s", out)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s content = %q; want %q", path, got, want)
	}
}
