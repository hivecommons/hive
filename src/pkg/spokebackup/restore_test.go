package spokebackup

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hubbackup"
)

// The restore path is the half of #6529 that did not exist: decrypting a spoke
// archive already worked (hubbackup.Extract is format-agnostic), but nothing
// mapped the archive's prefixes onto /data and nothing refused to splice one
// hive's identity and GitHub App keys onto another's. These tests hold both:
// the round trip must reproduce the source data dir exactly, and every identity
// case must land on the documented side of the guard.

// sealedBackupOf builds a sealed spoke archive from a seeded data dir and
// returns the archive bytes together with the source directory.
func sealedBackupOf(t *testing.T, hiveID string) (sealed []byte, srcDir string) {
	t.Helper()
	srcDir = seedSpoke(t)
	if hiveID != "" {
		mustWrite(t, filepath.Join(srcDir, hiveIDFile), hiveID)
	} else if err := os.Remove(filepath.Join(srcDir, hiveIDFile)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	t.Setenv(EnvDataDir, srcDir)

	res, err := Build(testKey(), hiveID, testLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return res.Sealed, srcDir
}

// restoreInto runs a real restore into a fresh directory and fails the test on
// error, returning the result.
func restoreInto(t *testing.T, sealed []byte, dest string, opts RestoreOptions) *RestoreResult {
	t.Helper()
	opts.DataDir = dest
	res, err := Restore(testKey(), sealed, opts, testLogger())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	return res
}

// TestRestoreRoundTripReproducesDataDir is the headline claim: back up a spoke,
// restore into an empty directory, and every captured file is byte-identical at
// the path the entrypoint reads it from.
func TestRestoreRoundTripReproducesDataDir(t *testing.T) {
	sealed, srcDir := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()

	res := restoreInto(t, sealed, dest, RestoreOptions{})

	if res.ArchiveHiveID != "hive-abc123" {
		t.Fatalf("ArchiveHiveID = %q, want hive-abc123", res.ArchiveHiveID)
	}
	if res.ExistingHiveID != "" {
		t.Fatalf("ExistingHiveID = %q, want empty for a fresh destination", res.ExistingHiveID)
	}
	if res.Forced {
		t.Fatal("Forced = true on a fresh destination; nothing should have needed overriding")
	}

	// Every root file the archive includes lands at the data-dir root, with the
	// same bytes as the source.
	for _, name := range []string{
		configOverlayFile, configRuntimeFile, hiveIDFile, stateFile,
		"gh-app-key.pem", "gh-app-key-5686.pem",
	} {
		want := mustRead(t, filepath.Join(srcDir, name))
		got := mustRead(t, filepath.Join(dest, name))
		if got != want {
			t.Errorf("%s: restored %q, want %q", name, got, want)
		}
	}

	// The bead ledger lands one level down, per agent.
	for _, agent := range []string{"scanner", "quality", "ci-maintainer", "sec-check", "strategist"} {
		p := filepath.Join(dest, beadsSubdir, agent, "beads.json")
		if got, want := mustRead(t, p), `{"agent":"`+agent+`"}`; got != want {
			t.Errorf("beads/%s: restored %q, want %q", agent, got, want)
		}
	}
	if len(res.BeadDirs) != 5 {
		t.Errorf("BeadDirs = %v, want 5 agents", res.BeadDirs)
	}
	if !sort.StringsAreSorted(res.BeadDirs) || !sort.StringsAreSorted(res.Files) {
		t.Errorf("result lists must be sorted for a stable dry-run/restore comparison: %v %v",
			res.BeadDirs, res.Files)
	}
}

// TestRestoreDoesNotResurrectExcludedFiles proves the restore places only what
// the backup captured. The excluded set (live session tokens, bulk, logs) is
// excluded for good reasons; a restore that recreated it would undo them.
func TestRestoreDoesNotResurrectExcludedFiles(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()
	restoreInto(t, sealed, dest, RestoreOptions{})

	for _, name := range []string{
		"dashboard-sessions.json", // live browser sessions: never restored
		"audit.jsonl",
		"hive.yaml", // regenerated from seed + overlay on every boot
		filepath.Join("nous", "snapshots", "1.json"),
		filepath.Join("logs", "hive.log"),
		filepath.Join("home", "agent", "creds"),
	} {
		if _, err := os.Stat(filepath.Join(dest, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists after restore; it is deliberately not captured", name)
		}
	}
}

// TestRestoreLeavesNoStagingDirectory proves the temporary tree the archive is
// decrypted into — which holds plaintext GitHub App private keys — is removed,
// including on the failure paths.
func TestRestoreLeavesNoStagingDirectory(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")

	t.Run("success", func(t *testing.T) {
		dest := t.TempDir()
		restoreInto(t, sealed, dest, RestoreOptions{})
		assertNoStaging(t, dest)
	})

	t.Run("refused", func(t *testing.T) {
		dest := t.TempDir()
		mustWrite(t, filepath.Join(dest, hiveIDFile), "hive-someone-else")
		if _, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest}, testLogger()); err == nil {
			t.Fatal("expected a refusal")
		}
		assertNoStaging(t, dest)
	})

	t.Run("bad key", func(t *testing.T) {
		dest := t.TempDir()
		wrong := make([]byte, 32)
		if _, err := Restore(wrong, sealed, RestoreOptions{DataDir: dest}, testLogger()); err == nil {
			t.Fatal("expected a decrypt failure")
		}
		assertNoStaging(t, dest)
	})
}

func assertNoStaging(t *testing.T, dest string) {
	t.Helper()
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), restoreStagingPrefix) {
			t.Fatalf("staging directory %q left behind in %s — it holds decrypted App keys", e.Name(), dest)
		}
	}
}

