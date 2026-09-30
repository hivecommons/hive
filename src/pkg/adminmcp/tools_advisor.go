package adminmcp

import (
	"fmt"
	"net/url"
	"strings"
)

// ToolAdvisorRecords answers "what did the advisor flag" (hivecommons/hive#9725,
// phase 4 of #9638) for one agent or across the fleet over a time window. It
// is a read-only wrapper over GET /api/advisor/records, so it returns exactly
// the records the REST listing returns for the same agent and window, behind
// the same read-write floor. There is deliberately no advisor write tool: the
// advisor speaks, it never acts, and nothing here can change what it says.
const ToolAdvisorRecords = "advisor_records"

// AdvisorRecordsReadPath builds the GET /api/advisor/records request for the
// advisor_records tool from its optional agent, since, until and hours
// arguments plus the usual capped limit. Window validation (RFC3339 bounds,
// hours range) is left to the endpoint so both surfaces refuse identically.
func AdvisorRecordsReadPath(args map[string]any) string {
	values := url.Values{}
	for _, key := range []string{"agent", "since", "until"} {
		if v := strings.TrimSpace(stringArg(args, key)); v != "" {
			values.Set(key, v)
		}
	}
	switch v := args["hours"].(type) {
	case float64:
		values.Set("hours", fmt.Sprint(int(v)))
	case int:
		values.Set("hours", fmt.Sprint(v))
	case string:
		if s := strings.TrimSpace(v); s != "" {
			values.Set("hours", s)
		}
	}
	values.Set("limit", fmt.Sprint(LimitFromArgs(args)))
	return "/api/advisor/records?" + values.Encode()
}

func advisorRecordsSchema() map[string]any {
	return map[string]any{
		"agent": map[string]any{"type": "string", "description": "Agent name; unset reads the whole fleet."},
		"since": map[string]any{"type": "string", "format": "date-time", "description": "RFC3339 start of the window (inclusive)."},
		"until": map[string]any{"type": "string", "format": "date-time", "description": "RFC3339 end of the window (inclusive)."},
		"hours": map[string]any{"type": "integer", "minimum": 1, "maximum": 720, "description": "Lookback from now in hours, instead of since (24 = the last day)."},
		"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": MaxResultLimit},
	}
}
