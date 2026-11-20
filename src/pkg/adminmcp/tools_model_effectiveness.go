package adminmcp

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// pr_models_read, contributor_model_stats_read and effective_models_read read
// model effectiveness, which the dashboard shows and the MCP could not:
// Governor → "PRs by model" (GET /api/governor/pr-models), the contributor run
// stats by model (GET /api/contribute/run-stats/models) and the Operations
// page's "Most effective models" table (GET /api/contribute/effective-models),
// the only view that splits hive-agent PRs from contributor PRs and says how
// much history the numbers cover. spend_read gives cost by model but not
// outcomes, and contributors_list gives per-contributor totals with no window
// or merge outcome.
//
// The endpoints quietly fall back to their default window or filter on a value
// they do not know, so the tools check window, days and filter first and
// refuse a bad value with its reason rather than answer for a window the
// caller did not ask for.

const (
	ToolPRModelsRead              = "pr_models_read"
	ToolContributorModelStatsRead = "contributor_model_stats_read"
	ToolEffectiveModelsRead       = "effective_models_read"

	prModelsPath              = "/api/governor/pr-models"
	contributorModelStatsPath = "/api/contribute/run-stats/models"
	effectiveModelsPath       = "/api/contribute/effective-models"

	defaultPRModelsWindow            = "7d"
	defaultContributorModelStatsDays = 7
	maxContributorModelStatsDays     = 365
	defaultEffectiveModelsFilter     = "all"
)

// prModelsWindows are the windows GET /api/governor/pr-models knows, the same
// three the Governor card's 7d / 30d / all pills ask for. GET
// /api/contribute/effective-models knows the same three.
var prModelsWindows = []string{"7d", "30d", "all"}

// effectiveModelsFilters are the filters GET /api/contribute/effective-models
// knows, the Operations panel's All work / Hive agents / Contributor relays
// chips.
var effectiveModelsFilters = []string{"all", "hive", "contributor"}

const prModelsReadDescription = "Read Governor → \"PRs by model\" through GET /api/governor/pr-models, the endpoint the dashboard card reads. window is 7d (default), 30d or all. Returns window, min_merged_prs, total, unknown, and buckets: one per canonical model with prs, merged, closed_unmerged, open, rework (first-pass merge rate, review rounds, fix attempts, follow-up commits, median time to merge), effectiveness (rank, eligible, score, runs, verified-PR run rate, failure rate, nothing-to-ship rate) and per-agent counts (agents). Also most_reworked (up to ten PRs) and fixer_queue. limit caps the buckets rows and offset skips rows; a capped answer says so in _admin_mcp and gives next_offset. No cost: use spend_read for spend by model. Run stats here join contributor runs to every PR of a model; effective_models_read splits hive-agent PRs from contributor PRs. Read-only."

const contributorModelStatsReadDescription = "Read the contributor run stats by model through GET /api/contribute/run-stats/models: per model, runs, verified_pr_share (runs that ended in a verified PR) and failure_rate, over window_days. days is 0-365 (default 7); 0 reads every recorded run. Rows are sorted by model name. limit caps the models rows and offset skips rows; a capped answer says so in _admin_mcp and gives next_offset. Read-only."

const effectiveModelsReadDescription = "Read the Operations page's \"Most effective models\" table through GET /api/contribute/effective-models, the endpoint the panel reads. window is 7d (default), 30d or all. filter is all (default), hive (PRs the hive's own agents opened; these have no contributor runs, so run columns are empty) or contributor (only PRs a verified contributor run produced, with those runs). Returns window, filter, min_merged_prs, ranked (models with at least min_merged_prs merged PRs, best score first, with effectiveness_rank), insufficient (the rest) and coverage (closed_pr_lookback_days, closed_prs_partial, closed_prs_since, runs_since, runs_partial: how much history the window really covers; closed_prs_since is when the hive's stored PR history starts). Each row has model, backends, sources, runtime, score, prs, merged_prs, first-pass merges and rate, average review rounds and fix attempts, runs, and verified-PR, failed and nothing-to-ship run counts and rates. ranked is returned whole; limit caps the insufficient rows and offset skips rows of it. Read-only."

