package spokebackup

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hubbackup"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testKey is a deterministic AES-256 key for tests only. It is NOT a default
// and never reaches non-test code — production reads HIVE_BACKUP_KEY.
func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// seedSpoke lays out a realistic spoke data dir, mirroring what was observed
// on a production spoke.
func seedSpoke(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// The three config files deliberately DIFFER, as they do on a live spoke:
	// hive.yaml is regenerated each boot, the overlay wins the boot merge and
	// is written secret-free, and hive.yaml.runtime is the post-merge snapshot
	// that retains env-derived secrets. Distinct content is what proves which
	// file the archive captured.
	mustWrite(t, filepath.Join(dir, "hive.yaml"), "stale: configmap-seed\n")
	mustWrite(t, filepath.Join(dir, "hive.yaml.dashboard"), "live: owner-customised\noverlay: wins-the-merge\n")
	mustWrite(t, filepath.Join(dir, configRuntimeFile), "live: owner-customised\nbak: post-merge-snapshot\n")
	mustWrite(t, filepath.Join(dir, "hive-id"), "hive-abc123")
	mustWrite(t, filepath.Join(dir, "hive-state.json"), `{"agents":[]}`)
	mustWrite(t, filepath.Join(dir, "gh-app-key.pem"), "-----BEGIN PRIVATE KEY-----\nAAA\n")
	mustWrite(t, filepath.Join(dir, "gh-app-key-5686.pem"), "-----BEGIN PRIVATE KEY-----\nBBB\n")

	// Things that must NOT be captured.
	mustWrite(t, filepath.Join(dir, "dashboard-sessions.json"), `{"sess":"secret"}`)
	mustWrite(t, filepath.Join(dir, "audit.jsonl"), "{}\n")
	mustWrite(t, filepath.Join(dir, "nous", "snapshots", "1.json"), "bulk")
	mustWrite(t, filepath.Join(dir, "logs", "hive.log"), "noise")
	mustWrite(t, filepath.Join(dir, "home", "agent", "creds"), "token")

	// The bead ledger: one dir per agent, more than the original five.
	for _, agent := range []string{"scanner", "quality", "ci-maintainer", "sec-check", "strategist"} {
		mustWrite(t, filepath.Join(dir, "beads", agent, "beads.json"), `{"agent":"`+agent+`"}`)
	}
	return dir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func buildTestBackup(t *testing.T) (map[string]string, *hubbackup.Manifest, *Result) {
	t.Helper()
	dir := seedSpoke(t)
	t.Setenv(EnvDataDir, dir)

	res, err := Build(testKey(), "hive-abc123", testLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Extract through hubbackup's shared path — proving the two backups share
	// one archive format and one restore path.
	dest := t.TempDir()
	man, err := hubbackup.Extract(testKey(), res.Sealed, dest)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	files := map[string]string{}
	filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dest, p)
		b, _ := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	return files, man, res
}

// TestBackupCapturesAuthoritativeConfig locks in that the backup takes
// hive.yaml.runtime and NOT hive.yaml. Getting this wrong silently backs up a
// stale ConfigMap seed instead of the owner's real configuration.
func TestBackupCapturesAuthoritativeConfig(t *testing.T) {
	files, _, _ := buildTestBackup(t)

	got, ok := files["spoke/"+configRuntimeFile]
	if !ok {
		t.Fatalf("%s missing from archive", configRuntimeFile)
	}
	if !strings.Contains(got, "owner-customised") {
		t.Errorf("%s content wrong: %q", configRuntimeFile, got)
	}
	if _, ok := files["spoke/hive.yaml"]; ok {
		t.Error("archive must NOT contain the stale hive.yaml")
	}
}

// TestBackupCapturesLegacyRuntimeConfig covers the migration case: a hive that
// has not saved since the hive.yaml.runtime rename carries ONLY the legacy
// hive.yaml.bak. Dropping it from the archive would silently lose that hive's
// configuration — and, since the overlay is written secret-free, the only copy
// of its env-derived secrets.
func TestBackupCapturesLegacyRuntimeConfig(t *testing.T) {
	dir := t.TempDir()
	// Legacy name only, exactly as a not-yet-migrated PVC looks.
	mustWrite(t, filepath.Join(dir, configRuntimeFileLegacy), "live: owner-customised\nlegacy: pre-rename\n")
	mustWrite(t, filepath.Join(dir, "hive-id"), "hive-legacy")
	t.Setenv(EnvDataDir, dir)

	res, err := Build(testKey(), "hive-legacy", testLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	dest := t.TempDir()
	if _, err := hubbackup.Extract(testKey(), res.Sealed, dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	files := map[string]string{}
	filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dest, p)
		b, _ := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})

	got, ok := files["spoke/"+configRuntimeFileLegacy]
	if !ok {
		t.Fatalf("%s missing from archive — a hive that has not saved since the "+
			"rename would lose its config on restore", configRuntimeFileLegacy)
	}
	if !strings.Contains(got, "owner-customised") {
		t.Errorf("%s content wrong: %q", configRuntimeFileLegacy, got)
	}
}

// TestBackupIncludesBeads is the reason this package exists: the fleet-wide
// hub backup excludes the bead ledger (issue #2318) and this one must not.
func TestBackupIncludesBeads(t *testing.T) {
	files, _, res := buildTestBackup(t)

	for _, agent := range []string{"scanner", "quality", "ci-maintainer", "sec-check", "strategist"} {
		key := "beads/" + agent + "/beads.json"
		if _, ok := files[key]; !ok {
			t.Errorf("bead ledger for %s missing from archive", agent)
		}
	}
	if len(res.BeadDirs) != 5 {
		t.Errorf("BeadDirs = %d, want 5 (discovered, not hardcoded)", len(res.BeadDirs))
	}
}

