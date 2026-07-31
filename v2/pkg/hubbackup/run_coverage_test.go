package hubbackup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- LoadTargets ----

func TestLoadTargetsMapsHivesToClusters(t *testing.T) {
	dir := seedHubData(t)
	clusters, hives, err := LoadTargets(dir)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if len(clusters) != 2 {
		t.Fatalf("want 2 clusters, got %d", len(clusters))
	}
	// The in-cluster hub target must not carry kubeconfig flags.
	if clusters["hive-oke"].KubeconfigPath != "" {
		t.Error("in-cluster target should have no kubeconfig path")
	}
	// The remote target must retain how to reach it, or its spokes are read
	// from the WRONG cluster.
	remote := clusters["vllm-d"]
	if remote.KubeconfigPath != "/etc/hive/kubeconfigs/vllm-d.yaml" || remote.Context != "vllm-d" {
		t.Fatalf("remote cluster addressing lost: %+v", remote)
	}
	if hives["hosted-x"] != "hive-oke" {
		t.Fatalf("hive->cluster mapping wrong: %v", hives)
	}
}

func TestLoadTargetsErrors(t *testing.T) {
	t.Run("missing clusters.json", func(t *testing.T) {
		if _, _, err := LoadTargets(t.TempDir()); err == nil {
			t.Fatal("missing clusters.json must be an error")
		}
	})

	t.Run("malformed clusters.json", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, saasSubdir, "clusters.json"), "{not json")
		if _, _, err := LoadTargets(dir); err == nil {
			t.Fatal("malformed clusters.json must be an error")
		}
	})

	t.Run("missing registry", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, saasSubdir, "clusters.json"), `[]`)
		if _, _, err := LoadTargets(dir); err == nil {
			t.Fatal("missing hub-registry.json must be an error")
		}
	})

	t.Run("malformed registry", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, saasSubdir, "clusters.json"), `[]`)
		mustWrite(t, filepath.Join(dir, registryFile), "{not json")
		if _, _, err := LoadTargets(dir); err == nil {
			t.Fatal("malformed hub-registry.json must be an error")
		}
	})
}

// Registry entries missing an id or clusterId must be skipped rather than
// producing an unaddressable "" -> "" mapping.
func TestLoadTargetsSkipsIncompleteHives(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, saasSubdir, "clusters.json"), `[{"id":"c1","in_cluster":true}]`)
	mustWrite(t, filepath.Join(dir, registryFile),
		`{"hives":[{"id":"good","clusterId":"c1"},{"id":"","clusterId":"c1"},{"id":"noclust","clusterId":""}]}`)

	_, hives, err := LoadTargets(dir)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if len(hives) != 1 {
		t.Fatalf("incomplete registry entries must be skipped, got %v", hives)
	}
	if _, ok := hives["good"]; !ok {
		t.Fatal("the complete entry was dropped")
	}
}

// ---- Run ----

// The local-only path exercises build -> self-verify -> write without needing
// object storage, and the archive it drops must itself verify.
func TestRunLocalOnlyWritesVerifiableArchive(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)

	out := filepath.Join(t.TempDir(), "backup.enc")
	man, path, err := Run(Options{
		LocalOnly: true, LocalPath: out, SkipSpokes: true, SkipSecrets: true,
	}, quietLogger())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if path != out {
		t.Fatalf("want archive at %q, got %q", out, path)
	}
	if man == nil || len(man.Files) == 0 {
		t.Fatal("Run returned an empty manifest")
	}

	sealed, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("archive not written: %v", err)
	}
	if _, err := Verify(decodeTestKey(t), sealed); err != nil {
		t.Fatalf("the archive Run wrote does not verify: %v", err)
	}

	// The file must be written with owner-only permissions: it contains the
	// hub's crypto material.
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("archive mode = %o, want 0600 (it holds hub key material)", perm)
	}
}

// An unset HIVE_BACKUP_KEY must hard-fail before anything is written, rather
// than falling back to writing an unencrypted archive.
func TestRunRefusesToRunWithoutKey(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, "")

	out := filepath.Join(t.TempDir(), "backup.enc")
	_, _, err := Run(Options{
		LocalOnly: true, LocalPath: out, SkipSpokes: true, SkipSecrets: true,
	}, quietLogger())
	if err == nil {
		t.Fatal("Run without a key must fail rather than write plaintext")
	}
	if !strings.Contains(err.Error(), EnvBackupKey) {
		t.Errorf("error should name %s, got %v", EnvBackupKey, err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Fatal("no archive may be written when the key is missing")
	}
}

// A malformed key must be rejected too — a short key is not silently padded.
func TestRunRejectsMalformedKey(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, "abcd")

	_, _, err := Run(Options{
		LocalOnly: true, LocalPath: filepath.Join(t.TempDir(), "b.enc"),
		SkipSpokes: true, SkipSecrets: true,
	}, quietLogger())
	if err == nil {
		t.Fatal("a short key must be rejected, not padded")
	}
}

// When spokes are enabled but the fleet cannot be enumerated, Run must abort:
// a backup silently missing every spoke is not a usable backup.
func TestRunFailsWhenFleetCannotBeEnumerated(t *testing.T) {
	t.Setenv(EnvDataDir, t.TempDir()) // no clusters.json
	t.Setenv(EnvBackupKey, testKey)

	_, _, err := Run(Options{
		LocalOnly: true, LocalPath: filepath.Join(t.TempDir(), "b.enc"),
		SkipSecrets: true,
	}, quietLogger())
	if err == nil {
		t.Fatal("Run must fail when the fleet cannot be enumerated")
	}
	if !strings.Contains(err.Error(), "enumerate spokes") {
		t.Errorf("error should explain the enumeration failure, got %v", err)
	}
}

