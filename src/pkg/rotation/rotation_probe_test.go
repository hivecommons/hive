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
	fakeCLI(t, "agy", `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"Gemini Models","buckets":[{"id":"gemini-weekly","window":"weekly","remaining_fraction":0.55,"reset_time":"2026-09-20T00:00:00Z"}]}]}}}`, 0)
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
	// #6986: agy's /usage envelope carries no plan tier, so none is claimed.
	if h.PlanType != "" {
		t.Errorf("PlanType = %q, want empty", h.PlanType)
	}
	// #6966: the emitted window must carry reset timing, which the old scraper
	// never did.
	if len(h.Limits) != 1 || h.Limits[0].ResetAt.IsZero() {
		t.Errorf("window carries no ResetAt; Limits=%+v", h.Limits)
	}
}

func TestAgyProber_ExhaustedAndErrors(t *testing.T) {
	fakeCLI(t, "agy", `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"Gemini Models","buckets":[{"id":"gemini-weekly","window":"weekly","remaining_fraction":0.10,"reset_time":"2026-09-20T00:00:00Z"}]}]}}}`, 0)
	h := AgyProber{ThresholdPct: 80}.Probe(context.Background())
	if h.Available {
		t.Error("Available = true, want false (90 used >= 80 threshold)")
	}

	// Decorative/text output from a CLI too old for --output-format json must
	// fail to parse and read as unknown, NOT as a confident number.
	fakeCLI(t, "agy", "Weekly Limit Remaining: 55%", 0)
	h = AgyProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with parse error on non-JSON (capability-absent) output")
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
	if _, err := codexHeadroom("openai", 80, []byte("not-json")); err == nil {
		t.Error("err = nil, want parse error")
	}
}

// ── kubestellar/hive#6952 ───────────────────────────────────────────────────

func codexPayload(t *testing.T, body string) json.RawMessage {
	t.Helper()
	return json.RawMessage(body)
}

