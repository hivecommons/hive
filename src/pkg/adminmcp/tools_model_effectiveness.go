package adminmcp

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// pr_models_read and contributor_model_stats_read read model effectiveness,
// which the dashboard shows and the MCP could not: Governor → "PRs by model"
// (GET /api/governor/pr-models) and the contributor run stats by model (GET
// /api/contribute/run-stats/models). spend_read gives cost by model but not
// outcomes, and contributors_list gives per-contributor totals with no window
// or merge outcome.
//
// Both endpoints quietly fall back to their default window on a value they do
// not know, so the tools check window and days first and refuse a bad value
// with its reason rather than answer for a window the caller did not ask for.

const (
	ToolPRModelsRead              = "pr_models_read"
	ToolContributorModelStatsRead = "contributor_model_stats_read"

	prModelsPath              = "/api/governor/pr-models"
	contributorModelStatsPath = "/api/contribute/run-stats/models"

	defaultPRModelsWindow            = "7d"
	defaultContributorModelStatsDays = 7
	maxContributorModelStatsDays     = 365
)

// prModelsWindows are the windows GET /api/governor/pr-models knows, the same
// three the Governor card's 7d / 30d / all pills ask for.
var prModelsWindows = []string{"7d", "30d", "all"}

const prModelsReadDescription = "Read Governor → \"PRs by model\" through GET /api/governor/pr-models, the endpoint the dashboard card reads. window is 7d (default), 30d or all. Returns window, min_merged_prs, total, unknown, and buckets: one per canonical model with prs, merged, closed_unmerged, open, rework (first-pass merge rate, review rounds, fix attempts, follow-up commits, median time to merge), effectiveness (rank, eligible, score, runs, verified-PR run rate, failure rate, nothing-to-ship rate) and per-agent counts (agents). Also most_reworked (up to ten PRs) and fixer_queue. limit caps the buckets rows; a capped answer says so in _admin_mcp. No cost: use spend_read for spend by model. Read-only."

const contributorModelStatsReadDescription = "Read the contributor run stats by model through GET /api/contribute/run-stats/models: per model, runs, verified_pr_share (runs that ended in a verified PR) and failure_rate, over window_days. days is 0-365 (default 7); 0 reads every recorded run. limit caps the models rows; a capped answer says so in _admin_mcp. Read-only."

// ModelEffectivenessTool reports whether tool is pr_models_read or
// contributor_model_stats_read.
func ModelEffectivenessTool(tool string) bool {
	return tool == ToolPRModelsRead || tool == ToolContributorModelStatsRead
}

// ModelEffectivenessReadPath is the one GET a model effectiveness tool makes,
// with its window checked. A window or days value the endpoint does not know
// is refused with its reason.
func ModelEffectivenessReadPath(tool string, args map[string]any) (string, error) {
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

// ModelEffectivenessResult returns the endpoint's answer as the dashboard gets
// it, with only the per-model rows (buckets for pr_models_read, models for
// contributor_model_stats_read) cut to limit. A cut answer carries
// <field>_total and an _admin_mcp note naming the field, so a capped list is
// never mistaken for the whole.
func ModelEffectivenessResult(tool string, data any, args map[string]any) (any, error) {
	body, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the hive answered %s with an unexpected shape", tool)
	}
	field := "buckets"
	if tool == ToolContributorModelStatsRead {
		field = "models"
	}
	rows, _ := body[field].([]any)
	limit := LimitFromArgs(args)
	if len(rows) <= limit {
		return body, nil
	}
	out := make(map[string]any, len(body)+3)
	for k, v := range body {
		out[k] = v
	}
	out[field] = rows[:limit]
	out[field+"_total"] = len(rows)
	out["_admin_mcp"] = map[string]any{
		"truncated":        true,
		"limit":            limit,
		"total":            len(rows),
		"truncated_fields": []string{field},
		"note":             fmt.Sprintf("%s holds the first %d of %d rows; raise limit (up to %d) to read more.", field, limit, len(rows), MaxResultLimit),
	}
	return out, nil
}

func prModelsReadSchema() map[string]any {
	return map[string]any{
		"window": map[string]any{"type": "string", "enum": prModelsWindows, "description": "PRs opened in the last 7 days (7d, the default), the last 30 days (30d), or ever (all)."},
		"limit":  modelRowsLimitSchema("buckets"),
	}
}

func contributorModelStatsReadSchema() map[string]any {
	return map[string]any{
		"days":  map[string]any{"type": "integer", "minimum": 0, "maximum": maxContributorModelStatsDays, "description": fmt.Sprintf("Runs from the last N days, 0-%d (default %d); 0 reads every recorded run.", maxContributorModelStatsDays, defaultContributorModelStatsDays)},
		"limit": modelRowsLimitSchema("models"),
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
