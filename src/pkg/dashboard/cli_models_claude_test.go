package dashboard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
)

// withoutClaudeCredentials makes the claude discovery credential resolution
// deterministic and offline: no ANTHROPIC_API_KEY, an empty temp HOME, and the
// hosted-pod credentials path redirected to a nonexistent temp file. Restores
// the path seam on cleanup.
func withoutClaudeCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("HOME", t.TempDir())
	orig := claudePodCredentialsPath
	claudePodCredentialsPath = filepath.Join(t.TempDir(), "absent", ".credentials.json")
	t.Cleanup(func() { claudePodCredentialsPath = orig })
}

// writeClaudeCredentials writes a credentials file in the CLI's on-disk shape
// under a temp HOME and points $HOME at it. The hosted-pod path seam is
// redirected to a nonexistent file so a real /data volume (if present) can
// never leak into the test. Returns the file path.
func writeClaudeCredentials(t *testing.T, token string, expiresAt int64) string {
	t.Helper()
	orig := claudePodCredentialsPath
	claudePodCredentialsPath = filepath.Join(t.TempDir(), "absent", ".credentials.json")
	t.Cleanup(func() { claudePodCredentialsPath = orig })
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".credentials.json")
	creds := claude.Credentials{ClaudeAIOAuth: &claude.OAuthTokens{
		AccessToken: token,
		ExpiresAt:   expiresAt,
	}}
	data, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// redirectAnthropicEndpoint points the models probe at a test server and
// restores it on cleanup.
func redirectAnthropicEndpoint(t *testing.T, url string) {
	t.Helper()
	orig := anthropicModelsEndpoint
	anthropicModelsEndpoint = url
	t.Cleanup(func() { anthropicModelsEndpoint = orig })
}

// futureExpiryMs returns a millisecond-epoch expiry comfortably outside the
// skew window.
func futureExpiryMs() int64 {
	return time.Now().Add(claudeTokenExpirySkew + time.Hour).UnixMilli()
}

// --- credentials parsing ---

func TestReadFreshClaudeToken_Valid(t *testing.T) {
	path := writeClaudeCredentials(t, "sk-ant-oat-test", futureExpiryMs())
	if got := readFreshClaudeToken(path); got != "sk-ant-oat-test" {
		t.Fatalf("got %q, want the stored token", got)
	}
}

func TestReadFreshClaudeToken_AbsentFile(t *testing.T) {
	if got := readFreshClaudeToken(filepath.Join(t.TempDir(), "nope.json")); got != "" {
		t.Fatalf("absent file must yield empty token, got %q", got)
	}
}

func TestReadFreshClaudeToken_Malformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFreshClaudeToken(path); got != "" {
		t.Fatalf("malformed file must yield empty token, got %q", got)
	}
}

func TestReadFreshClaudeToken_Expired(t *testing.T) {
	// Long expired — the copilot-only-spoke case where the file sits stale
	// for weeks. Must be rejected BEFORE any HTTP call.
	path := writeClaudeCredentials(t, "tok", time.Now().Add(-24*time.Hour).UnixMilli())
	if got := readFreshClaudeToken(path); got != "" {
		t.Fatalf("expired token must yield empty, got %q", got)
	}
}

func TestReadFreshClaudeToken_WithinSkewWindow(t *testing.T) {
	// Not yet expired, but inside the skew margin — treated as expired so a
	// few seconds of clock skew can't turn the probe into a doomed 401.
	path := writeClaudeCredentials(t, "tok", time.Now().Add(claudeTokenExpirySkew/2).UnixMilli())
	if got := readFreshClaudeToken(path); got != "" {
		t.Fatalf("token inside skew window must yield empty, got %q", got)
	}
}

// --- credential precedence ---

func TestClaudeAPICredential_EnvKeyPrecedence(t *testing.T) {
	// A valid OAuth file exists, but ANTHROPIC_API_KEY must win with x-api-key.
	writeClaudeCredentials(t, "oauth-token", futureExpiryMs())
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api-key")
	header, value, ok := claudeAPICredential(nil)
	if !ok || header != "x-api-key" || value != "sk-ant-api-key" {
		t.Fatalf("env key must win: got %q=%q ok=%v", header, value, ok)
	}
}