// TestRestoreRefusesDifferentHive is the guard the issue asks for: restoring
// onto a live, differently-identified hive would splice one hive's config and
// credentials onto another's identity.
func TestRestoreRefusesDifferentHive(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()
	mustWrite(t, filepath.Join(dest, hiveIDFile), "hive-someone-else")
	mustWrite(t, filepath.Join(dest, configOverlayFile), "theirs: untouched\n")

	_, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest}, testLogger())
	if err == nil {
		t.Fatal("Restore succeeded onto a different hive; want a refusal")
	}
	for _, want := range []string{"hive-someone-else", "hive-abc123", "-force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}

	// A refusal must be a no-op: the other hive's files are untouched.
	if got := mustRead(t, filepath.Join(dest, hiveIDFile)); got != "hive-someone-else" {
		t.Errorf("hive-id = %q after a refused restore, want it untouched", got)
	}
	if got := mustRead(t, filepath.Join(dest, configOverlayFile)); got != "theirs: untouched\n" {
		t.Errorf("config = %q after a refused restore, want it untouched", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "gh-app-key.pem")); !os.IsNotExist(err) {
		t.Error("a refused restore wrote a GitHub App key")
	}
}

// TestRestoreForceOverridesDifferentHive proves -force is a real escape hatch
// and that it is reported rather than silent.
func TestRestoreForceOverridesDifferentHive(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()
	mustWrite(t, filepath.Join(dest, hiveIDFile), "hive-someone-else")

	res := restoreInto(t, sealed, dest, RestoreOptions{Force: true})
	if !res.Forced {
		t.Error("Forced = false after overriding a mismatch; the override must be visible")
	}
	if res.ExistingHiveID != "hive-someone-else" {
		t.Errorf("ExistingHiveID = %q, want the pre-restore identity", res.ExistingHiveID)
	}
	if got := mustRead(t, filepath.Join(dest, hiveIDFile)); got != "hive-abc123" {
		t.Errorf("hive-id = %q after a forced restore, want the archive's", got)
	}
}

// TestRestoreOntoSameHiveIsAllowed covers the ordinary recovery: an owner
// restoring their OWN hive's backup over their own hive. No -force needed.
func TestRestoreOntoSameHiveIsAllowed(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()
	// A trailing newline is how a hive-id lands on disk in practice; it must
	// not read as a different identity.
	mustWrite(t, filepath.Join(dest, hiveIDFile), "hive-abc123\n")

	res := restoreInto(t, sealed, dest, RestoreOptions{})
	if res.Forced {
		t.Error("Forced = true restoring a hive over itself; no override should be needed")
	}
}

// TestRestoreRefusesUnidentifiableArchive covers the "cannot tell" case: the
// archive carries no hive-id, so nothing proves it belongs to the identified
// destination. Refused rather than allowed, because the failure mode is a
// credential mix-up.
func TestRestoreRefusesUnidentifiableArchive(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "")
	dest := t.TempDir()
	mustWrite(t, filepath.Join(dest, hiveIDFile), "hive-live")

	_, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest}, testLogger())
	if err == nil {
		t.Fatal("Restore succeeded from an archive with no hive-id onto an identified hive")
	}
	if !strings.Contains(err.Error(), hiveIDFile) {
		t.Errorf("refusal %q does not explain the missing %s", err, hiveIDFile)
	}

	// -force still gets through, and a fresh destination never needed the guard.
	if _, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest, Force: true}, testLogger()); err != nil {
		t.Fatalf("forced restore failed: %v", err)
	}
	fresh := t.TempDir()
	if _, err := Restore(testKey(), sealed, RestoreOptions{DataDir: fresh}, testLogger()); err != nil {
		t.Fatalf("restore onto a fresh destination failed: %v", err)
	}
}

// TestRestoreDryRunWritesNothing proves a dry run answers the "is my target
// safe?" question without being the thing it is checking.
func TestRestoreDryRunWritesNothing(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()

	res := restoreInto(t, sealed, dest, RestoreOptions{DryRun: true})
	if !res.DryRun {
		t.Error("DryRun = false on a dry run; a plan must not read as an outcome")
	}
	if len(res.Files) == 0 {
		t.Fatal("dry run planned no files")
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dry run wrote %d entries into the destination: %v", len(entries), entries)
	}

	// The plan must match what the real restore then does.
	real := restoreInto(t, sealed, dest, RestoreOptions{})
	if strings.Join(real.Files, "\n") != strings.Join(res.Files, "\n") {
		t.Errorf("restore wrote %v but the dry run planned %v", real.Files, res.Files)
	}
}

