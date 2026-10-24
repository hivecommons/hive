package proxy

import (
	"log/slog"
	"net/http"
	"testing"
)

// These tests pin the inference gateway-health signal surface on GitHubProxy:
// GatewayHealth (previously 0% covered), recordInferenceSuccessFor, and the
// nil-receiver guards on InferenceBudgetExceeded / InferenceAuthError. The
// dashboard status builder (pkg/dashboard/status_builder.go) consumes
// GatewayHealth directly, so a regression here silently blanks the operator's
// failing-gateway panel rather than failing loudly.

// TestGatewayHealthNilGuards pins that GatewayHealth is safe to call on a nil
// receiver and on a proxy without a health store, returning nil both times —
// the documented "safe on nil" contract callers rely on.
func TestGatewayHealthNilGuards(t *testing.T) {
	var nilProxy *GitHubProxy
	if got := nilProxy.GatewayHealth(); got != nil {
		t.Errorf("nil receiver GatewayHealth() = %v, want nil", got)
	}

	noStore := &GitHubProxy{}
	if got := noStore.GatewayHealth(); got != nil {
		t.Errorf("GatewayHealth() with nil store = %v, want nil", got)
	}
}

// TestGatewayHealthReportsAndClearsFault drives the production wiring: an
// inference HTTP error recorded via recordInferenceError must surface in
// GatewayHealth's snapshot, and a subsequent success on the same route must
// clear it via recordInferenceSuccessFor — the self-heal path the dashboard
// depends on to stop flagging a recovered gateway.
func TestGatewayHealthReportsAndClearsFault(t *testing.T) {
	redirectCAPaths(t)
	p, err := NewGitHubProxy(slog.Default(), "myorg", []string{"repo"})
	if err != nil {
		t.Fatalf("NewGitHubProxy: %v", err)
	}

	if got := p.GatewayHealth(); len(got) != 0 {
		t.Fatalf("fresh proxy reports gateway faults: %v", got)
	}

	route := &InferenceRoute{Backend: "litellm", Endpoint: "https://gw.example/v1", Model: "gpt-4o"}
	p.recordInferenceError(route, "quality", http.StatusBadGateway,
		[]byte(`{"error":{"message":"upstream connect error"}}`))

	snap := p.GatewayHealth()
	if len(snap) != 1 {
		t.Fatalf("GatewayHealth() after error = %v, want exactly one fault", snap)
	}
	if snap[0].Name != "litellm" {
		t.Errorf("fault Name = %q, want %q", snap[0].Name, "litellm")
	}
	if snap[0].HTTPStatus != http.StatusBadGateway {
		t.Errorf("fault HTTPStatus = %d, want %d", snap[0].HTTPStatus, http.StatusBadGateway)
	}
	if snap[0].Endpoint == "" {
		t.Error("fault Endpoint is empty — endpoint attribution lost")
	}

	p.recordInferenceSuccessFor(route)
	if got := p.GatewayHealth(); len(got) != 0 {
		t.Errorf("GatewayHealth() after success = %v, want fault cleared", got)
	}
}

// TestRecordInferenceSuccessForNilRouteStillClearsLatches pins the documented
// nil-route tolerance: recordInferenceSuccessFor(nil) must not panic and must
// still clear the budget latch (it delegates to recordInferenceSuccess), while
// leaving unrelated gateway faults untouched — a nil route names no backend,
// so nothing can be attributed for clearing.
func TestRecordInferenceSuccessForNilRouteStillClearsLatches(t *testing.T) {
	redirectCAPaths(t)
	p, err := NewGitHubProxy(slog.Default(), "myorg", []string{"repo"})
	if err != nil {
		t.Fatalf("NewGitHubProxy: %v", err)
	}

	route := &InferenceRoute{Backend: "litellm", Endpoint: "https://gw.example/v1"}
	p.recordInferenceError(route, "quality", http.StatusTooManyRequests,
		[]byte(`{"error":{"message":"Budget has been exceeded! Current cost: 100.02, Max budget: 100.0"}}`))
	if cause, _, _, _ := p.InferenceBudgetExceeded(); cause == "" {
		t.Fatal("spend rebuff did not latch — test premise broken")
	}
	if len(p.GatewayHealth()) != 1 {
		t.Fatal("gateway fault did not latch — test premise broken")
	}

	p.recordInferenceSuccessFor(nil)

	if cause, _, _, _ := p.InferenceBudgetExceeded(); cause != "" {
		t.Errorf("nil-route success did not clear the budget latch: %q", cause)
	}
	if len(p.GatewayHealth()) != 1 {
		t.Error("nil-route success cleared a gateway fault it cannot attribute")
	}
}

// TestInferenceSignalNilReceiverGuards pins the "safe on a nil receiver"
// contract documented on InferenceBudgetExceeded and InferenceAuthError:
// status surfaces call these before the proxy may exist, and a nil receiver
// must read as "no signal", not panic.
func TestInferenceSignalNilReceiverGuards(t *testing.T) {
	var p *GitHubProxy

	cause, since, lastRebuff, rebuffs := p.InferenceBudgetExceeded()
	if cause != "" || !since.IsZero() || !lastRebuff.IsZero() || rebuffs != 0 {
		t.Errorf("nil receiver InferenceBudgetExceeded() = (%q, %v, %v, %d), want all zero",
			cause, since, lastRebuff, rebuffs)
	}

	authErr, authSince := p.InferenceAuthError()
	if authErr != "" || !authSince.IsZero() {
		t.Errorf("nil receiver InferenceAuthError() = (%q, %v), want zero values", authErr, authSince)
	}

	noTrackers := &GitHubProxy{}
	if cause, _, _, _ := noTrackers.InferenceBudgetExceeded(); cause != "" {
		t.Errorf("nil-tracker InferenceBudgetExceeded() cause = %q, want empty", cause)
	}
	if authErr, _ := noTrackers.InferenceAuthError(); authErr != "" {
		t.Errorf("nil-tracker InferenceAuthError() = %q, want empty", authErr)
	}
	// recordInferenceSuccessFor must be a no-op, not a panic, on a proxy with
	// no trackers wired.
	noTrackers.recordInferenceSuccessFor(&InferenceRoute{Backend: "litellm"})
}