func TestClaudeAPICredential_OAuthBearerFromHome(t *testing.T) {
	writeClaudeCredentials(t, "oauth-token", futureExpiryMs())
	t.Setenv("ANTHROPIC_API_KEY", "")
	header, value, ok := claudeAPICredential(nil)
	if !ok || header != "Authorization" || value != "Bearer oauth-token" {
		t.Fatalf("expected bearer from $HOME credentials: got %q=%q ok=%v", header, value, ok)
	}
}

func TestClaudeAPICredential_NoneAvailable(t *testing.T) {
	withoutClaudeCredentials(t)
	if _, _, ok := claudeAPICredential(nil); ok {
		t.Fatal("no env key and no file must yield ok=false")
	}
}

// --- discovery over httptest (never the real API) ---

// claudeModelsTestServer serves a canned /v1/models body and records the
// request headers for assertion.
func claudeModelsTestServer(t *testing.T, status int, body string, gotHeaders *http.Header) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotHeaders != nil {
			*gotHeaders = r.Header.Clone()
		}
		if r.URL.Query().Get("limit") == "" {
			t.Error("models request missing ?limit= page-size parameter")
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestDiscoverClaudeModels_LiveSuccess(t *testing.T) {
	writeClaudeCredentials(t, "oauth-token", futureExpiryMs())
	t.Setenv("ANTHROPIC_API_KEY", "")
	var hdr http.Header
	ts := claudeModelsTestServer(t, http.StatusOK,
		`{"data":[{"id":"claude-opus-5","type":"model"},{"id":"claude-sonnet-5","type":"model"},{"id":""}]}`, &hdr)
	redirectAnthropicEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverClaudeModels()
	if r.fallback {
		t.Fatal("successful probe must not be marked fallback")
	}
	if !equalStrings(r.models, []string{"claude-opus-5", "claude-sonnet-5"}) {
		t.Fatalf("got %v, want the two non-empty live ids", r.models)
	}
	// OAuth request shape: bearer + version + oauth beta.
	if got := hdr.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := hdr.Get("anthropic-version"); got != anthropicAPIVersion {
		t.Fatalf("anthropic-version = %q", got)
	}
	if got := hdr.Get("anthropic-beta"); got != anthropicOAuthBeta {
		t.Fatalf("anthropic-beta = %q", got)
	}
}

func TestDiscoverClaudeModels_APIKeyHeader(t *testing.T) {
	withoutClaudeCredentials(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-key")
	var hdr http.Header
	ts := claudeModelsTestServer(t, http.StatusOK, `{"data":[{"id":"claude-opus-5"}]}`, &hdr)
	redirectAnthropicEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverClaudeModels()
	if r.fallback || !equalStrings(r.models, []string{"claude-opus-5"}) {
		t.Fatalf("got %+v, want live single-id result", r)
	}
	if got := hdr.Get("x-api-key"); got != "sk-ant-key" {
		t.Fatalf("x-api-key = %q", got)
	}
	if hdr.Get("Authorization") != "" {
		t.Fatal("API-key path must not send an Authorization header")
	}
}

func TestDiscoverClaudeModels_Unauthorized(t *testing.T) {
	writeClaudeCredentials(t, "stale-but-unexpired", futureExpiryMs())
	t.Setenv("ANTHROPIC_API_KEY", "")
	ts := claudeModelsTestServer(t, http.StatusUnauthorized, `{"error":{"type":"authentication_error"}}`, nil)
	redirectAnthropicEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	if r := s.discoverClaudeModels(); !r.fallback || len(r.models) != 0 {
		t.Fatalf("401 must yield empty fallback result, got %+v", r)
	}
}

func TestDiscoverClaudeModels_MalformedBody(t *testing.T) {
	writeClaudeCredentials(t, "tok", futureExpiryMs())
	t.Setenv("ANTHROPIC_API_KEY", "")
	ts := claudeModelsTestServer(t, http.StatusOK, `{not json`, nil)
	redirectAnthropicEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	if r := s.discoverClaudeModels(); !r.fallback || len(r.models) != 0 {
		t.Fatalf("malformed body must yield empty fallback result, got %+v", r)
	}
}

func TestDiscoverClaudeModels_ExpiredSkipsHTTP(t *testing.T) {
	// The expired-token path must never reach the network: the test server
	// fails the test if it receives any request.
	writeClaudeCredentials(t, "tok", time.Now().Add(-time.Hour).UnixMilli())
	t.Setenv("ANTHROPIC_API_KEY", "")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("expired credentials must not trigger an HTTP call")
	}))
	t.Cleanup(ts.Close)
	redirectAnthropicEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	if r := s.discoverClaudeModels(); !r.fallback {
		t.Fatalf("expired token must fall back, got %+v", r)
	}
}

