package adminmcp

import (
	"strings"
	"testing"
)

func sampleOverviewCardResponse() map[string]any {
	body := sampleOverviewIssuesResponse()
	body["card_bands"] = []any{
		map[string]any{"key": "your-turn", "count": float64(2)},
		map[string]any{"key": "queued", "count": float64(2)},
	}
	body["card_groups"] = []any{
		map[string]any{"key": "your-turn", "count": float64(2)},
		map[string]any{"key": "actionable-now", "count": float64(2)},
	}
	rows := body["rows"].([]any)
	for i, raw := range rows {
		m := raw.(map[string]any)
		if i%2 == 0 {
			m["card_band"], m["card_group"] = "your-turn", "your-turn"
		} else {
			m["card_band"], m["card_group"] = "queued", "actionable-now"
		}
	}
	return body
}

// card_band and card_group filter rows before the limit cap and paging,
// and leave card_bands[] and card_groups[] whole.
func TestBandReadResultFiltersByCardBandAndGroupBeforePaging(t *testing.T) {
	for _, args := range []map[string]any{
		{"card_band": "queued", "limit": float64(1)},
		{"card_group": "actionable-now", "limit": float64(1)},
	} {
		out, err := BandReadResult(ToolIssuesByBand, sampleOverviewCardResponse(), args)
		if err != nil {
			t.Fatal(err)
		}
		body, ok := out.(map[string]any)
		if !ok {
			t.Fatalf("%v: result = %#v", args, out)
		}
		rows, _ := body["rows"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["card_band"] != "queued" {
			t.Fatalf("%v: rows = %#v, want the first queued row", args, rows)
		}
		if body["next_offset"] != 1 {
			t.Fatalf("%v: next_offset = %#v, want 1 (two queued rows, limit 1)", args, body["next_offset"])
		}
		if bands, _ := body["card_bands"].([]any); len(bands) != 2 {
			t.Fatalf("%v: card_bands = %#v, want the full shape", args, body["card_bands"])
		}
		if groups, _ := body["card_groups"].([]any); len(groups) != 2 {
			t.Fatalf("%v: card_groups = %#v, want the full shape", args, body["card_groups"])
		}
	}
}

func TestBandReadResultRefusesUnknownCardKeys(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
		want []string
	}{
		{ToolIssuesByBand, map[string]any{"card_band": "bot-rebase"}, issueCardBandKeys},
		{ToolPrsByBand, map[string]any{"card_band": "queued"}, prCardBandKeys},
		{ToolPrsByBand, map[string]any{"card_group": "nope"}, cardGroupKeys},
	} {
		out, err := BandReadResult(tc.tool, sampleOverviewIssuesResponse(), tc.args)
		if err != nil {
			t.Fatal(err)
		}
		refusal, ok := out.(RefusalData)
		if !ok || refusal.Kind != RefusalKindInvalidArgument {
			t.Fatalf("%s %v: result = %#v", tc.tool, tc.args, out)
		}
		for _, key := range tc.want {
			if !strings.Contains(refusal.Reason, key) {
				t.Errorf("%s %v: refusal %q does not list %q", tc.tool, tc.args, refusal.Reason, key)
			}
		}
	}
}

// BandReadPath never forwards the card filters, so the endpoint always
// counts the full card shape.
func TestBandReadPathDoesNotForwardCardFilters(t *testing.T) {
	path := BandReadPath(ToolIssuesByBand, map[string]any{"card_band": "queued", "card_group": "your-turn", "repo": "o/r"})
	if strings.Contains(path, "card_") {
		t.Fatalf("path = %q forwards a card filter", path)
	}
}

// An item the export lists twice (the claim guard's copy, a stale draft on
// both PR lists) carries the first copy's card band on both rows; a card
// filter returns it once, as card_bands[] counts it.
func TestBandReadResultCardFilterReturnsADuplicateOnce(t *testing.T) {
	body := sampleOverviewCardResponse()
	rows := body["rows"].([]any)
	for _, raw := range rows {
		raw.(map[string]any)["repo"] = "o/r"
	}
	dup := map[string]any{"number": float64(2), "title": "done one, second copy", "band": "done", "repo": "o/r", "card_band": "queued", "card_group": "actionable-now"}
	body["rows"] = append(rows, dup)
	out, err := BandReadResult(ToolIssuesByBand, body, map[string]any{"card_band": "queued"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := out.(map[string]any)["rows"].([]any)
	if len(got) != 2 {
		t.Fatalf("rows = %#v, want #2 and #4 once each", got)
	}
	if all, _ := BandReadResult(ToolIssuesByBand, sampleOverviewCardResponse(), map[string]any{}); len(all.(map[string]any)["rows"].([]any)) != 4 {
		t.Fatal("without a card filter every row stays")
	}
}
