package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/openrouter"
)

// redirectKeyExchange points openrouter.KeyExchangeURL at a test server and
// restores it on cleanup, mirroring the geminiModelsURL seam.
func redirectKeyExchange(t *testing.T, url string) {
	t.Helper()
	orig := openrouter.KeyExchangeURL
	openrouter.KeyExchangeURL = url
	t.Cleanup(func() { openrouter.KeyExchangeURL = orig })
}

// redirectKeyInfo points openrouter.KeyInfoURL at a test server and restores
// it on cleanup.
func redirectKeyInfo(t *testing.T, url string) {
	t.Helper()
	orig := openrouter.KeyInfoURL
	openrouter.KeyInfoURL = url
	t.Cleanup(func() { openrouter.KeyInfoURL = orig })
}

// TestOpenRouterCallback_SuccessStoresGatewayAndRedirectsConnected drives the
// FULL happy path of handleOpenRouterCallback: state consume → PKCE code
// exchange (against a stubbed key-exchange endpoint) → gateway upsert → the
// ?openrouter=connected redirect. Before this test only the error redirects
// were exercised.
func TestOpenRouterCallback_SuccessStoresGatewayAndRedirectsConnected(t *testing.T) {
	covK2RedirectSecrets(t)
	s := covK2ORServer(t)

	var gotBody map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":"sk-or-exchanged"}`))
	}))
	t.Cleanup(ts.Close)
	redirectKeyExchange(t, ts.URL)

	state, err := s.openRouterState().Create("verifier-abc", "", "anthropic/claude-3.5-sonnet")
	if err != nil {
		t.Fatalf("create state: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleOpenRouterCallback(rec, httptest.NewRequest(http.MethodGet, "/openrouter/callback?code=the-code&state="+state, nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("success callback: expected 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "openrouter=connected") {
		t.Fatalf("Location = %q, want openrouter=connected flag", loc)
	}
	// The exchange must have carried the code + stored verifier.
	if gotBody["code"] != "the-code" || gotBody["code_verifier"] != "verifier-abc" {
		t.Fatalf("exchange body = %v, want code/the-code + verifier-abc", gotBody)
	}
	// The exchanged key must now resolve through the stored gateway.
	if key := s.resolveOpenRouterKey(); key != "sk-or-exchanged" {
		t.Fatalf("resolveOpenRouterKey = %q, want sk-or-exchanged", key)
	}
	// The chosen model must have been recorded on the gateway.
	gw := s.deps.Config.Governor.ResolveGateway(openRouterGatewayName)
	if gw == nil || gw.DefaultModel != "anthropic/claude-3.5-sonnet" {
		t.Fatalf("gateway = %+v, want default model anthropic/claude-3.5-sonnet", gw)
	}
	// State is single-use: replaying the same callback must fail.
	rec = httptest.NewRecorder()
	s.handleOpenRouterCallback(rec, httptest.NewRequest(http.MethodGet, "/openrouter/callback?code=the-code&state="+state, nil))
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "openrouter=error") {
		t.Fatalf("replayed state Location = %q, want openrouter=error", loc)
	}
}

