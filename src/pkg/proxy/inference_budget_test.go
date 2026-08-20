package proxy

import (
	"log/slog"
	"net/http"
	"testing"
	"time"
)

// ── Provider spend-rebuff detection (#4294) ─────────────────────────────────
//
// The load-bearing property is the NEGATIVE one: an ordinary rate limit must
// not latch. Over-matching here would suspend every agent on a busy gateway,
// which is a worse failure than the one being fixed.

func TestBudgetRebuffMatchesRealProviderBodies(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{
			// The exact shape from the field report in #4294.
			name:   "litellm daily dollar cap",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"message":"Budget has been exceeded! Current cost: 100.02, Max budget: 100.0","type":"budget_exceeded"}}`,
		},
		{
			name:   "litellm BudgetExceededError as 400",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"litellm.BudgetExceededError: Budget has been exceeded"}}`,
		},
		{
			name:   "openai insufficient quota",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","code":"insufficient_quota"}}`,
		},
		{
			name:   "anthropic credit balance",
			status: http.StatusBadRequest,
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Claude API."}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !isInferenceBudgetRebuff(tc.status, []byte(tc.body)) {
				t.Errorf("spend rebuff not detected for %s\nstatus=%d body=%s", tc.name, tc.status, tc.body)
			}
		})
	}
}

// TestBudgetRebuffIgnoresTransientThrottles is the guard that matters most. A
// 429 that means "slow down" must keep flowing to the existing retry path; if
// it latched here, a busy gateway would silently suspend the whole hive.
func TestBudgetRebuffIgnoresTransientThrottles(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "plain rate limit",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"message":"Rate limit reached for requests","type":"rate_limit_error"}}`,
		},
		{
			name:   "litellm tpm throttle",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"message":"Max parallel request limit reached. Please try again."}}`,
		},
		{
			name:   "overloaded",
			status: http.StatusTooManyRequests,
			body:   `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		},
		{
			name:   "output token cap 400 (the case manager.go already handles)",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"max_tokens is too large: 128000. This model supports at most 16384 completion tokens"}}`,
		},
		{
			name:   "auth failure is the OTHER signal, not this one",
			status: http.StatusUnauthorized,
			body:   `{"error":{"message":"Authentication Error, Invalid proxy server token passed"}}`,
		},
		{
			name:   "entitlement 403 is the OTHER signal, not this one",
			status: http.StatusForbidden,
			body:   `{"error":{"message":"team not allowed to access model"}}`,
		},
		{
			name:   "empty body never matches",
			status: http.StatusTooManyRequests,
			body:   ``,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if isInferenceBudgetRebuff(tc.status, []byte(tc.body)) {
				t.Errorf("FALSE POSITIVE: %s would suspend every agent\nstatus=%d body=%s", tc.name, tc.status, tc.body)
			}
		})
	}
}

// TestBudgetRebuffRequiresBothStatusAndBody pins that neither half alone is
// enough — a budget-sounding body on an unrelated status, or a plausible status
// with an unrelated body, must not latch.
func TestBudgetRebuffRequiresBothStatusAndBody(t *testing.T) {
	budgetBody := []byte(`{"error":{"message":"Budget has been exceeded! Max budget: 100"}}`)
	if isInferenceBudgetRebuff(http.StatusInternalServerError, budgetBody) {
		t.Error("a 500 must not latch a spend rebuff even with a budget-shaped body")
	}
	if isInferenceBudgetRebuff(http.StatusTooManyRequests, []byte(`{"error":"try later"}`)) {
		t.Error("a plausible status with no spend wording must not latch")
	}
}

func TestBudgetStateLatchesOnFirstRebuffAndSelfHeals(t *testing.T) {
	var st inferenceBudgetState
	if cause, _, _ := st.snapshot(); cause != "" {
		t.Fatalf("zero value must not report a rebuff, got %q", cause)
	}

	t0 := time.Now()
	st.recordRebuff("litellm refused on a spending limit (429)", t0)

	cause, since, rebuffs := st.snapshot()
	if cause == "" {
		t.Fatal("the FIRST rebuff must latch — waiting for more would burn more runs to learn what is already known")
	}
	if !since.Equal(t0) {
		t.Errorf("since = %v, want the first-rebuff time %v", since, t0)
	}
	if rebuffs != 1 {
		t.Errorf("rebuffs = %d, want 1", rebuffs)
	}

	// A later rebuff counts but must not move `since` — an operator needs to
	// know how long the hive has been clipped, not when it last tried.
	st.recordRebuff("litellm refused on a spending limit (429)", t0.Add(time.Hour))
	_, since2, rebuffs2 := st.snapshot()
	if !since2.Equal(t0) {
		t.Errorf("since moved to %v; it must stay at the first latch %v", since2, t0)
	}
	if rebuffs2 != 2 {
		t.Errorf("rebuffs = %d, want 2", rebuffs2)
	}

	// The self-heal: the provider window resets, a call succeeds, the hive
	// resumes with no operator action.
	st.recordSuccess()
	if cause, _, n := st.snapshot(); cause != "" || n != 0 {
		t.Errorf("a successful call must clear the latch, got cause=%q rebuffs=%d", cause, n)
	}
}

