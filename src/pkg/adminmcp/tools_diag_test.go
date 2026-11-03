package adminmcp

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestDiagReadPath(t *testing.T) {
	for _, tc := range []struct {
		tool  string
		args  map[string]any
		path  string
		query url.Values
	}{
		{ToolAgentLogRead, map[string]any{"agent": " scanner "}, "/api/diag/agents/scanner/log", url.Values{}},
		{ToolAgentLogRead, map[string]any{"agent": "a b", "max_bytes": float64(4096), "explain": "only", "limit": float64(3)}, "/api/diag/agents/a%20b/log", url.Values{"max_bytes": {"4096"}, "explain": {"only"}}},
		{ToolAgentKicksList, map[string]any{"agent": "scanner", "max_bytes": "9"}, "/api/diag/agents/scanner/kicks", url.Values{}},
		{ToolAgentKickRead, map[string]any{"agent": "scanner", "kick_id": "20260820-070000.000-kick.log", "max_bytes": "2048"}, "/api/diag/agents/scanner/kicks/20260820-070000.000-kick.log", url.Values{"max_bytes": {"2048"}}},
		{ToolRunLogRead, map[string]any{"run": "org/repo#12", "stage": "plan", "gen": float64(3), "tail": 50, "max_bytes": float64(1024)}, "/api/diag/runs/org%2Frepo%2312/log", url.Values{"stage": {"plan"}, "gen": {"3"}, "tail": {"50"}, "max_bytes": {"1024"}}},
		{ToolRunLogRead, map[string]any{"run": "r1"}, "/api/diag/runs/r1/log", url.Values{}},
		{ToolRunDetailRead, map[string]any{"run": "r1", "max_bytes": float64(5)}, "/api/diag/runs/r1/detail", url.Values{}},
		{ToolHealthDeepRead, nil, "/api/diag/health", url.Values{}},
		{ToolPromptHistoryRead, map[string]any{}, "/api/diag/prompt-history", url.Values{}},
		{ToolPromptHistoryRead, map[string]any{"agent": "scanner", "window_hours": float64(48)}, "/api/diag/prompt-history", url.Values{"agent": {"scanner"}, "windowHours": {"48"}}},
		{ToolConfigEffectiveRead, map[string]any{"offset": "40"}, "/api/diag/config", url.Values{"offset": {"40"}}},
		{ToolConfigEffectiveRead, map[string]any{"offset": float64(0)}, "/api/diag/config", url.Values{}},
	} {
		got, err := DiagReadPath(tc.tool, tc.args)
		if err != nil {
			t.Fatalf("%s %v: %v", tc.tool, tc.args, err)
		}
		path, rawQuery, _ := strings.Cut(got, "?")
		query, _ := url.ParseQuery(rawQuery)
		if path != tc.path || query.Encode() != tc.query.Encode() {
			t.Fatalf("%s %v = %q, want %s?%s", tc.tool, tc.args, got, tc.path, tc.query.Encode())
		}
	}
}

func TestDiagReadPathRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		tool string
		args map[string]any
	}{
		"agent missing":       {ToolAgentLogRead, map[string]any{}},
		"agent blank":         {ToolAgentKicksList, map[string]any{"agent": "  "}},
		"bad explain":         {ToolAgentLogRead, map[string]any{"agent": "a", "explain": "all"}},
		"kick id missing":     {ToolAgentKickRead, map[string]any{"agent": "a"}},
		"kick id traversal":   {ToolAgentKickRead, map[string]any{"agent": "a", "kick_id": "../x"}},
		"kick id slash":       {ToolAgentKickRead, map[string]any{"agent": "a", "kick_id": "a/b"}},
		"run missing":         {ToolRunDetailRead, map[string]any{}},
		"fractional gen":      {ToolRunLogRead, map[string]any{"run": "r", "gen": 1.5}},
		"word tail":           {ToolRunLogRead, map[string]any{"run": "r", "tail": "many"}},
		"negative max":        {ToolAgentLogRead, map[string]any{"agent": "a", "max_bytes": float64(-1)}},
		"object max":          {ToolAgentKickRead, map[string]any{"agent": "a", "kick_id": "k", "max_bytes": map[string]any{}}},
		"word window":         {ToolPromptHistoryRead, map[string]any{"window_hours": "day"}},
		"negative offset":     {ToolConfigEffectiveRead, map[string]any{"offset": -2}},
		"not a diag tool":     {ToolHiveStatus, map[string]any{}},
		"log tool is not one": {ToolLogRead, map[string]any{}},
	} {
		if path, err := DiagReadPath(tc.tool, tc.args); err == nil {
			t.Errorf("%s: built %q", name, path)
		}
	}
}

