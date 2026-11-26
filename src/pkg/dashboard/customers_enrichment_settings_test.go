package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// enrichmentFeatures is the part of the governor payload the enrichment
// switches report.
type enrichmentFeatures struct {
	Features struct {
		Enabled             bool   `json:"customersEnrichmentEnabled"`
		Effective           bool   `json:"customersEnrichmentEffective"`
		GitdmEnabled        bool   `json:"customersEnrichmentGitdmEnabled"`
		GitdmEffective      bool   `json:"customersEnrichmentGitdmEffective"`
		DevstatsEnabled     bool   `json:"customersEnrichmentDevstatsEnabled"`
		DevstatsEffective   bool   `json:"customersEnrichmentDevstatsEffective"`
		CrowdDevEnabled     bool   `json:"customersEnrichmentCrowdDevEnabled"`
		CrowdDevEffective   bool   `json:"customersEnrichmentCrowdDevEffective"`
		CrowdDevTokenSet    bool   `json:"customersEnrichmentCrowdDevTokenSet"`
		GitdmRefresh        string `json:"customersEnrichmentGitdmRefresh"`
		DevstatsHourlyLimit int    `json:"customersEnrichmentDevstatsHourlyLimit"`
	} `json:"features"`
}

func clearEnrichmentEnvDashboard(t *testing.T) {
	t.Helper()
	for _, env := range []string{
		config.EnvCustomersEnabled,
		config.EnvCustomersEnrichmentEnabled,
		config.EnvCustomersEnrichmentGitdmEnabled,
		config.EnvCustomersEnrichmentGitdmRefreshInterval,
		config.EnvCustomersEnrichmentDevstatsEnabled,
		config.EnvCustomersEnrichmentDevstatsHourlyLimit,
		config.EnvCustomersEnrichmentCrowdDevEnabled,
		config.EnvCustomersEnrichmentCrowdDevToken,
		config.EnvCustomersEnrichmentCrowdDevTokenFile,
	} {
		t.Setenv(env, "")
	}
}

func readEnrichmentFeatures(t *testing.T, s *Server) enrichmentFeatures {
	t.Helper()
	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d - %s", rec.Code, rec.Body.String())
	}
	var p enrichmentFeatures
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestGovernorFeatures_CustomersEnrichmentRoundTrip covers
// customers.enrichment.*: gitdm defaults on but enrichment defaults off;
// a non-owner is refused, and the owner's values persist with their effective
// value, an unrelated PUT leaves them alone, a provider follows its parents,
// and the env overrides win.
func TestGovernorFeatures_CustomersEnrichmentRoundTrip(t *testing.T) {
	clearEnrichmentEnvDashboard(t)
	s := covApiServer(t)
	s.deps.Config.Customers.Enrichment.CrowdDev.TokenFile = filepath.Join(t.TempDir(), "crowd")
	got := readEnrichmentFeatures(t, s).Features
	if got.Enabled || got.Effective || !got.GitdmEnabled || got.GitdmEffective || got.DevstatsEnabled ||
		got.DevstatsEffective || got.CrowdDevEnabled || got.CrowdDevEffective || got.CrowdDevTokenSet {
		t.Fatalf("default payload = %+v, want only gitdm configured on", got)
	}
	all := map[string]any{
		"customersEnrichmentEnabled":             true,
		"customersEnrichmentGitdmEnabled":        true,
		"customersEnrichmentDevstatsEnabled":     true,
		"customersEnrichmentCrowdDevEnabled":     true,
		"customersEnrichmentGitdmRefresh":        "12h",
		"customersEnrichmentDevstatsHourlyLimit": 40,
	}
	if rec := putFeatures(s, all, func(r *http.Request) {
		r.Header.Set("X-Hive-Role", "read-write")
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT = %d, want 403", rec.Code)
	}
	if e := s.deps.Config.Customers.Enrichment; e.IsEnabled() || e.Gitdm.Enabled != nil || e.Gitdm.RefreshInterval != "" || e.Devstats.HourlyLimit != 0 {
		t.Fatalf("a refused PUT changed the config: %+v", e)
	}
	if rec := doPut(s, "/api/config/governor/features", all); rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d - %s", rec.Code, rec.Body.String())
	}
	got = readEnrichmentFeatures(t, s).Features
	if !got.Enabled || !got.Effective || !got.GitdmEffective || !got.DevstatsEffective || !got.CrowdDevEffective ||
		got.GitdmRefresh != "12h" || got.DevstatsHourlyLimit != 40 {
		t.Fatalf("after PUT = %+v", got)
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"retroEnabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("unrelated PUT: %d", rec.Code)
	}
	if !s.deps.Config.CustomersEnrichmentGitdmEnabled() || s.deps.Config.Customers.Enrichment.Gitdm.RefreshInterval != "12h" {
		t.Fatal("a PUT that did not mention enrichment changed it")
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"customersEnrichmentEnabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("PUT off: %d", rec.Code)
	}
	got = readEnrichmentFeatures(t, s).Features
	if !got.GitdmEnabled || got.GitdmEffective || got.DevstatsEffective || got.CrowdDevEffective {
		t.Fatalf("block off = %+v, want providers saved on, effective off", got)
	}
	t.Setenv(config.EnvCustomersEnrichmentEnabled, "true")
	t.Setenv(config.EnvCustomersEnrichmentDevstatsEnabled, "false")
	got = readEnrichmentFeatures(t, s).Features
	if got.Enabled || !got.Effective || !got.GitdmEffective || !got.DevstatsEnabled || got.DevstatsEffective {
		t.Fatalf("env overrides = %+v", got)
	}
	t.Setenv(config.EnvCustomersEnabled, "false")
	if got = readEnrichmentFeatures(t, s).Features; got.Effective || got.GitdmEffective {
		t.Fatalf("Customers off = %+v, want enrichment effective off", got)
	}
}

