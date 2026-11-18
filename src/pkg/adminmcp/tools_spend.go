package adminmcp

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// spend_read with a window (hivecommons/hive#18968): with hours, or since
// and/or until, the tool reads GET /api/cost/agents, the per-agent tokens and
// estimated $ the Cost card's usage-by-sandbox table shows for the same
// window, instead of the all-time GET /api/cost. Without them it is
// unchanged.

const (
	spendPath       = "/api/cost"
	spendAgentsPath = "/api/cost/agents"

	// spendWindowMaxHours mirrors the endpoint's hours ceiling and the
	// snapshot ring it reads (~30 days); a since/until span is held to it too.
	spendWindowMaxHours = 720
)

const spendReadDescription = "Read spend and budget status through GET /api/cost: the estimated all-time totals by model and by agent, the daily rollup and the budget. With hours (1-720), or since and/or until (RFC3339), it reads GET /api/cost/agents instead: each agent's tokens (input, output, cache_read, cache_create, total_tokens) and estimated usd over that window, highest spend first, the same rows the Cost card's usage-by-sandbox table shows for the window. The answer names the window (since, until), says whether the cost snapshots cover it (coverage full or partial, with data_since), and gives total_usd and total_tokens. Figures are estimates (token counts × list price), not billed spend; an agent's token fields are null where its snapshots predate per-agent token recording. agents is capped by limit. Read-only."

// SpendReadPath is the one GET spend_read makes. A bad window is refused
// with its reason, before any request is made.
func SpendReadPath(args map[string]any) (string, error) {
	return spendReadPathAt(args, time.Now().UTC())
}

func spendReadPathAt(args map[string]any, now time.Time) (string, error) {
	hours, hasHours, err := spendHoursArg(args)
	if err != nil {
		return "", err
	}
	since, hasSince, err := spendTimeArg(args, "since")
	if err != nil {
		return "", err
	}
	until, hasUntil, err := spendTimeArg(args, "until")
	if err != nil {
		return "", err
	}
	if !hasHours && !hasSince && !hasUntil {
		return spendPath, nil
	}
	if hasHours && hasSince {
		return "", fmt.Errorf("use either since or hours, not both")
	}
	end := now
	if hasUntil {
		end = until
	}
	start := end.Add(-24 * time.Hour)
	switch {
	case hasHours:
		start = now.Add(-time.Duration(hours) * time.Hour)
	case hasSince:
		start = since
	}
	if end.Before(start) {
		return "", fmt.Errorf("until must not be before since")
	}
	if end.Sub(start) > spendWindowMaxHours*time.Hour {
		return "", fmt.Errorf("the window must span at most %d hours (the cost snapshots keep about 30 days), got %s", spendWindowMaxHours, end.Sub(start).Round(time.Hour))
	}
	values := url.Values{}
	if hasHours {
		values.Set("hours", strconv.Itoa(hours))
	}
	if hasSince {
		values.Set("since", since.Format(time.RFC3339))
	}
	if hasUntil {
		values.Set("until", until.Format(time.RFC3339))
	}
	return spendAgentsPath + "?" + values.Encode(), nil
}

// spendHoursArg reads hours: a whole number from 1 to spendWindowMaxHours.
func spendHoursArg(args map[string]any) (int, bool, error) {
	raw, ok := args["hours"]
	if !ok || raw == nil {
		return 0, false, nil
	}
	var v float64
	switch x := raw.(type) {
	case float64:
		v = x
	case int:
		v = float64(x)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false, fmt.Errorf("hours must be a whole number from 1 to %d, got %q", spendWindowMaxHours, x)
		}
		v = parsed
	default:
		return 0, false, fmt.Errorf("hours must be a whole number from 1 to %d, got %v", spendWindowMaxHours, raw)
	}
	if v != math.Trunc(v) || v < 1 || v > spendWindowMaxHours {
		return 0, false, fmt.Errorf("hours must be a whole number from 1 to %d, got %v", spendWindowMaxHours, raw)
	}
	return int(v), true, nil
}

// spendTimeArg reads since or until: an RFC3339 timestamp.
func spendTimeArg(args map[string]any, key string) (time.Time, bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return time.Time{}, false, nil
	}
	s, ok := raw.(string)
	if !ok {
		return time.Time{}, false, fmt.Errorf("%s must be an RFC3339 timestamp such as 2026-11-18T00:00:00Z, got %v", key, raw)
	}
	if strings.TrimSpace(s) == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%s must be an RFC3339 timestamp such as 2026-11-18T00:00:00Z, got %q", key, s)
	}
	return t.UTC(), true, nil
}

func spendReadSchema() map[string]any {
	return map[string]any{
		"hours": map[string]any{"type": "integer", "minimum": 1, "maximum": spendWindowMaxHours, "description": "Per-agent tokens and estimated $ over the last N hours (24 = the last day), instead of the all-time totals."},
		"since": map[string]any{"type": "string", "format": "date-time", "description": "RFC3339 start of a per-agent window (inclusive); not with hours."},
		"until": map[string]any{"type": "string", "format": "date-time", "description": "RFC3339 end of a per-agent window (inclusive); default now. The window spans at most 720 hours."},
		"limit": limitSchema(),
	}
}