// TestRestoreDryRunStillRefusesMismatch proves the identity guard runs on a dry
// run too — the dry run's whole job is to tell you the restore would be refused
// before you attempt it for real.
func TestRestoreDryRunStillRefusesMismatch(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()
	mustWrite(t, filepath.Join(dest, hiveIDFile), "hive-someone-else")

	if _, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest, DryRun: true}, testLogger()); err == nil {
		t.Fatal("dry run reported success for a restore that would be refused")
	}
}

// TestRestoreHardensSecretModes proves an archive cannot dictate a loose mode
// for a GitHub App private key. hubbackup.Extract masks group/other WRITE but
// not group/other READ, so a hand-built archive claiming 0644 would otherwise
// restore a world-readable credential.
func TestRestoreHardensSecretModes(t *testing.T) {
	b := hubbackup.NewBuilder()
	for _, f := range []struct {
		name string
		mode int64
	}{
		{hiveIDFile, 0o644},
		{"gh-app-key.pem", 0o644},
		{configOverlayFile, 0o644},
	} {
		if err := b.AddBytes(filepath.Join(spokePrefix, f.name), f.mode, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	sealed, err := b.Finish(testKey(), &hubbackup.Manifest{FormatVersion: hubbackup.FormatVersion()}, 0)
	if err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	restoreInto(t, sealed, dest, RestoreOptions{})
	for _, name := range []string{hiveIDFile, "gh-app-key.pem", configOverlayFile} {
		info, err := os.Stat(filepath.Join(dest, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != restoreSecretMode {
			t.Errorf("%s restored with mode %o, want %o — an archive does not get to widen a credential",
				name, got, restoreSecretMode)
		}
	}
}

func TestRestoreRejectsUnreadableHiveID(t *testing.T) {
	t.Run("destination", func(t *testing.T) {
		sealed, _ := sealedBackupOf(t, "hive-abc123")
		dest := t.TempDir()
		if err := os.Mkdir(filepath.Join(dest, hiveIDFile), restoreDirMode); err != nil {
			t.Fatal(err)
		}

		_, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest}, testLogger())
		if err == nil || !strings.Contains(err.Error(), "read destination "+hiveIDFile) {
			t.Fatalf("expected an error reading the destination identity, got %v", err)
		}
	})

	t.Run("archive", func(t *testing.T) {
		dest := t.TempDir()
		probe := filepath.Join(dest, "unreadable")
		if err := os.WriteFile(probe, nil, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := os.ReadFile(probe); err == nil {
			t.Skip("filesystem allows reading a file without read permissions")
		} else if !errors.Is(err, os.ErrPermission) {
			t.Fatal(err)
		}

		builder := hubbackup.NewBuilder()
		if err := builder.AddBytes(filepath.Join(spokePrefix, hiveIDFile), 0, []byte("hive-abc123")); err != nil {
			t.Fatal(err)
		}
		sealed, err := builder.Finish(testKey(), &hubbackup.Manifest{FormatVersion: hubbackup.FormatVersion()}, 0)
		if err != nil {
			t.Fatal(err)
		}

		_, err = Restore(testKey(), sealed, RestoreOptions{DataDir: dest}, testLogger())
		if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "read archive "+hiveIDFile) {
			t.Fatalf("expected an error reading the staged identity, got %v", err)
		}
	})
}

// TestRestoreRejectsHubArchive proves a hub disaster-recovery archive pointed at
// this command fails loudly instead of "succeeding" with nothing restored.
func TestRestoreRejectsHubArchive(t *testing.T) {
	b := hubbackup.NewBuilder()
	if err := b.AddBytes(filepath.Join("hub", "saas", "users.json"), 0o600, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := b.AddBytes(filepath.Join("secrets", "hive-secrets.json"), 0o600, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	sealed, err := b.Finish(testKey(), &hubbackup.Manifest{FormatVersion: hubbackup.FormatVersion()}, 0)
	if err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	_, err = Restore(testKey(), sealed, RestoreOptions{DataDir: dest}, testLogger())
	if err == nil {
		t.Fatal("Restore accepted a hub archive; want a refusal naming extract")
	}
	if !strings.Contains(err.Error(), "extract") {
		t.Errorf("refusal %q does not point at the right command", err)
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 0 {
		t.Errorf("a rejected hub archive left %d entries behind", len(entries))
	}
}

// TestRestoreReportsIgnoredMembers proves the manifest — and anything else the
// restore path does not place — is reported rather than silently dropped.
func TestRestoreReportsIgnoredMembers(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()
	res := restoreInto(t, sealed, dest, RestoreOptions{})

	found := false
	for _, name := range res.Ignored {
		if strings.HasSuffix(name, "MANIFEST.json") {
			found = true
		}
	}
	if !found {
		t.Errorf("Ignored = %v, want the manifest listed as not-restored", res.Ignored)
	}
	if _, err := os.Stat(filepath.Join(dest, "MANIFEST.json")); !os.IsNotExist(err) {
		t.Error("MANIFEST.json was restored into the data dir; it is archive metadata, not spoke state")
	}
}

// TestRestoreMergesIntoExistingBeads proves a restore adds the archive's ledger
// alongside whatever the destination already had for other agents, rather than
// requiring an empty beads tree.
func TestRestoreMergesIntoExistingBeads(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()
	mustWrite(t, filepath.Join(dest, beadsSubdir, "local-only", "beads.json"), `{"agent":"local-only"}`)
	mustWrite(t, filepath.Join(dest, beadsSubdir, "scanner", "beads.json"), `{"stale":true}`)

	restoreInto(t, sealed, dest, RestoreOptions{})

	if got := mustRead(t, filepath.Join(dest, beadsSubdir, "local-only", "beads.json")); got != `{"agent":"local-only"}` {
		t.Errorf("an agent absent from the archive lost its ledger: %q", got)
	}
	if got := mustRead(t, filepath.Join(dest, beadsSubdir, "scanner", "beads.json")); got != `{"agent":"scanner"}` {
		t.Errorf("scanner ledger = %q, want the archive's copy to win", got)
	}
}

// TestRestoreValidatesInputs covers the guards that fire before anything is
// read or written.
func TestRestoreValidatesInputs(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")

	if _, err := Restore(nil, sealed, RestoreOptions{DataDir: t.TempDir()}, testLogger()); err == nil {
		t.Error("Restore accepted an empty key")
	}
	if _, err := Restore(testKey(), sealed, RestoreOptions{DataDir: "  "}, testLogger()); err == nil {
		t.Error("Restore accepted a blank destination")
	}
}

// TestRestoreCreatesMissingDestination proves the primary case — a brand-new
// volume with no /data yet — needs no manual mkdir.
func TestRestoreCreatesMissingDestination(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := filepath.Join(t.TempDir(), "nested", "data")

	restoreInto(t, sealed, dest, RestoreOptions{})
	if got := mustRead(t, filepath.Join(dest, hiveIDFile)); got != "hive-abc123" {
		t.Errorf("hive-id = %q after restoring into a fresh path", got)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

// TestRestoreAcceptsNilLogger keeps Restore usable from small CLI paths that do
// not have a logger handy; it should fall back to slog.Default rather than
// failing or panicking after decrypting a valid archive.
func TestRestoreAcceptsNilLogger(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()

	res, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest}, nil)
	if err != nil {
		t.Fatalf("Restore with nil logger: %v", err)
	}
	if got := mustRead(t, filepath.Join(dest, hiveIDFile)); got != "hive-abc123" {
		t.Fatalf("hive-id = %q after restore with nil logger", got)
	}
	if res.Manifest == nil {
		t.Fatal("restore with nil logger lost the verified manifest")
	}
}

// TestRestoreRejectsFileAsDestination proves the destination must be a
// directory. Treating a regular file as /data would otherwise hide a setup
// error behind later extraction failures.
func TestRestoreRejectsFileAsDestination(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := filepath.Join(t.TempDir(), "data-file")
	mustWrite(t, dest, "not a directory")

	_, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest}, testLogger())
	if err == nil {
		t.Fatal("Restore accepted a regular file as the destination data dir")
	}
	if !strings.Contains(err.Error(), "prepare destination") {
		t.Fatalf("destination preparation error should be explicit, got %v", err)
	}
	if got := mustRead(t, dest); got != "not a directory" {
		t.Fatalf("destination file was modified: %q", got)
	}
}

// TestRestoreReportsStagingCreationFailure covers a real host-side migration
// failure mode: the destination exists but is not writable, so Restore cannot
// safely create its same-filesystem staging directory for decrypted secrets.
func TestRestoreReportsStagingCreationFailure(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()
	if err := os.Chmod(dest, 0o500); err != nil {
		t.Skipf("cannot make destination unwritable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dest, 0o700) })

	_, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest}, testLogger())
	if err == nil {
		t.Skip("destination remained writable despite chmod; cannot exercise staging failure")
	}
	if !strings.Contains(err.Error(), "create staging directory") {
		t.Fatalf("staging creation error should be explicit, got %v", err)
	}
}

// TestRestoreFailsWhenBeadParentIsAFile proves restore does not bulldoze an
// existing non-directory path to make room for an agent ledger; it reports the
// conflict instead of silently dropping or flattening beads.
func TestRestoreFailsWhenBeadParentIsAFile(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()
	blocked := filepath.Join(dest, beadsSubdir, "scanner")
	mustWrite(t, blocked, "not a directory")
	earlier := filepath.Join(dest, beadsSubdir, "quality", "beads.json")
	mustWrite(t, earlier, "existing bead ledger")

	for _, dryRun := range []bool{true, false} {
		_, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest, DryRun: dryRun}, testLogger())
		if err == nil || !strings.Contains(err.Error(), filepath.Join(beadsSubdir, "scanner")+": path is not a directory") {
			t.Fatalf("dry run=%t: bead parent conflict should name %s, got %v", dryRun, blocked, err)
		}
		if got := mustRead(t, blocked); got != "not a directory" {
			t.Fatalf("dry run=%t: restore modified the conflicting bead path: %q", dryRun, got)
		}
		if got := mustRead(t, earlier); got != "existing bead ledger" {
			t.Fatalf("dry run=%t: restore modified an earlier bead file: %q", dryRun, got)
		}
	}
}

func TestRestoreRejectsUnusableBeadParentSymlinks(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	for _, tc := range []struct {
		target, want string
		file         bool
	}{
		{target: "missing", want: "check parent directory"},
		{target: "regular-file", want: "path is not a directory", file: true},
	} {
		t.Run(tc.target, func(t *testing.T) {
			dest := t.TempDir()
			earlier := filepath.Join(dest, beadsSubdir, "quality", "beads.json")
			mustWrite(t, earlier, "existing ledger")
			if tc.file {
				mustWrite(t, filepath.Join(dest, beadsSubdir, tc.target), "existing file")
			}
			link := filepath.Join(dest, beadsSubdir, "scanner")
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatal(err)
			}

			for _, dryRun := range []bool{true, false} {
				_, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest, DryRun: dryRun}, testLogger())
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("dry run=%t: expected a parent symlink error, got %v", dryRun, err)
				}
				if got := mustRead(t, earlier); got != "existing ledger" {
					t.Fatalf("dry run=%t: restore replaced an earlier bead file: %q", dryRun, got)
				}
			}
		})
	}
}