// Bad limits are refused before anything is written.
func TestGovernorFeatures_CustomersEnrichmentRejectsBadValues(t *testing.T) {
	clearEnrichmentEnvDashboard(t)
	s := covApiServer(t)
	for _, body := range []map[string]any{
		{"customersEnrichmentEnabled": true, "customersEnrichmentGitdmRefresh": "soon"},
		{"customersEnrichmentEnabled": true, "customersEnrichmentGitdmRefresh": "5m"},
		{"customersEnrichmentEnabled": true, "customersEnrichmentDevstatsHourlyLimit": -1},
		{"customersEnrichmentEnabled": true, "customersEnrichmentDevstatsHourlyLimit": config.MaxCustomersEnrichmentDevstatsHourlyLimit + 1},
	} {
		if rec := doPut(s, "/api/config/governor/features", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%v = %d, want 400", body, rec.Code)
		}
		if s.deps.Config.Customers.Enrichment.IsEnabled() {
			t.Fatalf("%v: a refused PUT turned enrichment on", body)
		}
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"customersEnrichmentGitdmRefresh": ""}); rec.Code != http.StatusOK {
		t.Fatalf("empty refresh (default) = %d", rec.Code)
	}
}

// With every enrichment switch at its default, a PUT of the existing
// Customers toggles behaves exactly as before: nothing under
// customers.enrichment is written.
func TestGovernorFeatures_CustomersEnrichmentAllOffNoChange(t *testing.T) {
	clearEnrichmentEnvDashboard(t)
	s := covApiServer(t)
	if rec := doPut(s, "/api/config/governor/features", map[string]any{
		"customersEnabled": true, "customersSearchEnabled": true, "customersHubImportEnabled": false,
	}); rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d", rec.Code)
	}
	if e := s.deps.Config.Customers.Enrichment; e.Enabled != nil || e.Gitdm.Enabled != nil || e.Devstats.Enabled != nil ||
		e.CrowdDev.Enabled != nil || e.Gitdm.RefreshInterval != "" || e.Devstats.HourlyLimit != 0 {
		t.Fatalf("enrichment written by an unrelated Customers PUT: %+v", e)
	}
	if s.deps.Config.CustomersEnrichmentEnabled() {
		t.Fatal("enrichment must stay off")
	}
}