func TestCodexHeadroomReadsBothWindowsAndDerivesKindFromDuration(t *testing.T) {
	// Durations are the authoritative discriminator: 300 min is the five-hour
	// window, 10080 min is the weekly one. The probe used to hard-code
	// "weekly" onto whichever window arrived as `primary`.
	h, err := codexHeadroom("openai", 80, codexPayload(t, `{
      "rateLimits": {
        "primary":   {"usedPercent": 10, "resetsAt": 1789400000, "windowDurationMins": 300},
        "secondary": {"usedPercent": 70, "resetsAt": 1789500000, "windowDurationMins": 10080},
        "planType": "pro"
      },
      "ordinaryUsageAllowed": true}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(h.Limits) != 2 {
		t.Fatalf("len(Limits) = %d, want 2 — the secondary window was invisible before #6952", len(h.Limits))
	}
	byID := map[string]LimitWindow{}
	for _, w := range h.Limits {
		byID[w.ID] = w
	}
	if got := byID["primary"].Kind; got != "five_hour" {
		t.Errorf("primary Kind = %q, want five_hour (derived from 300 min)", got)
	}
	if got := byID["secondary"].Kind; got != "weekly" {
		t.Errorf("secondary Kind = %q, want weekly (derived from 10080 min)", got)
	}
	if got := byID["primary"].DurationMins; got != 300 {
		t.Errorf("primary DurationMins = %d, want 300 — #6833's reading shape requires the duration", got)
	}
	// The binding window is the most-used one, else an exhausted secondary
	// passes unnoticed behind a roomy primary.
	if h.PctRemaining != 30 {
		t.Errorf("PctRemaining = %d, want 30 (the worse, secondary window)", h.PctRemaining)
	}
	if h.PlanType != "pro" {
		t.Errorf("PlanType = %q, want pro", h.PlanType)
	}
}

func TestCodexHeadroomSurfacesPaidCreditsWithoutEnablingThem(t *testing.T) {
	yes := `{"rateLimits":{"primary":{"usedPercent":5,"windowDurationMins":300},
	         "credits":{"available":true,"balance":42}},"ordinaryUsageAllowed":true}`
	h, err := codexHeadroom("openai", 80, codexPayload(t, yes))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if h.PaidCreditsAvailable == nil || !*h.PaidCreditsAvailable {
		t.Error("PaidCreditsAvailable should report the provider's own statement that credits exist")
	}
	// Absent credits must stay unknown, not collapse to "no".
	h2, err := codexHeadroom("openai", 80, codexPayload(t,
		`{"rateLimits":{"primary":{"usedPercent":5,"windowDurationMins":300}},"ordinaryUsageAllowed":true}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if h2.PaidCreditsAvailable != nil {
		t.Error("an absent credits block must stay nil (unknown), not read as false")
	}
}

func TestCodexHeadroomOrdinaryUsageAllowedIsNeverRecovery(t *testing.T) {
	// Explicitly false overrides healthy percentages.
	h, err := codexHeadroom("openai", 80, codexPayload(t,
		`{"rateLimits":{"primary":{"usedPercent":1,"windowDurationMins":300}},"ordinaryUsageAllowed":false}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if h.Available {
		t.Error("ordinaryUsageAllowed=false must remove headroom even at 1% used")
	}
	// Absent grants nothing it did not already have, and is carried as unknown.
	h2, err := codexHeadroom("openai", 80, codexPayload(t,
		`{"rateLimits":{"primary":{"usedPercent":1,"windowDurationMins":300}}}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if h2.OrdinaryUsageAllowed != nil {
		t.Error("absent ordinaryUsageAllowed must remain nil (unknown), never coerced to a bool")
	}
}

func TestCodexHeadroomRejectsUnrecognizedSchema(t *testing.T) {
	// A CLI too old to carry rateLimits, or a schema that moved on, must
	// surface as an error so the caller enters unknown-data behaviour. A
	// silent permissive reading is the failure mode #6833 calls worse than no
	// guard at all.
	for _, body := range []string{
		`{"rateLimits":{"planType":"pro"}}`,
		`{"somethingElse":{}}`,
	} {
		if _, err := codexHeadroom("openai", 80, codexPayload(t, body)); err == nil {
			t.Errorf("codexHeadroom(%s) err = nil, want an explicit unrecognized-schema error", body)
		}
	}
}

func TestNoCodexSpendOrBillingMutation(t *testing.T) {
	// #6833 criterion: no code path purchases credits. The consume endpoint
	// sits on the same app-server surface the probe already talks to, so its
	// absence deserves to be pinned rather than assumed.
	root := filepath.Join("..", "..")
	banned := []string{"rateLimitResetCredit", "/consume", "purchaseCredits"}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, s := range banned {
			if strings.Contains(string(b), s) {
				t.Errorf("%s references %q — no code path may spend credits or mutate billing", path, s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ── kubestellar/hive#6964 ───────────────────────────────────────────────────

// TestCodexHeadroomFoldsRateLimitsByLimitIDFromFixture drives the adapter with
// a recorded-shape payload from testdata (codex-cli 0.154.0's documented
// RateLimitSnapshot) rather than an inline literal, so the fixture is what an
// acceptance criterion asks for and the parse runs against the whole schema at
// once. It pins that rateLimitsByLimitId is folded in, that a positional
// window is not double-counted via its own limitId, and that an exhausted
// scoped window binds instead of hiding behind roomier primary/secondary ones.
func TestCodexHeadroomFoldsRateLimitsByLimitIDFromFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "codex_rate_limits_0.154.0.json"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := codexHeadroom("openai", 80, raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	byID := map[string]LimitWindow{}
	for _, w := range h.Limits {
		byID[w.ID] = w
	}
	// primary + secondary + exactly one scoped window: the two byLimitId
	// entries that repeat a positional limitId must be deduped away.
	if len(h.Limits) != 3 {
		t.Fatalf("len(Limits) = %d, want 3 (primary, secondary, one scoped); Limits=%+v", len(h.Limits), h.Limits)
	}
	scoped, ok := byID["codex-scoped-daily"]
	if !ok {
		t.Fatalf("rateLimitsByLimitId window codex-scoped-daily was dropped; Limits=%+v", h.Limits)
	}
	if scoped.Kind != "daily" {
		t.Errorf("scoped Kind = %q, want daily (derived from 1440 min)", scoped.Kind)
	}
	if got := scoped.Scope["limit_name"]; got != "Scoped daily usage" {
		t.Errorf("scoped limit_name = %q, want %q", got, "Scoped daily usage")
	}
	// The 90%-used scoped limit is the binding one; without folding it in, the
	// worst window is the 68%-used secondary and the guard would over-report.
	if h.PctRemaining != 10 {
		t.Errorf("PctRemaining = %d, want 10 (the 90%%-used scoped limit binds)", h.PctRemaining)
	}
	if h.Available {
		t.Error("Available should be false: a scoped limit is past the 80%% threshold")
	}
	if h.PlanType != "pro" {
		t.Errorf("PlanType = %q, want pro", h.PlanType)
	}
	if h.PaidCreditsAvailable == nil || !*h.PaidCreditsAvailable {
		t.Error("PaidCreditsAvailable should carry the provider's own credits.available")
	}
}

// ── kubestellar/hive#6965 ───────────────────────────────────────────────────

// TestClaudeHeadroomFromFixture drives ClaudeProber through the real HTTP parse
// path with a recorded-shape /api/oauth/usage body from testdata rather than an
// inline literal, so the fixture is what an acceptance criterion asks for and
// the whole schema is exercised at once. It pins reset timing, per-kind
// duration banding, and that the worst (most-used) window binds.
func TestClaudeHeadroomFromFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "claude_oauth_usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := claudeUsageServer(t, http.StatusOK, string(raw))
	h := ClaudeProber{ThresholdPct: 80, BaseURL: srv.URL, CredentialsPath: claudeCredsFile(t, "test-token")}.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if len(h.Limits) != 3 {
		t.Fatalf("len(Limits) = %d, want 3 (session, weekly_all, weekly_scoped); Limits=%+v", len(h.Limits), h.Limits)
	}
	byID := map[string]LimitWindow{}
	for _, w := range h.Limits {
		byID[w.ID] = w
	}
	// weekly_all normalizes to "weekly"; the scoped window keeps its own kind
	// so the relay guard's weekly/weekly_scoped reserves still match.
	if got := byID["weekly_all"].Kind; got != "weekly" {
		t.Errorf("weekly_all Kind = %q, want weekly", got)
	}
	// DurationMins completes #6833's reading shape and ties each window to the
	// shared codexWindowKind banding.
	if got := byID["session"].DurationMins; got != 300 {
		t.Errorf("session DurationMins = %d, want 300", got)
	}
	if got := byID["weekly_all"].DurationMins; got != 10080 {
		t.Errorf("weekly_all DurationMins = %d, want 10080", got)
	}
	// Emitted windows must carry reset timing so #6833's terminal message can
	// show it.
	if byID["weekly_all"].ResetAt.IsZero() {
		t.Error("weekly_all window carries no ResetAt")
	}
	// The 88%-used weekly_all window is the binding one; without worst-binds
	// the 37% session or 12% scoped window would over-report headroom.
	if h.PctRemaining != 12 {
		t.Errorf("PctRemaining = %d, want 12 (the 88%%-used weekly_all binds)", h.PctRemaining)
	}
	if !h.ResetAt.Equal(byID["weekly_all"].ResetAt) {
		t.Errorf("ResetAt = %v, want the binding weekly_all reset %v", h.ResetAt, byID["weekly_all"].ResetAt)
	}
	if h.Available {
		t.Error("Available should be false: the weekly_all window is past the 80% threshold")
	}
}

// TestClaudeHeadroomRejectsUnrecognizedSchema pins that an empty or
// percent-less limits array — or a body missing the field entirely — surfaces
// as an error (fail-open with ProbeErr set = unknown to the guard), never a
// confident healthy reading. Before #6965 the same input returned
// Available=true with PctRemaining=100 and no error: the silent permissive
// misread #6833 calls worse than no guard at all.
func TestClaudeHeadroomRejectsUnrecognizedSchema(t *testing.T) {
	for _, body := range []string{
		`{"limits":[]}`,
		`{"limits":[{"kind":"session"}]}`,
		`{"somethingElse":true}`,
	} {
		srv := claudeUsageServer(t, http.StatusOK, body)
		h := ClaudeProber{ThresholdPct: 80, BaseURL: srv.URL, CredentialsPath: claudeCredsFile(t, "test-token")}.Probe(context.Background())
		if h.ProbeErr == nil {
			t.Errorf("body %s: ProbeErr = nil, want an explicit unrecognized-schema error", body)
		}
		if len(h.Limits) != 0 {
			t.Errorf("body %s: unrecognized schema must yield no windows, got %+v", body, h.Limits)
		}
	}
}

// ── kubestellar/hive#6966 ───────────────────────────────────────────────────

// TestAgyHeadroomFromFixture drives AgyProber through the real parse path with
// a recorded-shape structured `/usage` payload from testdata rather than an
// inline literal, so the whole documented quota map is exercised at once. It
// pins the remaining_fraction→percent conversion, per-window duration banding,
// reset timing (which the old scraper never emitted), and that the worst
// (most-used) window binds.
func TestAgyHeadroomFromFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "agy_usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	fakeCLI(t, "agy", string(raw), 0)
	h := AgyProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	// With no Model hint every pool is folded in: 2 groups x 2 buckets.
	if len(h.Limits) != 4 {
		t.Fatalf("len(Limits) = %d, want 4 (gemini + 3p, weekly + 5h each); Limits=%+v", len(h.Limits), h.Limits)
	}
	byID := map[string]LimitWindow{}
	for _, w := range h.Limits {
		byID[w.ID] = w
	}
	// Bucket IDs stay group-qualified so the guard can name the pool that bound.
	for _, id := range []string{"gemini-weekly", "gemini-5h", "3p-weekly", "3p-5h"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("missing bucket %q; got %+v", id, h.Limits)
		}
	}
	// remaining_fraction 0.483... -> 48% remaining; window "weekly" bands to weekly.
	if got := byID["gemini-weekly"].PctRemaining; got != 48 {
		t.Errorf("gemini-weekly PctRemaining = %d, want 48 (from remaining_fraction 0.4831)", got)
	}
	if got := byID["gemini-weekly"].Kind; got != "weekly" {
		t.Errorf("gemini-weekly Kind = %q, want weekly (banded from 10080 min)", got)
	}
	if got := byID["gemini-weekly"].DurationMins; got != 10080 {
		t.Errorf("gemini-weekly DurationMins = %d, want 10080", got)
	}
	// window "5h" must band to five_hour; the payload carries no numeric duration.
	if got := byID["gemini-5h"].Kind; got != "five_hour" {
		t.Errorf("gemini-5h Kind = %q, want five_hour (banded from the \"5h\" label)", got)
	}
	if got := byID["gemini-5h"].DurationMins; got != 300 {
		t.Errorf("gemini-5h DurationMins = %d, want 300", got)
	}
	// #6966: emitted windows must carry reset timing, from `reset_time`.
	if byID["3p-weekly"].ResetAt.IsZero() {
		t.Error("3p-weekly carries no ResetAt (reset_time not read?)")
	}
	// The payload carries no plan tier at all (#6986); claiming one would be a guess.
	if h.PlanType != "" {
		t.Errorf("PlanType = %q, want empty: agy's /usage envelope has no plan field", h.PlanType)
	}
	// Worst binds across pools when no model is known: 3p-weekly at 9% remaining.
	if h.PctRemaining != 9 {
		t.Errorf("PctRemaining = %d, want 9 (3p-weekly binds)", h.PctRemaining)
	}
	if !h.ResetAt.Equal(byID["3p-weekly"].ResetAt) {
		t.Errorf("ResetAt = %v, want the binding 3p-weekly reset %v", h.ResetAt, byID["3p-weekly"].ResetAt)
	}
	if h.Available {
		t.Error("Available should be false: 3p-weekly is past the 80% threshold")
	}
}

// TestAgyHeadroomSelectsPoolForModel pins the two-pool behaviour #6986 found:
// agy meters Gemini and third-party Claude/GPT models against separate quotas,
// so a contributor running Gemini Flash must be measured against the Gemini
// pool. Without this, the exhausted 3p pool (9% remaining) binds and holds a
// relay whose actual pool is at 48% — safe, but needlessly idle.
func TestAgyHeadroomSelectsPoolForModel(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "agy_usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	fakeCLI(t, "agy", string(raw), 0)
	h := AgyProber{ThresholdPct: 80, Model: "gemini-flash"}.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if len(h.Limits) != 2 {
		t.Fatalf("len(Limits) = %d, want 2 (gemini pool only); Limits=%+v", len(h.Limits), h.Limits)
	}
	for _, w := range h.Limits {
		if !strings.HasPrefix(w.ID, "gemini-") {
			t.Errorf("bucket %q leaked in from another pool", w.ID)
		}
	}
	if h.PctRemaining != 48 {
		t.Errorf("PctRemaining = %d, want 48 (gemini-weekly binds, not the 9%% 3p pool)", h.PctRemaining)
	}
	if !h.Available {
		t.Error("Available should be true: the Gemini pool is at 48%, well inside the 80% threshold")
	}

	// A model that maps to the third-party pool sees the exhausted one.
	h = AgyProber{ThresholdPct: 80, Model: "claude-sonnet-4-6"}.Probe(context.Background())
	if h.PctRemaining != 9 {
		t.Errorf("claude model: PctRemaining = %d, want 9 (3p-weekly binds)", h.PctRemaining)
	}
	if h.Available {
		t.Error("claude model: Available should be false: the 3p pool is past the threshold")
	}

	// An unrecognized model must not silently drop the reading; it widens the
	// fold back to every pool (conservative), never picks one at random.
	h = AgyProber{ThresholdPct: 80, Model: "some-unknown-model"}.Probe(context.Background())
	if len(h.Limits) != 4 {
		t.Errorf("unknown model: len(Limits) = %d, want 4 (fold widens to all pools)", len(h.Limits))
	}
	if h.PctRemaining != 9 {
		t.Errorf("unknown model: PctRemaining = %d, want 9 (worst pool binds)", h.PctRemaining)
	}
}

// TestAgyHeadroomRejectsUnrecognizedSchema pins that a payload with no quota
// map, no windows, or windows without a remaining_fraction — as well as the
// decorative text the old scraper matched — surfaces as an error (fail-open
// with ProbeErr set = unknown to the guard), never a confident number. The
// earlier scraper's failure mode was a confident WRONG number, which #6833
// calls worse than no guard at all. This path does not depend on the
// accepted-shape field spellings, so it holds even if the fixture's schema is
// later corrected.
func TestAgyHeadroomRejectsUnrecognizedSchema(t *testing.T) {
	for _, body := range []string{
		`{"quota":{"plan":"pro"}}`,
		`{"quota":{"windows":{}}}`,
		`{"quota":{"windows":{"weekly":{"reset_at":"2026-09-20T00:00:00Z"}}}}`,
		`{"somethingElse":true}`,
		"Weekly Limit Remaining: 55%",
	} {
		if _, err := agyHeadroom("google", 80, []byte(body), ""); err == nil {
			t.Errorf("agyHeadroom(%s) err = nil, want an explicit unrecognized-schema error", body)
		}
	}
}

// TestAgyHeadroomErrorCauseDistinguishesDeadAdapter pins the #6986 primary
// defect fix: a SUCCESS envelope this adapter cannot map (a DEAD ADAPTER on a
// host that DOES have credentials) must categorize as unrecognized_schema,
// while an envelope the CLI returned with a non-SUCCESS status (the host has no
// credentials) must categorize as no_credentials. Before this, both produced an
// identical `unknown` and the dead adapter was invisible — it looked exactly
// like "no credentials on this host". The fail-safe direction is unchanged:
// both are still errors (a hold); only the reported cause differs.
func TestAgyHeadroomErrorCauseDistinguishesDeadAdapter(t *testing.T) {
	// A well-formed SUCCESS envelope whose quota shape drifted from what the
	// adapter maps: the dead-adapter signal.
	_, deadErr := agyHeadroom("google", 80, []byte(`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[]}}}`), "")
	if deadErr == nil {
		t.Fatal("schema-drift SUCCESS envelope: err = nil, want an error (must still hold)")
	}
	if got := probeErrorCause(deadErr); got != ProbeCauseUnrecognizedSchema {
		t.Errorf("schema drift cause = %q, want %q (a dead adapter must be nameable)", got, ProbeCauseUnrecognizedSchema)
	}

	// A non-SUCCESS envelope: the CLI ran and answered but is not serving usage,
	// most often because this host has no agy credentials.
	_, noCredErr := agyHeadroom("google", 80, []byte(`{"status":"ERROR","command":{"name":"usage"}}`), "")
	if noCredErr == nil {
		t.Fatal("non-SUCCESS envelope: err = nil, want an error (must still hold)")
	}
	if got := probeErrorCause(noCredErr); got != ProbeCauseNoCredentials {
		t.Errorf("non-SUCCESS cause = %q, want %q (unconfigured host, not a schema drift)", got, ProbeCauseNoCredentials)
	}

	if probeErrorCause(deadErr) == probeErrorCause(noCredErr) {
		t.Error("dead adapter and no-credentials collapsed to the same cause: the #6986 defect is not fixed")
	}

	// Non-JSON output (an old CLI's decorative text or a plain error line) is
	// unrecognized shape, not something we can pin to auth.
	_, textErr := agyHeadroom("google", 80, []byte("Weekly Limit Remaining: 55%"), "")
	if got := probeErrorCause(textErr); got != ProbeCauseUnrecognizedSchema {
		t.Errorf("decorative-text cause = %q, want %q", got, ProbeCauseUnrecognizedSchema)
	}
}

// TestAgyProberCauseNotInstalledVsDeadAdapter drives the whole Probe path and
// pins that the three failure states are told apart on the published-facing
// Headroom: a CLI absent from PATH is not_installed, a SUCCESS-but-unmappable
// payload is unrecognized_schema, and every one of them keeps failOpen's
// Available:true hold shape (kubestellar/hive#6986).
func TestAgyProberCauseNotInstalledVsDeadAdapter(t *testing.T) {
	// agy absent from PATH.
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)
	h := AgyProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Fatalf("missing CLI: want fail-open with ProbeErr set, got %+v", h)
	}
	if h.ProbeErrCause != ProbeCauseNotInstalled {
		t.Errorf("missing CLI cause = %q, want %q", h.ProbeErrCause, ProbeCauseNotInstalled)
	}

	// agy present but returning a SUCCESS envelope the adapter cannot map.
	fakeCLI(t, "agy", `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[]}}}`, 0)
	h = AgyProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Fatalf("dead adapter: want fail-open with ProbeErr set, got %+v", h)
	}
	if h.ProbeErrCause != ProbeCauseUnrecognizedSchema {
		t.Errorf("dead adapter cause = %q, want %q", h.ProbeErrCause, ProbeCauseUnrecognizedSchema)
	}
	if h.ProbeErrCause == ProbeCauseNotInstalled {
		t.Error("a dead adapter must not be reported as a missing CLI")
	}
}
