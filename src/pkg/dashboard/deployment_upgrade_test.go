package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestDetectDeploymentExplicitRuntimes(t *testing.T) {
	clearDeploymentEnv(t)
	helper := fakeUpgradeHelper(t)
	tests := []struct {
		name      string
		cfg       config.DeploymentConfig
		runtime   string
		mode      string
		supported bool
		action    string
	}{
		{"kubernetes", config.DeploymentConfig{Runtime: "kubernetes"}, deploymentRuntimeKubernetes, "", true, "hub"},
		{"compose", config.DeploymentConfig{Runtime: "docker-compose", UpgradeHelper: helper}, deploymentRuntimeDockerCompose, "", true, "docker-compose"},
		{"podman rootless", config.DeploymentConfig{Runtime: "podman-quadlet", PodmanMode: "rootless", UpgradeHelper: helper}, deploymentRuntimePodmanQuadlet, podmanModeRootless, true, "podman-quadlet"},
		{"podman rootful", config.DeploymentConfig{Runtime: "podman-quadlet", PodmanMode: "rootful", UpgradeHelper: helper}, deploymentRuntimePodmanQuadlet, podmanModeRootful, true, "podman-quadlet"},
		{"podman uncertain mode", config.DeploymentConfig{Runtime: "podman-quadlet", UpgradeHelper: helper}, deploymentRuntimePodmanQuadlet, podmanModeUnknown, false, ""},
		{"compose missing helper", config.DeploymentConfig{Runtime: "docker-compose"}, deploymentRuntimeDockerCompose, "", false, ""},
		{"invalid", config.DeploymentConfig{Runtime: "lxd"}, deploymentRuntimeUnknown, "", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(0, slog.Default())
			srv.deps = &Dependencies{Config: &config.Config{Deployment: tc.cfg}, Logger: slog.Default()}
			got := srv.detectDeployment()
			if got.Runtime != tc.runtime || got.PodmanMode != tc.mode || got.UpgradeSupported != tc.supported || got.UpgradeAction != tc.action {
				t.Fatalf("detectDeployment() = %+v, want runtime=%s mode=%s supported=%v action=%s", got, tc.runtime, tc.mode, tc.supported, tc.action)
			}
			if !tc.supported && got.Reason == "" {
				t.Fatal("unsupported/unknown detection must include an honest reason")
			}
		})
	}
}

