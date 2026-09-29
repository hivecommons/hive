package policies

import (
	"os"
	"path/filepath"
	"testing"
)

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
