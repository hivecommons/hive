package adminmcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The Activity monitor over admin MCP: stage_tracker_read reads the board the
// dashboard's Activity monitor draws (GET /api/stage-tracker), and the
// repository.item_* write ops are the monitor's row actions, each the same
// request the dashboard's button sends (tracker-actions.js
// stageTrackerActionRequest). The merge actions ("Approve and let hive merge",
// "Merge now as maintainer") are not offered: they are the queue-automerge
// exclusion in another form.
const (
	ToolStageTrackerRead         = "stage_tracker_read"
	ToolDelegatedDecisionStatus  = "delegated_decision_status"
	stageTrackerViewSummary      = "summary"
	stageTrackerViewFull         = "full"
	stageTrackerPath             = "/api/stage-tracker"
	stageTrackerDecisionMaxNotes = 4000

	// RefusalKindBoardUnavailable is stage_tracker_read's answer while the
	// Activity monitor has no snapshot to read.
	RefusalKindBoardUnavailable = "board_unavailable"

	WriteOpRepositoryRescan               = "repository.rescan"
	WriteOpRepositoryItemAcceptTriage     = "repository.item_accept_triage"
	WriteOpRepositoryItemNeedsHuman       = "repository.item_needs_human"
	WriteOpRepositoryItemBlockerRecheck   = "repository.item_blocker_recheck"
	WriteOpRepositoryItemBlockerHandoff   = "repository.item_blocker_handoff"
	WriteOpRepositoryItemBlockerClosePart = "repository.item_blocker_close_partial"
	WriteOpRepositoryItemDecision         = "repository.item_decision"
	WriteOpRepositoryItemDelegate         = "repository.item_delegate"
)

// stageTrackerSummaryKeys are the fields a summary row keeps: what the
// monitor's row shows (stage, wait, decision, links, blocker) without the
// activity strips, history and Sentinel detail. view=full keeps every field.
var stageTrackerSummaryKeys = []string{
	"repo", "number", "kind", "title", "url", "author", "author_kind",
	"stage", "stage_slug", "stage_label", "stage_since", "reason", "caption",
	"lane", "actor", "finisher", "finisher_reason", "claimant", "modifiers", "labels",
	"needs_human", "on_hold", "hold", "waiting_on_you", "wait_kind", "human_attention",
	"blocked_on", "blocked_on_pr", "blocker", "decision_needed",
	"linked_prs", "linked_issues", "nested_under", "superseded_by",
	"sentinel_alert", "sentinel_reason", "merge_gate", "soak_remaining_seconds",
	"regressed_from", "regressed_to", "regressed_reason",
	"last_operator_action", "decided", "updated_at",
}

// stageTrackerDecisions are the Decide answers repository.item_decision
// sends, mapped to the dashboard's request. merge-now and queue-merge are
// absent on purpose.
var stageTrackerDecisions = []string{
	"approve", "direction", "close-superseded", "close-wontfix", "close-read",
	"close-issue", "adopt", "adopt-now", "re-author", "reroute",
}

var stageTrackerMergeDecisions = []string{"merge-now", "queue-merge"}

func stageTrackerReadSchema() map[string]any {
	return map[string]any{
		"limit":          limitSchema(),
		"offset":         map[string]any{"type": "integer", "minimum": 0, "description": "Items to skip; pass the previous answer's next_offset to read the next page."},
		"repo":           map[string]any{"type": "string", "description": "owner/name; unset reads every repo on the board."},
		"kind":           map[string]any{"type": "string", "enum": []string{"issue", "pr"}},
		"number":         map[string]any{"type": "integer", "minimum": 1, "description": "Only the item with this number (pair with repo when several repos share it)."},
		"q":              map[string]any{"type": "string", "description": "The Activity monitor's search box: a number (\"488\" or \"#488\") matches the item or a linked PR or issue with that number; other text matches titles."},
		"stage":          map[string]any{"type": "integer", "minimum": 0, "description": "Only items at this stage number (stages[] lists them)."},
		"needs_human":    map[string]any{"type": "boolean"},
		"on_hold":        map[string]any{"type": "boolean"},
		"waiting_on_you": map[string]any{"type": "boolean", "description": "Items parked on the operator or maintainers (a decision, a human merge, an approval), as the monitor's \"waiting on you\" view; wait_kind says which."},
		"view":           map[string]any{"type": "string", "enum": []string{stageTrackerViewSummary, stageTrackerViewFull}, "description": "summary (default) keeps the row's fields; full keeps every field, history and activity included."},
		"recent":         map[string]any{"type": "boolean", "description": "Also return recently_merged and recently_closed, capped by limit."},
	}
}

