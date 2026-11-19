package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
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
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

const (
	askTestKey         = "kapa-hive-key-secret"
	askTestProject     = "proj-hive-1111"
	askTestIntegration = "integ-hive-2222"
	askTestBearer      = "hive-heartbeat-bearer"
)

// askFakeRemote stands in for Kapa (direct mode) or the hub relay (hub mode)
// and records every request body.
type askFakeRemote struct {
	mu      sync.Mutex
	bodies  []string
	headers []http.Header
	paths   []string
	srv     *httptest.Server
}

func newAskFakeRemote(t *testing.T, reply string) *askFakeRemote {
	t.Helper()
	f := &askFakeRemote{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		f.headers = append(f.headers, r.Header.Clone())
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *askFakeRemote) seen() ([]string, []http.Header, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...), append([]http.Header(nil), f.headers...), append([]string(nil), f.paths...)
}

const askKapaReply = `{"answer":"Open Settings, then Features.","thread_id":"t-1","question_answer_id":"qa","is_uncertain":false,
	"relevant_sources":[{"source_url":"https://docs.hivecommons.dev/settings/","title":"Settings","contains_internal_data":false}]}`

const askHubReply = `{"outcome":"ok","answer":"Open Settings, then Features.","sources":[{"title":"Settings","url":"https://docs.hivecommons.dev/settings/"}],"conversation":"00112233445566778899aabbccddeeff","ai_generated":true,"docs_url":"https://docs.hivecommons.dev/"}`

// askTestServer builds a dashboard server with the given ask block. It
// isolates the consent dir, the secret roots, the vendor host and env.
func askTestServer(t *testing.T, ask config.AskConfig, hub config.HubConfig, kapaURL string) (*Server, *bytes.Buffer) {
	t.Helper()
	for _, k := range []string{config.EnvAskEnabled, config.EnvAskMode, config.EnvAskKapaProjectID, config.EnvAskKapaIntegrationID,
		config.EnvAskKapaAPIKeyFile, config.EnvAskSendSectionLabel} {
		t.Setenv(k, "")
	}
	t.Setenv(spoke.EnvHeartbeatKey, askTestBearer)
	oldDir, oldBase := askConsentDir, askKapaBaseURL
	askConsentDir, askKapaBaseURL = filepath.Join(t.TempDir(), "ask-consents"), kapaURL
	t.Cleanup(func() { askConsentDir, askKapaBaseURL = oldDir, oldBase })
	secrets := t.TempDir()
	t.Cleanup(config.SetSecretFileRootsForTest(secrets))
	keyPath := filepath.Join(secrets, "kapa-api-key")
	if err := os.WriteFile(keyPath, []byte(askTestKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if ask.Kapa.APIKeyFile == "key" {
		ask.Kapa.APIKeyFile = keyPath
	}
	s := NewServer(0, dismissLogger())
	logs := &bytes.Buffer{}
	s.logger = slog.New(slog.NewTextHandler(logs, nil))
	s.deps = &Dependencies{Config: &config.Config{HiveID: "acme-secret-hive", Hub: hub, Ask: ask}}
	return s, logs
}

func askCall(s *Server, method, path, body, user string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
		req.Header.Set("X-Hive-Role", "write")
	}
	rec := httptest.NewRecorder()
	switch {
	case path == "/api/ask/status":
		s.handleAskStatus(rec, req)
	case path == "/api/ask/consent":
		s.handleAskConsent(rec, req)
	default:
		s.handleAsk(rec, req)
	}
	return rec
}

func askResult(t *testing.T, rec *httptest.ResponseRecorder) askai.Result {
	t.Helper()
	var r askai.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %d %s: %v", rec.Code, rec.Body, err)
	}
	return r
}

func askOn() *bool { v := true; return &v }

var askDirect = config.AskConfig{Enabled: askOn(), Mode: config.AskModeDirect,
	Kapa: config.AskKapaConfig{ProjectID: askTestProject, IntegrationID: askTestIntegration, APIKeyFile: "key"}}

// TestHiveAskOffIsInert: no ask block, or a half-configured one, answers 404
// on every route, writes no consent and sends nothing.
func TestHiveAskOffIsInert(t *testing.T) {
	kapa := newAskFakeRemote(t, askKapaReply)
	half := askDirect
	half.Kapa.IntegrationID = ""
	for name, ask := range map[string]config.AskConfig{"unset": {}, "switched off": {Mode: config.AskModeDirect}, "half-configured": half} {
		s, logs := askTestServer(t, ask, config.HubConfig{}, kapa.srv.URL)
		for _, c := range []struct{ method, path, body string }{
			{"GET", "/api/ask/status", ""}, {"PUT", "/api/ask/consent", `{"text_version":"1"}`}, {"POST", "/api/ask", `{"question":"q?"}`},
		} {
			if rec := askCall(s, c.method, c.path, c.body, "alice"); rec.Code != http.StatusNotFound {
				t.Errorf("%s: %s %s = %d", name, c.method, c.path, rec.Code)
			}
		}
		if _, err := os.Stat(askConsentDir); !os.IsNotExist(err) {
			t.Errorf("%s: a consent dir was created", name)
		}
		if strings.Contains(logs.String(), askTestProject) || strings.Contains(logs.String(), askTestIntegration) {
			t.Errorf("%s: logs name a vendor id", name)
		}
	}
	if b, _, _ := kapa.seen(); len(b) != 0 {
		t.Fatalf("the vendor saw %d requests with Ask off", len(b))
	}
}

