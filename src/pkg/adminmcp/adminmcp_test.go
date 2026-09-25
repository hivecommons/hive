package adminmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type staticProvider struct{ seenTool string }

func (p *staticProvider) Read(_ context.Context, tool string, _ map[string]any) (any, error) {
	p.seenTool = tool
	return map[string]any{"status": "ok", "dashboard_token": "secret-value"}, nil
}

func TestCapResultBoundsKnownListFields(t *testing.T) {
	input := map[string]any{"agents": []any{"a", "b", "c"}, "other": []any{"x", "y", "z"}}
	capped, ok := CapResult(input, 2).(map[string]any)
	if !ok {
		t.Fatalf("capped = %#v", capped)
	}
	agents, ok := capped["agents"].([]any)
	if !ok || len(agents) != 2 || capped["agents_truncated"] != true {
		t.Fatalf("agents cap = %#v", capped)
	}
	meta, ok := capped["_admin_mcp"].(map[string]any)
	if !ok || meta["truncated"] != true {
		t.Fatalf("missing truncation disclosure: %#v", capped)
	}
	other, ok := capped["other"].([]any)
	if !ok || len(other) != 3 {
		t.Fatalf("unexpected non-list cap = %#v", capped)
	}
}

func TestCapResultDisclosesTopLevelArrayTruncation(t *testing.T) {
	capped, ok := CapResult([]any{"a", "b", "c"}, 2).(map[string]any)
	if !ok {
		t.Fatalf("capped = %#v", capped)
	}
	items, ok := capped["items"].([]any)
	if !ok || len(items) != 2 || capped["truncated"] != true || capped["limit"] != 2 {
		t.Fatalf("top-level array cap = %#v", capped)
	}
}

func TestReadPathCoversPhaseTwoReadSurface(t *testing.T) {
	for _, tool := range []string{
		ToolFleetStatus,
		ToolAgentsList,
		ToolLeasesList,
		ToolClaimsList,
		ToolPlansList,
		ToolAuditLog,
		ToolSettingsRead,
		ToolAutonomyReadiness,
		ToolSpendRead,
		ToolContributorsList,
		ToolKnowledgeRead,
		ToolHiveAdvisor,
	} {
		if !AllowedTool(tool) {
			t.Fatalf("%s is not allowed", tool)
		}
		if path, ok := ReadPath(tool, 2); !ok || path == "" {
			t.Fatalf("%s path = %q, %v", tool, path, ok)
		}
	}
}

func TestHandlerReadToolScrubsAndWraps(t *testing.T) {
	provider := &staticProvider{}
	h := NewHandler(provider)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hive_status","arguments":{}}}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	text := resultText(t, rec.Body.Bytes())
	if provider.seenTool != ToolHiveStatus {
		t.Fatalf("tool = %q", provider.seenTool)
	}
	if strings.Contains(text, "secret-value") || !strings.Contains(text, "[masked:token]") {
		t.Fatalf("scrubbed text = %s", text)
	}
}

func TestExclusionCatalogueIsAskable(t *testing.T) {
	h := NewHandler(nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exclusion_catalogue","arguments":{}}}`))
	h.ServeHTTP(rec, req)
	text := resultText(t, rec.Body.Bytes())
	if !strings.Contains(text, "POST /api/self-upgrade") || !strings.Contains(text, RefusalKindMechanical) {
		t.Fatalf("catalogue text = %s", text)
	}
}

func TestRefusalReasonsMatchDesignDoc(t *testing.T) {
	doc, err := os.ReadFile("../../docs/design/admin-mcp.md")
	if err != nil {
		t.Fatal(err)
	}
	normalizedDoc := strings.ToLower(normalizeWhitespace(string(doc)))
	for _, ex := range Exclusions() {
		if !strings.Contains(normalizedDoc, strings.ToLower(normalizeWhitespace(ex.Operation))) {
			t.Fatalf("design doc does not record operation %q", ex.Operation)
		}
		if ex.Reason == "" {
			t.Fatalf("empty runtime reason for %q", ex.Operation)
		}
		for _, sentence := range strings.Split(ex.Reason, ".") {
			sentence = strings.TrimSpace(sentence)
			if len(sentence) < 24 {
				continue
			}
			if !strings.Contains(normalizedDoc, strings.ToLower(normalizeWhitespace(sentence))) {
				t.Fatalf("runtime reason for %q is not recorded in design doc: %q", ex.Operation, sentence)
			}
		}
	}
}

func resultText(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("rpc error: %#v body=%s", resp.Error, body)
	}
	if len(resp.Result.Content) != 1 {
		t.Fatalf("content = %#v", resp.Result.Content)
	}
	return resp.Result.Content[0].Text
}

