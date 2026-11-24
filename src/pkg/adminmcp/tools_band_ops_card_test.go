package adminmcp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func sampleOverviewCardResponse() map[string]any {
	body := sampleOverviewIssuesResponse()
	body["card_bands"] = []any{
		map[string]any{"key": "decide", "count": float64(1)},
		map[string]any{"key": "review", "count": float64(1)},
		map[string]any{"key": "queued", "count": float64(2)},
	}
	body["card_groups"] = []any{
		map[string]any{"key": "your-turn", "count": float64(2)},
		map[string]any{"key": "actionable-now", "count": float64(2)},
	}
	rows := body["rows"].([]any)
	for i, raw := range rows {
		m := raw.(map[string]any)
		switch i {
		case 0:
			m["card_band"], m["card_group"] = "decide", "your-turn"
		case 2:
			m["card_band"], m["card_group"] = "review", "your-turn"
		default:
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
		if bands, _ := body["card_bands"].([]any); len(bands) != 3 {
			t.Fatalf("%v: card_bands = %#v, want the full shape", args, body["card_bands"])
		}
		if groups, _ := body["card_groups"].([]any); len(groups) != 2 {
			t.Fatalf("%v: card_groups = %#v, want the full shape", args, body["card_groups"])
		}
	}
}

// The Your turn group's bands say what the person has to do; card_group
// your-turn still returns every one of them, and card_band your-turn, the
// single band of before the split, returns the rows it held: every
// your-turn row but to-close and unexplained. That includes the issue churn
// rows it held, which are check-finished rows now. The schema lists it.
func TestBandReadResultYourTurnGroupSpansItsBands(t *testing.T) {
	out, err := BandReadResult(ToolIssuesByBand, sampleOverviewCardResponse(), map[string]any{"card_group": "your-turn"})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := out.(map[string]any)["rows"].([]any)
	var bands []any
	for _, row := range rows {
		bands = append(bands, row.(map[string]any)["card_band"])
	}
	if len(bands) != 2 || bands[0] != "decide" || bands[1] != "review" {
		t.Fatalf("card_group=your-turn rows carry bands %v, want decide and review", bands)
	}
	for _, tool := range []string{ToolIssuesByBand, ToolPrsByBand} {
		body := sampleOverviewIssuesResponse()
		group := []string{"decide", "to-close", "unexplained", "queued"}
		for i, raw := range body["rows"].([]any) {
			m := raw.(map[string]any)
			m["card_band"], m["card_group"] = group[i], "your-turn"
			if group[i] == "queued" {
				m["card_group"] = "actionable-now"
			}
		}
		out, err := BandReadResult(tool, body, map[string]any{"card_band": "your-turn"})
		if err != nil {
			t.Fatal(err)
		}
		got, ok := out.(map[string]any)
		if !ok {
			t.Fatalf("%s card_band=your-turn: result = %#v", tool, out)
		}
		rows, _ := got["rows"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["card_band"] != "decide" {
			t.Fatalf("%s card_band=your-turn rows = %#v, want the decide row only", tool, rows)
		}
	}
	// An issue churn row is one the single band held: check-finished on a
	// current server, and to-close with the churn reason code on an older
	// one, which this MCP may read. A plain to-close row stays out.
	body := sampleOverviewIssuesResponse()
	for i, raw := range body["rows"].([]any) {
		m := raw.(map[string]any)
		m["card_band"], m["card_group"] = []string{"decide", "check-finished", "to-close", "to-close"}[i], "your-turn"
		m["card_reason_code"] = []string{"test", "verdict:issue_churn", "verdict:issue_churn", "ownership:verified_done"}[i]
	}
	out, err = BandReadResult(ToolIssuesByBand, body, map[string]any{"card_band": "your-turn"})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ = out.(map[string]any)["rows"].([]any)
	var codes []any
	for _, row := range rows {
		m := row.(map[string]any)
		codes = append(codes, m["card_band"], m["card_reason_code"])
	}
	if want := []any{"decide", "test", "check-finished", "verdict:issue_churn", "to-close", "verdict:issue_churn"}; !reflect.DeepEqual(codes, want) {
		t.Fatalf("issues card_band=your-turn rows = %v, want %v (the decide row and both churn rows, not the plain to-close row)", codes, want)
	}
	for _, tool := range Tools() {
		name, _ := tool["name"].(string)
		if name != ToolIssuesByBand && name != ToolPrsByBand {
			continue
		}
		props, _ := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		band, _ := props["card_band"].(map[string]any)
		enum, _ := band["enum"].([]string)
		if !containsBand(enum, "your-turn") {
			t.Errorf("%s card_band enum %v does not list your-turn", name, enum)
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

// An issue churn row's card_churn_prs (the issue export's list of the pull
// requests behind it) reaches issues_by_band unchanged, through the
// card_band filter and the limit cap, and survives the JSON round trip.
func TestBandReadResultKeepsIssueChurnPRs(t *testing.T) {
	const export = `{"kind":"issues","bands":[{"key":"ready","label":"Unclaimed","count":2}],
		"card_bands":[{"key":"check-finished","count":1},{"key":"queued","count":1}],
		"rows":[
			{"number":40,"repo":"octo/demo","band":"Unclaimed","card_band":"check-finished","card_group":"your-turn","card_reason_code":"verdict:issue_churn",
			 "card_reason":"2 PRs merged (#1236, #1240), 1 closed unmerged (#1251) and it is still open; close it if it is done, or add hive: churn-triaged to give it back to agents.",
			 "card_churn_prs":[
				{"repo":"octo/demo","number":1236,"state":"merged","title":"First slice","url":"https://github.com/octo/demo/pull/1236","merged_at":"2026-11-21T17:00:00Z"},
				{"repo":"octo/demo","number":1240,"state":"merged","url":"https://github.com/octo/demo/pull/1240"},
				{"repo":"octo/demo","number":1251,"state":"closed","url":"https://github.com/octo/demo/pull/1251"}]},
			{"number":41,"repo":"octo/demo","band":"Unclaimed","card_band":"queued","card_group":"actionable-now","card_churn_prs":[]}]}`
	var data any
	if err := json.Unmarshal([]byte(export), &data); err != nil {
		t.Fatal(err)
	}
	out, err := BandReadResult(ToolIssuesByBand, data, map[string]any{"card_band": "check-finished", "limit": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Rows []struct {
			Number       int    `json:"number"`
			CardReason   string `json:"card_reason"`
			CardChurnPRs []struct {
				Repo     string `json:"repo"`
				Number   int    `json:"number"`
				State    string `json:"state"`
				Title    string `json:"title"`
				URL      string `json:"url"`
				MergedAt string `json:"merged_at"`
			} `json:"card_churn_prs"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Rows) != 1 || body.Rows[0].Number != 40 {
		t.Fatalf("rows = %+v, want the churn row only", body.Rows)
	}
	prs := body.Rows[0].CardChurnPRs
	if len(prs) != 3 || prs[0].Number != 1236 || prs[0].State != "merged" || prs[0].Title != "First slice" ||
		prs[0].URL != "https://github.com/octo/demo/pull/1236" || prs[0].MergedAt != "2026-11-21T17:00:00Z" ||
		prs[2].Number != 1251 || prs[2].State != "closed" || prs[2].Title != "" {
		t.Errorf("card_churn_prs = %+v", prs)
	}
	if !strings.Contains(body.Rows[0].CardReason, "(#1236, #1240)") {
		t.Errorf("card_reason = %q", body.Rows[0].CardReason)
	}
	for _, tool := range Tools() {
		if name, _ := tool["name"].(string); name == ToolIssuesByBand {
			if desc, _ := tool["description"].(string); !strings.Contains(desc, "card_churn_prs") {
				t.Errorf("issues_by_band description does not name card_churn_prs: %q", desc)
			}
		}
	}
}
