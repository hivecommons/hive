package hub

import (
	"bytes"
	"encoding/json"
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

// writeHubNotificationsConfig writes a hub config that passes validation AND
// the Save fullness guard (one agent), so PUT handlers can persist it. The
// returned server has its config path installed; the runtime-config mirror is
// redirected into the temp dir so Save never touches the live PVC path.
func writeHubNotificationsConfig(t *testing.T, body string) (*HubServer, string) {
	t.Helper()
	t.Setenv("HIVE_HUB_SECRET", "test-secret-not-a-real-credential")
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	cfgYAML := `hive_id: notifications-admin
project:
  org: test-org
  repos: [repo-a]
hub:
  enabled: true
data:
  agents_dir: ` + filepath.Join(dir, "agents") + `
agents:
  worker:
    backend: claude
    enabled: true
` + body
	if err := os.WriteFile(path, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	origRuntime := config.RuntimeConfigFile
	config.RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	t.Cleanup(func() { config.RuntimeConfigFile = origRuntime })

	srv := NewHubServer(0, slog.Default(), "abc1234", "v5")
	srv.SetHubConfigPath(path, "")
	t.Cleanup(func() { srv.SetGitHubActivityFeed(nil) })
	return srv, path
}

func hubNotificationsFeedInstalled(s *HubServer) bool {
	s.githubActivityMu.Lock()
	defer s.githubActivityMu.Unlock()
	return s.githubActivityFeed != nil
}

func decodeHubNotificationsPayload(t *testing.T, rec *httptest.ResponseRecorder) hubNotificationsPayload {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var p hubNotificationsPayload
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatalf("decode payload: %v (body %q)", err, rec.Body.String())
	}
	return p
}

func TestValidateHubNotificationURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr string
	}{
		{"empty is allowed", "", ""},
		{"masked placeholder is allowed", "••••••••", ""},
		{"public https", "https://discord.com/api/webhooks/1/abc", ""},
		{"plain http rejected", "http://discord.com/api/webhooks/1/abc", "must start with https://"},
		{"no scheme rejected", "discord.com/api/webhooks/1/abc", "must start with https://"},
		{"localhost rejected", "https://localhost/hook", "private/internal"},
		{"localhost with port rejected", "https://localhost:8443/hook", "private/internal"},
		{"uppercase host is lowercased", "https://LOCALHOST/hook", "private/internal"},
		{"loopback rejected", "https://127.0.0.1/hook", "private/internal"},
		{"rfc1918 10/8 rejected", "https://10.1.2.3/hook", "private/internal"},
		{"rfc1918 172.16/12 rejected", "https://172.31.0.1:9000/hook", "private/internal"},
		{"rfc1918 192.168/16 rejected", "https://192.168.1.1/hook", "private/internal"},
		{"link-local rejected", "https://169.254.169.254/latest/meta-data", "private/internal"},
		{"ipv6 loopback rejected", "https://[::1]/hook", "private/internal"},
		{"unspecified rejected", "https://0.0.0.0/hook", "private/internal"},
		{"172.15 is public", "https://172.15.0.1/hook", ""},
		{"172.32 is public", "https://172.32.0.1/hook", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHubNotificationURL(tc.url, "factoryWebhook")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateHubNotificationURL(%q) = %v, want nil", tc.url, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateHubNotificationURL(%q) = nil, want error containing %q", tc.url, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
			if !strings.HasPrefix(err.Error(), "factoryWebhook ") {
				t.Fatalf("error = %q, want it to name the field", err.Error())
			}
		})
	}
}

func TestSanitizeEventListFiltersTrimsAndDedupes(t *testing.T) {
	in := []string{" pr_merged ", "bogus", "pr_merged", "", "issue_opened", "PR_MERGED"}
	got := sanitizeEventList(in, factoryActivityEvents)
	want := []string{"pr_merged", "issue_opened"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitizeEventList = %v, want %v (input order preserved, unknown dropped)", got, want)
	}
	if got := sanitizeEventList(nil, factoryActivityEvents); got == nil || len(got) != 0 {
		t.Fatalf("sanitizeEventList(nil) = %#v, want empty non-nil slice", got)
	}
}

func TestSanitizeStringsTrimsDedupesAndSorts(t *testing.T) {
	got := sanitizeStrings([]string{" repo-b", "repo-a", "", "  ", "repo-a", "repo-c "})
	want := []string{"repo-a", "repo-b", "repo-c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitizeStrings = %v, want %v", got, want)
	}
	if got := sanitizeStrings(nil); got == nil || len(got) != 0 {
		t.Fatalf("sanitizeStrings(nil) = %#v, want empty non-nil slice", got)
	}
}

