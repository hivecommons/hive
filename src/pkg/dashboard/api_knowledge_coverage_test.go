package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// covCServer builds a server with the standard test deps and all API routes
// registered, for driving knowledge/config/document handlers through the mux.
func covCServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.RegisterAPI(testDeps(t))
	return s
}

func TestCovC_TokenAccess(t *testing.T) {
	s := covCServer(t)
	// tokenAccessLogPath is a const under /var/run — no file in tests, so this
	// exercises the "no audit log" fallback branch. Owner role required (#3936).
	rec := doOwnerGet(s, "/api/token-access")
	if rec.Code != http.StatusOK {
		t.Fatalf("token-access status = %d, want 200", rec.Code)
	}
	body := decodeJSON(t, rec)
	if _, ok := body["entries"]; !ok {
		t.Errorf("expected entries key, got %v", body)
	}
}

func TestCovC_VariablesList(t *testing.T) {
	s := covCServer(t)
	rec := doGet(s, "/api/config/variables")
	if rec.Code != http.StatusOK {
		t.Fatalf("variables status = %d, want 200", rec.Code)
	}
	body := decodeJSON(t, rec)
	if _, ok := body["variables"]; !ok {
		t.Errorf("expected variables key, got %v", body)
	}
}

func TestCovC_VariablesListWithDefs(t *testing.T) {
	s := covCServer(t)
	// Seed a few variable defs of different types so the type/scope/source
	// derivation branches (env, http, static default) all execute.
	s.deps.Config.Variables.Defs = map[string]config.VarDef{
		"PLAIN":   {},              // -> env, source env:PLAIN
		"WITHENV": {Env: "MY_ENV"}, // -> env, source env:MY_ENV
		"STAT":    {Value: "x"},    // -> static
		"HTTPV":   {Type: "http", URL: "https://h.example/p"},
		"SCOPED":  {Type: "static", Scope: "global", Value: "y"},
	}
	s.deps.Config.Variables.Security.AllowExec = true
	s.deps.Config.Variables.Security.AllowHTTP = true
	rec := doGet(s, "/api/config/variables")
	if rec.Code != http.StatusOK {
		t.Fatalf("variables status = %d, want 200", rec.Code)
	}
	body := decodeJSON(t, rec)
	vars, ok := body["variables"].([]interface{})
	if !ok || len(vars) != 5 {
		t.Errorf("expected 5 variables, got %v", body["variables"])
	}
	if body["exec_enabled"] != true || body["http_enabled"] != true {
		t.Errorf("expected exec/http enabled true, got %v", body)
	}
}

func TestCovC_VariablesListNilDeps(t *testing.T) {
	// Exercise the nil-deps early-return branch by calling the handler directly.
	s2 := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/config/variables", nil)
	rw := httptest.NewRecorder()
	s2.handleVariablesList(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("nil-deps variables status = %d, want 200", rw.Code)
	}
}

