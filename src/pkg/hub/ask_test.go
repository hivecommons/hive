package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/askai"
	"github.com/hivecommons/hive/pkg/config"
)

const (
	askTestOrigin      = "https://hub.example.com"
	askTestProject     = "proj-aaaa-1111"
	askTestIntegration = "integ-bbbb-2222"
	askTestKey         = "kapa-secret-key-cccc"
)

// askFakeUpstream records the queries the hub would send to the vendor.
type askFakeUpstream struct {
	mu      sync.Mutex
	queries []string
}

func (f *askFakeUpstream) Chat(_ context.Context, _, query string) (askai.UpstreamAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
	return askai.UpstreamAnswer{
		Answer:   "Open My Hives and choose Add Hosted Hive.",
		ThreadID: "thread-1",
		Sources:  []askai.UpstreamSource{{URL: "https://docs.hivecommons.dev/hosted-hub/", Title: "Hosted hub"}},
	}, nil
}

func (f *askFakeUpstream) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

// newAskTestHub builds a hub with the Ask routes registered as production
// does. on=false leaves every HIVE_ASK_* variable unset.
func newAskTestHub(t *testing.T, on bool, up askai.Upstream) (*HubServer, *bytes.Buffer) {
	t.Helper()
	t.Cleanup(helperSetupTempDirs(t))
	t.Setenv("HIVE_HUB_PUBLIC_URL", askTestOrigin)
	for _, k := range []string{config.EnvAskEnabled, config.EnvAskKapaProjectID, config.EnvAskKapaIntegrationID, config.EnvAskKapaAPIKeyFile} {
		t.Setenv(k, "")
	}
	s := newHandlerHub()
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("DELETE /admin/{username}", s.requireAdmin(s.handleAdminDeleteUser))
	logs := &bytes.Buffer{}
	s.logger = slog.New(slog.NewTextHandler(logs, nil))
	if on {
		dir := t.TempDir()
		t.Cleanup(config.SetSecretFileRootsForTest(dir))
		keyPath := filepath.Join(dir, "kapa-api-key")
		if err := os.WriteFile(keyPath, []byte(askTestKey), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(config.EnvAskEnabled, "true")
		t.Setenv(config.EnvAskKapaProjectID, askTestProject)
		t.Setenv(config.EnvAskKapaIntegrationID, askTestIntegration)
		t.Setenv(config.EnvAskKapaAPIKeyFile, keyPath)
		s.ask = newHubAsk(s.logger, os.Getenv, up)
		if s.ask == nil {
			t.Fatalf("ask did not start:\n%s", logs)
		}
	}
	s.registerAskRoutes()
	mkUser(t, hubAdminUsername)
	mkUser(t, "alice")
	mkUser(t, "bob")
	return s, logs
}

func askDo(s *HubServer, method, path, body, login string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	r := reqWithUser(method, path, body, login)
	if login == "" {
		r.Header.Del("Cookie")
	}
	if method != http.MethodGet {
		r.Header.Set("Origin", askTestOrigin)
	}
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w
}

func TestHubAskOffRegistersNothingAndSendsNothing(t *testing.T) {
	s, logs := newAskTestHub(t, false, nil)
	if s.ask != nil {
		t.Fatal("ask must be off with no configuration")
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", askStatusPath, ""},
		{"PUT", askConsentPath, `{"text_version":"1"}`},
		{"POST", askPath, `{"question":"where?"}`},
		{"GET", askStatsPath, ""},
	} {
		if w := askDo(s, c.method, c.path, c.body, "alice", nil); w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d with the feature off", c.method, c.path, w.Code)
		}
	}
	if strings.Contains(logs.String(), "ask:") {
		t.Fatalf("an unconfigured feature must be silent:\n%s", logs)
	}

	// Switched on but missing a value: still no routes, one warning, no value.
	t.Setenv(config.EnvAskEnabled, "true")
	t.Setenv(config.EnvAskKapaProjectID, askTestProject)
	s.mux = http.NewServeMux()
	s.registerAskRoutes()
	if s.ask != nil {
		t.Fatal("a half-configured feature must stay off")
	}
	if w := askDo(s, "POST", askPath, `{"question":"where?"}`, "alice", nil); w.Code != http.StatusNotFound {
		t.Fatalf("half-configured route answered %d", w.Code)
	}
	if n := strings.Count(logs.String(), "ask: requested but staying off"); n != 1 || strings.Contains(logs.String(), askTestProject) {
		t.Fatalf("want one warning without values:\n%s", logs)
	}
}