func TestBoolPtrReturnsDistinctPointers(t *testing.T) {
	a, b := boolPtr(true), boolPtr(true)
	if a == b || *a != true {
		t.Fatalf("boolPtr must return a fresh pointer per call")
	}
	*a = false
	if *b != true {
		t.Fatal("mutating one boolPtr result leaked into another")
	}
}

func TestWriteHubJSONSetsContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	writeHubJSON(rec, map[string]int{"n": 1})
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"n":1}` {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestLoadAdminNotificationsPayloadDefaults(t *testing.T) {
	srv, _ := writeHubNotificationsConfig(t, "github:\n  token: test-token-not-real\n")
	p, err := srv.loadAdminNotificationsPayload()
	if err != nil {
		t.Fatalf("loadAdminNotificationsPayload: %v", err)
	}
	if p.HasFactory || p.FactoryWebhook != "" {
		t.Fatalf("no webhook configured: HasFactory=%v FactoryWebhook=%q", p.HasFactory, p.FactoryWebhook)
	}
	if p.Enabled {
		t.Fatal("Enabled should default to false")
	}
	if p.RepoScope != "all" || len(p.Repos) != 0 {
		t.Fatalf("RepoScope=%q Repos=%v, want all/empty", p.RepoScope, p.Repos)
	}
	if !p.FilterBots {
		t.Fatal("FilterBots should default to true")
	}
	if !reflect.DeepEqual(p.Events, factoryActivityEvents) {
		t.Fatalf("Events = %v, want every factory event %v", p.Events, factoryActivityEvents)
	}
	// The default list must be a copy: mutating the payload must not alter the
	// package-level catalogue the next request is built from.
	p.Events[0] = "mutated"
	if factoryActivityEvents[0] == "mutated" {
		t.Fatal("payload Events aliases factoryActivityEvents")
	}
}

func TestHandleGetAdminNotificationsMasksWebhookAndReportsSelection(t *testing.T) {
	srv, _ := writeHubNotificationsConfig(t, `github:
  token: test-token-not-real
notifications:
  discord:
    factory_webhook: https://discord.invalid/webhook/secret-path
  github_activity:
    enabled: true
    repos: [repo-a, repo-b]
    events: [pr_merged]
    filter_bots: false