func delegatedDecisionStatusSchema() map[string]any {
	return itemTargetProps()
}

// StageTrackerReadPath is the request stage_tracker_read makes. Filters are
// applied by StageTrackerReadResult so the stage counts always describe the
// whole board.
func StageTrackerReadPath() string { return stageTrackerPath }

// DelegatedDecisionStatusPath is the GET a "Let hive decide" job is polled at.
func DelegatedDecisionStatusPath(args map[string]any) (string, error) {
	owner, repo, number, err := itemTargetArgs(args)
	if err != nil {
		return "", err
	}
	return itemPath(owner, repo, number, "decide-delegated"), nil
}

// StageTrackerReadResult filters, trims and pages a decoded GET
// /api/stage-tracker answer for stage_tracker_read.
func StageTrackerReadResult(data any, args map[string]any) (any, error) {
	refuse := func(reason string) (any, error) {
		return RefusalData{Type: "refusal", Operation: ToolStageTrackerRead, Kind: RefusalKindInvalidArgument, Reason: reason}, nil
	}
	body, ok := data.(map[string]any)
	if !ok {
		return CapResult(data, LimitFromArgs(args)), nil
	}
	// Before its first snapshot the endpoint answers {"status":"initializing"}
	// with no items: that is "not built yet", never an empty board.
	if _, hasItems := body["items"]; !hasItems {
		status := stringField(body, "status")
		if status == "" {
			status = "unavailable"
		}
		return RefusalData{Type: "refusal", Operation: ToolStageTrackerRead, Kind: RefusalKindBoardUnavailable, Reason: "the Activity monitor is " + status + "; retry shortly"}, nil
	}
	offset, err := OffsetFromArgs(args)
	if err != nil {
		return refuse(err.Error())
	}
	kind := strings.ToLower(stringArg(args, "kind"))
	if kind != "" && kind != "issue" && kind != "pr" {
		return refuse(fmt.Sprintf("unknown kind %q; valid kinds: issue, pr", kind))
	}
	view := strings.ToLower(stringArg(args, "view"))
	if view == "" {
		view = stageTrackerViewSummary
	}
	if view != stageTrackerViewSummary && view != stageTrackerViewFull {
		return refuse(fmt.Sprintf("unknown view %q; valid views: summary, full", view))
	}
	number, hasNumber, err := optionalNonNegativeIntArg(args, "number")
	if err != nil {
		return refuse(err.Error())
	}
	stage, hasStage, err := optionalNonNegativeIntArg(args, "stage")
	if err != nil {
		return refuse(err.Error())
	}
	repo := stringArg(args, "repo")
	q := strings.ToLower(stringArg(args, "q"))
	items, _ := body["items"].([]any)
	matched := make([]any, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if repo != "" && !strings.EqualFold(stringField(item, "repo"), repo) {
			continue
		}
		if kind != "" && stringField(item, "kind") != kind {
			continue
		}
		if hasNumber && intField(item, "number") != number {
			continue
		}
		if hasStage && intField(item, "stage") != stage {
			continue
		}
		if q != "" && !stageTrackerQueryMatches(item, q) {
			continue
		}
		if !boolFilterMatches(args, item, "needs_human") || !boolFilterMatches(args, item, "on_hold") || !boolFilterMatches(args, item, "waiting_on_you") {
			continue
		}
		if view == stageTrackerViewSummary {
			item = pickKeys(item, stageTrackerSummaryKeys)
		}
		matched = append(matched, item)
	}
	limit := LimitFromArgs(args)
	total := len(matched)
	start := min(offset, total)
	end := min(start+limit, total)
	out := map[string]any{
		"generated_at": body["generated_at"],
		"source":       body["source"],
		"stages":       stageTrackerStageCounts(body["stages"], items),
		"items":        matched[start:end],
		"total_items":  len(items),
		"matched":      total,
		"offset":       offset,
		"view":         view,
	}
	for _, k := range []string{"hive_id", "open_issues", "open_prs", "needs_human", "on_hold", "waiting_on_human"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	if recent, _ := args["recent"].(bool); recent {
		for _, k := range []string{"recently_merged", "recently_closed"} {
			if list, ok := body[k].([]any); ok {
				out[k] = list[:min(limit, len(list))]
			}
		}
	}
	if end < total {
		out["next_offset"] = end
		out["_admin_mcp"] = map[string]any{"truncated": true, "limit": limit, "total": total, "truncated_fields": []string{"items"}}
	}
	return out, nil
}

// stageTrackerQueryMatches is the monitor's search box (tracker-filter.js
// stageTrackerMatches): a number matches the item or a linked PR or issue
// by number; any text matches the item's or a linked item's title.
func stageTrackerQueryMatches(item map[string]any, q string) bool {
	num := strings.TrimPrefix(q, "#")
	_, err := strconv.Atoi(num)
	isNum := err == nil
	matches := func(m map[string]any) bool {
		if isNum && strconv.Itoa(intField(m, "number")) == num {
			return true
		}
		return strings.Contains(strings.ToLower(stringField(m, "title")), q)
	}
	if matches(item) {
		return true
	}
	for _, key := range []string{"linked_prs", "linked_issues"} {
		list, _ := item[key].([]any)
		for _, raw := range list {
			if m, ok := raw.(map[string]any); ok && matches(m) {
				return true
			}
		}
	}
	return false
}

// stageTrackerStageCounts is stages[] trimmed to what a caller needs to read
// the board, each with how many items sit at it now.
func stageTrackerStageCounts(rawStages any, items []any) []map[string]any {
	counts := map[int]int{}
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok {
			counts[intField(item, "stage")]++
		}
	}
	stages, _ := rawStages.([]any)
	out := make([]map[string]any, 0, len(stages))
	for _, raw := range stages {
		st, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		row := pickKeys(st, []string{"number", "slug", "label", "dwell_seconds"})
		row["count"] = counts[intField(st, "number")]
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return intField(out[i], "number") < intField(out[j], "number") })
	return out
}

