package managedagents

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/tokens"
)

const (
	envPath         = "GET /v1/environments/env_private"
	envBody         = `{"id":"env_private","type":"self_hosted","version":"spoke-1"}`
	liveIncarnation = "env_private@spoke-1"
	listPath        = "GET /v1/sessions"
	createPath      = "POST /v1/sessions"
	session1Path    = "GET /v1/sessions/ses_1"
	events1Path     = "GET /v1/sessions/ses_1/events"
	interrupt1Path  = "POST /v1/sessions/ses_1/events"
	filesPath       = "GET /v1/files"
	file1Path       = "GET /v1/files/f1/content"
	runningSession1 = `{"id":"ses_1","status":"running","environment_id":"env_private"}`
	emptyList       = `{"data":[]}`
)

type fakeRoute struct {
	status int
	body   string
	hook   func()
}

// fakeVendor is a deterministic stand-in for the Managed Agents API keyed by
// "METHOD /path". Unknown routes reply 404.
type fakeVendor struct {
	mu     sync.Mutex
	routes map[string]fakeRoute
	calls  map[string]int
	server *httptest.Server
}

func newFakeVendor(t *testing.T) *fakeVendor {
	t.Helper()
	t.Setenv("MANAGED_AGENTS_TEST_KEY", "test-secret")
	f := &fakeVendor{routes: map[string]fakeRoute{}, calls: map[string]int{}}
	f.set(envPath, http.StatusOK, envBody)
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeVendor) set(key string, status int, body string) {
	f.setHook(key, status, body, nil)
}

func (f *fakeVendor) setHook(key string, status int, body string, hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[key] = fakeRoute{status: status, body: body, hook: hook}
}

func (f *fakeVendor) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (f *fakeVendor) serve(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	f.mu.Lock()
	route, ok := f.routes[key]
	f.calls[key]++
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if route.hook != nil {
		route.hook()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(route.status)
	_, _ = io.WriteString(w, route.body)
}

func (f *fakeVendor) adapter(t *testing.T) *Adapter {
	t.Helper()
	return newTestAdapter(t, f.server.URL, t.TempDir(), t.TempDir())
}

func (f *fakeVendor) adapterWithLedger(t *testing.T, ledger *tokens.ManagedAgentsLedger) *Adapter {
	t.Helper()
	cfg := testConfig(f.server.URL, t.TempDir(), t.TempDir(), "")
	cfg.Ledger = ledger
	return newTestAdapterWithConfig(t, cfg)
}

const boundKey = extwork.ExecutionKey("key-1")

// boundAdapter returns an adapter whose local state already binds boundKey to
// ses_1, so lookup goes straight to GET /v1/sessions/ses_1.
func (f *fakeVendor) boundAdapter(t *testing.T) *Adapter {
	t.Helper()
	a := f.adapter(t)
	bindSession(t, a, "ses_1", "")
	return a
}

func bindSession(t *testing.T, a *Adapter, sessionID, incarnation string) {
	t.Helper()
	if err := a.state.put(boundKey, sessionBinding{RequestDigest: "digest-1", SessionID: sessionID, Incarnation: incarnation}); err != nil {
		t.Fatal(err)
	}
}

func plainFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// breakDir replaces dir with a regular file so any later write beneath it
// fails, regardless of the uid running the test. It may run on the fake
// server's goroutine, so it reports with Errorf rather than Fatal.
func breakDir(t *testing.T, dir string) {
	if err := os.RemoveAll(dir); err != nil {
		t.Errorf("remove %s: %v", dir, err)
		return
	}
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Errorf("replace %s: %v", dir, err)
	}
}

func newLedger(t *testing.T, dir string, budget int64) *tokens.ManagedAgentsLedger {
	t.Helper()
	ledger, err := tokens.NewManagedAgentsLedger(dir, budget)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func bundlePayload(t *testing.T, adm extwork.Admission, summary, repo string) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"admission": map[string]any{
			"work_key":          adm.WorkKey,
			"assignment_id":     adm.AssignmentID,
			"generation":        adm.Generation,
			"stage":             adm.Stage,
			"contract_revision": adm.ContractRevision,
			"input_revision":    adm.InputRevision,
		},
		"summary": summary,
		"repo":    repo,
	})
}

func keyMetadataList(key extwork.ExecutionKey, extra string) string {
	return `{"data":[{"id":"ses_1","status":"running"` + extra + `,"metadata":{"hive_execution_key":"` + string(key) + `"}}]}`
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cases := map[string]func(t *testing.T, cfg *Config){
		"unparseable endpoint": func(_ *testing.T, cfg *Config) { cfg.Endpoint = "http://[::1" },
		"non-http scheme":      func(_ *testing.T, cfg *Config) { cfg.Endpoint = "ftp://127.0.0.1" },
		"missing host":         func(_ *testing.T, cfg *Config) { cfg.Endpoint = "https://" },
		"user info":            func(_ *testing.T, cfg *Config) { cfg.Endpoint = "https://user@api.example.com" },
		"query":                func(_ *testing.T, cfg *Config) { cfg.Endpoint = "https://api.example.com?x=1" },
		"fragment":             func(_ *testing.T, cfg *Config) { cfg.Endpoint = "https://api.example.com#frag" },
		"workflow version":     func(_ *testing.T, cfg *Config) { cfg.WorkflowVersion = " " },
		"agent id":             func(_ *testing.T, cfg *Config) { cfg.AgentID = "" },
		"agent version":        func(_ *testing.T, cfg *Config) { cfg.AgentVersion = 0 },
		"toolset version":      func(_ *testing.T, cfg *Config) { cfg.ToolsetVersion = "" },
		"environment id":       func(_ *testing.T, cfg *Config) { cfg.EnvironmentID = "" },
		"cloud environment":    func(_ *testing.T, cfg *Config) { cfg.EnvironmentType = "cloud" },
		"open network":         func(_ *testing.T, cfg *Config) { cfg.NetworkPolicy = "open" },
		"no key reference":     func(_ *testing.T, cfg *Config) { cfg.APIKeyEnv = "" },
		"relative key file":    func(_ *testing.T, cfg *Config) { cfg.APIKeyFile = "secrets/key" },
		"key file outside":     func(_ *testing.T, cfg *Config) { cfg.APIKeyFile = "/etc/managed-agents-key" },
		"key file lookalike":   func(_ *testing.T, cfg *Config) { cfg.APIKeyFile = "/data/secretsx/key" },
		"model":                func(_ *testing.T, cfg *Config) { cfg.Model = "" },
		"negative budget":      func(_ *testing.T, cfg *Config) { cfg.DailyBudgetCents = -1 },
		"budget above cap": func(_ *testing.T, cfg *Config) {
			cfg.DailyBudgetCents = tokens.DefaultManagedAgentsDailyBudgetCents + 1
		},
		"state dir":   func(_ *testing.T, cfg *Config) { cfg.StateDir = " " },
		"metrics dir": func(_ *testing.T, cfg *Config) { cfg.MetricsDir = "" },
		"unwritable metrics dir": func(t *testing.T, cfg *Config) {
			cfg.MetricsDir = filepath.Join(plainFile(t), "metrics")
		},
		"unwritable state dir": func(t *testing.T, cfg *Config) {
			cfg.StateDir = filepath.Join(plainFile(t), "state")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig("http://127.0.0.1:1", t.TempDir(), t.TempDir(), "")
			mutate(t, &cfg)
			if adapter, err := New(cfg); err == nil {
				t.Fatalf("config accepted: %+v", adapter)
			}
		})
	}
}

