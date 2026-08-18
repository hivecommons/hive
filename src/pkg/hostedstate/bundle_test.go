package hostedstate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func testMetadata() Metadata {
	return Metadata{
		Repository:       RepositoryIdentity{FullName: "example/widgets", ID: 123456},
		StateBranch:      "hive/state",
		Sequence:         2,
		PriorStateCommit: strings.Repeat("1", 40),
		ExecutionOwner:   "hosted",
		Release: ReleaseIdentity{
			Version:                    "v0.4.1-integrated.19",
			HiveCommit:                 strings.Repeat("2", 40),
			VisualHiveCommit:           strings.Repeat("3", 40),
			DistributionManifestSHA256: strings.Repeat("6", 64),
			HostedControllerProtocol:   1,
		},
		Controller: ControllerIdentity{
			RunID:          98765,
			Attempt:        2,
			Event:          "schedule",
			WorkflowPath:   ".github/workflows/hive-hosted.yml",
			WorkflowSHA:    strings.Repeat("4", 40),
			DefaultHeadSHA: strings.Repeat("5", 40),
		},
		CreatedAt: time.Date(2026, time.July, 15, 18, 30, 45, 123, time.UTC),
	}
}

func expected(metadata Metadata) ExpectedState {
	return ExpectedState{
		Repository:       metadata.Repository,
		StateBranch:      metadata.StateBranch,
		Sequence:         metadata.Sequence,
		PriorStateCommit: metadata.PriorStateCommit,
		Release:          metadata.Release,
	}
}

func privateStateRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		chmodToRoot(t, root, filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func chmodToRoot(t *testing.T, root, leaf string, mode os.FileMode) {
	t.Helper()
	for current := leaf; ; current = filepath.Dir(current) {
		if err := os.Chmod(current, mode); err != nil {
			t.Fatal(err)
		}
		if current == root {
			return
		}
	}
}

func exportFixture(t *testing.T) ([]byte, Metadata) {
	t.Helper()
	metadata := testMetadata()
	root := privateStateRoot(t, map[string]string{
		"lifecycle/issues.json": "{\"16\":\"open\"}\n",
		"scheduler.json":        "{\"paused\":false}\n",
	})
	encoded, err := Export(ExportOptions{
		Root: root,
		Paths: []string{
			"scheduler.json",
			"lifecycle/issues.json",
		},
		Metadata: metadata,
		Secret:   testSecret,
	})
	if err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	return encoded, metadata
}

func resign(t *testing.T, bundle *Bundle, key []byte) []byte {
	t.Helper()
	canonical, err := json.Marshal(unsignedBundle{
		SchemaVersion: bundle.SchemaVersion,
		Metadata:      bundle.Metadata,
		Files:         bundle.Files,
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle.Signature = sign(canonical, key)
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestRoundTripCanonicalAuthenticatedBundle(t *testing.T) {
	encoded, metadata := exportFixture(t)
	bundle, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if bundle.SchemaVersion != SchemaVersion {
		t.Fatalf("schema = %q", bundle.SchemaVersion)
	}
	if bundle.SchemaVersion != "hive.hosted-state.v2" {
		t.Fatalf("hosted-state fixture did not exercise schema v2: %q", bundle.SchemaVersion)
	}
	gotPaths := []string{bundle.Files[0].Path, bundle.Files[1].Path}
	if !sort.StringsAreSorted(gotPaths) {
		t.Fatalf("inventory is not sorted: %v", gotPaths)
	}
	if bundle.Metadata != metadata {
		t.Fatalf("metadata changed:\n got: %#v\nwant: %#v", bundle.Metadata, metadata)
	}

	parent := t.TempDir()
	destination := filepath.Join(parent, "restored")
	if err := Restore(RestoreOptions{
		Bundle:      encoded,
		Destination: destination,
		Secret:      testSecret,
		Expected:    expected(metadata),
	}); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	for path, want := range map[string]string{
		"lifecycle/issues.json": "{\"16\":\"open\"}\n",
		"scheduler.json":        "{\"paused\":false}\n",
	} {
		got, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
		if runtime.GOOS != "windows" {
			info, err := os.Stat(filepath.Join(destination, filepath.FromSlash(path)))
			if err != nil {
				t.Fatal(err)
			}
			if gotMode := info.Mode().Perm(); gotMode != 0o600 {
				t.Errorf("%s mode = %o, want 600", path, gotMode)
			}
		}
	}

	// An existing empty ordinary destination is supported as well.
	empty := filepath.Join(parent, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Restore(RestoreOptions{Bundle: encoded, Destination: empty, Secret: testSecret, Expected: expected(metadata)}); err != nil {
		t.Fatalf("Restore() into empty destination error = %v", err)
	}
}

func TestBundleTamperIsRejected(t *testing.T) {
	encoded, metadata := exportFixture(t)
	bundle, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("unsigned metadata change", func(t *testing.T) {
		changed := bundle
		changed.Metadata.Controller.Event = "workflow_dispatch"
		payload, err := json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		assertRestoreFailsWithoutDestination(t, payload, testSecret, expected(metadata), "signature")
	})

	t.Run("resigned content with stale digest", func(t *testing.T) {
		changed := bundle
		changed.Files = append([]File(nil), bundle.Files...)
		changed.Files[0].ContentBase64 = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", int(changed.Files[0].Size))))
		assertRestoreFailsWithoutDestination(t, resign(t, &changed, testSecret), testSecret, expected(metadata), "SHA-256")
	})

	t.Run("resigned release manifest tamper", func(t *testing.T) {
		changed := bundle
		changed.Metadata.Release.DistributionManifestSHA256 = strings.Repeat("a", 64)
		assertRestoreFailsWithoutDestination(t, resign(t, &changed, testSecret), testSecret, expected(metadata), "release identity")
	})

	t.Run("resigned release protocol tamper", func(t *testing.T) {
		changed := bundle
		changed.Metadata.Release.HostedControllerProtocol++
		assertRestoreFailsWithoutDestination(t, resign(t, &changed, testSecret), testSecret, expected(metadata), "release identity")
	})

	t.Run("signed legacy schema", func(t *testing.T) {
		changed := bundle
		changed.SchemaVersion = "hive.hosted-state.v1"
		assertRestoreFailsWithoutDestination(t, resign(t, &changed, testSecret), testSecret, expected(metadata), "unsupported hosted state schema")
	})

	t.Run("unknown JSON field", func(t *testing.T) {
		payload := append([]byte(nil), encoded...)
		payload = []byte(strings.Replace(string(payload), "{", "{\"unexpected\":true,", 1))
		assertRestoreFailsWithoutDestination(t, payload, testSecret, expected(metadata), "unknown field")
	})

	t.Run("trailing JSON", func(t *testing.T) {
		payload := append(append([]byte(nil), encoded...), []byte(" {}")...)
		assertRestoreFailsWithoutDestination(t, payload, testSecret, expected(metadata), "multiple JSON")
	})
}

func TestRestoreRejectsReplayPriorIdentityReleaseAndKey(t *testing.T) {
	encoded, metadata := exportFixture(t)
	tests := []struct {
		name     string
		secret   []byte
		expected ExpectedState
		contains string
	}{
		{name: "replayed sequence", secret: testSecret, expected: func() ExpectedState { value := expected(metadata); value.Sequence++; return value }(), contains: "sequence"},
		{name: "wrong prior", secret: testSecret, expected: func() ExpectedState {
			value := expected(metadata)
			value.PriorStateCommit = strings.Repeat("a", 40)
			return value
		}(), contains: "prior"},
		{name: "wrong repository name", secret: testSecret, expected: func() ExpectedState {
			value := expected(metadata)
			value.Repository.FullName = "example/other"
			return value
		}(), contains: "repository"},
		{name: "wrong repository id", secret: testSecret, expected: func() ExpectedState { value := expected(metadata); value.Repository.ID++; return value }(), contains: "repository"},
		{name: "wrong branch", secret: testSecret, expected: func() ExpectedState { value := expected(metadata); value.StateBranch = "other/state"; return value }(), contains: "branch"},
		{name: "wrong release version", secret: testSecret, expected: func() ExpectedState { value := expected(metadata); value.Release.Version = "v-next"; return value }(), contains: "release"},
		{name: "wrong Hive commit", secret: testSecret, expected: func() ExpectedState {
			value := expected(metadata)
			value.Release.HiveCommit = strings.Repeat("b", 40)
			return value
		}(), contains: "release"},
		{name: "wrong Visual Hive commit", secret: testSecret, expected: func() ExpectedState {
			value := expected(metadata)
			value.Release.VisualHiveCommit = strings.Repeat("c", 40)
			return value
		}(), contains: "release"},
		{name: "wrong manifest digest", secret: testSecret, expected: func() ExpectedState {
			value := expected(metadata)
			value.Release.DistributionManifestSHA256 = strings.Repeat("d", 64)
			return value
		}(), contains: "release"},
		{name: "wrong controller protocol", secret: testSecret, expected: func() ExpectedState {
			value := expected(metadata)
			value.Release.HostedControllerProtocol++
			return value
		}(), contains: "release"},
		{name: "wrong HMAC key", secret: []byte("abcdef0123456789abcdef0123456789"), expected: expected(metadata), contains: "signature"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertRestoreFailsWithoutDestination(t, encoded, test.secret, test.expected, test.contains)
		})
	}
}

func TestExportRejectsUnsafeAndAmbiguousPaths(t *testing.T) {
	root := privateStateRoot(t, map[string]string{"state.json": "{}"})
	tests := []struct {
		name  string
		paths []string
	}{
		{name: "absolute", paths: []string{filepath.Join(root, "state.json")}},
		{name: "traversal", paths: []string{"../state.json"}},
		{name: "backslash", paths: []string{"nested\\state.json"}},
		{name: "duplicate", paths: []string{"state.json", "state.json"}},
		{name: "case collision", paths: []string{"state.json", "STATE.JSON"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Export(ExportOptions{Root: root, Paths: test.paths, Metadata: testMetadata(), Secret: testSecret})
			if err == nil {
				t.Fatal("Export() succeeded")
			}
		})
	}
}

func TestRestoreRejectsSignedUnsafeAndAmbiguousPaths(t *testing.T) {
	encoded, metadata := exportFixture(t)
	original, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		files    []File
		contains string
	}{
		{name: "traversal", files: []File{fileRecord("../escape", []byte("x"))}, contains: "safe canonical"},
		{name: "backslash", files: []File{fileRecord(`nested\state`, []byte("x"))}, contains: "backslash"},
		{name: "portable drive path", files: []File{fileRecord("C:/state", []byte("x"))}, contains: "hosted state path"},
		{name: "portable reserved name", files: []File{fileRecord("state/CON.json", []byte("x"))}, contains: "unsafe component"},
		{name: "duplicate", files: []File{fileRecord("same", []byte("x")), fileRecord("same", []byte("x"))}, contains: "strictly sorted"},
		{name: "case collision", files: []File{fileRecord("STATE", []byte("x")), fileRecord("state", []byte("y"))}, contains: "case-fold collision"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := original
			changed.Files = test.files
			assertRestoreFailsWithoutDestination(t, resign(t, &changed, testSecret), testSecret, expected(metadata), test.contains)
		})
	}
}

func TestExportRejectsUnlistedSymlinkHardlinkAndNonRegular(t *testing.T) {
	t.Run("unlisted", func(t *testing.T) {
		root := privateStateRoot(t, map[string]string{"selected": "yes", "unlisted": "no"})
		_, err := Export(ExportOptions{Root: root, Paths: []string{"selected"}, Metadata: testMetadata(), Secret: testSecret})
		if err == nil || !strings.Contains(err.Error(), "unlisted") {
			t.Fatalf("Export() error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		root := privateStateRoot(t, map[string]string{"selected": "yes"})
		if err := os.Symlink(filepath.Join(root, "selected"), filepath.Join(root, "link")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		_, err := Export(ExportOptions{Root: root, Paths: []string{"selected"}, Metadata: testMetadata(), Secret: testSecret})
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("Export() error = %v", err)
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		root := privateStateRoot(t, map[string]string{"selected": "yes"})
		if err := os.Link(filepath.Join(root, "selected"), filepath.Join(root, "alias")); err != nil {
			t.Skipf("hardlinks unavailable: %v", err)
		}
		_, err := Export(ExportOptions{Root: root, Paths: []string{"selected", "alias"}, Metadata: testMetadata(), Secret: testSecret})
		if err == nil || !strings.Contains(err.Error(), "hard links") {
			t.Fatalf("Export() error = %v", err)
		}
	})

}

func TestInjectableFileAndByteLimits(t *testing.T) {
	root := privateStateRoot(t, map[string]string{"one": "1", "two": "22"})
	metadata := testMetadata()

	if _, err := Export(ExportOptions{
		Root: root, Paths: []string{"one", "two"}, Metadata: metadata, Secret: testSecret,
		Limits: Limits{MaxFiles: 1, MaxTotalBytes: 10},
	}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("file-bound Export() error = %v", err)
	}
	if _, err := Export(ExportOptions{
		Root: root, Paths: []string{"one", "two"}, Metadata: metadata, Secret: testSecret,
		Limits: Limits{MaxFiles: 2, MaxTotalBytes: 2},
	}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("byte-bound Export() error = %v", err)
	}

	encoded, _ := exportFixture(t)
	assertRestoreFailsWithLimits(t, encoded, metadata, Limits{MaxFiles: 1, MaxTotalBytes: 1024}, "limit")
	assertRestoreFailsWithLimits(t, encoded, metadata, Limits{MaxFiles: 10, MaxTotalBytes: 1}, "limit")
}

func TestRestoreCleansStagingOnPartialWriteFailure(t *testing.T) {
	encoded, metadata := exportFixture(t)
	bundle, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	contentA := []byte("ordinary file")
	contentNested := []byte("cannot be placed below a file")
	bundle.Files = []File{
		fileRecord("collision", contentA),
		fileRecord("collision/nested", contentNested),
	}
	encoded = resign(t, &bundle, testSecret)

	parent := t.TempDir()
	destination := filepath.Join(parent, "restored")
	err = Restore(RestoreOptions{Bundle: encoded, Destination: destination, Secret: testSecret, Expected: expected(metadata)})
	if err == nil {
		t.Fatal("Restore() succeeded")
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("destination exists after failed restore: %v", statErr)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("staging content remains after failed restore: %v", entries)
	}
}

func TestMetadataSequenceRulesAndMinimumKey(t *testing.T) {
	root := privateStateRoot(t, map[string]string{"state": "ok"})
	metadata := testMetadata()
	metadata.Sequence = 1
	metadata.PriorStateCommit = strings.Repeat("1", 40)
	if _, err := Export(ExportOptions{Root: root, Paths: []string{"state"}, Metadata: metadata, Secret: testSecret}); err == nil {
		t.Fatal("sequence 1 with prior commit succeeded")
	}
	metadata.PriorStateCommit = ""
	if _, err := Export(ExportOptions{Root: root, Paths: []string{"state"}, Metadata: metadata, Secret: []byte("too short")}); err == nil {
		t.Fatal("short HMAC key succeeded")
	}
}

func TestExportRejectsUnsafePermissionsWhenApplicable(t *testing.T) {
	if !permissionSafetyApplicable() {
		t.Skip("os.FileMode permissions are not authoritative on this platform")
	}
	root := privateStateRoot(t, map[string]string{"state": "ok"})
	if err := os.Chmod(filepath.Join(root, "state"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Export(ExportOptions{Root: root, Paths: []string{"state"}, Metadata: testMetadata(), Secret: testSecret})
	if err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("Export() error = %v", err)
	}
}

func fileRecord(path string, content []byte) File {
	digest := sha256.Sum256(content)
	return File{
		Path:          path,
		SHA256:        hex.EncodeToString(digest[:]),
		Size:          int64(len(content)),
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	}
}

func assertRestoreFailsWithoutDestination(t *testing.T, encoded, secret []byte, expected ExpectedState, contains string) {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "must-not-exist")
	err := Restore(RestoreOptions{Bundle: encoded, Destination: destination, Secret: secret, Expected: expected})
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("Restore() error = %v, want containing %q", err, contains)
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("destination exists after rejected restore: %v", statErr)
	}
}

func assertRestoreFailsWithLimits(t *testing.T, encoded []byte, metadata Metadata, limits Limits, contains string) {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "must-not-exist")
	err := Restore(RestoreOptions{
		Bundle: encoded, Destination: destination, Secret: testSecret, Expected: expected(metadata), Limits: limits,
	})
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("Restore() error = %v, want containing %q", err, contains)
	}
}