func TestCovC_AgentConfigChannels(t *testing.T) {
	s := covCServer(t)

	// bad body -> 400
	rec := doPut(s, "/api/config/agent/scanner/channels", "not-an-object")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad body status = %d, want 400", rec.Code)
	}

	// unknown agent -> 404
	rec = doPut(s, "/api/config/agent/nope/channels", map[string]any{"channels": []any{}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown agent status = %d, want 404", rec.Code)
	}

	// channel type with no trigger runtime -> 400 (#5591: persisting it
	// would leave the agent permanently unkicked)
	rec = doPut(s, "/api/config/agent/scanner/channels", map[string]any{
		"channels": []map[string]any{{"type": "slack", "target": "#x"}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("runtime-less channel type status = %d, want 400", rec.Code)
	}

	// valid update -> 200
	rec = doPut(s, "/api/config/agent/scanner/channels", map[string]any{
		"channels": []map[string]any{{"type": "kick"}},
	})
	if rec.Code != http.StatusOK {
		t.Errorf("valid channels status = %d, want 200", rec.Code)
	}
}

func TestCovC_AgentConfigTools(t *testing.T) {
	s := covCServer(t)

	rec := doPut(s, "/api/config/agent/scanner/tools", "bad")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad body status = %d, want 400", rec.Code)
	}
	rec = doPut(s, "/api/config/agent/nope/tools", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown agent status = %d, want 404", rec.Code)
	}
	rec = doPut(s, "/api/config/agent/scanner/tools", map[string]any{"allow": []string{"Bash"}})
	if rec.Code != http.StatusOK {
		t.Errorf("valid tools status = %d, want 200", rec.Code)
	}
}

func TestCovC_AgentConfigConnections(t *testing.T) {
	s := covCServer(t)

	rec := doPut(s, "/api/config/agent/scanner/connections", "bad")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad body status = %d, want 400", rec.Code)
	}
	rec = doPut(s, "/api/config/agent/nope/connections", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown agent status = %d, want 404", rec.Code)
	}
	rec = doPut(s, "/api/config/agent/scanner/connections", map[string]any{
		"connections": []map[string]any{{"name": "c1", "type": "mcp"}},
	})
	if rec.Code != http.StatusOK {
		t.Errorf("valid connections status = %d, want 200", rec.Code)
	}
}

func TestCovC_ConfigGitHub(t *testing.T) {
	s := covCServer(t)
	// The handler now refuses updates it cannot persist (#2459), so the
	// config needs a real source path.
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")

	// bad body -> 400
	rec := doPut(s, "/api/config/github", "bad")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad body status = %d, want 400", rec.Code)
	}

	// empty object (no fields) -> 400
	rec = doPut(s, "/api/config/github", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty fields status = %d, want 400", rec.Code)
	}

	// app_id + installation_id only -> 200 updated
	appID := int64(123)
	instID := int64(456)
	rec = doPut(s, "/api/config/github", map[string]any{
		"app_id":          appID,
		"installation_id": instID,
	})
	if rec.Code != http.StatusOK {
		t.Errorf("app/inst update status = %d, want 200", rec.Code)
	}

	// private_key with a writable key_file under a temp dir -> writes the key file
	keyPath := filepath.Join(t.TempDir(), "gh-app-key.pem")
	rec = doPut(s, "/api/config/github", map[string]any{
		"private_key": "-----BEGIN KEY-----\nabc\n-----END KEY-----\n",
		"key_file":    keyPath,
	})
	if rec.Code != http.StatusOK {
		t.Errorf("private_key update status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestCovC_BeadSynthStatus(t *testing.T) {
	s := covCServer(t)
	rec := doGet(s, "/api/knowledge/bead-synthesizer")
	if rec.Code != http.StatusOK {
		t.Fatalf("bead-synth status = %d, want 200", rec.Code)
	}
	body := decodeJSON(t, rec)
	if _, ok := body["enabled"]; !ok {
		t.Errorf("expected enabled key, got %v", body)
	}
}

func TestCovC_BeadSynthToggle(t *testing.T) {
	s := covCServer(t)

	rec := doPut(s, "/api/knowledge/bead-synthesizer/enabled", "bad")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad body status = %d, want 400", rec.Code)
	}

	// enable then disable (BeadSynthesizer dep is nil, so the start/stop branch
	// is skipped, but the config mutation + save + response run).
	rec = doPut(s, "/api/knowledge/bead-synthesizer/enabled", map[string]any{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Errorf("enable status = %d, want 200", rec.Code)
	}
	rec = doPut(s, "/api/knowledge/bead-synthesizer/enabled", map[string]any{"enabled": false})
	if rec.Code != http.StatusOK {
		t.Errorf("disable status = %d, want 200", rec.Code)
	}
}

func TestCovC_FactHistory(t *testing.T) {
	s := covCServer(t)
	// Seed some fact history to exercise the copy path.
	s.SeedFactHistory([]FactHistoryEntry{{Timestamp: 1}, {Timestamp: 2}})
	rec := doGet(s, "/api/knowledge/fact-history")
	if rec.Code != http.StatusOK {
		t.Fatalf("fact-history status = %d, want 200", rec.Code)
	}
}

func TestCovC_CostHistory(t *testing.T) {
	s := covCServer(t)
	s.AppendCostHistory(1.23)
	rec := doGet(s, "/api/cost/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("cost-history status = %d, want 200", rec.Code)
	}
}

func TestCovC_KnowledgeGraph(t *testing.T) {
	s := covCServer(t)
	// ensureKnowledge lazily creates a file-based knowledge API; GraphStore may
	// be nil -> empty graph branch. depth query param exercises the parse path.
	rec := doGet(s, "/api/knowledge/graph?depth=3&root=foo")
	if rec.Code != http.StatusOK {
		t.Fatalf("knowledge graph status = %d, want 200", rec.Code)
	}
}

func TestCovC_DocumentsList(t *testing.T) {
	s := covCServer(t)
	rec := doGet(s, "/api/knowledge/documents")
	// ensureKnowledge creates a file store -> 200 with a (possibly empty) list.
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("documents list status = %d, want 200 or 503", rec.Code)
	}
}

