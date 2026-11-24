package adminmcp

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	ToolIssuesByBand = "issues_by_band"
	ToolPrsByBand    = "prs_by_band"

	RefusalKindInvalidArgument = "invalid_argument"
)

// issueBandKeys and prBandKeys mirror pkg/dashboard's issueBandOrder and
// prBandOrder (the Overview donut classifier #9102 ported to Go). adminmcp
// cannot import pkg/dashboard — dashboard imports adminmcp to serve the admin
// MCP endpoint — so the band-read tools carry their own copy of the valid
// keys, in the fixed order the Overview endpoints already return bands[] in.
//
// The issues export has one band beyond issueBandOrder: "outside"
// (overviewBandOutside), the open issues the scanner kept off the agent
// queues. It comes back in bands[] with a count like the others, so it is a
// band a caller can ask for.
var (
	issueBandKeys = []string{"ready", "in-progress", "agent-filed", "waiting", "done", "outside"}
	prBandKeys    = []string{"waiting", "eligible", "blocked", "in-review", "open", "draft"}
)

// issueCardBandKeys, prCardBandKeys and cardGroupKeys mirror pkg/dashboard's
// Overview card: the issue and PR donuts' band keys in plot order
// (cardIssueBandOrder, cardPRBandOrder) and the six group keys in tile order
// (cardGroupOrder). Every export row carries its card_band and card_group.
// The Your turn group's bands say what the person has to do (inspect,
// decide, ...); card_group "your-turn" returns all of them.
var (
	issueCardBandKeys = []string{"inspect", "decide", "review", "triage", "admit", "your-turn-other", "to-close", "unexplained", "covered-by-pr", "waiting-on-item", "human-hold", "reporter-confirm", "queued", "working", "held", "not-work"}
	prCardBandKeys    = []string{"inspect", "decide", "approve", "merge-by-hand", "review", "triage", "admit", "your-turn-other", "to-close", "unexplained", "waiting-on-item", "human-hold", "author-update", "bot-rebase", "working", "will-merge", "held", "draft"}
	cardGroupKeys     = []string{"your-turn", "waiting-elsewhere", "held", "blocked", "actionable-now", "outside"}
)

// cardBandYourTurnAlias is the single Your turn band of before the split. A
// card_band filter still accepts it and returns the rows it held: every row
// in the your-turn group but to-close and unexplained (cardTurnAction in
// pkg/dashboard). card_group=your-turn returns the whole group.
const cardBandYourTurnAlias = "your-turn"

// cardTurnActionRow reports a row the your-turn card_band alias returns.
func cardTurnActionRow(m map[string]any) bool {
	return m["card_group"] == "your-turn" && m["card_band"] != "to-close" && m["card_band"] != "unexplained"
}

// validCardBandsFor returns the card band keys a band-read tool accepts:
// the donut's keys, then the your-turn alias.
func validCardBandsFor(tool string) []string {
	keys := issueCardBandKeys
	if tool == ToolPrsByBand {
		keys = prCardBandKeys
	}
	return append(append([]string{}, keys...), cardBandYourTurnAlias)
}

// validBandsFor returns the band keys a band-read tool accepts, or false if
// tool is not one of the band-read tools.
func validBandsFor(tool string) ([]string, bool) {
	switch tool {
	case ToolIssuesByBand:
		return issueBandKeys, true
	case ToolPrsByBand:
		return prBandKeys, true
	default:
		return nil, false
	}
}