// TestRestoreReportsFileDirectoryConflict proves a destination directory at a
// file path is not replaced by archive content. That conflict must fail loudly
// because replacing a directory with a config file would destroy unrelated
// operator state.
func TestRestoreReportsFileDirectoryConflict(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, stateFile), restoreDirMode); err != nil {
		t.Fatal(err)
	}

	earlier := filepath.Join(dest, beadsSubdir, "scanner", "beads.json")
	mustWrite(t, earlier, "existing bead ledger")

	for _, mode := range []struct {
		name   string
		dryRun bool
	}{
		{name: "dry run", dryRun: true},
		{name: "apply"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			_, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest, DryRun: mode.dryRun}, testLogger())
			if err == nil || !strings.Contains(err.Error(), "restore "+stateFile) {
				t.Fatalf("file/directory conflict should name the restore target, got %v", err)
			}
			if info, err := os.Stat(filepath.Join(dest, stateFile)); err != nil || !info.IsDir() {
				t.Fatalf("destination directory was not preserved; info=%v err=%v", info, err)
			}
			if got := mustRead(t, earlier); got != "existing bead ledger" {
				t.Fatalf("restore modified an earlier bead file: %q", got)
			}
		})
	}
}

func TestRestoreRejectsReplacingDirectorySymlinkUsedByAnotherPath(t *testing.T) {
	for _, tc := range []struct {
		name, leaf, spelling string
	}{
		{name: "exact path", leaf: configRuntimeFile, spelling: configRuntimeFile},
		{name: "case alias", leaf: configRuntimeFile, spelling: strings.ToUpper(configRuntimeFile)},
		{name: "normalized leaf", leaf: filepath.Join(beadsSubdir, "quality", "Caf\u00e9"), spelling: filepath.Join(beadsSubdir, "quality", "Cafe\u0301")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := seedSpoke(t)
			fact := filepath.Join(vaultsSubdir, beadSynthPrefix, "stable-fact.md")
			mustWrite(t, filepath.Join(src, fact), "archived fact")
			if tc.leaf != configRuntimeFile {
				mustWrite(t, filepath.Join(src, tc.leaf), "archived custom bead")
			}
			t.Setenv(EnvDataDir, src)
			archive, err := Build(testKey(), "hive-abc123", testLogger())
			if err != nil {
				t.Fatal(err)
			}

			dest := t.TempDir()
			target := t.TempDir()
			mustWrite(t, filepath.Join(target, beadSynthPrefix, "stable-fact.md"), "existing fact")
			leaf := filepath.Join(dest, tc.leaf)
			if err := os.MkdirAll(filepath.Dir(leaf), restoreDirMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, leaf); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(dest, tc.spelling)); err != nil {
				if tc.spelling != tc.leaf && os.IsNotExist(err) {
					t.Skip("destination filesystem does not alias these spellings")
				}
				t.Fatal(err)
			}
			links := filepath.Join(dest, "links")
			if err := os.Mkdir(links, restoreDirMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("..", tc.spelling), filepath.Join(links, "vault")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("links", "vault"), filepath.Join(dest, vaultsSubdir)); err != nil {
				t.Fatal(err)
			}
			earlier := filepath.Join(dest, beadsSubdir, "ci-maintainer", "beads.json")
			mustWrite(t, earlier, "existing bead ledger")

			for _, dryRun := range []bool{true, false} {
				_, err := Restore(testKey(), archive.Sealed, RestoreOptions{DataDir: dest, DryRun: dryRun}, testLogger())
				if err == nil || !strings.Contains(err.Error(), "archive also restores a file there") ||
					(!strings.Contains(err.Error(), tc.leaf) && !strings.Contains(err.Error(), tc.spelling)) {
					t.Fatalf("dry run=%t: expected a directory conflict at %s, got %v", dryRun, tc.leaf, err)
				}
				if got := mustRead(t, earlier); got != "existing bead ledger" {
					t.Fatalf("dry run=%t: restore modified an earlier bead file: %q", dryRun, got)
				}
				if got := mustRead(t, filepath.Join(dest, fact)); got != "existing fact" {
					t.Fatalf("dry run=%t: restore modified the linked vault: %q", dryRun, got)
				}
			}
		})
	}
}

