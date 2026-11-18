package adminmcp

import (
	"net/url"
	"strings"
	"testing"
)

func TestAuditLogReadPathSendsFilters(t *testing.T) {
	path, err := AuditLogReadPath(map[string]any{})
	if err != nil || path != "/api/audit" {
		t.Fatalf("no filters: %q, %v", path, err)
	}
	path, err = AuditLogReadPath(map[string]any{"repo": "o/r", "number": float64(42), "action": "pr_*", "since": "24h", "contributor": "alice", "sensitive": true, "limit": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	raw, query, _ := strings.Cut(path, "?")
	q, _ := url.ParseQuery(query)
	if raw != "/api/audit" || q.Get("repo") != "o/r" || q.Get("number") != "42" || q.Get("action") != "pr_*" || q.Get("since") != "24h" || q.Get("contributor") != "alice" || q.Get("sensitive") != "1" || q.Has("limit") {
		t.Fatalf("path = %q", path)
	}
	for _, bad := range []map[string]any{{"number": float64(0)}, {"number": 1.5}, {"number": "abc"}, {"sensitive": "maybe"}, {"sensitive": float64(1)}} {
		if _, err := AuditLogReadPath(bad); err == nil {
			t.Errorf("%v: want a refusal", bad)
		}
	}
}

func TestItemHistoryReadPath(t *testing.T) {
	path, err := ItemHistoryReadPath(map[string]any{"repo": "hivecommons/hive", "number": "#19105"})
	if err != nil || path != "/api/audit/item?number=19105&repo=hivecommons%2Fhive" {
		t.Fatalf("path = %q, %v", path, err)
	}
	for _, bad := range []map[string]any{{"number": float64(1)}, {"repo": "o/r"}, {"repo": "o/r", "number": float64(-2)}} {
		if _, err := ItemHistoryReadPath(bad); err == nil {
			t.Errorf("%v: want a refusal", bad)
		}
	}
}

// audit_log and item_history_read are listed read-only with their
// arguments; item_history_read requires repo and number.
func TestAuditToolsAreListedReadOnly(t *testing.T) {
	want := map[string][]string{
		ToolAuditLog:        {"repo", "number", "action", "since", "contributor", "sensitive", "limit"},
		ToolItemHistoryRead: {"repo", "number"},
	}
	for _, tool := range Tools() {
		name, _ := tool["name"].(string)
		args, ok := want[name]
		if !ok {
			continue
		}
		delete(want, name)
		if !AllowedTool(name) {
			t.Errorf("%s is listed but not allowed", name)
		}
		if annotations, _ := tool["annotations"].(map[string]any); annotations["readOnlyHint"] != true {
			t.Errorf("%s is not read-only", name)
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for _, arg := range args {
			if _, ok := props[arg]; !ok {
				t.Errorf("%s schema has no %q", name, arg)
			}
		}
		if name == ToolItemHistoryRead {
			required, _ := schema["required"].([]string)
			if strings.Join(required, ",") != "repo,number" {
				t.Errorf("item_history_read required = %v", required)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("tools not listed: %v", want)
	}
}