func normalizeWhitespace(s string) string { return strings.Join(strings.Fields(s), " ") }

type writeProvider struct {
	staticProvider
	requests []WriteRequest
	err      error
}

func (p *writeProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	p.requests = append(p.requests, req)
	if p.err != nil {
		return nil, p.err
	}
	return map[string]any{"ok": true}, nil
}

func TestWritePreviewDisabledByDefault(t *testing.T) {
	h := NewHandler(&writeProvider{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"agent.pause","args":{"agent":"scanner"}}}}`))
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), ErrWritesDisabled.Error()) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestWriteConfirmationPersistsAndExecutesAfterRestart(t *testing.T) {
	path := t.TempDir() + "/pending.json"
	store := NewFilePendingStore(path)
	provider := &writeProvider{}
	preview := NewHandler(provider, WithWritesEnabled(true), WithPendingStore(store), WithHiveID("hive-a"))
	rec := httptest.NewRecorder()
	preview.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"agent.pause","args":{"agent":"scanner"}}}}`)))
	text := resultText(t, rec.Body.Bytes())
	var env struct {
		Data struct {
			ConfirmationID string `json:"confirmation_id"`
			Preview        struct {
				WideningDisclosure string `json:"widening_disclosure"`
			} `json:"preview"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.ConfirmationID == "" || !strings.Contains(env.Data.Preview.WideningDisclosure, "No widening") {
		t.Fatalf("preview envelope = %s", text)
	}

	restartedProvider := &writeProvider{}
	restarted := NewHandler(restartedProvider, WithWritesEnabled(true), WithPendingStore(NewFilePendingStore(path)), WithHiveID("hive-a"))
	rec = httptest.NewRecorder()
	restarted.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"`+env.Data.ConfirmationID+`"}}}`)))
	_ = resultText(t, rec.Body.Bytes())
	if len(restartedProvider.requests) != 1 || restartedProvider.requests[0].Path != "/api/pause/scanner" || restartedProvider.requests[0].Method != http.MethodPost {
		t.Fatalf("requests = %#v", restartedProvider.requests)
	}

	reuse := httptest.NewRecorder()
	restarted.ServeHTTP(reuse, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"`+env.Data.ConfirmationID+`"}}}`)))
	if !strings.Contains(reuse.Body.String(), ErrConfirmationMissing.Error()) {
		t.Fatalf("reuse body = %s", reuse.Body.String())
	}
}

func TestWriteConfirmPassesHiveRefusalBodyThrough(t *testing.T) {
	provider := &writeProvider{err: &HiveRefusalError{StatusCode: http.StatusForbidden, Message: "owner access required", Body: []byte(`{"error":"owner access required"}`)}}
	h := NewHandler(provider, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
	preview := httptest.NewRecorder()
	h.ServeHTTP(preview, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"agent.resume","args":{"agent":"scanner"}}}}`)))
	text := resultText(t, preview.Body.Bytes())
	var env struct {
		Data struct {
			ConfirmationID string `json:"confirmation_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	confirm := httptest.NewRecorder()
	h.ServeHTTP(confirm, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"`+env.Data.ConfirmationID+`"}}}`)))
	text = resultText(t, confirm.Body.Bytes())
	if !strings.Contains(text, `"type":"hive-refusal"`) || !strings.Contains(text, `"error":"owner access required"`) {
		t.Fatalf("refusal text = %s", text)
	}
}

func TestAgentWriteOpsEscapeAgentPathSegment(t *testing.T) {
	preview, err := agentPauseOp{}.Preview(context.Background(), map[string]any{"agent": "team/scanner"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Request.Path != "/api/pause/team%2Fscanner" {
		t.Fatalf("pause path = %q", preview.Request.Path)
	}
	preview, err = agentResumeOp{}.Preview(context.Background(), map[string]any{"agent": "team/scanner"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Request.Path != "/api/resume/team%2Fscanner" {
		t.Fatalf("resume path = %q", preview.Request.Path)
	}
}
