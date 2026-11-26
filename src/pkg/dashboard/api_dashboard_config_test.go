package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"gopkg.in/yaml.v3"
)

func TestDashboardSettingsAPIRoundTripAndMasksToken(t *testing.T) {
	s := govServer(t)
	s.deps.Config.Dashboard.AuthToken = "old-token"
	rec := doOwnerGet(s, "/api/config/dashboard")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !json.Valid([]byte(body)) || body == "" || strings.Contains(body, "old-token") {
		t.Fatalf("GET leaked token or invalid JSON: %s", body)
	}
	put := doPut(s, "/api/config/dashboard", map[string]any{
		"port":                  8080,
		"agent_poll_interval_s": 7,
		"experiment_lab":        true,
		"auth_token":            "new-token",
		"authorized_users":      []string{"alice:owner", "bob:read"},
		"authorized_user_names": map[string]string{"alice": "Alice A."},
		"hub_proxied":           true,
		"issue_bands": map[string]any{
			"waiting_labels": []string{"waiting"},
			"done_labels":    []string{"done"},
			"stale_days":     14,
		},
	})
	if put.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", put.Code, put.Body.String())
	}
	d := s.deps.Config.Dashboard
	if d.Port != 8080 || d.AgentPollIntervalS != 7 || !d.ExperimentLab || d.AuthToken != "new-token" || !d.HubProxied {
		t.Fatalf("dashboard scalar config not updated: %+v", d)
	}
	if len(d.AuthorizedUsers) != 2 || d.AuthorizedUserNames["alice"] != "Alice A." {
		t.Fatalf("access config not updated: %+v", d)
	}
	if d.IssueBands.StaleDays != 14 || len(d.IssueBands.WaitingLabels) != 1 || len(d.IssueBands.DoneLabels) != 1 {
		t.Fatalf("issue bands not updated: %+v", d.IssueBands)
	}
}

func TestDashboardSettingsAPIRejectsInvalidNumbers(t *testing.T) {
	s := govServer(t)
	s.deps.Config.Dashboard.Port = 3001
	for _, body := range []map[string]any{
		{"port": 70000},
		{"agent_poll_interval_s": 0},
		{"port": 8080, "issue_bands": map[string]any{"stale_days": -1}},
	} {
		rec := doPut(s, "/api/config/dashboard", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %#v => %d, want 400 (%s)", body, rec.Code, rec.Body.String())
		}
	}
	if s.deps.Config.Dashboard.Port != 3001 {
		t.Fatalf("invalid PUT mutated dashboard.port to %d", s.deps.Config.Dashboard.Port)
	}
}

// TestDashboardSettingsAPISubscriptionGaugeHeaderToggle covers the display-only
// dashboard.subscription_gauge_header flag: off by default, owner can turn it on and
// off, and an omitted field leaves it unchanged.
func TestDashboardSettingsAPISubscriptionGaugeHeaderToggle(t *testing.T) {
	s := govServer(t)

	get := doOwnerGet(s, "/api/config/dashboard")
	if get.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", get.Code, get.Body.String())
	}
	var got dashboardSettingsPayload
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET body: %v", err)
	}
	if got.SubscriptionGaugeHeader {
		t.Fatal("subscription_gauge_header = true by default, want false")
	}

	put := doPut(s, "/api/config/dashboard", map[string]any{"subscription_gauge_header": true})
	if put.Code != http.StatusOK {
		t.Fatalf("PUT on = %d: %s", put.Code, put.Body.String())
	}
	if !s.deps.Config.Dashboard.SubscriptionGaugeHeader {
		t.Fatal("dashboard.subscription_gauge_header not saved as true")
	}
	if err := json.Unmarshal(put.Body.Bytes(), &got); err != nil || !got.SubscriptionGaugeHeader {
		t.Fatalf("PUT response subscription_gauge_header = %v (err %v), want true", got.SubscriptionGaugeHeader, err)
	}

	if rec := doPut(s, "/api/config/dashboard", map[string]any{"agent_poll_interval_s": 9}); rec.Code != http.StatusOK {
		t.Fatalf("unrelated PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if !s.deps.Config.Dashboard.SubscriptionGaugeHeader {
		t.Fatal("a PUT without subscription_gauge_header turned the flag off")
	}

	if rec := doPut(s, "/api/config/dashboard", map[string]any{"subscription_gauge_header": false}); rec.Code != http.StatusOK {
		t.Fatalf("PUT off = %d: %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Dashboard.SubscriptionGaugeHeader {
		t.Fatal("dashboard.subscription_gauge_header not saved as false")
	}
}

// TestDashboardSettingsAPIGitHubSignInToggle covers dashboard.github_sign_in
// in Settings (#19721): an unset key reads as on, the owner can turn it off and
// on, an omitted field leaves it unchanged, and the login page follows the
// saved value without a restart.
func TestDashboardSettingsAPIGitHubSignInToggle(t *testing.T) {
	t.Setenv(config.HubURLEnvVar, "")
	s := govServer(t)
	s.deps.Config.Dashboard.GitHubSignIn = nil
	// Turning it off needs a hub sign-in link (see the refusal test below).
	s.deps.Config.HiveID = "h1"
	s.deps.Config.Hub.URL = "https://hub.example.com"

	var got dashboardSettingsPayload
	get := doOwnerGet(s, "/api/config/dashboard")
	if get.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", get.Code, get.Body.String())
	}
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET body: %v", err)
	}
	if !got.GitHubSignIn {
		t.Fatal("github_sign_in = false with the key unset, want true")
	}

	put := doPut(s, "/api/config/dashboard", map[string]any{"github_sign_in": false})
	if put.Code != http.StatusOK {
		t.Fatalf("PUT off = %d: %s", put.Code, put.Body.String())
	}
	if s.deps.Config.Dashboard.GitHubSignInEnabled() {
		t.Fatal("dashboard.github_sign_in not saved as false")
	}
	if err := json.Unmarshal(put.Body.Bytes(), &got); err != nil || got.GitHubSignIn {
		t.Fatalf("PUT response github_sign_in = %v (err %v), want false", got.GitHubSignIn, err)
	}
	if s.signInPage() == loginPage {
		t.Fatal("login page still offers GitHub sign-in after the toggle was turned off")
	}

	if rec := doPut(s, "/api/config/dashboard", map[string]any{"agent_poll_interval_s": 9}); rec.Code != http.StatusOK {
		t.Fatalf("unrelated PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Dashboard.GitHubSignInEnabled() {
		t.Fatal("a PUT without github_sign_in turned GitHub sign-in back on")
	}

	if rec := doPut(s, "/api/config/dashboard", map[string]any{"github_sign_in": true}); rec.Code != http.StatusOK {
		t.Fatalf("PUT on = %d: %s", rec.Code, rec.Body.String())
	}
	if !s.deps.Config.Dashboard.GitHubSignInEnabled() {
		t.Fatal("dashboard.github_sign_in not saved as true")
	}
	if s.signInPage() != loginPage {
		t.Fatal("login page does not offer GitHub sign-in after the toggle was turned back on")
	}
}

