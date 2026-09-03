package spokebackup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hubbackup"
)

// ---- DataDir ----

func TestDataDirHonoursOverrideAndDefault(t *testing.T) {
	t.Run("override", func(t *testing.T) {
		t.Setenv(EnvDataDir, "/custom/spoke")
		if got := DataDir(); got != "/custom/spoke" {
			t.Fatalf("want the override, got %q", got)
		}
	})
	t.Run("blank falls back", func(t *testing.T) {
		t.Setenv(EnvDataDir, "   ")
		if got := DataDir(); got != DefaultDataDir {
			t.Fatalf("a blank override must fall back to %q, got %q", DefaultDataDir, got)
		}
	})
	t.Run("unset falls back", func(t *testing.T) {
		t.Setenv(EnvDataDir, "")
		if got := DataDir(); got != DefaultDataDir {
			t.Fatalf("unset must fall back to %q, got %q", DefaultDataDir, got)
		}
	})
}

// ---- ArchiveName ----

// A blank or fully-sanitized-away hive ID must still yield a usable filename
// rather than one with an empty component.
func TestArchiveNameFallsBackForUnusableID(t *testing.T) {
	at := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"", "///", "---", "..."} {
		name := ArchiveName(id, at)
		if !strings.HasPrefix(name, "hive-spoke-backup-hive-") {
			t.Errorf("id %q must fall back to 'hive', got %q", id, name)
		}
		if strings.Contains(name, "--2026") {
			t.Errorf("id %q produced an empty component: %q", id, name)
		}
	}
}

// The timestamp must be UTC regardless of the caller's zone, so archives from
// different zones sort correctly against each other.
func TestArchiveNameIsUTC(t *testing.T) {
	instant := time.Date(2026, 7, 31, 23, 30, 0, 0, time.UTC)
	want := ArchiveName("h", instant)
	got := ArchiveName("h", instant.In(time.FixedZone("TEST", -8*3600)))
	if got != want {
		t.Fatalf("ArchiveName must normalise to UTC: %q vs %q", got, want)
	}
}

// The .enc suffix is load-bearing UX and the name must be shell/header safe.
func TestArchiveNameShape(t *testing.T) {
	name := ArchiveName("hosted-x", time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC))
	if !strings.HasSuffix(name, ".tar.gz.enc") {
		t.Errorf("archive must advertise encryption via .enc: %q", name)
	}
	if !strings.Contains(name, "20260731T120000Z") {
		t.Errorf("archive name must carry a sortable UTC stamp: %q", name)
	}
}

// A hive ID carrying separators or quotes must never escape into the filename,
// or it could break a Content-Disposition header.
func TestArchiveNameNeutralisesInjection(t *testing.T) {
	for _, bad := range []string{
		`../../etc/passwd`,
		`a"b`,
		"a/b\\c",
		"a b;rm -rf /",
	} {
		name := ArchiveName(bad, time.Now())
		for _, ch := range []string{"/", "\\", `"`, ";", " ", ".."} {
			// The ".tar.gz.enc" suffix legitimately contains dots, so check only
			// the ID portion.
			idPart := strings.TrimPrefix(name, "hive-spoke-backup-")
			idPart = idPart[:strings.LastIndex(idPart, "-")]
			if strings.Contains(idPart, ch) {
				t.Errorf("hive ID %q leaked %q into the filename: %q", bad, ch, name)
			}
		}
	}
}

// ---- readCapped ----

// A file above the per-file cap must be skipped, not truncated into the
// archive: a truncated bead ledger would restore as corrupt data.
func TestReadCappedRefusesOversizedFile(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.json")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse file: cheap to create, still reports a size above the cap.
	if err := f.Truncate(MaxFileBytes + 1); err != nil {
		f.Close()
		t.Skipf("cannot create a sparse file here: %v", err)
	}
	f.Close()

	_, err = readCapped(big)
	if err == nil {
		t.Fatal("a file above the per-file cap must be refused")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error should mention the cap, got %v", err)
	}
}