func boolFilterMatches(args, item map[string]any, key string) bool {
	want, ok := args[key].(bool)
	if !ok {
		return true
	}
	have, _ := item[key].(bool)
	return have == want
}

func pickKeys(m map[string]any, keys []string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func stageTrackerWriteOps() []WriteOp {
	return []WriteOp{
		repositoryRescanOp{},
		itemAcceptTriageOp{},
		itemNeedsHumanOp{},
		itemBlockerOp{name: WriteOpRepositoryItemBlockerRecheck, segment: "blocker/recheck", label: "Re-check permissions", effect: "The hive re-reads the GitHub App's installation permissions and releases the item's capability blocker if the permission is now granted."},
		itemBlockerOp{name: WriteOpRepositoryItemBlockerHandoff, segment: "blocker/handoff", label: "Hand to workflows lane", effect: "The item is routed to the lane that can push workflow files (`hive/workflows-lane`) and its workflows-permission blocker is released."},
		itemBlockerOp{name: WriteOpRepositoryItemBlockerClosePart, segment: "blocker/close-partial", label: "Mark partially-addressed and close", effect: "Posts your partially-addressed note on the issue and closes it as completed.", danger: true},
		itemDecisionOp{},
		itemDelegateOp{},
	}
}

type repositoryRescanOp struct{}
type itemAcceptTriageOp struct{}
type itemNeedsHumanOp struct{}
type itemBlockerOp struct {
	name, segment, label, effect string
	danger                       bool
}
type itemDecisionOp struct{}
type itemDelegateOp struct{}

func itemTargetProps() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"owner": stringSchema(), "repo": stringSchema(), "number": map[string]any{"type": "integer", "minimum": 1}}, "required": []string{"owner", "repo", "number"}, "additionalProperties": false}
}