func TestRestoreRejectsReplacingDestinationDirectorySymlink(t *testing.T) {
	sealed, _ := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()
	link := filepath.Join(dest, configRuntimeFile)
	if err := os.Symlink(".", link); err != nil {
		t.Fatal(err)
	}
	earlier := filepath.Join(dest, beadsSubdir, "quality", "beads.json")
	mustWrite(t, earlier, "existing ledger")

	for _, dryRun := range []bool{true, false} {
		_, err := Restore(testKey(), sealed, RestoreOptions{DataDir: link, DryRun: dryRun}, testLogger())
		if err == nil || !strings.Contains(err.Error(), configRuntimeFile+": archive also restores a file there") {
			t.Fatalf("dry run=%t: expected a conflict at the destination symlink, got %v", dryRun, err)
		}
		if target, err := os.Readlink(link); err != nil || target != "." {
			t.Fatalf("dry run=%t: destination symlink changed: target=%q err=%v", dryRun, target, err)
		}
		if got := mustRead(t, earlier); got != "existing ledger" {
			t.Fatalf("dry run=%t: restore replaced an earlier bead file: %q", dryRun, got)
		}
	}
}

func TestRestoreReplacesStandaloneSymlinkToDirectory(t *testing.T) {
	sealed, src := sealedBackupOf(t, "hive-abc123")
	dest := t.TempDir()
	target := t.TempDir()
	marker := filepath.Join(target, "keep")
	mustWrite(t, marker, "existing directory contents")
	runtime := filepath.Join(dest, configRuntimeFile)
	if err := os.Symlink(target, runtime); err != nil {
		t.Fatal(err)
	}

	restoreInto(t, sealed, dest, RestoreOptions{DryRun: true})
	restoreInto(t, sealed, dest, RestoreOptions{})
	if got, want := mustRead(t, runtime), mustRead(t, filepath.Join(src, configRuntimeFile)); got != want {
		t.Fatalf("restored runtime config = %q, want %q", got, want)
	}
	if got := mustRead(t, marker); got != "existing directory contents" {
		t.Fatalf("restore changed the symlink target: %q", got)
	}
}