// TestBudgetMessageIsLogSafeAndUseful pins that the operator-facing cause keeps
// the gateway's own numbers — which limit was hit is what turns "agents are
// paused" into an action — while never carrying key material.
func TestBudgetMessageIsLogSafeAndUseful(t *testing.T) {
	body := `{"error":{"message":"Budget has been exceeded! Current cost: 100.02, Max budget: 100.0"}}`
	msg := inferenceBudgetMessage("litellm", http.StatusTooManyRequests, body)

	for _, want := range []string{"litellm", "429", "Max budget: 100.0"} {
		if !contains(msg, want) {
			t.Errorf("cause %q does not carry %q", msg, want)
		}
	}
	if contains(msg, "sk-") {
		t.Errorf("cause must never carry key material: %q", msg)
	}

	// An unnamed backend still produces a sentence rather than a dangling one.
	if got := inferenceBudgetMessage("", 400, ""); got == "" || contains(got, "  ") {
		t.Errorf("empty backend/body produced a malformed cause: %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	}()
}

// TestRecordInferenceErrorLatchesBudgetOnProductionPath drives the real
// recordInferenceError / recordInferenceSuccess pair rather than the state
// helpers, so a future refactor that stops calling the detector from the
// translator's error path fails here instead of passing on helper tests alone.
func TestRecordInferenceErrorLatchesBudgetOnProductionPath(t *testing.T) {
	redirectCAPaths(t)
	p, err := NewGitHubProxy(slog.Default(), "myorg", []string{"repo"})
	if err != nil {
		t.Fatalf("NewGitHubProxy: %v", err)
	}
	route := &InferenceRoute{Backend: "litellm", Endpoint: "https://gw.example/v1", Model: "gpt-4o"}

	if cause, _, _ := p.InferenceBudgetExceeded(); cause != "" {
		t.Fatalf("fresh proxy reports a rebuff: %q", cause)
	}

	// An ordinary throttle must leave the signal clear.
	p.recordInferenceError(route, "quality", http.StatusTooManyRequests,
		[]byte(`{"error":{"message":"Rate limit reached for requests"}}`))
	if cause, _, _ := p.InferenceBudgetExceeded(); cause != "" {
		t.Fatalf("a transient throttle latched a spend rebuff: %q", cause)
	}

	// The field-report body must latch on the production path.
	p.recordInferenceError(route, "quality", http.StatusTooManyRequests,
		[]byte(`{"error":{"message":"Budget has been exceeded! Current cost: 100.02, Max budget: 100.0"}}`))
	cause, since, rebuffs := p.InferenceBudgetExceeded()
	if cause == "" {
		t.Fatal("recordInferenceError did not latch the spend rebuff — the translator's error path is not wired to the detector")
	}
	if since.IsZero() || rebuffs != 1 {
		t.Errorf("since=%v rebuffs=%d, want a stamped time and 1", since, rebuffs)
	}

	// A success clears it, which is how the hive resumes after the provider's
	// window resets.
	p.recordInferenceSuccess()
	if cause, _, _ := p.InferenceBudgetExceeded(); cause != "" {
		t.Errorf("a successful inference call did not clear the latch: %q", cause)
	}
}

// TestBudgetSignalIsIndependentOfAuthSignal pins that the two latches do not
// contaminate each other: an operator seeing "spending limit" must not also be
// told their key is invalid, and vice versa.
func TestBudgetSignalIsIndependentOfAuthSignal(t *testing.T) {
	redirectCAPaths(t)
	p, err := NewGitHubProxy(slog.Default(), "myorg", []string{"repo"})
	if err != nil {
		t.Fatalf("NewGitHubProxy: %v", err)
	}
	route := &InferenceRoute{Backend: "litellm", Endpoint: "https://gw.example/v1"}

	p.recordInferenceError(route, "quality", http.StatusTooManyRequests,
		[]byte(`{"error":{"message":"Budget has been exceeded! Max budget: 100"}}`))

	if cause, _, _ := p.InferenceBudgetExceeded(); cause == "" {
		t.Fatal("spend rebuff did not latch")
	}
	if authErr, _ := p.InferenceAuthError(); authErr != "" {
		t.Errorf("a spending-limit refusal must not report an auth failure, got %q", authErr)
	}
}