func TestDiscoverClaudeModelsLogsDoNotLeakCredentials(t *testing.T) {
	const oauthToken = "sk-ant-oat-do-not-log"
	writeClaudeCredentials(t, oauthToken, time.Now().Add(-time.Hour).UnixMilli())
	t.Setenv("ANTHROPIC_API_KEY", "")

	var logs bytes.Buffer
	s := &Server{
		cliModels: newCLIModelCache(),
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	if r := s.discoverClaudeModels(); !r.fallback {
		t.Fatalf("expired token must fall back, got %+v", r)
	}
	if got := logs.String(); strings.Contains(got, oauthToken) || strings.Contains(got, "Bearer "+oauthToken) {
		t.Fatalf("logs leaked credential material: %s", got)
	}

	logs.Reset()
	const apiKey = "sk-ant-api-key-do-not-log"
	t.Setenv("ANTHROPIC_API_KEY", apiKey)
	ts := claudeModelsTestServer(t, http.StatusUnauthorized, `{"error":{"type":"authentication_error"}}`, nil)
	redirectAnthropicEndpoint(t, ts.URL)

	if r := s.discoverClaudeModels(); !r.fallback {
		t.Fatalf("unauthorized API-key probe must fall back, got %+v", r)
	}
	if got := logs.String(); strings.Contains(got, apiKey) {
		t.Fatalf("logs leaked API key material: %s", got)
	}
}

func TestParseClaudeModelsResponse_Malformed(t *testing.T) {
	if _, err := parseClaudeModelsResponse(strings.NewReader("nope")); err == nil {
		t.Fatal("expected error on malformed body")
	}
}

// --- full queryCLIModels paths: aliases + auto-heal exclusion semantics ---

func TestQueryCLIModels_ClaudeAliasesOnLivePath(t *testing.T) {
	writeClaudeCredentials(t, "tok", futureExpiryMs())
	t.Setenv("ANTHROPIC_API_KEY", "")
	ts := claudeModelsTestServer(t, http.StatusOK,
		`{"data":[{"id":"claude-opus-5"},{"id":"claude-sonnet-5"}]}`, nil)
	redirectAnthropicEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("claude")
	if r.fallback {
		t.Fatalf("live probe must not be fallback: %+v", r)
	}
	if !contains(r.models, "claude-opus-5") {
		t.Fatalf("live id missing: %v", r.models)
	}
	for _, alias := range claudeAlwaysIncludeModels {
		if !contains(r.models, alias) {
			t.Fatalf("alias %q must survive the live path (API never returns it): %v", alias, r.models)
		}
	}
	// Live data IS usable for validation/auto-heal — and the appended aliases
	// are part of the cached list, so auto-heal can never heal an agent off a
	// bare-alias selection.
	ids := s.modelIDsForBackend("claude")
	if len(ids) == 0 {
		t.Fatal("modelIDsForBackend must return live claude ids")
	}
	for _, alias := range claudeAlwaysIncludeModels {
		if !contains(ids, alias) {
			t.Fatalf("alias %q missing from validation ids: %v", alias, ids)
		}
	}
}

func TestQueryCLIModels_ClaudeFallbackExcludedFromAutoHeal(t *testing.T) {
	withoutClaudeCredentials(t)
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("claude")
	if !r.fallback {
		t.Fatalf("no-credential claude must be fallback, got %+v", r)
	}
	// Fallback (static) data is a maintained guess, not the account's
	// inventory — it must never drive validation or model auto-heal.
	if ids := s.modelIDsForBackend("claude"); ids != nil {
		t.Fatalf("fallback claude data must yield nil validation ids, got %v", ids)
	}
}
