package hub

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const adminNotifMaskedWebhook = "••••••••"

func writeAdminNotifConfig(t *testing.T, extra string) (*HubServer, string) {
	t.Helper()
	t.Setenv("HIVE_HUB_SECRET", "test-secret-not-a-real-credential")
	dir := t.TempDir()
	oldRuntime := config.RuntimeConfigFile
	oldOverlay := config.DashboardOverlayFile
	config.RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	config.DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	t.Cleanup(func() {
		config.RuntimeConfigFile = oldRuntime
		config.DashboardOverlayFile = oldOverlay
	})
	path := filepath.Join(dir, "hive.yaml")
	cfgYAML := `hive_id: admin-notif
project:
  org: test-org
github:
  token: test-token-not-real
hub:
  enabled: true
data:
  agents_dir: ` + filepath.Join(dir, "agents") + `
agents:
  worker:
    backend: claude
` + extra
	if err := os.WriteFile(path, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := NewHubServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)), "abc1234", "v5")
	srv.SetHubConfigPath(path, "")
	return srv, path
}

func adminNotifFeedInstalled(srv *HubServer) bool {
	srv.githubActivityMu.Lock()
	defer srv.githubActivityMu.Unlock()
	return srv.githubActivityFeed != nil
}

func adminNotifPut(srv *HubServer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/saas/admin/notifications", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handlePutAdminNotifications(rec, req)
	return rec
}

func adminNotifGet(srv *HubServer) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/saas/admin/notifications", nil)
	rec := httptest.NewRecorder()
	srv.handleGetAdminNotifications(rec, req)
	return rec
}

func TestValidateHubNotificationURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"empty", "", false},
		{"masked placeholder", adminNotifMaskedWebhook, false},
		{"https public", "https://discord.com/api/webhooks/1/abc", false},
		{"http rejected", "http://discord.com/api/webhooks/1/abc", true},
		{"no scheme rejected", "discord.com/hook", true},
		{"localhost", "https://localhost/hook", true},
		{"localhost uppercase", "https://LOCALHOST:8080/hook", true},
		{"loopback", "https://127.0.0.1/hook", true},
		{"10/8", "https://10.1.2.3/hook", true},
		{"172.16", "https://172.16.0.1/hook", true},
		{"172.17", "https://172.17.0.1/hook", true},
		{"172.18", "https://172.18.0.1/hook", true},
		{"172.19", "https://172.19.0.1/hook", true},
		{"172.20", "https://172.20.0.1/hook", true},
		{"172.21", "https://172.21.0.1/hook", true},
		{"172.22", "https://172.22.0.1/hook", true},
		{"172.23", "https://172.23.0.1/hook", true},
		{"172.24", "https://172.24.0.1/hook", true},
		{"172.25", "https://172.25.0.1/hook", true},
		{"172.26", "https://172.26.0.1/hook", true},
		{"172.27", "https://172.27.0.1/hook", true},
		{"172.28", "https://172.28.0.1/hook", true},
		{"172.29", "https://172.29.0.1/hook", true},
		{"172.30", "https://172.30.0.1/hook", true},
		{"172.31", "https://172.31.255.255/hook", true},
		{"172.15 allowed", "https://172.15.0.1/hook", false},
		{"172.32 allowed", "https://172.32.0.1/hook", false},
		{"192.168", "https://192.168.1.1/hook", true},
		{"link-local", "https://169.254.169.254/latest", true},
		{"ipv6 loopback", "https://[::1]/hook", true},
		{"unspecified", "https://0.0.0.0/hook", true},
		{"port stripped", "https://10.0.0.1:8443/hook", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHubNotificationURL(tt.url, "factoryWebhook")
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateHubNotificationURL(%q) err = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "factoryWebhook") {
				t.Fatalf("error %q should name the field", err)
			}
		})
	}
}