func TestRestoreChecksAliasOfMissingFileAndDirectory(t *testing.T) {
	for _, tc := range []struct {
		name, beadAgent, vaultAgent, file string
	}{
		{name: "case-only suffix", beadAgent: "scanner", vaultAgent: "scanner", file: "Bead-Synth-Wiki"},
		{name: "normalized parent", beadAgent: "Caf\u00e9", vaultAgent: "Cafe\u0301", file: beadSynthPrefix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := seedSpoke(t)
			bead := filepath.Join(beadsSubdir, tc.beadAgent, tc.file)
			fact := filepath.Join(vaultsSubdir, beadSynthPrefix, "stable-fact.md")
			mustWrite(t, filepath.Join(src, bead), "archived bead")
			mustWrite(t, filepath.Join(src, fact), "archived fact")
			t.Setenv(EnvDataDir, src)
			archive, err := Build(testKey(), "hive-abc123", testLogger())
			if err != nil {
				t.Fatal(err)
			}

			dest := t.TempDir()
			beadParent := filepath.Join(beadsSubdir, tc.beadAgent)
			vaultParent := filepath.Join(beadsSubdir, tc.vaultAgent)
			mustWrite(t, filepath.Join(dest, beadParent, "case-probe"), "probe")
			probe := filepath.Join(dest, vaultParent, "case-probe")
			if tc.beadAgent == tc.vaultAgent {
				probe = filepath.Join(dest, vaultParent, "CASE-PROBE")
			}
			_, err = os.Lstat(probe)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			aliases := err == nil
			if err := os.MkdirAll(filepath.Join(dest, vaultParent), restoreDirMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(vaultParent, filepath.Join(dest, vaultsSubdir)); err != nil {
				t.Fatal(err)
			}
			earlier := filepath.Join(dest, beadsSubdir, "quality", "beads.json")
			mustWrite(t, earlier, "existing quality ledger")

			for _, dryRun := range []bool{true, false} {
				_, err := Restore(testKey(), archive.Sealed, RestoreOptions{DataDir: dest, DryRun: dryRun}, testLogger())
				if aliases {
					if err == nil || !strings.Contains(err.Error(), "archive also restores a file there") {
						t.Fatalf("dry run=%t: expected a path alias conflict, got %v", dryRun, err)
					}
					if got := mustRead(t, earlier); got != "existing quality ledger" {
						t.Fatalf("dry run=%t: restore modified an earlier bead file: %q", dryRun, got)
					}
				} else if err != nil {
					t.Fatalf("dry run=%t: destination rejected distinct paths: %v", dryRun, err)
				}
			}
			if !aliases {
				if got := mustRead(t, filepath.Join(dest, bead)); got != "archived bead" {
					t.Fatalf("restored bead = %q", got)
				}
				if got := mustRead(t, filepath.Join(dest, fact)); got != "archived fact" {
					t.Fatalf("restored fact = %q", got)
				}
			}
		})
	}
}

