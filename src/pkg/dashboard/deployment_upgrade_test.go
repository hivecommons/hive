package dashboard

import (
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestDetectDeploymentExplicitRuntimes(t *testing.T) {
	clearDeploymentEnv(t)
	helper := fakeUpgradeHelper(t)
	requestDir := t.TempDir()
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
		{"podman rootless", config.DeploymentConfig{Runtime: "podman-quadlet", PodmanMode: "rootless", UpgradeRequestDir: requestDir}, deploymentRuntimePodmanQuadlet, podmanModeRootless, true, "podman-quadlet-request"},
		{"podman rootful", config.DeploymentConfig{Runtime: "podman-quadlet", PodmanMode: "rootful", UpgradeRequestDir: requestDir}, deploymentRuntimePodmanQuadlet, podmanModeRootful, true, "podman-quadlet-request"},
		{"podman helper alone", config.DeploymentConfig{Runtime: "podman-quadlet", PodmanMode: "rootless", UpgradeHelper: helper}, deploymentRuntimePodmanQuadlet, podmanModeRootless, false, ""},
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

func TestDetectDeploymentPodmanMissingHelperNamesHostCommand(t *testing.T) {
	clearDeploymentEnv(t)
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: &config.Config{Deployment: config.DeploymentConfig{Runtime: "podman-quadlet", PodmanMode: "rootless", UpgradeHelper: filepath.Join(t.TempDir(), "missing")}}, Logger: slog.Default()}
	got := srv.detectDeployment()
	if got.UpgradeSupported || !strings.Contains(got.Reason, "systemctl --user start podman-auto-update.service") {
		t.Fatalf("detectDeployment() = %+v, want unsupported with host command in reason", got)
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
	requestDir := t.TempDir()
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: &config.Config{
		Deployment: config.DeploymentConfig{Runtime: "podman-quadlet", PodmanMode: "rootful", UpgradeRequestDir: requestDir},
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

func TestKubernetesSelfUpgradePrecheckRejectsMissingPatchRBAC(t *testing.T) {
	clearDeploymentEnv(t)
	dir := t.TempDir()
	nsPath := filepath.Join(dir, "namespace")
	tokenPath := filepath.Join(dir, "token")
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(nsPath, []byte("hive-ns"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"kind":"Deployment"}`))
		case http.MethodPatch:
			http.Error(w, "deployments.apps \"hive\" is forbidden", http.StatusForbidden)
		default:
			t.Errorf("unexpected method %s", r.Method)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	oldNS, oldToken, oldCA, oldAPI := kubernetesServiceAccountNamespacePath, kubernetesServiceAccountTokenPath, kubernetesServiceAccountCACertPath, kubernetesAPIServerURL
	kubernetesServiceAccountNamespacePath = nsPath
	kubernetesServiceAccountTokenPath = tokenPath
	kubernetesServiceAccountCACertPath = caPath
	kubernetesAPIServerURL = api.URL
	t.Cleanup(func() {
		kubernetesServiceAccountNamespacePath = oldNS
		kubernetesServiceAccountTokenPath = oldToken
		kubernetesServiceAccountCACertPath = oldCA
		kubernetesAPIServerURL = oldAPI
	})

	srv := NewServer(0, slog.Default())
	if err := srv.precheckKubernetesSelfUpgrade(""); err == nil || !strings.Contains(err.Error(), "cannot patch deployment/hive") {
		t.Fatalf("precheck error = %v, want patch RBAC refusal", err)
	}
}

func TestHandleSelfUpgradePersistsHubAcceptedState(t *testing.T) {
	const hiveID = "hosted-test-hive"
	statePath := filepath.Join(t.TempDir(), "dashboard-upgrade-state.json")
	oldStatePath := dashboardUpgradeStatePath
	dashboardUpgradeStatePath = statePath
	t.Cleanup(func() { dashboardUpgradeStatePath = oldStatePath })

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"upgrading","mode":"heartbeat"}`))
	}))
	defer hub.Close()

	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: &config.Config{
		Hub:        config.HubConfig{URL: hub.URL},
		HiveID:     hiveID,
		Dashboard:  config.DashboardConfig{AuthToken: "spoke-token"},
		Deployment: config.DeploymentConfig{Runtime: "kubernetes"},
	}, Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPost, "/api/self-upgrade", nil)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleSelfUpgrade(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	st := readDashboardUpgradeState()
	if st == nil || st.State != dashboardUpgradeStateStarted || st.StartedAt.IsZero() {
		t.Fatalf("dashboard upgrade state = %+v, want started with timestamp", st)
	}
}

