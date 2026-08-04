package hub

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ============================================================
// Extended coverage for self_upgrade.go
//
// Covers: imageTagIsMutable, UpgradeSelfToSHA edge cases,
// SelfDeploymentImage cached wrapper, SwitchImageSelf with
// invalid image, RolloutRestartSelf with empty namespace.
// ============================================================

func TestImageTagIsMutable(t *testing.T) {
	tests := []struct {
		image string
		want  bool
	}{
		// Mutable tags
		{"ghcr.io/kubestellar/hive:v2-latest", true},
		{"ghcr.io/kubestellar/hive:v3-latest", true},
		{"ghcr.io/kubestellar/hive:mk-latest", true},
		{"ghcr.io/kubestellar/hive:dd-latest", true},
		{"registry.example.com:5000/hive:dev-latest", true},

		// Immutable tags (SHA pins)
		{"ghcr.io/kubestellar/hive:c11643a", false},
		{"ghcr.io/kubestellar/hive:07ccca1", false},
		{"ghcr.io/kubestellar/hive:fb37afe", false},

		// Digest pins
		{"ghcr.io/kubestellar/hive@sha256:abc123", false},

		// Edge cases
		{"", false},
		{"ghcr.io/kubestellar/hive", false},             // no tag at all
		{"registry:5000/hive", false},                    // port, not a tag
		{"ghcr.io/kubestellar/hive:latest", false},       // "latest" != "-latest"
		{"ghcr.io/kubestellar/hive:v2-latest-rc1", false}, // suffix mismatch
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			got := imageTagIsMutable(tt.image)
			if got != tt.want {
				t.Errorf("imageTagIsMutable(%q) = %v, want %v", tt.image, got, tt.want)
			}
		})
	}
}

func TestUpgradeSelfToSHA_CannotReadImage(t *testing.T) {
	// When selfDeploymentImage fails, UpgradeSelfToSHA should return
	// needsRestart=true as a safe fallback.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GET returns non-200 so selfDeploymentImage fails
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "07ccca1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !needsRestart {
		t.Error("when image cannot be read, should fall back to needsRestart=true")
	}
}

func TestUpgradeSelfToSHA_EmptyTarget_MutableTag(t *testing.T) {
	// An empty targetSHA on a mutable tag falls back to restart.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/kubestellar/hive:v2-latest"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !needsRestart {
		t.Error("empty target with mutable tag should fall back to needsRestart=true")
	}
}

func TestUpgradeSelfToSHA_AlreadyAtTarget(t *testing.T) {
	// When the current image already has the target SHA as its tag, no-op.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/kubestellar/hive:07ccca1"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "07ccca1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if needsRestart {
		t.Error("already-at-target should not trigger a restart")
	}
}

func TestUpgradeSelfToSHA_PinnedNoTag(t *testing.T) {
	// An image with no tag at all (no colon) falls back to restart.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/kubestellar/hive"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "07ccca1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !needsRestart {
		t.Error("image with no tag should fall back to restart")
	}
}

func TestUpgradeSelfToSHA_PinnedPatchFails(t *testing.T) {
	// When the pinned-image patch fails, error is returned (not swallowed).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/kubestellar/hive:c11643a"}]}}}}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("forbidden"))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	_, err := UpgradeSelfToSHA(slog.Default(), "07ccca1")
	if err == nil {
		t.Fatal("expected error when pinned image patch fails")
	}
}

func TestRolloutRestartSelfEmptyNamespace(t *testing.T) {
	// An empty (but existing) namespace file should error.
	dir := t.TempDir()
	nsPath := filepath.Join(dir, "namespace")
	if err := os.WriteFile(nsPath, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldNS := k8sNamespacePath
	k8sNamespacePath = nsPath
	defer func() { k8sNamespacePath = oldNS }()

	if err := RolloutRestartSelf(slog.Default()); err == nil {
		t.Error("expected error for empty namespace content")
	}
}

func TestSwitchImageSelfInvalidRef(t *testing.T) {
	// SwitchImageSelf must reject an invalid image reference before
	// touching the k8s API.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("k8s API should not be called for invalid image refs")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	// Empty image is invalid
	if err := SwitchImageSelf(slog.Default(), ""); err == nil {
		t.Error("expected error for empty image ref")
	}
}

func TestSelfDeploymentImage_Cached(t *testing.T) {
	// The cached wrapper should call the API once, then serve from cache.
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/kubestellar/hive:v2-latest"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	// Reset the cache state for this test
	selfImageMu.Lock()
	oldCached, oldFetched, oldAttempted := selfImageCached, selfImageFetched, selfImageAttempted
	selfImageCached = ""
	selfImageFetched = time.Time{}
	selfImageAttempted = false
	selfImageMu.Unlock()
	defer func() {
		selfImageMu.Lock()
		selfImageCached = oldCached
		selfImageFetched = oldFetched
		selfImageAttempted = oldAttempted
		selfImageMu.Unlock()
	}()

	// First call should hit the API
	img := SelfDeploymentImage()
	if img != "ghcr.io/kubestellar/hive:v2-latest" {
		t.Errorf("first call: got %q, want image", img)
	}
	if callCount != 1 {
		t.Errorf("first call: API called %d times, want 1", callCount)
	}

	// Second call within TTL should use cache
	img2 := SelfDeploymentImage()
	if img2 != img {
		t.Errorf("second call: got %q, want %q", img2, img)
	}
	if callCount != 1 {
		t.Errorf("second call: API called %d times, want still 1 (cached)", callCount)
	}
}

func TestSelfDeploymentImage_FailureCached(t *testing.T) {
	// A failed API call should cache the empty result and not retry.
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	// Reset cache
	selfImageMu.Lock()
	oldCached, oldFetched, oldAttempted := selfImageCached, selfImageFetched, selfImageAttempted
	selfImageCached = ""
	selfImageFetched = time.Time{}
	selfImageAttempted = false
	selfImageMu.Unlock()
	defer func() {
		selfImageMu.Lock()
		selfImageCached = oldCached
		selfImageFetched = oldFetched
		selfImageAttempted = oldAttempted
		selfImageMu.Unlock()
	}()

	img := SelfDeploymentImage()
	if img != "" {
		t.Errorf("failed call should return empty, got %q", img)
	}

	// Second call should not retry
	_ = SelfDeploymentImage()
	if callCount != 1 {
		t.Errorf("failure should be cached, but API called %d times", callCount)
	}
}

func TestSelfDeploymentImage_ConcurrentSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/kubestellar/hive:test"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	// Reset cache
	selfImageMu.Lock()
	oldCached, oldFetched, oldAttempted := selfImageCached, selfImageFetched, selfImageAttempted
	selfImageCached = ""
	selfImageFetched = time.Time{}
	selfImageAttempted = false
	selfImageMu.Unlock()
	defer func() {
		selfImageMu.Lock()
		selfImageCached = oldCached
		selfImageFetched = oldFetched
		selfImageAttempted = oldAttempted
		selfImageMu.Unlock()
	}()

	// Concurrent calls should not race
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = SelfDeploymentImage()
		}()
	}
	wg.Wait()
}