func TestDetectDeploymentFallsBackToKubernetesOnlyWithServiceAccountEvidence(t *testing.T) {
	clearDeploymentEnv(t)
	orig := kubernetesServiceAccountNamespacePath
	t.Cleanup(func() { kubernetesServiceAccountNamespacePath = orig })

	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: &config.Config{
		Hub:    config.HubConfig{URL: "https://hub.example"},
		HiveID: "hive-1",
	}, Logger: slog.Default()}

	kubernetesServiceAccountNamespacePath = filepath.Join(t.TempDir(), "missing")
	if got := srv.detectDeployment(); got.Runtime != deploymentRuntimeUnknown || got.UpgradeSupported {
		t.Fatalf("detectDeployment without service account evidence = %+v, want unknown unsupported", got)
	}

	dir := t.TempDir()
	kubernetesServiceAccountNamespacePath = filepath.Join(dir, "namespace")
	if err := os.WriteFile(kubernetesServiceAccountNamespacePath, []byte("default"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := srv.detectDeployment(); got.Runtime != deploymentRuntimeKubernetes || !got.UpgradeSupported || got.UpgradeAction != "hub" {
		t.Fatalf("detectDeployment with service account evidence = %+v, want kubernetes hub", got)
	}
}

func TestHandleVersionExposesDeploymentRuntime(t *testing.T) {
	clearDeploymentEnv(t)
	helper := fakeUpgradeHelper(t)
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: &config.Config{
		Deployment: config.DeploymentConfig{Runtime: "podman-quadlet", PodmanMode: "rootful", UpgradeHelper: helper},
	}, Logger: slog.Default()}

	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	w := httptest.NewRecorder()
	srv.handleVersion(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var body struct {
		Deployment deploymentInfo `json:"deployment"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Deployment.Runtime != deploymentRuntimePodmanQuadlet || body.Deployment.PodmanMode != podmanModeRootful || !body.Deployment.UpgradeSupported {
		t.Fatalf("deployment in /api/version = %+v, want supported rootful podman-quadlet", body.Deployment)
	}
}

func TestStandaloneSelfUpgradeDispatchesToHelperByRuntime(t *testing.T) {
	clearDeploymentEnv(t)
	for _, tc := range []struct {
		name string
		dep  config.DeploymentConfig
		want string
	}{
		{"compose", config.DeploymentConfig{Runtime: "docker-compose"}, "upgrade --runtime docker-compose --ref ghcr.io/hivecommons/hive:abcdef1"},
		{"podman rootless", config.DeploymentConfig{Runtime: "podman-quadlet", PodmanMode: "rootless"}, "upgrade --runtime podman-quadlet --ref ghcr.io/hivecommons/hive:abcdef1 --podman-mode rootless"},
		{"podman rootful", config.DeploymentConfig{Runtime: "podman-quadlet", PodmanMode: "rootful"}, "upgrade --runtime podman-quadlet --ref ghcr.io/hivecommons/hive:abcdef1 --podman-mode rootful"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsPath := filepath.Join(t.TempDir(), "args")
			helper := fakeUpgradeHelperRecording(t, argsPath)
			tc.dep.UpgradeHelper = helper
			srv := NewServer(0, slog.Default())
			srv.deps = &Dependencies{Config: &config.Config{
				Deployment: tc.dep,
				Dashboard:  config.DashboardConfig{AuthToken: "token"},
			}, Logger: slog.Default()}
			req := httptest.NewRequest(http.MethodPost, "/api/self-upgrade", strings.NewReader(`{"target":"abcdef1234567890"}`))
			markOwnerRequest(req)
			w := httptest.NewRecorder()
			srv.handleSelfUpgrade(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
			}
			data, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(data)); got != tc.want {
				t.Fatalf("helper args = %q, want %q", got, tc.want)
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["runtime"] != tc.dep.Runtime {
				t.Fatalf("response runtime = %q, want %q", body["runtime"], tc.dep.Runtime)
			}
		})
	}
}

func TestStandaloneSelfUpgradeRejectsUnsafeOrUncertainInputs(t *testing.T) {
	clearDeploymentEnv(t)
	helper := fakeUpgradeHelper(t)
	tests := []struct {
		name string
		dep  config.DeploymentConfig
		body string
		want int
	}{
		{"unknown runtime", config.DeploymentConfig{}, `{"target":"abcdef1"}`, http.StatusConflict},
		{"podman missing mode", config.DeploymentConfig{Runtime: "podman-quadlet", UpgradeHelper: helper}, `{"target":"abcdef1"}`, http.StatusConflict},
		{"unsafe target", config.DeploymentConfig{Runtime: "docker-compose", UpgradeHelper: helper}, `{"target":"abcdef1;reboot"}`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(0, slog.Default())
			srv.deps = &Dependencies{Config: &config.Config{Deployment: tc.dep}, Logger: slog.Default()}
			req := httptest.NewRequest(http.MethodPost, "/api/self-upgrade", strings.NewReader(tc.body))
			markOwnerRequest(req)
			w := httptest.NewRecorder()
			srv.handleSelfUpgrade(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestStandaloneSelfUpgradeStillRequiresOwner(t *testing.T) {
	clearDeploymentEnv(t)
	argsPath := filepath.Join(t.TempDir(), "args")
	helper := fakeUpgradeHelperRecording(t, argsPath)
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: &config.Config{
		Deployment: config.DeploymentConfig{Runtime: "docker-compose", UpgradeHelper: helper},
	}, Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPost, "/api/self-upgrade", strings.NewReader(`{"target":"abcdef1"}`))
	w := httptest.NewRecorder()
	srv.handleSelfUpgrade(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
		t.Fatalf("helper ran for non-owner request; stat err=%v", err)
	}
}

func fakeUpgradeHelper(t *testing.T) string {
	return fakeUpgradeHelperRecording(t, filepath.Join(t.TempDir(), "args"))
}

func clearDeploymentEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HIVE_DEPLOYMENT_RUNTIME", "")
	t.Setenv("HIVE_DEPLOYMENT_PODMAN_MODE", "")
	t.Setenv("HIVE_DASHBOARD_UPGRADE_HELPER", "")
}

func fakeUpgradeHelperRecording(t *testing.T, argsPath string) string {
	t.Helper()
	helper := filepath.Join(t.TempDir(), "helper.sh")
	script := "#!/bin/sh\nprintf '%s' \"$*\" > " + shellQuote(argsPath) + "\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return helper
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
