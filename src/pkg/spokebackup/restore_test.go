package spokebackup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testArchive(t *testing.T) []byte {
	t.Helper()
	dir := seedSpoke(t)
	t.Setenv(EnvDataDir, dir)
	res, err := Build(testKey(), "hive-abc123", testLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return res.Sealed
}

func TestRestoreDryRunDoesNotWriteFiles(t *testing.T) {
	dest := t.TempDir()
	sealed := testArchive(t)

	res, err := Restore(testKey(), sealed, RestoreOptions{DestDir: dest, DryRun: true})
	if err != nil {
		t.Fatalf("Restore dry-run: %v", err)
	}
	if !res.DryRun {
		t.Fatal("result must report dry-run")
	}
	if !slices.Contains(res.FilesPlanned, "hive-id") {
		t.Fatalf("restore plan missing hive-id: %v", res.FilesPlanned)
	}
	if _, err := os.Stat(filepath.Join(dest, "hive-id")); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote hive-id or hit unexpected error: %v", err)
	}
}

func TestRestoreMapsSpokeAndBeadsIntoDataDir(t *testing.T) {
	dest := t.TempDir()
	sealed := testArchive(t)

	res, err := Restore(testKey(), sealed, RestoreOptions{DestDir: dest})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.FilesWritten == 0 {
		t.Fatal("restore wrote no files")
	}
	hiveID, err := os.ReadFile(filepath.Join(dest, "hive-id"))
	if err != nil {
		t.Fatalf("hive-id not restored at data-dir root: %v", err)
	}
	if strings.TrimSpace(string(hiveID)) != "hive-abc123" {
		t.Fatalf("hive-id = %q", hiveID)
	}
	bead, err := os.ReadFile(filepath.Join(dest, "beads", "scanner", "beads.json"))
	if err != nil {
		t.Fatalf("bead ledger not restored under data-dir beads/: %v", err)
	}
	if !strings.Contains(string(bead), `"scanner"`) {
		t.Fatalf("wrong bead content: %q", bead)
	}
	if _, err := os.Stat(filepath.Join(dest, spokePrefix, "hive-id")); !os.IsNotExist(err) {
		t.Fatalf("restore left archive spoke/ prefix in destination: %v", err)
	}
}

func TestRestoreRefusesDifferentExistingHiveIDWithoutForce(t *testing.T) {
	dest := t.TempDir()
	mustWrite(t, filepath.Join(dest, "hive-id"), "other-hive")
	sealed := testArchive(t)

	_, err := Restore(testKey(), sealed, RestoreOptions{DestDir: dest})
	if err == nil {
		t.Fatal("restore silently clobbered a different live hive-id")
	}
	if !strings.Contains(err.Error(), "without -force") {
		t.Fatalf("guard error should tell operator how to override, got %v", err)
	}

	res, err := Restore(testKey(), sealed, RestoreOptions{DestDir: dest, Force: true})
	if err != nil {
		t.Fatalf("forced restore: %v", err)
	}
	if !res.Forced {
		t.Fatal("forced restore result did not record Force")
	}
}

func TestRestoreRefusesExistingSymlinkTargets(t *testing.T) {
	dest := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, "hive-id")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := Restore(testKey(), testArchive(t), RestoreOptions{DestDir: dest, Force: true})
	if err == nil {
		t.Fatal("restore followed an existing symlink in the target data dir")
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Fatalf("restore clobbered symlink target outside destination: %q", got)
	}
}

func TestRestoreDryRunRefusesSymlinkHiveID(t *testing.T) {
	dest := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("not-a-real-hive-id"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, "hive-id")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := Restore(testKey(), testArchive(t), RestoreOptions{DestDir: dest, DryRun: true})
	if err == nil {
		t.Fatal("dry-run followed a symlinked hive-id")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink refusal, got %v", err)
	}
}

func TestRestoreRefusesSymlinkParentWithoutCreatingOutsideDirs(t *testing.T) {
	dest := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dest, "beads")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := Restore(testKey(), testArchive(t), RestoreOptions{DestDir: dest, Force: true})
	if err == nil {
		t.Fatal("restore followed a symlinked parent directory")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "scanner")); !os.IsNotExist(statErr) {
		t.Fatalf("restore created directories through symlinked parent: %v", statErr)
	}
}

func TestRestoreTightensExistingFileMode(t *testing.T) {
	dest := t.TempDir()
	mustWrite(t, filepath.Join(dest, "hive-id"), "hive-abc123")
	keyPath := filepath.Join(dest, "gh-app-key.pem")
	mustWrite(t, keyPath, "old")
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Restore(testKey(), testArchive(t), RestoreOptions{DestDir: dest}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("restored private key mode = %o, want 0600", got)
	}
}

func TestRestoreRequiresArchiveHiveID(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, configOverlayFile), "owner: config\n")
	t.Setenv(EnvDataDir, src)
	res, err := Build(testKey(), "manifest-only-id", testLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	_, err = Restore(testKey(), res.Sealed, RestoreOptions{DestDir: t.TempDir(), DryRun: true})
	if err == nil {
		t.Fatal("restore accepted an archive that cannot restore /data/hive-id")
	}
	if !strings.Contains(err.Error(), "spoke/hive-id") {
		t.Fatalf("missing hive-id error should point to extract/manual recovery, got %v", err)
	}
}
