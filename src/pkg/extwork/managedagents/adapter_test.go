package managedagents

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/tokens"
)

func TestStartReadsSelfHostedEnvironmentBeforeDispatchAndDeduplicates(t *testing.T) {
	t.Setenv("MANAGED_AGENTS_TEST_KEY", "test-secret")
	var mu sync.Mutex
	var calls []string
	var posted map[string]any
	adm, payload := hostedAdmission(t, "implement")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-secret" || r.Header.Get("anthropic-beta") != ManagedAgentsBetaHeader {
			t.Fatalf("missing managed agents authentication headers")
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/env_private":
			_, _ = w.Write([]byte(`{"id":"env_private","type":"self_hosted","version":"spoke-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			_, _ = w.Write([]byte(`{"id":"ses_private","status":"running","environment_id":"env_private"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/ses_private":
			_, _ = w.Write([]byte(`{"id":"ses_private","status":"running","environment_id":"env_private","environment_version":"spoke-1","metadata":{"hive_request_digest":"` + adm.RequestDigest + `"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := newTestAdapter(t, server.URL, t.TempDir(), t.TempDir())
	result, err := adapter.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemoteRunID != "ses_private" || result.RemoteIncarnation != "env_private@spoke-1" || result.Deduplicated {
		t.Fatalf("start result = %+v", result)
	}
	if posted["environment_id"] != "env_private" {
		t.Fatalf("create request environment = %#v", posted["environment_id"])
	}
	budget := posted["budget"].(map[string]any)["max_list_cost"].(map[string]any)
	if budget["amount"] != "2500" || budget["currency"] != "USD" {
		t.Fatalf("create request budget = %#v", budget)
	}
	if strings.Contains(string(mustJSON(t, posted)), "test-secret") {
		t.Fatal("API key was included in the session payload")
	}

	result, err = adapter.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Deduplicated || result.RemoteRunID != "ses_private" {
		t.Fatalf("duplicate start result = %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	posts := 0
	for _, call := range calls {
		if call == "POST /v1/sessions" {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("session POST count = %d, want 1; calls=%v", posts, calls)
	}
}

func TestCancelAndOpenArtifactUsePinnedSession(t *testing.T) {
	t.Setenv("MANAGED_AGENTS_TEST_KEY", "test-secret")
	adm, payload := hostedAdmission(t, "implement")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/env_private":
			_, _ = w.Write([]byte(`{"id":"env_private","type":"self_hosted","version":"spoke-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`{"id":"ses_artifact","status":"running","environment_id":"env_private"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/ses_artifact":
			_, _ = w.Write([]byte(`{"id":"ses_artifact","status":"running","environment_id":"env_private","environment_version":"spoke-1","metadata":{"hive_request_digest":"` + adm.RequestDigest + `"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/files":
			if r.URL.Query().Get("scope_id") != "ses_artifact" || r.URL.Query().Get("beta") != "true" {
				t.Fatalf("file scope query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"file_1","filename":"outputs/receipt.json"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/files/file_1/content":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("receipt-body"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/ses_artifact/events":
			var body struct {
				Events []struct {
					Type string `json:"type"`
				} `json:"events"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) != 1 || body.Events[0].Type != "user.interrupt" {
				t.Fatalf("interrupt body = %+v", body)
			}
			_, _ = w.Write([]byte(`{"acknowledged":true,"stopped":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := newTestAdapter(t, server.URL, t.TempDir(), t.TempDir())
	started, err := adapter.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := adapter.OpenArtifact(context.Background(), adm.ExecutionKey(), started.RemoteIncarnation, "receipt.json")
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	raw, err := io.ReadAll(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "receipt-body" {
		t.Fatalf("artifact body = %q", raw)
	}
	facts, err := adapter.Cancel(context.Background(), adm.ExecutionKey(), started.RemoteIncarnation)
	if err != nil {
		t.Fatal(err)
	}
	if !facts.Requested || !facts.Acknowledged || facts.Stopped {
		t.Fatalf("cancel facts = %+v", facts)
	}
}

func TestObserveMetersTokensDeduplicatesEventsAndStopsAtDailyBudget(t *testing.T) {
	t.Setenv("MANAGED_AGENTS_TEST_KEY", "test-secret")
	adm, payload := hostedAdmission(t, "implement")
	var interruptCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/env_private":
			_, _ = w.Write([]byte(`{"id":"env_private","type":"self_hosted","version":"spoke-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`{"id":"ses_private","status":"running","environment_id":"env_private"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/ses_private":
			_, _ = w.Write([]byte(`{"id":"ses_private","status":"running","environment_id":"env_private","environment_version":"spoke-1","metadata":{"hive_request_digest":"` + adm.RequestDigest + `","hive_stage":"implement"},"usage":{"list_cost":{"amount":"2500","currency":"USD"},"active_seconds":3}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/ses_private/events":
			_, _ = w.Write([]byte(`{"data":[{"id":"evt_model_1","type":"span.model_request_end","processed_at":"2026-09-24T12:00:01Z","model_usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/ses_private/events":
			interruptCalls++
			_, _ = w.Write([]byte(`{"acknowledged":true,"stopped":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	metricsDir := t.TempDir()
	adapter := newTestAdapter(t, server.URL, t.TempDir(), metricsDir)
	started, err := adapter.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := adapter.Observe(context.Background(), adm.ExecutionKey(), started.RemoteIncarnation)
	if err != nil {
		t.Fatal(err)
	}
	if obs.State != extwork.StateWaiting || !strings.Contains(obs.Detail, "budget") {
		t.Fatalf("budget observation = %+v", obs)
	}
	if interruptCalls != 1 {
		t.Fatalf("interrupt calls = %d, want 1", interruptCalls)
	}

	agg, err := tokens.CollectFromDir(metricsDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := agg.ByAgent["agt_private"]; got != 157 {
		t.Fatalf("metered tokens = %d, want 157", got)
	}
}

func TestNewRefusesDirectGitHubWritePaths(t *testing.T) {
	for _, path := range []string{"direct", "github_repository", "vaulted_secret", "github_mcp", "bogus"} {
		t.Run(path, func(t *testing.T) {
			if _, err := New(testConfig("http://127.0.0.1:1", t.TempDir(), t.TempDir(), path)); err == nil {
				t.Fatalf("write path %q accepted", path)
			}
		})
	}
	cfg := testConfig("http://127.0.0.1:1", t.TempDir(), t.TempDir(), "")
	cfg.DirectGitHubWrite = true
	if _, err := New(cfg); err == nil {
		t.Fatal("direct_github_write accepted")
	}
	for _, path := range []string{"", "none", "proxy"} {
		adapter := newTestAdapterWithConfig(t, testConfig("http://127.0.0.1:1", t.TempDir(), t.TempDir(), path))
		adm, _ := hostedAdmission(t, "implement")
		if err := adapter.Admit(adm); err != nil {
			t.Fatalf("path %q implement refused: %v", path, err)
		}
	}
}

func TestNewRequiresTLSForNonLoopbackEndpoint(t *testing.T) {
	if _, err := New(testConfig("http://api.example.com", t.TempDir(), t.TempDir(), "")); err == nil {
		t.Fatal("plaintext remote endpoint accepted")
	}
	if _, err := New(testConfig("https://api.example.com", t.TempDir(), t.TempDir(), "")); err != nil {
		t.Fatalf("https endpoint refused: %v", err)
	}
	for _, host := range []string{"http://localhost:1", "http://127.0.0.1:1", "http://[::1]:1"} {
		if _, err := New(testConfig(host, t.TempDir(), t.TempDir(), "")); err != nil {
			t.Fatalf("loopback %s refused: %v", host, err)
		}
	}
}

func TestObserveAfterStateWipeUsesEnvironmentIncarnation(t *testing.T) {
	t.Setenv("MANAGED_AGENTS_TEST_KEY", "test-secret")
	adm, payload := hostedAdmission(t, "implement")
	meta := `"metadata":{"hive_execution_key":"` + string(adm.ExecutionKey()) + `","hive_request_digest":"` + adm.RequestDigest + `"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/env_private":
			_, _ = w.Write([]byte(`{"id":"env_private","type":"self_hosted","version":"spoke-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`{"data":[{"id":"ses_private","status":"running","environment_id":"env_private",` + meta + `}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/ses_private":
			_, _ = w.Write([]byte(`{"id":"ses_private","status":"running","environment_id":"env_private",` + meta + `}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/ses_private/events":
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := newTestAdapter(t, server.URL, t.TempDir(), t.TempDir())
	result, err := adapter.Start(context.Background(), extwork.StartRequest{Admission: adm, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Deduplicated || result.RemoteIncarnation != "env_private@spoke-1" {
		t.Fatalf("start result = %+v", result)
	}
	// Fresh state dir simulates a crash that lost the local binding.
	adapter = newTestAdapter(t, server.URL, t.TempDir(), t.TempDir())
	for _, pinned := range []string{"", "env_private@spoke-1"} {
		obs, err := adapter.Observe(context.Background(), adm.ExecutionKey(), pinned)
		if err != nil {
			t.Fatalf("observe pinned=%q: %v", pinned, err)
		}
		if obs.RemoteIncarnation != "env_private@spoke-1" {
			t.Fatalf("observed incarnation = %q", obs.RemoteIncarnation)
		}
	}
}

func hostedAdmission(t *testing.T, stage string) (extwork.Admission, []byte) {
	t.Helper()
	adm := extwork.Admission{
		WorkKey:          "hivecommons/example#14124",
		AssignmentID:     "task-14124",
		Generation:       1,
		Stage:            stage,
		ContractRevision: "external-workflow-admission/v1",
		Engine:           Engine,
		WorkflowVersion:  "hosted/1",
		InputRevision:    "input-1",
		Authority: extwork.AuthorityBinding{
			Identity:   "private-relay",
			Tier:       "T3",
			Capability: Capability,
			Mode:       extwork.ModeReportOnly,
		},
	}
	payload, err := json.Marshal(map[string]any{
		"admission": map[string]any{
			"work_key":          adm.WorkKey,
			"assignment_id":     adm.AssignmentID,
			"generation":        adm.Generation,
			"stage":             adm.Stage,
			"contract_revision": adm.ContractRevision,
			"input_revision":    adm.InputRevision,
		},
		"summary": "Implement the requested change",
		"repo":    "hivecommons/hive",
	})
	if err != nil {
		t.Fatal(err)
	}
	adm.RequestDigest = extwork.RequestDigest(payload)
	return adm, payload
}

func newTestAdapter(t *testing.T, endpoint, stateDir, metricsDir string) *Adapter {
	t.Helper()
	return newTestAdapterWithConfig(t, testConfig(endpoint, stateDir, metricsDir, ""))
}

func testConfig(endpoint, stateDir, metricsDir, writePath string) Config {
	return Config{
		GitHubWritePath: writePath,
		Endpoint:        endpoint,
		WorkflowVersion: "hosted/1",
		AgentID:         "agt_private",
		AgentVersion:    1,
		ToolsetVersion:  "tools/1",
		EnvironmentID:   "env_private",
		EnvironmentType: "self_hosted",
		NetworkPolicy:   "forced-egress",
		APIKeyEnv:       "MANAGED_AGENTS_TEST_KEY",
		Model:           "claude-sonnet-4-6",
		StateDir:        stateDir,
		MetricsDir:      metricsDir,
		Now:             func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
	}
}

func newTestAdapterWithConfig(t *testing.T, cfg Config) *Adapter {
	t.Helper()
	adapter, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
