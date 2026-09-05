package spoke

import (
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================
// self_upgrade.go + cluster_metrics.go k8s API helpers
//
// These call the in-cluster K8s API using package-level path/URL vars. Tests
// redirect those vars at a temp service-account dir + an httptest server so the
// happy and error paths run without a real cluster.
// ============================================================

// withFakeK8sAPI points k8sAPIServer/token/ca/namespace at a temp SA dir and the
// given httptest server, restoring the originals on cleanup.
func withFakeK8sAPI(t *testing.T, srv *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	nsPath := filepath.Join(dir, "namespace")
	if err := os.WriteFile(tokenPath, []byte("fake-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nsPath, []byte("hive-hosted-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldServer, oldToken, oldCA, oldNS := k8sAPIServer, k8sTokenPath, k8sCACertPath, k8sNamespacePath
	k8sAPIServer = srv.URL
	k8sTokenPath = tokenPath
	k8sCACertPath = filepath.Join(dir, "ca.crt")
	writeTestK8sCACert(t, k8sCACertPath)
	k8sNamespacePath = nsPath
	t.Cleanup(func() {
		k8sAPIServer, k8sTokenPath, k8sCACertPath, k8sNamespacePath = oldServer, oldToken, oldCA, oldNS
	})
}

func writeTestK8sCACert(t *testing.T, path string) {
	t.Helper()
	certSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer certSrv.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certSrv.Certificate().Raw})
	if err := os.WriteFile(path, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestK8sAPIGetSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer fake-token" {
			t.Errorf("missing/wrong auth header: %q", got)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	body, err := k8sAPIGet("/api/v1/nodes")
	if err != nil {
		t.Fatalf("k8sAPIGet: %v", err)
	}
	if !strings.Contains(string(body), "ok") {
		t.Errorf("unexpected body: %s", body)
	}
}

func TestK8sAPIGetNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("denied"))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if _, err := k8sAPIGet("/api/v1/nodes"); err == nil {
		t.Error("expected error on non-200")
	}
}

func TestK8sAPIGetMissingCAFailsClosed(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)
	if err := os.Remove(k8sCACertPath); err != nil {
		t.Fatal(err)
	}

	_, err := k8sAPIGet("/api/v1/nodes")
	if err == nil {
		t.Fatal("expected k8sAPIGet to fail when CA cert cannot be read")
	}
	if !strings.Contains(err.Error(), "refusing to connect with TLS verification disabled") {
		t.Fatalf("error = %v, want fail-closed CA message", err)
	}
	if hits != 0 {
		t.Fatalf("server was contacted %d times despite missing CA", hits)
	}
}

func TestK8sAPIGetInvalidCAFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server must not be contacted with invalid CA")
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)
	if err := os.WriteFile(k8sCACertPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := k8sAPIGet("/api/v1/nodes")
	if err == nil {
		t.Fatal("expected k8sAPIGet to fail when CA cert has no PEM certificates")
	}
	if !strings.Contains(err.Error(), "empty cert pool") {
		t.Fatalf("error = %v, want empty cert pool fail-closed message", err)
	}
}

func TestK8sAPIPatchMissingCAFailsClosed(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)
	if err := os.Remove(k8sCACertPath); err != nil {
		t.Fatal(err)
	}

	err := k8sAPIPatch("/x", []byte(`{}`))
	if err == nil {
		t.Fatal("expected k8sAPIPatch to fail when CA cert cannot be read")
	}
	if !strings.Contains(err.Error(), "refusing to connect with TLS verification disabled") {
		t.Fatalf("error = %v, want fail-closed CA message", err)
	}
	if hits != 0 {
		t.Fatalf("server was contacted %d times despite missing CA", hits)
	}
}

func TestK8sAPIPatchSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if err := k8sAPIPatch("/apis/apps/v1/namespaces/x/deployments/hive", []byte(`{}`)); err != nil {
		t.Fatalf("k8sAPIPatch: %v", err)
	}
}

func TestK8sAPIPatchNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if err := k8sAPIPatch("/x", []byte(`{}`)); err == nil {
		t.Error("expected error on non-200 patch")
	}
}

func TestK8sAPIPatchNoToken(t *testing.T) {
	oldToken := k8sTokenPath
	k8sTokenPath = filepath.Join(t.TempDir(), "missing")
	defer func() { k8sTokenPath = oldToken }()
	if err := k8sAPIPatch("/x", []byte(`{}`)); err == nil {
		t.Error("expected error when SA token missing")
	}
}

func TestRolloutRestartSelfSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if err := RolloutRestartSelf(slog.Default()); err != nil {
		t.Fatalf("RolloutRestartSelf: %v", err)
	}
}

func TestRolloutRestartSelfNoNamespace(t *testing.T) {
	oldNS := k8sNamespacePath
	k8sNamespacePath = filepath.Join(t.TempDir(), "missing")
	defer func() { k8sNamespacePath = oldNS }()
	if err := RolloutRestartSelf(slog.Default()); err == nil {
		t.Error("expected error when namespace file missing")
	}
}

