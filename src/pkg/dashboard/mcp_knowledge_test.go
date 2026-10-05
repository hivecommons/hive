package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// publicKnowledgeServer builds a token-secured spoke with a vault holding one
// fact of every interesting kind: a public gotcha, a tagged public pattern,
// and a private governance fact that must never leave through /mcp/knowledge.
func publicKnowledgeServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "dashboard-secret", logger)
	s.RegisterAPI(testDeps(t))

	vault := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(vault, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("gotcha-nil-map.md", "---\ntitle: Nil Map Gotcha\ntype: gotcha\n---\nAlways initialize maps before use.\n")
	write("pattern-caching.md", "---\ntitle: Caching Pattern\ntype: pattern\ntags: [linux, public]\n---\nUse Redis for caching hot paths.\n")
	write("vision-world-domination.md", "---\ntitle: Secret Roadmap\ntype: vision\n---\nWe will acquire a competitor in Q3.\n")
	// Long body (>200 runes) with a Related link to a private fact: exercises
	// full-body re-reads and Related filtering.
	write("pattern-long-body.md", "---\ntitle: Long Caching Notes\ntype: pattern\nrelated: [pattern-caching, vision-world-domination]\n---\n"+strings.Repeat("Redis caching detail sentence. ", 20)+"END-MARKER\n")

	api := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, logger)
	if err := api.ConnectVault(vault, "vault"); err != nil {
		t.Fatalf("ConnectVault: %v", err)
	}
	s.deps.Knowledge = api
	return s
}

func mcpCall(t *testing.T, s *Server, payload string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, publicKnowledgeMCPPath, bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately anonymous: no Authorization, no X-Hive-* headers. Goes
	// through the full handler chain so the authenticate middleware is tested.
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeRPC(t *testing.T, rec *httptest.ResponseRecorder) jsonRPCResponse {
	t.Helper()
	var resp jsonRPCResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return resp
}

// toolText extracts the single text content of a tools/call result.
func rpcToolText(t *testing.T, resp jsonRPCResponse) (string, bool) {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", resp.Error)
	}
	raw, _ := json.Marshal(resp.Result)
	var res mcpToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode tool result %s: %v", raw, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("content = %+v, want 1 item", res.Content)
	}
	return res.Content[0].Text, res.IsError
}

func TestPublicKnowledgeMCPDisabledByDefault(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "")
	s := publicKnowledgeServer(t)

	rec := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("switch off: status = %d, want non-200; body=%q", rec.Code, rec.Body.String())
	}
	if isPublicPath(publicKnowledgeMCPPath) {
		t.Fatal("switch off: /mcp/knowledge must not be a public path")
	}
}

func TestPublicKnowledgeMCPDisabledEvenWhenAuthenticated(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "0")
	s := publicKnowledgeServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, publicKnowledgeMCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer dashboard-secret")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("switch off + owner token: status = %d, want 404 (the surface does not exist until the owner opens it)", rec.Code)
	}
}

func TestPublicKnowledgeMCPEnabledSpellings(t *testing.T) {
	for _, v := range []string{"1", "true", "YES", " on "} {
		t.Setenv(publicKnowledgeEnabledEnv, v)
		if !publicKnowledgeEnabled() {
			t.Errorf("%q should enable", v)
		}
	}
	for _, v := range []string{"", "0", "false", "off", "maybe"} {
		t.Setenv(publicKnowledgeEnabledEnv, v)
		if publicKnowledgeEnabled() {
			t.Errorf("%q should not enable", v)
		}
	}
}

