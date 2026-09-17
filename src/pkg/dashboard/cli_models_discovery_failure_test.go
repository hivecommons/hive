package dashboard

// hivecommons/hive#7384: model discovery failure was silent. On the
// projectbluefin spoke the installed Copilot SDK helper failed on arm64
// (#7365), discovery fell through to the raw-HTTP chat-completions probe —
// a LIVE result that is a different catalog from the CLI's — the dropdown
// rendered it as authoritative, and the browser-side auto-heal moved all six
// agents onto gpt-4o-mini off it. No audit entry, no dropdown error, for two
// days. These tests hold the three fixes: the audit log records the failure,
// the wire carries the error for the dropdown, and neither a fallback nor a
// degraded list is treated as authority.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// arm64Failure is the helper error verbatim from the #7365 spoke logs.
var arm64Failure = errors.New("sdk helper: exit status 1 (stderr: copilot-models: Could not find a @github/copilot platform package (tried @github/copilot-linux-arm64))")

// healthyCopilotAPI stands in for api.github.com + the Copilot API host and
// answers the HTTP probe with the legacy chat-completions catalog.
func healthyCopilotAPI(t *testing.T, ids ...string) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, copilotModelsPath) {
			var data []map[string]any
			for _, id := range ids {
				data = append(data, map[string]any{"id": id, "capabilities": map[string]any{"type": "chat"}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			return
		}
		_, _ = w.Write([]byte(`{"endpoints":{"api":"` + srv.URL + `"}}`))
	}))
	t.Cleanup(srv.Close)
	prev := copilotUserEndpointURL
	copilotUserEndpointURL = srv.URL + "/copilot_internal/user"
	t.Cleanup(func() { copilotUserEndpointURL = prev })
}

// The incident shape: helper installed and failing, HTTP probe succeeding.
// The result must be DEGRADED — served, but not authority.
func TestDiscoverCopilotModels_SDKFailureWithHTTPSuccessIsDegraded(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "ghu_test")
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) { return nil, arm64Failure })
	healthyCopilotAPI(t, "gpt-4o-mini-2024-07-18", "gpt-4o")
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}

	r := s.discoverCopilotModels()
	if r.fallback {
		t.Fatalf("HTTP success must not be a static fallback: %+v", r)
	}
	if !r.degraded || r.authoritative() {
		t.Fatalf("HTTP catalog after an installed-helper failure must be degraded, got %+v", r)
	}
	if !strings.Contains(r.discoveryErr, "Could not find a @github/copilot platform package") {
		t.Errorf("discoveryErr must carry the helper's own words, got %q", r.discoveryErr)
	}
	if !contains(r.models, "gpt-4o-mini-2024-07-18") {
		t.Errorf("degraded list must still be served: %v", r.models)
	}

	// Through the full pipeline: not fed to retention, not usable for
	// validation — the two places a "live" list is treated as truth.
	q := s.queryCLIModels("copilot")
	if !q.degraded {
		t.Fatalf("queryCLIModels must preserve degraded: %+v", q)
	}
	if got := s.modelIDsForBackend("copilot"); got != nil {
		t.Errorf("a degraded catalog must be unverifiable for model validation, got %v", got)
	}
	if len(s.cliModels.retained["copilot"]) != 0 {
		t.Errorf("a degraded catalog must not feed retention: %v", s.cliModels.retained["copilot"])
	}
}

// Helper ABSENT (dev machine, CI) with HTTP success stays a clean live
// result — that is the normal state outside the image, not a failure.
func TestDiscoverCopilotModels_AbsentHelperWithHTTPSuccessIsLive(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "ghu_test")
	// Wrapped so errors.Is matches the sentinel exactly as execCopilotSDKHelper does.
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return nil, errCopilotSDKHelperAbsentWrapped()
	})
	healthyCopilotAPI(t, "gpt-4o")
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverCopilotModels()
	if !r.authoritative() || r.failed() {
		t.Fatalf("absent helper + HTTP success must be a clean live result, got %+v", r)
	}
}

func errCopilotSDKHelperAbsentWrapped() error {
	return errors.Join(errCopilotSDKHelperAbsent, errors.New("stat: no such file"))
}

// Helper installed and failing, no token: static fallback, WITH the error.
func TestDiscoverCopilotModels_SDKFailureNoTokenCarriesError(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) { return nil, arm64Failure })
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("copilot")
	if !r.fallback || r.degraded || !r.failed() {
		t.Fatalf("want static fallback carrying the failure, got %+v", r)
	}
	if !strings.Contains(r.discoveryErr, "copilot-linux-arm64") {
		t.Errorf("discoveryErr = %q, want the helper's diagnostic", r.discoveryErr)
	}
}

