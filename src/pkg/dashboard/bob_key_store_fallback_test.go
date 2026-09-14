package dashboard

// Tests pinning the storeBobAPIKey PVC-failure fallback semantics
// (bob_key_store.go), mirroring litellm_key_store_fallback_test.go — the bob
// store deliberately reuses the same machinery and must keep the same
// contract. The happy path (PVC write succeeds) is covered in
// bob_key_store_test.go; these pin the outcomes when the PVC write fails:
//
//  1. Secret patch succeeds  → key IS persisted; api_key_file must be the
//     Secret mount path (config.DefaultBobAPIKeyFile), not the PVC path.
//  2. Not in cluster         → nothing persisted; the PVC error surfaces.
//  3. Secret patch also fails → both stores failed; the PVC error surfaces.
//
// Plus the PVC-succeeds-but-Secret-patch-rejected case (save still succeeds
// on the PVC path), the g+x directory repair in writeBobKeyFile, and the
// non-ENOENT error path of clearBobKeyFile. Every test asserts the secrecy
// invariant: the key value never appears in returned errors.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// breakBobPVCKeyFile points writableBobKeyFile at a path whose parent is a
// regular file, so writeBobKeyFile fails (ENOTDIR from MkdirAll) no matter
// what uid the tests run as. Restores the original on cleanup.
func breakBobPVCKeyFile(t *testing.T) {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing blocker file: %v", err)
	}
	orig := writableBobKeyFile
	writableBobKeyFile = filepath.Join(blocker, "secrets", "bob_api_key")
	t.Cleanup(func() { writableBobKeyFile = orig })
}

// PVC write fails but the hive-secrets Secret patch succeeds: the save must
// succeed and record the Secret mount path, and the PATCH must carry the
// base64 key under the bob_api_key data key.
func TestStoreBobAPIKey_PVCFailsSecretFallbackSucceeds(t *testing.T) {
	s := NewServer(0, fallbackTestLogger())
	breakBobPVCKeyFile(t)
	body, req := fakeKubeAPI(t, http.StatusOK)

	path, err := s.storeBobAPIKey(bobTestKey)
	if err != nil {
		t.Fatalf("storeBobAPIKey should succeed via the Secret fallback, got %v", err)
	}
	if path != config.DefaultBobAPIKeyFile {
		t.Errorf("api_key_file = %q, want Secret mount path %q", path, config.DefaultBobAPIKeyFile)
	}

	if req.Method != http.MethodPatch {
		t.Errorf("Secret patch method = %q, want PATCH", req.Method)
	}
	wantPath := "/api/v1/namespaces/hive-test-ns/secrets/hive-secrets"
	if req.URL.Path != wantPath {
		t.Errorf("Secret patch path = %q, want %q", req.URL.Path, wantPath)
	}

	var patch struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(*body, &patch); err != nil {
		t.Fatalf("patch body is not JSON: %v (%q)", err, *body)
	}
	if got := patch.Data[bobSecretDataKey]; got != base64.StdEncoding.EncodeToString([]byte(bobTestKey)) {
		t.Errorf("patch data[%s] = %q, want base64 of the key", bobSecretDataKey, got)
	}
}

// PVC write fails and we are not in a cluster: nothing persisted the key, so
// the save must fail with the PVC error — and never leak the key value.
func TestStoreBobAPIKey_PVCFailsNotInCluster(t *testing.T) {
	s := NewServer(0, fallbackTestLogger())
	breakBobPVCKeyFile(t)
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	path, err := s.storeBobAPIKey(bobTestKey)
	if err == nil {
		t.Fatalf("storeBobAPIKey should fail when no store persisted the key (path=%q)", path)
	}
	if path != "" {
		t.Errorf("api_key_file = %q on failure, want empty", path)
	}
	if !errors.Is(err, fs.ErrPermission) && !strings.Contains(err.Error(), filepath.Dir(writableBobKeyFile)) {
		t.Errorf("error should surface the PVC store path %q, got %v", filepath.Dir(writableBobKeyFile), err)
	}
	if strings.Contains(err.Error(), bobTestKey) {
		t.Errorf("error leaks the key value: %v", err)
	}
}