func TestNewAcceptsOptionalOverrides(t *testing.T) {
	ledger := newLedger(t, t.TempDir(), 100)
	client := &http.Client{}
	cfg := testConfig("https://api.example.com", t.TempDir(), t.TempDir(), "proxy")
	cfg.NetworkPolicy = " Proxy-Only "
	cfg.EnvironmentType = "SELF_HOSTED"
	cfg.APIKeyEnv = ""
	cfg.APIKeyFile = "/data/secrets/managed-agents/key"
	cfg.DailyBudgetCents = 100
	cfg.Ledger = ledger
	cfg.HTTPClient = client
	cfg.Now = nil
	a := newTestAdapterWithConfig(t, cfg)
	if a.ledger != ledger || a.client != client || a.now == nil {
		t.Fatalf("overrides not applied: %+v", a)
	}
	if a.networkPolicy != "proxy-only" || a.environmentType != "self_hosted" || a.githubWritePath != "proxy" {
		t.Fatalf("normalised settings = %q %q %q", a.networkPolicy, a.environmentType, a.githubWritePath)
	}
	if a.Engine() != Engine || a.WorkflowVersion() != "hosted/1" {
		t.Fatalf("identity = %q %q", a.Engine(), a.WorkflowVersion())
	}

	cfg = testConfig("http://127.0.0.1:1", t.TempDir(), t.TempDir(), "")
	cfg.APIKeyFile = "/secrets/managed-agents"
	a = newTestAdapterWithConfig(t, cfg)
	if a.client == nil || a.ledger == nil || a.ledger.BudgetCents() != tokens.DefaultManagedAgentsDailyBudgetCents {
		t.Fatal("default client or ledger not built")
	}
}

func factorySettings(stateDir, metricsDir string) map[string]string {
	return map[string]string{
		SettingEndpoint:          "http://127.0.0.1:1",
		SettingWorkflowVersion:   "hosted/1",
		SettingAgentID:           "agt_private",
		SettingAgentVersion:      "2",
		SettingToolsetVersion:    "tools/1",
		SettingEnvironmentID:     "env_private",
		SettingEnvironmentType:   "self_hosted",
		SettingNetworkPolicy:     "forced-egress",
		SettingAPIKeyEnv:         "MANAGED_AGENTS_TEST_KEY",
		SettingModel:             "claude-sonnet-4-6",
		SettingStateDir:          stateDir,
		SettingMetricsDir:        metricsDir,
		SettingDailyBudgetCents:  "100",
		SettingDirectGitHubWrite: "false",
		SettingGitHubWritePath:   "none",
	}
}

func TestFactory(t *testing.T) {
	adapter, err := Factory(factorySettings(t.TempDir(), t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if adapter.Engine() != Engine {
		t.Fatalf("factory engine = %q", adapter.Engine())
	}
	built := adapter.(*Adapter)
	if built.agentVersion != 2 || built.ledger.BudgetCents() != 100 {
		t.Fatalf("factory adapter = %+v", built)
	}

	settings := factorySettings(t.TempDir(), t.TempDir())
	settings[SettingDailyBudgetCents] = " "
	settings[SettingDirectGitHubWrite] = ""
	if _, err := Factory(settings); err != nil {
		t.Fatalf("optional settings omitted: %v", err)
	}

	cases := map[string][2]string{
		"missing agent version":  {SettingAgentVersion, ""},
		"textual agent version":  {SettingAgentVersion, "two"},
		"zero agent version":     {SettingAgentVersion, "0"},
		"textual budget":         {SettingDailyBudgetCents, "lots"},
		"textual direct write":   {SettingDirectGitHubWrite, "maybe"},
		"direct write requested": {SettingDirectGitHubWrite, "true"},
		"missing model":          {SettingModel, ""},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			settings := factorySettings(t.TempDir(), t.TempDir())
			settings[change[0]] = change[1]
			if _, err := Factory(settings); err == nil {
				t.Fatalf("settings %s=%q accepted", change[0], change[1])
			}
		})
	}
}

func TestAdmitRefusals(t *testing.T) {
	a := newTestAdapter(t, "http://127.0.0.1:1", t.TempDir(), t.TempDir())
	base, _ := hostedAdmission(t, "implement")

	invalid := base
	invalid.WorkKey = ""
	if err := a.Admit(invalid); !errors.Is(err, extwork.ErrInvalidAdmission) {
		t.Fatalf("invalid admission err = %v", err)
	}
	wrongEngine := base
	wrongEngine.Engine = "flue"
	if err := a.Admit(wrongEngine); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("wrong engine err = %v", err)
	}
	wrongCapability := base
	wrongCapability.Authority.Capability = "ext-exec/flue"
	if err := a.Admit(wrongCapability); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("wrong capability err = %v", err)
	}
	off := base
	off.Authority.Mode = extwork.ModeOff
	if err := a.Admit(off); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("off mode err = %v", err)
	}
	shadow := base
	shadow.Authority.Mode = extwork.ModeShadow
	if err := a.Admit(shadow); err != nil {
		t.Fatalf("shadow admission refused: %v", err)
	}
}

func TestIncarnationEnvironmentChecks(t *testing.T) {
	f := newFakeVendor(t)
	a := f.adapter(t)
	ctx := context.Background()

	if got, err := a.Incarnation(ctx); err != nil || got != liveIncarnation {
		t.Fatalf("incarnation = %q err=%v", got, err)
	}
	f.set(envPath, http.StatusOK, `{"id":"env_private","type":"self_hosted"}`)
	if got, err := a.Incarnation(ctx); err != nil || got != "env_private" {
		t.Fatalf("unversioned incarnation = %q err=%v", got, err)
	}
	f.set(envPath, http.StatusOK, `{"type":"self_hosted"}`)
	if _, err := a.Incarnation(ctx); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("missing id err = %v", err)
	}
	f.set(envPath, http.StatusOK, `{"id":"env_other","type":"self_hosted"}`)
	if _, err := a.Incarnation(ctx); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("recreated environment err = %v", err)
	}
	f.set(envPath, http.StatusOK, `{"id":"env_private","type":"cloud"}`)
	if _, err := a.Incarnation(ctx); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("cloud environment err = %v", err)
	}
	f.set(envPath, http.StatusInternalServerError, `{"type":"api_error"}`)
	var apiErr *apiError
	if _, err := a.Incarnation(ctx); !errors.As(err, &apiErr) || apiErr.status != http.StatusInternalServerError || apiErr.typ != "api_error" {
		t.Fatalf("server error = %v", err)
	}
	t.Setenv("MANAGED_AGENTS_TEST_KEY", "")
	if _, err := a.Incarnation(ctx); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("missing key err = %v", err)
	}
}