func TestHubAskConsentAndQuestionFlow(t *testing.T) {
	up := &askFakeUpstream{}
	s, logs := newAskTestHub(t, true, up)
	ask := func(login, body string, mutate func(*http.Request)) (*httptest.ResponseRecorder, askai.Result) {
		t.Helper()
		w := askDo(s, "POST", askPath, body, login, mutate)
		var r askai.Result
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
		}
		return w, r
	}
	const question = `{"question":"How do I add a hosted hive?","section":"hub-my-hives"}`

	if w, _ := ask("", question, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", w.Code)
	}
	if w, _ := ask("alice", question, func(r *http.Request) { r.Header.Set("Authorization", "Bearer hub-api-token") }); w.Code != http.StatusUnauthorized {
		t.Fatalf("bearer must not stand in for the session: %d", w.Code)
	}
	if w, _ := ask("alice", question, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", w.Code)
	}
	if _, r := ask("alice", question, nil); r.Outcome != askai.OutcomeNotConsented || len(up.sent()) != 0 {
		t.Fatalf("no consent yet: %+v, sent %d", r, len(up.sent()))
	}

	status := askDo(s, "GET", askStatusPath, "", "alice", nil)
	var st askStatus
	if err := json.Unmarshal(status.Body.Bytes(), &st); err != nil || !st.Enabled || st.Consented || st.TextVersion != askai.ConsentTextVersion || st.Disclosure == "" {
		t.Fatalf("status %s", status.Body)
	}
	if w := askDo(s, "PUT", askConsentPath, `{"text_version":"0"}`, "alice", nil); w.Code != http.StatusConflict {
		t.Fatalf("stale disclosure: %d", w.Code)
	}
	if w := askDo(s, "PUT", askConsentPath, `{"text_version":"`+askai.ConsentTextVersion+`","person":"bob"}`, "alice", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", w.Code)
	}
	if w := askDo(s, "PUT", askConsentPath, `{"text_version":"`+askai.ConsentTextVersion+`"}`, "alice", nil); w.Code != http.StatusNoContent {
		t.Fatalf("grant: %d %s", w.Code, w.Body)
	}

	if w, _ := ask("alice", `{"question":"q?","person":"bob"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("the person comes from the session, never the body: %d", w.Code)
	}
	_, r := ask("alice", question, nil)
	if r.Outcome != askai.OutcomeOK || !r.AIGenerated || len(r.Sources) != 1 || r.Conversation == "" {
		t.Fatalf("answer %+v", r)
	}
	sent := up.sent()
	if len(sent) != 1 || sent[0] != "Dashboard section: Hub, My Hives.\n\nHow do I add a hosted hive?" || strings.Contains(sent[0], "alice") {
		t.Fatalf("vendor saw %q", sent)
	}
	if _, r := ask("bob", question, nil); r.Outcome != askai.OutcomeNotConsented {
		t.Fatalf("alice's consent covered bob: %+v", r)
	}
	if _, r := ask("alice", `{"question":"my token ghp_abcdefghijklmnopqrstuvwxyz0123"}`, nil); r.Outcome != askai.OutcomeRefusedSecretShape || len(up.sent()) != 1 {
		t.Fatalf("credential-shaped: %+v", r)
	}
	if w := askDo(s, "DELETE", askConsentPath, "", "alice", nil); w.Code != http.StatusNoContent {
		t.Fatalf("withdraw: %d", w.Code)
	}
	if _, r := ask("alice", question, nil); r.Outcome != askai.OutcomeNotConsented || len(up.sent()) != 1 {
		t.Fatalf("after withdrawal: %+v", r)
	}

	if w := askDo(s, "GET", askStatsPath, "", "alice", nil); w.Code != http.StatusForbidden {
		t.Fatalf("stats for a non-admin: %d", w.Code)
	}
	stats := askDo(s, "GET", askStatsPath, "", hubAdminUsername, nil)
	if stats.Code != http.StatusOK || !strings.Contains(stats.Body.String(), `"ok":1`) {
		t.Fatalf("stats %d %s", stats.Code, stats.Body)
	}
	for _, line := range []string{"audit: ask consent grant", "audit: ask consent withdraw", "outcome=ok", "outcome=not_consented"} {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("missing audit %q", line)
		}
	}
	if strings.Contains(logs.String(), "hosted hive?") || strings.Contains(logs.String(), "ghp_") {
		t.Fatalf("audit lines carry question text:\n%s", logs)
	}

	// Every response and every log line so far is free of the credentials.
	everything := logs.String() + status.Body.String() + stats.Body.String()
	for _, v := range []string{askTestKey, askTestProject, askTestIntegration} {
		if strings.Contains(everything, v) {
			t.Fatalf("credential %q exposed", v)
		}
	}
}

func TestHubAskConsentIsDeletedWithTheUser(t *testing.T) {
	s, _ := newAskTestHub(t, true, &askFakeUpstream{})
	if w := askDo(s, "PUT", askConsentPath, `{"text_version":"`+askai.ConsentTextVersion+`"}`, "alice", nil); w.Code != http.StatusNoContent {
		t.Fatalf("grant: %d", w.Code)
	}
	path := filepath.Join(askConsentDir(), "alice.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("consent file: %v", err)
	}
	if w := askDo(s, "DELETE", "/admin/alice", "", hubAdminUsername, nil); w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("delete user: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ask consent survived user deletion: %v", err)
	}
}
