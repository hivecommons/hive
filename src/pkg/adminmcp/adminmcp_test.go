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
	other, ok := capped["other"].([]any)
	if !ok || len(other) != 3 {
		t.Fatalf("unexpected non-list cap = %#v", capped)
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
