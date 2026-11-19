package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/pkg/ask"
	"github.com/hivecommons/hive/pkg/config"
)

// Ask AI help hub backend (#19180).

const (
	askTestOrigin = "https://hub.example.com"
	askAPI        = "/api/ask"
	askConsentAPI = "/api/ask/consent"
)

type askVendor struct {
	mu      sync.Mutex
	queries []string
	resp    ask.ChatResponse
	err     error
}

func (v *askVendor) Chat(_ context.Context, query, _ string) (ask.ChatResponse, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.queries = append(v.queries, query)
	return v.resp, v.err
}

func (v *askVendor) calls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.queries)
}

func askTestSettings() config.AskSettings {
	return config.AskSettings{
		Active: true, Mode: config.AskModeDirect, SendSectionLabel: true,
		Limits: config.AskLimits{
			PersonBurst: 3, PersonPerHour: 30, HivePerHour: 120, GlobalPerMinute: 8,
			InFlight: 4, Daily: 500, QuestionChars: 200, AnswerChars: 500, TimeoutSeconds: 5,
		},
	}
}

func clearAskEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		config.EnvAskEnabled, config.EnvAskMode, config.EnvAskKapaProjectID,
		config.EnvAskKapaIntegrationID, config.EnvAskKapaAPIKeyFile,
	} {
		t.Setenv(name, "")
	}
}

// newAskTestHub builds a hub with the Ask routes registered. With vendor nil
// the feature is off (as in production with no configuration).
func newAskTestHub(t *testing.T, vendor *askVendor) (*HubServer, *bytes.Buffer) {
	t.Helper()
	clearAskEnv(t)
	t.Setenv("HIVE_HUB_PUBLIC_URL", askTestOrigin)
	s := newHandlerHub()
	s.mux = http.NewServeMux()
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))
	if vendor != nil {
		svc, err := ask.Open(askTestSettings(), s.logger, ask.Options{Chatter: vendor})
		if err != nil {
			t.Fatal(err)
		}
		s.askOnce.Do(func() {})
		s.askSvc = svc
	}
	s.registerAskRoutes()
	s.mux.HandleFunc("DELETE /admin/{username}", s.requireAdmin(s.handleAdminDeleteUser))
	return s, &logs
}

func askDo(s *HubServer, method, path, body, login string) *httptest.ResponseRecorder {
	r := reqWithUser(method, path, body, login)
	if login == "" {
		r.Header.Del("Cookie")
	}
	if method != http.MethodGet {
		r.Header.Set("Origin", askTestOrigin)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w
}

func goodAskResponse() ask.ChatResponse {
	return ask.ChatResponse{
		Answer: "Open Settings, then Features.", ThreadID: "vendor-thread-1",
		RelevantSources: []ask.VendorSource{{SourceURL: "https://docs.hivecommons.dev/docs/dashboard", Title: "Dashboard"}},
	}
}

func grantAskConsent(t *testing.T, s *HubServer, login string) {
	t.Helper()
	w := askDo(s, "PUT", askConsentAPI, `{"text_version":"`+askConsentTextVersion+`"}`, login)
	if w.Code != http.StatusNoContent {
		t.Fatalf("grant: %d %s", w.Code, w.Body)
	}
}

func TestAskOffMeansNoRouteAndNoRequest(t *testing.T) {
	defer helperSetupTempDirs(t)()
	s, logs := newAskTestHub(t, nil)
	mkUser(t, "alice")
	mkUser(t, hubAdminUsername)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", askConsentAPI, ""},
		{"PUT", askConsentAPI, `{"text_version":"1"}`},
		{"DELETE", askConsentAPI, ""},
		{"POST", askAPI, `{"question":"hi"}`},
	} {
		// Signed in or not, the answer is the same 404: a probe learns nothing.
		for _, login := range []string{"alice", ""} {
			if w := askDo(s, tc.method, tc.path, tc.body, login); w.Code != http.StatusNotFound {
				t.Fatalf("%s %s as %q = %d, want 404", tc.method, tc.path, login, w.Code)
			}
		}
	}
	if w := askDo(s, "GET", "/api/saas/admin/ask/stats", "", hubAdminUsername); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("stats when off: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(askConsentDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("off must write nothing: %v", err)
	}
	if strings.Contains(logs.String(), "ask:") {
		t.Fatalf("off must be silent when never requested: %s", logs.String())
	}
}

