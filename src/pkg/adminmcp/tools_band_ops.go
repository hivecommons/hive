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
var (
	issueBandKeys = []string{"ready", "in-progress", "agent-filed", "waiting", "done"}
	prBandKeys    = []string{"waiting", "eligible", "blocked", "in-review", "open", "draft"}
)

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
// endpoint without a `band` query parameter, so bands[] always carries every
// band's real count and rule sentence — the tool's own `band` argument filters
// `rows` here instead, after capping, so an agent asking "what's ready to
// close?" always gets the full donut shape, not just the slice it filtered to.
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
	body, ok := data.(map[string]any)
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
	limit := LimitFromArgs(args)
	total := len(rows)
	out := make(map[string]any, len(body)+1)
	for k, v := range body {
		out[k] = v
	}
	if total > limit {
		out["rows"] = rows[:limit]
		out["rows_truncated"] = true
		out["_admin_mcp"] = map[string]any{
			"truncated":        true,
			"limit":            limit,
			"total":            total,
			"truncated_fields": []string{"rows"},
		}
	} else {
		out["rows"] = rows
	}
	return out, nil
}

// BandReadPath builds the GET /api/overview/{issues,prs}.json request for the
// issues_by_band / prs_by_band tools. It deliberately never forwards `band` or
// `limit`: the endpoint would compute bands[] counts and rows over only the
// band-filtered subset, so BandReadResult fetches every band and applies the
// tool's own band filter and cap afterwards instead. Both the dashboard and
// stdio admin MCP servers share this so the path can't drift between them
// again (#10014, following #9160).
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

func containsBand(bands []string, band string) bool {
	for _, b := range bands {
		if b == band {
			return true
		}
	}
	return false
}