func itemSchema(extra map[string]any, required ...string) map[string]any {
	s := itemTargetProps()
	props := s["properties"].(map[string]any)
	for k, v := range extra {
		props[k] = v
	}
	s["required"] = append(s["required"].([]string), required...)
	return s
}

func itemTargetArgs(args map[string]any) (owner, repo string, number int, err error) {
	owner = stringArg(args, "owner")
	repo = stringArg(args, "repo")
	number, err = positiveIntArg(args, "number")
	if err != nil {
		return "", "", 0, err
	}
	if owner == "" || repo == "" {
		return "", "", 0, fmt.Errorf("owner, repo and number are required")
	}
	return owner, repo, number, nil
}

func itemPath(owner, repo string, number int, suffix string) string {
	return "/api/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/items/" + strconv.Itoa(number) + "/" + suffix
}

func itemRef(owner, repo string, number int) string {
	return owner + "/" + repo + "#" + strconv.Itoa(number)
}

// boardRow is one item as a preview found it on the Activity monitor. read is
// false when the board itself could not be read (no provider, an error, or
// no snapshot yet), so a missing item is not mistaken for one that left.
type boardRow struct {
	item map[string]any
	read bool
}

// stageTrackerItem reads one item off the Activity monitor for a preview: its
// row (full view) when it is on the board.
func stageTrackerItem(ctx context.Context, provider Provider, owner, repo string, number int) boardRow {
	if provider == nil {
		return boardRow{}
	}
	data, err := provider.Read(ctx, ToolStageTrackerRead, map[string]any{"repo": owner + "/" + repo, "number": number, "view": stageTrackerViewFull, "limit": MaxResultLimit})
	if err != nil {
		return boardRow{}
	}
	body, ok := data.(map[string]any)
	if !ok {
		return boardRow{}
	}
	items, _ := body["items"].([]any)
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok && strings.EqualFold(stringField(item, "repo"), owner+"/"+repo) && intField(item, "number") == number {
			return boardRow{item: item, read: true}
		}
	}
	return boardRow{read: true}
}

// boardDetails is what a preview says about the item as the monitor shows it,
// so the caller confirms against the state the operator would have seen.
func boardDetails(row boardRow, details map[string]any) []string {
	item := row.item
	if !row.read {
		details["on_board"] = "unknown"
		return []string{"The Activity monitor could not be read just now, so this preview cannot show the item's current state; the hive still decides whether the request applies."}
	}
	if item == nil {
		details["on_board"] = false
		return []string{"This item is not on the Activity monitor now, so the dashboard would not offer this action on it; the hive still decides whether the request applies."}
	}
	details["on_board"] = true
	for _, k := range []string{"kind", "title", "stage", "stage_slug", "reason", "wait_kind"} {
		if v, ok := item[k]; ok {
			details[k] = v
		}
	}
	if dn, ok := item["decision_needed"].(map[string]any); ok {
		if q := stringField(dn, "question"); q != "" {
			details["decision_question"] = q
		}
	}
	return nil
}

func itemKind(args map[string]any, item map[string]any) (string, error) {
	kind := strings.ToLower(stringArg(args, "type"))
	if kind == "" && item != nil {
		kind = stringField(item, "kind")
	}
	switch kind {
	case "":
		return "", fmt.Errorf("type is required (issue or pr) when the item is not on the Activity monitor or the monitor cannot be read")
	case "issue", "pr":
		return kind, nil
	}
	return "", fmt.Errorf("type must be issue or pr, got %q", kind)
}