// ModelEffectivenessTool reports whether tool is one of the model
// effectiveness reads.
func ModelEffectivenessTool(tool string) bool {
	return tool == ToolPRModelsRead || tool == ToolContributorModelStatsRead || tool == ToolEffectiveModelsRead
}

// ModelEffectivenessReadPath is the one GET a model effectiveness tool makes,
// with its window checked. A window, days, filter or offset value the
// endpoint or the tool does not know is refused with its reason, before any
// request is made.
func ModelEffectivenessReadPath(tool string, args map[string]any) (string, error) {
	if _, err := OffsetFromArgs(args); err != nil {
		return "", err
	}
	switch tool {
	case ToolPRModelsRead:
		window, err := prModelsWindowArg(args)
		if err != nil {
			return "", err
		}
		return prModelsPath + "?window=" + window, nil
	case ToolContributorModelStatsRead:
		days, err := contributorModelStatsDaysArg(args)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s?days=%d", contributorModelStatsPath, days), nil
	case ToolEffectiveModelsRead:
		window, err := prModelsWindowArg(args)
		if err != nil {
			return "", err
		}
		filter, err := effectiveModelsFilterArg(args)
		if err != nil {
			return "", err
		}
		return effectiveModelsPath + "?window=" + window + "&filter=" + filter, nil
	}
	return "", fmt.Errorf("%s is not a model effectiveness tool", tool)
}

func prModelsWindowArg(args map[string]any) (string, error) {
	raw, ok := args["window"]
	if !ok || raw == nil {
		return defaultPRModelsWindow, nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("window must be one of %s, got %v", strings.Join(prModelsWindows, ", "), raw)
	}
	window := strings.ToLower(strings.TrimSpace(s))
	if window == "" {
		return defaultPRModelsWindow, nil
	}
	for _, w := range prModelsWindows {
		if window == w {
			return window, nil
		}
	}
	return "", fmt.Errorf("window must be one of %s, got %q", strings.Join(prModelsWindows, ", "), s)
}

func effectiveModelsFilterArg(args map[string]any) (string, error) {
	raw, ok := args["filter"]
	if !ok || raw == nil {
		return defaultEffectiveModelsFilter, nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("filter must be one of %s, got %v", strings.Join(effectiveModelsFilters, ", "), raw)
	}
	filter := strings.ToLower(strings.TrimSpace(s))
	if filter == "" {
		return defaultEffectiveModelsFilter, nil
	}
	for _, f := range effectiveModelsFilters {
		if filter == f {
			return filter, nil
		}
	}
	return "", fmt.Errorf("filter must be one of %s, got %q", strings.Join(effectiveModelsFilters, ", "), s)
}

func contributorModelStatsDaysArg(args map[string]any) (int, error) {
	bad := func(v any) error {
		return fmt.Errorf("days must be a whole number from 0 to %d (0 reads every recorded run), got %v", maxContributorModelStatsDays, v)
	}
	days := defaultContributorModelStatsDays
	switch v := args["days"].(type) {
	case nil:
	case float64:
		if v != math.Trunc(v) {
			return 0, bad(v)
		}
		days = int(v)
	case int:
		days = v
	case string:
		if strings.TrimSpace(v) == "" {
			break
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, bad(fmt.Sprintf("%q", v))
		}
		days = n
	default:
		return 0, bad(v)
	}
	if days < 0 || days > maxContributorModelStatsDays {
		return 0, bad(days)
	}
	return days, nil
}

// modelRowsField is the list a model effectiveness tool pages: buckets for
// pr_models_read, models for contributor_model_stats_read and insufficient
// for effective_models_read, whose ranked list is short (only models with
// enough merged PRs) and is always returned whole.
func modelRowsField(tool string) string {
	switch tool {
	case ToolContributorModelStatsRead:
		return "models"
	case ToolEffectiveModelsRead:
		return "insufficient"
	}
	return "buckets"
}