func TestSanitizeEventList(t *testing.T) {
	allowed := []string{"pr_opened", "pr_merged", "issue_opened"}
	got := sanitizeEventList([]string{" pr_merged ", "pr_opened", "pr_merged", "bogus", "", "issue_opened"}, allowed)
	want := []string{"pr_merged", "pr_opened", "issue_opened"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitizeEventList = %v, want %v", got, want)
	}
	if got := sanitizeEventList(nil, allowed); got == nil || len(got) != 0 {
		t.Fatalf("sanitizeEventList(nil) = %#v, want empty non-nil slice", got)
	}
}

func TestSanitizeStrings(t *testing.T) {
	got := sanitizeStrings([]string{" zeta ", "alpha", "", "  ", "alpha", "mid"})
	want := []string{"alpha", "mid", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitizeStrings = %v, want %v", got, want)
	}
	if got := sanitizeStrings(nil); got == nil || len(got) != 0 {
		t.Fatalf("sanitizeStrings(nil) = %#v, want empty non-nil slice", got)
	}
}

func TestBoolPtr(t *testing.T) {
	if p := boolPtr(true); p == nil || !*p {
		t.Fatalf("boolPtr(true) = %v", p)
	}
	if p := boolPtr(false); p == nil || *p {
		t.Fatalf("boolPtr(false) = %v", p)
	}
}