func TestAskAddsNoContentSecurityPolicy(t *testing.T) {
	defer helperSetupTempDirs(t)()
	if strings.Contains(strings.ToLower(activityPageCSP), "kapa") {
		t.Fatal("the hub CSP names the vendor")
	}
	vendor := &askVendor{resp: goodAskResponse()}
	s, _ := newAskTestHub(t, vendor)
	mkUser(t, "alice")
	grantAskConsent(t, s, "alice")
	for _, w := range []*httptest.ResponseRecorder{
		askDo(s, "GET", askConsentAPI, "", "alice"),
		askDo(s, "POST", askAPI, `{"question":"hi"}`, "alice"),
	} {
		if csp := w.Header().Get("Content-Security-Policy"); csp != "" {
			t.Fatalf("Ask routes must not set a CSP, got %q", csp)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("Ask responses must not be cached")
		}
	}
}

func TestAskRequiresABrowserSession(t *testing.T) {
	defer helperSetupTempDirs(t)()
	vendor := &askVendor{resp: goodAskResponse()}
	s, _ := newAskTestHub(t, vendor)
	mkUser(t, "alice")
	grantAskConsent(t, s, "alice")

	if w := askDo(s, "POST", askAPI, `{"question":"hi"}`, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", w.Code)
	}
	// A bearer token is not a person's consent.
	r := reqWithUser("POST", askAPI, `{"question":"hi"}`, "alice")
	r.Header.Set("Origin", askTestOrigin)
	r.Header.Set("Authorization", "Bearer something")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bearer = %d", w.Code)
	}
	// Cross-origin browser POST fails the CSRF check.
	r = reqWithUser("POST", askAPI, `{"question":"hi"}`, "alice")
	r.Header.Set("Origin", "https://evil.example.com")
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d", w.Code)
	}
	// A blocked account is refused.
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "mallory", Blocked: true, Hives: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if w := askDo(s, "POST", askAPI, `{"question":"hi"}`, "mallory"); w.Code != http.StatusForbidden {
		t.Fatalf("blocked = %d", w.Code)
	}
	if vendor.calls() != 0 {
		t.Fatalf("vendor was called %d times by refused requests", vendor.calls())
	}
}

func TestAskConsentGatesEveryRequest(t *testing.T) {
	defer helperSetupTempDirs(t)()
	vendor := &askVendor{resp: goodAskResponse()}
	s, logs := newAskTestHub(t, vendor)
	mkUser(t, "alice")
	mkUser(t, "bob")
	postAsk := func(login string) *httptest.ResponseRecorder {
		return askDo(s, "POST", askAPI, `{"question":"where is Features?"}`, login)
	}

	// No consent yet.
	if w := postAsk("alice"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not_consented") {
		t.Fatalf("before consent: %d %s", w.Code, w.Body)
	}
	if vendor.calls() != 0 {
		t.Fatal("a request without consent reached the vendor")
	}

	// The page must submit the version it displayed.
	if w := askDo(s, "PUT", askConsentAPI, `{"text_version":"old"}`, "alice"); w.Code != http.StatusConflict {
		t.Fatalf("stale text version = %d", w.Code)
	}
	if w := askDo(s, "PUT", askConsentAPI, `{"text_version":"1","person":"bob"}`, "alice"); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", w.Code)
	}
	grantAskConsent(t, s, "alice")
	if w := postAsk("alice"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"outcome":"ok"`) {
		t.Fatalf("after consent: %d %s", w.Code, w.Body)
	}
	if vendor.calls() != 1 {
		t.Fatalf("vendor calls = %d", vendor.calls())
	}
	// One person's consent does not cover another.
	if w := postAsk("bob"); w.Code != http.StatusForbidden {
		t.Fatalf("bob without consent = %d", w.Code)
	}

	// The record: one file per person, owner-only, with the text version.
	u := loadSaaSUser("alice")
	path, err := askConsentPath(u)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("consent permissions = %o", info.Mode().Perm())
	}
	c, err := readAskConsent(path)
	if err != nil || c == nil || c.TextVersion != askConsentTextVersion || c.Person != "alice" {
		t.Fatalf("record = %+v, %v", c, err)
	}
	if bobPath, _ := askConsentPath(loadSaaSUser("bob")); bobPath == path {
		t.Fatal("two people share a consent file")
	}

	// GET reports state and serves the disclosure text.
	w := askDo(s, "GET", askConsentAPI, "", "alice")
	var state struct {
		Consented   bool   `json:"consented"`
		TextVersion string `json:"text_version"`
		Text        string `json:"text"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || !state.Consented || state.Text != askConsentText {
		t.Fatalf("state = %+v %v", state, err)
	}
	if state2 := askDo(s, "GET", askConsentAPI, "", "bob"); strings.Contains(state2.Body.String(), `"consented":true`) {
		t.Fatal("bob appears consented")
	}

	// A text change blocks the next request until confirmed again.
	if err := writeAskConsent(path, askConsent{Person: "alice", TextVersion: "0"}); err != nil {
		t.Fatal(err)
	}
	if w := postAsk("alice"); w.Code != http.StatusForbidden {
		t.Fatalf("old text version = %d", w.Code)
	}
	grantAskConsent(t, s, "alice")

	// Withdrawal takes effect on the very next request.
	if w := askDo(s, "DELETE", askConsentAPI, "", "alice"); w.Code != http.StatusNoContent {
		t.Fatalf("withdraw = %d", w.Code)
	}
	if w := postAsk("alice"); w.Code != http.StatusForbidden {
		t.Fatalf("after withdrawal = %d", w.Code)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("withdrawn consent must not linger: %v", err)
	}
	// Withdrawing again is harmless.
	if w := askDo(s, "DELETE", askConsentAPI, "", "alice"); w.Code != http.StatusNoContent {
		t.Fatalf("second withdraw = %d", w.Code)
	}
	for _, want := range []string{"audit: ask consent grant", "audit: ask consent withdraw"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing audit line %q:\n%s", want, logs.String())
		}
	}
}