func TestRestoreChecksArchiveMemberCollisions(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
	}{
		{name: "duplicate name", first: "Notes.md", second: "Notes.md"},
		{name: "case variants", first: "Notes.md", second: "notes.md"},
		{name: "Unicode variants", first: "Caf\u00e9.md", second: "Cafe\u0301.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beadDir := filepath.Join(beadsPrefix, "quality")
			secondContent := "second archived note"
			if tc.first == tc.second {
				secondContent = "first archived note" // Both digests must verify.
			}
			builder := hubbackup.NewBuilder()
			if err := builder.AddBytes(filepath.Join(beadDir, tc.first), 0o600, []byte("first archived note")); err != nil {
				t.Fatal(err)
			}
			if err := builder.AddBytes(filepath.Join(beadDir, tc.second), 0o600, []byte(secondContent)); err != nil {
				t.Fatal(err)
			}
			sealed, err := builder.Finish(testKey(), &hubbackup.Manifest{FormatVersion: hubbackup.FormatVersion()}, 0)
			if err != nil {
				t.Fatal(err)
			}

			dest := t.TempDir()
			first := filepath.Join(dest, beadsSubdir, "quality", tc.first)
			second := filepath.Join(dest, beadsSubdir, "quality", tc.second)
			mustWrite(t, first, "existing note")
			_, err = os.Lstat(second)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			aliases := err == nil

			for _, dryRun := range []bool{true, false} {
				_, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest, DryRun: dryRun}, testLogger())
				if aliases {
					if !errors.Is(err, os.ErrExist) {
						t.Fatalf("dry run=%t: expected a staging collision, got %v", dryRun, err)
					}
					if got := mustRead(t, first); got != "existing note" {
						t.Fatalf("dry run=%t: restore replaced an existing note: %q", dryRun, got)
					}
				} else if err != nil {
					t.Fatalf("dry run=%t: restore rejected distinct archive members: %v", dryRun, err)
				}
			}
			if !aliases {
				if got := mustRead(t, first); got != "first archived note" {
					t.Errorf("first restored note = %q", got)
				}
				if got := mustRead(t, second); got != secondContent {
					t.Errorf("second restored note = %q", got)
				}
			}
		})
	}
}

func TestRestoreChecksCollidingFileDestinations(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
		existing            bool
	}{
		{name: "same missing file", first: "beads.json", second: "beads.json"},
		{name: "same existing file", first: "beads.json", second: "beads.json", existing: true},
		{name: "case variants", first: "Notes.md", second: "notes.md"},
		{name: "Unicode variants", first: "Caf\u00e9.md", second: "Cafe\u0301.md", existing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			earlierRel := filepath.Join(beadsSubdir, "ci-maintainer", "beads.json")
			firstRel := filepath.Join(beadsSubdir, "quality", tc.first)
			secondRel := filepath.Join(beadsSubdir, "scanner", tc.second)
			src := t.TempDir()
			mustWrite(t, filepath.Join(src, earlierRel), "archived ledger")
			mustWrite(t, filepath.Join(src, firstRel), "first archived note")
			mustWrite(t, filepath.Join(src, secondRel), "second archived note")
			t.Setenv(EnvDataDir, src)
			archive, err := Build(testKey(), "hive-abc123", testLogger())
			if err != nil {
				t.Fatal(err)
			}

			dest := t.TempDir()
			earlier := filepath.Join(dest, earlierRel)
			first := filepath.Join(dest, firstRel)
			second := filepath.Join(dest, secondRel)
			mustWrite(t, earlier, "existing ledger")
			mustWrite(t, first, "existing note")
			if err := os.Symlink("quality", filepath.Dir(second)); err != nil {
				t.Fatal(err)
			}
			_, err = os.Lstat(second)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			aliases := err == nil
			if !tc.existing {
				if err := os.Remove(first); err != nil {
					t.Fatal(err)
				}
			}

			for _, dryRun := range []bool{true, false} {
				_, err := Restore(testKey(), archive.Sealed, RestoreOptions{DataDir: dest, DryRun: dryRun}, testLogger())
				if !aliases {
					if err != nil {
						t.Fatalf("dry run=%t: restore rejected distinct destinations: %v", dryRun, err)
					}
					continue
				}
				if err == nil || !strings.Contains(err.Error(), "same destination file") ||
					!strings.Contains(err.Error(), firstRel) || !strings.Contains(err.Error(), secondRel) {
					t.Fatalf("dry run=%t: expected a conflict naming both moves, got %v", dryRun, err)
				}
				if got := mustRead(t, earlier); got != "existing ledger" {
					t.Fatalf("dry run=%t: restore replaced an earlier bead file: %q", dryRun, got)
				}
				if tc.existing {
					if got := mustRead(t, first); got != "existing note" {
						t.Fatalf("dry run=%t: restore replaced the conflicting file: %q", dryRun, got)
					}
				} else if _, err := os.Lstat(first); !os.IsNotExist(err) {
					t.Fatalf("dry run=%t: conflicting file should remain absent, got %v", dryRun, err)
				}
			}
			if !aliases {
				if got := mustRead(t, first); got != "first archived note" {
					t.Errorf("first restored note = %q", got)
				}
				if got := mustRead(t, second); got != "second archived note" {
					t.Errorf("second restored note = %q", got)
				}
			}
		})
	}
}