`)
	rec := httptest.NewRecorder()
	srv.handleGetAdminNotifications(rec, httptest.NewRequest(http.MethodGet, "/api/saas/admin/notifications", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-path") {
		t.Fatalf("GET leaked the webhook URL: %s", rec.Body.String())
	}
	p := decodeHubNotificationsPayload(t, rec)
	if !p.HasFactory || p.FactoryWebhook != "••••••••" {
		t.Fatalf("HasFactory=%v FactoryWebhook=%q, want masked placeholder", p.HasFactory, p.FactoryWebhook)
	}
	if !p.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if p.RepoScope != "selected" || !reflect.DeepEqual(p.Repos, []string{"repo-a", "repo-b"}) {
		t.Fatalf("RepoScope=%q Repos=%v", p.RepoScope, p.Repos)
	}
	if !reflect.DeepEqual(p.Events, []string{"pr_merged"}) {
		t.Fatalf("Events = %v, want [pr_merged]", p.Events)
	}
	if p.FilterBots {
		t.Fatal("FilterBots = true, want false (filter_bots: false in config)")
	}
}

func TestHandleGetAdminNotificationsConfigLoadFailure(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "test-secret-not-a-real-credential")
	srv := NewHubServer(0, slog.Default(), "abc1234", "v5")
	srv.SetHubConfigPath(filepath.Join(t.TempDir(), "missing.yaml"), "")
	rec := httptest.NewRecorder()
	srv.handleGetAdminNotifications(rec, httptest.NewRequest(http.MethodGet, "/api/saas/admin/notifications", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the config cannot be loaded", rec.Code)
	}
}

func TestHandlePutAdminNotificationsRejectsBadInput(t *testing.T) {
	srv, path := writeHubNotificationsConfig(t, "github:\n  token: test-token-not-real\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"malformed json", `{"enabled": tru`, "invalid body"},
		{"http webhook", `{"factoryWebhook":"http://discord.com/x","enabled":false}`, "must start with https://"},
		{"private webhook", `{"factoryWebhook":"https://10.0.0.5/x","enabled":false}`, "private/internal"},
		{"enabled without events", `{"enabled":true,"events":[]}`, "at least one event"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/api/saas/admin/notifications", strings.NewReader(tc.body))
			srv.handlePutAdminNotifications(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body = %q, want it to contain %q", rec.Body.String(), tc.want)
			}
		})
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a rejected PUT must not rewrite the config file")
	}
}

func TestHandlePutAdminNotificationsConfigLoadFailure(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "test-secret-not-a-real-credential")
	srv := NewHubServer(0, slog.Default(), "abc1234", "v5")
	srv.SetHubConfigPath(filepath.Join(t.TempDir(), "missing.yaml"), "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/saas/admin/notifications", strings.NewReader(`{"enabled":false}`))
	srv.handlePutAdminNotifications(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the config cannot be loaded", rec.Code)
	}
}

func TestHandlePutAdminNotificationsPersistsSanitizesAndHotReloads(t *testing.T) {
	srv, path := writeHubNotificationsConfig(t, "github:\n  token: test-token-not-real\n")
	if hubNotificationsFeedInstalled(srv) {
		t.Fatal("precondition: no feed before the PUT")
	}

	body := `{
	  "factoryWebhook": "https://discord.invalid/api/webhooks/42/secret-path",
	  "enabled": true,
	  "repoScope": "selected",
	  "repos": [" repo-b", "repo-a", "", "repo-a"],
	  "events": ["pr_merged", "bogus_event", " issue_opened ", "pr_merged"],
	  "filterBots": false
	}`
	rec := httptest.NewRecorder()
	srv.handlePutAdminNotifications(rec, httptest.NewRequest(http.MethodPut, "/api/saas/admin/notifications", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-path") {
		t.Fatalf("PUT response echoed the webhook URL: %s", rec.Body.String())
	}
	p := decodeHubNotificationsPayload(t, rec)
	if !p.HasFactory || p.FactoryWebhook != "••••••••" {
		t.Fatalf("HasFactory=%v FactoryWebhook=%q, want masked", p.HasFactory, p.FactoryWebhook)
	}
	if !p.Enabled || p.FilterBots {
		t.Fatalf("Enabled=%v FilterBots=%v, want true/false", p.Enabled, p.FilterBots)
	}
	if p.RepoScope != "selected" || !reflect.DeepEqual(p.Repos, []string{"repo-a", "repo-b"}) {
		t.Fatalf("RepoScope=%q Repos=%v, want selected/[repo-a repo-b]", p.RepoScope, p.Repos)
	}
	if !reflect.DeepEqual(p.Events, []string{"pr_merged", "issue_opened"}) {
		t.Fatalf("Events = %v, want [pr_merged issue_opened]", p.Events)
	}

	cfg, err := config.LoadWithDashboardOverlayForHub(path)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if cfg.Notifications.Discord == nil || cfg.Notifications.Discord.FactoryWebhook != "https://discord.invalid/api/webhooks/42/secret-path" {
		t.Fatalf("persisted webhook = %+v", cfg.Notifications.Discord)
	}
	a := cfg.Notifications.GitHubActivity
	if a == nil || !a.Enabled {
		t.Fatalf("persisted github_activity = %+v, want enabled", a)
	}
	if !reflect.DeepEqual(a.Repos, []string{"repo-a", "repo-b"}) || !reflect.DeepEqual(a.Events, []string{"pr_merged", "issue_opened"}) {
		t.Fatalf("persisted repos=%v events=%v", a.Repos, a.Events)
	}
	if a.FilterBots == nil || *a.FilterBots || a.FilterDependabot == nil || *a.FilterDependabot {
		t.Fatalf("persisted filter_bots=%v filter_dependabot=%v, want both explicitly false", a.FilterBots, a.FilterDependabot)
	}
	if !hubNotificationsFeedInstalled(srv) {
		t.Fatal("enabling notifications must hot-reload and install the activity feed")
	}

	// Second PUT: the masked placeholder must keep the stored webhook, scope
	// "all" clears the repo list, and disabling tears the feed down.
	rec = httptest.NewRecorder()
	srv.handlePutAdminNotifications(rec, httptest.NewRequest(http.MethodPut, "/api/saas/admin/notifications",
		strings.NewReader(`{"factoryWebhook":"••••••••","enabled":false,"repoScope":"all","repos":["ignored"],"events":[],"filterBots":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("second PUT status = %d, body %q", rec.Code, rec.Body.String())
	}
	p = decodeHubNotificationsPayload(t, rec)
	if !p.HasFactory || p.Enabled || p.RepoScope != "all" || len(p.Repos) != 0 || !p.FilterBots {
		t.Fatalf("second PUT payload = %+v", p)
	}
	if !reflect.DeepEqual(p.Events, factoryActivityEvents) {
		t.Fatalf("empty saved event list must fall back to the full catalogue, got %v", p.Events)
	}
	cfg, err = config.LoadWithDashboardOverlayForHub(path)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if cfg.Notifications.Discord.FactoryWebhook != "https://discord.invalid/api/webhooks/42/secret-path" {
		t.Fatalf("masked placeholder overwrote the stored webhook: %q", cfg.Notifications.Discord.FactoryWebhook)
	}
	if a := cfg.Notifications.GitHubActivity; a.Enabled || len(a.Repos) != 0 {
		t.Fatalf("persisted github_activity after disable = %+v", a)
	}
	if hubNotificationsFeedInstalled(srv) {
		t.Fatal("disabling notifications must tear the activity feed down")
	}
}