func TestSessionRequestRefusals(t *testing.T) {
	a := newTestAdapter(t, "http://127.0.0.1:1", t.TempDir(), t.TempDir())
	adm, payload := hostedAdmission(t, "implement")

	if _, err := a.sessionRequest(adm, []byte("{"), 100); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("bad bundle err = %v", err)
	}
	other := adm
	other.Generation = 2
	if _, err := a.sessionRequest(other, payload, 100); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("identity mismatch err = %v", err)
	}
	if _, err := a.sessionRequest(adm, bundlePayload(t, adm, " ", "hivecommons/hive"), 100); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("empty summary err = %v", err)
	}
	if _, err := a.sessionRequest(adm, bundlePayload(t, adm, "summary", ""), 100); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("empty repo err = %v", err)
	}
	if _, err := a.sessionRequest(adm, payload, 0); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("zero remaining err = %v", err)
	}
	body, err := a.sessionRequest(adm, payload, 42)
	if err != nil {
		t.Fatal(err)
	}
	metadata := body["metadata"].(map[string]string)
	if metadata["hive_execution_key"] != string(adm.ExecutionKey()) || metadata["hive_toolset_version"] != "tools/1" {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func TestStartRefusesBeforeDispatch(t *testing.T) {
	f := newFakeVendor(t)
	a := f.adapter(t)
	ctx := context.Background()
	adm, payload := hostedAdmission(t, "implement")

	wrongEngine := adm
	wrongEngine.Engine = "flue"
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: wrongEngine, Payload: payload}); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("wrong engine err = %v", err)
	}
	shadow := adm
	shadow.Authority.Mode = extwork.ModeShadow
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: shadow, Payload: payload}); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("shadow start err = %v", err)
	}
	version := adm
	version.WorkflowVersion = "hosted/2"
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: version, Payload: payload}); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("version mismatch err = %v", err)
	}
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: adm, Payload: append([]byte(" "), payload...)}); !errors.Is(err, extwork.ErrPayloadDigest) {
		t.Fatalf("digest mismatch err = %v", err)
	}
	if n := f.count(envPath); n != 0 {
		t.Fatalf("environment read %d times before admission passed", n)
	}

	f.set(envPath, http.StatusBadGateway, "bad gateway")
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: adm, Payload: payload}); err == nil {
		t.Fatal("start with unreadable environment succeeded")
	}
	f.set(envPath, http.StatusOK, envBody)
	f.set(listPath, http.StatusInternalServerError, "boom")
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: adm, Payload: payload}); err == nil || errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("list failure err = %v", err)
	}
	if n := f.count(createPath); n != 0 {
		t.Fatalf("session created %d times despite refusal", n)
	}
}

func TestStartBudgetAndLedgerFailures(t *testing.T) {
	ctx := context.Background()
	adm, payload := hostedAdmission(t, "implement")
	key := string(adm.ExecutionKey())

	t.Run("budget exhausted", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(listPath, http.StatusOK, emptyList)
		ledger := newLedger(t, t.TempDir(), 100)
		if _, err := ledger.ReserveSession("another-execution"); err != nil {
			t.Fatal(err)
		}
		a := f.adapterWithLedger(t, ledger)
		if _, err := a.Start(ctx, extwork.StartRequest{Admission: adm, Payload: payload}); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("exhausted budget err = %v", err)
		}
		if f.count(createPath) != 0 {
			t.Fatal("session created without budget")
		}
	})

	t.Run("stale reservation cannot be cleared", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(listPath, http.StatusOK, emptyList)
		metrics := t.TempDir()
		ledger := newLedger(t, metrics, 100)
		if _, err := ledger.ReserveSession(key); err != nil {
			t.Fatal(err)
		}
		breakDir(t, metrics)
		a := f.adapterWithLedger(t, ledger)
		_, err := a.Start(ctx, extwork.StartRequest{Admission: adm, Payload: payload})
		if !errors.Is(err, extwork.ErrTransport) || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("stale reservation err = %v", err)
		}
	})

	t.Run("reservation cannot be persisted", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(listPath, http.StatusOK, emptyList)
		metrics := t.TempDir()
		ledger := newLedger(t, metrics, 100)
		breakDir(t, metrics)
		a := f.adapterWithLedger(t, ledger)
		_, err := a.Start(ctx, extwork.StartRequest{Admission: adm, Payload: payload})
		if !errors.Is(err, extwork.ErrTransport) || !strings.Contains(err.Error(), "reserve") {
			t.Fatalf("reserve err = %v", err)
		}
	})

	t.Run("reservation cannot be rebound", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(listPath, http.StatusOK, emptyList)
		metrics := t.TempDir()
		ledger := newLedger(t, metrics, 100)
		f.setHook(createPath, http.StatusOK, runningSession1, func() { breakDir(t, metrics) })
		a := f.adapterWithLedger(t, ledger)
		result, err := a.Start(ctx, extwork.StartRequest{Admission: adm, Payload: payload})
		if !errors.Is(err, extwork.ErrTransport) || !strings.Contains(err.Error(), "bind") {
			t.Fatalf("rebind err = %v", err)
		}
		if result.RemoteRunID != "ses_1" {
			t.Fatalf("rebind result = %+v", result)
		}
	})

	t.Run("binding cannot be persisted", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(listPath, http.StatusOK, emptyList)
		f.set(createPath, http.StatusOK, runningSession1)
		a := f.adapter(t)
		a.state.path = filepath.Join(plainFile(t), "sessions.json")
		result, err := a.Start(ctx, extwork.StartRequest{Admission: adm, Payload: payload})
		if !errors.Is(err, extwork.ErrTransport) || result.RemoteRunID != "ses_1" || result.RemoteIncarnation != liveIncarnation {
			t.Fatalf("persist failure: result=%+v err=%v", result, err)
		}
	})
}

