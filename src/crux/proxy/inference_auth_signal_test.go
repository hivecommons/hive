package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newTestAuthState returns a fresh inference-auth tracker.
func newTestAuthState() *inferenceAuthState { return &inferenceAuthState{} }

// discardLogger is a quiet logger for proxy tests that log Warn/Info.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestInferenceAuthState_SingleFailureDoesNotLatch(t *testing.T) {
	s := newTestAuthState()
	// One transient 401 must never raise the signal — a gateway restart mid
	// key-rotation is not a stale-key outage.
	s.recordFailure("litellm inference backend auth failed (401): invalid proxy token", time.Now())
	if errMsg, _ := s.snapshot(); errMsg != "" {
		t.Fatalf("a single 401 must not latch the signal, got %q", errMsg)
	}
}

func TestInferenceAuthState_LatchesAfterThreshold(t *testing.T) {
	s := newTestAuthState()
	now := time.Now()
	// Fail one short of the threshold: still not latched.
	for i := 0; i < inferenceAuthFailureThreshold-1; i++ {
		s.recordFailure("boom", now)
	}
	if errMsg, _ := s.snapshot(); errMsg != "" {
		t.Fatalf("below threshold (%d) the signal must stay clear, got %q after %d",
			inferenceAuthFailureThreshold, errMsg, inferenceAuthFailureThreshold-1)
	}
	// The threshold-th consecutive failure latches it.
	s.recordFailure("litellm inference backend auth failed (401): invalid proxy token", now)
	errMsg, since := s.snapshot()
	if errMsg == "" {
		t.Fatalf("the signal must latch at %d consecutive failures", inferenceAuthFailureThreshold)
	}
	if since.IsZero() {
		t.Fatalf("a latched signal must stamp the time it first failed")
	}
}

func TestInferenceAuthState_SuccessClearsAndResetsConsecutive(t *testing.T) {
	s := newTestAuthState()
	now := time.Now()
	for i := 0; i < inferenceAuthFailureThreshold; i++ {
		s.recordFailure("boom", now)
	}
	if errMsg, _ := s.snapshot(); errMsg == "" {
		t.Fatalf("precondition: signal should be latched")
	}
	// Self-heal: the very next success clears the latch.
	s.recordSuccess()
	if errMsg, since := s.snapshot(); errMsg != "" || !since.IsZero() {
		t.Fatalf("a success must clear the latched signal, got err=%q since=%v", errMsg, since)
	}
	// And it also reset the consecutive counter: it must take a FULL fresh run
	// of failures to latch again, not a single one.
	s.recordFailure("boom", now)
	if errMsg, _ := s.snapshot(); errMsg != "" {
		t.Fatalf("after a success, one failure must not re-latch immediately, got %q", errMsg)
	}
}

func TestInferenceAuthState_SinceIsStableWhileFailing(t *testing.T) {
	s := newTestAuthState()
	t0 := time.Now()
	for i := 0; i < inferenceAuthFailureThreshold; i++ {
		s.recordFailure("boom", t0)
	}
	_, first := s.snapshot()
	// Further failures at a later time must not move the "since" forward — the
	// outage began when it first latched, and the pill must not churn.
	s.recordFailure("boom", t0.Add(10*time.Minute))
	_, later := s.snapshot()
	if !first.Equal(later) {
		t.Fatalf("since must stay pinned to the first latch: %v then %v", first, later)
	}
}

func TestIsInferenceAuthStatus(t *testing.T) {
	if !isInferenceAuthStatus(http.StatusUnauthorized) {
		t.Fatalf("401 must be treated as an auth failure")
	}
	// 403 is an ENTITLEMENT signal (team-not-allowed), NOT an auth failure —
	// folding it in would false-alarm a correctly-keyed hive on a bad model.
	if isInferenceAuthStatus(http.StatusForbidden) {
		t.Fatalf("403 must NOT be treated as an auth failure (it is an entitlement signal)")
	}
	if isInferenceAuthStatus(http.StatusOK) {
		t.Fatalf("200 must not be an auth failure")
	}
}

func TestInferenceAuthErrorMessage_NoKeyMaterialAndNamesBackend(t *testing.T) {
	// The message must never carry key material; it names the backend and status
	// and describes the rejection. Feed a body that (defensively) contained a
	// token-looking string and assert the built message does not surface a key.
	msg := inferenceAuthErrorMessage("litellm", http.StatusUnauthorized,
		`{"error":{"message":"Authentication Error, Invalid proxy server token passed..."}}`)
	if !strings.Contains(msg, "litellm") {
		t.Fatalf("message should name the backend, got %q", msg)
	}
	if !strings.Contains(msg, "401") {
		t.Fatalf("message should carry the status, got %q", msg)
	}
	if strings.Contains(strings.ToLower(msg), "sk-") {
		t.Fatalf("message must not carry key material, got %q", msg)
	}
}

// TestRecordInferenceError_LatchesAndClearsThroughProxy exercises the full
// spoke path: repeated 401s through recordInferenceError latch the signal that
// InferenceAuthError() reports, and recordInferenceSuccess clears it.
func TestRecordInferenceError_LatchesAndClearsThroughProxy(t *testing.T) {
	p := &GitHubProxy{inferenceAuth: &inferenceAuthState{}, logger: discardLogger()}
	route := &InferenceRoute{Backend: "litellm", Endpoint: "http://gw", Model: "gpt-4o", APIKey: "sk-secret-should-never-surface"}
	body := []byte(`{"error":{"message":"Authentication Error, Invalid proxy server token passed..."}}`)

	// Below threshold: no signal.
	for i := 0; i < inferenceAuthFailureThreshold-1; i++ {
		p.recordInferenceError(route, "scanner", http.StatusUnauthorized, body)
	}
	if errMsg, _ := p.InferenceAuthError(); errMsg != "" {
		t.Fatalf("below threshold the proxy must report no inference-auth error, got %q", errMsg)
	}

	// Threshold reached: signal latches, and must not leak the API key.
	p.recordInferenceError(route, "scanner", http.StatusUnauthorized, body)
	errMsg, since := p.InferenceAuthError()
	if errMsg == "" {
		t.Fatalf("at threshold the proxy must report an inference-auth error")
	}
	if since.IsZero() {
		t.Fatalf("a latched signal must carry a since timestamp")
	}
	if strings.Contains(errMsg, "sk-secret") {
		t.Fatalf("the reported error must never carry the API key, got %q", errMsg)
	}

	// A success clears it (self-heal).
	p.recordInferenceSuccess()
	if errMsg, _ := p.InferenceAuthError(); errMsg != "" {
		t.Fatalf("a successful inference call must clear the signal, got %q", errMsg)
	}
}

// TestRecordInferenceError_403IsNotAuthFailure guards the entitlement/auth
// split at the proxy boundary: a 403 must not latch the auth signal.
func TestRecordInferenceError_403IsNotAuthFailure(t *testing.T) {
	p := &GitHubProxy{inferenceAuth: &inferenceAuthState{}, entitlements: newEntitlementStore(), logger: discardLogger()}
	route := &InferenceRoute{Backend: "litellm", Endpoint: "http://gw", Model: "gpt-4o"}
	for i := 0; i < inferenceAuthFailureThreshold+2; i++ {
		p.recordInferenceError(route, "scanner", http.StatusForbidden, []byte(`{"error":"team not allowed"}`))
	}
	if errMsg, _ := p.InferenceAuthError(); errMsg != "" {
		t.Fatalf("repeated 403s must NOT latch the inference-auth signal, got %q", errMsg)
	}
}
