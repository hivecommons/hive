package main

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
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/linearagent"
	"github.com/hivecommons/hive/pkg/openrouter"
	"github.com/hivecommons/hive/pkg/watsonx"
)

// The adapters in provider_gateways.go are the only bridge between the
// dashboard's provider interfaces and the concrete pkg/watsonx, pkg/openrouter
// and pkg/linearagent implementations. Each method is a one-liner, which is
// exactly why a dropped field or swapped argument in one of them would survive
// every other test in the tree: nothing else exercises the glue.

// Compile-time pins: the adapters must keep satisfying the dashboard contracts.
var (
	_ dashboard.WatsonxGateway      = watsonxGateway{}
	_ dashboard.OpenRouterGateway   = openRouterGateway{}
	_ dashboard.OpenRouterFlowStore = openRouterFlowStore{}
	_ dashboard.LinearAgentGateway  = (*linearAgentService)(nil)
)

func TestWatsonxGatewayForwardsToPackage(t *testing.T) {
	gw := watsonxGateway{}

	if got, want := gw.EndpointForRegion("us-south"), watsonx.EndpointForRegion("us-south"); got != want {
		t.Errorf("EndpointForRegion = %q, want %q", got, want)
	}
	if got := gw.ProjectIDHeader(); got != watsonx.ProjectIDHeader {
		t.Errorf("ProjectIDHeader = %q, want %q", got, watsonx.ProjectIDHeader)
	}

	models := gw.GraniteFallbackModels()
	if len(models) != len(watsonx.GraniteFallbackModels) {
		t.Fatalf("GraniteFallbackModels len = %d, want %d", len(models), len(watsonx.GraniteFallbackModels))
	}
	for i := range models {
		if models[i] != watsonx.GraniteFallbackModels[i] {
			t.Errorf("GraniteFallbackModels[%d] = %q, want %q", i, models[i], watsonx.GraniteFallbackModels[i])
		}
	}
	// The adapter must hand out a copy: a caller mutating the slice must not
	// corrupt the package-level fallback list every later caller reads.
	models[0] = "mutated"
	if watsonx.GraniteFallbackModels[0] == "mutated" {
		t.Error("GraniteFallbackModels returned the package slice by reference; want a copy")
	}
}