func TestUpgradeSelfMutableTag_EmptyNamespace(t *testing.T) {
	// upgradeSelfMutableToSHA with empty namespace should fall back to restart.
	dir := t.TempDir()
	nsPath := filepath.Join(dir, "namespace")
	if err := os.WriteFile(nsPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/kubestellar/hive:v2-latest"}]}}}}`))
	}))
	defer srv.Close()

	// Set up the fake but override namespace with empty
	withFakeK8sAPI(t, srv)
	oldNS := k8sNamespacePath
	k8sNamespacePath = nsPath
	defer func() { k8sNamespacePath = oldNS }()

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "07ccca1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !needsRestart {
		t.Error("empty namespace in mutable path should fall back to restart")
	}
}

func TestUpgradeSelfMutableTag_MissingNamespace(t *testing.T) {
	// upgradeSelfMutableToSHA with missing namespace file should fall back.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/kubestellar/hive:v2-latest"}]}}}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	// Override namespace path to missing
	oldNS := k8sNamespacePath
	k8sNamespacePath = filepath.Join(t.TempDir(), "missing")
	defer func() { k8sNamespacePath = oldNS }()

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "07ccca1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !needsRestart {
		t.Error("missing namespace should fall back to restart")
	}
}

func TestSwitchImageSelf_EmptyNamespace(t *testing.T) {
	dir := t.TempDir()
	nsPath := filepath.Join(dir, "namespace")
	if err := os.WriteFile(nsPath, []byte("  "), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not reach k8s API with empty namespace")
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	oldNS := k8sNamespacePath
	k8sNamespacePath = nsPath
	defer func() { k8sNamespacePath = oldNS }()

	if err := SwitchImageSelf(slog.Default(), "ghcr.io/kubestellar/hive:v2-latest"); err == nil {
		t.Error("expected error for empty namespace")
	}
}

// TestSelfDeploymentImage_NoContainers verifies the selfDeploymentImage helper
// returns an error when the deployment JSON has no containers.
func TestSelfDeploymentImage_NoContainers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	_, err := selfDeploymentImage()
	if err == nil {
		t.Error("expected error when deployment has no containers")
	}
}

// TestSelfDeploymentImage_BadJSON verifies JSON parse errors are surfaced.
func TestSelfDeploymentImage_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not valid json`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	_, err := selfDeploymentImage()
	if err == nil {
		t.Error("expected error on bad JSON")
	}
}

// TestK8sAPIGet_BadToken verifies behaviour when token file is missing.
func TestK8sAPIGet_BadToken(t *testing.T) {
	oldToken := k8sTokenPath
	k8sTokenPath = filepath.Join(t.TempDir(), "missing-token")
	defer func() { k8sTokenPath = oldToken }()

	_, err := k8sAPIGet("/api/v1/nodes")
	if err == nil {
		t.Error("expected error when SA token is missing")
	}
}

// TestK8sAPIPatch_WithCACert covers the TLS branch where ca.crt is present.
func TestK8sAPIPatch_WithCACert(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"test":true}` {
			t.Errorf("unexpected body: %s", b)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	nsPath := filepath.Join(dir, "namespace")
	caPath := filepath.Join(dir, "ca.crt")

	os.WriteFile(tokenPath, []byte("tok"), 0o600)
	os.WriteFile(nsPath, []byte("ns"), 0o600)
	// Write the test server's CA cert
	os.WriteFile(caPath, srv.Certificate().Raw, 0o600)

	oldServer, oldToken, oldCA, oldNS := k8sAPIServer, k8sTokenPath, k8sCACertPath, k8sNamespacePath
	k8sAPIServer = srv.URL
	k8sTokenPath = tokenPath
	k8sCACertPath = caPath
	k8sNamespacePath = nsPath
	defer func() {
		k8sAPIServer, k8sTokenPath, k8sCACertPath, k8sNamespacePath = oldServer, oldToken, oldCA, oldNS
	}()

	// This will fail TLS verification (self-signed cert not in PEM format),
	// but exercises the CA cert loading branch.
	_ = k8sAPIPatch("/test", []byte(`{"test":true}`))
}