func TestGetAdminNotificationsMasksWebhookAndReportsScope(t *testing.T) {
	srv, _ := writeAdminNotifConfig(t, `notifications:
  discord:
    factory_webhook: https://discord.invalid/webhook/secret
  github_activity:
    enabled: true
    repos: [hive, docs]
    events: [pr_opened, pr_merged]
    filter_bots: false
`)
	rec := adminNotifGet(srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if strings.Contains(rec.Body.String(), "discord.invalid") {
		t.Fatalf("GET leaked the webhook: %s", rec.Body.String())
	}
	payload, err := srv.loadAdminNotificationsPayload()
	if err != nil {
		t.Fatal(err)
	}
	if payload.FactoryWebhook != adminNotifMaskedWebhook || !payload.HasFactory {
		t.Fatalf("webhook not masked: %+v", payload)
	}
	if !payload.Enabled || payload.RepoScope != "selected" || payload.FilterBots {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if !reflect.DeepEqual(payload.Repos, []string{"hive", "docs"}) {
		t.Fatalf("repos = %v", payload.Repos)
	}
	if !reflect.DeepEqual(payload.Events, []string{"pr_opened", "pr_merged"}) {
		t.Fatalf("events = %v", payload.Events)
	}
}

func TestGetAdminNotificationsDefaults(t *testing.T) {
	srv, _ := writeAdminNotifConfig(t, "")
	payload, err := srv.loadAdminNotificationsPayload()
	if err != nil {
		t.Fatal(err)
	}
	if payload.HasFactory || payload.FactoryWebhook != "" || payload.Enabled {
		t.Fatalf("unexpected defaults: %+v", payload)
	}
	if payload.RepoScope != "all" || !payload.FilterBots || len(payload.Repos) != 0 {
		t.Fatalf("unexpected defaults: %+v", payload)
	}
	if !reflect.DeepEqual(payload.Events, factoryActivityEvents) {
		t.Fatalf("events = %v, want default catalogue", payload.Events)
	}
}

func TestGetAdminNotificationsLoadFailure(t *testing.T) {
	srv, path := writeAdminNotifConfig(t, "")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if rec := adminNotifGet(srv); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestPutAdminNotificationsRejectsBadInputWithoutWriting(t *testing.T) {
	srv, path := writeAdminNotifConfig(t, "")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		body string
	}{
		{"malformed json", `{not json`},
		{"http webhook", `{"factoryWebhook":"http://discord.com/hook","enabled":false}`},
		{"private webhook", `{"factoryWebhook":"https://192.168.0.5/hook","enabled":false}`},
		{"enabled without events", `{"factoryWebhook":"https://discord.com/hook","enabled":true,"events":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := adminNotifPut(srv, tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("config file modified by rejected PUT:\n%s", after)
			}
		})
	}
	if _, err := os.Stat(config.RuntimeConfigFile); !os.IsNotExist(err) {
		t.Fatalf("runtime config written by rejected PUT (stat err=%v)", err)
	}
}

func TestPutAdminNotificationsLoadFailure(t *testing.T) {
	srv, path := writeAdminNotifConfig(t, "")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	rec := adminNotifPut(srv, `{"enabled":false}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestPutAdminNotificationsPersistsAndHotReloads(t *testing.T) {
	srv, path := writeAdminNotifConfig(t, "")
	const webhook = "https://discord.com/api/webhooks/42/token-value"

	rec := adminNotifPut(srv, `{"factoryWebhook":"`+webhook+`","enabled":true,"repoScope":"selected",`+
		`"repos":[" zeta ","alpha","alpha",""],"events":["pr_merged"," pr_opened ","bogus","pr_merged"],"filterBots":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "token-value") || strings.Contains(rec.Body.String(), "discord.com") {
		t.Fatalf("response echoed the webhook: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), adminNotifMaskedWebhook) {
		t.Fatalf("response missing masked webhook: %s", rec.Body.String())
	}
	if !adminNotifFeedInstalled(srv) {
		t.Fatal("PUT did not hot-reload the activity feed")
	}

	cfg, err := config.LoadWithDashboardOverlayForHub(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notifications.Discord == nil || cfg.Notifications.Discord.FactoryWebhook != webhook {
		t.Fatalf("webhook not persisted: %+v", cfg.Notifications.Discord)
	}
	a := cfg.Notifications.GitHubActivity
	if a == nil || !a.Enabled {
		t.Fatalf("activity not persisted enabled: %+v", a)
	}
	if !reflect.DeepEqual(a.Repos, []string{"alpha", "zeta"}) {
		t.Fatalf("repos = %v", a.Repos)
	}
	if !reflect.DeepEqual(a.Events, []string{"pr_merged", "pr_opened"}) {
		t.Fatalf("events = %v", a.Events)
	}
	if a.BotsFiltered() {
		t.Fatal("filterBots=false was not persisted")
	}

	// A masked placeholder must keep the stored webhook; repoScope "all" clears repos.
	rec = adminNotifPut(srv, `{"factoryWebhook":"`+adminNotifMaskedWebhook+`","enabled":true,"repoScope":"all",`+
		`"repos":["ignored"],"events":["pr_opened"],"filterBots":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	cfg, err = config.LoadWithDashboardOverlayForHub(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notifications.Discord.FactoryWebhook != webhook {
		t.Fatalf("masked placeholder overwrote webhook: %q", cfg.Notifications.Discord.FactoryWebhook)
	}
	if len(cfg.Notifications.GitHubActivity.Repos) != 0 {
		t.Fatalf("repoScope all kept repos: %v", cfg.Notifications.GitHubActivity.Repos)
	}
	if !adminNotifFeedInstalled(srv) {
		t.Fatal("feed missing after second PUT")
	}

	// Disabling tears the feed down.
	rec = adminNotifPut(srv, `{"factoryWebhook":"`+adminNotifMaskedWebhook+`","enabled":false,"repoScope":"all"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	if adminNotifFeedInstalled(srv) {
		t.Fatal("disabling notifications left the feed installed")
	}
}

func TestPutAdminNotificationsEnabledWithoutTokenStillSaves(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	srv, path := writeAdminNotifConfig(t, "")
	cfg, err := config.LoadWithDashboardOverlayForHub(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.GitHub.Token = ""
	cfg.GitHub.AppID = 12345
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	rec := adminNotifPut(srv, `{"factoryWebhook":"https://discord.com/hook","enabled":true,"events":["pr_opened"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s (hot-reload failure must only warn)", rec.Code, rec.Body.String())
	}
	if adminNotifFeedInstalled(srv) {
		t.Fatal("feed installed without a token")
	}
}

func TestReloadGitHubActivityNoopWhenUnconfigured(t *testing.T) {
	var nilSrv *HubServer
	if err := nilSrv.ReloadGitHubActivityFromConfig(slog.Default()); err != nil {
		t.Fatalf("nil receiver: %v", err)
	}
	srv := NewHubServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)), "abc1234", "v5")
	srv.SetHubConfigPath("   ", "")
	if err := srv.ReloadGitHubActivityFromConfig(slog.Default()); err != nil {
		t.Fatalf("blank path: %v", err)
	}
	var nilSrv2 *HubServer
	nilSrv2.SetHubConfigPath("x", "y")
}

const adminNotifActivityEnabled = `notifications:
  discord:
    factory_webhook: https://discord.invalid/webhook
  github_activity:
    enabled: true
    poll_interval_s: 60
`

func TestReloadGitHubActivityClearsFeedCases(t *testing.T) {
	srv, path := writeAdminNotifConfig(t, adminNotifActivityEnabled)
	if err := srv.ReloadGitHubActivityFromConfig(slog.Default()); err != nil {
		t.Fatal(err)
	}
	if !adminNotifFeedInstalled(srv) {
		t.Fatal("enabled config did not install a feed")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := srv.ReloadGitHubActivityFromConfig(slog.Default()); err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if adminNotifFeedInstalled(srv) {
		t.Fatal("missing file left the feed installed")
	}

	srv, _ = writeAdminNotifConfig(t, `notifications:
  discord:
    factory_webhook: https://discord.invalid/webhook
  github_activity:
    enabled: false
`)
	srv.SetGitHubActivityFeed(NewGitHubActivityFeed(GitHubActivityOptions{Org: "o", Token: "t"}, slog.Default()))
	if err := srv.ReloadGitHubActivityFromConfig(slog.Default()); err != nil {
		t.Fatalf("disabled: %v", err)
	}
	if adminNotifFeedInstalled(srv) {
		t.Fatal("disabled activity left the feed installed")
	}
}

func TestReloadGitHubActivityInvalidFileErrors(t *testing.T) {
	srv, path := writeAdminNotifConfig(t, "")
	if err := os.WriteFile(path, []byte("hive_id: [unterminated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv.ReloadGitHubActivityFromConfig(slog.Default()); err == nil {
		t.Fatal("expected error for invalid config")
	}
}

func TestReloadGitHubActivityMissingWebhookErrors(t *testing.T) {
	srv, _ := writeAdminNotifConfig(t, `notifications:
  github_activity:
    enabled: true
`)
	err := srv.ReloadGitHubActivityFromConfig(slog.Default())
	if err == nil || !strings.Contains(err.Error(), "factory_webhook") {
		t.Fatalf("err = %v, want factory_webhook error", err)
	}
	if adminNotifFeedInstalled(srv) {
		t.Fatal("feed installed without a webhook")
	}
}

func TestReloadGitHubActivityAppIDOnlyNeedsToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	srv, path := writeAdminNotifConfig(t, adminNotifActivityEnabled)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(raw), "  token: test-token-not-real\n", "  app_id: 12345\n", 1)
	if patched == string(raw) {
		t.Fatal("failed to patch github.token out of the fixture")
	}
	if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}

	err = srv.ReloadGitHubActivityFromConfig(slog.Default())
	if err == nil || !strings.Contains(err.Error(), "no GitHub token") {
		t.Fatalf("err = %v, want no GitHub token", err)
	}
	if adminNotifFeedInstalled(srv) {
		t.Fatal("feed installed without a token")
	}

	srv.SetHubConfigPath(path, "env-token-not-real")
	if err := srv.ReloadGitHubActivityFromConfig(slog.Default()); err != nil {
		t.Fatalf("env token fallback: %v", err)
	}
	if !adminNotifFeedInstalled(srv) {
		t.Fatal("env token fallback did not install a feed")
	}
}