func TestK8sDeploymentContainerNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive"}],"initContainers":[{"name":"init-x"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	names, err := k8sDeploymentContainerNames("/apis/apps/v1/namespaces/x/deployments/hive")
	if err != nil {
		t.Fatalf("k8sDeploymentContainerNames: %v", err)
	}
	if len(names.containers) != 1 || names.containers[0] != "hive" {
		t.Errorf("containers = %+v", names.containers)
	}
	if len(names.initContainers) != 1 || names.initContainers[0] != "init-x" {
		t.Errorf("initContainers = %+v", names.initContainers)
	}
}

func TestK8sDeploymentContainerNamesBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if _, err := k8sDeploymentContainerNames("/x"); err == nil {
		t.Error("expected error on bad JSON")
	}
}

func TestSwitchImageSelfSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive"}],"initContainers":[{"name":"init"}]}}}}`))
			return
		}
		// PATCH
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if err := SwitchImageSelf(slog.Default(), "ghcr.io/hivecommons/hive:dev-latest"); err != nil {
		t.Fatalf("SwitchImageSelf: %v", err)
	}
}

func TestSwitchImageSelfNoContainers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[],"initContainers":[]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if err := SwitchImageSelf(slog.Default(), "img"); err == nil {
		t.Error("expected error when deployment has no containers")
	}
}

func TestSwitchImageSelfNoNamespace(t *testing.T) {
	oldNS := k8sNamespacePath
	k8sNamespacePath = filepath.Join(t.TempDir(), "missing")
	defer func() { k8sNamespacePath = oldNS }()
	if err := SwitchImageSelf(slog.Default(), "img"); err == nil {
		t.Error("expected error when namespace file missing")
	}
}

// TestUpgradeSelfMutableTagForcesRollout is the regression test for the
// floating-tag no-op. A deployment on v2-latest used to take a bare rollout
// restart, which re-pulls the tag and lands on whatever CI last published —
// verified on the live fleet as fb37afe while the hub targeted 07ccca1. The
// upgrade must instead patch the pod template with the target SHA, so the
// rollout is forced AND the requested target is recorded on the deployment.
func TestUpgradeSelfMutableTagForcesRollout(t *testing.T) {
	const (
		currentImage = "ghcr.io/hivecommons/hive:v2-latest"
		targetSHA    = "07ccca1"
	)
	var patchBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			b, _ := io.ReadAll(r.Body)
			patchBody = string(b)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"` + currentImage + `"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), targetSHA)
	if err != nil {
		t.Fatalf("UpgradeSelfToSHA on a mutable tag: %v", err)
	}
	// The annotation patch rolls the deployment by itself; a bare restart on
	// top would be the very no-op this fix removes.
	if needsRestart {
		t.Error("a mutable-tag upgrade must roll via the template patch, not a bare restart")
	}
	if patchBody == "" {
		t.Fatal("expected the deployment to be patched, but no PATCH was issued")
	}
	if !strings.Contains(patchBody, selfUpgradeTargetAnnotation) {
		t.Errorf("patch must carry the target annotation %q, got: %s", selfUpgradeTargetAnnotation, patchBody)
	}
	if !strings.Contains(patchBody, targetSHA) {
		t.Errorf("patch must record the target SHA %q, got: %s", targetSHA, patchBody)
	}
	// The template, not the Deployment, must be annotated — only template
	// changes roll pods.
	if !strings.Contains(patchBody, `"template"`) {
		t.Errorf("annotation must land on the POD TEMPLATE, got: %s", patchBody)
	}
	// The floating tag must survive: pinning the image would defeat the
	// platform's deliberate preference for mutable tags.
	if strings.Contains(patchBody, `"image"`) {
		t.Errorf("a mutable-tag upgrade must NOT rewrite the image, got: %s", patchBody)
	}
}

// TestUpgradeSelfMutableTagPatchFailureIsReported guards that a failed patch
// surfaces as an error. Swallowing it would return needsRestart and reproduce
// the original silent no-op.
func TestUpgradeSelfMutableTagPatchFailureIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"forbidden"}`))
			return
		}
		_, _ = w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/hivecommons/hive:v2-latest"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if _, err := UpgradeSelfToSHA(slog.Default(), "07ccca1"); err == nil {
		t.Fatal("a rejected patch must be reported as an error, not swallowed")
	}
}

// TestUpgradeSelfPinnedStillPatchesImage guards that the pinned path (#2308) is
// unchanged by the mutable-tag fix: a SHA-pinned deployment must still have its
// image rewritten, since no annotation can move it onto new code.
func TestUpgradeSelfPinnedStillPatchesImage(t *testing.T) {
	var patchBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			b, _ := io.ReadAll(r.Body)
			patchBody = string(b)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/hivecommons/hive:c11643a"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	needsRestart, err := UpgradeSelfToSHA(slog.Default(), "07ccca1")
	if err != nil {
		t.Fatalf("UpgradeSelfToSHA on a pinned tag: %v", err)
	}
	if needsRestart {
		t.Error("a pinned upgrade rolls via the image patch")
	}
	if !strings.Contains(patchBody, "ghcr.io/hivecommons/hive:07ccca1") {
		t.Errorf("pinned upgrade must rewrite the image to the target, got: %s", patchBody)
	}
}
