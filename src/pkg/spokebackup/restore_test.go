package spokebackup

import (
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
	mustWrite(t, filepath.Join(dest, beadsSubdir, "scanner"), "not a directory")

	_, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest}, testLogger())
	if err == nil {
		t.Fatal("Restore succeeded even though beads/scanner is a file")
	}
	if !strings.Contains(err.Error(), "create") || !strings.Contains(err.Error(), filepath.Join(beadsSubdir, "scanner")) {
		t.Fatalf("bead parent conflict should name the path creation failure, got %v", err)
	}
	if got := mustRead(t, filepath.Join(dest, beadsSubdir, "scanner")); got != "not a directory" {
		t.Fatalf("restore modified the conflicting bead path: %q", got)
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

	_, err := Restore(testKey(), sealed, RestoreOptions{DataDir: dest}, testLogger())
	if err == nil {
		t.Fatal("Restore replaced a destination directory with hive-state.json")
	}
	if !strings.Contains(err.Error(), "restore "+stateFile) {
		t.Fatalf("file/directory conflict should name the restore target, got %v", err)
	}
	if info, statErr := os.Stat(filepath.Join(dest, stateFile)); statErr != nil || !info.IsDir() {
		t.Fatalf("destination directory was not preserved; info=%v err=%v", info, statErr)
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
