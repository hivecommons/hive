package adminmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefusalForMatchesOperationExactly(t *testing.T) {
	for _, operation := range []string{"knowledge", "api", "post", "upgrade", "a", "self-upgrade", "knowledge_read"} {
		data, matched := RefusalFor(operation)
		if matched {
			t.Fatalf("RefusalFor(%q) matched recorded exclusion %q", operation, data.Operation)
		}
		if data.Kind != RefusalKindSwitchedOff || data.Operation != operation {
			t.Fatalf("RefusalFor(%q) = %#v, want generic switched-off refusal for the asked operation", operation, data)
		}
	}
	for _, operation := range []string{"POST /api/self-upgrade", "  post /API/Self-Upgrade ", "Knowledge Writes"} {
		data, matched := RefusalFor(operation)
		if !matched {
			t.Fatalf("RefusalFor(%q) did not match its recorded exclusion", operation)
		}
		if !strings.EqualFold(strings.TrimSpace(operation), data.Operation) {
			t.Fatalf("RefusalFor(%q) returned %q", operation, data.Operation)
		}
	}
}

type bigProvider struct{ data any }

func (p bigProvider) Read(context.Context, string, map[string]any) (any, error) { return p.data, nil }

func callToolText(t *testing.T, h *Handler, tool string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":{}}}`))
	h.ServeHTTP(rec, req)
	var resp struct {
		Error  *rpcError `json:"error"`
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("oversized result returned rpc error %#v instead of a truncated result", resp.Error)
	}
	if len(resp.Result.Content) != 1 {
		t.Fatalf("content = %#v", resp.Result.Content)
	}
	return resp.Result.Content[0].Text
}

func truncationMeta(t *testing.T, text string) map[string]any {
	t.Helper()
	if len(text) > MaxTextBytes {
		t.Fatalf("text is %d bytes, cap is %d", len(text), MaxTextBytes)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("truncated text is not valid JSON: %v", err)
	}
	meta, _ := env.Data["_admin_mcp"].(map[string]any)
	if meta["truncated"] != true {
		t.Fatalf("missing truncation disclosure in %.200s", text)
	}
	return env.Data
}

func TestHandlerTruncatesOversizedUncappedListInsteadOfErroring(t *testing.T) {
	repos := make([]any, 4000)
	for i := range repos {
		repos[i] = map[string]any{"name": "repo", "health": strings.Repeat("x", 120)}
	}
	h := NewHandler(bigProvider{data: map[string]any{"status": "ok", "repos": repos}})
	data := truncationMeta(t, callToolText(t, h, ToolFleetStatus))
	if data["status"] != "ok" {
		t.Fatalf("non-list fields dropped: %#v", data["status"])
	}
	kept, _ := data["repos"].([]any)
	if len(kept) == 0 || len(kept) >= len(repos) {
		t.Fatalf("repos kept = %d of %d", len(kept), len(repos))
	}
	if data["repos_truncated"] != true {
		t.Fatalf("repos_truncated missing")
	}
}

func TestHandlerTruncatesOversizedScalarResult(t *testing.T) {
	h := NewHandler(bigProvider{data: map[string]any{"blob": strings.Repeat("y", MaxTextBytes*2)}})
	data := truncationMeta(t, callToolText(t, h, ToolHiveStatus))
	if _, ok := data["text_preview"].(string); !ok {
		t.Fatalf("missing text_preview: %#v", data)
	}
}

func TestWritesUnavailableReasonFailsFastBeforePreview(t *testing.T) {
	const reason = "caller authenticated without a dashboard bearer"
	h := NewHandler(&writeProvider{}, WithWritesEnabled(true), WithWritesUnavailable(reason))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"agent.pause","args":{"agent":"scanner"}}}}`)))
	if strings.Contains(rec.Body.String(), "confirmation_id") || !strings.Contains(rec.Body.String(), reason) {
		t.Fatalf("preview was not refused with the reason: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	var list struct {
		Result struct {
			Tools []struct {
				Name     string       `json:"name"`
				Metadata ToolMetadata `json:"metadata"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Result.Tools {
		if tool.Name == ToolWritePreview && (tool.Metadata.Preview.Enabled || !strings.Contains(tool.Metadata.Preview.Note, reason)) {
			t.Fatalf("write_preview metadata = %#v", tool.Metadata)
		}
	}

	text := callToolText(t, h, ToolExclusionCatalogue)
	if !strings.Contains(text, `"writes_enabled":false`) || !strings.Contains(text, reason) {
		t.Fatalf("catalogue = %.400s", text)
	}
}
