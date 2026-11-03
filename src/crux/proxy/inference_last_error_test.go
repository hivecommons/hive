package proxy

import (
	"strings"
	"testing"
	"time"
)

// #11761: the proxy remembers the FULL error text it returned to each agent,
// so the dashboard can show it while Claude Code's retry line is still clipped
// to one pane row, and forgets it on that agent's next successful call.
func TestLastInferenceError_RecordsPerAgentAndClearsOnSuccess(t *testing.T) {
	p := &GitHubProxy{inferenceLastErrors: newInferenceLastErrorState()}

	if _, _, _, ok := p.LastInferenceError("scanner"); ok {
		t.Fatal("no error recorded yet, want ok=false")
	}

	body := []byte(`{"error":{"message":"Authentication Error, Invalid proxy server token passed. Unable to find token in cache or LiteLLM_VerificationTokenTable","type":"auth_error"}}`)
	full := inferenceBackendErrorMessage(401, body, "https://gw.example/v1/chat/completions", "uvicorn")
	before := time.Now()
	p.recordAgentInferenceError("scanner", full, 401)

	msg, status, at, ok := p.LastInferenceError("scanner")
	if !ok || msg != full || status != 401 {
		t.Fatalf("LastInferenceError = %q/%d/%v, want %q/401/true", msg, status, ok, full)
	}
	if at.Before(before) {
		t.Fatalf("at = %v, want >= %v", at, before)
	}
	if !strings.Contains(msg, "LiteLLM_VerificationTokenTable") || !strings.Contains(msg, "upstream: https://gw.example/v1/chat/completions") {
		t.Fatalf("recorded message lost the full detail: %q", msg)
	}

	// Per agent: another agent is unaffected.
	if _, _, _, ok := p.LastInferenceError("quality"); ok {
		t.Fatal("error recorded for scanner leaked to quality")
	}
	p.recordAgentInferenceError("quality", inferenceBackendUnreachableMessage(errTestDial, "https://gw.example/v1/chat/completions"), 0)

	p.clearAgentInferenceError("scanner")
	if _, _, _, ok := p.LastInferenceError("scanner"); ok {
		t.Fatal("successful call must clear scanner's last error")
	}
	if msg, status, _, ok := p.LastInferenceError("quality"); !ok || status != 0 || !strings.HasPrefix(msg, "inference backend unreachable: ") {
		t.Fatalf("quality entry = %q/%d/%v, want unreachable message kept", msg, status, ok)
	}
}

func TestLastInferenceError_IgnoresUnattributedAndNilSafe(t *testing.T) {
	p := &GitHubProxy{inferenceLastErrors: newInferenceLastErrorState()}
	p.recordAgentInferenceError("", "inference backend returned 401: x", 401)
	if _, _, _, ok := p.LastInferenceError(""); ok {
		t.Fatal("an unattributed request must not be recorded")
	}

	var nilProxy *GitHubProxy
	nilProxy.recordAgentInferenceError("scanner", "x", 401)
	nilProxy.clearAgentInferenceError("scanner")
	if _, _, _, ok := nilProxy.LastInferenceError("scanner"); ok {
		t.Fatal("nil proxy must report no error")
	}

	bare := &GitHubProxy{}
	bare.recordAgentInferenceError("scanner", "x", 401)
	if _, _, _, ok := bare.LastInferenceError("scanner"); ok {
		t.Fatal("proxy without a tracker must report no error")
	}
}

type testDialError struct{}

func (testDialError) Error() string { return "dial tcp 10.0.0.1:443: connect: connection refused" }

var errTestDial error = testDialError{}
