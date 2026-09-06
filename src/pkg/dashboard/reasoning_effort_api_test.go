package dashboard

import (
	"net/http"
	"testing"
)

// TestHandleAgentConfigModels_ReasoningEffort pins the field semantics of the
// models endpoint's reasoning_effort: set alongside a backend change, left
// unchanged when the field is absent, and cleared by an explicit "".
func TestHandleAgentConfigModels_ReasoningEffort(t *testing.T) {
	s, deps := apiServer(t)

	rec := doPut(s, "/api/config/agent/scanner/models",
		map[string]interface{}{"backend": "codex", "model": "gpt-6-astra", "reasoning_effort": "xhigh"})
	if rec.Code != http.StatusOK {
		t.Fatalf("set: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Agents["scanner"].ReasoningEffort; got != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want %q", got, "xhigh")
	}

	// Absent field leaves the stored effort unchanged — "" is a meaningful
	// value (the backend's own default), so only an explicit "" clears.
	rec = doPut(s, "/api/config/agent/scanner/models",
		map[string]interface{}{"model": "gpt-5.6-sol"})
	if rec.Code != http.StatusOK {
		t.Fatalf("absent: status = %d, want 200", rec.Code)
	}
	if got := deps.Config.Agents["scanner"].ReasoningEffort; got != "xhigh" {
		t.Fatalf("absent field must leave effort unchanged; got %q", got)
	}

	rec = doPut(s, "/api/config/agent/scanner/models",
		map[string]interface{}{"reasoning_effort": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: status = %d, want 200", rec.Code)
	}
	if got := deps.Config.Agents["scanner"].ReasoningEffort; got != "" {
		t.Fatalf("explicit \"\" must clear the effort; got %q", got)
	}
}

// TestHandleAgentConfigModels_ReasoningEffortRejected pins set-time rejection:
// a backend with no effort control, and a value the backend does not accept,
// both 400 without persisting anything from the request.
func TestHandleAgentConfigModels_ReasoningEffortRejected(t *testing.T) {
	s, deps := apiServer(t)

	// scanner's fixture backend is claude, which has no effort control.
	rec := doPut(s, "/api/config/agent/scanner/models",
		map[string]interface{}{"reasoning_effort": "high"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("claude effort: status = %d, want 400", rec.Code)
	}

	// codex rejects values outside its vocabulary; the backend change in the
	// same rejected request must not be persisted either.
	rec = doPut(s, "/api/config/agent/scanner/models",
		map[string]interface{}{"backend": "codex", "reasoning_effort": "turbo"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad value: status = %d, want 400", rec.Code)
	}
	if got := deps.Config.Agents["scanner"].Backend; got != "claude" {
		t.Fatalf("rejected request must persist nothing; backend = %q", got)
	}
}

// TestHandleEffortSet covers the grid dropdown endpoint: set, clear via the
// "default" sentinel, and reject a value the backend does not accept.
func TestHandleEffortSet(t *testing.T) {
	s, deps := apiServer(t)
	if rec := doPut(s, "/api/config/agent/scanner/models",
		map[string]interface{}{"backend": "codex"}); rec.Code != http.StatusOK {
		t.Fatalf("backend switch: status = %d, want 200", rec.Code)
	}

	rec := doPost(s, "/api/effort/scanner/xhigh", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Agents["scanner"].ReasoningEffort; got != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want %q", got, "xhigh")
	}

	rec = doPost(s, "/api/effort/scanner/default", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: status = %d, want 200", rec.Code)
	}
	if got := deps.Config.Agents["scanner"].ReasoningEffort; got != "" {
		t.Fatalf("'default' must clear the effort; got %q", got)
	}

	if rec := doPost(s, "/api/effort/scanner/turbo", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad value: status = %d, want 400", rec.Code)
	}
}

// TestHandleEffortSet_NoEffortBackend pins the 400 for a backend with no
// effort control (scanner's fixture backend is claude).
func TestHandleEffortSet_NoEffortBackend(t *testing.T) {
	s, _ := apiServer(t)
	if rec := doPost(s, "/api/effort/scanner/high", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleEffortSet_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	if rec := doPost(s, "/api/effort/nonexistent/high", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