func TestHandleSelfUpgradeQueuesUnpublishedTargetWithoutFailure(t *testing.T) {
	const target = "abc1234"
	statePath := filepath.Join(t.TempDir(), "dashboard-upgrade-state.json")
	oldStatePath := dashboardUpgradeStatePath
	dashboardUpgradeStatePath = statePath
	ghcrCacheMu.Lock()
	ghcrCacheResult[target] = false
	ghcrCacheExpiry[target] = time.Now().Add(time.Hour)
	ghcrCacheMu.Unlock()
	t.Cleanup(func() {
		dashboardUpgradeStatePath = oldStatePath
		ghcrCacheMu.Lock()
		delete(ghcrCacheResult, target)
		delete(ghcrCacheExpiry, target)
		ghcrCacheMu.Unlock()
	})

	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: &config.Config{
		Hub:        config.HubConfig{URL: "https://hub.example.test"},
		HiveID:     "hosted-test-hive",
		Dashboard:  config.DashboardConfig{AuthToken: "spoke-token"},
		Deployment: config.DeploymentConfig{Runtime: "kubernetes"},
	}, Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPost, "/api/self-upgrade?target="+target, nil)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleSelfUpgrade(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", w.Code, w.Body.String())
	}
	st := readDashboardUpgradeState()
	if st == nil || st.State != dashboardUpgradeStateQueued || st.Target != target {
		t.Fatalf("dashboard upgrade state = %+v, want queued target %s", st, target)
	}
	if attempt := upgradeAttemptFromDashboardState(st); attempt.State != upgradeAttemptInProgress || !strings.Contains(attempt.Detail, "queued — image building") {
		t.Fatalf("attempt = %+v, want neutral queued/building detail", attempt)
	}
}