// With no LocalPath, Run falls back to the generated archive name.
func TestRunLocalOnlyDefaultsToGeneratedName(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)

	// Run writes to a relative path, so isolate the working directory.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })

	_, path, err := Run(Options{
		LocalOnly: true, SkipSpokes: true, SkipSecrets: true,
	}, quietLogger())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(path, "hive-hub-backup-") {
		t.Fatalf("want a generated archive name, got %q", path)
	}
	if _, err := os.Stat(filepath.Join(tmp, path)); err != nil {
		t.Fatalf("archive not written at generated name: %v", err)
	}
}

// ---- Retention / DataDir env handling ----

func TestRetentionRejectsNonPositiveAndGarbage(t *testing.T) {
	for _, v := range []string{"0", "-3", "abc", ""} {
		t.Run("value_"+v, func(t *testing.T) {
			t.Setenv(EnvRetention, v)
			if got := Retention(); got != DefaultRetention {
				t.Fatalf("%q must fall back to the default %d, got %d",
					v, DefaultRetention, got)
			}
		})
	}
}

func TestDataDirHonoursOverrideAndDefault(t *testing.T) {
	t.Run("override", func(t *testing.T) {
		t.Setenv(EnvDataDir, "/custom/data")
		if got := DataDir(); got != "/custom/data" {
			t.Fatalf("want the override, got %q", got)
		}
	})
	t.Run("whitespace is ignored", func(t *testing.T) {
		t.Setenv(EnvDataDir, "   ")
		if got := DataDir(); got != DefaultDataDir {
			t.Fatalf("blank override must fall back to %q, got %q", DefaultDataDir, got)
		}
	})
}

// ---- Exported Builder (used by pkg/spokebackup) ----

// An archive assembled through the exported Builder must be accepted by the
// SAME Verify used for hub archives — that shared format is the whole point of
// exporting it.
func TestExportedBuilderProducesVerifiableArchive(t *testing.T) {
	key := decodeTestKey(t)
	b := NewBuilder()

	if err := b.AddBytes("spoke/hive.yaml.bak", 0o600, []byte("project: acme")); err != nil {
		t.Fatalf("AddBytes: %v", err)
	}

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "keep", "a.json"), `{"a":1}`)
	mustWrite(t, filepath.Join(src, "skipme", "b.json"), `{"b":2}`)
	if err := b.AddTree(src, "spoke/tree", func(rel string) bool {
		return strings.HasPrefix(rel, "skipme")
	}, quietLogger()); err != nil {
		t.Fatalf("AddTree: %v", err)
	}

	man := &Manifest{FormatVersion: FormatVersion()}
	sealed, err := b.Finish(key, man, 0)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	verified, err := Verify(key, sealed)
	if err != nil {
		t.Fatalf("hub Verify must accept a Builder-made archive: %v", err)
	}
	if len(verified.Files) == 0 {
		t.Fatal("manifest digests were not populated — Verify would be vacuous")
	}

	dest := t.TempDir()
	if _, err := Extract(key, sealed, dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "spoke", "hive.yaml.bak")); string(got) != "project: acme" {
		t.Fatalf("content corrupted: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "spoke", "tree", "keep", "a.json")); string(got) != `{"a":1}` {
		t.Fatalf("tree content corrupted: %q", got)
	}
	// The skip predicate must actually prune.
	if _, err := os.Stat(filepath.Join(dest, "spoke", "tree", "skipme")); err == nil {
		t.Fatal("AddTree ignored the skip predicate")
	}
}

// FormatVersion must match what Verify enforces, or archives built by other
// packages are rejected on restore.
func TestExportedFormatVersionMatchesVerify(t *testing.T) {
	if FormatVersion() != backupFormatVersion {
		t.Fatalf("FormatVersion() = %d but Verify enforces %d",
			FormatVersion(), backupFormatVersion)
	}
}

func TestBuilderFinishRequiresManifest(t *testing.T) {
	if _, err := NewBuilder().Finish(decodeTestKey(t), nil, 0); err == nil {
		t.Fatal("Finish without a manifest must error")
	}
}

// The size cap protects a caller streaming an archive to a browser.
func TestBuilderFinishEnforcesSizeCap(t *testing.T) {
	b := NewBuilder()
	if err := b.AddBytes("big", 0o600, []byte(strings.Repeat("x", 4096))); err != nil {
		t.Fatal(err)
	}
	_, err := b.Finish(decodeTestKey(t), &Manifest{FormatVersion: FormatVersion()}, 8)
	if err == nil {
		t.Fatal("an archive above the cap must be refused")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error should mention the cap, got %v", err)
	}
}

// A zero/negative cap means unlimited.
func TestBuilderFinishZeroCapMeansUnlimited(t *testing.T) {
	b := NewBuilder()
	if err := b.AddBytes("big", 0o600, []byte(strings.Repeat("x", 4096))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Finish(decodeTestKey(t), &Manifest{FormatVersion: FormatVersion()}, 0); err != nil {
		t.Fatalf("cap 0 must mean unlimited, got %v", err)
	}
}
