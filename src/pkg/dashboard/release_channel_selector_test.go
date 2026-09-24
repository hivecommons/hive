package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestReleaseChannelSwitchRelaysIntentAndReportsPending(t *testing.T) {
	const token = "spoke-dashboard-token"
	var sawPath, sawBody, sawRole, sawProof string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawRole = r.Header.Get("X-Hive-Role")
		sawProof = r.Header.Get(proxyAuthHeader)
		body, _ := io.ReadAll(r.Body)
		sawBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"switching","branch":"candidate","via":"heartbeat"}`))
	}))
	defer hub.Close()

	oldImage := selfDeploymentImageForDashboard
	selfDeploymentImageForDashboard = func() string { return "ghcr.io/hivecommons/hive:stable" }
	setPendingReleaseChannel("")
	t.Cleanup(func() { selfDeploymentImageForDashboard = oldImage; setPendingReleaseChannel("") })

	srv := NewServerWithAuth(0, token, slog.Default())
	srv.deps = &Dependencies{Config: &config.Config{Hub: config.HubConfig{URL: hub.URL}, HiveID: "hosted-test", Dashboard: config.DashboardConfig{AuthToken: token}}, Logger: slog.Default()}
	handler := srv.authenticate(http.HandlerFunc(srv.handleReleaseChannelSwitch))
	req := httptest.NewRequest(http.MethodPost, "/api/release-channel", strings.NewReader(`{"channel":"candidate"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Internal", token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if sawPath != "/api/saas/hives/hosted-test/switch-branch" {
		t.Fatalf("hub path = %q", sawPath)
	}
	if sawBody != `{"branch":"candidate"}` {
		t.Fatalf("hub body = %q", sawBody)
	}
	if sawRole != "owner" || sawProof != token {
		t.Fatalf("hub auth headers role=%q proof=%q", sawRole, sawProof)
	}
	var out struct {
		ReleaseStatus SpokeReleaseStatus `json:"releaseStatus"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.ReleaseStatus.Channel.Channel != "stable" || out.ReleaseStatus.Channel.PendingChannel != "candidate" {
		t.Fatalf("release status channel=%q pending=%q, want stable pending candidate", out.ReleaseStatus.Channel.Channel, out.ReleaseStatus.Channel.PendingChannel)
	}
}

func TestReleaseChannelSwitchUsesFileSourcedConfigTokenForHubProof(t *testing.T) {
	const token = "file-sourced-dashboard-token"
	var sawProof string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawProof = r.Header.Get(proxyAuthHeader)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"switching","branch":"candidate","via":"heartbeat"}`))
	}))
	defer hub.Close()

	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "dashboard-token")
	if err := os.WriteFile(tokenPath, []byte(" "+token+"\n"), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}
	t.Setenv("DASHBOARD_AUTH_TOKEN", "")
	t.Setenv("HIVE_DASHBOARD_TOKEN", "")
	t.Setenv("DASHBOARD_AUTH_TOKEN_FILE", tokenPath)

	configPath := filepath.Join(dir, "hive.yaml")
	configYAML := `
project:
  org: acme
  repos: [repo-a]
github:
  token: ghp_test
agents:
  worker:
    backend: claude
    enabled: true
hive_id: hosted-file-token
hub:
  url: ` + hub.URL + `
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Dashboard.AuthToken != token {
		t.Fatalf("Dashboard.AuthToken = %q, want file token", cfg.Dashboard.AuthToken)
	}

	oldImage := selfDeploymentImageForDashboard
	selfDeploymentImageForDashboard = func() string { return "ghcr.io/hivecommons/hive:stable" }
	setPendingReleaseChannel("")
	t.Cleanup(func() { selfDeploymentImageForDashboard = oldImage; setPendingReleaseChannel("") })

	srv := NewServerWithAuth(0, "", slog.Default())
	srv.deps = &Dependencies{Config: cfg, Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPost, "/api/release-channel", strings.NewReader(`{"channel":"candidate"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	req.Header.Set(ownerRoleVerifiedHeader, "true")
	w := httptest.NewRecorder()
	srv.handleReleaseChannelSwitch(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if sawProof != token {
		t.Fatalf("hub proof = %q, want file-sourced token", sawProof)
	}
}

func TestReleaseChannelSelectorUnavailableWhenImageUnresolved(t *testing.T) {
	oldImage := selfDeploymentImageForDashboard
	oldVersionSource := versionImageSource
	selfDeploymentImageForDashboard = func() string { return "ghcr.io/hivecommons/hive:v4-latest" }
	versionImageSource = selfDeploymentImageForDashboard
	setPendingReleaseChannel("")
	t.Cleanup(func() {
		selfDeploymentImageForDashboard = oldImage
		versionImageSource = oldVersionSource
		setPendingReleaseChannel("")
	})

	srv := NewServerWithAuth(0, "token", slog.Default())
	srv.RegisterAPI(&Dependencies{Config: &config.Config{Hub: config.HubConfig{URL: "https://hub.example"}, HiveID: "hosted-test"}, Logger: slog.Default()})
	rec := doGet(srv, "/api/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/version status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		ReleaseStatus SpokeReleaseStatus `json:"releaseStatus"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ReleaseStatus.Channel.Resolved {
		t.Fatalf("branch tag unexpectedly resolved as channel: %+v", out.ReleaseStatus.Channel)
	}
	if out.ReleaseStatus.Channel.SelectorEnabled {
		t.Fatalf("selector enabled for unresolved branch tag: %+v", out.ReleaseStatus.Channel)
	}
	if !strings.Contains(out.ReleaseStatus.Channel.SelectorDetail, "branch-tracking") {
		t.Fatalf("selector detail = %q, want honest branch-tracking explanation", out.ReleaseStatus.Channel.SelectorDetail)
	}
}

func TestReleaseChannelSelectorUnavailableNamesPodmanSelfHosted(t *testing.T) {
	oldVersionSource := versionImageSource
	oldImageSource := selfDeploymentImageSourceForDashboard
	versionImageSource = func() string { return "ghcr.io/hivecommons/hive:candidate" }
	selfDeploymentImageSourceForDashboard = func() string { return "podman-env" }
	t.Cleanup(func() { versionImageSource = oldVersionSource; selfDeploymentImageSourceForDashboard = oldImageSource })

	srv := NewServerWithAuth(0, "token", slog.Default())
	srv.RegisterAPI(&Dependencies{Config: &config.Config{Hub: config.HubConfig{URL: "https://hub.example"}, HiveID: "hosted-test"}, Logger: slog.Default()})
	rec := doGet(srv, "/api/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/version status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		ReleaseStatus SpokeReleaseStatus `json:"releaseStatus"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ReleaseStatus.Channel.SelectorEnabled {
		t.Fatalf("selector enabled for podman self-hosted: %+v", out.ReleaseStatus.Channel)
	}
	if out.ReleaseStatus.Channel.SelectorReason != "podman-self-hosted" {
		t.Fatalf("selector reason = %q", out.ReleaseStatus.Channel.SelectorReason)
	}
	if !strings.Contains(out.ReleaseStatus.Channel.SelectorDetail, "self-hosted Podman spoke") ||
		!strings.Contains(out.ReleaseStatus.Channel.SelectorDetail, "Image=") {
		t.Fatalf("selector detail = %q", out.ReleaseStatus.Channel.SelectorDetail)
	}
}

func TestVersionReportsPodmanRegistryTracking(t *testing.T) {
	oldVersionSource := versionImageSource
	oldImageSource := selfDeploymentImageSourceForDashboard
	versionImageSource = func() string { return "ghcr.io/hivecommons/hive:candidate" }
	selfDeploymentImageSourceForDashboard = func() string { return "podman-env" }
	t.Setenv("HIVE_SELF_IMAGE_TRACKING", "registry")
	t.Cleanup(func() { versionImageSource = oldVersionSource; selfDeploymentImageSourceForDashboard = oldImageSource })

	srv := NewServerWithAuth(0, "token", slog.Default())
	srv.RegisterAPI(&Dependencies{Config: &config.Config{}, Logger: slog.Default()})
	rec := doGet(srv, "/api/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/version status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Tracking      string             `json:"tracking"`
		Channel       string             `json:"channel"`
		ReleaseStatus SpokeReleaseStatus `json:"releaseStatus"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Tracking != "registry" {
		t.Fatalf("tracking = %q, want registry", out.Tracking)
	}
	if out.Channel != "candidate" || !out.ReleaseStatus.Channel.Resolved {
		t.Fatalf("channel response = %q release=%+v, want candidate resolved", out.Channel, out.ReleaseStatus.Channel)
	}
}