// ModelEffectivenessResult returns the endpoint's answer as the dashboard gets
// it, with only the per-model rows (see modelRowsField) cut to the page that
// offset and limit ask for. An answer that fits from offset 0 is returned
// unchanged. A cut answer carries <field>_total, offset, next_offset when
// more rows follow, and an _admin_mcp note naming the field, so a capped list
// is never mistaken for the whole.
func ModelEffectivenessResult(tool string, data any, args map[string]any) (any, error) {
	body, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the hive answered %s with an unexpected shape", tool)
	}
	offset, err := OffsetFromArgs(args)
	if err != nil {
		return nil, err
	}
	field := modelRowsField(tool)
	rows, _ := body[field].([]any)
	limit := LimitFromArgs(args)
	total := len(rows)
	if offset == 0 && total <= limit {
		return body, nil
	}
	start := min(offset, total)
	end := min(start+limit, total)
	out := make(map[string]any, len(body)+4)
	for k, v := range body {
		out[k] = v
	}
	out[field] = rows[start:end]
	out[field+"_total"] = total
	out["offset"] = offset
	more := ""
	if end < total {
		out["next_offset"] = end
		more = fmt.Sprintf(" Pass offset=%d for the next page.", end)
	}
	var note string
	switch {
	case start == 0:
		note = fmt.Sprintf("%s holds the first %d of %d rows; raise limit (up to %d) to read more.%s", field, end, total, MaxResultLimit, more)
	case start >= total:
		note = fmt.Sprintf("offset %d is past the last of the %d %s rows.", offset, total, field)
	default:
		note = fmt.Sprintf("%s holds rows %d-%d of %d.%s", field, start+1, end, total, more)
	}
	out["_admin_mcp"] = map[string]any{
		"truncated":        start > 0 || end < total,
		"limit":            limit,
		"offset":           offset,
		"total":            total,
		"truncated_fields": []string{field},
		"note":             note,
	}
	return out, nil
}

func prModelsReadSchema() map[string]any {
	return map[string]any{
		"window": map[string]any{"type": "string", "enum": prModelsWindows, "description": "PRs opened in the last 7 days (7d, the default), the last 30 days (30d), or ever (all)."},
		"limit":  modelRowsLimitSchema("buckets"),
		"offset": modelRowsOffsetSchema("buckets"),
	}
}

func contributorModelStatsReadSchema() map[string]any {
	return map[string]any{
		"days":   map[string]any{"type": "integer", "minimum": 0, "maximum": maxContributorModelStatsDays, "description": fmt.Sprintf("Runs from the last N days, 0-%d (default %d); 0 reads every recorded run.", maxContributorModelStatsDays, defaultContributorModelStatsDays)},
		"limit":  modelRowsLimitSchema("models"),
		"offset": modelRowsOffsetSchema("models"),
	}
}

func effectiveModelsReadSchema() map[string]any {
	return map[string]any{
		"window": map[string]any{"type": "string", "enum": prModelsWindows, "description": "PRs opened in the last 7 days (7d, the default), the last 30 days (30d), or ever (all)."},
		"filter": map[string]any{"type": "string", "enum": effectiveModelsFilters, "description": "all (default): every attributed PR, with contributor run stats; hive: PRs the hive's own agents opened, no run stats; contributor: PRs a verified contributor run produced, with those runs. The Operations panel's All work / Hive agents / Contributor relays chips."},
		"limit":  modelRowsLimitSchema("insufficient"),
		"offset": modelRowsOffsetSchema("insufficient"),
	}
}

func modelRowsLimitSchema(field string) map[string]any {
	return map[string]any{
		"type":        "integer",
		"minimum":     1,
		"maximum":     MaxResultLimit,
		"description": fmt.Sprintf("%s rows to return, 1-%d (default %d).", field, MaxResultLimit, DefaultResultLimit),
	}
}

func modelRowsOffsetSchema(field string) map[string]any {
	return map[string]any{
		"type":        "integer",
		"minimum":     0,
		"description": fmt.Sprintf("%s rows to skip (default 0); pass the previous answer's next_offset to read the next page.", field),
	}
}
