package rotation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// fakeCLI installs an executable shell script named `name` on PATH that
// prints `output` and exits with `exitCode`.
func fakeCLI(t *testing.T, name, output string, exitCode int) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncat <<'EOF'\n%s\nEOF\nexit %d\n", output, exitCode)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// claudeCredsFile writes a credentials file holding the given token and
// returns its path.
func claudeCredsFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q}}`, token)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type countingUnauthorizedTransport struct {
	calls int
}

func (t *countingUnauthorizedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{StatusCode: http.StatusUnauthorized, Body: http.NoBody, Header: make(http.Header)}, nil
}

// claudeUsageServer serves /api/oauth/usage and verifies the Authorization
// and anthropic-beta headers the probe must send.
func claudeUsageServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Errorf("anthropic-beta = %q, want oauth-2025-04-20", got)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClaudeProber_UsedBelowThreshold(t *testing.T) {
	srv := claudeUsageServer(t, http.StatusOK, `{"limits":[
		{"kind":"session","percent":11,"resets_at":"2026-08-23T16:49:59Z"},
		{"kind":"weekly_all","percent":40,"resets_at":"2026-08-26T23:00:00Z"}]}`)
	p := ClaudeProber{ThresholdPct: 80, BaseURL: srv.URL, CredentialsPath: claudeCredsFile(t, "test-token")}
	if p.Provider() != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", p.Provider())
	}
	h := p.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if !h.Available {
		t.Error("Available = false, want true (40 used < 80 threshold)")
	}
	if h.PctRemaining != 60 {
		t.Errorf("PctRemaining = %d, want 60", h.PctRemaining)
	}
	if h.ResetAt.IsZero() {
		t.Error("ResetAt unset, want the weekly resets_at")
	}
}

func TestClaudeProber_Exhausted(t *testing.T) {
	srv := claudeUsageServer(t, http.StatusOK, `{"limits":[{"kind":"weekly_all","percent":95,"resets_at":"2026-08-26T23:00:00Z"}]}`)
	h := ClaudeProber{ThresholdPct: 80, BaseURL: srv.URL, CredentialsPath: claudeCredsFile(t, "test-token")}.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if h.Available {
		t.Error("Available = true, want false (95 used >= 80 threshold)")
	}
}

func TestClaudeProber_Unauthenticated(t *testing.T) {
	srv := claudeUsageServer(t, http.StatusUnauthorized, `{"error":{"type":"authentication_error"}}`)
	h := ClaudeProber{ThresholdPct: 80, BaseURL: srv.URL, CredentialsPath: claudeCredsFile(t, "test-token")}.Probe(context.Background())
	if h.ProbeErr == nil {
		t.Fatal("ProbeErr = nil, want error")
	}
	if !h.Available {
		t.Error("Available = false, want true (fail-open)")
	}
}

func TestClaudeProber_ExpiredRefreshableTokenDoesNotProbeWithStaleAccessToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"stale-token","refreshToken":"refresh-token","expiresAt":%d}}`,
		time.Now().Add(-time.Minute).UnixMilli())
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	transport := &countingUnauthorizedTransport{}
	h := ClaudeProber{
		ThresholdPct:    80,
		BaseURL:         "https://example.invalid",
		Client:          &http.Client{Transport: transport},
		CredentialsPath: path,
	}.Probe(context.Background())
	if h.ProbeErr == nil || !strings.Contains(h.ProbeErr.Error(), "refresh token present") {
		t.Fatalf("ProbeErr = %v, want an explicit refreshable-expiry result", h.ProbeErr)
	}
	if !h.Available {
		t.Fatal("a refreshable expiry is inconclusive, not evidence of exhaustion")
	}
	if transport.calls != 0 {
		t.Fatalf("usage endpoint called %d times with a known-expired access token, want 0", transport.calls)
	}
}

func TestClaudeProber_EmptyToken(t *testing.T) {
	h := ClaudeProber{ThresholdPct: 80, CredentialsPath: claudeCredsFile(t, "")}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with error on empty accessToken")
	}
}

func TestClaudeProber_MissingCredentials(t *testing.T) {
	h := ClaudeProber{ThresholdPct: 80, CredentialsPath: filepath.Join(t.TempDir(), "nope.json")}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with error on missing credentials file")
	}
}