func TestStartCreateFailuresReleaseReservation(t *testing.T) {
	ctx := context.Background()
	adm, payload := hostedAdmission(t, "implement")
	cases := []struct {
		name   string
		status int
		body   string
		check  func(error) bool
	}{
		{"conflict", http.StatusConflict, `{"type":"conflict_error"}`, func(err error) bool { return errors.Is(err, extwork.ErrConflict) }},
		{"server error", http.StatusInternalServerError, `{"type":"api_error"}`, func(err error) bool {
			var apiErr *apiError
			return errors.As(err, &apiErr) && apiErr.status == http.StatusInternalServerError && !errors.Is(err, extwork.ErrConflict)
		}},
		{"missing id", http.StatusOK, `{"status":"running"}`, func(err error) bool { return errors.Is(err, extwork.ErrRefused) }},
		{"foreign environment", http.StatusOK, `{"id":"ses_1","environment_id":"env_other"}`, func(err error) bool {
			return errors.Is(err, extwork.ErrIncarnationMismatch)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVendor(t)
			f.set(listPath, http.StatusOK, emptyList)
			f.set(createPath, tc.status, tc.body)
			ledger := newLedger(t, t.TempDir(), 100)
			a := f.adapterWithLedger(t, ledger)
			if _, err := a.Start(ctx, extwork.StartRequest{Admission: adm, Payload: payload}); !tc.check(err) {
				t.Fatalf("start err = %v", err)
			}
			if got := ledger.RemainingDailyCents(); got != 100 {
				t.Fatalf("reservation leaked: remaining=%d", got)
			}
		})
	}

	t.Run("bundle refused", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(listPath, http.StatusOK, emptyList)
		ledger := newLedger(t, t.TempDir(), 100)
		a := f.adapterWithLedger(t, ledger)
		bad := adm
		badPayload := bundlePayload(t, bad, "", "hivecommons/hive")
		bad.RequestDigest = extwork.RequestDigest(badPayload)
		if _, err := a.Start(ctx, extwork.StartRequest{Admission: bad, Payload: badPayload}); !errors.Is(err, extwork.ErrRefused) {
			t.Fatalf("bundle err = %v", err)
		}
		if got := ledger.RemainingDailyCents(); got != 100 {
			t.Fatalf("reservation leaked: remaining=%d", got)
		}
		if f.count(createPath) != 0 {
			t.Fatal("refused bundle was dispatched")
		}
	})
}

func TestStartConflictsOnChangedPayload(t *testing.T) {
	ctx := context.Background()
	f := newFakeVendor(t)
	f.set(listPath, http.StatusOK, emptyList)
	f.set(createPath, http.StatusOK, runningSession1)
	a := f.adapter(t)
	adm, payload := hostedAdmission(t, "implement")
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: adm, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	changed := adm
	changedPayload := bundlePayload(t, changed, "A different request", "hivecommons/hive")
	changed.RequestDigest = extwork.RequestDigest(changedPayload)
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: changed, Payload: changedPayload}); !errors.Is(err, extwork.ErrConflict) {
		t.Fatalf("changed payload err = %v", err)
	}

	// Vendor metadata binding the key to another digest is also a conflict.
	f2 := newFakeVendor(t)
	f2.set(listPath, http.StatusOK, `{"data":[{"id":"ses_1","metadata":{"hive_execution_key":"`+string(adm.ExecutionKey())+`","hive_request_digest":"other"}}]}`)
	a2 := f2.adapter(t)
	if _, err := a2.Start(ctx, extwork.StartRequest{Admission: adm, Payload: payload}); !errors.Is(err, extwork.ErrConflict) {
		t.Fatalf("vendor metadata conflict err = %v", err)
	}
}

func TestLookupBindingBranches(t *testing.T) {
	ctx := context.Background()

	t.Run("session environment mismatch", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(session1Path, http.StatusOK, `{"id":"ses_1","status":"running","environment_id":"env_other"}`)
		a := f.boundAdapter(t)
		if _, err := a.Observe(ctx, boundKey, ""); !errors.Is(err, extwork.ErrIncarnationMismatch) {
			t.Fatalf("observe err = %v", err)
		}
	})

	t.Run("binding incarnation mismatch", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(session1Path, http.StatusOK, runningSession1)
		a := f.adapter(t)
		bindSession(t, a, "ses_1", "env_private@old")
		if _, err := a.Observe(ctx, boundKey, ""); !errors.Is(err, extwork.ErrIncarnationMismatch) {
			t.Fatalf("observe err = %v", err)
		}
	})

	t.Run("unsafe session id", func(t *testing.T) {
		f := newFakeVendor(t)
		a := f.adapter(t)
		bindSession(t, a, "../ses", "")
		if _, err := a.Observe(ctx, boundKey, ""); !errors.Is(err, extwork.ErrRefused) {
			t.Fatalf("observe err = %v", err)
		}
	})

	t.Run("session read failure", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(session1Path, http.StatusInternalServerError, "boom")
		a := f.boundAdapter(t)
		var apiErr *apiError
		if _, err := a.Observe(ctx, boundKey, ""); !errors.As(err, &apiErr) || apiErr.status != http.StatusInternalServerError {
			t.Fatalf("observe err = %v", err)
		}
	})

	t.Run("deleted session and no list", func(t *testing.T) {
		f := newFakeVendor(t)
		a := f.boundAdapter(t)
		if _, err := a.Observe(ctx, boundKey, ""); !errors.Is(err, extwork.ErrNotFound) {
			t.Fatalf("observe err = %v", err)
		}
		if f.count(listPath) != 1 {
			t.Fatalf("list scans = %d, want 1", f.count(listPath))
		}
	})

	t.Run("session id defaults to binding", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(session1Path, http.StatusOK, `{"status":"running","environment_id":"env_private"}`)
		f.set(events1Path, http.StatusOK, emptyList)
		a := f.boundAdapter(t)
		obs, err := a.Observe(ctx, boundKey, "")
		if err != nil {
			t.Fatal(err)
		}
		if obs.RemoteRunID != "ses_1" || obs.State != extwork.StateRunning {
			t.Fatalf("observation = %+v", obs)
		}
	})
}

func TestLookupListBranches(t *testing.T) {
	ctx := context.Background()

	t.Run("list failure", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(listPath, http.StatusInternalServerError, "boom")
		a := f.adapter(t)
		if _, err := a.Observe(ctx, boundKey, ""); err == nil || errors.Is(err, extwork.ErrNotFound) {
			t.Fatalf("observe err = %v", err)
		}
	})

	t.Run("no matching entries", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(listPath, http.StatusOK, `{"data":[{"id":"ses_x","metadata":{"hive_execution_key":"other"}},{"id":"","metadata":{"hive_execution_key":"`+string(boundKey)+`"}}]}`)
		a := f.adapter(t)
		if _, err := a.Observe(ctx, boundKey, ""); !errors.Is(err, extwork.ErrNotFound) {
			t.Fatalf("observe err = %v", err)
		}
	})

	t.Run("matching entry in another environment", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(listPath, http.StatusOK, keyMetadataList(boundKey, `,"environment_id":"env_other"`))
		a := f.adapter(t)
		if _, err := a.Observe(ctx, boundKey, ""); !errors.Is(err, extwork.ErrIncarnationMismatch) {
			t.Fatalf("observe err = %v", err)
		}
	})

	t.Run("reconciled binding cannot be persisted", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(listPath, http.StatusOK, keyMetadataList(boundKey, ""))
		a := f.adapter(t)
		a.state.path = filepath.Join(plainFile(t), "sessions.json")
		if _, err := a.Observe(ctx, boundKey, ""); !errors.Is(err, extwork.ErrTransport) {
			t.Fatalf("observe err = %v", err)
		}
	})

	t.Run("reconciled reservation cannot be rebound", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(listPath, http.StatusOK, keyMetadataList(boundKey, ""))
		metrics := t.TempDir()
		ledger := newLedger(t, metrics, 100)
		if _, err := ledger.ReserveSession(string(boundKey)); err != nil {
			t.Fatal(err)
		}
		breakDir(t, metrics)
		a := f.adapterWithLedger(t, ledger)
		if _, err := a.Observe(ctx, boundKey, ""); !errors.Is(err, extwork.ErrTransport) {
			t.Fatalf("observe err = %v", err)
		}
	})

	t.Run("reconciled entry is persisted", func(t *testing.T) {
		f := newFakeVendor(t)
		f.set(listPath, http.StatusOK, keyMetadataList(boundKey, ""))
		f.set(events1Path, http.StatusOK, emptyList)
		a := f.adapter(t)
		if _, err := a.Observe(ctx, boundKey, ""); err != nil {
			t.Fatal(err)
		}
		binding, ok := a.state.get(boundKey)
		if !ok || binding.SessionID != "ses_1" || binding.Incarnation != liveIncarnation {
			t.Fatalf("reconciled binding = %+v ok=%v", binding, ok)
		}
	})
}

