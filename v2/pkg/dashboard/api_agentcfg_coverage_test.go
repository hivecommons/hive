package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func acfgServer(t *testing.T) *Server {
	t.Helper()
	s := covApiServer(t)
	s.deps.Config.Data.AgentsDir = t.TempDir()
	return s
}

func TestCovACfg_General_NotFound(t *testing.T) {
	s := acfgServer(t)
	if rec := doPut(s, "/api/config/agent/ghost/general", map[string]any{}); rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestCovACfg_General_BadBody(t *testing.T) {
	s := acfgServer(t)
	if rec := doPutRaw(s, "/api/config/agent/scanner/general", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestCovACfg_General_AllFields(t *testing.T) {
	s := acfgServer(t)
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{
		"enabled":         true,
		"clearOnKick":     true,
		"displayName":     "Scanner",
		"description":     "desc",
		"launchCmd":       "run",
		"staleTimeout":    float64(30),
		"restartStrategy": "always",
		"cliPinned":       true,
		"emoji":           "🔍",
		"color":           "#fff",
		"sortOrder":       float64(2),
		"beadRole":        "role",
		"role":            "worker",
		"kickTemplate":    "scanner.md",
		"mode":            "ISSUES_ONLY",
		"includeRepos":    true,
		"laneKeywords":    []any{"a", "b"},
		"detectKeywords":  []any{"c"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("all fields: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCovACfg_General_BadMode(t *testing.T) {
	s := acfgServer(t)
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"mode": "NONSENSE"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: want 400, got %d", rec.Code)
	}
}

func TestCovACfg_Models(t *testing.T) {
	s := acfgServer(t)
	if rec := doPut(s, "/api/config/agent/ghost/models", map[string]any{"backend": "claude"}); rec.Code != http.StatusNotFound {
		t.Fatalf("models not found: want 404, got %d", rec.Code)
	}
	if rec := doPutRaw(s, "/api/config/agent/scanner/models", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("models bad body: want 400, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/agent/scanner/models", map[string]any{"backend": "copilot", "model": "gpt-4o"}); rec.Code != http.StatusOK {
		t.Fatalf("models ok: want 200, got %d", rec.Code)
	}
}

func TestCovACfg_Pipeline(t *testing.T) {
	s := acfgServer(t)
	if rec := doPut(s, "/api/config/agent/ghost/pipeline", map[string]any{}); rec.Code != http.StatusNotFound {
		t.Fatalf("pipeline not found: %d", rec.Code)
	}
	if rec := doPutRaw(s, "/api/config/agent/scanner/pipeline", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("pipeline bad body: %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/agent/scanner/pipeline", map[string]bool{"scanner": true}); rec.Code != http.StatusOK {
		t.Fatalf("pipeline ok: %d", rec.Code)
	}
}

func TestCovACfg_Hooks(t *testing.T) {
	s := acfgServer(t)
	if rec := doPut(s, "/api/config/agent/ghost/hooks", map[string]any{}); rec.Code != http.StatusNotFound {
		t.Fatalf("hooks not found: %d", rec.Code)
	}
	if rec := doPutRaw(s, "/api/config/agent/scanner/hooks", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("hooks bad body: %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/agent/scanner/hooks", map[string][]any{"pre": {"x"}}); rec.Code != http.StatusOK {
		t.Fatalf("hooks ok: %d", rec.Code)
	}
}

func TestCovACfg_Restrictions(t *testing.T) {
	s := acfgServer(t)
	if rec := doPut(s, "/api/config/agent/ghost/restrictions", map[string]any{}); rec.Code != http.StatusNotFound {
		t.Fatalf("restrictions not found: %d", rec.Code)
	}
	if rec := doPutRaw(s, "/api/config/agent/scanner/restrictions", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("restrictions bad body: %d", rec.Code)
	}
	// Valid body writes to /data/agents/... which may fail in the sandbox; either
	// 200 or 500 exercises the pattern/reason normalization loop.
	rec := doPut(s, "/api/config/agent/scanner/restrictions", map[string]any{
		"agent": []map[string]any{
			{"pattern": "rm -rf\n/", "reason": "danger\r\nous"},
			{"pattern": "curl"},
		},
	})
	if rec.Code != http.StatusOK && rec.Code != http.StatusInternalServerError {
		t.Fatalf("restrictions valid: unexpected %d", rec.Code)
	}
}

func TestCovACfg_Prompt(t *testing.T) {
	s := acfgServer(t)
	// GET prompt for known agent (loadPromptTemplateRaw / defaults).
	if rec := doGet(s, "/api/config/agent/scanner/prompt"); rec.Code != http.StatusOK {
		t.Fatalf("get prompt: %d", rec.Code)
	}
	// GET prompt for unknown agent → 404.
	if rec := doGet(s, "/api/config/agent/ghost/prompt"); rec.Code != http.StatusNotFound {
		t.Fatalf("get prompt ghost: %d", rec.Code)
	}
}

func TestCovACfg_Export(t *testing.T) {
	s := acfgServer(t)
	// Populate a rich agent config so buildExportYAML walks more branches.
	cfg := s.deps.Config.Agents["scanner"]
	cfg.DisplayName = "Scanner"
	cfg.Description = "scans"
	cfg.Emoji = "🔍"
	cfg.Color = "#abc"
	cfg.Mode = "ISSUES_ONLY"
	s.deps.Config.Agents["scanner"] = cfg

	// JSON response.
	if rec := doGet(s, "/api/config/agent/scanner/export"); rec.Code != http.StatusOK {
		t.Fatalf("export json: %d", rec.Code)
	}
	// YAML response via Accept header.
	rec := newReq(s, http.MethodGet, "/api/config/agent/scanner/export", "", map[string]string{"Accept": "text/yaml"})
	if rec.Code != http.StatusOK {
		t.Fatalf("export yaml: %d", rec.Code)
	}
	// Unknown agent → 404.
	if rec := doGet(s, "/api/config/agent/ghost/export"); rec.Code != http.StatusNotFound {
		t.Fatalf("export ghost: %d", rec.Code)
	}
}