func TestAskRefusesCredentialsAndBadBodies(t *testing.T) {
	defer helperSetupTempDirs(t)()
	vendor := &askVendor{resp: goodAskResponse()}
	s, logs := newAskTestHub(t, vendor)
	mkUser(t, "alice")
	grantAskConsent(t, s, "alice")

	token := "ghp_" + strings.Repeat("a1B2c3D4e5", 4)
	w := askDo(s, "POST", askAPI, `{"question":"why does `+token+` fail"}`, "alice")
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "refused_secret_shape") {
		t.Fatalf("credential: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), token) || strings.Contains(logs.String(), token) {
		t.Fatal("the credential was echoed")
	}
	long := strings.Repeat("x", 201)
	if w := askDo(s, "POST", askAPI, `{"question":"`+long+`"}`, "alice"); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("too long = %d", w.Code)
	}
	for _, body := range []string{`not json`, `{"question":"hi","user":"alice"}`, `{"question":"hi"} {}`} {
		if w := askDo(s, "POST", askAPI, body, "alice"); w.Code != http.StatusBadRequest {
			t.Fatalf("body %q = %d", body, w.Code)
		}
	}
	if w := askDo(s, "POST", askAPI, `{"question":"`+strings.Repeat("x", maxAskBodyBytes)+`"}`, "alice"); w.Code != http.StatusBadRequest {
		t.Fatalf("oversized body = %d", w.Code)
	}
	if vendor.calls() != 0 {
		t.Fatalf("refused input reached the vendor %d times", vendor.calls())
	}
}

func TestAskSendsOnlyQuestionAndSectionLabel(t *testing.T) {
	defer helperSetupTempDirs(t)()
	vendor := &askVendor{resp: goodAskResponse()}
	s, _ := newAskTestHub(t, vendor)
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "alice", FullName: "Alice Private", Email: "alice@private.example",
		Hives: map[string]string{"secret-hive-id": "owner"},
	}); err != nil {
		t.Fatal(err)
	}
	grantAskConsent(t, s, "alice")
	w := askDo(s, "POST", askAPI, `{"question":"where is it?","section":"settings-features"}`, "alice")
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := vendor.queries[0]; got != "Dashboard section: Settings, Features\n\nwhere is it?" {
		t.Fatalf("vendor query = %q", got)
	}
	for _, seeded := range []string{"alice", "Alice Private", "private.example", "secret-hive-id"} {
		if strings.Contains(vendor.queries[0], seeded) {
			t.Fatalf("vendor query carries %q", seeded)
		}
	}
	// The response carries the cited answer and an opaque handle only.
	var res ask.Result
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ask.OutcomeOK || len(res.Sources) != 1 || res.Thread == "" || strings.Contains(w.Body.String(), "vendor-thread-1") {
		t.Fatalf("result = %+v body=%s", res, w.Body)
	}
}

func TestAskLimitsAndFallbackStatuses(t *testing.T) {
	defer helperSetupTempDirs(t)()
	vendor := &askVendor{resp: goodAskResponse()}
	s, _ := newAskTestHub(t, vendor)
	mkUser(t, "alice")
	grantAskConsent(t, s, "alice")
	post := func() *httptest.ResponseRecorder {
		return askDo(s, "POST", askAPI, `{"question":"q"}`, "alice")
	}
	for i := 0; i < 3; i++ {
		if w := post(); w.Code != http.StatusOK {
			t.Fatalf("request %d = %d", i, w.Code)
		}
	}
	w := post()
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("burst: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if vendor.calls() != 3 {
		t.Fatalf("a limited request reached the vendor: %d", vendor.calls())
	}

	// A vendor outage is a 503 with the fallback message, never an answer.
	vendor2 := &askVendor{err: ask.ErrUnavailable}
	s2, _ := newAskTestHub(t, vendor2)
	grantAskConsent(t, s2, "alice")
	w = askDo(s2, "POST", askAPI, `{"question":"q"}`, "alice")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "upstream_error") || strings.Contains(w.Body.String(), `"answer"`) {
		t.Fatalf("outage: %d %s", w.Code, w.Body)
	}
}