// fakeCodexAppServer installs a `codex` script that speaks the app-server
// JSON-RPC protocol: it answers the initialize handshake, then the
// account/rateLimits/read call with the given primary window.
func fakeCodexAppServer(t *testing.T, usedPercent int, resetsAt int64) {
	t.Helper()
	reply := fmt.Sprintf(`{"id":1,"result":{"rateLimits":{"primary":{"usedPercent":%d,"resetsAt":%d}}}}`,
		usedPercent, resetsAt)
	script := "#!/bin/sh\n" +
		"IFS= read -r line\n" +
		"printf '%s\\n' '{\"id\":0,\"result\":{\"userAgent\":\"fake/1\"}}'\n" +
		"IFS= read -r line\n" +
		fmt.Sprintf("printf '%%s\\n' '%s'\n", reply) +
		"exit 0\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCodexProber(t *testing.T) {
	fakeCodexAppServer(t, 30, 1787968245)
	p := CodexProber{ThresholdPct: 80}
	if p.Provider() != "openai" {
		t.Errorf("Provider = %q, want openai", p.Provider())
	}
	h := p.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if !h.Available {
		t.Error("Available = false, want true (30 used < 80 threshold)")
	}
	if h.PctRemaining != 70 {
		t.Errorf("PctRemaining = %d, want 70", h.PctRemaining)
	}
	if want := time.Unix(1787968245, 0).UTC(); !h.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v", h.ResetAt, want)
	}
}

func TestCodexProber_ExhaustedAndErrors(t *testing.T) {
	fakeCodexAppServer(t, 95, 1787968245)
	h := CodexProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if h.Available {
		t.Error("Available = true, want false (95 used >= 80 threshold)")
	}

	// A codex that exits without answering the rate-limits call is fail-open.
	fakeCLI(t, "codex", "garbage", 0)
	h = CodexProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with error on garbage output")
	}

	fakeCLI(t, "codex", "", 3)
	h = CodexProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with error on non-zero exit")
	}
}

func TestAgyProber(t *testing.T) {
	fakeCLI(t, "agy", "Weekly Limit Remaining: 55%", 0)
	p := AgyProber{ThresholdPct: 80}
	if p.Provider() != "google" {
		t.Errorf("Provider = %q, want google", p.Provider())
	}
	h := p.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if !h.Available {
		t.Error("Available = false, want true (45 used < 80 threshold)")
	}
	if h.PctRemaining != 55 {
		t.Errorf("PctRemaining = %d, want 55", h.PctRemaining)
	}
}

func TestAgyProber_ExhaustedAndErrors(t *testing.T) {
	fakeCLI(t, "agy", "Weekly Limit Remaining: 10%", 0)
	h := AgyProber{ThresholdPct: 80}.Probe(context.Background())
	if h.Available {
		t.Error("Available = true, want false (90 used >= 80 threshold)")
	}

	fakeCLI(t, "agy", "nope", 0)
	h = AgyProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with parse error on unmatched output")
	}

	fakeCLI(t, "agy", "x", 3)
	h = AgyProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with error on non-zero exit")
	}
}

func TestDeepSeekProber_BadJSONAndBadBalance(t *testing.T) {
	srv := deepSeekServer(t, http.StatusOK, `not-json`)
	h := DeepSeekProber{APIKey: "test-key", BaseURL: srv.URL}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open on unparseable JSON")
	}

	srv2 := deepSeekServer(t, http.StatusOK, `{"is_available":true,"total_balance":"NaN$"}`)
	h = DeepSeekProber{APIKey: "test-key", BaseURL: srv2.URL}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open on unparseable balance string")
	}
}

func TestDeepSeekProber_BalanceInfosFallback(t *testing.T) {
	srv := deepSeekServer(t, http.StatusOK, `{"is_available":true,"balance_infos":[{"total_balance":"3.50"}]}`)
	p := DeepSeekProber{APIKey: "test-key", BaseURL: srv.URL}
	if p.Provider() != "deepseek" {
		t.Errorf("Provider = %q, want deepseek", p.Provider())
	}
	h := p.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if !h.Available || h.PctRemaining != 100 {
		t.Errorf("Available=%v PctRemaining=%d, want true/100", h.Available, h.PctRemaining)
	}
}

func TestDeepSeekProber_ConnectionRefused(t *testing.T) {
	h := DeepSeekProber{APIKey: "test-key", BaseURL: "http://127.0.0.1:1"}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open when the balance endpoint is unreachable")
	}
}

