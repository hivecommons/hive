package adminmcp

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux/logread"
)

// ToolLogRead and ToolLogSources read this hive's logs (hivecommons/hive#11942)
// for an operator with no shell on the host. They wrap GET /api/logs/read and
// GET /api/logs/sources, so they return what those endpoints return, behind
// the same read-write floor the read endpoint checks itself. Nothing either
// tool does rotates, truncates, deletes or edits a log, or changes a log
// setting.
//
// Their results are bounded by crux/logread (lines, bytes, scanned work) and
// are never passed through CapResult: lines[] is the page, and paging goes
// through next_cursor rather than the shared limit.
const (
	ToolLogRead    = "log_read"
	ToolLogSources = "log_sources"
)

// LogTool reports whether tool is one of the log read tools.
func LogTool(tool string) bool {
	return tool == ToolLogRead || tool == ToolLogSources
}

// LogReadPath builds the GET request for a log tool from its arguments. Values
// that the endpoint would refuse (a since/until without an explicit offset, an
// unknown min_level, a non-integer lines) are refused here with the same
// reason, so a caller learns why rather than seeing only an HTTP status.
func LogReadPath(tool string, args map[string]any) (string, error) {
	if tool == ToolLogSources {
		return "/api/logs/sources", nil
	}
	if tool != ToolLogRead {
		return "", fmt.Errorf("%q is not a log tool", tool)
	}
	values := url.Values{}
	for _, key := range []string{"source", "include", "cursor"} {
		if v := stringArg(args, key); v != "" {
			values.Set(key, v)
		}
	}
	for _, key := range []string{"since", "until"} {
		v := stringArg(args, key)
		if _, err := logread.ParseInstant(key, v); err != nil {
			return "", err
		}
		if v != "" {
			values.Set(key, v)
		}
	}
	level, err := logread.ParseMinLevel(stringArg(args, "min_level"))
	if err != nil {
		return "", err
	}
	if level != "" {
		values.Set("min_level", level)
	}
	lines, err := logLinesArg(args["lines"])
	if err != nil {
		return "", err
	}
	if lines > 0 {
		values.Set("lines", strconv.Itoa(lines))
	}
	exclude, err := logExcludeArg(args["exclude"])
	if err != nil {
		return "", err
	}
	for _, ex := range exclude {
		values.Add("exclude", ex)
	}
	if encoded := values.Encode(); encoded != "" {
		return "/api/logs/read?" + encoded, nil
	}
	return "/api/logs/read", nil
}

func logLinesArg(v any) (int, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case float64:
		if x != float64(int(x)) {
			return 0, fmt.Errorf("lines must be a whole number")
		}
		return int(x), nil
	case int:
		return x, nil
	case string:
		if strings.TrimSpace(x) == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0, fmt.Errorf("lines must be a whole number, got %q", x)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("lines must be a whole number")
	}
}

func logExcludeArg(v any) ([]string, error) {
	var out []string
	switch x := v.(type) {
	case nil:
	case string:
		if s := strings.TrimSpace(x); s != "" {
			out = append(out, s)
		}
	case []string:
		for _, s := range x {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case []any:
		for _, item := range x {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("exclude must be a list of strings")
			}
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	default:
		return nil, fmt.Errorf("exclude must be a string or a list of strings")
	}
	return out, nil
}

func logReadSchema() map[string]any {
	return map[string]any{
		"source": map[string]any{"type": "string", "description": "Log source name from log_sources; default \"hive\" (the hive's own log). An unknown name returns status not_available, not an error."},
		"lines":  map[string]any{"type": "integer", "minimum": 1, "description": fmt.Sprintf("Newest matching lines to return (default %d, at most %d; a larger value is clamped and lines_clamped is set). The %d KiB result cap can return fewer, with byte_capped set.", logread.DefaultLines, logread.MaxLines, logread.MaxResultBytes/1024)},
		"since":  map[string]any{"type": "string", "format": "date-time", "description": "RFC 3339 start of the window with an explicit offset (2026-01-02T15:04:05Z or ...-05:00); a value without an offset is refused."},
		"until":  map[string]any{"type": "string", "format": "date-time", "description": "RFC 3339 end of the window with an explicit offset."},
		"min_level": map[string]any{
			"type":        "string",
			"enum":        []string{"debug", "info", "warn", "error"},
			"description": "Drop lines below this level. Lines with no level are dropped too and counted in unlevelled_dropped.",
		},
		"include": map[string]any{"type": "string", "description": "Case-insensitive literal text a line must contain, matched against the scrubbed line."},
		"exclude": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Case-insensitive literal texts; a scrubbed line containing any of them is dropped.",
		},
		"cursor": map[string]any{"type": "string", "description": "next_cursor from the previous answer, to read the next older page. Follow it also after a status incomplete answer, which stopped on its scan bound before covering the request. A cursor that can no longer be honoured returns status cursor_expired."},
	}
}