// The wire: /api/config/backends carries degraded and discovery{ok:false,
// error} so the dropdown can render an explicit error state.
func TestHandleBackends_CarriesDiscoveryFailure(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "ghu_test")
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) { return nil, arm64Failure })
	healthyCopilotAPI(t, "gpt-4o-mini-2024-07-18")
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}

	rec := httptest.NewRecorder()
	s.handleBackends(rec, httptest.NewRequest(http.MethodGet, "/api/config/backends", nil))
	var backends []struct {
		ID        string   `json:"id"`
		Models    []string `json:"models"`
		Fallback  bool     `json:"fallback"`
		Degraded  bool     `json:"degraded"`
		Discovery *struct {
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
			Fallback bool   `json:"fallback"`
			Degraded bool   `json:"degraded"`
		} `json:"discovery"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &backends); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	found := false
	for _, b := range backends {
		if b.ID != "copilot" {
			if b.Discovery != nil && b.Discovery.Error != "" && strings.Contains(b.Discovery.Error, "copilot") {
				t.Errorf("%s: copilot's failure leaked onto another backend", b.ID)
			}
			continue
		}
		found = true
		if b.Fallback || !b.Degraded {
			t.Errorf("copilot: fallback=%v degraded=%v, want a degraded live list", b.Fallback, b.Degraded)
		}
		if b.Discovery == nil || b.Discovery.OK || !b.Discovery.Degraded || !strings.Contains(b.Discovery.Error, "platform package") {
			t.Errorf("copilot: discovery = %+v, want ok=false with the helper's error", b.Discovery)
		}
		if len(b.Models) == 0 {
			t.Error("copilot: the list must still be served")
		}
	}
	if !found {
		t.Fatal("copilot missing from /api/config/backends")
	}
}

// The audit log: one entry when discovery breaks, one when the error text
// changes, one on recovery — never one per 30 s probe.
func TestQueryCLIModels_AuditsDiscoveryTransitions(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	var helperErr error = arm64Failure
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		if helperErr != nil {
			return nil, helperErr
		}
		return []byte(`{"models":[{"id":"gpt-5.4","policyState":"enabled"}]}`), nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger(), audit: &AuditLog{lastAction: map[string]time.Time{}}}
	since := time.Now().Add(-time.Minute)
	entries := func(action string) []AuditEntry {
		return s.audit.RecentWithPrefixSince(since, action)
	}
	requery := func() cliModelResult {
		s.cliModels.entries = map[string]cliModelCacheEntry{} // expire the TTL
		return s.queryCLIModels("copilot")
	}

	requery()
	requery()
	requery()
	failed := entries(auditActionModelDiscoveryFailed)
	if len(failed) != 1 {
		t.Fatalf("three identical failing probes must audit once, got %d: %+v", len(failed), failed)
	}
	for _, want := range []string{"backend=copilot", "platform=", "fallback=true", "degraded=false", "served=static-fallback", "copilot-linux-arm64"} {
		if !strings.Contains(failed[0].Detail, want) {
			t.Errorf("audit detail missing %q: %s", want, failed[0].Detail)
		}
	}
	if failed[0].User != "system" {
		t.Errorf("audit user = %q, want system", failed[0].User)
	}

	// The helper now resolves the SDK but the account is not signed in —
	// the post-#7365 diagnostic the issue calls out. A CHANGED error is a
	// new entry.
	helperErr = errors.New("sdk helper: exit status 1 (stderr: Not authenticated)")
	requery()
	if failed = entries(auditActionModelDiscoveryFailed); len(failed) != 2 || !strings.Contains(failed[1].Detail, "Not authenticated") {
		t.Fatalf("a changed failure must audit again with the new text, got %+v", failed)
	}

	// Recovery: one entry, naming the state it recovered from.
	helperErr = nil
	if r := requery(); !r.authoritative() {
		t.Fatalf("SDK success must be authoritative: %+v", r)
	}
	requery()
	recovered := entries(auditActionModelDiscoveryRecovered)
	if len(recovered) != 1 || !strings.Contains(recovered[0].Detail, "Not authenticated") {
		t.Fatalf("recovery must audit exactly once with the previous failure, got %+v", recovered)
	}
	if len(entries(auditActionModelDiscoveryFailed)) != 2 {
		t.Error("recovery must not add a failure entry")
	}
}

// A healthy hive audits nothing: not-configured fallbacks (no helper, no
// token) are not failures, and a clean first probe is not a recovery.
func TestQueryCLIModels_HealthyAndUnconfiguredAuditNothing(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return nil, errCopilotSDKHelperAbsentWrapped()
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger(), audit: &AuditLog{lastAction: map[string]time.Time{}}}
	r := s.queryCLIModels("copilot")
	if !r.fallback || r.failed() {
		t.Fatalf("no helper + no token is an unconfigured fallback, not a failure: %+v", r)
	}
	if got := s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), "model_discovery"); len(got) != 0 {
		t.Errorf("unconfigured fallback must not be audited: %+v", got)
	}
	// bob: authoritative single sentinel, never audited.
	s.queryCLIModels(bobBackendID)
	if got := s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), "model_discovery"); len(got) != 0 {
		t.Errorf("healthy probe must not be audited: %+v", got)
	}
}

func TestCLIModelErrText(t *testing.T) {
	if cliModelErrText(nil) != "" {
		t.Error("nil error must render empty")
	}
	got := cliModelErrText(errors.New("  multi\n  line\t error  "))
	if got != "multi line error" {
		t.Errorf("whitespace not collapsed: %q", got)
	}
	long := cliModelErrText(errors.New(strings.Repeat("x", 2*cliModelErrLimit)))
	if len([]rune(long)) != cliModelErrLimit+1 || !strings.HasSuffix(long, "…") {
		t.Errorf("long error not capped: %d runes", len([]rune(long)))
	}
}

// The dashboard side of the contract: the discovery error reaches the
// dropdown label/tooltip, and auto-heal is gated on degraded as well as
// fallback.
func TestDiscoveryFailureWiredInDashboard(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"⚠ model discovery failed — showing fallback list",
		"⚠ model discovery degraded — not the CLI catalog",
		"reconcileModelsAfterDiscovery(b.id, models, !!b.fallback || !!b.degraded)",
		"b.discovery.error",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q (#7384)", want)
		}
	}
}
