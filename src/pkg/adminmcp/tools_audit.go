package adminmcp

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// audit_log filters and item_history_read. audit_log used to read only the
// newest page of the in-memory audit ring; with repo, number, action, since,
// contributor or sensitive it asks GET /api/audit to search every retained
// audit file and filter BEFORE its 200-row cap, so the tool's own limit cap
// never hides a match a narrower query would have shown. item_history_read
// answers one item, open or closed: GET /api/audit/item, which also backs
// the dashboard's item history dialog.

// ToolItemHistoryRead is the per-item history tool.
const ToolItemHistoryRead = "item_history_read"

const auditLogDescription = "Read this hive's audit log through GET /api/audit, newest first. Without filters it is the newest page of the in-memory log. With repo (owner/name or the bare name), number (an issue or PR), action (comma-separated names; a trailing * matches a prefix, e.g. relay_*), since (RFC3339, or a duration back from now such as 24h or 7d, at most 90 days), contributor (a login) or sensitive=true, the hive searches every retained audit file and filters before its 200-row cap; the answer then carries matched (how many rows matched), truncated and scanned (files, or ring on a hive without audit files). entries is capped by limit. Read-only."

const itemHistoryReadDescription = "Read everything the hive recorded about one issue or PR, open or closed, through GET /api/audit/item: state (open, merged, closed or unknown), its audit entries newest first (every retained row naming this repo and number, plus the rows of the issue the PR was nested under, where an agent's claim is recorded; each row's target says which item), merged (the merged-ledger entry: merged_at, merged_by = hive or a login, lane, issue) when it merged in the last 90 days, tracker (its Activity monitor row) while it is still open, and closed (the monitor's record) for 30 minutes after it closed. Use it for a PR that has left the board. Read-only."

func auditLogSchema() map[string]any {
	return map[string]any{
		"limit":       limitSchema(),
		"repo":        map[string]any{"type": "string", "description": "owner/name, or the bare repository name."},
		"number":      map[string]any{"type": "integer", "minimum": 1, "description": "An issue or PR number."},
		"action":      map[string]any{"type": "string", "description": "Action name(s), comma-separated; a trailing * matches a prefix (pr_*, relay_*)."},
		"since":       map[string]any{"type": "string", "description": "RFC3339, or a duration back from now (24h, 7d); at most 90 days."},
		"contributor": map[string]any{"type": "string", "description": "Only rows whose user is this login."},
		"sensitive":   map[string]any{"type": "boolean", "description": "Only sensitive actions; without since, the last 24 hours."},
	}
}

func itemHistoryReadSchema() (map[string]any, []string) {
	return map[string]any{
		"repo":   map[string]any{"type": "string", "minLength": 1, "description": "owner/name (a bare name is qualified with the hive's org)."},
		"number": map[string]any{"type": "integer", "minimum": 1, "description": "The issue or PR number."},
	}, []string{"repo", "number"}
}

// auditNumberArg reads an integer argument that must be positive when set.
func auditNumberArg(args map[string]any, name string) (int, bool, error) {
	switch v := args[name].(type) {
	case nil:
		return 0, false, nil
	case float64:
		if v != float64(int(v)) || v <= 0 {
			return 0, false, fmt.Errorf("%s must be a positive integer", name)
		}
		return int(v), true, nil
	case int:
		if v <= 0 {
			return 0, false, fmt.Errorf("%s must be a positive integer", name)
		}
		return v, true, nil
	case string:
		s := strings.TrimPrefix(strings.TrimSpace(v), "#")
		if s == "" {
			return 0, false, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return 0, false, fmt.Errorf("%s must be a positive integer, got %q", name, v)
		}
		return n, true, nil
	default:
		return 0, false, fmt.Errorf("%s must be a positive integer", name)
	}
}

// AuditLogReadPath is the GET audit_log makes: /api/audit, with the filters
// as query parameters. since is checked by the endpoint, which answers a bad
// one with its reason.
func AuditLogReadPath(args map[string]any) (string, error) {
	values := url.Values{}
	for _, name := range []string{"repo", "action", "since", "contributor"} {
		if v := strings.TrimSpace(stringArg(args, name)); v != "" {
			values.Set(name, v)
		}
	}
	n, ok, err := auditNumberArg(args, "number")
	if err != nil {
		return "", err
	}
	if ok {
		values.Set("number", strconv.Itoa(n))
	}
	switch v := args["sensitive"].(type) {
	case nil:
	case bool:
		if v {
			values.Set("sensitive", "1")
		}
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err != nil {
			return "", fmt.Errorf("sensitive must be true or false, got %q", v)
		} else if b {
			values.Set("sensitive", "1")
		}
	default:
		return "", fmt.Errorf("sensitive must be true or false")
	}
	if len(values) == 0 {
		return "/api/audit", nil
	}
	return "/api/audit?" + values.Encode(), nil
}

// ItemHistoryReadPath is the GET item_history_read makes.
func ItemHistoryReadPath(args map[string]any) (string, error) {
	repo := strings.TrimSpace(stringArg(args, "repo"))
	if repo == "" {
		return "", fmt.Errorf("repo is required (owner/name)")
	}
	n, ok, err := auditNumberArg(args, "number")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("number is required")
	}
	values := url.Values{}
	values.Set("repo", repo)
	values.Set("number", strconv.Itoa(n))
	return "/api/audit/item?" + values.Encode(), nil
}
