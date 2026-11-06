package adminmcp

import (
	"context"
	"encoding/json"
	"fmt"
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

// The text cap cut the longest list first, by entry count. On a real
// fleet_status (15 agents, 12 repos, 350 KB) that halved three small 15-entry
// lists that freed a few KB, as well as the two lists that were over the cap.
func TestTextCapCutsTheLargestListNotTheLongest(t *testing.T) {
	small := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"name": fmt.Sprintf("agent-%02d", i), "cadence": "15m"}
		}
		return out
	}
	big := func(n, bytes int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"name": fmt.Sprintf("row-%02d", i), "detail": strings.Repeat("x", bytes)}
		}
		return out
	}
	// repos alone is over the cap; every other list is longer but tiny.
	h := NewHandler(bigProvider{data: map[string]any{
		"status":           "ok",
		"repos":            big(12, 30*1024),
		"cadenceMatrix":    small(15),
		"configuredAgents": small(15),
		"acmmPackAgents":   small(15),
	}})
	data := truncationMeta(t, callToolText(t, h, ToolFleetStatus))
	for _, key := range []string{"cadenceMatrix", "configuredAgents", "acmmPackAgents"} {
		if kept, _ := data[key].([]any); len(kept) != 15 {
			t.Errorf("%s kept %d of 15 entries; a small list was cut though it could not bring the result under the cap", key, len(kept))
		}
		if _, cut := data[key+"_truncated"]; cut {
			t.Errorf("%s is marked truncated", key)
		}
	}
	if kept, _ := data["repos"].([]any); len(kept) == 0 || len(kept) >= 12 {
		t.Fatalf("repos kept %d of 12", len(kept))
	}
	meta, _ := data["_admin_mcp"].(map[string]any)
	fields, _ := meta["text_cap_truncated_fields"].([]any)
	if len(fields) != 1 || fields[0] != "repos" {
		t.Fatalf("text_cap_truncated_fields = %v, want [repos]", fields)
	}
}

// Lists of equal size are cut in name order, so one result is not truncated
// differently from call to call.
func TestTextCapCutsEqualListsInAStableOrder(t *testing.T) {
	rows := func() []any {
		out := make([]any, 8)
		for i := range out {
			out[i] = map[string]any{"detail": strings.Repeat("y", 14*1024)}
		}
		return out
	}
	var first string
	for i := 0; i < 20; i++ {
		b, err := EncodeResult(map[string]any{"alpha": rows(), "beta": rows(), "gamma": rows()})
		if err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = string(b)
			continue
		}
		if string(b) != first {
			t.Fatalf("call %d truncated the same result differently", i)
		}
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(first), &env); err != nil {
		t.Fatal(err)
	}
	// Three 112 KB lists: halving alpha and then beta is enough, so gamma
	// is whole.
	for key, want := range map[string]int{"alpha": 4, "beta": 4, "gamma": 8} {
		if kept, _ := env.Data[key].([]any); len(kept) != want {
			t.Errorf("%s kept %d entries, want %d", key, len(kept), want)
		}
	}
}
