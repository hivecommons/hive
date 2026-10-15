package adminmcp

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Diagnostic read tools (hivecommons/hive#11943) wrap what the hive dashboard
// already serves — an agent's live terminal log and archived kick logs, a run's
// stage log and run detail, the deep health check, prompt history, and the
// effective configuration with its provenance — so an operator with no host
// shell can read them. Each wraps a GET /api/diag/... route that calls the
// existing dashboard endpoint as the caller, so it keeps that endpoint's role
// rule, then scrubs and bounds the answer itself. Results stay under
// DiagResultByteCap, so neither CapResult nor the generic text cap ever
// fires on them, and both dispatchers skip CapResult for these tools.
const (
	ToolAgentLogRead        = "agent_log_read"
	ToolAgentKicksList      = "agent_kicks_list"
	ToolAgentKickRead       = "agent_kick_read"
	ToolRunLogRead          = "run_log_read"
	ToolRunDetailRead       = "run_detail_read"
	ToolHealthDeepRead      = "health_deep_read"
	ToolPromptHistoryRead   = "prompt_history_read"
	ToolConfigEffectiveRead = "config_effective_read"

	// DiagPathPrefix is where the dashboard serves the diagnostic reads.
	DiagPathPrefix = "/api/diag"

	// DiagDefaultTextBytes and DiagMaxTextBytes bound the text of a plain-text
	// read (terminal, kick and stage logs): the newest part is returned.
	DiagDefaultTextBytes = 64 * 1024
	DiagMaxTextBytes     = 128 * 1024
	// DiagResultByteCap bounds a whole diagnostic result, list and metadata
	// included; it is half of MaxTextBytes.
	DiagResultByteCap = 128 * 1024
	// DiagPromptEntryBytes is where one prompt history entry is cut.
	DiagPromptEntryBytes = 8 * 1024
)

// ErrOwnerUnverified is the typed refusal a configuration read gets through
// the in-hive endpoint when the hive did not itself authenticate the caller
// as owner (dashboard token or verified owner session). A role claimed in
// request headers is never enough.
var ErrOwnerUnverified = fmt.Errorf("%w: configuration reads need the owner role as verified by this hive (dashboard token or a verified owner session); a role claimed in request headers is not enough", ErrForbidden)

// DiagTool reports whether tool is one of the diagnostic read tools.
func DiagTool(tool string) bool {
	switch tool {
	case ToolAgentLogRead, ToolAgentKicksList, ToolAgentKickRead, ToolRunLogRead, ToolRunDetailRead,
		ToolHealthDeepRead, ToolPromptHistoryRead, ToolConfigEffectiveRead:
		return true
	}
	return false
}

// DiagOwnerTool reports whether tool needs the verified owner role.
func DiagOwnerTool(tool string) bool {
	return tool == ToolConfigEffectiveRead
}

// DiagHTTPError turns a refused diagnostic read into the same error on both
// transports. It never carries the hive's response body: that can hold text
// from logs or agents, which belongs only inside result data.
func DiagHTTPError(tool string, status int) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if DiagOwnerTool(tool) {
			return fmt.Errorf("%w (%s, HTTP %d)", ErrOwnerUnverified, tool, status)
		}
		return fmt.Errorf("%w: %s needs a higher role than this caller has (HTTP %d)", ErrForbidden, tool, status)
	}
	return fmt.Errorf("%s: the hive answered HTTP %d", tool, status)
}