// A directory or other non-regular file must be refused rather than read.
func TestReadCappedRefusesNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	_, err := readCapped(dir)
	if err == nil {
		t.Fatal("a directory must not be read as a file")
	}
	if !strings.Contains(err.Error(), "regular") {
		t.Errorf("error should say the path is not a regular file, got %v", err)
	}
}

func TestReadCappedReportsMissingFile(t *testing.T) {
	if _, err := readCapped(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

// A file exactly at the cap is allowed — the boundary must not be off by one.
func TestReadCappedAllowsFileAtExactCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atcap")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxFileBytes); err != nil {
		f.Close()
		t.Skipf("cannot create a sparse file here: %v", err)
	}
	f.Close()

	if _, err := readCapped(path); err != nil {
		t.Fatalf("a file exactly at the cap must be allowed, got %v", err)
	}
}

// ---- Build edges ----

// An unreadable beads directory must be recorded, not silently produce a
// backup missing the very thing this package exists to capture.
func TestBuildRecordsUnreadableBeadsDir(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, configRuntimeFile), "live: config\n")
	// No beads dir at all.
	t.Setenv(EnvDataDir, dir)

	res, err := Build(testKey(), "hive-x", testLogger())
	if err != nil {
		t.Fatalf("a missing beads dir must not abort the backup: %v", err)
	}
	if _, ok := res.Skipped[beadsSubdir]; !ok {
		t.Fatal("a missing beads dir must be recorded in Skipped — it is the " +
			"whole reason this backup exists")
	}
	if len(res.BeadDirs) != 0 {
		t.Fatalf("no bead dirs should be reported, got %v", res.BeadDirs)
	}
	// The gap must also reach the manifest the owner sees.
	if _, ok := res.Manifest.SpokeErrors[beadsSubdir]; !ok {
		t.Fatal("the gap must surface in the manifest, not only in the Result")
	}
}

// Loose files directly inside beads/ are not agent directories and must be
// ignored rather than treated as one.
func TestBuildIgnoresNonDirEntriesInBeads(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, configRuntimeFile), "live: config\n")
	mustWrite(t, filepath.Join(dir, beadsSubdir, "stray.txt"), "not an agent dir")
	mustWrite(t, filepath.Join(dir, beadsSubdir, "scanner", "b1.json"), `{"id":1}`)
	t.Setenv(EnvDataDir, dir)

	res, err := Build(testKey(), "hive-x", testLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.BeadDirs) != 1 || res.BeadDirs[0] != "scanner" {
		t.Fatalf("only agent directories should be captured, got %v", res.BeadDirs)
	}
}

// The bead ledger must be discovered, not hardcoded: a hive running agents
// beyond the original five must still get a complete ledger.
func TestBuildDiscoversArbitraryAgentDirs(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, configRuntimeFile), "live: config\n")
	agents := []string{"scanner", "reviewer", "feature", "outreach", "custom-7", "zz-extra"}
	for _, a := range agents {
		mustWrite(t, filepath.Join(dir, beadsSubdir, a, "b.json"), `{"a":1}`)
	}
	t.Setenv(EnvDataDir, dir)

	res, err := Build(testKey(), "hive-x", testLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.BeadDirs) != len(agents) {
		t.Fatalf("want %d agent ledgers, got %d (%v)", len(agents), len(res.BeadDirs), res.BeadDirs)
	}
	// And the extra agents' beads must really be in the archive.
	man, err := hubbackup.Verify(testKey(), res.Sealed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var sawCustom bool
	for _, f := range man.Files {
		if strings.Contains(f.Path, "custom-7") {
			sawCustom = true
		}
	}
	if !sawCustom {
		t.Fatal("a non-standard agent's bead ledger was not archived")
	}
}

