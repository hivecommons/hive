package adminmcp

import (
	"fmt"
	"sort"
)

// ToolGitHubAPIUsage answers "where does this hive's GitHub API budget go":
// the core bucket, the API budget mode, the calls that cost the most, the
// same window summed per caller, the per-agent proxy caps and the ETag cache.
// The dashboard shows these on the GitHub API card and under Debug → GitHub
// API budget; this tool reads the endpoint both of those read,
// GET /api/gh-rate-limits, with ?usage=1 for the per-caller view.
//
// It never spends a core API call. The endpoint makes one GET /rate_limit,
// which GitHub does not charge to any bucket, and everything else in the
// answer is the hive's own in-memory accounting of the last hour.
const ToolGitHubAPIUsage = "github_api_usage"

const gitHubAPIUsagePath = "/api/gh-rate-limits"

const gitHubAPIUsageDescription = "Read where this hive's GitHub API budget goes, through GET /api/gh-rate-limits (the endpoint behind the dashboard's GitHub API card and Debug → GitHub API budget). Returns core (limit, remaining, reset, observed_at, reset_in_seconds); api_budget (mode normal, conserve or critical, since, skipped_steps, and the configured reserve and critical thresholds); top_consumers (caller, endpoint, method, requests, charged, not_modified, rate_limited, last_status, last_seen), most charged first, limit rows; by_caller (the same window summed per caller, never cut) with window_totals; agent_caps (each agent's proxy cap and its calls this hour); etag_cache; and window_seconds, the span the counts cover (one hour). charged counts requests GitHub billed; not_modified counts free 304 answers. Reading it spends no core API call: the hive makes one GET /rate_limit, which GitHub does not charge, and the rest is the hive's own in-memory accounting. Read-only."

const gitHubAPIUsageReading = "The counts cover the last window_seconds seconds. requests is every request a caller made; charged is the ones GitHub billed to a rate-limit bucket (every answer except a 304 and GET /rate_limit; rows on /graphql draw on the graphql bucket, not core); not_modified is conditional requests answered 304, which are free; rate_limited is answers refused for an exhausted limit. Callers named agent:<name> are agents going through the hive's GitHub proxy; hive:<name> is the hive's own sweep or step of that name. top_consumers holds the limit most charged caller + method + endpoint rows (top_consumers_total says how many there are); by_caller and window_totals always cover the whole window. api_budget.mode is normal while core.remaining is at or above api_budget.reserve, conserve below it (optional sweeps are skipped and the eval loop slows) and critical below api_budget.critical (the unstick steps are skipped too); skipped_steps names what the last cycle left out."

// GitHubAPIUsageReadPath is the one GET the github_api_usage tool makes. top
// asks the endpoint for as many top_consumers rows as the tool's limit, and
// usage=1 for the per-caller totals, agent caps and window.
func GitHubAPIUsageReadPath(args map[string]any) string {
	return fmt.Sprintf("%s?top=%d&usage=1", gitHubAPIUsagePath, LimitFromArgs(args))
}

// GitHubAPIUsageResult shapes a decoded GET /api/gh-rate-limits?usage=1 answer
// for the github_api_usage tool. Every number is the hive's: nothing is
// counted here except the cut of top_consumers to limit.
//
// A hive older than the ?usage=1 view ignores the parameter and answers with
// its fixed ten top_consumers and no per-caller fields. That answer is still
// shaped and returned, with usage_view false and a note saying what is
// missing, rather than refused.
func GitHubAPIUsageResult(data any, args map[string]any) (any, error) {
	body, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the hive answered GET %s with an unexpected shape", gitHubAPIUsagePath)
	}
	limit := LimitFromArgs(args)
	_, usageView := body["window_seconds"]

	core := ghUsageObject(body["core"])
	if v, ok := body["core_reset_in_seconds"]; ok {
		core["reset_in_seconds"] = v
	}
	budget := ghUsageObject(body["api_budget"])
	if _, ok := budget["skipped_steps"]; !ok {
		budget["skipped_steps"] = []any{}
	}
	for k, v := range ghUsageObject(body["api_budget_thresholds"]) {
		budget[k] = v
	}

	rows := ghUsageRows(body["top_consumers"])
	total := len(rows)
	if n, ok := body["top_consumers_total"]; ok && int(rowNumber(n)) > total {
		total = int(rowNumber(n))
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}

	out := map[string]any{
		"type":                ToolGitHubAPIUsage,
		"reading":             gitHubAPIUsageReading,
		"usage_view":          usageView,
		"core":                core,
		"api_budget":          budget,
		"top_consumers":       rows,
		"top_consumers_total": total,
		"limit":               limit,
		"etag_cache":          ghUsageObject(body["etag_cache"]),
	}
	for _, key := range []string{"graphql", "search"} {
		if v, ok := body[key].(map[string]any); ok {
			out[key] = v
		}
	}
	if usageView {
		out["by_caller"] = ghUsageRows(body["by_caller"])
		out["window_totals"] = ghUsageObject(body["window_totals"])
		out["window_seconds"] = body["window_seconds"]
		out["agent_caps"] = ghUsageObject(body["agent_caps"])
	} else {
		out["note"] = "This hive's build does not have the per-caller view yet: by_caller, window_totals, agent_caps, window_seconds, the budget thresholds and reset_in_seconds are missing, and top_consumers is the hive's fixed top ten whatever limit says. The counts still cover the last hour. Upgrade the hive for the full answer."
	}
	if total > len(rows) {
		out["top_consumers_truncated"] = true
		out["_admin_mcp"] = map[string]any{
			"truncated":        true,
			"limit":            limit,
			"total":            total,
			"truncated_fields": []string{"top_consumers"},
		}
	}
	return out, nil
}

// ghUsageObject copies a decoded JSON object, or returns an empty one, so a
// missing section is {} and the endpoint's own map is never modified.
func ghUsageObject(v any) map[string]any {
	src, _ := v.(map[string]any)
	out := make(map[string]any, len(src)+4)
	for k, val := range src {
		out[k] = val
	}
	return out
}

// ghUsageRows returns a decoded JSON list of accounting rows, most charged
// first. The endpoint already sorts them; sorting again here keeps the order
// the tool promises against any hive, and a stable sort keeps the endpoint's
// order among rows with equal charged. A missing list is [].
func ghUsageRows(v any) []any {
	src, _ := v.([]any)
	out := make([]any, 0, len(src))
	out = append(out, src...)
	sort.SliceStable(out, func(i, j int) bool {
		return ghUsageCharged(out[i]) > ghUsageCharged(out[j])
	})
	return out
}

func ghUsageCharged(row any) float64 {
	m, _ := row.(map[string]any)
	return rowNumber(m["charged"])
}

func gitHubAPIUsageSchema() map[string]any {
	return map[string]any{
		"limit": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     MaxResultLimit,
			"description": fmt.Sprintf("top_consumers rows to return, 1-%d (default %d), most charged first. by_caller and window_totals are never cut by it.", MaxResultLimit, DefaultResultLimit),
		},
	}
}