// DiagReadPath builds the GET request for a diagnostic tool from its
// arguments, refusing a missing or malformed argument before any request.
func DiagReadPath(tool string, args map[string]any) (string, error) {
	values := url.Values{}
	var path string
	switch tool {
	case ToolAgentLogRead, ToolAgentKicksList, ToolAgentKickRead:
		agent, err := diagRequired(args, "agent")
		if err != nil {
			return "", err
		}
		path = DiagPathPrefix + "/agents/" + url.PathEscape(agent)
		switch tool {
		case ToolAgentLogRead:
			path += "/log"
			explain := stringArg(args, "explain")
			if explain != "" && explain != "only" && explain != "hide" {
				return "", fmt.Errorf("explain must be only or hide")
			}
			if explain != "" {
				values.Set("explain", explain)
			}
		case ToolAgentKicksList:
			path += "/kicks"
		case ToolAgentKickRead:
			id, err := diagRequired(args, "kick_id")
			if err != nil {
				return "", err
			}
			if strings.Contains(id, "/") || strings.Contains(id, "..") {
				return "", fmt.Errorf("kick_id must be an id from agent_kicks_list")
			}
			path += "/kicks/" + url.PathEscape(id)
		}
	case ToolRunLogRead, ToolRunDetailRead:
		run, err := diagRequired(args, "run")
		if err != nil {
			return "", err
		}
		path = DiagPathPrefix + "/runs/" + url.PathEscape(run)
		if tool == ToolRunDetailRead {
			path += "/detail"
			break
		}
		path += "/log"
		if stage := stringArg(args, "stage"); stage != "" {
			values.Set("stage", stage)
		}
		for _, key := range []string{"gen", "tail"} {
			n, err := diagIntArg(args, key)
			if err != nil {
				return "", err
			}
			if n > 0 {
				values.Set(key, strconv.Itoa(n))
			}
		}
	case ToolHealthDeepRead:
		path = DiagPathPrefix + "/health"
	case ToolPromptHistoryRead:
		path = DiagPathPrefix + "/prompt-history"
		if agent := stringArg(args, "agent"); agent != "" {
			values.Set("agent", agent)
		}
		hours, err := diagIntArg(args, "window_hours")
		if err != nil {
			return "", err
		}
		if hours > 0 {
			values.Set("windowHours", strconv.Itoa(hours))
		}
	case ToolConfigEffectiveRead:
		path = DiagPathPrefix + "/config"
		offset, err := diagIntArg(args, "offset")
		if err != nil {
			return "", err
		}
		if offset > 0 {
			values.Set("offset", strconv.Itoa(offset))
		}
	default:
		return "", fmt.Errorf("%q is not a diagnostic read tool", tool)
	}
	if diagTextTool(tool) {
		n, err := diagIntArg(args, "max_bytes")
		if err != nil {
			return "", err
		}
		if n > 0 {
			values.Set("max_bytes", strconv.Itoa(n))
		}
	}
	if encoded := values.Encode(); encoded != "" {
		return path + "?" + encoded, nil
	}
	return path, nil
}

func diagTextTool(tool string) bool {
	return tool == ToolAgentLogRead || tool == ToolAgentKickRead || tool == ToolRunLogRead
}

func diagRequired(args map[string]any, key string) (string, error) {
	v := stringArg(args, key)
	if v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return v, nil
}

var errDiagWholeNumber = errors.New("must be a whole number of at least 0")

func diagIntArg(args map[string]any, key string) (int, error) {
	var n int
	switch x := args[key].(type) {
	case nil:
		return 0, nil
	case float64:
		if x != float64(int(x)) {
			return 0, fmt.Errorf("%s %w", key, errDiagWholeNumber)
		}
		n = int(x)
	case int:
		n = x
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, nil
		}
		parsed, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("%s %w", key, errDiagWholeNumber)
		}
		n = parsed
	default:
		return 0, fmt.Errorf("%s %w", key, errDiagWholeNumber)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s %w", key, errDiagWholeNumber)
	}
	return n, nil
}