func TestPublicKnowledgeMCPHandshakeAndToolsList(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "1")
	s := publicKnowledgeServer(t)

	if !isPublicPath(publicKnowledgeMCPPath) {
		t.Fatal("switch on: /mcp/knowledge must be public")
	}

	rec := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"goose","version":"1"}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize anonymous: status = %d body=%q", rec.Code, rec.Body.String())
	}
	resp := decodeRPC(t, rec)
	if resp.Error != nil {
		t.Fatalf("initialize error: %+v", resp.Error)
	}
	init := resp.Result.(map[string]interface{})
	if init["protocolVersion"] != mcpProtocolVersion {
		t.Errorf("protocolVersion = %v", init["protocolVersion"])
	}

	rec = mcpCall(t, s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Fatalf("notification: status = %d body=%q, want 202 and empty", rec.Code, rec.Body.String())
	}

	rec = mcpCall(t, s, `{"jsonrpc":"2.0","id":"abc","method":"ping"}`)
	if resp = decodeRPC(t, rec); resp.Error != nil || string(resp.ID) != `"abc"` {
		t.Fatalf("ping: %+v id=%s", resp.Error, resp.ID)
	}

	rec = mcpCall(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	resp = decodeRPC(t, rec)
	raw, _ := json.Marshal(resp.Result)
	var list struct {
		Tools []mcpToolDef `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, td := range list.Tools {
		names = append(names, td.Name)
		ann, _ := td.Annotations.(map[string]interface{})
		if ann["readOnlyHint"] != true || ann["destructiveHint"] != false {
			t.Errorf("tool %s must be annotated read-only, got %v", td.Name, td.Annotations)
		}
		for _, verb := range []string{"create", "update", "delete", "write", "import", "promote"} {
			if strings.Contains(td.Name, verb) {
				t.Errorf("tool %s looks like a write tool; public surface must be read-only", td.Name)
			}
		}
	}
	if got := strings.Join(names, ","); got != "knowledge_search,knowledge_get,knowledge_export" {
		t.Fatalf("tools = %s", got)
	}
}

func TestPublicKnowledgeMCPSearchGetExportHidePrivateTypes(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "true")
	t.Setenv(publicKnowledgeTagsEnv, "")
	s := publicKnowledgeServer(t)

	// Export: public types present, vision absent, sources never rendered.
	resp := decodeRPC(t, mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_export","arguments":{}}}`))
	md, isErr := rpcToolText(t, resp)
	if isErr {
		t.Fatalf("export isError: %s", md)
	}
	for _, want := range []string{"# Hive Public Knowledge", "## Gotchas", "### Nil Map Gotcha", "## Patterns", "### Caching Pattern", "Tags: linux, public"} {
		if !strings.Contains(md, want) {
			t.Errorf("export missing %q:\n%s", want, md)
		}
	}
	for _, leak := range []string{"Secret Roadmap", "acquire a competitor", "## Vision"} {
		if strings.Contains(md, leak) {
			t.Errorf("export leaked private content %q:\n%s", leak, md)
		}
	}

	// Get: public slug resolves, private slug is "not found" (not 'forbidden',
	// which would confirm existence).
	resp = decodeRPC(t, mcpCall(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"knowledge_get","arguments":{"slug":"gotcha-nil-map"}}}`))
	text, isErr := rpcToolText(t, resp)
	if isErr || !strings.Contains(text, "Always initialize maps") {
		t.Fatalf("get public: isErr=%v text=%s", isErr, text)
	}
	var pf publicFact
	if err := json.Unmarshal([]byte(text), &pf); err != nil {
		t.Fatalf("get result not a publicFact: %v", err)
	}
	var generic map[string]interface{}
	_ = json.Unmarshal([]byte(text), &generic)
	for _, hidden := range []string{"sources", "usage_count", "confidence_reason", "last_used", "phase"} {
		if _, ok := generic[hidden]; ok {
			t.Errorf("public fact exposes %q", hidden)
		}
	}

	resp = decodeRPC(t, mcpCall(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"knowledge_get","arguments":{"slug":"vision-world-domination"}}}`))
	text, isErr = rpcToolText(t, resp)
	if !isErr || !strings.Contains(text, "fact not found") {
		t.Fatalf("get private: isErr=%v text=%s, want not-found", isErr, text)
	}

	// Search: a query that only matches the private fact returns nothing.
	resp = decodeRPC(t, mcpCall(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"knowledge_search","arguments":{"query":"competitor"}}}`))
	text, _ = rpcToolText(t, resp)
	if strings.Contains(text, "Secret Roadmap") {
		t.Fatalf("search leaked private fact: %s", text)
	}
	resp = decodeRPC(t, mcpCall(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"knowledge_search","arguments":{"query":"maps","limit":1000}}}`))
	text, _ = rpcToolText(t, resp)
	if !strings.Contains(text, "Nil Map Gotcha") {
		t.Fatalf("search did not find public fact: %s", text)
	}
}

func TestPublicKnowledgeMCPTagAllowList(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "1")
	t.Setenv(publicKnowledgeTagsEnv, "Public, troubleshooting")
	s := publicKnowledgeServer(t)

	resp := decodeRPC(t, mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_export"}}`))
	md, _ := rpcToolText(t, resp)
	if !strings.Contains(md, "Caching Pattern") {
		t.Errorf("tagged fact missing:\n%s", md)
	}
	if strings.Contains(md, "Nil Map Gotcha") {
		t.Errorf("untagged fact exposed despite tag allow-list:\n%s", md)
	}
}

func TestPublicKnowledgeMCPRejectsWritesAndBadInput(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "1")
	s := publicKnowledgeServer(t)

	cases := map[string]int{
		`{"jsonrpc":"2.0","id":1,"method":"resources/write"}`:                                                    jsonRPCMethodNotFound,
		`{"jsonrpc":"2.0","id":1,"method":"knowledge/create"}`:                                                   jsonRPCMethodNotFound,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_create","arguments":{}}}`:     jsonRPCInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_delete","arguments":{}}}`:     jsonRPCInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_search","arguments":{}}}`:     jsonRPCInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_get","arguments":{}}}`:        jsonRPCInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_search","arguments":"nope"}}`: jsonRPCInvalidParams,
		`{"jsonrpc":"1.0","id":1,"method":"tools/list"}`:                                                         jsonRPCInvalidRequest,
		`not json`: jsonRPCParseError,
	}
	for payload, wantCode := range cases {
		resp := decodeRPC(t, mcpCall(t, s, payload))
		if resp.Error == nil || resp.Error.Code != wantCode {
			t.Errorf("%s: error = %+v, want code %d", payload, resp.Error, wantCode)
		}
	}

	// Non-POST verbs are refused outright; there is no SSE stream to open.
	for _, m := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(m, publicKnowledgeMCPPath, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", m, rec.Code)
		}
	}

	// Oversized bodies are rejected before parsing.
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", publicKnowledgeMaxBodyBytes) + `"}}`
	if rec := mcpCall(t, s, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: status = %d, want 413", rec.Code)
	}

	// Search with a private type filter is refused as a tool error.
	resp := decodeRPC(t, mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_search","arguments":{"query":"x","type":"vision"}}}`))
	if text, isErr := rpcToolText(t, resp); !isErr || !strings.Contains(text, "not a public fact type") {
		t.Errorf("private type filter: isErr=%v text=%s", isErr, text)
	}
}