// previewItemKind is itemKind for a preview. ConfirmWrite re-runs Preview
// with no provider and compares only the method and path, which the kind does
// not change, so there an unknown kind reads as issue; the request it executes
// is the one the provider-backed preview stored.
func previewItemKind(args map[string]any, item map[string]any, provider Provider) (string, error) {
	kind, err := itemKind(args, item)
	if err != nil && provider == nil && stringArg(args, "type") == "" {
		return "issue", nil
	}
	return kind, err
}

func (repositoryRescanOp) Name() string { return WriteOpRepositoryRescan }
func (repositoryRescanOp) Description() string {
	return "Re-evaluate now: ask the hive to rescan its repositories through POST /api/repos/rescan (the Activity monitor's \"Re-evaluate now\")."
}
func (repositoryRescanOp) InputSchema() map[string]any { return emptySchema() }
func (repositoryRescanOp) Preview(_ context.Context, _ map[string]any) (WritePreview, error) {
	return WritePreview{Operation: WriteOpRepositoryRescan, Summary: "Rescan every watched repository now", Target: "all watched repositories", Request: WriteRequest{Method: http.MethodPost, Path: "/api/repos/rescan", Body: map[string]any{}}, Effects: []string{"The hive re-reads issues and pull requests on its watched repositories now instead of on its next scheduled pass. If a rescan is already running, or one finished less than 30 seconds ago, nothing new is started: the hive answers in-progress or debounced (with retry_after_s) and the previous scan's counts, and no rescan is queued."}, WideningDisclosure: "Hive-wide: the rescan covers every watched repository, as the dashboard's button does, but changes no item by itself.", ConfirmationMessage: "Confirm rescanning the watched repositories now.", Details: map[string]any{}}, nil
}

func (itemAcceptTriageOp) Name() string { return WriteOpRepositoryItemAcceptTriage }
func (itemAcceptTriageOp) Description() string {
	return "Accept triage on one issue through POST /api/repos/{owner}/{repo}/items/{number}/accept-triage."
}
func (itemAcceptTriageOp) InputSchema() map[string]any { return itemTargetProps() }
func (op itemAcceptTriageOp) Preview(ctx context.Context, args map[string]any) (WritePreview, error) {
	return op.PreviewWithProvider(ctx, args, nil)
}
func (itemAcceptTriageOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	owner, repo, number, err := itemTargetArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	target := itemRef(owner, repo, number)
	details := map[string]any{"owner": owner, "repo": repo, "number": number}
	row := stageTrackerItem(ctx, provider, owner, repo, number)
	item := row.item
	if item != nil && stringField(item, "kind") == "pr" {
		return WritePreview{}, fmt.Errorf("%s is a pull request; triage is accepted on issues only", target)
	}
	effects := append([]string{"The issue gets the triage-accepted label and loses needs-triage; the hive takes it on its next pass."}, boardDetails(row, details)...)
	return WritePreview{Operation: WriteOpRepositoryItemAcceptTriage, Summary: "Accept triage on " + target, Target: target, Request: WriteRequest{Method: http.MethodPost, Path: itemPath(owner, repo, number, "accept-triage"), Body: map[string]any{}}, Effects: effects, WideningDisclosure: "No widening: this operation targets exactly one issue.", ConfirmationMessage: "Confirm accepting triage on " + target + ".", Details: details}, nil
}

