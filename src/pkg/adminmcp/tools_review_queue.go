package adminmcp

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ToolReviewQueue answers "give me things to review" (#10019): one merged,
// prioritised list of the issues and pull requests that need a human now,
// each row carrying a one-line reason. It reads the same two Overview
// endpoints as issues_by_band / prs_by_band and computes the queue here, so
// both admin MCP servers return the same order for the same hive state. It is
// read-only and never merges anything; queue-automerge stays excluded.
const ToolReviewQueue = "review_queue"

// Review-queue reason codes, in priority order (see reviewQueueOrdering).
const (
	ReviewReasonHold             = "hold"
	ReviewReasonNeedsHuman       = "needs_human"
	ReviewReasonSweepOutstanding = "sweep_outstanding"
	ReviewReasonConfirmClose     = "confirm_close"
	ReviewReasonAgentFiled       = "agent_filed"
)

var reviewReasonOrder = []string{
	ReviewReasonHold,
	ReviewReasonNeedsHuman,
	ReviewReasonSweepOutstanding,
	ReviewReasonConfirmClose,
	ReviewReasonAgentFiled,
}

const reviewQueueOrdering = "Rows are ordered by reason priority — hold (held, waiting on a maintainer to release it), needs_human (labelled for a human decision), sweep_outstanding (PR whose merge-sweep verdict is outstanding), confirm_close (issue an agent marked done that a human must verify and close), agent_filed (agent-filed issue with no approved direction) — then oldest updated_at first, then pull requests before issues, then repo and number ascending. Items in no listed state (unclaimed, claimed, merge-eligible, blocked, draft, ordinary open) are not in the queue: they need no human yet."

// ReviewQueueReadPaths returns the two GET requests the review_queue tool
// needs: the Overview issues and pull-request exports, unfiltered except for
// the optional repo, so every band is classified by the hive's own rules.
func ReviewQueueReadPaths(args map[string]any) (issuesPath, prsPath string) {
	scoped := map[string]any{}
	if repo := strings.TrimSpace(stringArg(args, "repo")); repo != "" {
		scoped["repo"] = repo
	}
	return BandReadPath(ToolIssuesByBand, scoped), BandReadPath(ToolPrsByBand, scoped)
}

type reviewQueueRow struct {
	Kind       string `json:"kind"`
	Repo       string `json:"repo"`
	Number     any    `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"url,omitempty"`
	Band       string `json:"band"`
	ReasonCode string `json:"reason_code"`
	Reason     string `json:"reason"`
	UpdatedAt  string `json:"updated_at,omitempty"`

	priority int
	updated  time.Time
}

// ReviewQueueResult merges decoded GET /api/overview/issues.json and
// /api/overview/prs.json responses into the review_queue answer: the rows
// needing a human, ordered as reviewQueueOrdering documents, paged by the
// tool's offset/limit with truncation disclosed the way BandReadResult does.
func ReviewQueueResult(issues, prs any, args map[string]any) (any, error) {
	offset, err := reviewQueueOffset(args)
	if err != nil {
		return RefusalData{Type: "refusal", Operation: ToolReviewQueue, Kind: RefusalKindInvalidArgument, Reason: err.Error()}, nil
	}
	var queue []reviewQueueRow
	for _, row := range overviewRows(issues) {
		if r, ok := reviewQueueIssueRow(row); ok {
			queue = append(queue, r)
		}
	}
	for _, row := range overviewRows(prs) {
		if r, ok := reviewQueuePRRow(row); ok {
			queue = append(queue, r)
		}
	}
	sort.SliceStable(queue, func(i, j int) bool { return reviewQueueLess(queue[i], queue[j]) })

	counts := make(map[string]int, len(reviewReasonOrder))
	for _, code := range reviewReasonOrder {
		counts[code] = 0
	}
	for _, r := range queue {
		counts[r.ReasonCode]++
	}
	limit := LimitFromArgs(args)
	total := len(queue)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	page := make([]reviewQueueRow, 0, end-start)
	page = append(page, queue[start:end]...)
	out := map[string]any{
		"type":     ToolReviewQueue,
		"ordering": reviewQueueOrdering,
		"total":    total,
		"offset":   offset,
		"limit":    limit,
		"counts":   counts,
		"rows":     page,
	}
	if end < total {
		out["next_offset"] = end
		out["rows_truncated"] = true
		out["_admin_mcp"] = map[string]any{
			"truncated":        true,
			"limit":            limit,
			"total":            total,
			"truncated_fields": []string{"rows"},
		}
	}
	return out, nil
}

func overviewRows(data any) []map[string]any {
	body, ok := rekeyBandRows(data).(map[string]any)
	if !ok {
		return nil
	}
	raw, _ := body["rows"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func reviewQueueIssueRow(row map[string]any) (reviewQueueRow, bool) {
	band, _ := row["band"].(string)
	code, reason := "", ""
	switch {
	case row["held"] == true:
		code, reason = ReviewReasonHold, holdReason(row)
	case band == "waiting":
		code, reason = ReviewReasonNeedsHuman, "labelled for a human decision — a human must unblock or decide before agents continue"
	case band == "done":
		code, reason = ReviewReasonConfirmClose, "confirm-and-close candidate — an agent marked it done or a merged PR references it; verify the work landed, then close"
	case band == "agent-filed":
		code, reason = ReviewReasonAgentFiled, "agent-filed with no approved direction — add approved-direction, assign a human, or close it"
	default:
		return reviewQueueRow{}, false
	}
	return newReviewQueueRow("issue", band, code, reason, row), true
}

func reviewQueuePRRow(row map[string]any) (reviewQueueRow, bool) {
	band, _ := row["band"].(string)
	verdict, _ := row["merge_verdict"].(string)
	code, reason := "", ""
	switch {
	case row["held"] == true:
		code, reason = ReviewReasonHold, holdReason(row)
	case band == "waiting":
		code, reason = ReviewReasonNeedsHuman, "labelled for a human gate — a human must review or decide before automation continues"
	case band == "in-review" && strings.HasPrefix(verdict, "outstanding"):
		code, reason = ReviewReasonSweepOutstanding, "sweep verdict outstanding — "+verdict
	default:
		return reviewQueueRow{}, false
	}
	return newReviewQueueRow("pr", band, code, reason, row), true
}

func holdReason(row map[string]any) string {
	if why, _ := row["hold_reason"].(string); strings.TrimSpace(why) != "" {
		return "on hold, waiting on a maintainer to release it — " + strings.TrimSpace(why)
	}
	return "on hold, waiting on a maintainer to release it"
}

func newReviewQueueRow(kind, band, code, reason string, row map[string]any) reviewQueueRow {
	r := reviewQueueRow{Kind: kind, Band: band, ReasonCode: code, Reason: reason, Number: row["number"]}
	r.Repo, _ = row["repo"].(string)
	r.Title, _ = row["title"].(string)
	r.URL, _ = row["url"].(string)
	r.UpdatedAt, _ = row["updated_at"].(string)
	if t, err := time.Parse(time.RFC3339Nano, r.UpdatedAt); err == nil {
		r.updated = t
	}
	for i, c := range reviewReasonOrder {
		if c == code {
			r.priority = i
		}
	}
	return r
}

func reviewQueueLess(a, b reviewQueueRow) bool {
	if a.priority != b.priority {
		return a.priority < b.priority
	}
	// Unknown activity sorts after every known timestamp, so an item missing
	// updated_at never jumps ahead of one that has been waiting a known time.
	if a.updated.IsZero() != b.updated.IsZero() {
		return !a.updated.IsZero()
	}
	if !a.updated.Equal(b.updated) {
		return a.updated.Before(b.updated)
	}
	if a.Kind != b.Kind {
		return a.Kind == "pr"
	}
	if a.Repo != b.Repo {
		return a.Repo < b.Repo
	}
	return rowNumber(a.Number) < rowNumber(b.Number)
}

func rowNumber(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return 0
	}
}

func reviewQueueOffset(args map[string]any) (int, error) {
	offset := 0
	switch v := args["offset"].(type) {
	case nil:
	case float64:
		offset = int(v)
	case int:
		offset = v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("offset must be a non-negative integer, got %q", v)
		}
		offset = n
	default:
		return 0, fmt.Errorf("offset must be a non-negative integer")
	}
	if offset < 0 {
		return 0, fmt.Errorf("offset must be a non-negative integer, got %d", offset)
	}
	return offset, nil
}

func reviewQueueSchema() map[string]any {
	return map[string]any{
		"repo":   map[string]any{"type": "string", "description": "owner/name; unset reads every repo this hive tracks."},
		"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": MaxResultLimit},
		"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Rows to skip; pass the previous answer's next_offset to read the next page."},
	}
}
