package spoke

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// resetSelfImageCache clears the memoised deployment image so each test starts
// from a cold cache and restores the previous state afterwards.
func resetSelfImageCache(t *testing.T) {
	t.Helper()
	selfImageMu.Lock()
	pc, pf, pa := selfImageCached, selfImageFetched, selfImageAttempted
	selfImageCached, selfImageFetched, selfImageAttempted = "", time.Time{}, false
	selfImageMu.Unlock()
	t.Cleanup(func() {
		selfImageMu.Lock()
		selfImageCached, selfImageFetched, selfImageAttempted = pc, pf, pa
		selfImageMu.Unlock()
	})
}

// deploymentJSON renders a Deployment carrying one container image.
func deploymentJSON(image string) string {
	return fmt.Sprintf(
		`{"spec":{"template":{"spec":{"containers":[{"name":"hub","image":%q}]}}}}`, image)
}

// ---- selfDeploymentImage ----

func TestSelfDeploymentImageReadsContainerImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(deploymentJSON("ghcr.io/hivecommons/hive-hub:abc123")))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	got, err := selfDeploymentImage()
	if err != nil {
		t.Fatalf("selfDeploymentImage: %v", err)
	}
	if got != "ghcr.io/hivecommons/hive-hub:abc123" {
		t.Fatalf("image = %q", got)
	}
}

// The namespace comes from the mounted ServiceAccount; without it the hub
// cannot address its own Deployment and must error rather than guess.
//
// The stub deliberately accepts everything and records the paths it is asked
// for. An earlier version had the mock reject a "/namespaces//" path, which
// meant the test still passed with the production guard deleted — it was
// asserting the stub's behaviour. Asserting that no request is issued at all
// pins the guard itself.
func TestSelfDeploymentImageRequiresNamespace(t *testing.T) {
	// The server must NOT be the thing that rejects an empty namespace.
	// Previously it returned 400 for a "/namespaces//" path, so this test still
	// passed when the production empty-namespace guard was deleted — it was
	// asserting the fake server's behaviour, not the code under test. Here the
	// server accepts everything and records what it was asked for, so the only
	// way to get an error is for selfDeploymentImage to refuse locally, and the
	// assertions below prove no request was ever issued.
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.Write([]byte(deploymentJSON("x")))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	t.Run("missing namespace file", func(t *testing.T) {
		old := k8sNamespacePath
		k8sNamespacePath = filepath.Join(t.TempDir(), "absent")
		t.Cleanup(func() { k8sNamespacePath = old })
		gotPaths = nil
		if _, err := selfDeploymentImage(); err == nil {
			t.Fatal("a missing namespace file must be an error")
		}
		if len(gotPaths) != 0 {
			t.Fatalf("must not call the API without a namespace, but requested %v", gotPaths)
		}
	})

	t.Run("empty namespace file", func(t *testing.T) {
		old := k8sNamespacePath
		p := filepath.Join(t.TempDir(), "namespace")
		if err := os.WriteFile(p, []byte("   \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		k8sNamespacePath = p
		t.Cleanup(func() { k8sNamespacePath = old })
		gotPaths = nil
		if _, err := selfDeploymentImage(); err == nil {
			t.Fatal("an empty namespace must be an error, not an empty API path")
		}
		// The specific failure this pins: building "/namespaces//deployments/..."
		// and asking the API server about it. That path can match an unintended
		// object, so the refusal has to happen before any request goes out.
		if len(gotPaths) != 0 {
			t.Fatalf("must reject an empty namespace locally, but requested %v", gotPaths)
		}
	})
}

// A Deployment with no containers must error rather than panic on index 0.
func TestSelfDeploymentImageRejectsContainerlessDeployment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if _, err := selfDeploymentImage(); err == nil {
		t.Fatal("a Deployment with no containers must be an error")
	}
}

func TestSelfDeploymentImageRejectsMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not json"))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if _, err := selfDeploymentImage(); err == nil {
		t.Fatal("malformed Deployment JSON must be an error")
	}
}

func TestSelfDeploymentImagePropagatesAPIFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if _, err := selfDeploymentImage(); err == nil {
		t.Fatal("an API failure must propagate")
	}
}

// ---- SelfDeploymentImage (memoised wrapper) ----

// The result is cached so the hub does not GET its own Deployment on every
// dashboard poll.
func TestSelfDeploymentImageIsMemoised(t *testing.T) {
	resetSelfImageCache(t)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(deploymentJSON("ghcr.io/hivecommons/hive-hub:cached")))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	first := SelfDeploymentImage()
	second := SelfDeploymentImage()
	if first != "ghcr.io/hivecommons/hive-hub:cached" || second != first {
		t.Fatalf("image = %q / %q", first, second)
	}
	if calls != 1 {
		t.Fatalf("want 1 API call thanks to the cache, got %d", calls)
	}
}

// A failed lookup must be cached as "" rather than retried on every call —
// otherwise a broken RBAC grant produces an API request per dashboard poll.
// It must also never return a stale-but-wrong image.
func TestSelfDeploymentImageCachesFailureAsEmpty(t *testing.T) {
	resetSelfImageCache(t)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if got := SelfDeploymentImage(); got != "" {
		t.Fatalf("a failed lookup must yield an empty image, got %q", got)
	}
	if got := SelfDeploymentImage(); got != "" {
		t.Fatalf("second call = %q", got)
	}
	if calls != 1 {
		t.Fatalf("a failure must be cached too, got %d API calls", calls)
	}
}
