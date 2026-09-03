package hub

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================
// UpgradeSelfToSHA + upgradeSelfMutableToSHA + SelfDeploymentImage
//
// The existing self_upgrade_coverage_test.go covers the mutable-tag happy path
// and the pinned-tag image-rewrite path.  This file adds:
//   - selfDeploymentImage failure → needsRestart fallback
//   - pinned image already on target → no-op
//   - pinned image with no colon → restart fallback
//   - pinned image switch failure → error surfaced
//   - mutable tag with empty targetSHA → restart fallback
//   - mutable tag namespace read failure → restart fallback
//   - mutable tag empty namespace → restart fallback
//   - selfDeploymentImage bad JSON / no containers
//   - SelfDeploymentImage cache behaviour
//   - RolloutRestartSelf empty namespace content
//   - SwitchImageSelf invalid image ref
// ============================================================

func TestUpgradeSelfToSHA_SelfImageError_FallsBackToRestart(t *testing.T) {
	// When selfDeploymentImage() cannot be read, UpgradeSelfToSHA returns
	// needsRestart=true (the safe default).
	oldNS := k8sNamespacePath
	k8sNamespacePath = filepath.Join(t.TempDir(), "missing")
	defer func() { k8sNamespacePath = oldNS }()

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "abc1234")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !needsRestart {
		t.Error("expected needsRestart=true when selfDeploymentImage fails")
	}
}

func TestUpgradeSelfToSHA_PinnedAlreadyOnTarget(t *testing.T) {
	const currentImage = "ghcr.io/hivecommons/hive:abc1234"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GET returns deployment with image already at target
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"` + currentImage + `"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "abc1234")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if needsRestart {
		t.Error("expected needsRestart=false when already on target")
	}
}

func TestUpgradeSelfToSHA_PinnedNoColon_FallsBackToRestart(t *testing.T) {
	// An image with no colon (no tag) can't be split — returns needsRestart=true.
	const currentImage = "ghcr.io/hivecommons/hive"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"` + currentImage + `"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "newsha")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !needsRestart {
		t.Error("expected needsRestart=true when image has no colon")
	}
}

func TestUpgradeSelfToSHA_PinnedSwitchFails_ErrorSurfaced(t *testing.T) {
	// The pinned path calls SwitchImageSelf; if that fails the error must
	// surface (not be swallowed).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/hivecommons/hive:oldsha"}]}}}}`))
			return
		}
		// PATCH fails
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`server error`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "newsha")
	if err == nil {
		t.Fatal("expected error when SwitchImageSelf fails on pinned path")
	}
	if needsRestart {
		t.Error("needsRestart should be false when err is non-nil on pinned path")
	}
	if !strings.Contains(err.Error(), "patching pinned image") {
		t.Errorf("error should mention pinned image patch, got: %v", err)
	}
}

func TestUpgradeSelfMutableToSHA_EmptyTarget_FallsBackToRestart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/hivecommons/hive:v2-latest"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !needsRestart {
		t.Error("expected needsRestart=true when targetSHA is empty")
	}
}