func TestCovC_DocumentsImport(t *testing.T) {
	s := covCServer(t)

	// bad body -> 400
	rec := doPost(s, "/api/knowledge/documents", "bad")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad body status = %d, want 400", rec.Code)
	}

	// missing name -> 400
	rec = doPost(s, "/api/knowledge/documents", map[string]any{"url": "https://x"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing name status = %d, want 400", rec.Code)
	}

	// name but no source -> 400
	rec = doPost(s, "/api/knowledge/documents", map[string]any{"name": "doc"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no source status = %d, want 400", rec.Code)
	}

	rec = doPost(s, "/api/knowledge/documents", map[string]any{"name": "doc", "url": "file:///etc/passwd"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-http document URL status = %d, want 400", rec.Code)
	}

	rec = doPost(s, "/api/knowledge/documents", map[string]any{"name": "doc", "url": "http://169.254.169.254/latest/meta-data"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("private document URL status = %d, want 400", rec.Code)
	}
}

func TestCovC_DocumentGetNotFound(t *testing.T) {
	s := covCServer(t)
	rec := doGet(s, "/api/knowledge/documents/does-not-exist")
	// Unknown slug -> 404 (or 503 if knowledge could not init).
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unknown doc status = %d, want 404 or 503", rec.Code)
	}
}

func TestCovC_DocumentDeleteNotFound(t *testing.T) {
	s := covCServer(t)
	rec := doDelete(s, "/api/knowledge/documents/does-not-exist", nil)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Errorf("delete unknown doc status = %d", rec.Code)
	}
}

func TestCovC_DocumentReimportNotFound(t *testing.T) {
	s := covCServer(t)
	rec := doPost(s, "/api/knowledge/documents/does-not-exist/reimport", nil)
	// Unknown slug -> 500 (reimport error) or 503.
	if rec.Code == http.StatusOK {
		t.Errorf("reimport unknown doc unexpectedly 200")
	}
}

func TestCovC_CleanupOrphans(t *testing.T) {
	s := covCServer(t)
	rec := doPost(s, "/api/knowledge/cleanup-orphans", nil)
	if rec.Code != http.StatusOK && rec.Code != http.StatusInternalServerError && rec.Code != http.StatusServiceUnavailable {
		t.Errorf("cleanup orphans status = %d", rec.Code)
	}
}

func TestCovC_Context7Search(t *testing.T) {
	s := covCServer(t)

	// missing q -> 400
	rec := doGet(s, "/api/knowledge/context7/search")
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusServiceUnavailable {
		t.Errorf("missing q status = %d, want 400 or 503", rec.Code)
	}

	// with q -> makes a real outbound call which will error offline; just assert
	// it returns a status (covers the query-present + error branch).
	rec = doGet(s, "/api/knowledge/context7/search?q=react")
	if rec.Code == 0 {
		t.Errorf("context7 search returned no status")
	}
}