// App keys are credentials; several may exist and all must be captured, in a
// deterministic order.
func TestBuildCapturesAllAppKeysDeterministically(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, configRuntimeFile), "live: config\n")
	mustWrite(t, filepath.Join(dir, "gh-app-key-2.pem"), "KEY2")
	mustWrite(t, filepath.Join(dir, "gh-app-key-1.pem"), "KEY1")
	t.Setenv(EnvDataDir, dir)

	res, err := Build(testKey(), "hive-x", testLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	man, err := hubbackup.Verify(testKey(), res.Sealed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var keys []string
	for _, f := range man.Files {
		if strings.Contains(f.Path, "gh-app-key") {
			keys = append(keys, f.Path)
		}
	}
	if len(keys) != 2 {
		t.Fatalf("want both app keys archived, got %v", keys)
	}
	if !strings.Contains(keys[0], "key-1") {
		t.Fatalf("app keys must be archived in sorted order, got %v", keys)
	}
}

// Build must refuse a nil key as well as an empty one — no unencrypted archive
// carrying App private keys may ever be produced.
func TestBuildRefusesNilKey(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, configRuntimeFile), "x")
	t.Setenv(EnvDataDir, dir)

	_, err := Build(nil, "hive-x", testLogger())
	if err == nil {
		t.Fatal("a nil key must be refused: the archive carries App private keys")
	}
	if !strings.Contains(err.Error(), "refusing to build an unencrypted spoke backup") {
		t.Fatalf("want the explicit up-front refusal, got %v", err)
	}

	// An empty (non-nil) key is the same hazard and must be refused identically.
	_, err = Build([]byte{}, "hive-x", testLogger())
	if err == nil {
		t.Fatal("an empty key must be refused")
	}
	if !strings.Contains(err.Error(), "refusing to build an unencrypted spoke backup") {
		t.Fatalf("want the explicit up-front refusal for an empty key, got %v", err)
	}
}

// The manifest must name the hive so an owner with several archives can tell
// them apart.
func TestBuildManifestIdentifiesHive(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, configRuntimeFile), "live: config\n")
	t.Setenv(EnvDataDir, dir)

	res, err := Build(testKey(), "hosted-xyz", testLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Manifest.SpokeIDs) != 1 || res.Manifest.SpokeIDs[0] != "hosted-xyz" {
		t.Fatalf("manifest must identify the hive, got %v", res.Manifest.SpokeIDs)
	}
	if res.Manifest.FormatVersion != hubbackup.FormatVersion() {
		t.Errorf("manifest must stamp the shared format version, got %d",
			res.Manifest.FormatVersion)
	}
	// CreatedAt must be UTC so archives compare correctly.
	if res.Manifest.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt must be UTC, got %v", res.Manifest.CreatedAt.Location())
	}
}

// A spoke that is missing everything still produces a verifiable (near-empty)
// archive rather than an error, with every gap recorded.
func TestBuildOnEmptySpokeStillVerifies(t *testing.T) {
	t.Setenv(EnvDataDir, t.TempDir())

	res, err := Build(testKey(), "hive-x", testLogger())
	if err != nil {
		t.Fatalf("an empty spoke must still produce an archive: %v", err)
	}
	if _, err := hubbackup.Verify(testKey(), res.Sealed); err != nil {
		t.Fatalf("even a near-empty archive must verify: %v", err)
	}
	// Every expected file must be accounted for as skipped.
	for _, name := range includedRootFiles {
		if _, ok := res.Skipped[name]; !ok {
			t.Errorf("missing %q must be recorded in Skipped", name)
		}
	}
}

