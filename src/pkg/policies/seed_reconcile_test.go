package policies

import (
	"os"
	"path/filepath"
	"testing"
)

// --- loadSeedManifest / saveSeedManifest ---

// TestLoadSeedManifestInvalidJSON: a corrupt manifest must degrade to the
// empty manifest (same as a missing one), never to an error that blocks
// seeding.
func TestLoadSeedManifestInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, seedManifestFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt manifest: %v", err)
	}
	if got := loadSeedManifest(dir); len(got) != 0 {
		t.Fatalf("loadSeedManifest on corrupt JSON = %v, want empty", got)
	}
}

// --- ReconcileSeededDefaults ---

// TestReconcileSeededDefaultsSeedsEmbeddedOnly covers the "embedded only"
// acceptance case (hivecommons/hive#9428): an empty dir gets every embedded
// default written to it, byte-identical to the embedded copy.
func TestReconcileSeededDefaultsSeedsEmbeddedOnly(t *testing.T) {
	dir := t.TempDir()

	if err := ReconcileSeededDefaults(dir, testLogger()); err != nil {
		t.Fatalf("ReconcileSeededDefaults: %v", err)
	}

	embedded, err := DefaultPolicies.ReadFile("defaults/scanner.md")
	if err != nil {
		t.Fatalf("read embedded default: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "scanner.md"))
	if err != nil {
		t.Fatalf("seeded file not written: %v", err)
	}
	if string(got) != string(embedded) {
		t.Fatalf("seeded content = %q, want embedded default %q", got, embedded)
	}

	if _, err := os.Stat(filepath.Join(dir, seedManifestFileName)); err != nil {
		t.Fatalf("seed manifest not written: %v", err)
	}
}

// TestReconcileSeededDefaultsPreservesUserEdit covers the "user edit wins"
// acceptance case: a file whose content diverges from both the current
// embedded default and any hash this function last recorded (i.e. it was
// never seeded by ReconcileSeededDefaults, or was edited since) must survive
// repeated reconciliation runs untouched — a genuine dashboard edit must
// survive an image roll.
func TestReconcileSeededDefaultsPreservesUserEdit(t *testing.T) {
	dir := t.TempDir()
	edited := []byte("# my custom scanner policy\n")
	if err := os.WriteFile(filepath.Join(dir, "scanner.md"), edited, 0o644); err != nil {
		t.Fatalf("seed edited file: %v", err)
	}

	if err := ReconcileSeededDefaults(dir, testLogger()); err != nil {
		t.Fatalf("ReconcileSeededDefaults (first run): %v", err)
	}
	if err := ReconcileSeededDefaults(dir, testLogger()); err != nil {
		t.Fatalf("ReconcileSeededDefaults (second run): %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "scanner.md"))
	if err != nil {
		t.Fatalf("read scanner.md: %v", err)
	}
	if string(got) != string(edited) {
		t.Fatalf("edited content was overwritten: got %q, want %q", got, edited)
	}
}

// TestReconcileSeededDefaultsRefreshesStaleSeed covers the "stale seed
// ignored/refreshed" acceptance case: a file this function itself seeded on
// a prior run, never edited since, must be refreshed when the embedded
// default it was seeded from is no longer current — reproducing the bug
// report's stale byte-identical-to-an-old-image copy shadowing an update.
func TestReconcileSeededDefaultsRefreshesStaleSeed(t *testing.T) {
	dir := t.TempDir()

	oldDefault := []byte("# scanner v1\n")
	if err := os.WriteFile(filepath.Join(dir, "scanner.md"), oldDefault, 0o644); err != nil {
		t.Fatalf("seed old default: %v", err)
	}
	manifest := map[string]string{"scanner.md": sha256Hex(oldDefault)}
	if err := saveSeedManifest(dir, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	if err := ReconcileSeededDefaults(dir, testLogger()); err != nil {
		t.Fatalf("ReconcileSeededDefaults: %v", err)
	}

	embedded, err := DefaultPolicies.ReadFile("defaults/scanner.md")
	if err != nil {
		t.Fatalf("read embedded default: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "scanner.md"))
	if err != nil {
		t.Fatalf("read scanner.md: %v", err)
	}
	if string(got) != string(embedded) {
		t.Fatalf("stale seed was not refreshed: got %q, want embedded default %q", got, embedded)
	}

	newManifest := loadSeedManifest(dir)
	if newManifest["scanner.md"] != sha256Hex(embedded) {
		t.Fatalf("manifest not updated after refresh: %v", newManifest)
	}
}

// TestReconcileSeededDefaultsIsIdempotentWhenCurrent covers a second run
// against already-current files: nothing should change, and the manifest
// should already record the current hash.
func TestReconcileSeededDefaultsIsIdempotentWhenCurrent(t *testing.T) {
	dir := t.TempDir()

	if err := ReconcileSeededDefaults(dir, testLogger()); err != nil {
		t.Fatalf("ReconcileSeededDefaults (first run): %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "scanner.md"))
	if err != nil {
		t.Fatalf("read scanner.md after first run: %v", err)
	}

	if err := ReconcileSeededDefaults(dir, testLogger()); err != nil {
		t.Fatalf("ReconcileSeededDefaults (second run): %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "scanner.md"))
	if err != nil {
		t.Fatalf("read scanner.md after second run: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("content changed on idempotent re-run: before %q after %q", before, after)
	}
}

// TestReconcileSeededDefaultsPreservesEditAfterSeed covers the edited-after-
// seed branch (recorded hash != current file hash): a file this function
// seeded and an operator later edited must never be refreshed, even though
// the manifest still records the old seed hash.
func TestReconcileSeededDefaultsPreservesEditAfterSeed(t *testing.T) {
	dir := t.TempDir()

	if err := ReconcileSeededDefaults(dir, testLogger()); err != nil {
		t.Fatalf("ReconcileSeededDefaults (seed run): %v", err)
	}
	edited := []byte("# operator-tuned scanner policy\n")
	if err := os.WriteFile(filepath.Join(dir, "scanner.md"), edited, 0o644); err != nil {
		t.Fatalf("edit seeded file: %v", err)
	}

	// nil logger also exercises the slog.Default() fallback.
	if err := ReconcileSeededDefaults(dir, nil); err != nil {
		t.Fatalf("ReconcileSeededDefaults (post-edit run): %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "scanner.md"))
	if err != nil {
		t.Fatalf("read scanner.md: %v", err)
	}
	if string(got) != string(edited) {
		t.Fatalf("post-seed edit was overwritten: got %q, want %q", got, edited)
	}
}

// TestReconcileSeededDefaultsHealsMissingManifestEntry covers the manifest
// heal branch: a file already byte-identical to the embedded default but
// absent from the manifest gets its entry recorded without a rewrite.
func TestReconcileSeededDefaultsHealsMissingManifestEntry(t *testing.T) {
	dir := t.TempDir()
	embedded, err := DefaultPolicies.ReadFile("defaults/scanner.md")
	if err != nil {
		t.Fatalf("read embedded default: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scanner.md"), embedded, 0o644); err != nil {
		t.Fatalf("pre-place current file: %v", err)
	}

	if err := ReconcileSeededDefaults(dir, testLogger()); err != nil {
		t.Fatalf("ReconcileSeededDefaults: %v", err)
	}

	manifest := loadSeedManifest(dir)
	if manifest["scanner.md"] != sha256Hex(embedded) {
		t.Fatalf("manifest entry not healed: %v", manifest)
	}
}

// TestReconcileSeededDefaultsMkdirError: dir whose parent path component is
// a regular file cannot be created, and that is the one fatal case.
func TestReconcileSeededDefaultsMkdirError(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	if err := ReconcileSeededDefaults(filepath.Join(blocker, "policies"), testLogger()); err == nil {
		t.Fatal("expected error when dir cannot be created")
	}
}

// TestReconcileSeededDefaultsSkipsUnreadableTarget covers the non-ENOENT
// read-error branch: a directory squatting on a template's path fails
// os.ReadFile (EISDIR) and must be skipped, not treated as missing.
func TestReconcileSeededDefaultsSkipsUnreadableTarget(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "scanner.md"), 0o755); err != nil {
		t.Fatalf("mkdir squatting dir: %v", err)
	}

	if err := ReconcileSeededDefaults(dir, testLogger()); err != nil {
		t.Fatalf("ReconcileSeededDefaults: %v", err)
	}

	if fi, err := os.Stat(filepath.Join(dir, "scanner.md")); err != nil || !fi.IsDir() {
		t.Fatalf("squatting directory was replaced: fi=%v err=%v", fi, err)
	}
	if _, ok := loadSeedManifest(dir)["scanner.md"]; ok {
		t.Fatal("manifest recorded a hash for a template that was never written")
	}
}

// TestReconcileSeededDefaultsSeedWriteFailure covers the seed-write warn
// branch: a dangling symlink reads as ErrNotExist, and the follow-up write
// through it fails because the destination directory does not exist. The
// failure must be skipped without a manifest entry.
func TestReconcileSeededDefaultsSeedWriteFailure(t *testing.T) {
	dir := t.TempDir()
	dangling := filepath.Join(t.TempDir(), "gone", "scanner.md")
	if err := os.Symlink(dangling, filepath.Join(dir, "scanner.md")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	if err := ReconcileSeededDefaults(dir, testLogger()); err != nil {
		t.Fatalf("ReconcileSeededDefaults: %v", err)
	}

	if _, ok := loadSeedManifest(dir)["scanner.md"]; ok {
		t.Fatal("manifest recorded a hash for a template whose write failed")
	}
}

// TestReconcileSeededDefaultsManifestSaveFailure covers the manifest-save
// warn branch: a directory squatting on the manifest path makes both load
// (empty manifest) and save fail, yet reconciliation still seeds every
// template and returns nil.
func TestReconcileSeededDefaultsManifestSaveFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, seedManifestFileName), 0o755); err != nil {
		t.Fatalf("mkdir squatting manifest dir: %v", err)
	}

	if err := ReconcileSeededDefaults(dir, testLogger()); err != nil {
		t.Fatalf("ReconcileSeededDefaults: %v", err)
	}

	if _, err := os.ReadFile(filepath.Join(dir, "scanner.md")); err != nil {
		t.Fatalf("templates were not seeded despite manifest failure: %v", err)
	}
}