func (itemNeedsHumanOp) Name() string { return WriteOpRepositoryItemNeedsHuman }
func (itemNeedsHumanOp) Description() string {
	return "Mark one item needs-human with a reason through POST /api/review/pipeline/{owner}/{repo}/{number}/send-to-human. Needs the merger or owner role."
}
func (itemNeedsHumanOp) InputSchema() map[string]any {
	return itemSchema(map[string]any{"reason": map[string]any{"type": "string", "minLength": 1, "maxLength": stageTrackerDecisionMaxNotes}}, "reason")
}
func (op itemNeedsHumanOp) Preview(ctx context.Context, args map[string]any) (WritePreview, error) {
	return op.PreviewWithProvider(ctx, args, nil)
}
func (itemNeedsHumanOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	owner, repo, number, err := itemTargetArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	reason, err := requiredString(args, "reason")
	if err != nil {
		return WritePreview{}, err
	}
	if len(reason) > stageTrackerDecisionMaxNotes {
		return WritePreview{}, fmt.Errorf("reason exceeds %d characters", stageTrackerDecisionMaxNotes)
	}
	target := itemRef(owner, repo, number)
	details := map[string]any{"owner": owner, "repo": repo, "number": number, "reason": reason}
	effects := append([]string{"The item gets the needs-human label with your reason; agents stop working it until a person clears the label."}, boardDetails(stageTrackerItem(ctx, provider, owner, repo, number), details)...)
	path := "/api/review/pipeline/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/" + strconv.Itoa(number) + "/send-to-human"
	return WritePreview{Operation: WriteOpRepositoryItemNeedsHuman, Summary: "Mark " + target + " needs-human", Target: target, Request: WriteRequest{Method: http.MethodPost, Path: path, Body: map[string]any{"reason": reason}}, Effects: effects, WideningDisclosure: "No widening: this operation targets exactly one repository item.", ConfirmationMessage: "Confirm marking " + target + " needs-human.", Details: details}, nil
}

func (op itemBlockerOp) Name() string { return op.name }
func (op itemBlockerOp) Description() string {
	return op.label + " on one issue's recorded blocker through POST /api/repos/{owner}/{repo}/items/{number}/" + op.segment + "."
}
func (itemBlockerOp) InputSchema() map[string]any { return itemTargetProps() }
func (op itemBlockerOp) Preview(ctx context.Context, args map[string]any) (WritePreview, error) {
	return op.PreviewWithProvider(ctx, args, nil)
}
func (op itemBlockerOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	owner, repo, number, err := itemTargetArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	target := itemRef(owner, repo, number)
	details := map[string]any{"owner": owner, "repo": repo, "number": number}
	row := stageTrackerItem(ctx, provider, owner, repo, number)
	item := row.item
	if item != nil {
		if stringField(item, "kind") == "pr" {
			return WritePreview{}, fmt.Errorf("%s is a pull request; blocker actions apply to issues only", target)
		}
		if b, ok := item["blocker"].(map[string]any); ok {
			details["blocker"] = b
		}
	}
	effects := append([]string{op.effect}, boardDetails(row, details)...)
	if op.danger {
		details["danger"] = true
	}
	return WritePreview{Operation: op.name, Summary: op.label + " on " + target, Target: target, Request: WriteRequest{Method: http.MethodPost, Path: itemPath(owner, repo, number, op.segment), Body: map[string]any{}}, Effects: effects, WideningDisclosure: "No widening: this operation targets exactly one issue's blocker.", ConfirmationMessage: "Confirm " + strings.ToLower(op.label) + " on " + target + ".", Details: details}, nil
}