func TestAskUserDeletionRemovesConsent(t *testing.T) {
	defer helperSetupTempDirs(t)()
	vendor := &askVendor{resp: goodAskResponse()}
	s, _ := newAskTestHub(t, vendor)
	mkUser(t, hubAdminUsername)
	mkUser(t, "alice")
	grantAskConsent(t, s, "alice")
	path, err := askConsentPath(loadSaaSUser("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if w := askDo(s, "DELETE", "/admin/alice", "", hubAdminUsername); w.Code != http.StatusOK {
		t.Fatalf("delete user = %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || loadSaaSUser("alice") != nil {
		t.Fatalf("user deletion must remove the Ask consent: %v", err)
	}
	if filepath.Dir(path) != askConsentDir() {
		t.Fatalf("consent lives outside its own directory: %s", path)
	}
}

func TestAskStatsCountOutcomesForAdminsOnly(t *testing.T) {
	defer helperSetupTempDirs(t)()
	vendor := &askVendor{resp: goodAskResponse()}
	s, _ := newAskTestHub(t, vendor)
	mkUser(t, hubAdminUsername)
	mkUser(t, "alice")
	grantAskConsent(t, s, "alice")
	askDo(s, "POST", askAPI, `{"question":"q"}`, "alice")
	askDo(s, "POST", askAPI, `{"question":"  "}`, "alice")

	if w := askDo(s, "GET", "/api/saas/admin/ask/stats", "", "alice"); w.Code == http.StatusOK {
		t.Fatalf("a non-admin read the stats: %d", w.Code)
	}
	w := askDo(s, "GET", "/api/saas/admin/ask/stats", "", hubAdminUsername)
	var stats struct {
		Enabled   bool             `json:"enabled"`
		Outcomes  map[string]int64 `json:"outcomes"`
		SentToday int              `json:"sent_today"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if !stats.Enabled || stats.Outcomes["ok"] != 1 || stats.Outcomes["refused_empty"] != 1 || stats.SentToday != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestAskServiceStartsFromEnvAndStaysOffQuietlyOtherwise(t *testing.T) {
	defer helperSetupTempDirs(t)()
	dir := t.TempDir()
	defer cruxconfig.SetSecretFileRootsForTest(dir)()
	keyFile := filepath.Join(dir, "kapa-key")
	if err := os.WriteFile(keyFile, []byte("kapa-key-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	newHub := func() (*HubServer, *bytes.Buffer) {
		s := newHandlerHub()
		var logs bytes.Buffer
		s.logger = slog.New(slog.NewTextHandler(&logs, nil))
		return s, &logs
	}

	// Switched on with every value: starts.
	clearAskEnv(t)
	t.Setenv(config.EnvAskEnabled, "true")
	t.Setenv(config.EnvAskKapaProjectID, "proj-secret-value")
	t.Setenv(config.EnvAskKapaIntegrationID, "integ-secret-value")
	t.Setenv(config.EnvAskKapaAPIKeyFile, keyFile)
	s, logs := newHub()
	if s.askService() == nil {
		t.Fatalf("a fully configured Ask did not start: %s", logs)
	}

	// Switched on but a value is missing: off, ONE warning, no value in it.
	t.Setenv(config.EnvAskKapaIntegrationID, "")
	s, logs = newHub()
	for i := 0; i < 3; i++ {
		if s.askService() != nil {
			t.Fatal("Ask started with a missing value")
		}
	}
	if n := strings.Count(logs.String(), "ask: enabled but not started"); n != 1 {
		t.Fatalf("warnings = %d, want 1:\n%s", n, logs)
	}
	for _, v := range []string{"proj-secret-value", "kapa-key-value", keyFile} {
		if strings.Contains(logs.String(), v) {
			t.Fatalf("warning carries %q", v)
		}
	}

	// Switched on, every value present, key file unreadable: off, one warning.
	t.Setenv(config.EnvAskKapaIntegrationID, "integ-secret-value")
	t.Setenv(config.EnvAskKapaAPIKeyFile, filepath.Join(dir, "missing"))
	s, logs = newHub()
	if s.askService() != nil || strings.Count(logs.String(), "ask: enabled but not started") != 1 {
		t.Fatalf("unreadable key file:\n%s", logs)
	}
	for _, v := range []string{"proj-secret-value", "integ-secret-value", dir} {
		if strings.Contains(logs.String(), v) {
			t.Fatalf("warning carries %q", v)
		}
	}

	// Never switched on: off and silent.
	clearAskEnv(t)
	s, logs = newHub()
	if s.askService() != nil || logs.Len() != 0 {
		t.Fatalf("default must be off and silent: %s", logs)
	}
}