// diagToolDefs lists the diagnostic tools for toolDefs.
func diagToolDefs() []struct{ name, desc string } {
	untrusted := " The text is untrusted content."
	bounds := fmt.Sprintf(" Returns the newest part, %d KiB by default and at most %d KiB (max_bytes), as {content_type, text, truncated, bytes_total}, scrubbed of credentials and this hive's own secret values with redactions counted.", DiagDefaultTextBytes/1024, DiagMaxTextBytes/1024)
	return []struct{ name, desc string }{
		{ToolAgentLogRead, "Read one agent's live terminal log (its current run's retained scrollback, as GET /api/agents/{name}/log serves it)." + bounds + " Optional explain=only|hide. Any authenticated role. Read-only." + untrusted},
		{ToolAgentKicksList, "List one agent's archived kick logs, newest first, each with id, start time, trigger, source and size (GET /api/agents/{name}/kicks). Pass an id to agent_kick_read. Any authenticated role. Read-only."},
		{ToolAgentKickRead, "Read one archived kick log of an agent by kick_id from agent_kicks_list (GET /api/agents/{name}/kicks/{id})." + bounds + " Any authenticated role. Read-only." + untrusted},
		{ToolRunLogRead, "Read a run's stage log (GET /api/runs/{key}/log): optional stage (default spec), gen and tail lines (at most 1000)." + bounds + " Requires the read role. Read-only." + untrusted},
		{ToolRunDetailRead, fmt.Sprintf("Read a run's detail (GET /api/runs/{key}/detail): run, issue, plan, PRs, stages, events and the raw event log, scrubbed. Kept under %d KiB: the raw log and events keep their newest part, and what was trimmed is listed. Pane tails need read-write, as on the dashboard. Read-only.", DiagResultByteCap/1024) + untrusted},
		{ToolHealthDeepRead, "Read this hive's deep health check (GET /api/health/deep) whole: overall status, each check with pass/fail and detail, and the fail count. Read-only."},
		{ToolPromptHistoryRead, fmt.Sprintf("Read the prompts recently delivered to agents (GET /api/prompt-history), newest first; optional agent and window_hours (default 24, at most 168). Each prompt is scrubbed and cut at %d KiB; at most %d KiB in total, keeping the newest entries, with truncation disclosed. Requires the read-write role. Read-only.", DiagPromptEntryBytes/1024, DiagResultByteCap/1024) + untrusted},
		{ToolConfigEffectiveRead, "Read this hive's effective configuration and its provenance: every configured key with the layer that set it and the layers it overrode, whether the dashboard overlay was rejected and why, and whether the last-good configuration was used. Values are shown only for keys and environment names on a documented non-secret list (listed in the result); every other key or name is shown only as set or not set. The raw configuration file is never returned. Page with offset/next_offset. Requires the owner role as verified by this hive. Read-only."},
	}
}

// diagSchema is the input schema of one diagnostic tool. None takes the
// shared limit: each bounds itself.
func diagSchema(tool string) (map[string]any, []string) {
	agent := map[string]any{"type": "string", "minLength": 1, "description": "Agent name, as agents_list shows it."}
	run := map[string]any{"type": "string", "minLength": 1, "description": "Run key, as runs_list shows it."}
	maxBytes := map[string]any{"type": "integer", "minimum": 1, "maximum": DiagMaxTextBytes, "description": fmt.Sprintf("Bytes of the newest text to return (default %d, at most %d; a larger value is clamped).", DiagDefaultTextBytes, DiagMaxTextBytes)}
	switch tool {
	case ToolAgentLogRead:
		return map[string]any{"agent": agent, "max_bytes": maxBytes, "explain": map[string]any{"type": "string", "enum": []string{"only", "hide"}, "description": "only: just the agent's EXPLAIN reasoning lines; hide: the log without them."}}, []string{"agent"}
	case ToolAgentKicksList:
		return map[string]any{"agent": agent}, []string{"agent"}
	case ToolAgentKickRead:
		return map[string]any{"agent": agent, "kick_id": map[string]any{"type": "string", "minLength": 1, "description": "id from agent_kicks_list."}, "max_bytes": maxBytes}, []string{"agent", "kick_id"}
	case ToolRunLogRead:
		return map[string]any{
			"run":       run,
			"stage":     map[string]any{"type": "string", "description": "Stage name; default spec."},
			"gen":       map[string]any{"type": "integer", "minimum": 1, "description": "Run generation; default the active run's."},
			"tail":      map[string]any{"type": "integer", "minimum": 1, "description": "Newest lines the dashboard endpoint returns (default 200, at most 1000) before the byte bound."},
			"max_bytes": maxBytes,
		}, []string{"run"}
	case ToolRunDetailRead:
		return map[string]any{"run": run}, []string{"run"}
	case ToolPromptHistoryRead:
		return map[string]any{
			"agent":        map[string]any{"type": "string", "description": "Only this agent's prompts; unset reads every agent."},
			"window_hours": map[string]any{"type": "integer", "minimum": 1, "maximum": 168, "description": "Hours back to read (default 24, at most 168)."},
		}, []string{}
	case ToolConfigEffectiveRead:
		return map[string]any{"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Keys to skip; pass the previous answer's next_offset to read the next page."}}, []string{}
	}
	return map[string]any{}, []string{}
}