// TestDashboardSettingsAPIGitHubSignInOffNeedsHubLink: with no hub sign-in
// link, turning GitHub sign-in off would leave the login page with no way in,
// so the PUT is refused and the value stays on. HIVE_HUB_URL counts as a hub
// URL, as it does for the login page.
func TestDashboardSettingsAPIGitHubSignInOffNeedsHubLink(t *testing.T) {
	t.Setenv(config.HubURLEnvVar, "")
	s := govServer(t)
	s.deps.Config.Dashboard.GitHubSignIn = nil
	s.deps.Config.Dashboard.HubSignInURL = ""
	s.deps.Config.Hub.URL = ""
	s.deps.Config.HiveID = "h1"

	put := doPut(s, "/api/config/dashboard", map[string]any{"github_sign_in": false})
	if put.Code != http.StatusConflict {
		t.Fatalf("PUT off without a hub link = %d, want 409: %s", put.Code, put.Body.String())
	}
	if !strings.Contains(put.Body.String(), "no hub sign-in link") {
		t.Fatalf("refusal does not explain itself: %s", put.Body.String())
	}
	if !s.deps.Config.Dashboard.GitHubSignInEnabled() || s.signInPage() != loginPage {
		t.Fatal("a refused PUT still turned GitHub sign-in off")
	}
	if rec := doPut(s, "/api/config/dashboard", map[string]any{"github_sign_in": true}); rec.Code != http.StatusOK {
		t.Fatalf("PUT on without a hub link = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	t.Setenv(config.HubURLEnvVar, "https://hub.example.com")
	if rec := doPut(s, "/api/config/dashboard", map[string]any{"github_sign_in": false}); rec.Code != http.StatusOK {
		t.Fatalf("PUT off with HIVE_HUB_URL set = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Dashboard.GitHubSignInEnabled() {
		t.Fatal("dashboard.github_sign_in not saved as false with HIVE_HUB_URL set")
	}
}

func TestDashboardSettingsAPISubscriptionGaugeHeaderRejectsNonOwner(t *testing.T) {
	s := govServer(t)
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(map[string]any{"subscription_gauge_header": true})
	req := httptest.NewRequest(http.MethodPut, "/api/config/dashboard", &b)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "read-write")
	rec := httptest.NewRecorder()

	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT status = %d, want 403", rec.Code)
	}
	if s.deps.Config.Dashboard.SubscriptionGaugeHeader {
		t.Fatal("non-owner PUT changed dashboard.subscription_gauge_header")
	}
}

// TestDashboardSubscriptionGaugeHeaderOmittedFromYAMLWhenOff checks that a hive
// that never turns the gauge on keeps a saved config without the key.
func TestDashboardSubscriptionGaugeHeaderOmittedFromYAMLWhenOff(t *testing.T) {
	out, err := yaml.Marshal(config.DashboardConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "subscription_gauge_header") {
		t.Fatalf("default dashboard config emitted subscription_gauge_header:\n%s", out)
	}
	out, err = yaml.Marshal(config.DashboardConfig{SubscriptionGaugeHeader: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "subscription_gauge_header: true") {
		t.Fatalf("enabled dashboard config lacks subscription_gauge_header: true:\n%s", out)
	}
	var cfg config.Config
	if err := yaml.Unmarshal([]byte("dashboard:\n  subscription_gauge_header: true\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Dashboard.SubscriptionGaugeHeader {
		t.Fatal("dashboard.subscription_gauge_header: true was not read")
	}
}
