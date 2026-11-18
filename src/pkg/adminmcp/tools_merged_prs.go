package adminmcp

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// merged_prs_read reads the dashboard's merged ledger over a window
// (GET /api/merged-prs): every PR the hive tracked that reached merged —
// by the hive, a person or another bot — kept 90 days. Before it the MCP
// reached merges only through stage_tracker_read's recently_merged, which
// covers the last 30 minutes.

// ToolMergedPRsRead is the merged-ledger read tool.
const ToolMergedPRsRead = "merged_prs_read"

const mergedPRsPath = "/api/merged-prs"

// mergedPRsMaxLimit mirrors the endpoint's page ceiling.
const mergedPRsMaxLimit = 500

const mergedPRsReadDescription = "Read the PRs this hive saw merge, from the dashboard's merged ledger (GET /api/merged-prs, the Activity monitor's Merged list): every pull request the hive tracked that reached merged, by the hive, a person or another bot, kept 90 days. since is an RFC3339 time or a duration back from now (24h, 7d; default 24h, at most 90d). Filters: repo, merged_by (hive, human = any login other than the hive, or a login), lane, author, exclude_author (comma-separated logins), approved (human or none). Each entry has repo, number, title, url, merged_at, merged_by, lane, issue, author, author_kind (agent, bot or human) and approved_by with approvals_known. approved_by lists only the human approvals the hive recorded on the merged head (a trusted merger's Approve that released a human gate, an operator's Approve that admitted the head), not every Approve review. Entries are newest first; counts {total, by_hive, by_human, unknown} cover the window after repo and lane, before the other filters. An entry recorded before author and approvals were kept is left out of those filters and counted in author_unknown or approvals_unknown. Example: merged_by=hive, approved=none, exclude_author=<operator> lists what the hive merged on its own in the last day. Pages with limit (1-500, default 50) and offset (next_offset). Read-only."

func mergedPRsReadSchema() map[string]any {
	return map[string]any{
		"since":          map[string]any{"type": "string", "description": "RFC3339 time, or a duration back from now such as 24h, 90m or 7d. Default 24h, at most 90d (the ledger's retention)."},
		"repo":           map[string]any{"type": "string", "description": "owner/name or the bare repo name; unset reads every repo."},
		"merged_by":      map[string]any{"type": "string", "description": "hive (the hive merged it), human (any login other than the hive) or a specific login."},
		"lane":           map[string]any{"type": "string", "description": "The tracker lane the PR merged from (issue, hive, contributor)."},
		"author":         map[string]any{"type": "string", "description": "Only PRs by this login."},
		"exclude_author": map[string]any{"type": "string", "description": "Comma-separated logins whose PRs are left out (for example the operator's own)."},
		"approved":       map[string]any{"type": "string", "enum": []string{"human", "none"}, "description": "human: a human approval the hive recorded on the merged head; none: no such approval recorded."},
		"limit":          map[string]any{"type": "integer", "minimum": 1, "maximum": mergedPRsMaxLimit, "description": "Entries to return, 1-500 (default 50)."},
		"offset":         map[string]any{"type": "integer", "minimum": 0, "description": "Entries to skip; pass the previous answer's next_offset to read the next page."},
	}
}

// MergedPRsReadPath is the one GET merged_prs_read makes. A bad argument is
// refused with its reason before any request is made.
func MergedPRsReadPath(args map[string]any) (string, error) {
	return mergedPRsReadPathAt(args, time.Now().UTC())
}

func mergedPRsReadPathAt(args map[string]any, now time.Time) (string, error) {
	values := url.Values{}
	if raw, ok := args["since"]; ok && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("since must be a string")
		}
		s = strings.TrimSpace(s)
		if s != "" {
			if err := checkMergedPRsSince(s, now); err != nil {
				return "", err
			}
			values.Set("since", s)
		}
	}
	for _, key := range []string{"repo", "merged_by", "lane", "author", "exclude_author", "approved"} {
		raw, ok := args[key]
		if !ok || raw == nil {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("%s must be a string", key)
		}
		if s = strings.TrimSpace(s); s != "" {
			values.Set(key, s)
		}
	}
	if a := values.Get("approved"); a != "" && a != "human" && a != "none" {
		return "", fmt.Errorf("approved must be human or none, got %q", a)
	}
	for _, key := range []string{"limit", "offset"} {
		n, ok, err := optionalIntArg(args, key)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		if key == "limit" && (n < 1 || n > mergedPRsMaxLimit) {
			return "", fmt.Errorf("limit must be 1-%d", mergedPRsMaxLimit)
		}
		if key == "offset" && n < 0 {
			return "", fmt.Errorf("offset must be 0 or more")
		}
		values.Set(key, strconv.Itoa(n))
	}
	if len(values) == 0 {
		return mergedPRsPath, nil
	}
	return mergedPRsPath + "?" + values.Encode(), nil
}

// checkMergedPRsSince mirrors the endpoint's since rules so a bad window is
// refused before the call.
func checkMergedPRsSince(s string, now time.Time) error {
	const retention = 90 * 24 * time.Hour
	var since time.Time
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		since = t
	} else {
		var d time.Duration
		if strings.HasSuffix(s, "d") {
			n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
			if err != nil {
				return fmt.Errorf("since must be an RFC3339 time or a duration such as 24h or 7d, got %q", s)
			}
			d = time.Duration(n) * 24 * time.Hour
		} else if d, err = time.ParseDuration(s); err != nil {
			return fmt.Errorf("since must be an RFC3339 time or a duration such as 24h or 7d, got %q", s)
		}
		if d <= 0 {
			return fmt.Errorf("since duration must be positive, got %q", s)
		}
		since = now.Add(-d)
	}
	if since.After(now) {
		return fmt.Errorf("since must not be in the future")
	}
	if now.Sub(since) > retention {
		return fmt.Errorf("since must be within the ledger's 90 days of retention")
	}
	return nil
}