func TestHeadroom_ProbeErrorAndMarshalJSON(t *testing.T) {
	h := Headroom{Provider: "anthropic", Available: true, PctRemaining: 42}
	if h.ProbeError() != "" {
		t.Errorf("ProbeError = %q, want empty", h.ProbeError())
	}
	h.ProbeErr = errors.New("probe blew up")
	if h.ProbeError() != "probe blew up" {
		t.Errorf("ProbeError = %q", h.ProbeError())
	}
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded["provider"] != "anthropic" {
		t.Errorf("provider = %v", decoded["provider"])
	}
	if decoded["probe_error"] != "probe blew up" {
		t.Errorf("probe_error = %v", decoded["probe_error"])
	}
	if decoded["pct_remaining"] != float64(42) {
		t.Errorf("pct_remaining = %v", decoded["pct_remaining"])
	}
}

func TestNewManager_DefaultProbers(t *testing.T) {
	cfg := config.RotationConfig{
		Enabled: true,
		Providers: map[string]config.ProviderRotationConfig{
			"anthropic": {Class: ClassSubscription, Backends: []string{"claude"}},
			"openai":    {Class: ClassSubscription, Backends: []string{"codex"}},
			"google":    {Class: ClassSubscription, Backends: []string{"agy"}},
			"deepseek":  {Class: ClassMetered, Backends: []string{"litellm"}},
			"unknown":   {Class: ClassMetered, Backends: []string{"other"}},
		},
	}
	m := NewManager(cfg)
	if len(m.probers) != 4 {
		t.Fatalf("len(probers) = %d, want 4 (unknown provider gets none)", len(m.probers))
	}
	got := map[string]bool{}
	for _, p := range m.probers {
		got[p.Provider()] = true
	}
	for _, want := range []string{"anthropic", "openai", "google", "deepseek"} {
		if !got[want] {
			t.Errorf("missing default prober for %q", want)
		}
	}
}

// stubProber records probes and returns a canned headroom.
type stubProber struct {
	name   string
	h      Headroom
	probed chan struct{}
}

func (s *stubProber) Provider() string { return s.name }

func (s *stubProber) Probe(context.Context) Headroom {
	select {
	case s.probed <- struct{}{}:
	default:
	}
	return s.h
}

func TestManager_StartProbesAndStores(t *testing.T) {
	m := NewManager(rotationTestConfig())
	stub := &stubProber{
		name:   "anthropic",
		h:      Headroom{Provider: "anthropic", Available: false, PctRemaining: 3},
		probed: make(chan struct{}, 1),
	}
	m.SetProbers([]Prober{stub})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	select {
	case <-stub.probed:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not probe within 5s")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		h := m.HeadroomFor("anthropic")
		if h.ProbeErr == nil {
			if h.Available || h.PctRemaining != 3 {
				t.Errorf("stored headroom = %+v", h)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe result never stored")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
}

func TestManager_HeadroomFor_NeverProbed(t *testing.T) {
	m := NewManager(rotationTestConfig())
	h := m.HeadroomFor("anthropic")
	if h.ProbeErr == nil {
		t.Fatal("ProbeErr = nil, want never-probed error")
	}
	if !h.Available {
		t.Error("Available = false, want true (fail-open)")
	}
	if !strings.Contains(h.ProbeErr.Error(), "never probed") {
		t.Errorf("ProbeErr = %v", h.ProbeErr)
	}
}

func TestManager_Exhausted(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false, PctRemaining: 0})
	if !m.Exhausted("claude") {
		t.Error("Exhausted = false, want true (positive exhaustion measurement)")
	}
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false, ProbeErr: errors.New("x")})
	if m.Exhausted("claude") {
		t.Error("Exhausted = true on probe error, want false")
	}
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: true, PctRemaining: 50})
	if m.Exhausted("claude") {
		t.Error("Exhausted = true for available provider, want false")
	}
}

func TestManager_StrandRecovered_UnknownBackend(t *testing.T) {
	m := NewManager(rotationTestConfig())
	if m.StrandRecovered("unmapped-backend") {
		t.Error("StrandRecovered = true for unmapped backend, want false")
	}
}

func TestManager_ShouldRotate_NoAlternative(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false})
	m.SetHeadroom(Headroom{Provider: "openai", Available: false})
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: false})
	if m.ShouldRotate("worker", "claude", 14400) {
		t.Error("ShouldRotate = true, want false (nowhere to go)")
	}
}