func (itemDecisionOp) Name() string { return WriteOpRepositoryItemDecision }
func (itemDecisionOp) Description() string {
	return "Answer one item's decision as the Activity monitor's Decide menu does, through POST /api/repos/{owner}/{repo}/items/{number}/decision: approve, direction (needs comment), close-superseded (optional superseded_by), close-wontfix, close-read, close-issue, adopt, adopt-now, re-author, or reroute (optional to, default any). The merge answers (merge-now, queue-merge) are excluded."
}
func (itemDecisionOp) InputSchema() map[string]any {
	return itemSchema(map[string]any{
		"decision":      map[string]any{"type": "string", "enum": stageTrackerDecisions},
		"type":          map[string]any{"type": "string", "enum": []string{"issue", "pr"}, "description": "Read from the Activity monitor when omitted."},
		"comment":       map[string]any{"type": "string", "maxLength": stageTrackerDecisionMaxNotes},
		"superseded_by": map[string]any{"type": "string", "description": "close-superseded only: \"458\", \"#458\", \"owner/repo#458\" or a GitHub URL."},
		"to":            map[string]any{"type": "string", "description": "reroute only: the target clanker's login, or \"any\"."},
	}, "decision")
}
func (op itemDecisionOp) Preview(ctx context.Context, args map[string]any) (WritePreview, error) {
	return op.PreviewWithProvider(ctx, args, nil)
}
func (itemDecisionOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	owner, repo, number, err := itemTargetArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	decision := strings.ToLower(stringArg(args, "decision"))
	if slices.Contains(stageTrackerMergeDecisions, decision) {
		return WritePreview{}, fmt.Errorf("decision %q merges the pull request and is excluded from admin MCP like POST /api/prs/{owner}/{repo}/{number}/queue-automerge; do not approximate it with other operations", decision)
	}
	if !slices.Contains(stageTrackerDecisions, decision) {
		return WritePreview{}, fmt.Errorf("decision must be one of %s", strings.Join(stageTrackerDecisions, ", "))
	}
	comment, _ := args["comment"].(string)
	comment = strings.TrimSpace(comment)
	if len(comment) > stageTrackerDecisionMaxNotes {
		return WritePreview{}, fmt.Errorf("comment exceeds %d characters", stageTrackerDecisionMaxNotes)
	}
	if decision == "direction" && comment == "" {
		return WritePreview{}, fmt.Errorf("a direction decision needs the direction text in comment")
	}
	target := itemRef(owner, repo, number)
	row := stageTrackerItem(ctx, provider, owner, repo, number)
	kind, err := previewItemKind(args, row.item, provider)
	if err != nil {
		return WritePreview{}, err
	}
	if decision == "close-issue" && kind != "issue" {
		return WritePreview{}, fmt.Errorf("close-issue closes an issue; %s is a pull request", target)
	}
	body := map[string]any{"decision": decision, "comment": comment, "type": kind}
	var effect string
	danger := false
	switch decision {
	case "approve":
		effect = "The issue is unparked with your approval and the hive resumes it."
		if kind == "pr" {
			effect = "Posts `/lgtm` and `/approve` as your decision; the pull request becomes merge-eligible, and the hive's merge sweep merges it once CI is green and the head has soaked."
		}
	case "direction":
		effect = "Records your direction for the hive and unparks the item; the hive resumes with it."
	case "close-superseded":
		effect = "Closes the item as superseded, with your comment."
		if by := stringArg(args, "superseded_by"); by != "" {
			body["superseded_by"] = by
			effect = "Closes the item as superseded by " + by + ", with your comment."
		}
		danger = true
	case "close-wontfix":
		effect, danger = "Closes the item as won't-fix (not planned), with your comment.", true
	case "close-read":
		effect, danger = "Closes the advisory issue as completed, as read.", true
	case "close-issue":
		effect, danger = "Closes only this issue as completed, with your comment.", true
	case "adopt":
		effect = "Asks the hive to adopt the pull request: a hive copy on its own branch, the original closed as superseded. Author presence, the grace window and maintainer_policy still apply."
	case "adopt-now":
		body["decision"], body["force"] = "adopt", true
		effect, danger = "Adopts the pull request now, bypassing the author's presence, the grace window and maintainer_policy; the original is closed as superseded.", true
	case "re-author":
		effect = "The hive re-homes the commits on a hive-owned branch and opens a superseding pull request."
	case "reroute":
		to := stringArg(args, "to")
		if to == "" {
			to = "any"
		}
		body["to"] = to
		effect = "Hands the item to another clanker (" + to + ")."
	}
	details := map[string]any{"owner": owner, "repo": repo, "number": number, "decision": decision, "type": kind}
	if danger {
		details["danger"] = true
	}
	effects := append([]string{effect, "Recorded as your decision, attributed to your login."}, boardDetails(row, details)...)
	return WritePreview{Operation: WriteOpRepositoryItemDecision, Summary: "Decide " + decision + " on " + target, Target: target, Request: WriteRequest{Method: http.MethodPost, Path: itemPath(owner, repo, number, "decision"), Body: body}, Effects: effects, WideningDisclosure: "No widening: this operation targets exactly one repository item.", ConfirmationMessage: "Confirm deciding " + decision + " on " + target + ".", Details: details}, nil
}