// BandReadResult filters and caps a decoded GET /api/overview/{issues,prs}.json
// response for the issues_by_band / prs_by_band tools. The caller must fetch the
// endpoint without a `band` or `held` query parameter, so bands[] always
// carries every band's real count and rule sentence — the tool's own `band`
// and `held` arguments filter `rows` here instead, before the limit cap, so
// an agent asking "what's ready to close?" always gets the full donut shape,
// not just the slice it filtered to, and a held row past the cap is not
// silently dropped before the `held` filter ever sees it (#10018). `offset`
// pages the filtered rows the same way review_queue does — skip offset rows,
// then cap to limit, disclosing next_offset when more remain — so a band
// with more than MaxResultLimit rows is fully readable a page at a time
// instead of only ever showing the first limit rows (#10018).
func BandReadResult(tool string, data any, args map[string]any) (any, error) {
	validBands, ok := validBandsFor(tool)
	if !ok {
		return nil, fmt.Errorf("%s is not a band-read tool", tool)
	}
	band := strings.TrimSpace(stringArg(args, "band"))
	if band != "" && !containsBand(validBands, band) {
		return RefusalData{
			Type:      "refusal",
			Operation: tool,
			Kind:      RefusalKindInvalidArgument,
			Reason:    fmt.Sprintf("unknown band %q; valid bands: %s", band, strings.Join(validBands, ", ")),
		}, nil
	}
	cardBand := strings.TrimSpace(stringArg(args, "card_band"))
	if valid := validCardBandsFor(tool); cardBand != "" && !containsBand(valid, cardBand) {
		return RefusalData{
			Type:      "refusal",
			Operation: tool,
			Kind:      RefusalKindInvalidArgument,
			Reason:    fmt.Sprintf("unknown card_band %q; valid card bands: %s", cardBand, strings.Join(valid, ", ")),
		}, nil
	}
	cardGroup := strings.TrimSpace(stringArg(args, "card_group"))
	if cardGroup != "" && !containsBand(cardGroupKeys, cardGroup) {
		return RefusalData{
			Type:      "refusal",
			Operation: tool,
			Kind:      RefusalKindInvalidArgument,
			Reason:    fmt.Sprintf("unknown card_group %q; valid card groups: %s", cardGroup, strings.Join(cardGroupKeys, ", ")),
		}, nil
	}
	offset, err := OffsetFromArgs(args)
	if err != nil {
		return RefusalData{Type: "refusal", Operation: tool, Kind: RefusalKindInvalidArgument, Reason: err.Error()}, nil
	}
	held, filterHeld := args["held"].(bool)
	body, ok := rekeyBandRows(data).(map[string]any)
	if !ok {
		return CapResult(data, LimitFromArgs(args)), nil
	}
	rows, _ := body["rows"].([]any)
	if band != "" {
		filtered := make([]any, 0, len(rows))
		for _, row := range rows {
			if m, ok := row.(map[string]any); ok && m["band"] == band {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	// card_band and card_group filter the same way, before paging, so
	// card_bands[] and card_groups[] keep the full shape.
	if cardBand == cardBandYourTurnAlias {
		filtered := make([]any, 0, len(rows))
		for _, row := range rows {
			if m, ok := row.(map[string]any); ok && cardTurnActionRow(m) {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	} else {
		rows = filterRowsByField(rows, "card_band", cardBand)
	}
	rows = filterRowsByField(rows, "card_group", cardGroup)
	if cardBand != "" || cardGroup != "" {
		// The export lists every copy of an item listed twice; the card
		// counts the first, so a card filter returns the first only.
		rows = firstCopyRows(rows)
	}
	// held is applied here, before the limit cap below, not forwarded as an
	// /api/overview query parameter: the endpoint computes bands[] counts
	// over whatever rows it returns, so forwarding held would make bands[]
	// report only held (or only unheld) counts instead of the full donut
	// shape BandReadPath's doc comment promises. Filtering client-side here,
	// like band above, keeps that promise while still letting an agent find
	// held rows beyond the cap without widening `repo` and guessing (#10018).
	if filterHeld {
		filtered := make([]any, 0, len(rows))
		for _, row := range rows {
			if m, ok := row.(map[string]any); ok {
				if v, ok := m["held"].(bool); ok && v == held {
					filtered = append(filtered, row)
				}
			}
		}
		rows = filtered
	}
	limit := LimitFromArgs(args)
	total := len(rows)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	out := make(map[string]any, len(body)+2)
	for k, v := range body {
		out[k] = v
	}
	out["rows"] = rows[start:end]
	out["offset"] = offset
	if end < total {
		out["next_offset"] = end
		out["rows_truncated"] = true
		out["_admin_mcp"] = map[string]any{
			"truncated":        true,
			"limit":            limit,
			"total":            total,
			"truncated_fields": []string{"rows"},
		}
	}
	return out, nil
}

// BandReadPath builds the GET /api/overview/{issues,prs}.json request for the
// issues_by_band / prs_by_band tools. It deliberately never forwards `band`,
// `held`, `offset` or `limit`: the endpoint would compute bands[] counts and
// rows over only the filtered subset, so BandReadResult fetches every band
// and row and applies the tool's own band filter, held filter, offset and
// cap afterwards instead. Both the dashboard and stdio admin MCP servers
// share this so the path can't drift between them again (#10014, following
// #9160).
func BandReadPath(tool string, args map[string]any) string {
	kind := "issues"
	if tool == ToolPrsByBand {
		kind = "prs"
	}
	values := url.Values{}
	if repo := strings.TrimSpace(stringArg(args, "repo")); repo != "" {
		values.Set("repo", repo)
	}
	if stale, ok := args["stale"].(bool); ok {
		values.Set("stale", strconv.FormatBool(stale))
	}
	path := "/api/overview/" + kind + ".json"
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path
}

// rekeyBandRows rewrites each row's "band" field from the Overview display
// label (e.g. "Confirm & close") that the Overview row builders set it to, back
// to the band key (e.g. "done") the response's own bands[] carries alongside
// that label. Without this the band argument — documented and validated
// against the band keys — never matches a row, so band=<key> always returns
// zero rows (#10018). It runs here rather than in one provider so the
// dashboard and stdio servers cannot drift apart again. Rows already carrying
// a key are left alone. data is the caller's freshly decoded copy, so it is
// edited in place.
func rekeyBandRows(data any) any {
	body, ok := data.(map[string]any)
	if !ok {
		return data
	}
	bands, ok := body["bands"].([]any)
	if !ok {
		return data
	}
	labelToKey := make(map[string]string, len(bands))
	for _, raw := range bands {
		spec, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		key, _ := spec["key"].(string)
		label, _ := spec["label"].(string)
		if key != "" && label != "" {
			labelToKey[label] = key
		}
	}
	rows, ok := body["rows"].([]any)
	if !ok {
		return data
	}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if label, ok := row["band"].(string); ok {
			if key, ok := labelToKey[label]; ok {
				row["band"] = key
			}
		}
	}
	return data
}

// filterRowsByField keeps the rows whose field equals want; "" keeps all.
func filterRowsByField(rows []any, field, want string) []any {
	if want == "" {
		return rows
	}
	filtered := make([]any, 0, len(rows))
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok && m[field] == want {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

// firstCopyRows keeps the first row of each repo#number.
func firstCopyRows(rows []any) []any {
	seen := map[string]bool{}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			out = append(out, row)
			continue
		}
		key := strings.ToLower(fmt.Sprint(m["repo"])) + "#" + fmt.Sprint(m["number"])
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, row)
	}
	return out
}

func containsBand(bands []string, band string) bool {
	for _, b := range bands {
		if b == band {
			return true
		}
	}
	return false
}