func TestPublicKnowledgeMCPNoKnowledgeBase(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "1")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewServerWithAuth(0, "", logger)
	// deps nil → ensureKnowledge false → graceful text, never a panic.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, publicKnowledgeMCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_export"}}`))
	s.handlePublicKnowledgeMCP(rec, req)
	resp := decodeRPC(t, rec)
	if text, _ := rpcToolText(t, resp); !strings.Contains(text, "not available") {
		t.Fatalf("text = %s", text)
	}
}

func TestPublicKnowledgeTypeListExcludesGovernance(t *testing.T) {
	got := strings.Join(publicKnowledgeTypeList(), ",")
	for _, private := range []string{"idea", "vision", "constitution", "requirement", "constraint", "stakeholder", "decision"} {
		if strings.Contains(got, private) {
			t.Errorf("public type list includes private type %q: %s", private, got)
		}
	}
	if !strings.Contains(got, "gotcha") || !strings.Contains(got, "pattern") {
		t.Errorf("public type list = %s", got)
	}
}

// The route is registered with a literal so docs/api-reference.md citations
// can verify it; keep the literal and the constant in lock-step.
func TestPublicKnowledgeMCPPathConstantMatchesRoute(t *testing.T) {
	if publicKnowledgeMCPPath != "/mcp/knowledge" {
		t.Fatalf("publicKnowledgeMCPPath = %q; update the HandleFunc literal in api.go and docs/api-reference.md", publicKnowledgeMCPPath)
	}
}

func TestPublicKnowledgeMCPRejectsBatchRequests(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_KNOWLEDGE", "1")
	s := publicKnowledgeServer(t)
	rec := mcpCall(t, s, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	resp := decodeRPC(t, rec)
	if resp.Error == nil || resp.Error.Code != jsonRPCInvalidRequest {
		t.Fatalf("error = %+v, want code %d", resp.Error, jsonRPCInvalidRequest)
	}
}

func TestPublicKnowledgeMCPGetReturnsFullBodyAndFiltersRelated(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_KNOWLEDGE", "1")
	s := publicKnowledgeServer(t)
	rec := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_get","arguments":{"slug":"pattern-long-body"}}}`)
	text, isErr := rpcToolText(t, decodeRPC(t, rec))
	if isErr {
		t.Fatalf("unexpected tool error: %s", text)
	}
	var pf publicFact
	if err := json.Unmarshal([]byte(text), &pf); err != nil {
		t.Fatalf("decode fact: %v", err)
	}
	if !strings.Contains(pf.Body, "END-MARKER") {
		t.Fatalf("body truncated, want full text ending in END-MARKER: %q", pf.Body)
	}
	if len(pf.Related) != 1 || pf.Related[0] != "pattern-caching" {
		t.Fatalf("related = %v, want only the public slug", pf.Related)
	}

	rec = mcpCall(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"knowledge_export","arguments":{}}}`)
	text, _ = rpcToolText(t, decodeRPC(t, rec))
	if !strings.Contains(text, "END-MARKER") {
		t.Fatal("export should carry full bodies")
	}
	if strings.Contains(text, "vision-world-domination") || strings.Contains(text, "acquire a competitor") {
		t.Fatal("export leaked private fact")
	}
}

func TestPublicKnowledgeMCPSearchTypeFilterAppliesToVaultResults(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_KNOWLEDGE", "1")
	s := publicKnowledgeServer(t)
	// "maps" only matches the gotcha; asking for type=pattern must yield nothing
	// even though the vault store ignores the type argument.
	rec := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_search","arguments":{"query":"initialize maps","type":"pattern"}}}`)
	text, isErr := rpcToolText(t, decodeRPC(t, rec))
	if isErr {
		t.Fatalf("unexpected tool error: %s", text)
	}
	var out struct {
		Count   int          `json:"count"`
		Results []publicFact `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Results {
		if r.Type != "pattern" {
			t.Fatalf("type filter leaked %q: %+v", r.Type, r)
		}
	}
	rec = mcpCall(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"knowledge_search","arguments":{"query":"initialize maps","type":"gotcha"}}}`)
	text, _ = rpcToolText(t, decodeRPC(t, rec))
	if !strings.Contains(text, "gotcha-nil-map") {
		t.Fatalf("gotcha search missing result: %s", text)
	}
}