func TestManager_NextBackend_PrefersHighestHeadroom(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false})
	m.SetHeadroom(Headroom{Provider: "openai", Available: true, PctRemaining: 40})
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: true, PctRemaining: 100})
	if got := m.NextBackend("worker", "claude"); got != "litellm" {
		t.Errorf("NextBackend = %q, want litellm (highest headroom wins)", got)
	}
}

func TestManager_NextBackend_SkipsEmptyBackendProvider(t *testing.T) {
	cfg := rotationTestConfig()
	cfg.Providers["google"] = config.ProviderRotationConfig{Class: ClassSubscription}
	m := NewManager(cfg)
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false})
	m.SetHeadroom(Headroom{Provider: "google", Available: true, PctRemaining: 100})
	m.SetHeadroom(Headroom{Provider: "openai", Available: true, PctRemaining: 60})
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: false})
	if got := m.NextBackend("worker", "claude"); got != "codex" {
		t.Errorf("NextBackend = %q, want codex (google has no backends)", got)
	}
}

func TestRunCLI_MissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	out, err := runCLI(context.Background(), "definitely-not-a-real-binary-4232")
	if err == nil {
		t.Fatalf("err = nil, out = %q; want lookup error", out)
	}
}

// The default (no CredentialsPath override) must resolve $HOME/.claude/
// .credentials.json — the same file the claude CLI itself writes — before
// falling back to the shared /data/home location the hive main process needs
// in production (where HOME=/home/dev holds no CLI state).
func TestClaudeProber_DefaultCredentialsFromHome(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"claudeAiOauth":{"accessToken":"test-token"}}`
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	srv := claudeUsageServer(t, http.StatusOK, `{"limits":[{"kind":"weekly_all","percent":10,"resets_at":"2026-08-26T23:00:00Z"}]}`)
	h := ClaudeProber{ThresholdPct: 80, BaseURL: srv.URL}.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if !h.Available || h.PctRemaining != 90 {
		t.Errorf("got (avail=%v, pct=%d), want (true, 90)", h.Available, h.PctRemaining)
	}
}

func TestClaudeProber_CorruptCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := ClaudeProber{ThresholdPct: 80, CredentialsPath: path}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with error on corrupt credentials JSON")
	}
}

func TestClaudeProber_MalformedUsageBody(t *testing.T) {
	srv := claudeUsageServer(t, http.StatusOK, `{"limits": not-json`)
	h := ClaudeProber{ThresholdPct: 80, BaseURL: srv.URL, CredentialsPath: claudeCredsFile(t, "test-token")}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with error on unparseable usage body")
	}
}

// Limits without a percent (some kinds omit it) must be skipped, not counted
// as 0% used — the binding limit is the max percent among those that have one.
func TestClaudeProber_SkipsLimitsWithoutPercent(t *testing.T) {
	srv := claudeUsageServer(t, http.StatusOK, `{"limits":[
		{"kind":"session"},
		{"kind":"weekly_all","percent":85,"resets_at":"2026-08-26T23:00:00Z"}]}`)
	h := ClaudeProber{ThresholdPct: 80, BaseURL: srv.URL, CredentialsPath: claudeCredsFile(t, "test-token")}.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if h.Available {
		t.Error("Available = true, want false (85 >= 80 threshold from the percented limit)")
	}
}

// An app-server that answers with a JSON-RPC error object (auth failure,
// unsupported method) is a failed measurement — fail-open, never exhaustion.
func TestCodexProber_AppServerError(t *testing.T) {
	script := "#!/bin/sh\n" +
		"IFS= read -r line\n" +
		"printf '%s\\n' '{\"id\":0,\"error\":{\"code\":-32000,\"message\":\"not logged in\"}}'\n" +
		"exit 0\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := CodexProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with error on app-server JSON-RPC error")
	}
	if h.ProbeErr != nil && !strings.Contains(h.ProbeErr.Error(), "not logged in") {
		t.Errorf("ProbeErr = %v, want the app-server error surfaced", h.ProbeErr)
	}
}

func TestParseCodexRateLimits_Invalid(t *testing.T) {
	if _, _, err := parseCodexRateLimits([]byte("not-json")); err == nil {
		t.Error("err = nil, want parse error")
	}
}