func TestRestoreReportsUnwritablePathProbe(t *testing.T) {
	earlierRel := filepath.Join(beadsSubdir, "ci-maintainer", "beads.json")
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, earlierRel), "archived ledger")
	mustWrite(t, filepath.Join(src, beadsSubdir, "quality", "Notes.md"), "archived note")
	mustWrite(t, filepath.Join(src, beadsSubdir, "scanner", "notes.md"), "other archived note")
	t.Setenv(EnvDataDir, src)
	archive, err := Build(testKey(), "hive-abc123", testLogger())
	if err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	earlier := filepath.Join(dest, earlierRel)
	mustWrite(t, earlier, "existing ledger")
	beads := filepath.Join(dest, beadsSubdir)
	shared := filepath.Join(beads, "quality")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("quality", filepath.Join(beads, "scanner")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o500); err != nil {
		t.Skipf("cannot make probe parent unwritable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(shared, 0o700) })
	probe, err := os.MkdirTemp(shared, "write-check-")
	if err == nil {
		if err := os.Remove(probe); err != nil {
			t.Fatal(err)
		}
		t.Skip("probe parent remained writable despite chmod")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}

	for _, dryRun := range []bool{true, false} {
		_, err := Restore(testKey(), archive.Sealed, RestoreOptions{DataDir: dest, DryRun: dryRun}, testLogger())
		if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "create path probe") {
			t.Fatalf("dry run=%t: expected a probe creation error, got %v", dryRun, err)
		}
		if got := mustRead(t, earlier); got != "existing ledger" {
			t.Fatalf("dry run=%t: restore replaced an earlier bead file: %q", dryRun, got)
		}
	}
}

func TestRestorePathAliasProbeCleansUp(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join("nested", "Notes.md")
	match, err := probeRestorePathAlias(parent, path, path)
	if err != nil || !match {
		t.Fatalf("expected a path to alias itself, got %t, %v", match, err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("probe left entries behind: %v, %v", entries, err)
	}
}

func TestRestoreNameKeyMatchesUnicodeAliases(t *testing.T) {
	for _, names := range [][2]string{
		{"Caf\u00e9", "Cafe\u0301"},
		{"Maße", "MASSE"},
	} {
		if restoreNameKey(names[0]) != restoreNameKey(names[1]) {
			t.Errorf("%q and %q should share a candidate key", names[0], names[1])
		}
	}
}

func TestRestorePlanRejectsFileAtPlannedDirectory(t *testing.T) {
	for _, spelling := range []string{beadsSubdir, strings.ToUpper(beadsSubdir)} {
		t.Run(spelling, func(t *testing.T) {
			dest := t.TempDir()
			staging, err := os.MkdirTemp(dest, restoreStagingPrefix)
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(staging, spokePrefix, spelling), "file")
			mustWrite(t, filepath.Join(staging, beadsPrefix, "scanner", "beads.json"), "contents")
			plan, err := planRestore(staging, dest)
			if err != nil {
				t.Fatal(err)
			}
			_, err = os.Lstat(filepath.Join(dest, strings.ToUpper(filepath.Base(staging))))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			foldsCase := err == nil
			err = plan.checkDestinations(dest, staging)
			if spelling == beadsSubdir || foldsCase {
				if err == nil || !strings.Contains(err.Error(), "archive also restores a file there") {
					t.Fatalf("expected a planned file/directory conflict, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("case-sensitive destination rejected distinct names: %v", err)
			}
		})
	}
}

// TestPlanRestorePropagatesWalkErrors keeps low-level filesystem failures from
// being mistaken for an empty or hub-shaped archive.
func TestPlanRestorePropagatesWalkErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-staging")
	_, err := planRestore(missing, t.TempDir())
	if err == nil {
		t.Fatal("planRestore accepted a missing staging directory")
	}
	if !strings.Contains(err.Error(), "scan extracted archive") {
		t.Fatalf("walk failure should be wrapped with restore context, got %v", err)
	}
}

// TestPlanRestoreIgnoresNonRegularEntries ensures only files extracted from the
// verified archive can be moved into /data. Symlinks or other odd entries in a
// staging tree are reported as ignored, not followed or restored.
func TestPlanRestoreIgnoresNonRegularEntries(t *testing.T) {
	staging := t.TempDir()
	dataDir := t.TempDir()
	mustWrite(t, filepath.Join(staging, spokePrefix, hiveIDFile), "hive-abc123")
	link := filepath.Join(staging, spokePrefix, "gh-app-key-link.pem")
	if err := os.Symlink("gh-app-key.pem", link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	plan, err := planRestore(staging, dataDir)
	if err != nil {
		t.Fatalf("planRestore: %v", err)
	}
	if len(plan.moves) != 1 || plan.moves[0].rel != hiveIDFile {
		t.Fatalf("only the regular hive-id should be planned, got %+v", plan.moves)
	}
	if len(plan.ignored) != 1 || plan.ignored[0] != filepath.ToSlash(filepath.Join(spokePrefix, "gh-app-key-link.pem")) {
		t.Fatalf("symlink should be reported as ignored, got %v", plan.ignored)
	}
}