func (itemDelegateOp) Name() string { return WriteOpRepositoryItemDelegate }
func (itemDelegateOp) Description() string {
	return "Let hive decide one item through POST /api/repos/{owner}/{repo}/items/{number}/decide-delegated, never merging: the request carries no_merge, so an approved pull request gets /lgtm + /approve rather than a review queued for auto-merge. Asynchronous; read delegated_decision_status for the outcome. prs (two or more) decides each of an issue's open pull requests; rerun re-runs a stuck decision."
}
func (itemDelegateOp) InputSchema() map[string]any {
	return itemSchema(map[string]any{
		"type":  map[string]any{"type": "string", "enum": []string{"issue", "pr"}, "description": "Read from the Activity monitor when omitted."},
		"prs":   map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1}, "description": "Issue only: the open pull requests to decide one by one (two or more)."},
		"rerun": map[string]any{"type": "boolean", "description": "Re-run past the \"already recorded\" check on a stuck decision."},
	})
}
func (op itemDelegateOp) Preview(ctx context.Context, args map[string]any) (WritePreview, error) {
	return op.PreviewWithProvider(ctx, args, nil)
}
func (itemDelegateOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	owner, repo, number, err := itemTargetArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	target := itemRef(owner, repo, number)
	row := stageTrackerItem(ctx, provider, owner, repo, number)
	kind, err := previewItemKind(args, row.item, provider)
	if err != nil {
		return WritePreview{}, err
	}
	body := map[string]any{"type": kind, "no_merge": true}
	// The row's own words for the park, as the dashboard sends them, so the
	// decider sees what the operator saw; head keys the plan cache.
	for _, k := range []string{"reason", "caption", "sentinel_reason"} {
		body[k] = stringField(row.item, k)
	}
	if head := stringField(row.item, "head"); head != "" {
		body["head"] = head
	}
	if raw, ok := args["prs"].([]any); ok && len(raw) > 0 {
		if kind != "issue" {
			return WritePreview{}, fmt.Errorf("prs applies to an issue's nested pull requests; %s is a pull request", target)
		}
		prs := make([]int, 0, len(raw))
		for _, v := range raw {
			n, ok := v.(float64)
			if !ok || n < 1 || n != float64(int(n)) {
				return WritePreview{}, fmt.Errorf("prs must be positive integers")
			}
			prs = append(prs, int(n))
		}
		if len(prs) < 2 {
			return WritePreview{}, fmt.Errorf("prs needs two or more pull requests; decide a single one on its own number")
		}
		body["prs"] = prs
	}
	if rerun, _ := args["rerun"].(bool); rerun {
		body["force"] = true
	}
	details := map[string]any{"owner": owner, "repo": repo, "number": number, "type": kind}
	effects := []string{
		"The hive decides for you under its delegate policy and rails, and the outcome is recorded as your decision with an audit marker.",
		"Never merges: no_merge is set, so an approved pull request gets `/lgtm` + `/approve` (merge-eligible for the hive's merge sweep) rather than an Approve review queued for auto-merge.",
		"Runs in the background; read delegated_decision_status for the outcome.",
	}
	effects = append(effects, boardDetails(row, details)...)
	return WritePreview{Operation: WriteOpRepositoryItemDelegate, Summary: "Let hive decide " + target, Target: target, Request: WriteRequest{Method: http.MethodPost, Path: itemPath(owner, repo, number, "decide-delegated"), Body: body}, Effects: effects, WideningDisclosure: "No widening: this operation targets one item (with prs, that issue's named pull requests).", ConfirmationMessage: "Confirm letting the hive decide " + target + ".", Details: details}, nil
}