func TestHandleVersionSurfacesPersistedManualUpgradeFailure(t *testing.T) {
	dir := t.TempDir()
	oldStatePath, oldMarkerPath, oldOutcomePath := dashboardUpgradeStatePath, upgradeMarkerPath, upgradeOutcomePath
	dashboardUpgradeStatePath = filepath.Join(dir, "dashboard-upgrade-state.json")
	upgradeMarkerPath = filepath.Join(dir, "missing-marker")
	upgradeOutcomePath = filepath.Join(dir, "missing-outcome")
	t.Cleanup(func() {
		dashboardUpgradeStatePath = oldStatePath
		upgradeMarkerPath = oldMarkerPath
		upgradeOutcomePath = oldOutcomePath
	})
	writeDashboardUpgradeState(dashboardUpgradeState{
		State:     dashboardUpgradeStateFailed,
		Target:    "abc1234",
		StartedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Reason:    "service account cannot patch deployment/hive",
	})

	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: &config.Config{}, Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	w := httptest.NewRecorder()
	srv.handleVersion(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var body struct {
		ManualUpgrade dashboardUpgradeState `json:"manualUpgrade"`
		ReleaseStatus SpokeReleaseStatus    `json:"releaseStatus"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ManualUpgrade.State != dashboardUpgradeStateFailed {
		t.Fatalf("manualUpgrade.state = %q, want failed", body.ManualUpgrade.State)
	}
	if body.ReleaseStatus.Attempt.State != upgradeAttemptFailed || !strings.Contains(body.ReleaseStatus.Attempt.Detail, "cannot patch") {
		t.Fatalf("release attempt = %+v, want failed patch detail", body.ReleaseStatus.Attempt)
	}
}

func fakeUpgradeHelper(t *testing.T) string {
	return fakeUpgradeHelperRecording(t, filepath.Join(t.TempDir(), "args"))
}

func TestPodmanUpgradeRequestDirectoryDetection(t *testing.T) {
	clearDeploymentEnv(t)
	writable := t.TempDir()
	missing := filepath.Join(t.TempDir(), "missing")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{podmanModeRootless, podmanModeRootful} {
		for _, dir := range []string{"", missing, file, writable} {
			srv := NewServer(0, slog.Default())
			srv.deps = &Dependencies{Config: &config.Config{Deployment: config.DeploymentConfig{
				Runtime: deploymentRuntimePodmanQuadlet, PodmanMode: mode, UpgradeRequestDir: dir,
			}}}
			got := srv.detectDeployment()
			if dir == writable {
				if !got.UpgradeSupported || got.UpgradeAction != "podman-quadlet-request" || got.Reason != "" {
					t.Fatalf("writable directory: %+v", got)
				}
			} else {
				want := "standalone upgrade helper is not available inside the container; upgrade from the host with `" + podmanHostUpgradeCommand(mode) + "` (as the hive user when rootless)"
				if got.UpgradeSupported || got.Reason != want {
					t.Fatalf("directory %q: %+v, want reason %q", dir, got, want)
				}
			}
		}
	}
	entries, err := os.ReadDir(writable)
	if err != nil || len(entries) != 0 {
		t.Fatalf("probe left files: %v, %v", entries, err)
	}
	t.Setenv("HIVE_DEPLOYMENT_UPGRADE_REQUEST_DIR", writable)
	if got := upgradeRequestDir(&config.Config{Deployment: config.DeploymentConfig{UpgradeRequestDir: missing}}); got != writable {
		t.Fatalf("env precedence: %q", got)
	}
	t.Run("unwritable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permission bits")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		t.Setenv("HIVE_DEPLOYMENT_UPGRADE_REQUEST_DIR", dir)
		srv := NewServer(0, slog.Default())
		srv.deps = &Dependencies{Config: &config.Config{Deployment: config.DeploymentConfig{
			Runtime: deploymentRuntimePodmanQuadlet, PodmanMode: podmanModeRootless,
		}}}
		if got := srv.detectDeployment(); got.UpgradeSupported || !strings.Contains(got.Reason, podmanHostUpgradeCommand(podmanModeRootless)) {
			t.Fatalf("unwritable directory: %+v", got)
		}
	})
}

func TestPodmanSelfUpgradeWritesRequestWithoutExecutingHelper(t *testing.T) {
	clearDeploymentEnv(t)
	for _, mode := range []string{podmanModeRootless, podmanModeRootful} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			argsPath := filepath.Join(t.TempDir(), "args")
			srv := NewServer(0, slog.Default())
			srv.deps = &Dependencies{Config: &config.Config{Deployment: config.DeploymentConfig{
				Runtime: deploymentRuntimePodmanQuadlet, PodmanMode: mode,
				UpgradeRequestDir: dir, UpgradeHelper: fakeUpgradeHelperRecording(t, argsPath),
			}}, Logger: slog.Default()}
			for _, target := range []string{"abcdef1;reboot", "abcdef1234567890"} {
				req := httptest.NewRequest(http.MethodPost, "/api/self-upgrade", strings.NewReader(`{"target":"`+target+`"}`))
				markOwnerRequest(req)
				req.Header.Set("X-Hive-User", "owner-test")
				w := httptest.NewRecorder()
				srv.handleSelfUpgrade(w, req)
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				if target == "abcdef1;reboot" {
					if w.Code != http.StatusBadRequest || len(entries) != 0 {
						t.Fatalf("invalid target: status=%d files=%v", w.Code, entries)
					}
					continue
				}
				if w.Code != http.StatusOK || len(entries) != 1 {
					t.Fatalf("valid target: status=%d files=%v body=%s", w.Code, entries, w.Body.String())
				}
				var response map[string]string
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response["status"] != "accepted" || !strings.Contains(response["message"], "not completed") {
					t.Fatalf("response claims completion: %v", response)
				}
				path := filepath.Join(dir, entries[0].Name())
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var payload upgradeRequest
				if err := json.Unmarshal(data, &payload); err != nil {
					t.Fatal(err)
				}
				when, err := time.Parse(time.RFC3339, payload.RequestedAt)
				if err != nil || time.Since(when) > time.Minute || payload.TargetRef != "ghcr.io/hivecommons/hive:abcdef1" || payload.Requester != "owner-test" {
					t.Fatalf("request = %+v, timestamp error=%v", payload, err)
				}
				st, err := os.Stat(path)
				if err != nil || st.Mode().Perm() != 0o600 || !strings.HasSuffix(path, ".json") {
					t.Fatalf("published request permissions/name: %v, %v", st, err)
				}
			}
			if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
				t.Fatalf("Podman executed helper: %v", err)
			}
			// Even a stale caller using the old action must never invoke a helper.
			req := httptest.NewRequest(http.MethodPost, "/?target=abcdef1", nil)
			if err := srv.runStandaloneUpgrade(req, deploymentInfo{Runtime: deploymentRuntimePodmanQuadlet, UpgradeAction: "podman-quadlet"}); err == nil {
				t.Fatal("old Podman helper action accepted")
			}
			if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
				t.Fatalf("stale action executed helper: %v", err)
			}
		})
	}
}

func TestUpgradeRequestsPublishedAtomically(t *testing.T) {
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			if err := writeUpgradeRequest(dir, "ghcr.io/hivecommons/hive:abcdef1", strings.Repeat("owner", 1024)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	// Always join the writer, including on a reader assertion failure.
	joined := false
	defer func() {
		if !joined {
			<-done
		}
	}()
	checkPublished := func() {
		t.Helper()
		paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil || !json.Valid(data) {
				t.Fatalf("partial published request %s: %q, %v", path, data, err)
			}
		}
	}
	for !joined {
		checkPublished()
		select {
		case err := <-done:
			joined = true
			if err != nil {
				t.Fatal(err)
			}
		default:
		}
	}
	checkPublished()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 100 {
		t.Fatalf("requests lost or temp files left: count=%d, %v", len(entries), err)
	}
	if err := writeUpgradeRequest(filepath.Join(dir, "missing"), "ghcr.io/hivecommons/hive:abcdef1", "owner"); err == nil {
		t.Fatal("missing directory accepted")
	}
}

func clearDeploymentEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HIVE_DEPLOYMENT_RUNTIME", "")
	t.Setenv("HIVE_DEPLOYMENT_PODMAN_MODE", "")
	t.Setenv("HIVE_DASHBOARD_UPGRADE_HELPER", "")
	t.Setenv("HIVE_DEPLOYMENT_UPGRADE_REQUEST_DIR", "")
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