func enrichmentSettingsPost(s *Server, body string, owner bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/customers/enrichment/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", "octo")
	if owner {
		markOwnerRequest(req)
	} else {
		req.Header.Set("X-Hive-Role", "read-write")
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

// TestCustomersEnrichmentSettings_CrowdDevToken pins the write-only token:
// owner only, sealed 0600 into the token file, never echoed, cleared by "".
func TestCustomersEnrichmentSettings_CrowdDevToken(t *testing.T) {
	clearEnrichmentEnvDashboard(t)
	s := covApiServer(t)
	tokFile := filepath.Join(t.TempDir(), "secrets", "crowd")
	s.deps.Config.Customers.Enrichment.CrowdDev.TokenFile = tokFile
	const secret = "crowd-secret-value"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/customers/enrichment/settings", nil)
	req.Header.Set("X-Hive-Role", "read-write")
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner GET = %d, want 403", rec.Code)
	}
	if rec := enrichmentSettingsPost(s, `{"crowddev_token":"`+secret+`"}`, false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner POST = %d, want 403", rec.Code)
	}
	if _, err := os.Stat(tokFile); !os.IsNotExist(err) {
		t.Fatalf("a refused POST wrote the token: %v", err)
	}

	var got CustomersEnrichmentSettings
	rec = doOwnerGet(s, "/api/customers/enrichment/settings")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || got.CrowdDevTokenSet {
		t.Fatalf("default GET = %d %s", rec.Code, rec.Body.String())
	}
	rec = enrichmentSettingsPost(s, `{"crowddev_token":"`+secret+`"}`, true)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.CrowdDevTokenSet {
		t.Fatalf("after POST %+v", got)
	}
	if fi, err := os.Stat(tokFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v, %v", fi, err)
	}
	if v, err := s.deps.Config.Customers.Enrichment.CrowdDev.Token(context.Background()); err != nil || v != secret {
		t.Fatalf("stored token = %q, %v", v, err)
	}
	for _, path := range []string{"/api/customers/enrichment/settings", "/api/config/governor"} {
		if rec := doOwnerGet(s, path); strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("GET %s must never echo the token", path)
		}
	}
	if !readEnrichmentFeatures(t, s).Features.CrowdDevTokenSet {
		t.Fatal("Features payload must report the token as set")
	}
	rec = enrichmentSettingsPost(s, `{}`, true)
	if _ = json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || !got.CrowdDevTokenSet {
		t.Fatalf("an absent field must leave the token alone: %d %+v", rec.Code, got)
	}
	rec = enrichmentSettingsPost(s, `{"crowddev_token":""}`, true)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.CrowdDevTokenSet {
		t.Fatal("an empty token clears it")
	}
	if _, err := os.Stat(tokFile); !os.IsNotExist(err) {
		t.Fatalf("cleared token file still present: %v", err)
	}
	if rec := enrichmentSettingsPost(s, `not json`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d, want 400", rec.Code)
	}
}

func TestFeaturesTab_CustomersEnrichmentMarkup(t *testing.T) {
	html := featuresMarkup(t)
	for _, want := range []string{
		`data-section="features" data-key="customersEnrichmentEnabled"`,
		`data-section="features" data-key="customersEnrichmentGitdmEnabled"`,
		`data-section="features" data-key="customersEnrichmentDevstatsEnabled"`,
		`data-section="features" data-key="customersEnrichmentCrowdDevEnabled"`,
		`data-arg1="customersEnrichmentGitdmRefresh"`,
		`data-arg1="customersEnrichmentDevstatsHourlyLimit"`,
		`yamlHint('customers.enrichment.enabled')`,
		`yamlHint('customers.enrichment.gitdm.enabled')`,
		`yamlHint('customers.enrichment.gitdm.refresh_interval')`,
		`yamlHint('customers.enrichment.devstats.enabled')`,
		`yamlHint('customers.enrichment.devstats.hourly_limit')`,
		`yamlHint('customers.enrichment.crowddev.enabled')`,
		`yamlHint('customers.enrichment.crowddev.token_file')`,
		`f.customersEnrichmentEffective !== f.customersEnrichmentEnabled`,
		`data-action="saveCustomersCrowdDevToken"`,
		`function saveCustomersCrowdDevToken(`,
		`'/api/customers/enrichment/settings'`,
		`key: 'customers.enrichment.enabled'`,
		`key: 'customers.enrichment.gitdm.enabled'`,
		`key: 'customers.enrichment.devstats.enabled'`,
		`key: 'customers.enrichment.crowddev.enabled'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
