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

	// gemini has no effort control (claude gained one in #8377, so the
	// fixture's claude scanner is switched off it first).
	rec := doPut(s, "/api/config/agent/scanner/models",
		map[string]interface{}{"backend": "gemini", "reasoning_effort": "high"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("gemini effort: status = %d, want 400", rec.Code)
	}
	if got := deps.Config.Agents["scanner"].Backend; got != "claude" {
		t.Fatalf("rejected request must persist nothing; backend = %q", got)
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
// effort control (the fixture's claude scanner is moved to gemini first;
// claude itself has an effort control since #8377).
func TestHandleEffortSet_NoEffortBackend(t *testing.T) {
	s, _ := apiServer(t)
	if rec := doPut(s, "/api/config/agent/scanner/models",
		map[string]interface{}{"backend": "gemini"}); rec.Code != http.StatusOK {
		t.Fatalf("backend switch: status = %d, want 200", rec.Code)
	}
	if rec := doPost(s, "/api/effort/scanner/high", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestHandleEffortSet_ClaudeRoundTrip pins the per-agent claude effort
// (#8377) through the same durable path every other per-agent setting takes:
// an owner POST stores a claude-valid value on the fixture's claude scanner
// and it reads back from config; each of the five levels is accepted;
// "default" clears it; a value outside claude's set (codex's "minimal") is
// rejected without touching the stored value.
func TestHandleEffortSet_ClaudeRoundTrip(t *testing.T) {
	s, deps := apiServer(t)
	if got := deps.Config.Agents["scanner"].Backend; got != "claude" {
		t.Fatalf("fixture scanner backend = %q, want claude", got)
	}

	for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
		rec := doOwnerPost(s, "/api/effort/scanner/"+level, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("set %s: status = %d, want 200 (%s)", level, rec.Code, rec.Body.String())
		}
		if got := deps.Config.Agents["scanner"].ReasoningEffort; got != level {
			t.Fatalf("after set %s: reasoning_effort = %q", level, got)
		}
	}

	if rec := doOwnerPost(s, "/api/effort/scanner/minimal", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("codex-only value on claude: status = %d, want 400", rec.Code)
	}
	if got := deps.Config.Agents["scanner"].ReasoningEffort; got != "max" {
		t.Fatalf("rejected value must leave the stored effort alone; got %q", got)
	}

	rec := doOwnerPost(s, "/api/effort/scanner/default", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: status = %d, want 200", rec.Code)
	}
	if got := deps.Config.Agents["scanner"].ReasoningEffort; got != "" {
		t.Fatalf("'default' must clear the claude effort; got %q", got)
	}

	// The models endpoint takes the same field for claude.
	rec = doPut(s, "/api/config/agent/scanner/models",
		map[string]interface{}{"reasoning_effort": "high"})
	if rec.Code != http.StatusOK {
		t.Fatalf("models endpoint set: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Agents["scanner"].ReasoningEffort; got != "high" {
		t.Fatalf("models endpoint: reasoning_effort = %q, want high", got)
	}
}

func TestHandleEffortSet_NotFound(t *testing.T) {
	s, _ := apiServer(t)
	if rec := doPost(s, "/api/effort/nonexistent/high", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