// The archive must be extractable through the shared hubbackup path, and the
// authoritative config must come back byte for byte.
func TestBuildArchiveExtractsThroughSharedPath(t *testing.T) {
	dir := seedSpoke(t)
	t.Setenv(EnvDataDir, dir)

	res, err := Build(testKey(), "hive-x", testLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	dest := t.TempDir()
	if _, err := hubbackup.Extract(testKey(), res.Sealed, dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Both config files must survive the round trip: the overlay because it
	// wins the boot merge, .bak because it serves the disaster fallback and
	// the variant-A/B hives. The fixture gives them a shared marker plus a
	// distinguishing line, so this asserts the owner's config came back AND
	// that the two files were not conflated.
	gotBak, err := os.ReadFile(filepath.Join(dest, spokePrefix, configRuntimeFile))
	if err != nil {
		t.Fatalf("post-merge snapshot missing from the restore: %v", err)
	}
	if !strings.Contains(string(gotBak), "owner-customised") {
		t.Fatalf("restored the WRONG config for %s: %q", configRuntimeFile, gotBak)
	}
	gotOverlay, err := os.ReadFile(filepath.Join(dest, spokePrefix, configOverlayFile))
	if err != nil {
		t.Fatalf("overlay missing from the restore: %v", err)
	}
	if !strings.Contains(string(gotOverlay), "owner-customised") {
		t.Fatalf("restored the WRONG config for %s: %q", configOverlayFile, gotOverlay)
	}
	// The stale hive.yaml must not be present at all.
	if _, err := os.Stat(filepath.Join(dest, spokePrefix, "hive.yaml")); err == nil {
		t.Fatal("the stale hive.yaml must never be archived")
	}
}

// An unreadable App key must be recorded and skipped rather than aborting the
// backup — the owner still wants the rest of their hive.
func TestBuildRecordsUnreadableAppKey(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, configRuntimeFile), "live: config\n")
	keyPath := filepath.Join(dir, "gh-app-key-1.pem")
	mustWrite(t, keyPath, "SECRET")
	if err := os.Chmod(keyPath, 0o000); err != nil {
		t.Skipf("cannot make a file unreadable here: %v", err)
	}
	if f, err := os.Open(keyPath); err == nil {
		f.Close()
		t.Skip("running as root; cannot make a file unreadable")
	}
	t.Setenv(EnvDataDir, dir)

	res, err := Build(testKey(), "hive-x", testLogger())
	if err != nil {
		t.Fatalf("an unreadable app key must not abort the backup: %v", err)
	}
	if _, ok := res.Skipped["gh-app-key-1.pem"]; !ok {
		t.Fatal("an unreadable app key must be recorded in Skipped, not silently absent")
	}
	// The key's contents must not have leaked into the archive.
	man, err := hubbackup.Verify(testKey(), res.Sealed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, f := range man.Files {
		if strings.Contains(f.Path, "gh-app-key-1") {
			t.Fatal("an unreadable key must not appear in the archive")
		}
	}
}

// An archive above MaxArchiveBytes must be refused rather than streamed to a
// browser. Verified by driving the shared builder directly at a low cap, since
// producing a real 256MiB archive in a unit test is not reasonable.
func TestArchiveSizeCapIsEnforcedByTheSharedBuilder(t *testing.T) {
	b := hubbackup.NewBuilder()
	if err := b.AddBytes("spoke/big", 0o600, []byte(strings.Repeat("x", 8192))); err != nil {
		t.Fatal(err)
	}
	man := &hubbackup.Manifest{FormatVersion: hubbackup.FormatVersion()}
	if _, err := b.Finish(testKey(), man, 64); err == nil {
		t.Fatal("an archive above the cap must be refused before streaming")
	}

	if MaxArchiveBytes <= 0 {
		t.Fatal("MaxArchiveBytes must be a positive cap")
	}
}

// Build must pass a real cap to the shared builder rather than 0 (unlimited).
// Driving the builder directly proves the cap works but says nothing about
// whether Build actually applies it, so this asserts the wiring by shrinking
// the cap and confirming a large spoke is refused.
func TestBuildAppliesTheArchiveCap(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, configRuntimeFile), "live: config\n")
	// Highly incompressible content so the sealed archive really exceeds a
	// small cap rather than gzipping down below it.
	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = byte(i * 2654435761 >> 13)
	}
	if err := os.WriteFile(filepath.Join(dir, "hive-state.json"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDataDir, dir)

	origCap := archiveCapBytes
	archiveCapBytes = 4096
	t.Cleanup(func() { archiveCapBytes = origCap })

	if _, err := Build(testKey(), "hive-x", testLogger()); err == nil {
		t.Fatal("Build must refuse an archive above the cap rather than " +
			"streaming it to a browser")
	}
}
