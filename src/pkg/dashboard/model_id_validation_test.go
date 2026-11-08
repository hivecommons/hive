package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

// hive-wild-mole, build 70a9203: PUT /api/config/agent/guide/models accepted
// the operator model "claude-sonnet-5.5" (a dot where the Anthropic id uses a
// dash). Claude Code then refused every request with "There's an issue with
// the selected model (claude-sonnet-5.5)" and the agent restart-looped. The
// dotted id is certain to fail on the claude backend, so it is a 400 at save.
func TestHandleAgentConfigModels_RejectsDottedClaudeModelID(t *testing.T) {
	s, deps := apiServer(t)
	before := deps.Config.Agents["scanner"].Model
	beforeProc, _ := deps.AgentMgr.GetStatus("scanner")

	rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"backend": "claude", "model": "claude-sonnet-5.5"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a dotted claude model id: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "claude-sonnet-5-5") {
		t.Errorf("error must suggest the dashed id: %s", body)
	}
	if got := deps.Config.Agents["scanner"].Model; got != before {
		t.Errorf("config model = %q, want it unchanged (%q) after a refused save", got, before)
	}
	afterProc, _ := deps.AgentMgr.GetStatus("scanner")
	if beforeProc != nil && afterProc != nil && afterProc.RestartCount != beforeProc.RestartCount {
		t.Errorf("a refused save restarted the agent (%d → %d)", beforeProc.RestartCount, afterProc.RestartCount)
	}
}

// A well-formed id hive's list does not know may be a model newer than the
// list, so it is saved — with a warning in the response, not a refusal.
func TestHandleAgentConfigModels_WarnsOnUnknownWellFormedClaudeModel(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"backend": "claude", "model": "claude-sonnet-9-9"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a well-formed unknown id: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	warning, _ := resp["warning"].(string)
	if !strings.Contains(warning, "claude-sonnet-9-9") {
		t.Errorf("response must warn that the id is not in the known list: %v", resp)
	}
	if got := deps.Config.Agents["scanner"].Model; got != "claude-sonnet-9-9" {
		t.Errorf("config model = %q, want the unknown-but-well-formed id saved", got)
	}

	// A listed id carries no warning.
	rec = doPut(s, "/api/config/agent/scanner/models", map[string]any{"backend": "claude", "model": "claude-sonnet-5-5"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if w, ok := decodeJSON(t, rec)["warning"]; ok {
		t.Errorf("a listed id must not warn, got %v", w)
	}
}

// The dotted spelling is the Copilot CLI's own nomenclature for 5.5
// (copilotPinnedCLIModels), so the shape rule must stay claude-only.
func TestHandleAgentConfigModels_DottedIDStillValidOnCopilot(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"backend": "copilot", "model": "claude-sonnet-5.5"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for copilot's dotted id: %s", rec.Code, rec.Body.String())
	}
}

// The card dropdown / agent.model admin op path (POST /api/model) refuses the
// same id even when the claude list cannot be enumerated (cold cache), where
// validateModelForAgent otherwise lets everything through.
func TestHandleModelSet_RejectsDottedClaudeModelID(t *testing.T) {
	s, deps := apiServer(t)
	if err := deps.AgentMgr.SetBackendOverride("scanner", "claude"); err != nil {
		t.Fatal(err)
	}
	rec := doPost(s, "/api/model/scanner/claude-sonnet-5.5", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestMalformedModelIDError(t *testing.T) {
	cases := []struct {
		backend, model string
		bad            bool
	}{
		{"claude", "claude-sonnet-5.5", true},
		{"claude", "claude-opus-4.8", true},
		{"claude", "claude-sonnet-5-5", false},
		{"claude", "claude-fable-5-1", false},
		{"claude", "claude-haiku-5-5", false},
		{"claude", "claude-opus-4-1-20250805", false},
		{"claude", "sonnet", false},
		{"claude", "claude-sonnet-4-5@20250929", false},                   // Vertex
		{"claude", "us.anthropic.claude-sonnet-4-5-20250929-v1:0", false}, // Bedrock
		{"copilot", "claude-sonnet-5.5", false},
		{"litellm", "claude-sonnet-5.5", false},
		{"openrouter", "anthropic/claude-opus-4.8", false},
	}
	for _, c := range cases {
		err := malformedModelIDError(c.backend, c.model)
		if (err != nil) != c.bad {
			t.Errorf("malformedModelIDError(%q, %q) = %v, want bad=%v", c.backend, c.model, err, c.bad)
		}
	}
}