func TestValidateSessionDigestConflict(t *testing.T) {
	a := newTestAdapter(t, "http://127.0.0.1:1", t.TempDir(), t.TempDir())
	view := sessionView{ID: "ses_1", Metadata: map[string]any{"hive_request_digest": "digest-a"}}
	if err := a.validateSession(view, sessionBinding{}, "", "digest-b"); !errors.Is(err, extwork.ErrConflict) {
		t.Fatalf("digest conflict err = %v", err)
	}
	if err := a.validateSession(view, sessionBinding{}, "", "digest-a"); err != nil {
		t.Fatalf("matching digest err = %v", err)
	}
}

func TestObservePreconditions(t *testing.T) {
	ctx := context.Background()
	f := newFakeVendor(t)
	a := f.boundAdapter(t)

	obs, err := a.Observe(ctx, boundKey, "env_private@old")
	if !errors.Is(err, extwork.ErrIncarnationMismatch) || obs.State != extwork.StateUnknown {
		t.Fatalf("pinned mismatch: obs=%+v err=%v", obs, err)
	}
	f.set(envPath, http.StatusInternalServerError, "boom")
	obs, err = a.Observe(ctx, boundKey, liveIncarnation)
	if err == nil || obs.State != extwork.StateUnknown {
		t.Fatalf("environment failure: obs=%+v err=%v", obs, err)
	}
}