// TestOpenRouterCallback_EmptyModelFallsBackToDefault covers the
// flow.Model == "" branch: the upserted gateway must carry openrouter.DefaultModel.
func TestOpenRouterCallback_EmptyModelFallsBackToDefault(t *testing.T) {
	covK2RedirectSecrets(t)
	s := covK2ORServer(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"key":"sk-or-default-model"}`))
	}))
	t.Cleanup(ts.Close)
	redirectKeyExchange(t, ts.URL)

	state, err := s.openRouterState().Create("verifier-def", "", "")
	if err != nil {
		t.Fatalf("create state: %v", err)
	}
	rec := httptest.NewRecorder()
	s.handleOpenRouterCallback(rec, httptest.NewRequest(http.MethodGet, "/openrouter/callback?code=c&state="+state, nil))
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "openrouter=connected") {
		t.Fatalf("Location = %q, want openrouter=connected", loc)
	}
	gw := s.deps.Config.Governor.ResolveGateway(openRouterGatewayName)
	if gw == nil || gw.DefaultModel != openrouter.DefaultModel {
		t.Fatalf("gateway = %+v, want default model %q", gw, openrouter.DefaultModel)
	}
}

// TestOpenRouterCallback_HiveMismatchRejected covers the hive-binding guard: a
// flow started for a DIFFERENT hive must be rejected before any key exchange.
func TestOpenRouterCallback_HiveMismatchRejected(t *testing.T) {
	s := covK2ORServer(t)
	s.deps.Config.HiveID = "self-hive"

	exchangeCalled := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchangeCalled = true
		_, _ = w.Write([]byte(`{"key":"sk-should-never-mint"}`))
	}))
	t.Cleanup(ts.Close)
	redirectKeyExchange(t, ts.URL)

	state, err := s.openRouterState().Create("verifier-x", "other-hive", "")
	if err != nil {
		t.Fatalf("create state: %v", err)
	}
	rec := httptest.NewRecorder()
	s.handleOpenRouterCallback(rec, httptest.NewRequest(http.MethodGet, "/openrouter/callback?code=c&state="+state, nil))
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "openrouter=error") {
		t.Fatalf("mismatch Location = %q, want openrouter=error", loc)
	}
	if exchangeCalled {
		t.Fatal("key exchange must never run for a mismatched hive")
	}
}

// TestOpenRouterCallback_UpsertFailureRedirectsError covers the gateway-store
// failure branch: the exchange succeeds but storeGatewayAPIKey cannot write
// (unwritable secrets dir), so the callback must redirect with the error flag.
func TestOpenRouterCallback_UpsertFailureRedirectsError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory write bits; cannot make the secrets dir unwritable")
	}
	unwritable := t.TempDir()
	if err := os.Chmod(unwritable, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unwritable, 0o755) })
	t.Cleanup(setGatewaySecretsDirForTest(unwritable))
	s := covK2ORServer(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"key":"sk-or-unstorable"}`))
	}))
	t.Cleanup(ts.Close)
	redirectKeyExchange(t, ts.URL)

	state, err := s.openRouterState().Create("verifier-y", "", "")
	if err != nil {
		t.Fatalf("create state: %v", err)
	}
	rec := httptest.NewRecorder()
	s.handleOpenRouterCallback(rec, httptest.NewRequest(http.MethodGet, "/openrouter/callback?code=c&state="+state, nil))
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "openrouter=error") {
		t.Fatalf("upsert-failure Location = %q, want openrouter=error", loc)
	}
	if key := s.resolveOpenRouterKey(); key != "" {
		t.Fatalf("no gateway should resolve after a failed store, got key %q", key)
	}
}

// TestOpenRouterCredit_SuccessReturnsCreditFields covers the successful
// FetchCredit branch of handleOpenRouterCredit (connected + label/limit/usage),
// previously only the not-connected and fetch-error branches ran.
func TestOpenRouterCredit_SuccessReturnsCreditFields(t *testing.T) {
	covK2RedirectSecrets(t)
	s := covK2ORServer(t)
	if err := s.upsertOpenRouterGateway("sk-or-credit-key", "openrouter/auto"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":{"label":"hive key","limit":25,"limit_remaining":20.5,"usage":4.5}}`))
	}))
	t.Cleanup(ts.Close)
	redirectKeyInfo(t, ts.URL)

	rec := doGet(s, "/api/openrouter/credit")
	if rec.Code != http.StatusOK {
		t.Fatalf("credit: %d", rec.Code)
	}
	if gotAuth != "Bearer sk-or-credit-key" {
		t.Fatalf("Authorization = %q, want Bearer sk-or-credit-key", gotAuth)
	}
	var out struct {
		Connected      bool     `json:"connected"`
		Label          string   `json:"label"`
		Limit          *float64 `json:"limit"`
		LimitRemaining *float64 `json:"limit_remaining"`
		Usage          float64  `json:"usage"`
		Error          string   `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if !out.Connected || out.Error != "" {
		t.Fatalf("want connected with no error, got %+v", out)
	}
	if out.Label != "hive key" || out.Limit == nil || *out.Limit != 25 ||
		out.LimitRemaining == nil || *out.LimitRemaining != 20.5 || out.Usage != 4.5 {
		t.Fatalf("credit fields = %+v, want label/limit/remaining/usage from stub", out)
	}
}
