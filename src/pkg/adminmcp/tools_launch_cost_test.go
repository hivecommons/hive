package adminmcp

import (
	"fmt"
	"strings"
	"testing"
)

// launch_cost_read is listed read-only, takes no write role, and describes
// ref, limit and offset.
func TestLaunchCostReadIsListedReadOnly(t *testing.T) {
	found := false
	for _, tool := range Tools() {
		if tool["name"] != ToolLaunchCostRead {
			continue
		}
		found = true
		if !AllowedTool(ToolLaunchCostRead) {
			t.Error("launch_cost_read is listed but not allowed")
		}
		annotations, _ := tool["annotations"].(map[string]any)
		if annotations["readOnlyHint"] != true {
			t.Errorf("annotations = %#v, want read-only", annotations)
		}
		if meta, _ := tool["metadata"].(ToolMetadata); meta.Writes {
			t.Error("metadata says it writes")
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for _, arg := range []string{"ref", "limit", "offset"} {
			if _, ok := props[arg]; !ok {
				t.Errorf("schema has no %q: %#v", arg, props)
			}
		}
	}
	if !found {
		t.Fatal("launch_cost_read is not listed")
	}
}

// The tool asks the endpoint for exactly the rows up to the page it returns,
// and passes ref through encoded.
func TestLaunchCostReadPath(t *testing.T) {
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{nil, "/api/launch-cost?max_points=20"},
		{map[string]any{"limit": float64(5)}, "/api/launch-cost?max_points=5"},
		{map[string]any{"limit": float64(10), "offset": float64(30)}, "/api/launch-cost?max_points=40"},
		{map[string]any{"ref": " projectbluefin/utah-packages#801 "}, "/api/launch-cost?max_points=20&ref=projectbluefin%2Futah-packages%23801"},
		{map[string]any{"ref": ""}, "/api/launch-cost?max_points=20"},
	} {
		got, err := LaunchCostReadPath(tc.args)
		if err != nil || got != tc.want {
			t.Errorf("%v path = %q, %v; want %q", tc.args, got, err, tc.want)
		}
	}
	if got, ok := ReadPath(ToolLaunchCostRead, 5); !ok || got != "/api/launch-cost?max_points=20" {
		t.Errorf("ReadPath = %q, %v", got, ok)
	}
}

// A bad offset or ref is refused with its reason before any request.
func TestLaunchCostReadRefusesBadArguments(t *testing.T) {
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"offset": float64(-1)}, "offset must be a non-negative integer"},
		{map[string]any{"offset": "page two"}, "offset must be a non-negative integer"},
		{map[string]any{"offset": float64(9990), "limit": float64(20)}, "at most 10000"},
		{map[string]any{"ref": true}, "ref must be a work item key"},
		{map[string]any{"ref": "owner/repo#1 &x=1"}, "ref must be a work item key"},
		{map[string]any{"ref": strings.Repeat("a", 201)}, "ref must be a work item key"},
	} {
		path, err := LaunchCostReadPath(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v = %q, %v; want a refusal containing %q", tc.args, path, err, tc.want)
		}
	}
}

func launchCostAnswer(refs, launches int) map[string]any {
	byRef := []any{}
	for i := 0; i < refs; i++ {
		byRef = append(byRef, map[string]any{"ref": fmt.Sprintf("o/r#%d", i+1), "kind": "pr", "launches": float64(1)})
	}
	byLaunch := []any{}
	for i := 0; i < launches; i++ {
		byLaunch = append(byLaunch, map[string]any{"agent": "reviewer", "launched_at": fmt.Sprintf("2026-11-17T%02d:00:00Z", 23-i)})
	}
	return map[string]any{
		"ready": true, "total_usd": 12.5, "by_ref": byRef, "by_launch": byLaunch,
		"by_ref_total": float64(9), "by_launch_total": float64(launches), "truncated": true,
		"unattributed": map[string]any{"usd": 1.0}, "backend_unsupported": map[string]any{"usd": nil},
		"disclaimer": "Not a bill.", "limitations": []any{"Attribution only."},
	}
}

// Without ref, by_ref is paged and by_launch is left out with a note; the
// endpoint's own truncated flag (its max_points cut, which the tool chose)
// is replaced by the tool's disclosure; totals and buckets are untouched.
func TestLaunchCostResultPagesByRef(t *testing.T) {
	body := launchCostAnswer(6, 4) // the endpoint answered max_points=6 of 9
	got, err := LaunchCostResult(body, map[string]any{"limit": float64(3), "offset": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	out := got.(map[string]any)
	refs := out["by_ref"].([]any)
	if len(refs) != 3 || refs[0].(map[string]any)["ref"] != "o/r#4" || out["by_ref_total"] != 9 || out["next_offset"] != 6 {
		t.Fatalf("page = %#v", out)
	}
	if _, ok := out["by_launch"]; ok {
		t.Fatal("by_launch returned without ref")
	}
	if _, ok := out["truncated"]; ok {
		t.Fatal("the endpoint's truncated flag survived")
	}
	meta, _ := out["_admin_mcp"].(map[string]any)
	note := fmt.Sprint(meta["note"])
	if meta["truncated"] != true || !strings.Contains(note, "rows 4-6 of 9") || !strings.Contains(note, "pass ref") {
		t.Fatalf("disclosure = %#v", meta)
	}
	if out["total_usd"] != 12.5 || out["unattributed"] == nil || out["backend_unsupported"] == nil || out["disclaimer"] != "Not a bill." {
		t.Fatalf("totals or buckets changed: %#v", out)
	}
	if len(body["by_ref"].([]any)) != 6 || len(body["by_launch"].([]any)) != 4 {
		t.Fatal("paging modified the endpoint's answer")
	}

	past, err := LaunchCostResult(launchCostAnswer(0, 0), map[string]any{"offset": float64(50)})
	if err != nil {
		t.Fatal(err)
	}
	pastMeta, _ := past.(map[string]any)["_admin_mcp"].(map[string]any)
	if !strings.Contains(fmt.Sprint(pastMeta["note"]), "past the last") {
		t.Fatalf("page past the end = %#v", past)
	}
}

// With ref, by_launch is that item's launches, newest first, capped to limit.
func TestLaunchCostResultWithRefKeepsLaunches(t *testing.T) {
	body := launchCostAnswer(1, 5)
	body["by_ref_total"] = float64(1)
	got, err := LaunchCostResult(body, map[string]any{"ref": "o/r#1", "limit": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	out := got.(map[string]any)
	launches, _ := out["by_launch"].([]any)
	if len(launches) != 2 || launches[0].(map[string]any)["launched_at"] != "2026-11-17T23:00:00Z" {
		t.Fatalf("by_launch = %#v", out["by_launch"])
	}
	meta, _ := out["_admin_mcp"].(map[string]any)
	if !strings.Contains(fmt.Sprint(meta["note"]), "newest 2 of 5") {
		t.Fatalf("disclosure = %#v", meta)
	}
	if _, ok := out["next_offset"]; ok {
		t.Fatal("next_offset with a single ref row")
	}
	if _, err := LaunchCostResult([]any{}, nil); err == nil {
		t.Fatal("a non-object answer was accepted")
	}
}
