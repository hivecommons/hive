package adminmcp

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMergedPRsReadIsListedReadOnly(t *testing.T) {
	found := false
	for _, tool := range Tools() {
		if name, _ := tool["name"].(string); name != ToolMergedPRsRead {
			continue
		}
		found = true
		if !AllowedTool(ToolMergedPRsRead) {
			t.Fatal("merged_prs_read is listed but not allowed")
		}
		if annotations, _ := tool["annotations"].(map[string]any); annotations["readOnlyHint"] != true {
			t.Fatalf("annotations = %#v, want read-only", annotations)
		}
		if meta, _ := tool["metadata"].(ToolMetadata); meta.Writes {
			t.Fatal("metadata says merged_prs_read writes")
		}
		props, _ := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		for _, arg := range []string{"since", "repo", "merged_by", "lane", "author", "exclude_author", "approved", "limit", "offset"} {
			if _, ok := props[arg]; !ok {
				t.Errorf("schema has no %q", arg)
			}
		}
		desc, _ := tool["description"].(string)
		if !strings.Contains(desc, "90 days") || !strings.Contains(desc, "approvals_known") {
			t.Errorf("description does not state the window or the approval caveat: %s", desc)
		}
	}
	if !found {
		t.Fatal("merged_prs_read is not listed")
	}
	if path, ok := ReadPath(ToolMergedPRsRead, 2); !ok || path != "/api/merged-prs" {
		t.Fatalf("default path = %q, %v", path, ok)
	}
}

func TestMergedPRsReadPath(t *testing.T) {
	now := time.Date(2026, 11, 18, 21, 0, 0, 0, time.UTC)
	path, err := mergedPRsReadPathAt(nil, now)
	if err != nil || path != "/api/merged-prs" {
		t.Fatalf("no args = %q, %v", path, err)
	}
	path, err = mergedPRsReadPathAt(map[string]any{
		"since": "24h", "merged_by": "hive", "approved": "none", "exclude_author": "operator", "repo": " acme/console ", "limit": float64(10), "offset": float64(20),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(path)
	q := u.Query()
	if u.Path != "/api/merged-prs" || q.Get("since") != "24h" || q.Get("merged_by") != "hive" || q.Get("approved") != "none" ||
		q.Get("exclude_author") != "operator" || q.Get("repo") != "acme/console" || q.Get("limit") != "10" || q.Get("offset") != "20" {
		t.Fatalf("path = %s", path)
	}
	for _, args := range []map[string]any{
		{"since": "91d"},
		{"since": "soon"},
		{"since": now.Add(time.Hour).Format(time.RFC3339)},
		{"approved": "maybe"},
		{"limit": float64(0)},
		{"limit": float64(501)},
		{"offset": float64(-1)},
		{"repo": 7},
	} {
		if _, err := mergedPRsReadPathAt(args, now); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if _, err := mergedPRsReadPathAt(map[string]any{"since": "2026-11-18T00:00:00Z"}, now); err != nil {
		t.Errorf("RFC3339 since refused: %v", err)
	}
}

func TestStageTrackerRecentDescribesItsWindow(t *testing.T) {
	recent, _ := stageTrackerReadSchema()["recent"].(map[string]any)
	if desc, _ := recent["description"].(string); !strings.Contains(desc, "last 30 minutes") || !strings.Contains(desc, ToolMergedPRsRead) {
		t.Fatalf("recent description = %q", desc)
	}
}
