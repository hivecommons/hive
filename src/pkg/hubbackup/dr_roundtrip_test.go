package hubbackup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixedSpokeCollector returns a canned fleet without touching kubectl.
type fixedSpokeCollector struct {
	spokes []SpokeConfig
	err    error
}

func (f fixedSpokeCollector) Collect(_ *slogLogger) ([]SpokeConfig, error) {
	return f.spokes, f.err
}

// fixedSecretCollector returns canned Secrets.
type fixedSecretCollector struct {
	items []SecretItem
	err   error
}

func (f fixedSecretCollector) Collect(_ *slogLogger) ([]SecretItem, error) {
	return f.items, f.err
}

// ---- Full DR round trip ----

// The end-to-end disaster-recovery path: seal -> verify -> open -> extract,
// asserting the restored bytes are identical to what went in. This is the
// scenario that runs after catastrophic loss of the hub.
func TestDRRoundTripSealVerifyOpenExtract(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	key := decodeTestKey(t)

	spokes := fixedSpokeCollector{spokes: []SpokeConfig{
		{ID: "hosted-x", Cluster: "hive-oke", Files: map[string][]byte{
			"hive.yaml.runtime": []byte("project:\n  org: acme\n"),
			"agents.json":       []byte(`{"scanner":{"enabled":true}}`),
		}},
	}}
	secrets := fixedSecretCollector{items: []SecretItem{
		{Name: "hive-hub-secrets", JSON: []byte(`{"kind":"Secret"}`)},
	}}

	sealed, man, err := Build(key, spokes, secrets, quietLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// 1. Sealed output must not contain plaintext.
	if strings.Contains(string(sealed), "hmac.key") ||
		strings.Contains(string(sealed), "project:") {
		t.Fatal("sealed archive leaks plaintext — encryption did not apply")
	}

	// 2. Verify accepts the good archive and returns a populated manifest.
	verified, err := Verify(key, sealed)
	if err != nil {
		t.Fatalf("Verify rejected a good archive: %v", err)
	}
	if len(verified.Files) == 0 {
		t.Fatal("manifest has no files — Verify would be vacuous")
	}

	// 3. Extract restores real content to disk.
	dest := t.TempDir()
	extracted, err := Extract(key, sealed, dest)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if extracted == nil {
		t.Fatal("Extract returned no manifest")
	}

	// The hub's crypto material must survive the round trip byte for byte:
	// a corrupted hmac.key means every issued session token is invalid.
	gotHMAC, err := os.ReadFile(filepath.Join(dest, "hub", "saas", "hmac.key"))
	if err != nil {
		t.Fatalf("hmac.key missing from restore: %v", err)
	}
	if string(gotHMAC) != strings.Repeat("k", 32) {
		t.Fatalf("hmac.key corrupted through the round trip: %q", gotHMAC)
	}

	// The spoke's authoritative config must survive too.
	gotSpoke, err := os.ReadFile(filepath.Join(dest, "spokes", "hosted-x", "hive.yaml.runtime"))
	if err != nil {
		t.Fatalf("spoke config missing from restore: %v", err)
	}
	if string(gotSpoke) != "project:\n  org: acme\n" {
		t.Fatalf("spoke config corrupted: %q", gotSpoke)
	}

	// The Secret that lives outside the PVC must be present, or the restored
	// hub cannot authenticate anyone.
	if _, err := os.ReadFile(filepath.Join(dest, "secrets", "hive-hub-secrets.json")); err != nil {
		t.Fatalf("out-of-PVC Secret missing from restore: %v", err)
	}

	// Excluded scratch dirs must NOT be in the archive.
	for _, excluded := range []string{"nous", "beads", "logs"} {
		if _, err := os.Stat(filepath.Join(dest, "hub", excluded)); err == nil {
			t.Errorf("scratch dir %q must be excluded from the backup", excluded)
		}
	}
	_ = man
}

// ---- Negative: corruption MUST be rejected ----

// This is the test that would have caught the manifest-serialized-before-digests
// bug, where Verify passed while checking nothing. Every mutation below must be
// rejected.
func TestVerifyRejectsCorruptedArchive(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	key := decodeTestKey(t)

	sealed, _, err := Build(key, nil, nil, quietLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	t.Run("flipped ciphertext byte", func(t *testing.T) {
		bad := append([]byte(nil), sealed...)
		bad[len(bad)/2] ^= 0xFF
		if _, err := Verify(key, bad); err == nil {
			t.Fatal("a corrupted archive was accepted as valid")
		}
	})

	t.Run("truncated archive", func(t *testing.T) {
		bad := append([]byte(nil), sealed[:len(sealed)/2]...)
		if _, err := Verify(key, bad); err == nil {
			t.Fatal("a truncated archive was accepted as valid")
		}
	})

	t.Run("flipped nonce", func(t *testing.T) {
		bad := append([]byte(nil), sealed...)
		bad[0] ^= 0x01
		if _, err := Verify(key, bad); err == nil {
			t.Fatal("a tampered nonce was accepted as valid")
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if _, err := Verify(key, nil); err == nil {
			t.Fatal("an empty archive was accepted as valid")
		}
	})
}

// Verify must fail when a file's bytes no longer match its manifest digest.
// Rebuilding the archive with a mutated payload but the ORIGINAL manifest is
// the precise shape of the silent-corruption bug.
func TestVerifyDetectsDigestMismatch(t *testing.T) {
	key := decodeTestKey(t)

	// Build an archive by hand so the manifest can be desynced from content.
	good := buildArchiveFixture(t, key, "hub/saas/hmac.key", []byte("original"), nil)
	if _, err := Verify(key, good); err != nil {
		t.Fatalf("fixture must verify clean first: %v", err)
	}

	// Same manifest digests, different file bytes.
	tampered := buildArchiveFixture(t, key, "hub/saas/hmac.key", []byte("TAMPERED"),
		func(m *Manifest) {
			// Keep the digest of the ORIGINAL content.
			for i := range m.Files {
				if m.Files[i].Path == "hub/saas/hmac.key" {
					m.Files[i].SHA256 = sha256Hex([]byte("original"))
				}
			}
		})
	if _, err := Verify(key, tampered); err == nil {
		t.Fatal("Verify accepted a file whose bytes do not match its manifest digest")
	}
}

// A manifest listing a file the archive does not contain must be rejected: a
// restore would silently be missing it.
func TestVerifyRejectsManifestListingMissingFile(t *testing.T) {
	key := decodeTestKey(t)
	bad := buildArchiveFixture(t, key, "hub/saas/hmac.key", []byte("x"),
		func(m *Manifest) {
			m.Files = append(m.Files, FileEntry{
				Path: "hub/saas/absent.key", Size: 4, SHA256: sha256Hex([]byte("gone")),
			})
		})
	_, err := Verify(key, bad)
	if err == nil {
		t.Fatal("Verify accepted a manifest listing a file not present in the archive")
	}
	if !strings.Contains(err.Error(), "absent.key") {
		t.Errorf("error should name the missing file, got %v", err)
	}
	// "absent from the archive" and "present but corrupted" are different
	// failures and must be reported differently: an operator restoring after a
	// disaster needs to know which one they are looking at. Asserting only that
	// the filename appears is satisfied by the checksum-mismatch message too.
	if !strings.Contains(err.Error(), "does not contain it") {
		t.Errorf("a missing file must be reported as missing, not as a checksum "+
			"mismatch; got %v", err)
	}
}

// An archive with no manifest at all must be rejected rather than treated as
// trivially valid.
func TestVerifyRejectsArchiveWithoutManifest(t *testing.T) {
	key := decodeTestKey(t)
	bad := buildArchiveFixtureNoManifest(t, key)
	if _, err := Verify(key, bad); err == nil {
		t.Fatal("an archive with no MANIFEST.json was accepted")
	}
}

// A future format version must be refused rather than half-restored.
func TestVerifyRejectsUnknownFormatVersion(t *testing.T) {
	key := decodeTestKey(t)
	bad := buildArchiveFixture(t, key, "hub/x", []byte("x"), func(m *Manifest) {
		m.FormatVersion = backupFormatVersion + 99
	})
	_, err := Verify(key, bad)
	if err == nil {
		t.Fatal("an archive from a newer format version was accepted")
	}
	if !strings.Contains(err.Error(), "format version") {
		t.Errorf("error should mention the format version, got %v", err)
	}
}

// ---- Key handling ----

// A wrong key must fail cleanly with a decrypt error, never return garbage.
func TestVerifyWithWrongKeyFailsCleanly(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	sealed, _, err := Build(decodeTestKey(t), nil, nil, quietLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	wrong := make([]byte, aesKeySize)
	for i := range wrong {
		wrong[i] = 0xAB
	}
	man, err := Verify(wrong, sealed)
	if err == nil {
		t.Fatal("a wrong key must not verify the archive")
	}
	if man != nil {
		t.Fatal("a failed verification must not return a manifest")
	}
	if !strings.Contains(err.Error(), EnvBackupKey) {
		t.Errorf("error should point the operator at %s, got %v", EnvBackupKey, err)
	}
}

// Extract must refuse a corrupted archive rather than writing partial files to
// disk — a half-restore is worse than a clean failure.
func TestExtractRefusesCorruptedArchive(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	key := decodeTestKey(t)
	sealed, _, err := Build(key, nil, nil, quietLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	bad := append([]byte(nil), sealed...)
	bad[len(bad)/2] ^= 0xFF

	dest := t.TempDir()
	if _, err := Extract(key, bad, dest); err == nil {
		t.Fatal("Extract accepted a corrupted archive")
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 0 {
		t.Fatalf("Extract wrote %d entries from a corrupted archive; "+
			"verification must happen before any file is written", len(entries))
	}
}

// Extract must run the FULL manifest verification before writing, not merely
// rely on the GCM tag. A digest-mismatched archive is cryptographically valid
// (Open succeeds) yet its content does not match the manifest, so only the
// explicit Verify call rejects it. Without that call this restores silently
// corrupted hub state.
func TestExtractVerifiesDigestsNotJustGCMTag(t *testing.T) {
	key := decodeTestKey(t)
	bad := buildArchiveFixture(t, key, "hub/saas/hmac.key", []byte("TAMPERED"),
		func(m *Manifest) {
			m.Files[0].SHA256 = sha256Hex([]byte("original"))
		})
	// Precondition: the archive decrypts cleanly, so GCM alone cannot catch it.
	if _, err := Open(key, bad); err != nil {
		t.Fatalf("fixture must be GCM-valid for this test to mean anything: %v", err)
	}

	dest := t.TempDir()
	if _, err := Extract(key, bad, dest); err == nil {
		t.Fatal("Extract accepted an archive whose content does not match its manifest")
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Fatalf("Extract wrote %d entries from an unverified archive", len(entries))
	}
}

// ---- Build: spoke degradation must not shrink the fleet ----

// A spoke reported as failed must appear in SpokeErrors, and the healthy ones
// must still be captured. Silently dropping a failed spoke is how a 50/50
// backup became 49/50.
func TestBuildRecordsSpokeErrorsWithoutLosingHealthySpokes(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	key := decodeTestKey(t)

	spokes := fixedSpokeCollector{spokes: []SpokeConfig{
		{ID: "healthy-1", Files: map[string][]byte{"hive.yaml.runtime": []byte("a")}},
		{ID: "evicted-1", Err: "no running pod in hive-hosted-evicted-1"},
		{ID: "healthy-2", Files: map[string][]byte{"hive.yaml.runtime": []byte("b")}},
	}}

	_, man, err := Build(key, spokes, nil, quietLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(man.SpokeIDs) != 2 {
		t.Fatalf("want 2 captured spokes, got %v", man.SpokeIDs)
	}
	if _, ok := man.SpokeErrors["evicted-1"]; !ok {
		t.Fatal("the failed spoke must be recorded in SpokeErrors so the operator " +
			"learns about the gap from the archive itself")
	}
	// Total accounted-for hives must equal the input fleet size.
	if len(man.SpokeIDs)+len(man.SpokeErrors) != 3 {
		t.Fatalf("a hive was lost: %d captured + %d errors != 3",
			len(man.SpokeIDs), len(man.SpokeErrors))
	}
}

// A collector-level failure must be recorded, not silently swallowed.
func TestBuildRecordsCollectorFailure(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	_, man, err := Build(decodeTestKey(t),
		fixedSpokeCollector{err: errFake}, nil, quietLogger())
	if err != nil {
		t.Fatalf("a collector error must not abort the hub backup: %v", err)
	}
	if _, ok := man.SpokeErrors["_collector"]; !ok {
		t.Fatal("collector-level failure must be recorded in SpokeErrors")
	}
}

// Secrets are required for a restorable hub, so a Secret failure IS fatal —
// the opposite policy from spokes, and worth pinning down.
func TestBuildFailsWhenSecretCollectionFails(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	_, _, err := Build(decodeTestKey(t), nil,
		fixedSecretCollector{err: errFake}, quietLogger())
	if err == nil {
		t.Fatal("a Secret collection failure must be fatal: the archive would not restore")
	}
}

// ---- Manifest / naming ----

func TestArchiveNameIsSortableUTC(t *testing.T) {
	older := ArchiveName(mustTime(t, "2026-07-01T00:00:00Z"))
	newer := ArchiveName(mustTime(t, "2026-07-31T00:00:00Z"))
	if older >= newer {
		t.Fatalf("archive names must sort chronologically: %q !< %q", older, newer)
	}
	if !strings.HasPrefix(older, "hive-hub-backup-") {
		t.Errorf("unexpected prefix: %q", older)
	}
	if !strings.HasSuffix(older, ".tar.gz.enc") {
		t.Errorf("unexpected suffix: %q", older)
	}
}

// ArchiveName must render in UTC regardless of the local zone, or archives
// taken from different zones sort incorrectly against each other.
func TestArchiveNameIsUTCRegardlessOfLocalZone(t *testing.T) {
	instant := mustTime(t, "2026-07-31T23:30:00Z")
	want := ArchiveName(instant)
	got := ArchiveName(instant.In(fixedZone(t, -8*3600)))
	if got != want {
		t.Fatalf("ArchiveName must normalise to UTC: %q vs %q", got, want)
	}
}

func TestManifestSurvivesJSONRoundTrip(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	_, man, err := Build(decodeTestKey(t), nil, nil, quietLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	var back Manifest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Files) != len(man.Files) {
		t.Fatalf("file list lost in JSON round trip: %d vs %d", len(back.Files), len(man.Files))
	}
	if back.FormatVersion != man.FormatVersion {
		t.Error("format version lost in JSON round trip")
	}
}
