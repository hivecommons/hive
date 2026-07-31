package hubbackup

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Run's spoke+secret path: with collectors enabled, the archive must contain
// both, proving the wiring in Run (not just Build) is correct.
func TestRunWithSpokesAndSecretsPopulatesArchive(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(EnvHubNamespace, "hive-hub")

	secretJSON := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"},"data":{"k":"dg=="}}`
	fakeKubectl(t, map[string]string{
		"get secret": secretJSON,
		"get pods":   "spoke-pod-1",
		"exec":       spokeStream(map[string]string{"hive.yaml.bak": "project: acme"}),
	})

	out := filepath.Join(t.TempDir(), "b.enc")
	man, _, err := Run(Options{LocalOnly: true, LocalPath: out}, quietLogger())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(man.SpokeIDs) != 1 || man.SpokeIDs[0] != "hosted-x" {
		t.Fatalf("spoke not captured through Run: %v", man.SpokeIDs)
	}
	if len(man.SecretNames) == 0 {
		t.Fatal("Secrets not captured through Run — the archive is not restorable")
	}

	// The spoke's config must really be inside the sealed archive.
	sealed, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := Extract(decodeTestKey(t), sealed, dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "spokes", "hosted-x", "hive.yaml.bak"))
	if err != nil {
		t.Fatalf("spoke config not in archive: %v", err)
	}
	if string(got) != "project: acme" {
		t.Fatalf("spoke config = %q", got)
	}
}

// A Secret collection failure must abort Run: an archive without the
// out-of-PVC Secrets cannot restore a working hub.
func TestRunFailsWhenSecretCollectionFails(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)
	fakeKubectl(t, map[string]string{"get secret": failMarker + "Forbidden"})

	out := filepath.Join(t.TempDir(), "b.enc")
	_, _, err := Run(Options{LocalOnly: true, LocalPath: out, SkipSpokes: true}, quietLogger())
	if err == nil {
		t.Fatal("a Secret collection failure must abort the backup")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Fatal("no archive may be written when Secrets could not be captured")
	}
}

// If the read-back after upload returns different bytes, Run must fail: this is
// the check that catches a truncated or partially-written upload.
func TestRunFailsWhenReadBackIsCorrupt(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ociNamespacePath {
			w.Write([]byte(`"testnamespace"`))
			return
		}
		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			// Read-back returns corrupted content.
			w.Write([]byte("this is not the archive that was uploaded"))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBucket, "backups")

	_, _, err := Run(Options{SkipSpokes: true, SkipSecrets: true}, quietLogger())
	if err == nil {
		t.Fatal("a corrupt read-back must fail the run")
	}
	if !strings.Contains(err.Error(), "verification") && !strings.Contains(err.Error(), "read-back") {
		t.Errorf("error should identify the read-back/verification step, got %v", err)
	}
}

// A failed read-back request (not just corrupt content) must also fail Run.
func TestRunFailsWhenReadBackRequestFails(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ociNamespacePath {
			w.Write([]byte(`"testnamespace"`))
			return
		}
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBucket, "backups")

	_, _, err := Run(Options{SkipSpokes: true, SkipSecrets: true}, quietLogger())
	if err == nil {
		t.Fatal("an archive that cannot be read back must fail the run")
	}
}

// A write to an unwritable local path must be reported.
func TestRunFailsWhenLocalPathUnwritable(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)

	// A path whose parent does not exist.
	out := filepath.Join(t.TempDir(), "no-such-dir", "b.enc")
	if _, _, err := Run(Options{
		LocalOnly: true, LocalPath: out, SkipSpokes: true, SkipSecrets: true,
	}, quietLogger()); err == nil {
		t.Fatal("an unwritable archive path must be reported")
	}
}

// Non-regular files (symlinks, sockets) must be skipped rather than archived as
// their target's content or causing an error.
func TestAddTreeSkipsNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	saas := filepath.Join(dir, saasSubdir)
	mustWrite(t, filepath.Join(saas, "real.json"), `{"real":1}`)
	if err := os.Symlink(filepath.Join(saas, "real.json"), filepath.Join(saas, "link.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv(EnvDataDir, dir)

	_, man, err := Build(decodeTestKey(t), nil, nil, quietLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, f := range man.Files {
		if strings.HasSuffix(f.Path, "link.json") {
			t.Fatal("a symlink must not be archived as a regular file")
		}
	}
	var sawReal bool
	for _, f := range man.Files {
		if strings.HasSuffix(f.Path, "real.json") {
			sawReal = true
		}
	}
	if !sawReal {
		t.Fatal("the real file should still be archived")
	}
}

// The exported Builder must skip non-regular files too.
func TestExportedAddTreeSkipsNonRegularFiles(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "real.json"), `{"real":1}`)
	if err := os.Symlink(filepath.Join(src, "real.json"), filepath.Join(src, "link.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	b := NewBuilder()
	if err := b.AddTree(src, "spoke", nil, quietLogger()); err != nil {
		t.Fatalf("AddTree: %v", err)
	}
	key := decodeTestKey(t)
	sealed, err := b.Finish(key, &Manifest{FormatVersion: FormatVersion()}, 0)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	man, err := Verify(key, sealed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, f := range man.Files {
		if strings.HasSuffix(f.Path, "link.json") {
			t.Fatal("Builder.AddTree archived a symlink")
		}
	}
}

// A directory tree that does not exist must not abort the exported builder.
func TestExportedAddTreeToleratesMissingSource(t *testing.T) {
	b := NewBuilder()
	err := b.AddTree(filepath.Join(t.TempDir(), "absent"), "spoke", nil, quietLogger())
	if err != nil {
		t.Fatalf("a missing source tree must be tolerated, got %v", err)
	}
}

// Build must record the excluded-directory policy correctly even when those
// directories are large — the archive must not balloon with scratch data.
func TestBuildExcludesScratchDirectoriesFromManifest(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)

	_, man, err := Build(decodeTestKey(t), nil, nil, quietLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, f := range man.Files {
		for _, excluded := range []string{"/nous/", "/beads/", "/logs/", "/home/"} {
			if strings.Contains(f.Path, excluded) {
				t.Errorf("scratch path %q must be excluded from the archive", f.Path)
			}
		}
	}
}