func TestObserveMeteringFailures(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name         string
		session      string
		eventsStatus int
		events       string
	}{
		{"usage currency", `{"id":"ses_1","status":"running","usage":{"list_cost":{"amount":"5","currency":"EUR"}}}`, http.StatusOK, emptyList},
		{"usage fractional cents", `{"id":"ses_1","status":"running","usage":{"list_cost":{"amount":"1.5","currency":"USD"}}}`, http.StatusOK, emptyList},
		{"events unavailable", runningSession1, http.StatusInternalServerError, "boom"},
		{"event not an object", runningSession1, http.StatusOK, `{"data":[1]}`},
		{"usage event malformed", runningSession1, http.StatusOK, `{"data":[{"type":"session.usage","usage":1}]}`},
		{"model usage malformed", runningSession1, http.StatusOK, `{"data":[{"type":"span.model_request_end","id":"evt_1","model_usage":"many"}]}`},
		{"usage event currency", runningSession1, http.StatusOK, `{"data":[{"type":"session.usage","usage":{"list_cost":{"amount":1,"currency":"GBP"}}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVendor(t)
			f.set(session1Path, http.StatusOK, tc.session)
			f.set(events1Path, tc.eventsStatus, tc.events)
			a := f.boundAdapter(t)
			obs, err := a.Observe(ctx, boundKey, "")
			if err == nil || obs.State != extwork.StateUnknown || obs.RemoteRunID != "ses_1" {
				t.Fatalf("obs=%+v err=%v", obs, err)
			}
		})
	}
}

func TestObserveMetersEventVariantsAndTerminalReceipt(t *testing.T) {
	ctx := context.Background()
	f := newFakeVendor(t)
	f.set(session1Path, http.StatusOK, `{"id":"ses_1","status":"idle","stop_reason":{"type":"end_turn"},"environment_id":"env_private","result_class":"success","metadata":{"hive_stage":"implement","receipt_path":"receipt.json","receipt_digest":"abc","receipt_size":3}}`)
	f.set(events1Path, http.StatusOK, `[
		{"type":"session.usage","usage":{"list_cost":{"amount":10,"currency":"USD"},"active_seconds":2,"server_tool_use":{"web_search_requests":2,"web_searches":"n/a"}}},
		{"type":"span.model_request_start","id":"evt_start"},
		{"type":"span.model_request_end","model_request_start":{"model":"claude-opus-4-1"},"usage":{"input_tokens":"7","output_tokens":3}},
		{"type":"span.model_request_end","id":"evt_m","model":"claude-haiku-4-5","processed_at":"2026-09-24T11:00:00Z","model_usage":{"input_tokens":1,"output_tokens":-4}},
		{"type":"span.model_request_end","id":"evt_default"}
	]`)
	metrics := t.TempDir()
	ledger := newLedger(t, metrics, 100)
	if _, err := ledger.ReserveSession("ses_1"); err != nil {
		t.Fatal(err)
	}
	a := f.adapterWithLedger(t, ledger)
	a.agentID = "agt_private"
	bindSession(t, a, "ses_1", "")
	obs, err := a.Observe(ctx, boundKey, liveIncarnation)
	if err != nil {
		t.Fatal(err)
	}
	if obs.State != extwork.StateTerminal || obs.Detail != "end_turn" || obs.Stage != "implement" || obs.ResultClass != "success" {
		t.Fatalf("observation = %+v", obs)
	}
	if obs.Receipt == nil || obs.Receipt.Path != "receipt.json" || obs.Receipt.Digest != "abc" || obs.Receipt.Size != 3 {
		t.Fatalf("receipt = %+v", obs.Receipt)
	}
	if !obs.ObservedAt.Equal(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("observed at = %v", obs.ObservedAt)
	}
	if got := ledger.RemainingDailyCents(); got != 90 {
		t.Fatalf("remaining after terminal release = %d, want 90", got)
	}
	agg, err := tokens.CollectFromDir(metrics, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := agg.ByAgent["agt_private"]; got != 11 {
		t.Fatalf("metered tokens = %d, want 11", got)
	}

	// Re-observing must not double count deduplicated model events.
	if _, err := a.Observe(ctx, boundKey, ""); err != nil {
		t.Fatal(err)
	}
	agg, err = tokens.CollectFromDir(metrics, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := agg.ByAgent["agt_private"]; got != 11 {
		t.Fatalf("re-observed tokens = %d, want 11", got)
	}
}

func TestObserveBudgetHitWithoutRunningSession(t *testing.T) {
	ctx := context.Background()
	f := newFakeVendor(t)
	f.set(session1Path, http.StatusOK, `{"id":"ses_1","status":"idle","stop_reason":"requires_action","usage":{"list_cost":{"amount":"100","currency":"USD"}}}`)
	f.set(events1Path, http.StatusOK, `{"events":[]}`)
	a := f.adapterWithLedger(t, newLedger(t, t.TempDir(), 100))
	bindSession(t, a, "ses_1", "")
	obs, err := a.Observe(ctx, boundKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if obs.State != extwork.StateWaiting || obs.Detail != "daily ledger budget reached" {
		t.Fatalf("observation = %+v", obs)
	}
	if f.count(interrupt1Path) != 0 {
		t.Fatal("interrupt sent to a session that is not running")
	}
}

func TestObserveBudgetInterruptFailure(t *testing.T) {
	ctx := context.Background()
	f := newFakeVendor(t)
	f.set(session1Path, http.StatusOK, `{"id":"ses_1","status":"running","usage":{"list_cost":{"amount":"100","currency":"USD"}}}`)
	f.set(events1Path, http.StatusOK, emptyList)
	f.set(interrupt1Path, http.StatusInternalServerError, "boom")
	a := f.adapterWithLedger(t, newLedger(t, t.TempDir(), 100))
	bindSession(t, a, "ses_1", "")
	obs, err := a.Observe(ctx, boundKey, "")
	if !errors.Is(err, extwork.ErrTransport) || obs.State != extwork.StateUnknown || obs.RemoteRunID != "ses_1" {
		t.Fatalf("obs=%+v err=%v", obs, err)
	}
}

func TestObserveTerminalReleaseFailure(t *testing.T) {
	ctx := context.Background()
	f := newFakeVendor(t)
	f.set(session1Path, http.StatusOK, `{"id":"ses_1","status":"archived"}`)
	f.set(events1Path, http.StatusOK, emptyList)
	metrics := t.TempDir()
	ledger := newLedger(t, metrics, 100)
	if _, err := ledger.ReserveSession("ses_1"); err != nil {
		t.Fatal(err)
	}
	breakDir(t, metrics)
	a := f.adapterWithLedger(t, ledger)
	bindSession(t, a, "ses_1", "")
	obs, err := a.Observe(ctx, boundKey, "")
	if !errors.Is(err, extwork.ErrTransport) || obs.State != extwork.StateUnknown {
		t.Fatalf("obs=%+v err=%v", obs, err)
	}
}

func TestRecordEventLedgerError(t *testing.T) {
	a := newTestAdapter(t, "http://127.0.0.1:1", t.TempDir(), t.TempDir())
	if _, err := a.recordEvent("", json.RawMessage(`{"type":"span.model_request_end","id":"evt_1"}`)); err == nil {
		t.Fatal("event without session accepted")
	}
	hit, err := a.recordEvent("ses_1", json.RawMessage(`{"type":"agent.message"}`))
	if err != nil || hit {
		t.Fatalf("ignored event: hit=%v err=%v", hit, err)
	}
}

func TestCancelBranches(t *testing.T) {
	ctx := context.Background()
	f := newFakeVendor(t)
	f.set(session1Path, http.StatusOK, runningSession1)
	a := f.boundAdapter(t)

	f.set(interrupt1Path, http.StatusOK, `{}`)
	facts, err := a.Cancel(ctx, boundKey, "")
	if err != nil || !facts.Requested || !facts.Acknowledged || facts.Stopped {
		t.Fatalf("default reply facts=%+v err=%v", facts, err)
	}
	f.set(interrupt1Path, http.StatusOK, `{"acknowledged":false,"stopped":true,"detail":"stopped"}`)
	facts, err = a.Cancel(ctx, boundKey, liveIncarnation)
	if err != nil || facts.Acknowledged || !facts.Stopped || facts.Detail != "stopped" {
		t.Fatalf("explicit reply facts=%+v err=%v", facts, err)
	}
	f.set(interrupt1Path, http.StatusInternalServerError, "boom")
	facts, err = a.Cancel(ctx, boundKey, liveIncarnation)
	if err == nil || !facts.Requested || facts.Acknowledged || facts.Detail == "" {
		t.Fatalf("failed interrupt facts=%+v err=%v", facts, err)
	}
	if _, err := a.Cancel(ctx, boundKey, "env_private@old"); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("pinned mismatch err = %v", err)
	}
	if _, err := a.Cancel(ctx, extwork.ExecutionKey("unknown"), liveIncarnation); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("unknown key err = %v", err)
	}
	f.set(envPath, http.StatusInternalServerError, "boom")
	if _, err := a.Cancel(ctx, boundKey, liveIncarnation); err == nil {
		t.Fatal("pinned cancel with unreadable environment succeeded")
	}
	if _, err := a.Cancel(ctx, boundKey, ""); err == nil {
		t.Fatal("unpinned cancel with unreadable environment succeeded")
	}
}

func TestOpenArtifactBranches(t *testing.T) {
	ctx := context.Background()
	f := newFakeVendor(t)
	f.set(session1Path, http.StatusOK, runningSession1)
	a := f.boundAdapter(t)

	if _, err := a.OpenArtifact(ctx, boundKey, "", "../escape"); !errors.Is(err, extwork.ErrReceiptPath) {
		t.Fatalf("unclean path err = %v", err)
	}
	if _, err := a.OpenArtifact(ctx, boundKey, "env_private@old", "receipt.json"); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Fatalf("pinned mismatch err = %v", err)
	}
	if _, err := a.OpenArtifact(ctx, extwork.ExecutionKey("unknown"), liveIncarnation, "receipt.json"); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("unknown key err = %v", err)
	}
	if _, err := a.OpenArtifact(ctx, boundKey, "", "receipt.json"); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("missing file list err = %v", err)
	}
	f.set(filesPath, http.StatusInternalServerError, "boom")
	if _, err := a.OpenArtifact(ctx, boundKey, "", "receipt.json"); err == nil || errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("file list failure err = %v", err)
	}
	f.set(filesPath, http.StatusOK, `{"data":[{"id":"f1","filename":"/mnt/session/outputs/receipt.json"},{"id":"f2","filename":"other/receipt.json"}]}`)
	if _, err := a.OpenArtifact(ctx, boundKey, "", "receipt.json"); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("ambiguous artifact err = %v", err)
	}
	f.set(filesPath, http.StatusOK, `{"data":[{"id":"a/b","filename":"receipt.json"},{"id":"f3","filename":"notes.txt"}]}`)
	if _, err := a.OpenArtifact(ctx, boundKey, "", "receipt.json"); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("no matching artifact err = %v", err)
	}
	f.set(filesPath, http.StatusOK, `{"data":[{"id":"f1","filename":"outputs/patches/change.diff"}]}`)
	if _, err := a.OpenArtifact(ctx, boundKey, "", "patches/change.diff"); !errors.Is(err, extwork.ErrNotFound) {
		t.Fatalf("missing content err = %v", err)
	}
	f.set(file1Path, http.StatusInternalServerError, "boom")
	if _, err := a.OpenArtifact(ctx, boundKey, "", "patches/change.diff"); !errors.Is(err, extwork.ErrTransport) {
		t.Fatalf("content failure err = %v", err)
	}
	f.set(file1Path, http.StatusOK, "diff-body")
	body, err := a.OpenArtifact(ctx, boundKey, "", "patches/change.diff")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(raw) != "diff-body" {
		t.Fatalf("artifact body = %q err=%v", raw, err)
	}
	f.set(envPath, http.StatusInternalServerError, "boom")
	if _, err := a.OpenArtifact(ctx, boundKey, liveIncarnation, "receipt.json"); err == nil {
		t.Fatal("pinned open with unreadable environment succeeded")
	}
}

func TestOpenAuthenticatedFailures(t *testing.T) {
	ctx := context.Background()
	f := newFakeVendor(t)
	a := f.adapter(t)
	if _, err := a.openAuthenticated(ctx, "http://127.0.0.1/%zz"); err == nil {
		t.Fatal("invalid URL accepted")
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	if _, err := a.openAuthenticated(ctx, closedURL+"/v1/files/f1/content"); !errors.Is(err, extwork.ErrTransport) {
		t.Fatalf("closed server err = %v", err)
	}
	t.Setenv("MANAGED_AGENTS_TEST_KEY", "")
	if _, err := a.openAuthenticated(ctx, f.server.URL+"/v1/files/f1/content"); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("missing key err = %v", err)
	}
}

func TestAPIKeySources(t *testing.T) {
	a := newTestAdapter(t, "http://127.0.0.1:1", t.TempDir(), t.TempDir())
	dir := t.TempDir()

	a.apiKeyFile = filepath.Join(dir, "missing")
	if _, err := a.apiKey(); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("missing key file err = %v", err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.apiKeyFile = empty
	if _, err := a.apiKey(); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("empty key file err = %v", err)
	}
	full := filepath.Join(dir, "key")
	if err := os.WriteFile(full, []byte(" file-secret \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.apiKeyFile = full
	if key, err := a.apiKey(); err != nil || key != "file-secret" {
		t.Fatalf("file key = %q err=%v", key, err)
	}
	a.apiKeyFile = ""
	t.Setenv("MANAGED_AGENTS_TEST_KEY", " env-secret ")
	if key, err := a.apiKey(); err != nil || key != "env-secret" {
		t.Fatalf("env key = %q err=%v", key, err)
	}
	t.Setenv("MANAGED_AGENTS_TEST_KEY", "")
	if _, err := a.apiKey(); !errors.Is(err, extwork.ErrRefused) {
		t.Fatalf("empty env key err = %v", err)
	}
}

func TestDoJSONBranches(t *testing.T) {
	ctx := context.Background()
	f := newFakeVendor(t)
	a := f.adapter(t)

	if err := a.doJSON(ctx, http.MethodPost, f.server.URL+"/v1/x", make(chan int), nil); err == nil {
		t.Fatal("unencodable body accepted")
	}
	if err := a.doJSON(ctx, http.MethodGet, "http://127.0.0.1/%zz", nil, nil); err == nil {
		t.Fatal("invalid URL accepted")
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	if err := a.doJSON(ctx, http.MethodGet, closedURL+"/v1/x", nil, nil); !errors.Is(err, extwork.ErrTransport) {
		t.Fatalf("closed server err = %v", err)
	}

	f.set("GET /v1/typed", http.StatusBadRequest, `{"type":"invalid_request_error"}`)
	err := a.doJSON(ctx, http.MethodGet, f.server.URL+"/v1/typed", nil, nil)
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.typ != "invalid_request_error" || !strings.Contains(err.Error(), "400 invalid_request_error") {
		t.Fatalf("typed error = %v", err)
	}
	f.set("GET /v1/untyped", http.StatusTeapot, "short and stout")
	err = a.doJSON(ctx, http.MethodGet, f.server.URL+"/v1/untyped", nil, nil)
	if !errors.As(err, &apiErr) || apiErr.typ != "" || err.Error() != "managed agents replied 418: short and stout" {
		t.Fatalf("untyped error = %v", err)
	}

	f.set("POST /v1/discard", http.StatusOK, `{"ignored":true}`)
	if err := a.doJSON(ctx, http.MethodPost, f.server.URL+"/v1/discard", map[string]string{"a": "b"}, nil); err != nil {
		t.Fatalf("discarded reply err = %v", err)
	}
	f.set("GET /v1/garbled", http.StatusOK, `{garbled`)
	var out map[string]any
	if err := a.doJSON(ctx, http.MethodGet, f.server.URL+"/v1/garbled", nil, &out); !errors.Is(err, extwork.ErrTransport) {
		t.Fatalf("garbled reply err = %v", err)
	}
	f.set("GET /v1/empty", http.StatusOK, "")
	if err := a.doJSON(ctx, http.MethodGet, f.server.URL+"/v1/empty", nil, &out); err != nil {
		t.Fatalf("empty reply err = %v", err)
	}
}

func TestSessionStateStorePersistence(t *testing.T) {
	dir := t.TempDir()
	store, err := newSessionStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := sessionBinding{RequestDigest: "d", SessionID: "ses_1", Incarnation: liveIncarnation}
	if err := store.put(boundKey, want); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newSessionStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.get(boundKey); !ok || got != want {
		t.Fatalf("reloaded binding = %+v ok=%v", got, ok)
	}

	for _, raw := range []string{`{}`, `{"bindings":null}`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := newSessionStateStore(dir)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if err := store.put(boundKey, want); err != nil {
			t.Fatalf("%s: put: %v", raw, err)
		}
	}
}

func TestSessionStateStoreErrors(t *testing.T) {
	if _, err := newSessionStateStore(filepath.Join(plainFile(t), "state")); err == nil {
		t.Fatal("uncreatable state dir accepted")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sessions.json"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := newSessionStateStore(dir); err == nil || !strings.Contains(err.Error(), "read managed agents state") {
		t.Fatalf("unreadable state err = %v", err)
	}
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newSessionStateStore(dir); err == nil || !strings.Contains(err.Error(), "decode managed agents state") {
		t.Fatalf("corrupt state err = %v", err)
	}

	store, err := newSessionStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.path = t.TempDir()
	if err := store.put(boundKey, sessionBinding{SessionID: "ses_1"}); err == nil {
		t.Fatal("rename over a directory succeeded")
	}
}

func TestAtomicWriteFileFailures(t *testing.T) {
	if err := atomicWriteFile(filepath.Join(plainFile(t), "x", "y"), []byte("{}"), 0o600); err == nil {
		t.Fatal("write under a regular file succeeded")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(target, []byte("{}"), 0o600); err == nil {
		t.Fatal("rename over a directory succeeded")
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".managed-agents-state-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v err=%v", leftovers, err)
	}
}

func TestEventPageUnmarshal(t *testing.T) {
	var page eventPage
	if err := json.Unmarshal([]byte(`[{"type":"a"},{"type":"b"}]`), &page); err != nil || len(page.Events) != 2 {
		t.Fatalf("array page = %+v err=%v", page, err)
	}
	page = eventPage{}
	if err := json.Unmarshal([]byte(`{"data":[{"type":"a"}]}`), &page); err != nil || len(page.Events) != 1 || len(page.Data) != 1 {
		t.Fatalf("data page = %+v err=%v", page, err)
	}
	page = eventPage{}
	if err := json.Unmarshal([]byte(`{"data":[{"type":"a"},{"type":"b"}],"events":[{"type":"c"}]}`), &page); err != nil || len(page.Events) != 1 {
		t.Fatalf("events page = %+v err=%v", page, err)
	}
	if err := page.UnmarshalJSON([]byte(`[1,`)); err == nil {
		t.Fatal("truncated array accepted")
	}
	if err := page.UnmarshalJSON([]byte(`{"data":1}`)); err == nil {
		t.Fatal("non-array data accepted")
	}
}

func TestParseCents(t *testing.T) {
	cases := []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"null", 0, false},
		{`"125"`, 125, false},
		{`" 7 "`, 7, false},
		{`"1.50"`, 0, true},
		{`42`, 42, false},
		{`1.5`, 0, true},
		{`true`, 0, true},
	}
	for _, tc := range cases {
		got, err := parseCents(json.RawMessage(tc.raw))
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("parseCents(%s) = %d, %v", tc.raw, got, err)
		}
	}
}

func TestRawDecoders(t *testing.T) {
	ints := map[string]int64{"": 0, `5`: 5, `-3`: 0, `"9"`: 9, `" 11 "`: 11, `"-2"`: 0, `"x"`: 0, `true`: 0}
	for raw, want := range ints {
		if got := rawInt64(json.RawMessage(raw)); got != want {
			t.Errorf("rawInt64(%s) = %d, want %d", raw, got, want)
		}
	}
	strs := map[string]string{"": "", `" model "`: "model", `5`: ""}
	for raw, want := range strs {
		if got := rawString(json.RawMessage(raw)); got != want {
			t.Errorf("rawString(%s) = %q, want %q", raw, got, want)
		}
	}
	stops := map[string]string{"": "", "null": "", `{"type":"end_turn"}`: "end_turn", `"requires_action"`: "requires_action", `{}`: "", `5`: ""}
	for raw, want := range stops {
		if got := stopReasonType(json.RawMessage(raw)); got != want {
			t.Errorf("stopReasonType(%s) = %q, want %q", raw, got, want)
		}
	}
	fallback := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := parseEventTime("not a time", fallback); !got.Equal(fallback) {
		t.Errorf("parseEventTime fallback = %v", got)
	}
	if got := parseEventTime(" 2026-09-24T11:00:00Z ", fallback); !got.Equal(time.Date(2026, 9, 24, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("parseEventTime = %v", got)
	}
}

func TestMapSessionState(t *testing.T) {
	cases := []struct {
		status, stop string
		want         extwork.State
	}{
		{"created", "", extwork.StateAccepted},
		{"queued", "", extwork.StateAccepted},
		{"starting", "", extwork.StateAccepted},
		{" RUNNING ", "", extwork.StateRunning},
		{"rescheduling", "", extwork.StateWaiting},
		{"idle", "budget_reached", extwork.StateWaiting},
		{"idle", "requires_action", extwork.StateWaiting},
		{"idle", "end_turn", extwork.StateTerminal},
		{"terminated", "", extwork.StateTerminal},
		{"archived", "", extwork.StateTerminal},
		{"mystery", "", extwork.StateUnknown},
	}
	for _, tc := range cases {
		if got := mapSessionState(tc.status, tc.stop); got != tc.want {
			t.Errorf("mapSessionState(%q, %q) = %q, want %q", tc.status, tc.stop, got, tc.want)
		}
	}
}

func TestReceiptFromSession(t *testing.T) {
	explicit := &extwork.ReceiptRef{Path: "r.json", Digest: "d", Size: 1}
	if got := receiptFromSession(sessionView{Receipt: explicit}); got != explicit {
		t.Fatalf("explicit receipt = %+v", got)
	}
	meta := func(size any) sessionView {
		return sessionView{Metadata: map[string]any{"receipt_path": "r.json", "receipt_digest": "d", "receipt_size": size}}
	}
	if got := receiptFromSession(meta("12")); got == nil || got.Size != 12 {
		t.Fatalf("metadata receipt = %+v", got)
	}
	for _, size := range []any{"twelve", "-1"} {
		if got := receiptFromSession(meta(size)); got != nil {
			t.Fatalf("receipt with size %v = %+v", size, got)
		}
	}
	if got := receiptFromSession(sessionView{Metadata: map[string]any{"receipt_path": "r.json"}}); got != nil {
		t.Fatalf("partial receipt = %+v", got)
	}
}

func TestSmallHelpers(t *testing.T) {
	view := sessionView{Metadata: map[string]any{"n": float64(3), "s": "text"}}
	if sessionMetadata(view, "n") != "3" || sessionMetadata(view, "s") != "text" || sessionMetadata(view, "missing") != "" {
		t.Fatal("sessionMetadata mismatch")
	}
	for _, id := range []string{"", ".", "..", "a/b", `a\b`} {
		if safeID(id) {
			t.Errorf("safeID(%q) = true", id)
		}
	}
	if !safeID("ses_1") {
		t.Error("safeID(ses_1) = false")
	}
	for path, want := range map[string]bool{"/secrets/k": true, "/data/secrets": true, "/secrets": true, "/data/secretsx/k": false, "/etc/k": false} {
		if got := secretPath(path); got != want {
			t.Errorf("secretPath(%q) = %v", path, got)
		}
	}
	if !fileMatchesPath("/mnt/session/outputs/a/b.txt", "a/b.txt") || !fileMatchesPath("a/b.txt", "a/b.txt") || fileMatchesPath("c.txt", "a/b.txt") {
		t.Error("fileMatchesPath mismatch")
	}
	if pathBase("dir/") != "dir" || pathBase("plain") != "plain" || pathBase("a/b/c") != "c" {
		t.Error("pathBase mismatch")
	}
	if got := uQuery("%zz", map[string]string{"a": "b"}); got != "%zz" {
		t.Errorf("uQuery invalid = %q", got)
	}
	if got := uQuery("http://h/p?x=1", map[string]string{"y": "2"}); got != "http://h/p?x=1&y=2" {
		t.Errorf("uQuery = %q", got)
	}
	for host, want := range map[string]bool{"localhost": true, "127.0.0.2": true, "::1": true, "example.com": false, "10.0.0.1": false} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v", host, got)
		}
	}
	if (&apiError{status: 500, typ: "api_error", body: "x"}).Error() != "managed agents replied 500 api_error: x" {
		t.Error("typed apiError message")
	}
	for raw, want := range map[string]int{"3": 3, " 4 ": 4} {
		if got, err := parsePositiveInt(raw); err != nil || got != want {
			t.Errorf("parsePositiveInt(%q) = %d, %v", raw, got, err)
		}
	}
	if _, err := parseOptionalBool("yes"); err == nil {
		t.Error("parseOptionalBool(yes) accepted")
	}
	if got, err := parseOptionalInt64(" 9 "); err != nil || got != 9 {
		t.Errorf("parseOptionalInt64 = %d, %v", got, err)
	}
}