func TestReloadGitHubActivityFromConfigNilAndUnsetAreNoOps(t *testing.T) {
	var nilSrv *HubServer
	if err := nilSrv.ReloadGitHubActivityFromConfig(slog.Default()); err != nil {
		t.Fatalf("nil receiver: %v", err)
	}
	nilSrv.SetHubConfigPath("/nonexistent", "tok") // must not panic

	t.Setenv("HIVE_HUB_SECRET", "test-secret-not-a-real-credential")
	srv := NewHubServer(0, slog.Default(), "abc1234", "v5")
	if err := srv.ReloadGitHubActivityFromConfig(slog.Default()); err != nil {
		t.Fatalf("empty config path: %v", err)
	}
}

func TestReloadGitHubActivityFromConfigMissingFileClearsFeed(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "test-secret-not-a-real-credential")
	srv := NewHubServer(0, slog.Default(), "abc1234", "v5")
	srv.SetGitHubActivityFeed(NewGitHubActivityFeed(GitHubActivityOptions{Org: "o", Token: "t", WebhookURL: "https://discord.invalid/x", DataDir: t.TempDir()}, slog.Default()))
	t.Cleanup(func() { srv.SetGitHubActivityFeed(nil) })
	srv.SetHubConfigPath(filepath.Join(t.TempDir(), "missing.yaml"), "")
	if err := srv.ReloadGitHubActivityFromConfig(slog.Default()); err != nil {
		t.Fatalf("missing config file must not be an error: %v", err)
	}
	if hubNotificationsFeedInstalled(srv) {
		t.Fatal("missing config file must clear any installed feed")
	}
}

func TestReloadGitHubActivityFromConfigInvalidFileReturnsError(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "test-secret-not-a-real-credential")
	path := filepath.Join(t.TempDir(), "hive.yaml")
	if err := os.WriteFile(path, []byte("project:\n  org: ''\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := NewHubServer(0, slog.Default(), "abc1234", "v5")
	srv.SetHubConfigPath(path, "")
	if err := srv.ReloadGitHubActivityFromConfig(slog.Default()); err == nil {
		t.Fatal("invalid config must surface the load error")
	}
}

func TestReloadGitHubActivityFromConfigDisabledClearsFeed(t *testing.T) {
	srv, _ := writeHubNotificationsConfig(t, `github:
  token: test-token-not-real
notifications:
  discord:
    factory_webhook: https://discord.invalid/webhook
  github_activity:
    enabled: false
`)
	srv.SetGitHubActivityFeed(NewGitHubActivityFeed(GitHubActivityOptions{Org: "o", Token: "t", WebhookURL: "https://discord.invalid/x", DataDir: t.TempDir()}, slog.Default()))
	if err := srv.ReloadGitHubActivityFromConfig(slog.Default()); err != nil {
		t.Fatalf("disabled activity: %v", err)
	}
	if hubNotificationsFeedInstalled(srv) {
		t.Fatal("disabled github_activity must clear the feed")
	}
}

func TestReloadGitHubActivityFromConfigNeedsTokenAndFallsBackToEnv(t *testing.T) {
	// An App-authenticated hive has no github.token; the activity poller still
	// needs a bearer token, so the reload must fail without the env fallback
	// and succeed with it.
	srv, _ := writeHubNotificationsConfig(t, `github:
  app_id: 12345
notifications:
  discord:
    factory_webhook: https://discord.invalid/webhook
  github_activity:
    enabled: true
    poll_interval_s: 60
`)
	err := srv.ReloadGitHubActivityFromConfig(slog.Default())
	if err == nil || !strings.Contains(err.Error(), "no GitHub token") {
		t.Fatalf("err = %v, want 'no GitHub token configured'", err)
	}
	if hubNotificationsFeedInstalled(srv) {
		t.Fatal("a failed reload must leave no feed installed")
	}

	srv.SetHubConfigPath(srv.configPath, "env-token-not-real")
	if err := srv.ReloadGitHubActivityFromConfig(slog.Default()); err != nil {
		t.Fatalf("env token fallback: %v", err)
	}
	if !hubNotificationsFeedInstalled(srv) {
		t.Fatal("env token fallback must install the feed")
	}
}