func TestWatsonxGatewayMintTokenUsesDefaultMinter(t *testing.T) {
	var gotForm string
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAllBody(r)
		gotForm = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"iam-bearer-1","expires_in":3600}`))
	}))
	defer iam.Close()

	orig := watsonx.DefaultMinter
	watsonx.DefaultMinter = watsonx.NewTokenMinterForTest(iam.URL+"/identity/token", nil)
	t.Cleanup(func() { watsonx.DefaultMinter = orig })

	tok, err := watsonxGateway{}.MintToken(context.Background(), "ibm-api-key")
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if tok != "iam-bearer-1" {
		t.Errorf("MintToken = %q, want iam-bearer-1", tok)
	}
	if !strings.Contains(gotForm, "apikey=ibm-api-key") {
		t.Errorf("IAM request form = %q, want the API key forwarded", gotForm)
	}

	// Empty key is rejected by the minter before any network call.
	if _, err := (watsonxGateway{}).MintToken(context.Background(), ""); err == nil {
		t.Error("MintToken with empty key: want error, got nil")
	}
}

func TestOpenRouterGatewayStaticForwards(t *testing.T) {
	gw := openRouterGateway{}

	if got := gw.AuthURL(); got != openrouter.AuthURL {
		t.Errorf("AuthURL = %q, want %q", got, openrouter.AuthURL)
	}
	if got := gw.BaseURL(); got != openrouter.BaseURL {
		t.Errorf("BaseURL = %q, want %q", got, openrouter.BaseURL)
	}
	if got := gw.DefaultModel(); got != openrouter.DefaultModel {
		t.Errorf("DefaultModel = %q, want %q", got, openrouter.DefaultModel)
	}

	suggested := gw.SuggestedModels()
	if len(suggested) != len(openrouter.SuggestedModels) {
		t.Fatalf("SuggestedModels len = %d, want %d", len(suggested), len(openrouter.SuggestedModels))
	}
	for i, m := range openrouter.SuggestedModels {
		if suggested[i].ID != m.ID || suggested[i].Label != m.Label {
			t.Errorf("SuggestedModels[%d] = %+v, want ID=%q Label=%q", i, suggested[i], m.ID, m.Label)
		}
	}

	verifier, challenge, err := gw.GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	if verifier == "" || challenge == "" {
		t.Fatalf("GeneratePKCE returned empty verifier/challenge: %q / %q", verifier, challenge)
	}
	if got := openrouter.ChallengeFor(verifier); got != challenge {
		t.Errorf("challenge %q does not match ChallengeFor(verifier) %q", challenge, got)
	}

	authURL, err := gw.BuildAuthorizeURL("https://hive.example/callback", challenge, "state-1")
	if err != nil {
		t.Fatalf("BuildAuthorizeURL: %v", err)
	}
	want, err := openrouter.BuildAuthorizeURL("https://hive.example/callback", challenge, "state-1")
	if err != nil {
		t.Fatalf("openrouter.BuildAuthorizeURL: %v", err)
	}
	if authURL != want {
		t.Errorf("BuildAuthorizeURL = %q, want %q", authURL, want)
	}

	png, err := gw.QRPNG("https://hive.example/qr")
	if err != nil {
		t.Fatalf("QRPNG: %v", err)
	}
	if !bytes.HasPrefix(png, []byte("\x89PNG")) {
		t.Errorf("QRPNG did not return a PNG (prefix %q)", png[:min(8, len(png))])
	}
}

func TestOpenRouterGatewayExchangeCodeAndFetchCredit(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/keys":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["code"] != "code-1" || in["code_verifier"] != "verifier-1" {
				http.Error(w, "bad exchange body", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"key":"sk-or-user-key"}`))
		case "/key":
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"data":{"label":"sponsor","limit":10,"limit_remaining":7.5,"usage":2.5}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	origExchange, origInfo := openrouter.KeyExchangeURL, openrouter.KeyInfoURL
	openrouter.KeyExchangeURL = srv.URL + "/auth/keys"
	openrouter.KeyInfoURL = srv.URL + "/key"
	t.Cleanup(func() { openrouter.KeyExchangeURL, openrouter.KeyInfoURL = origExchange, origInfo })

	gw := openRouterGateway{}

	key, err := gw.ExchangeCode("code-1", "verifier-1")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if key != "sk-or-user-key" {
		t.Errorf("ExchangeCode = %q, want sk-or-user-key", key)
	}
	if _, err := gw.ExchangeCode("", "verifier-1"); err == nil {
		t.Error("ExchangeCode with empty code: want error, got nil")
	}

	credit, err := gw.FetchCredit("sk-or-user-key")
	if err != nil {
		t.Fatalf("FetchCredit: %v", err)
	}
	if gotAuth != "Bearer sk-or-user-key" {
		t.Errorf("key-info Authorization = %q, want Bearer sk-or-user-key", gotAuth)
	}
	if credit.Label != "sponsor" || credit.Usage != 2.5 {
		t.Errorf("FetchCredit label/usage = %q/%v, want sponsor/2.5", credit.Label, credit.Usage)
	}
	if credit.Limit == nil || *credit.Limit != 10 {
		t.Errorf("FetchCredit Limit = %v, want 10", credit.Limit)
	}
	if credit.LimitRemaining == nil || *credit.LimitRemaining != 7.5 {
		t.Errorf("FetchCredit LimitRemaining = %v, want 7.5", credit.LimitRemaining)
	}

	// Error path: the adapter must return the zero value, not a half-mapped
	// struct, when the package call fails.
	zero, err := gw.FetchCredit("")
	if err == nil {
		t.Fatal("FetchCredit with empty key: want error, got nil")
	}
	if zero != (dashboard.OpenRouterCredit{}) {
		t.Errorf("FetchCredit on error = %+v, want zero value", zero)
	}
}