func TestDiagHTTPError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		err := DiagHTTPError(ToolPromptHistoryRead, status)
		if !errors.Is(err, ErrForbidden) || errors.Is(err, ErrOwnerUnverified) {
			t.Fatalf("%d: %v", status, err)
		}
		err = DiagHTTPError(ToolConfigEffectiveRead, status)
		if !errors.Is(err, ErrOwnerUnverified) || !errors.Is(err, ErrForbidden) {
			t.Fatalf("config %d: %v", status, err)
		}
	}
	err := DiagHTTPError(ToolRunLogRead, http.StatusNotFound)
	if errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404: %v", err)
	}
	if rpc := toolErr(DiagHTTPError(ToolConfigEffectiveRead, http.StatusForbidden)); rpc.Code != -32003 {
		t.Fatalf("owner refusal rpc code = %d", rpc.Code)
	}
}

// TestDiagToolsListedReadOnlyWithoutSharedLimit pins the tool contract: every
// diagnostic read is listed, allowed, read-only, free of the reserved name
// fragments, and bounds itself instead of taking the shared limit.
func TestDiagToolsListedReadOnlyWithoutSharedLimit(t *testing.T) {
	found := map[string]bool{}
	for _, tool := range ToolsWithWritesEnabled(true) {
		name, _ := tool["name"].(string)
		if !DiagTool(name) {
			continue
		}
		found[name] = true
		if !AllowedTool(name) || strings.Contains(name, "advisor") || strings.Contains(name, "select") {
			t.Errorf("%s: allowed=%v", name, AllowedTool(name))
		}
		if desc, _ := tool["description"].(string); !strings.Contains(desc, "Read-only") {
			t.Errorf("%s description does not say read-only: %q", name, desc)
		}
		annotations, _ := tool["annotations"].(map[string]any)
		if annotations["readOnlyHint"] != true {
			t.Errorf("%s annotations = %#v", name, annotations)
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["limit"]; ok {
			t.Errorf("%s must not take the shared limit", name)
		}
		required, _ := schema["required"].([]string)
		if required == nil {
			t.Errorf("%s required list is nil", name)
		}
		for _, key := range required {
			if _, ok := props[key]; !ok {
				t.Errorf("%s requires undeclared %q", name, key)
			}
		}
		if diagTextTool(name) {
			if _, ok := props["max_bytes"]; !ok {
				t.Errorf("%s takes no max_bytes", name)
			}
		}
	}
	for _, tool := range []string{ToolAgentLogRead, ToolAgentKicksList, ToolAgentKickRead, ToolRunLogRead, ToolRunDetailRead, ToolHealthDeepRead, ToolPromptHistoryRead, ToolConfigEffectiveRead} {
		if !found[tool] {
			t.Errorf("%s not listed", tool)
		}
	}
	if !DiagOwnerTool(ToolConfigEffectiveRead) || DiagOwnerTool(ToolPromptHistoryRead) {
		t.Fatal("only the configuration read needs the verified owner")
	}
	if props, required := diagSchema(ToolHealthDeepRead); len(props) != 0 || required == nil {
		t.Fatalf("health schema = %v %v", props, required)
	}
}

// TestEncodeResultLeavesDiagResultsAlone: a full-size diagnostic text result
// and a prompt page pass the outbound encoder unmasked and untruncated.
func TestEncodeResultLeavesDiagResultsAlone(t *testing.T) {
	entries := make([]any, 60)
	for i := range entries {
		entries[i] = map[string]any{"ts": "2026-03-01T12:00:00Z", "agent": "scanner", "trigger": "kick", "prompt": strings.Repeat("p", 2000)}
	}
	for _, data := range []map[string]any{
		{"type": "diag_text", "content_type": "text/plain; charset=utf-8", "text": strings.Repeat("line of output\n", DiagMaxTextBytes/15), "truncated": true, "bytes_total": 900000, "redactions": []any{}},
		{"type": "prompt_history", "entries": entries, "entry_count": 60, "entries_total": 200, "truncated": true},
	} {
		b, err := EncodeResult(data)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		if strings.Contains(text, "[masked:") || strings.Contains(text, "_admin_mcp") || strings.Contains(text, "text_preview") {
			t.Fatalf("%s altered by the outbound encoder: %.300s", data["type"], text)
		}
	}
}