// TestHiveAskCSPIsUnchangedWhenOn is the CSP tripwire: switching Ask on must
// not change a single byte of the security headers.
func TestHiveAskCSPIsUnchangedWhenOn(t *testing.T) {
	headers := func(ask config.AskConfig) http.Header {
		s, _ := askTestServer(t, ask, config.HubConfig{}, "")
		h := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Header()
	}
	off, onH := headers(config.AskConfig{}), headers(askDirect)
	for _, k := range []string{"Content-Security-Policy", "X-Frame-Options", "Referrer-Policy", "Permissions-Policy"} {
		if off.Get(k) != onH.Get(k) {
			t.Errorf("%s changed with Ask on:\n%q\n%q", k, off.Get(k), onH.Get(k))
		}
	}
	if off.Get("Content-Security-Policy") == "" {
		t.Fatal("expected a CSP to compare")
	}
}

// TestHiveAskDirectModeFlow covers consent, the exact vendor payload, refusal
// of credential-shaped input and that the person comes from the request.
func TestHiveAskDirectModeFlow(t *testing.T) {
	kapa := newAskFakeRemote(t, askKapaReply)
	s, logs := askTestServer(t, askDirect, config.HubConfig{}, kapa.srv.URL)

	if rec := askCall(s, "POST", "/api/ask", `{"question":"q?"}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	if r := askResult(t, askCall(s, "POST", "/api/ask", `{"question":"Where do I pause an agent?"}`, "alice-login")); r.Outcome != askai.OutcomeNotConsented {
		t.Fatalf("before consent: %+v", r)
	}
	var st hiveAskStatus
	if err := json.Unmarshal(askCall(s, "GET", "/api/ask/status", "", "alice-login").Body.Bytes(), &st); err != nil || !st.Enabled || st.Consented {
		t.Fatalf("status %+v %v", st, err)
	}
	if rec := askCall(s, "PUT", "/api/ask/consent", `{"text_version":"0"}`, "alice-login"); rec.Code != http.StatusConflict {
		t.Fatalf("stale disclosure: %d", rec.Code)
	}
	if rec := askCall(s, "PUT", "/api/ask/consent", `{"text_version":"`+askai.ConsentTextVersion+`"}`, "alice-login"); rec.Code != http.StatusNoContent {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body)
	}
	if rec := askCall(s, "POST", "/api/ask", `{"question":"q?","person":"bob"}`, "alice-login"); rec.Code != http.StatusBadRequest {
		t.Fatalf("person in body: %d", rec.Code)
	}
	r := askResult(t, askCall(s, "POST", "/api/ask", `{"question":"Where do I pause an agent?","section":"agents-section"}`, "alice-login"))
	if r.Outcome != askai.OutcomeOK || len(r.Sources) != 1 || !r.AIGenerated {
		t.Fatalf("answer %+v", r)
	}
	if r := askResult(t, askCall(s, "POST", "/api/ask", `{"question":"token ghp_abcdefghijklmnopqrstuvwxyz0123"}`, "alice-login")); r.Outcome != askai.OutcomeRefusedSecretShape {
		t.Fatalf("credential: %+v", r)
	}
	if r := askResult(t, askCall(s, "POST", "/api/ask", `{"question":"q?"}`, "bob")); r.Outcome != askai.OutcomeNotConsented {
		t.Fatalf("bob covered by alice: %+v", r)
	}

	bodies, headers, paths := kapa.seen()
	if len(bodies) != 1 || paths[0] != "/query/v1/projects/"+askTestProject+"/chat/" || headers[0].Get("X-API-KEY") != askTestKey {
		t.Fatalf("vendor saw %q %q", bodies, paths)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 2 || payload["query"] != "Dashboard section: Agents.\n\nWhere do I pause an agent?" || payload["integration_id"] != askTestIntegration {
		t.Fatalf("payload %v", payload)
	}
	for _, seeded := range []string{"alice-login", "acme-secret-hive", "ghp_"} {
		if strings.Contains(bodies[0], seeded) {
			t.Fatalf("payload leaks %q", seeded)
		}
	}
	if rec := askCall(s, "DELETE", "/api/ask/consent", "", "alice-login"); rec.Code != http.StatusNoContent {
		t.Fatalf("withdraw: %d", rec.Code)
	}
	if r := askResult(t, askCall(s, "POST", "/api/ask", `{"question":"q?"}`, "alice-login")); r.Outcome != askai.OutcomeNotConsented {
		t.Fatalf("after withdrawal: %+v", r)
	}
	all := logs.String()
	for _, v := range []string{askTestKey, askTestProject, askTestIntegration, "pause an agent"} {
		if strings.Contains(all, v) {
			t.Fatalf("logs carry %q:\n%s", v, all)
		}
	}
	if !strings.Contains(all, "audit: ask consent grant") || !strings.Contains(all, "audit: ask consent withdraw") {
		t.Fatalf("missing consent audit:\n%s", all)
	}
}

// TestHiveAskHubModeUsesTheRelay: a hosted hive in hub mode holds no key and
// posts to the hub relay with its heartbeat bearer; a self-hosted hive in hub
// mode stays off.
func TestHiveAskHubModeUsesTheRelay(t *testing.T) {
	hubSrv := newAskFakeRemote(t, askHubReply)
	hubMode := config.AskConfig{Enabled: askOn(), Mode: config.AskModeHub}
	s, _ := askTestServer(t, hubMode, config.HubConfig{Enabled: true, URL: hubSrv.srv.URL, HiveType: config.HiveTypeHosted}, "")
	askCall(s, "PUT", "/api/ask/consent", `{"text_version":"`+askai.ConsentTextVersion+`"}`, "alice")
	r := askResult(t, askCall(s, "POST", "/api/ask", `{"question":"Where is pause?","section":"agents-section"}`, "alice"))
	if r.Outcome != askai.OutcomeOK || r.Conversation == "" || r.Conversation == "00112233445566778899aabbccddeeff" {
		t.Fatalf("relayed answer %+v", r)
	}
	bodies, headers, paths := hubSrv.seen()
	if len(bodies) != 1 || paths[0] != askai.RelayPath || headers[0].Get("Authorization") != "Bearer "+askTestBearer ||
		headers[0].Get(askai.RelayHiveIDHeader) != "acme-secret-hive" || strings.Contains(bodies[0], "alice") {
		t.Fatalf("relay call %q %q %v", bodies, paths, headers)
	}
	if headers[0].Get("X-API-KEY") != "" {
		t.Fatal("a hive in hub mode must never send a vendor key")
	}

	selfHosted, _ := askTestServer(t, hubMode, config.HubConfig{Enabled: true, URL: hubSrv.srv.URL}, "")
	if rec := askCall(selfHosted, "GET", "/api/ask/status", "", "alice"); rec.Code != http.StatusNotFound {
		t.Fatalf("self-hosted hive in hub mode: %d", rec.Code)
	}
	if _, problem := askEffective(selfHosted.deps.Config); problem != askHostedOnlyProblem {
		t.Fatalf("problem %q", problem)
	}
	noHub, _ := askTestServer(t, hubMode, config.HubConfig{HiveType: config.HiveTypeHosted}, "")
	if _, problem := askEffective(noHub.deps.Config); problem != askNoHubProblem {
		t.Fatalf("no hub link: %q", problem)
	}
}

func TestHiveAskSettingsFeaturesAndExport(t *testing.T) {
	s, _ := askTestServer(t, askDirect, config.HubConfig{}, "")
	got := featuresAskResponse(s.deps.Config)
	if got["askEnabled"] != true || got["askEffective"] != true || got["askMode"] != config.AskModeDirect || got["askProblem"] != "" {
		t.Fatalf("features GET %v", got)
	}
	for _, v := range got {
		if str, ok := v.(string); ok && (strings.Contains(str, askTestProject) || strings.Contains(str, askTestIntegration)) {
			t.Fatalf("features GET exposes a vendor id: %v", got)
		}
	}
	if _, err := (featuresAskBody{AskMode: strPtr("sideways")}).prepare(); err == nil {
		t.Fatal("an unknown mode must be rejected")
	}
	off := false
	apply, err := (featuresAskBody{AskEnabled: &off, AskMode: strPtr(" HUB "), AskSendSectionLabel: &off}).prepare()
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.deps.Config
	apply(cfg)
	if *cfg.Ask.Enabled || cfg.Ask.Mode != config.AskModeHub || *cfg.Ask.SendSectionLabel {
		t.Fatalf("features PUT applied %+v", cfg.Ask)
	}
	if r := featuresAskResponse(cfg); r["askEffective"] != false || r["askSendSectionLabel"] != false {
		t.Fatalf("after switching off %v", r)
	}

	// The config export redacts every ask.kapa value, and JSON never carries
	// the vendor ids at all.
	exported := redactConfigValue(map[string]any{"ask": map[string]any{"kapa": map[string]any{
		"project_id": askTestProject, "integration_id": askTestIntegration, "api_key_file": "/secrets/kapa/api-key"}}}, "")
	raw, _ := json.Marshal(exported)
	for _, v := range []string{askTestProject, askTestIntegration, "/secrets/kapa/api-key"} {
		if strings.Contains(string(raw), v) {
			t.Fatalf("export leaks %q: %s", v, raw)
		}
	}
	js, _ := json.Marshal(cfg.Ask)
	if strings.Contains(string(js), askTestProject) || strings.Contains(string(js), askTestIntegration) {
		t.Fatalf("config JSON carries vendor ids: %s", js)
	}
}
