package adminmcp

import (
	"fmt"
	"net/url"
	"strings"
)

// launch_cost_read reads the Cost card's "cost by work item" table: the
// hive's estimated spend per issue and PR (GET /api/launch-cost,
// hivecommons/hive#12128), which the dashboard shows and the MCP could not.
// spend_read gives the totals by agent and model but never says which issue
// or PR the tokens went to.
//
// The endpoint answers up to max_points rows of by_ref, highest spend first,
// with by_ref_total for the whole window. The tool asks for exactly the rows
// up to the page it returns, so a page deep into a long list costs no more
// than the dashboard's own read.

const (
	ToolLaunchCostRead = "launch_cost_read"

	launchCostPath = "/api/launch-cost"

	// launchCostMaxPoints mirrors the endpoint's own max_points ceiling; the
	// tool refuses an offset that would ask for more.
	launchCostMaxPoints = 10000
	// launchCostMaxRefLen bounds the ref argument; work keys are
	// "owner/repo#123" or "repo!EXT-1".
	launchCostMaxRefLen = 200
)

const launchCostReadDescription = "Read the Cost card's \"cost by work item\" table through GET /api/launch-cost: the hive's estimated spend per issue and PR, from the work each recorded kick was launched for joined against per-message token timing. Inferred, not measured, and not a bill. Returns by_ref, highest spend first: one row per issue or PR with kind, repo, number, launches (how many kicks named it), agents, head_shas, first and last launch time, share_usd and share_tokens (each launch split evenly across the items it named) and launch_usd and launch_tokens (the undivided upper bound). Also returns by_ref_total, the totals (total_usd, attributed_tokens, total_tokens), the unattributed and backend_unsupported buckets (spend that could not be tied to any item), window_hours, ownership_hours, launches_retained, disclaimer and limitations. ref (\"owner/repo#123\") narrows the answer to one item and then also returns by_launch, that item's launches, newest first. limit caps the by_ref rows (and by_launch with ref) and offset skips by_ref rows; a capped answer says so in _admin_mcp and gives next_offset. Read-only."

// LaunchCostReadPath is the one GET launch_cost_read makes. A bad offset or
// ref is refused with its reason, before any request is made.
func LaunchCostReadPath(args map[string]any) (string, error) {
	offset, err := OffsetFromArgs(args)
	if err != nil {
		return "", err
	}
	points := offset + LimitFromArgs(args)
	if points > launchCostMaxPoints {
		return "", fmt.Errorf("offset must leave offset+limit at most %d, got offset %d", launchCostMaxPoints, offset)
	}
	ref, err := launchCostRefArg(args)
	if err != nil {
		return "", err
	}
	values := url.Values{}
	values.Set("max_points", fmt.Sprint(points))
	if ref != "" {
		values.Set("ref", ref)
	}
	return launchCostPath + "?" + values.Encode(), nil
}

func launchCostRefArg(args map[string]any) (string, error) {
	raw, ok := args["ref"]
	if !ok || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("ref must be a work item key such as owner/repo#123, got %v", raw)
	}
	ref := strings.TrimSpace(s)
	if len(ref) > launchCostMaxRefLen || strings.ContainsAny(ref, " \t\r\n") {
		return "", fmt.Errorf("ref must be a work item key such as owner/repo#123, got %q", s)
	}
	return ref, nil
}

// LaunchCostResult returns the endpoint's answer with by_ref cut to the page
// offset and limit ask for. Without ref, by_launch (every recorded kick in
// the window, which the Cost card does not list) is left out and the answer
// says how to read it; with ref it is that item's launches, capped to limit.
// The endpoint's own truncated flag describes its max_points cut, which the
// tool chose, so it is replaced by the tool's disclosure.
func LaunchCostResult(data any, args map[string]any) (any, error) {
	body, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the hive answered %s with an unexpected shape", ToolLaunchCostRead)
	}
	offset, err := OffsetFromArgs(args)
	if err != nil {
		return nil, err
	}
	ref, err := launchCostRefArg(args)
	if err != nil {
		return nil, err
	}
	limit := LimitFromArgs(args)
	refs, _ := body["by_ref"].([]any)
	total := len(refs)
	if n, ok := body["by_ref_total"].(float64); ok && int(n) > total {
		total = int(n)
	}
	start := min(offset, len(refs))
	end := min(start+limit, len(refs))

	out := make(map[string]any, len(body)+3)
	for k, v := range body {
		out[k] = v
	}
	delete(out, "truncated")
	page := refs[start:end]
	out["by_ref"] = page
	out["by_ref_total"] = total
	out["offset"] = offset
	var notes []string
	fields := []string{}
	// next counts from offset, not from the rows the endpoint sent, and is
	// only offered when this page returned rows, so a caller following
	// next_offset always moves forward.
	next := offset + len(page)
	switch {
	case offset > 0 && offset >= total:
		fields = append(fields, "by_ref")
		notes = append(notes, fmt.Sprintf("offset %d is past the last of the %d by_ref rows.", offset, total))
	case len(page) > 0 && next < total:
		out["next_offset"] = next
		fields = append(fields, "by_ref")
		notes = append(notes, fmt.Sprintf("by_ref holds rows %d-%d of %d; pass offset=%d for the next page.", offset+1, next, total, next))
	}
	launches, _ := body["by_launch"].([]any)
	launchTotal := len(launches)
	if n, ok := body["by_launch_total"].(float64); ok && int(n) > launchTotal {
		launchTotal = int(n)
	}
	if ref == "" {
		delete(out, "by_launch")
		if launchTotal > 0 {
			fields = append(fields, "by_launch")
			notes = append(notes, fmt.Sprintf("by_launch (%d kicks in the window) is left out; pass ref to list one work item's launches.", launchTotal))
		}
	} else if len(launches) > limit {
		out["by_launch"] = launches[:limit]
		fields = append(fields, "by_launch")
		notes = append(notes, fmt.Sprintf("by_launch holds the newest %d of %d launches for %s; raise limit (up to %d) to read more.", limit, launchTotal, ref, MaxResultLimit))
	}
	if len(notes) > 0 {
		out["_admin_mcp"] = map[string]any{
			"truncated":        true,
			"limit":            limit,
			"offset":           offset,
			"total":            total,
			"truncated_fields": fields,
			"note":             strings.Join(notes, " "),
		}
	}
	return out, nil
}

func launchCostReadSchema() map[string]any {
	return map[string]any{
		"ref":    map[string]any{"type": "string", "description": "One work item, as owner/repo#123 (case-insensitive): narrows by_ref to it and returns its launches in by_launch."},
		"limit":  modelRowsLimitSchema("by_ref"),
		"offset": map[string]any{"type": "integer", "minimum": 0, "description": "by_ref rows to skip (default 0); pass the previous answer's next_offset to read the next page."},
	}
}
