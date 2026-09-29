package adminmcp

import (
	"strings"
	"testing"
)

func sampleOverviewIssuesResponse() map[string]any {
	return map[string]any{
		"kind":       "issues",
		"stale_days": 14,
		"bands": []any{
			map[string]any{"key": "ready", "label": "Unclaimed", "rule": "no other band matched", "count": float64(1)},
			map[string]any{"key": "done", "label": "Confirm & close", "rule": "an agent applied hive/already-done, hive/covered-by-pr or hive/likely-done — verify the work landed and close the issue", "count": float64(3)},
		},
		"rows": []any{
			map[string]any{"number": float64(1), "title": "ready one", "band": "ready"},
			map[string]any{"number": float64(2), "title": "done one", "band": "done"},
			map[string]any{"number": float64(3), "title": "done two", "band": "done"},
			map[string]any{"number": float64(4), "title": "done three", "band": "done"},
		},
	}
}

func TestBandReadResultFiltersRowsButKeepsFullBands(t *testing.T) {
	out, err := BandReadResult(ToolIssuesByBand, sampleOverviewIssuesResponse(), map[string]any{"band": "done"})
	if err != nil {
		t.Fatal(err)
	}
	body, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("result = %#v", out)
	}
	bands, ok := body["bands"].([]any)
	if !ok || len(bands) != 2 {
		t.Fatalf("bands = %#v, want both bands returned even when filtered", body["bands"])
	}
	rows, ok := body["rows"].([]any)
	if !ok || len(rows) != 3 {
		t.Fatalf("rows = %#v, want only the 3 done-band rows", body["rows"])
	}
	for _, row := range rows {
		m := row.(map[string]any)
		if m["band"] != "done" {
			t.Fatalf("row leaked a non-done band: %#v", m)
		}
	}
}

func TestBandReadResultRefusesUnknownBand(t *testing.T) {
	out, err := BandReadResult(ToolIssuesByBand, sampleOverviewIssuesResponse(), map[string]any{"band": "nope"})
	if err != nil {
		t.Fatal(err)
	}
	refusal, ok := out.(RefusalData)
	if !ok || refusal.Type != "refusal" || refusal.Kind != RefusalKindInvalidArgument {
		t.Fatalf("result = %#v", out)
	}
	for _, key := range issueBandKeys {
		if !strings.Contains(refusal.Reason, key) {
			t.Fatalf("refusal reason %q missing valid key %q", refusal.Reason, key)
		}
	}
}

func TestBandReadResultDisclosesTruncationAndTotal(t *testing.T) {
	out, err := BandReadResult(ToolIssuesByBand, sampleOverviewIssuesResponse(), map[string]any{"band": "done", "limit": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	body := out.(map[string]any)
	rows, ok := body["rows"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("rows = %#v, want capped to 2", body["rows"])
	}
	if body["rows_truncated"] != true {
		t.Fatalf("rows_truncated missing: %#v", body)
	}
	meta, ok := body["_admin_mcp"].(map[string]any)
	if !ok || meta["total"] != 3 || meta["limit"] != 2 || meta["truncated"] != true {
		t.Fatalf("_admin_mcp = %#v", body["_admin_mcp"])
	}
}

func TestBandReadResultWithoutBandReturnsEveryRow(t *testing.T) {
	out, err := BandReadResult(ToolPrsByBand, map[string]any{"rows": []any{
		map[string]any{"number": float64(1), "band": "open"},
		map[string]any{"number": float64(2), "band": "blocked"},
	}}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	body := out.(map[string]any)
	rows, ok := body["rows"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("rows = %#v, want both rows kept when band is unset", body["rows"])
	}
}

func TestIssuesByBandAndPrsByBandToolsAreListedAsReadOnly(t *testing.T) {
	for _, tc := range []struct {
		tool string
		keys []string
	}{
		{ToolIssuesByBand, issueBandKeys},
		{ToolPrsByBand, prBandKeys},
	} {
		if !AllowedTool(tc.tool) {
			t.Fatalf("%s is not allowed", tc.tool)
		}
		var found bool
		for _, def := range Tools() {
			if def["name"] != tc.tool {
				continue
			}
			found = true
			annotations, _ := def["annotations"].(map[string]any)
			if annotations["readOnlyHint"] != true {
				t.Fatalf("%s annotations = %#v", tc.tool, annotations)
			}
			schema, _ := def["inputSchema"].(map[string]any)
			props, _ := schema["properties"].(map[string]any)
			bandProp, _ := props["band"].(map[string]any)
			enum, _ := bandProp["enum"].([]string)
			if len(enum) != len(tc.keys) {
				t.Fatalf("%s band enum = %#v, want %#v", tc.tool, enum, tc.keys)
			}
			if _, ok := props["repo"]; !ok {
				t.Fatalf("%s schema missing repo property: %#v", tc.tool, props)
			}
		}
		if !found {
			t.Fatalf("%s tool not listed", tc.tool)
		}
	}
}