func TestOpenRouterFlowStoreRoundTrip(t *testing.T) {
	store := openRouterGateway{}.NewFlowStore()
	if store == nil {
		t.Fatal("NewFlowStore returned nil")
	}

	state, err := store.Create("verifier-x", "hive-7", "deepseek/deepseek-chat")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if state == "" {
		t.Fatal("Create returned an empty state")
	}

	flow, ok := store.Consume(state)
	if !ok {
		t.Fatal("Consume(state) = false on first use, want true")
	}
	want := dashboard.OpenRouterFlow{Verifier: "verifier-x", HiveID: "hive-7", Model: "deepseek/deepseek-chat"}
	if flow != want {
		t.Errorf("Consume = %+v, want %+v", flow, want)
	}

	// States are single-use; a replay must fail with the zero flow.
	flow, ok = store.Consume(state)
	if ok {
		t.Error("Consume(state) succeeded on replay, want false")
	}
	if flow != (dashboard.OpenRouterFlow{}) {
		t.Errorf("Consume on miss = %+v, want zero value", flow)
	}
	if _, ok := store.Consume("never-issued"); ok {
		t.Error("Consume(unknown) = true, want false")
	}
}

func TestLinearStoredViewerIDHonoursStoreOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "linear-agent.json")
	t.Setenv(linearagent.StoreEnvVar, path)

	id, gotPath := linearStoredViewerID()
	if gotPath != path {
		t.Errorf("path = %q, want %q", gotPath, path)
	}
	if id != "" {
		t.Errorf("viewer id with no install = %q, want empty", id)
	}

	if err := os.WriteFile(path, []byte(`{"viewer_id":"usr_42"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	id, _ = linearStoredViewerID()
	if id != "usr_42" {
		t.Errorf("viewer id = %q, want usr_42", id)
	}
}

// fakeLinear stands in for both Linear endpoints the service talks to.
func fakeLinear(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if r.Form.Get("code") == "bad-code" {
				http.Error(w, `{"error":"invalid_grant","error_description":"code expired"}`, http.StatusBadRequest)
				return
			}
			if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "csecret" {
				http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"lin_access","refresh_token":"lin_refresh","expires_in":3600,"scope":"read"}`))
		case "/graphql":
			if r.Header.Get("Authorization") != "Bearer lin_access" {
				_, _ = w.Write([]byte(`{"errors":[{"message":"unauthenticated"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"usr_app"},"organization":{"id":"org_1","name":"Acme","urlKey":"acme"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestLinearService(t *testing.T, srv *httptest.Server, kicked *[]string) (*linearAgentService, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	ports := dashboard.LinearAgentPorts{
		Kick: func(agent, msg string) error {
			*kicked = append(*kicked, agent+":"+msg)
			return nil
		},
		ResolveSessionAgent: func() (string, error) { return "scanner", nil },
		TokenURL:            srv.URL + "/oauth/token",
		GraphqlURL:          srv.URL + "/graphql",
	}
	gw := newLinearAgentGateway(logger)(ports)
	svc, ok := gw.(*linearAgentService)
	if !ok {
		t.Fatalf("gateway factory returned %T, want *linearAgentService", gw)
	}
	return svc, &logs
}

func TestLinearAgentServiceInstallLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "linear-agent.json")
	t.Setenv(linearagent.StoreEnvVar, path)
	t.Setenv("LINEAR_CLIENT_ID", "cid")
	t.Setenv("LINEAR_CLIENT_SECRET", "csecret")
	srv := fakeLinear(t)

	var kicked []string
	svc, logs := newTestLinearService(t, srv, &kicked)

	if err := svc.StoreErr(); err != nil {
		t.Fatalf("StoreErr = %v, want nil", err)
	}
	if !svc.Configured() {
		t.Error("Configured = false with both env credentials set")
	}
	if !svc.HasInstallStore() {
		t.Error("HasInstallStore = false after a clean store open")
	}
	if svc.WebhookHandler() == nil {
		t.Error("WebhookHandler = nil, want the receiver")
	}
	if svc.AgentEventObserver() == nil {
		t.Error("AgentEventObserver = nil, want the responder hook")
	}

	// Pre-install: no install, no token, nothing active.
	if _, ok := svc.Install(); ok {
		t.Error("Install() = ok before CompleteInstall")
	}
	if _, err := svc.AccessToken(context.Background()); err == nil {
		t.Error("AccessToken before install: want error, got nil")
	}
	if _, _, ok := svc.ActiveSessionForIssue("ACME-1"); ok {
		t.Error("ActiveSessionForIssue = ok with no sessions")
	}
	snap, ok := svc.SessionsSnapshot()
	if !ok {
		t.Error("SessionsSnapshot ok = false, want true (tracker present)")
	}
	if sessions, isSlice := snap.([]linearagent.Session); !isSlice || len(sessions) != 0 {
		t.Errorf("SessionsSnapshot = %#v, want empty []linearagent.Session", snap)
	}

	// OAuth state round trip.
	state, err := svc.NewFlowState()
	if err != nil || state == "" {
		t.Fatalf("NewFlowState = %q, %v", state, err)
	}
	authURL := svc.AuthorizeURL("https://hive.example/cb", state)
	if want := linearagent.BuildAuthorizeURL("cid", "https://hive.example/cb", state); authURL != want {
		t.Errorf("AuthorizeURL = %q, want %q", authURL, want)
	}
	if !svc.ConsumeFlowState(state) {
		t.Error("ConsumeFlowState(state) = false on first use")
	}
	if svc.ConsumeFlowState(state) {
		t.Error("ConsumeFlowState(state) = true on replay")
	}

	// Code exchange failure must surface the error and persist nothing.
	if _, err := svc.CompleteInstall(context.Background(), "bad-code", "https://hive.example/cb"); err == nil {
		t.Fatal("CompleteInstall(bad-code): want error, got nil")
	}
	if !strings.Contains(logs.String(), "code exchange failed") {
		t.Errorf("exchange failure not logged; log: %s", logs.String())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("store file exists after failed exchange (stat err=%v)", err)
	}

	// Happy path: exchange, identity, persist.
	orgName, err := svc.CompleteInstall(context.Background(), "good-code", "https://hive.example/cb")
	if err != nil {
		t.Fatalf("CompleteInstall: %v", err)
	}
	if orgName != "Acme" {
		t.Errorf("CompleteInstall org = %q, want Acme", orgName)
	}

	inst, ok := svc.Install()
	if !ok {
		t.Fatal("Install() = !ok after CompleteInstall")
	}
	want := dashboard.LinearInstall{
		ViewerID:           "usr_app",
		OrganizationID:     "org_1",
		OrganizationName:   "Acme",
		OrganizationURLKey: "acme",
		ConnectedAt:        inst.ConnectedAt,
		HasAccessToken:     true,
	}
	if inst != want {
		t.Errorf("Install() = %+v, want %+v", inst, want)
	}
	if inst.ConnectedAt.IsZero() || time.Since(inst.ConnectedAt) > time.Minute {
		t.Errorf("ConnectedAt = %v, want roughly now", inst.ConnectedAt)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("install not persisted: %v", err)
	}
	if !strings.Contains(string(raw), `"usr_app"`) {
		t.Errorf("persisted install = %s, want viewer usr_app", raw)
	}

	tok, err := svc.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok != "lin_access" {
		t.Errorf("AccessToken = %q, want lin_access", tok)
	}

	// The probe used by worksource must see the same install.
	if id, _ := linearStoredViewerID(); id != "usr_app" {
		t.Errorf("linearStoredViewerID = %q, want usr_app", id)
	}

	if err := svc.ClearInstall(); err != nil {
		t.Fatalf("ClearInstall: %v", err)
	}
	if _, ok := svc.Install(); ok {
		t.Error("Install() = ok after ClearInstall")
	}
}

func TestLinearAgentServiceSessionTrackerBridge(t *testing.T) {
	t.Setenv(linearagent.StoreEnvVar, filepath.Join(t.TempDir(), "linear-agent.json"))
	t.Setenv("LINEAR_CLIENT_ID", "cid")
	t.Setenv("LINEAR_CLIENT_SECRET", "csecret")
	srv := fakeLinear(t)

	var kicked []string
	svc, _ := newTestLinearService(t, srv, &kicked)

	var ev linearagent.SessionEvent
	ev.AgentSession.ID = "sess-1"
	ev.AgentSession.Issue.ID = "iss-1"
	ev.AgentSession.Issue.Identifier = "ACME-1"
	svc.tracker.Observe(ev)
	svc.tracker.SetAgent("sess-1", "scanner")

	agent, id, ok := svc.ActiveSessionForIssue("ACME-1")
	if !ok || agent != "scanner" || id != "sess-1" {
		t.Errorf("ActiveSessionForIssue = (%q, %q, %v), want (scanner, sess-1, true)", agent, id, ok)
	}
	if _, _, ok := svc.ActiveSessionForIssue("ACME-2"); ok {
		t.Error("ActiveSessionForIssue(ACME-2) = ok, want miss")
	}
	snap, ok := svc.SessionsSnapshot()
	if sessions, isSlice := snap.([]linearagent.Session); !ok || !isSlice || len(sessions) != 1 || sessions[0].ID != "sess-1" {
		t.Errorf("SessionsSnapshot = %#v, %v; want one session sess-1", snap, ok)
	}

	// With no install token the responder's activity post fails and is
	// logged, but the bridge itself must not panic for either hook; an empty
	// URL is a documented no-op.
	svc.HandlePROpened("scanner", "hivecommons/hive", 1, "")
	svc.HandlePROpened("nobody", "hivecommons/hive", 1, "https://github.com/hivecommons/hive/pull/1")
	svc.AgentEventObserver()("scanner", "unrelated-event", "")
}

func TestLinearAgentServiceDegradesWhenStoreCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "linear-agent.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(linearagent.StoreEnvVar, path)
	t.Setenv("LINEAR_CLIENT_ID", "")
	t.Setenv("LINEAR_CLIENT_SECRET", "")
	srv := fakeLinear(t)

	var kicked []string
	svc, logs := newTestLinearService(t, srv, &kicked)

	if svc.StoreErr() == nil {
		t.Fatal("StoreErr = nil for a corrupt store, want error")
	}
	if !strings.Contains(logs.String(), "install store unreadable") {
		t.Errorf("store failure not logged; log: %s", logs.String())
	}
	if svc.Configured() {
		t.Error("Configured = true with no credentials in env")
	}
	if svc.HasInstallStore() {
		t.Error("HasInstallStore = true after store-open failure")
	}
	if svc.WebhookHandler() != nil {
		t.Error("WebhookHandler != nil without a receiver")
	}
	if svc.AgentEventObserver() != nil {
		t.Error("AgentEventObserver != nil without a responder")
	}
	if inst, ok := svc.Install(); ok || inst != (dashboard.LinearInstall{}) {
		t.Errorf("Install() = %+v, %v; want zero, false", inst, ok)
	}
	if _, err := svc.AccessToken(context.Background()); !errors.Is(err, errLinearClientUnavailable) {
		t.Errorf("AccessToken err = %v, want errLinearClientUnavailable", err)
	}
	if err := svc.ClearInstall(); !errors.Is(err, errLinearClientUnavailable) {
		t.Errorf("ClearInstall err = %v, want errLinearClientUnavailable", err)
	}
	if errLinearClientUnavailable.Error() != "linear client unavailable" {
		t.Errorf("errLinearClientUnavailable.Error() = %q", errLinearClientUnavailable.Error())
	}
	// The tracker is still constructed, so session reads keep working.
	if _, ok := svc.SessionsSnapshot(); !ok {
		t.Error("SessionsSnapshot ok = false; tracker should survive a store failure")
	}
	// Responder-dependent hooks are no-ops rather than nil dereferences.
	svc.HandlePROpened("scanner", "hivecommons/hive", 1, "https://github.com/hivecommons/hive/pull/1")
}

// TestLinearAgentServiceNilTrackerGuards covers the defensive nil-tracker branches, which the
// constructor never produces but the methods still guard.
func TestLinearAgentServiceNilTrackerGuards(t *testing.T) {
	svc := &linearAgentService{}
	if _, _, ok := svc.ActiveSessionForIssue("ACME-1"); ok {
		t.Error("ActiveSessionForIssue on nil tracker = ok")
	}
	if snap, ok := svc.SessionsSnapshot(); ok || snap != nil {
		t.Errorf("SessionsSnapshot on nil tracker = %v, %v; want nil, false", snap, ok)
	}
}

func readAllBody(r *http.Request) (string, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	return buf.String(), err
}