// PVC write fails AND the Secret patch is rejected (e.g. missing the
// hive-secrets-writer Role): both stores failed, the PVC error surfaces,
// and neither the key value nor the API error body leaks.
func TestStoreBobAPIKey_BothStoresFail(t *testing.T) {
	s := NewServer(0, fallbackTestLogger())
	breakBobPVCKeyFile(t)
	fakeKubeAPI(t, http.StatusForbidden)

	path, err := s.storeBobAPIKey(bobTestKey)
	if err == nil {
		t.Fatalf("storeBobAPIKey should fail when both stores fail (path=%q)", path)
	}
	if path != "" {
		t.Errorf("api_key_file = %q on failure, want empty", path)
	}
	if !strings.Contains(err.Error(), filepath.Dir(writableBobKeyFile)) {
		t.Errorf("error should surface the PVC (primary store) path, got %v", err)
	}
	if strings.Contains(err.Error(), bobTestKey) {
		t.Errorf("error leaks the key value: %v", err)
	}
}

// PVC write succeeds but the Secret patch is rejected: the PVC file is the
// authoritative store, so the save must still succeed and record the PVC
// path — a missing hive-secrets-writer Role must not sink a working save.
func TestStoreBobAPIKey_PVCSucceedsSecretPatchRejected(t *testing.T) {
	s := NewServer(0, fallbackTestLogger())
	pvcPath := pointBobKeyAtTempDir(t)
	fakeKubeAPI(t, http.StatusForbidden)

	path, err := s.storeBobAPIKey(bobTestKey)
	if err != nil {
		t.Fatalf("storeBobAPIKey should succeed on the PVC store alone, got %v", err)
	}
	if path != pvcPath {
		t.Errorf("api_key_file = %q, want PVC path %q", path, pvcPath)
	}
	got, readErr := os.ReadFile(pvcPath)
	if readErr != nil {
		t.Fatalf("reading stored key file: %v", readErr)
	}
	if string(got) != bobTestKey {
		t.Errorf("stored key = %q, want the submitted key", got)
	}
}

// A pre-existing owner-only (0700) secrets dir — every hive provisioned
// before the g+x fix — must be widened in place by exactly the group-execute
// bit so agent UIDs can traverse it, without granting anything else.
func TestWriteBobKeyFile_RepairsOwnerOnlyDirWithGroupExecOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics")
	}
	path := pointBobKeyAtTempDir(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("pre-creating secrets dir: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod 0700: %v", err)
	}

	if err := writeBobKeyFile(bobTestKey); err != nil {
		t.Fatalf("writeBobKeyFile: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat secrets dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o710 {
		t.Errorf("dir mode = %o, want 0710 (0700 + g+x only)", got)
	}
}

// A dir that already has group-execute must be left exactly as the operator
// set it — the repair adds one bit, never assigns bobKeyDirMode wholesale.
func TestWriteBobKeyFile_LeavesAlreadyTraversableDirModeAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics")
	}
	path := pointBobKeyAtTempDir(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("pre-creating secrets dir: %v", err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatalf("chmod 0750: %v", err)
	}

	if err := writeBobKeyFile(bobTestKey); err != nil {
		t.Fatalf("writeBobKeyFile: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat secrets dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("dir mode = %o, want 0750 unchanged", got)
	}
}

// A remove failure that is NOT file-not-found (here ENOTDIR: the parent is a
// regular file) must surface as an error, not be swallowed as "already
// cleared" — otherwise a revoke could silently leave the key on disk.
func TestClearBobKeyFile_NonNotExistErrorSurfaces(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing blocker file: %v", err)
	}
	orig := writableBobKeyFile
	writableBobKeyFile = filepath.Join(blocker, "bob_api_key")
	t.Cleanup(func() { writableBobKeyFile = orig })

	removed, err := clearBobKeyFile()
	if err == nil {
		t.Fatal("expected an error when the key file cannot be removed")
	}
	if removed {
		t.Error("removed = true, want false on failure")
	}
	if !strings.Contains(err.Error(), writableBobKeyFile) {
		t.Errorf("error should name the key file path, got %v", err)
	}
}