// TestBackupExcludesBulkAndLiveCredentials proves the exclusions are real —
// bulk directories stay out (so the download stays responsive) and live
// browser sessions stay out (they are credentials with no restore value).
func TestBackupExcludesBulkAndLiveCredentials(t *testing.T) {
	files, _, _ := buildTestBackup(t)

	for path := range files {
		for _, banned := range []string{"nous/", "logs/", "home/", "dashboard-sessions", "audit.jsonl"} {
			if strings.Contains(path, banned) {
				t.Errorf("archive must not contain %q (matched %q)", path, banned)
			}
		}
	}
}

// TestBackupCapturesAppKeys — the App keys must be present (they are needed to
// restore a working hive) which is precisely why encryption is mandatory.
func TestBackupCapturesAppKeys(t *testing.T) {
	files, _, _ := buildTestBackup(t)
	for _, name := range []string{"spoke/gh-app-key.pem", "spoke/gh-app-key-5686.pem"} {
		if _, ok := files[name]; !ok {
			t.Errorf("%s missing from archive", name)
		}
	}
}

// TestArchiveIsOpaqueCiphertext proves the sealed bytes leak neither the
// private keys nor the config in plaintext.
func TestArchiveIsOpaqueCiphertext(t *testing.T) {
	dir := seedSpoke(t)
	t.Setenv(EnvDataDir, dir)
	res, err := Build(testKey(), "hive-abc123", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"BEGIN PRIVATE KEY", "owner-customised", "hive-abc123"} {
		if strings.Contains(string(res.Sealed), secret) {
			t.Errorf("sealed archive leaks %q in plaintext", secret)
		}
	}
}

// TestWrongKeyIsRejected — the GCM auth tag must make a wrong key fail rather
// than yielding garbage that a restore might act on.
func TestWrongKeyIsRejected(t *testing.T) {
	dir := seedSpoke(t)
	t.Setenv(EnvDataDir, dir)
	res, err := Build(testKey(), "hive-abc123", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	wrong := make([]byte, 32)
	if _, err := hubbackup.Verify(wrong, res.Sealed); err == nil {
		t.Fatal("Verify accepted a wrong key")
	}
}

// TestTamperedArchiveIsRejected — a flipped bit must fail verification.
func TestTamperedArchiveIsRejected(t *testing.T) {
	dir := seedSpoke(t)
	t.Setenv(EnvDataDir, dir)
	res, err := Build(testKey(), "hive-abc123", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), res.Sealed...)
	tampered[len(tampered)/2] ^= 0xFF
	if _, err := hubbackup.Verify(testKey(), tampered); err == nil {
		t.Fatal("Verify accepted a tampered archive")
	}
}

// TestManifestVerifies — digests must actually cover the files, so Verify is
// not vacuously passing (the exact bug caught during PR #2315).
func TestManifestVerifies(t *testing.T) {
	_, man, res := buildTestBackup(t)
	if len(man.Files) == 0 {
		t.Fatal("manifest has no files: verification would be vacuous")
	}
	if _, err := hubbackup.Verify(testKey(), res.Sealed); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestBuildRefusesWithoutKey — no key must mean no archive, never a plaintext
// one containing GitHub App private keys.
// The assertion is on the MESSAGE, not merely that an error occurred: an empty
// key also fails later inside Seal (AES rejects a zero-length key), so a test
// accepting any error still passes with the explicit guard removed. The
// distinct refusal is what proves Build never even attempts to assemble an
// unencrypted archive carrying GitHub App private keys.
func TestBuildRefusesWithoutKey(t *testing.T) {
	dir := seedSpoke(t)
	t.Setenv(EnvDataDir, dir)
	_, err := Build(nil, "hive-abc123", testLogger())
	if err == nil {
		t.Fatal("Build produced an archive with no key")
	}
	if !strings.Contains(err.Error(), "refusing to build an unencrypted spoke backup") {
		t.Fatalf("a missing key must be refused up front, not incidentally by the "+
			"cipher; got %v", err)
	}
}

// TestMissingFilesAreRecordedNotSilent — a gap must be visible in the result
// rather than discovered during a restore.
func TestMissingFilesAreRecordedNotSilent(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "hive-id"), "hive-xyz")
	t.Setenv(EnvDataDir, dir)

	res, err := Build(testKey(), "hive-xyz", testLogger())
	if err != nil {
		t.Fatalf("Build should tolerate a sparse spoke: %v", err)
	}
	if _, ok := res.Skipped[configRuntimeFile]; !ok {
		t.Errorf("missing %s should be recorded in Skipped", configRuntimeFile)
	}
	if _, ok := res.Skipped[configRuntimeFileLegacy]; !ok {
		t.Errorf("missing %s should be recorded in Skipped", configRuntimeFileLegacy)
	}
	if len(res.Manifest.SpokeErrors) == 0 {
		t.Error("gaps should surface in the manifest")
	}
}

// TestArchiveNameIsFilenameSafe — a hostile hive ID must not escape the
// Content-Disposition header.
func TestArchiveNameIsFilenameSafe(t *testing.T) {
	name := ArchiveName(`../../etc/"passwd`, timeZero())
	if strings.ContainsAny(name, `/"\`) {
		t.Errorf("unsafe filename: %q", name)
	}
	if !strings.HasSuffix(name, ".tar.gz.enc") {
		t.Errorf("expected .enc suffix, got %q", name)
	}
}

// timeZero is a fixed timestamp so filename tests are deterministic.
func timeZero() time.Time { return time.Unix(0, 0).UTC() }
