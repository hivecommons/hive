package adminmcp

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ToolReviewQueue answers "give me things to review" (#10019, #11569): the
// issues and pull requests that are the operator's turn, as the dashboard's
// "Your turn" inbox lists them. It reads the same two Overview exports as
// issues_by_band / prs_by_band and filters them to the rows whose stored
// ownership decision says your_turn, in the server's inbox_rank order. It
// never recomputes the decision from labels or bands, so the inbox, this tool
// and the owner advice cannot disagree. It is read-only and never merges
// anything; queue-automerge stays excluded.
const ToolReviewQueue = "review_queue"

// reviewVerbClose is the verb whose items the inbox header counts as "to
// close" (M) rather than "your turn" (N).
const reviewVerbClose = "CLOSE"

const reviewQueueOrdering = "Rows are exactly the your-turn items (the dashboard inbox's N plus M), including parked issues, in the inbox's order (inbox_rank): INSPECT, DECIDE, STEER, SPEC, REVIEW, TRIAGE, ADMIT, then CLOSE; within a verb, least recently active first, then repository name, then item number. Each row carries the server's verb, reason_code and reason. counts.your_turn is the inbox header's N (every verb except CLOSE) and counts.to_close its M. With repo set, only that repository's rows are listed; inbox_rank still names the position across every repository."

// ReviewQueueReadPaths returns the two GET requests the review_queue tool
// needs: the Overview issues and pull-request exports, unfiltered except for
// the optional repo, so every row carries the server's ownership decision.
func ReviewQueueReadPaths(args map[string]any) (issuesPath, prsPath string) {
	scoped := map[string]any{}
	if repo := strings.TrimSpace(stringArg(args, "repo")); repo != "" {
		scoped["repo"] = repo
	}
	return BandReadPath(ToolIssuesByBand, scoped), BandReadPath(ToolPrsByBand, scoped)
}

// ErrReviewQueueSnapshotChanged means the issues and pull-request exports came
// from different status snapshots on both attempts, so merging them could mix
// inbox ranks and ownership decisions from two moments.
var ErrReviewQueueSnapshotChanged = errors.New("review_queue: the hive status snapshot changed while the issue and pull-request exports were being read; retry the call")

// ReadReviewQueue runs the review_queue read for a provider: fetch GETs one
// export path and returns its decoded JSON. The two exports are read
// separately and each reads the status snapshot on its own, so a refresh
// between them could pair rows from different snapshots. When their snapshot
// identities differ both are read again once; a second mismatch is
// ErrReviewQueueSnapshotChanged.
func ReadReviewQueue(fetch func(path string) (any, error), args map[string]any) (any, error) {
	issuesPath, prsPath := ReviewQueueReadPaths(args)
	for range 2 {
		issues, err := fetch(issuesPath)
		if err != nil {
			return nil, err
		}
		prs, err := fetch(prsPath)
		if err != nil {
			return nil, err
		}
		out, err := ReviewQueueResult(issues, prs, args)
		if errors.Is(err, ErrReviewQueueSnapshotChanged) {
			continue
		}
		return out, err
	}
	return nil, ErrReviewQueueSnapshotChanged
}

// exportSnapshot names the status snapshot an export was read from: the
// instance, sequence and timestamp the export carries. An export from a hive
// that predates these fields yields an empty identity and so never reports a
// mismatch against another such export.
func exportSnapshot(data any) string {
	body, _ := data.(map[string]any)
	instance, _ := body["snapshot_instance"].(string)
	at, _ := body["snapshot_at"].(string)
	return fmt.Sprintf("%s/%v/%s", instance, body["snapshot_seq"], at)
}

type reviewQueueRow struct {
	Kind       string `json:"kind"`
	Repo       string `json:"repo"`
	Number     any    `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"url,omitempty"`
	Band       string `json:"band"`
	Verb       string `json:"verb"`
	ReasonCode string `json:"reason_code"`
	Reason     string `json:"reason"`
	InboxRank  int    `json:"inbox_rank"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

// ReviewQueueResult merges decoded GET /api/overview/issues.json and
// /api/overview/prs.json responses into the review_queue answer: the your-turn
// rows in inbox_rank order, paged by the tool's offset/limit with truncation
// disclosed the way BandReadResult does.
func ReviewQueueResult(issues, prs any, args map[string]any) (any, error) {
	if exportSnapshot(issues) != exportSnapshot(prs) {
		return nil, ErrReviewQueueSnapshotChanged
	}
	offset, err := OffsetFromArgs(args)
	if err != nil {
		return RefusalData{Type: "refusal", Operation: ToolReviewQueue, Kind: RefusalKindInvalidArgument, Reason: err.Error()}, nil
	}
	var queue []reviewQueueRow
	for _, row := range overviewRows(issues) {
		if r, ok := reviewQueueRowOf("issue", row); ok {
			queue = append(queue, r)
		}
	}
	for _, row := range overviewRows(prs) {
		if r, ok := reviewQueueRowOf("pr", row); ok {
			queue = append(queue, r)
		}
	}
	sort.SliceStable(queue, func(i, j int) bool { return queue[i].InboxRank < queue[j].InboxRank })

	verbs := map[string]int{}
	toClose := 0
	for _, r := range queue {
		verbs[r.Verb]++
		if r.Verb == reviewVerbClose {
			toClose++
		}
	}
	counts := map[string]int{"your_turn": len(queue) - toClose, "to_close": toClose}
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
		"verbs":    verbs,
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

// reviewQueueRowOf lifts an export row onto the queue when its stored
// decision says your turn; every field is the server's, none is derived here.
func reviewQueueRowOf(kind string, row map[string]any) (reviewQueueRow, bool) {
	if yours, _ := row["your_turn"].(bool); !yours {
		return reviewQueueRow{}, false
	}
	r := reviewQueueRow{Kind: kind, Number: row["number"], InboxRank: int(rowNumber(row["inbox_rank"]))}
	r.Repo, _ = row["repo"].(string)
	r.Title, _ = row["title"].(string)
	r.URL, _ = row["url"].(string)
	r.Band, _ = row["band"].(string)
	r.Verb, _ = row["verb"].(string)
	r.ReasonCode, _ = row["reason_code"].(string)
	r.Reason, _ = row["reason"].(string)
	r.UpdatedAt, _ = row["updated_at"].(string)
	return r, true
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

func reviewQueueSchema() map[string]any {
	return map[string]any{
		"repo":   map[string]any{"type": "string", "description": "owner/name; unset reads every repo this hive tracks."},
		"limit":  limitSchema(),
		"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Rows to skip; pass the previous answer's next_offset to read the next page."},
	}
}
