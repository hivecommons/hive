package adminmcp

import (
	"net/url"
	"strings"
	"testing"
)

func TestLogReadPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		want url.Values
	}{
		{"defaults", map[string]any{}, url.Values{}},
		{"all", map[string]any{
			"source":    " hive ",
			"lines":     float64(5000),
			"since":     "2026-03-01T07:00:00-05:00",
			"until":     "2026-03-01T13:00:00Z",
			"min_level": "WARNING",
			"include":   "Worker",
			"exclude":   []any{"noise", " ", "heartbeat"},
			"cursor":    "7b7d",
			"limit":     float64(3),
		}, url.Values{
			"source": {"hive"}, "lines": {"5000"}, "since": {"2026-03-01T07:00:00-05:00"}, "until": {"2026-03-01T13:00:00Z"},
			"min_level": {"warn"}, "include": {"Worker"}, "exclude": {"noise", "heartbeat"}, "cursor": {"7b7d"},
		}},
		{"string forms", map[string]any{"lines": "20", "exclude": "noise"}, url.Values{"lines": {"20"}, "exclude": {"noise"}}},
		{"typed forms", map[string]any{"lines": 7, "exclude": []string{"a", ""}}, url.Values{"lines": {"7"}, "exclude": {"a"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := LogReadPath(ToolLogRead, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(path)
			if err != nil {
				t.Fatal(err)
			}
			if u.Path != "/api/logs/read" || u.Query().Encode() != tc.want.Encode() {
				t.Fatalf("path = %q, want query %q", path, tc.want.Encode())
			}
		})
	}
	if path, err := LogReadPath(ToolLogSources, map[string]any{"lines": "x"}); err != nil || path != "/api/logs/sources" {
		t.Fatalf("sources path = %q, %v", path, err)
	}
}

func TestLogReadPathRefusals(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"since without offset": {"since": "2026-03-01T07:00:00"},
		"until not a time":     {"until": "noon"},
		"unknown level":        {"min_level": "fatal"},
		"fractional lines":     {"lines": 2.5},
		"word lines":           {"lines": "many"},
		"object lines":         {"lines": map[string]any{}},
		"exclude number":       {"exclude": float64(3)},
		"exclude mixed list":   {"exclude": []any{"a", float64(1)}},
	} {
		if _, err := LogReadPath(ToolLogRead, args); err == nil {
			t.Errorf("%s: accepted %#v", name, args)
		}
	}
	if _, err := LogReadPath(ToolHiveStatus, nil); err == nil {
		t.Error("a non-log tool built a log path")
	}
}

// TestLogToolsListedReadOnlyWithoutSharedLimit pins the tool contract: both log
// tools are listed read-only, avoid the reserved name fragments, and log_read
// takes its own lines count instead of the shared limit.
func TestLogToolsListedReadOnlyWithoutSharedLimit(t *testing.T) {
	found := map[string]bool{}
	for _, tool := range ToolsWithWritesEnabled(true) {
		name, _ := tool["name"].(string)
		if !LogTool(name) {
			continue
		}
		found[name] = true
		if !AllowedTool(name) || strings.Contains(name, "advisor") || strings.Contains(name, "select") {
			t.Errorf("%s: allowed=%v", name, AllowedTool(name))
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
		if name == ToolLogRead {
			for _, key := range []string{"source", "lines", "since", "until", "min_level", "include", "exclude", "cursor"} {
				if _, ok := props[key]; !ok {
					t.Errorf("log_read schema missing %q", key)
				}
			}
		}
	}
	if !found[ToolLogRead] || !found[ToolLogSources] {
		t.Fatalf("log tools listed = %v", found)
	}
}

// TestEncodeResultLeavesLogLinesAlone: a full log_read page passes the outbound
// encoder intact — no line cut to the list cap and no result field masked.
func TestEncodeResultLeavesLogLinesAlone(t *testing.T) {
	lines := make([]any, 400)
	for i := range lines {
		lines[i] = map[string]any{"source": "hive", "level": "info", "text": "worker heartbeat ok after repo scan"}
	}
	data := map[string]any{
		"type": "log_read", "untrusted_content": "UNTRUSTED", "lines": lines, "line_count": 400,
		"next_cursor": "7b7d", "redactions": []any{map[string]any{"kind": "hive-secret-value", "count": 2}},
		"pre_existing_marker_lines": 1, "unlevelled_dropped": 3,
	}
	b, err := EncodeResult(data)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if strings.Contains(text, "[masked:") || strings.Contains(text, "truncated") || strings.Count(text, `"source":"hive"`) != 400 {
		t.Fatalf("log page was altered by the outbound encoder: %.300s", text)
	}
}