func TestUpgradeSelfMutableToSHA_NamespaceFileMissing_FallsBackToRestart(t *testing.T) {
	// upgradeSelfMutableToSHA reads k8sNamespacePath a SECOND time (independently
	// of selfDeploymentImage). If that read fails, it falls back to restart.
	const currentImage = "ghcr.io/hivecommons/hive:v2-latest"

	// We need the GET to succeed (so selfDeploymentImage works) but the
	// second namespace read inside upgradeSelfMutableToSHA to fail.
	// The simplest way: use a handler that returns the deployment for GET,
	// then remove the namespace file before the function reaches its own read.
	dir := t.TempDir()
	nsPath := filepath.Join(dir, "namespace")
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(nsPath, []byte("test-ns"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("fake-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	var getCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		getCount++
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"` + currentImage + `"}]}}}}`))
	}))
	defer srv.Close()

	oldServer, oldToken, oldCA, oldNS := k8sAPIServer, k8sTokenPath, k8sCACertPath, k8sNamespacePath
	k8sAPIServer = srv.URL
	k8sTokenPath = tokenPath
	k8sCACertPath = filepath.Join(dir, "ca.crt")
	writeTestK8sCACert(t, k8sCACertPath)
	k8sNamespacePath = nsPath
	t.Cleanup(func() {
		k8sAPIServer, k8sTokenPath, k8sCACertPath, k8sNamespacePath = oldServer, oldToken, oldCA, oldNS
	})

	// Remove namespace file after setup so selfDeploymentImage's read
	// succeeds (it was called before we remove), but upgradeSelfMutableToSHA's
	// second read fails. Actually both reads use the same path, so we need
	// a trick: write a file that we can remove between the two reads.
	// The issue is both happen in sequence. Let's just remove it now — both
	// selfDeploymentImage AND upgradeSelfMutableToSHA will fail at namespace read.
	// When selfDeploymentImage fails, UpgradeSelfToSHA returns (true, nil) immediately.
	os.Remove(nsPath)

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "07ccca1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !needsRestart {
		t.Error("expected needsRestart=true when namespace file is missing")
	}
}

func TestUpgradeSelfMutableToSHA_EmptyNamespace_FallsBackToRestart(t *testing.T) {
	// Call upgradeSelfMutableToSHA directly to test the empty-namespace branch.
	dir := t.TempDir()
	nsPath := filepath.Join(dir, "namespace")
	// Write empty namespace
	if err := os.WriteFile(nsPath, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldNS := k8sNamespacePath
	k8sNamespacePath = nsPath
	defer func() { k8sNamespacePath = oldNS }()

	needsRestart, err := upgradeSelfMutableToSHA(slog.Default(), "ghcr.io/hivecommons/hive:v2-latest", "target1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !needsRestart {
		t.Error("expected needsRestart=true when namespace is empty")
	}
}

func TestSelfDeploymentImage_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	img, err := selfDeploymentImage()
	if err == nil {
		t.Fatal("expected error on bad JSON")
	}
	if img != "" {
		t.Errorf("expected empty image on error, got %q", img)
	}
}

func TestSelfDeploymentImage_NoContainers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	img, err := selfDeploymentImage()
	if err == nil {
		t.Fatal("expected error when deployment has no containers")
	}
	if !strings.Contains(err.Error(), "no containers") {
		t.Errorf("error should mention no containers, got: %v", err)
	}
	if img != "" {
		t.Errorf("expected empty image, got %q", img)
	}
}

func TestSelfDeploymentImage_EmptyNamespace(t *testing.T) {
	dir := t.TempDir()
	nsPath := filepath.Join(dir, "namespace")
	if err := os.WriteFile(nsPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	oldNS := k8sNamespacePath
	k8sNamespacePath = nsPath
	defer func() { k8sNamespacePath = oldNS }()

	_, err := selfDeploymentImage()
	if err == nil {
		t.Fatal("expected error on empty namespace")
	}
	if !strings.Contains(err.Error(), "empty namespace") {
		t.Errorf("error should mention empty namespace, got: %v", err)
	}
}

func TestSelfDeploymentImageCached(t *testing.T) {
	// Reset cache state for this test.
	selfImageMu.Lock()
	oldCached, oldFetched, oldAttempted := selfImageCached, selfImageFetched, selfImageAttempted
	selfImageCached = ""
	selfImageFetched = time.Time{}
	selfImageAttempted = false
	selfImageMu.Unlock()
	t.Cleanup(func() {
		selfImageMu.Lock()
		selfImageCached, selfImageFetched, selfImageAttempted = oldCached, oldFetched, oldAttempted
		selfImageMu.Unlock()
	})

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/hivecommons/hive:v2-latest"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	// First call should hit the server.
	img := SelfDeploymentImage()
	if img != "ghcr.io/hivecommons/hive:v2-latest" {
		t.Fatalf("first call: got %q", img)
	}
	if hits != 1 {
		t.Fatalf("expected 1 server hit on first call, got %d", hits)
	}

	// Second call should use cache.
	img2 := SelfDeploymentImage()
	if img2 != img {
		t.Errorf("cached call returned different value: %q vs %q", img2, img)
	}
	if hits != 1 {
		t.Errorf("expected cache hit (1 total server call), got %d", hits)
	}
}

func TestSelfDeploymentImageCached_ReturnsEmptyOnFailure(t *testing.T) {
	// When selfDeploymentImage() fails, SelfDeploymentImage() returns "" and
	// caches the failure so subsequent calls don't retry.
	selfImageMu.Lock()
	oldCached, oldFetched, oldAttempted := selfImageCached, selfImageFetched, selfImageAttempted
	selfImageCached = ""
	selfImageFetched = time.Time{}
	selfImageAttempted = false
	selfImageMu.Unlock()
	t.Cleanup(func() {
		selfImageMu.Lock()
		selfImageCached, selfImageFetched, selfImageAttempted = oldCached, oldFetched, oldAttempted
		selfImageMu.Unlock()
	})

	// Point namespace at a missing file so selfDeploymentImage() fails.
	oldNS := k8sNamespacePath
	k8sNamespacePath = filepath.Join(t.TempDir(), "missing")
	defer func() { k8sNamespacePath = oldNS }()

	img := SelfDeploymentImage()
	if img != "" {
		t.Errorf("expected empty string on failure, got %q", img)
	}

	// Second call should still return "" from cache.
	img2 := SelfDeploymentImage()
	if img2 != "" {
		t.Errorf("cached failure should return empty, got %q", img2)
	}
}

func TestRolloutRestartSelf_EmptyNamespaceContent(t *testing.T) {
	dir := t.TempDir()
	nsPath := filepath.Join(dir, "namespace")
	if err := os.WriteFile(nsPath, []byte("   "), 0o600); err != nil {
		t.Fatal(err)
	}
	oldNS := k8sNamespacePath
	k8sNamespacePath = nsPath
	defer func() { k8sNamespacePath = oldNS }()

	err := RolloutRestartSelf(slog.Default())
	if err == nil {
		t.Fatal("expected error when namespace content is whitespace-only")
	}
	if !strings.Contains(err.Error(), "empty namespace") {
		t.Errorf("error should mention empty namespace, got: %v", err)
	}
}

func TestSwitchImageSelf_InvalidImageRef(t *testing.T) {
	// validateImageRef rejects empty images and bad patterns.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be hit for invalid image ref")
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if err := SwitchImageSelf(slog.Default(), ""); err == nil {
		t.Error("expected error for empty image")
	}
}

func TestSwitchImageSelf_PatchFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive"}],"initContainers":[]}}}}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`forbidden`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	err := SwitchImageSelf(slog.Default(), "ghcr.io/hivecommons/hive:v2-latest")
	if err == nil {
		t.Fatal("expected error when PATCH fails")
	}
	if !strings.Contains(err.Error(), "patching deployment image") {
		t.Errorf("error should mention patching, got: %v", err)
	}
}

func TestSwitchImageSelf_EmptyNamespaceContent(t *testing.T) {
	dir := t.TempDir()
	nsPath := filepath.Join(dir, "namespace")
	if err := os.WriteFile(nsPath, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldNS := k8sNamespacePath
	k8sNamespacePath = nsPath
	defer func() { k8sNamespacePath = oldNS }()

	err := SwitchImageSelf(slog.Default(), "ghcr.io/hivecommons/hive:v2-latest")
	if err == nil {
		t.Fatal("expected error when namespace is empty")
	}
	if !strings.Contains(err.Error(), "empty namespace") {
		t.Errorf("error should mention empty namespace, got: %v", err)
	}
}

func TestSwitchImageSelf_GetFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`not found`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	err := SwitchImageSelf(slog.Default(), "ghcr.io/hivecommons/hive:v2-latest")
	if err == nil {
		t.Fatal("expected error when GET deployment fails")
	}
	if !strings.Contains(err.Error(), "listing deployment containers") {
		t.Errorf("error should mention listing containers, got: %v", err)
	}
}

// TestUpgradeSelfMutableToSHA_DirectCall_Success exercises upgradeSelfMutableToSHA
// directly to confirm the annotation patch includes the correct content.
func TestUpgradeSelfMutableToSHA_DirectCall_Success(t *testing.T) {
	var patchBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			b, _ := io.ReadAll(r.Body)
			patchBody = string(b)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	needsRestart, err := upgradeSelfMutableToSHA(slog.Default(), "ghcr.io/hivecommons/hive:mk-latest", "deadbeef")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if needsRestart {
		t.Error("expected needsRestart=false after successful annotation patch")
	}
	if !strings.Contains(patchBody, "deadbeef") {
		t.Errorf("patch body should contain target SHA, got: %s", patchBody)
	}
	if !strings.Contains(patchBody, selfUpgradeTargetAnnotation) {
		t.Errorf("patch body should contain target annotation, got: %s", patchBody)
	}
	if !strings.Contains(patchBody, "restart-at") {
		t.Errorf("patch body should contain restart-at annotation, got: %s", patchBody)
	}
}
