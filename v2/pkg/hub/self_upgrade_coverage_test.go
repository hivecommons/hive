package hub

import (
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
	k8sCACertPath = filepath.Join(dir, "ca.crt") // absent -> InsecureSkipVerify branch
	k8sNamespacePath = nsPath
	t.Cleanup(func() {
		k8sAPIServer, k8sTokenPath, k8sCACertPath, k8sNamespacePath = oldServer, oldToken, oldCA, oldNS
	})
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

	if err := SwitchImageSelf(slog.Default(), "ghcr.io/kubestellar/hive:dev-latest"); err != nil {
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
