package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

func knowledgeTOCServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "", logger)
	s.RegisterAPI(testDeps(t))

	vault := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	files := [][2]string{
		{"current-gotcha.md", "---\ntitle: Current Gotcha\ntype: gotcha\ntags: [repo:hivecommons/hive, ci]\nlayer: project\nsource_url: https://example.com/pr/1\n---\nCurrent body with enough text.\n"},
		{"org-wide-pattern.md", "---\ntitle: Org Pattern\ntype: pattern\nlayer: org\n---\nOrg-wide body.\n"},
		{"other-repo.md", "---\ntitle: Other Repo Fact\ntype: gotcha\ntags: [repo:someone/else]\nlayer: project\n---\nOther repo body.\n"},
		{"draft-idea.md", "---\ntitle: Draft Idea\ntype: pattern\nstate: draft\nlayer: project\n---\nDraft body.\n"},
		{"old-way.md", "---\ntitle: Old Way\ntype: pattern\nstate: deprecated\nlayer: project\n---\nDeprecated body.\n"},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(vault, f[0]), []byte(f[1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	api := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, logger)
	if err := api.ConnectVault(vault, "vault"); err != nil {
		t.Fatalf("ConnectVault: %v", err)
	}
	s.deps.Knowledge = api
	return s
}

func tocGet(t *testing.T, s *Server, target string) (int, map[string]interface{}) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func tocIDs(t *testing.T, body map[string]interface{}) string {
	t.Helper()
	entries, _ := body["entries"].([]interface{})
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.(map[string]interface{})["id"].(string))
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func TestKnowledgeTOCDefaultsToApprovedWithoutBodies(t *testing.T) {
	s := knowledgeTOCServer(t)
	code, body := tocGet(t, s, "/api/knowledge/toc")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got, want := tocIDs(t, body), "current-gotcha,other-repo,org-wide-pattern"; got != want {
		t.Fatalf("ids = %s, want %s", got, want)
	}
	if body["total"] != float64(3) || body["truncated"] != false {
		t.Fatalf("counts = %v", body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "Current body") || strings.Contains(string(raw), "\"body\"") {
		t.Fatalf("toc leaked bodies: %s", raw)
	}
	first := body["entries"].([]interface{})[0].(map[string]interface{})
	if first["repo"] != "hivecommons/hive" || first["status"] != "approved" || first["source"] != "https://example.com/pr/1" || first["updated"] == nil || first["size_bytes"] == nil {
		t.Fatalf("entry metadata = %v", first)
	}
}

func TestKnowledgeTOCScopeFilters(t *testing.T) {
	s := knowledgeTOCServer(t)
	for _, tc := range [][2]string{
		{"/api/knowledge/toc?repos=hivecommons/hive", "current-gotcha,org-wide-pattern"},
		{"/api/knowledge/toc?layers=org", "org-wide-pattern"},
		{"/api/knowledge/toc?types=gotcha&tags=ci", "current-gotcha"},
		{"/api/knowledge/toc?include_states=draft&layers=project", "current-gotcha,draft-idea,other-repo"},
		{"/api/knowledge/toc?include_states=all&layers=project", "current-gotcha,draft-idea,old-way,other-repo"},
	} {
		target, want := tc[0], tc[1]
		code, body := tocGet(t, s, target)
		if code != http.StatusOK {
			t.Fatalf("%s status = %d", target, code)
		}
		if got := tocIDs(t, body); got != want {
			t.Errorf("%s ids = %s, want %s", target, got, want)
		}
	}
	_, body := tocGet(t, s, "/api/knowledge/toc?limit=1")
	if body["truncated"] != true || body["returned"] != float64(1) || body["total"] != float64(3) {
		t.Fatalf("capped counts = %v", body)
	}
}

func TestKnowledgeTOCPromptFormatIsBounded(t *testing.T) {
	s := knowledgeTOCServer(t)
	_, body := tocGet(t, s, "/api/knowledge/toc?format=prompt&max_chars=200")
	prompt, _ := body["prompt"].(string)
	if prompt == "" || len(prompt) > 200 {
		t.Fatalf("prompt len = %d: %q", len(prompt), prompt)
	}
	if !strings.Contains(prompt, "more entries not listed") {
		t.Fatalf("prompt missing omitted footer: %q", prompt)
	}
	_, plain := tocGet(t, s, "/api/knowledge/toc")
	if _, ok := plain["prompt"]; ok {
		t.Fatal("prompt must be opt-in")
	}
}

func TestKnowledgeTOCRejectsBadLifecycleState(t *testing.T) {
	s := knowledgeTOCServer(t)
	for _, target := range []string{"/api/knowledge/toc?include_states=bogus", "/api/knowledge/entry/current-gotcha?include_states=bogus"} {
		if code, _ := tocGet(t, s, target); code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", target, code)
		}
	}
}

func TestKnowledgeEntryFullRead(t *testing.T) {
	s := knowledgeTOCServer(t)
	code, body := tocGet(t, s, "/api/knowledge/entry/current-gotcha")
	if code != http.StatusOK {
		t.Fatalf("status = %d body=%v", code, body)
	}
	md, _ := body["markdown"].(string)
	for _, want := range []string{"---\ntitle: Current Gotcha\n", "state: approved\n", "layer: project\n", "Current body with enough text."} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	if body["id"] != "current-gotcha" || body["repo"] != "hivecommons/hive" || body["status"] != "approved" || body["fact"] == nil {
		t.Fatalf("entry = %v", body)
	}
}

func TestKnowledgeEntryHonoursLifecycleAndScope(t *testing.T) {
	s := knowledgeTOCServer(t)
	for _, tc := range []struct {
		target string
		want   int
	}{
		{"/api/knowledge/entry/draft-idea", http.StatusNotFound},
		{"/api/knowledge/entry/draft-idea?include_states=draft", http.StatusOK},
		{"/api/knowledge/entry/old-way", http.StatusNotFound},
		{"/api/knowledge/entry/old-way?include_states=deprecated", http.StatusOK},
		{"/api/knowledge/entry/other-repo?repos=hivecommons/hive", http.StatusNotFound},
		{"/api/knowledge/entry/current-gotcha?layers=org", http.StatusNotFound},
		{"/api/knowledge/entry/does-not-exist", http.StatusNotFound},
	} {
		if code, _ := tocGet(t, s, tc.target); code != tc.want {
			t.Errorf("%s status = %d, want %d", tc.target, code, tc.want)
		}
	}
	_, body := tocGet(t, s, "/api/knowledge/entry/old-way?include_states=deprecated")
	if body["status"] != "deprecated" {
		t.Fatalf("status = %v", body["status"])
	}
}

func TestKnowledgeTOCAndEntryWithoutDeps(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "", logger)
	for _, target := range []string{"/api/knowledge/toc", "/api/knowledge/entry/x"} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if target == "/api/knowledge/toc" && (rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`)) {
			t.Errorf("toc without deps = %d %s", rec.Code, rec.Body.String())
		}
		if target != "/api/knowledge/toc" && rec.Code != http.StatusNotFound {
			t.Errorf("entry without deps = %d, want 404", rec.Code)
		}
	}
}

func TestPublicKnowledgeMCPTOCTool(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "true")
	t.Setenv(publicKnowledgeTagsEnv, "")
	s := publicKnowledgeServer(t)

	resp := decodeRPC(t, mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_toc","arguments":{"limit":2}}}`))
	text, isErr := rpcToolText(t, resp)
	if isErr {
		t.Fatalf("toc isError: %s", text)
	}
	var toc knowledge.TOC
	if err := json.Unmarshal([]byte(text), &toc); err != nil {
		t.Fatalf("decode toc: %v", err)
	}
	if toc.Returned != 2 || !toc.Truncated || toc.Total < 3 {
		t.Fatalf("toc = %+v", toc)
	}
	for _, leak := range []string{"Secret Roadmap", "acquire a competitor", "Always initialize maps"} {
		if strings.Contains(text, leak) {
			t.Errorf("toc leaked %q: %s", leak, text)
		}
	}

	resp = decodeRPC(t, mcpCall(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"knowledge_toc","arguments":{"type":"gotcha","repo":"x/y"}}}`))
	text, _ = rpcToolText(t, resp)
	if !strings.Contains(text, "gotcha-nil-map") || strings.Contains(text, "pattern-caching") {
		t.Fatalf("filtered toc = %s", text)
	}

	resp = decodeRPC(t, mcpCall(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"knowledge_toc","arguments":{"type":"vision"}}}`))
	if text, isErr = rpcToolText(t, resp); !isErr {
		t.Fatalf("private type must be rejected, got %s", text)
	}

	resp = decodeRPC(t, mcpCall(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"knowledge_toc","arguments":"bad"}}`))
	if resp.Error == nil {
		t.Fatal("malformed arguments must be a JSON-RPC error")
	}
}
