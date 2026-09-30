package adminmcp

import (
	"net/url"
	"strings"
	"testing"
)

func TestAdvisorRecordsReadPath(t *testing.T) {
	path := AdvisorRecordsReadPath(map[string]any{
		"agent": " team/scout ",
		"since": "2026-01-01T00:00:00Z",
		"until": "2026-02-01T00:00:00Z",
		"limit": float64(500),
	})
	u, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/api/advisor/records" {
		t.Fatalf("path = %q", path)
	}
	q := u.Query()
	if q.Get("agent") != "team/scout" || q.Get("since") != "2026-01-01T00:00:00Z" || q.Get("until") != "2026-02-01T00:00:00Z" {
		t.Errorf("window/agent not forwarded: %q", path)
	}
	// LimitFromArgs treats out-of-range limits as unset (DefaultResultLimit),
	// the same refusal every other admin MCP read tool applies.
	if q.Get("limit") != "20" {
		t.Errorf("out-of-range limit must fall back to DefaultResultLimit: %q", path)
	}

	fleet := AdvisorRecordsReadPath(map[string]any{"hours": float64(24)})
	if strings.Contains(fleet, "agent=") || !strings.Contains(fleet, "hours=24") || !strings.Contains(fleet, "limit=20") {
		t.Errorf("fleet lookback path = %q", fleet)
	}
}

// TestAdvisorRecordsToolIsReadOnly pins the speak-only contract: the advisor
// has a read tool and no write operation of any kind.
func TestAdvisorRecordsToolIsReadOnly(t *testing.T) {
	var found bool
	for _, tool := range ToolsWithWritesEnabled(true) {
		name, _ := tool["name"].(string)
		if name != ToolAdvisorRecords && strings.Contains(name, "advisor") && name != ToolHiveAdvisor {
			t.Errorf("unexpected advisor tool %q", name)
		}
		if name != ToolAdvisorRecords {
			continue
		}
		found = true
		annotations, _ := tool["annotations"].(map[string]any)
		if annotations["readOnlyHint"] != true {
			t.Fatalf("annotations = %#v", annotations)
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for _, key := range []string{"agent", "since", "until", "hours", "limit"} {
			if _, ok := props[key]; !ok {
				t.Errorf("schema missing %q: %#v", key, props)
			}
		}
	}
	if !found {
		t.Fatal("advisor_records tool not listed")
	}
	for name := range DefaultWriteRegistry().ops {
		if strings.Contains(name, "advisor") {
			t.Errorf("advisor must stay speak-only; found write op %q", name)
		}
	}
}
