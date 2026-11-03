package adminmcp

import (
	"strings"
	"testing"
)

func TestSettingsReadPath(t *testing.T) {
	for _, tc := range []struct {
		args    map[string]any
		want    string
		wantErr string
	}{
		{nil, "/api/config/governor", ""},
		{map[string]any{"section": ""}, "/api/config/governor", ""},
		{map[string]any{"section": "auto_merge"}, "/api/config/auto-merge", ""},
		{map[string]any{"section": "channels"}, "/api/config/channels", ""},
		{map[string]any{"section": "upstream_watch"}, "/api/upstream-watch", ""},
		{map[string]any{"section": "agent", "agent": "scanner"}, "/api/config/agent/scanner", ""},
		{map[string]any{"section": "agent_prompt", "agent": "a b/../x"}, "/api/config/agent/a%20b%2F..%2Fx/prompt", ""},
		{map[string]any{"section": "agent"}, "", "needs agent"},
		{map[string]any{"section": "/api/config/download"}, "", "unknown settings_read section"},
		{map[string]any{"section": "export"}, "", "unknown settings_read section"},
	} {
		got, err := SettingsReadPath(tc.args)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("SettingsReadPath(%v) error = %v, want %q", tc.args, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("SettingsReadPath(%v) = %q, %v; want %q", tc.args, got, err, tc.want)
		}
	}
}

// The tool schema offers exactly the sections, and every section has a
// distinct name and a description.
func TestSettingsReadSchemaListsSections(t *testing.T) {
	enum, _ := inputSchema(ToolSettingsRead)["properties"].(map[string]any)["section"].(map[string]any)["enum"].([]string)
	seen := map[string]bool{}
	for _, s := range SettingsSections() {
		if seen[s.Name] || s.Desc == "" || !strings.HasPrefix(s.Path, "/api/") {
			t.Errorf("bad section %+v", s)
		}
		seen[s.Name] = true
	}
	if len(enum) != len(seen) {
		t.Fatalf("schema enum has %d sections, table has %d", len(enum), len(seen))
	}
	for _, name := range enum {
		if !seen[name] {
			t.Errorf("schema offers unknown section %q", name)
		}
	}
	if !strings.Contains(settingsReadDescription(), "agent_prompt") {
		t.Fatal("description does not list the sections")
	}
}
