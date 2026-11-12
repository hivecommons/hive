package adminmcp

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
)

// The contributor ready-work queue (Contribute → Operations → Contributor
// Queue): one read tool and three write operations, the same controls the
// Operations card has. Each wraps exactly the endpoint the card uses, so the
// endpoint's own role gate (requireContributorWrite: owner or read-write),
// validation, audit entry and persistence apply unchanged. Nothing here
// stores or reorders anything itself.
const (
	ToolContributorQueue = "contributor_queue"

	// ContributorQueueStateRead is not a listed tool and AllowedTool refuses
	// it, so no MCP caller can call it. The contributor_queue.* previews read
	// it through Provider.Read: the queue rows with the withheld rows, plus
	// the stored pinned order and hold set (GET /api/contribute/queue
	// ?controls=1, which the endpoint answers for owner/read-write only).
	ContributorQueueStateRead = "contributor_queue_state"

	WriteOpContributorQueueOrder     = "contributor_queue.order"
	WriteOpContributorQueueHold      = "contributor_queue.hold"
	WriteOpContributorQueueHoldClear = "contributor_queue.hold_clear"

	contributorQueuePath          = "/api/contribute/queue"
	contributorQueueOrderPath     = "/api/contribute/queue/order"
	contributorQueueHoldPath      = "/api/contribute/queue/hold"
	contributorQueueHoldClearPath = "/api/contribute/queue/hold/clear"

	// contributorQueueMaxOrderKeys and contributorQueueMaxHoldReason mirror
	// pkg/dashboard's maxQueueOrderKeys and maxQueueHoldReasonLen.
	contributorQueueMaxOrderKeys  = 512
	contributorQueueMaxHoldReason = 200
)

// contributorQueueKeyPattern mirrors pkg/dashboard's queueOrderKeyPattern, the
// shape the order and hold endpoints accept. adminmcp cannot import
// pkg/dashboard (dashboard imports adminmcp), so it keeps its own copy. The
// order endpoint drops a key that does not match it without saying so, so
// the previews refuse such a key instead.
var contributorQueueKeyPattern = regexp.MustCompile(`^[^\s/#]+/[^\s/#]+#[0-9]+$`)

const contributorQueueOrdering = "Rows are in the Operations card's order, from GET /api/contribute/queue: issues the operator pinned (contribute_queue_order) first, in pinned order, then every other admissible issue in the hive's default scan order; held issues (held true, with held_reason when the operator gave one) come last, and are never offered until resumed. position counts from 1 across all rows. If you have label interests on this hive, rows matching them float to the front, as the card shows them to you. A contributor's own work can still come first for that contributor, as when the hive assigns work."

func contributorQueueWriteOps() []WriteOp {
	return []WriteOp{contributorQueueOrderOp{}, contributorQueueHoldOp{}, contributorQueueHoldClearOp{}}
}

// ---- read ------------------------------------------------------------------

// ContributorQueueReadPath builds the GET for the contributor_queue tool and
// for the write previews' state read. Both transports share it.
func ContributorQueueReadPath(tool string, args map[string]any) (string, error) {
	switch tool {
	case ToolContributorQueue:
		withheld, err := cqOptionalBool(args, "withheld")
		if err != nil {
			return "", err
		}
		if withheld {
			return contributorQueuePath + "?withheld=1", nil
		}
		return contributorQueuePath, nil
	case ContributorQueueStateRead:
		return contributorQueuePath + "?controls=1&withheld=1", nil
	default:
		return "", fmt.Errorf("%s is not a contributor queue read", tool)
	}
}

// ContributorQueueReadResult pages the decoded GET /api/contribute/queue
// answer for the contributor_queue tool: its queue becomes rows, each with
// its 1-based position, and offset/limit page them as the band tools do. The
// state read is returned as it came.
func ContributorQueueReadResult(tool string, data any, args map[string]any) (any, error) {
	if tool == ContributorQueueStateRead {
		return data, nil
	}
	if tool != ToolContributorQueue {
		return nil, fmt.Errorf("%s is not a contributor queue read", tool)
	}
	offset, err := OffsetFromArgs(args)
	if err != nil {
		return RefusalData{Type: "refusal", Operation: tool, Kind: RefusalKindInvalidArgument, Reason: err.Error()}, nil
	}
	body, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the hive answered GET %s with an unexpected shape", contributorQueuePath)
	}
	queue, _ := body["queue"].([]any)
	rows := make([]any, 0, len(queue))
	for i, raw := range queue {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		out := make(map[string]any, len(row)+1)
		for k, v := range row {
			out[k] = v
		}
		out["position"] = i + 1
		rows = append(rows, out)
	}
	limit := LimitFromArgs(args)
	total := len(rows)
	start := min(offset, total)
	end := min(start+limit, total)
	out := make(map[string]any, len(body)+4)
	for k, v := range body {
		if k == "queue" || k == "controls" {
			continue
		}
		out[k] = v
	}
	out["rows"] = rows[start:end]
	out["rows_total"] = total
	out["offset"] = offset
	out["ordering"] = contributorQueueOrdering
	if end < total {
		out["next_offset"] = end
		out["rows_truncated"] = true
		out["_admin_mcp"] = map[string]any{"truncated": true, "limit": limit, "total": total, "truncated_fields": []string{"rows"}}
	}
	return out, nil
}

func contributorQueueReadSchema() map[string]any {
	return map[string]any{
		"limit":    limitSchema(),
		"offset":   map[string]any{"type": "integer", "minimum": 0, "description": "Rows to skip; pass the previous answer's next_offset to read the next page past the limit cap."},
		"withheld": map[string]any{"type": "boolean", "description": "Also return withheld[]: candidates the queue keeps out, each with the gate that refused it (GET /api/contribute/queue?withheld=1), and withheld_total. Bounded by the hive's own queue limit, not by limit."},
	}
}

// ---- state shared by the write previews ------------------------------------

// contributorQueueState is what the previews read: the keys of the queue rows
// (offerable rows, and held rows, which the card shows last) and of the
// withheld rows, and the stored lists the controls edit.
type contributorQueueState struct {
	rows     []string
	heldRows []string
	withheld []string
	order    []string
	hold     []string
	reasons  map[string]string
}

func readContributorQueueState(ctx context.Context, provider Provider) (contributorQueueState, error) {
	if provider == nil {
		return contributorQueueState{}, fmt.Errorf("the contributor queue could not be read, so nothing was previewed")
	}
	data, err := provider.Read(ctx, ContributorQueueStateRead, map[string]any{})
	if err != nil {
		return contributorQueueState{}, fmt.Errorf("the contributor queue could not be read (GET %s?controls=1, which needs the owner or read-write role): %v; nothing was previewed", contributorQueuePath, err)
	}
	body, _ := data.(map[string]any)
	controls, ok := body["controls"].(map[string]any)
	if !ok {
		return contributorQueueState{}, fmt.Errorf("this hive does not report the queue's stored pinned order and hold set (GET %s?controls=1), so a change cannot be previewed against them; upgrade the hive. Nothing was previewed", contributorQueuePath)
	}
	rows, heldRows := contributorQueueRowKeys(body["queue"])
	withheld, _ := contributorQueueRowKeys(body["withheld"])
	st := contributorQueueState{
		rows:     rows,
		heldRows: heldRows,
		withheld: withheld,
		order:    cqStringList(controls["order"]),
		hold:     cqStringList(controls["hold"]),
		reasons:  map[string]string{},
	}
	reasons, _ := controls["hold_reasons"].([]any)
	for _, raw := range reasons {
		m, _ := raw.(map[string]any)
		key, _ := m["key"].(string)
		reason, _ := m["reason"].(string)
		if key != "" && reason != "" {
			st.reasons[key] = reason
		}
	}
	return st, nil
}

// contributorQueueRowKeys returns each row's identity key (its key field, or
// repo#number for a row from a hive that does not send one), split into rows
// that can be offered and rows marked held.
func contributorQueueRowKeys(v any) (offerable, held []string) {
	list, _ := v.([]any)
	offerable, held = []string{}, []string{}
	for _, raw := range list {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		key, _ := row["key"].(string)
		if key == "" {
			repo, _ := row["repo"].(string)
			number, ok := row["number"].(float64)
			if repo == "" || !ok || number <= 0 {
				continue
			}
			key = fmt.Sprintf("%s#%d", repo, int64(number))
		}
		if isHeld, _ := row["held"].(bool); isHeld {
			held = append(held, key)
		} else {
			offerable = append(offerable, key)
		}
	}
	return offerable, held
}

func cqStringList(v any) []string {
	out := []string{}
	switch list := v.(type) {
	case []string:
		out = append(out, list...)
	case []any:
		for _, raw := range list {
			if s, ok := raw.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// known reports whether the queue knows key: a queue row, a withheld row, or
// an entry already in the pinned order or the hold set.
func (st contributorQueueState) known(key string) bool {
	for _, list := range [][]string{st.rows, st.heldRows, st.withheld, st.order, st.hold} {
		if cqContains(list, key) {
			return true
		}
	}
	return false
}

// notOfferable returns the keys of list that are not offerable queue rows now:
// held, withheld, or gone from the queue.
func (st contributorQueueState) notOfferable(list []string) []string {
	out := []string{}
	for _, key := range list {
		if !cqContains(st.rows, key) {
			out = append(out, key)
		}
	}
	return out
}

func cqContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func contributorQueueKeyArg(op, field, raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", fmt.Errorf("%s: %s is required", op, field)
	}
	if !contributorQueueKeyPattern.MatchString(key) {
		return "", fmt.Errorf("%s: %q is not an issue key; write owner/repo#number, for example acme/widgets#12", op, key)
	}
	return key, nil
}

func unknownContributorQueueKey(op, key string) error {
	return fmt.Errorf("%s: %s is not in this hive's contributor queue: it is not a queue row or a withheld row, and it is not pinned or held. Read contributor_queue with withheld true for the keys the queue knows; nothing was previewed", op, key)
}

// cqRejectUnknownArgs refuses an argument the operation does not take, so a
// typo is not silently ignored.
func cqRejectUnknownArgs(op string, args map[string]any, allowed ...string) error {
	for _, k := range SortKeys(args) {
		if !cqContains(allowed, k) {
			if len(allowed) == 0 {
				return fmt.Errorf("%s takes no arguments; got %q", op, k)
			}
			return fmt.Errorf("%s does not take %q; it takes %s", op, k, strings.Join(allowed, ", "))
		}
	}
	return nil
}

func cqOptionalBool(args map[string]any, key string) (bool, error) {
	switch v := args[key].(type) {
	case nil:
		return false, nil
	case bool:
		return v, nil
	default:
		return false, fmt.Errorf("%s must be true or false", key)
	}
}

// sanitizeContributorQueueOrder is the list the order endpoint stores for
// list: trimmed, well-formed and unique, in order. dropped is what it leaves
// out.
func sanitizeContributorQueueOrder(list []string) (kept, dropped []string) {
	kept, dropped = []string{}, []string{}
	seen := map[string]bool{}
	for _, raw := range list {
		key := strings.TrimSpace(raw)
		if key == "" || !contributorQueueKeyPattern.MatchString(key) || seen[key] {
			dropped = append(dropped, raw)
			continue
		}
		seen[key] = true
		kept = append(kept, key)
	}
	return kept, dropped
}

func cqSameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cqRemove(list []string, s string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// ---- contributor_queue.order -------------------------------------------------

type contributorQueueOrderOp struct{}

// contributorQueueOrderArgs is one of the three forms: the whole list
// (order), one key moved or inserted (pin, position), or one key removed
// (unpin).
type contributorQueueOrderArgs struct {
	form     string
	order    []string
	key      string
	position int
}

func (contributorQueueOrderOp) Name() string { return WriteOpContributorQueueOrder }
func (contributorQueueOrderOp) Description() string {
	return "Set the contributor queue's pinned order (contribute_queue_order) through PUT /api/contribute/queue/order, the endpoint the Operations card's drag and move controls save through. Pass exactly one of: order (the whole list of owner/repo#number keys), pin (one key to insert or move, at position, 0-based in the pinned list, default 0, clamped to its length), or unpin (one key to remove). Pinned issues are offered first, in this order; pinning never makes an issue admissible. The preview reads the current pinned list and shows it before and after; the endpoint replaces the whole list. A malformed key, or one the queue does not know (not a queue or withheld row, not pinned or held), is refused. write_confirm is refused if the pinned list changed since the preview. Owner or read-write: the endpoint refuses any other role."
}
func (contributorQueueOrderOp) InputSchema() map[string]any {
	key := map[string]any{"type": "string", "pattern": contributorQueueKeyPattern.String(), "description": "An issue key, owner/repo#number."}
	return map[string]any{"type": "object", "properties": map[string]any{
		"order":    map[string]any{"type": "array", "items": key, "maxItems": contributorQueueMaxOrderKeys, "description": "The whole pinned list, first offered first. [] removes every pin."},
		"pin":      key,
		"position": map[string]any{"type": "integer", "minimum": 0, "description": "Where pin goes in the pinned list, 0-based (default 0, the front); past the end means last."},
		"unpin":    key,
	}, "additionalProperties": false}
}

func parseContributorQueueOrderArgs(args map[string]any) (contributorQueueOrderArgs, error) {
	op := WriteOpContributorQueueOrder
	if err := cqRejectUnknownArgs(op, args, "order", "pin", "position", "unpin"); err != nil {
		return contributorQueueOrderArgs{}, err
	}
	var forms []string
	for _, f := range []string{"order", "pin", "unpin"} {
		if _, ok := args[f]; ok {
			forms = append(forms, f)
		}
	}
	if len(forms) != 1 {
		return contributorQueueOrderArgs{}, fmt.Errorf("%s takes exactly one of order, pin or unpin; got %s", op, listLabel(forms))
	}
	out := contributorQueueOrderArgs{form: forms[0]}
	if _, ok := args["position"]; ok && out.form != "pin" {
		return contributorQueueOrderArgs{}, fmt.Errorf("%s: position goes with pin only", op)
	}
	switch out.form {
	case "order":
		var raw []any
		switch v := args["order"].(type) {
		case []any:
			raw = v
		case []string:
			for _, s := range v {
				raw = append(raw, s)
			}
		default:
			return contributorQueueOrderArgs{}, fmt.Errorf("%s: order must be a list of owner/repo#number keys", op)
		}
		if len(raw) > contributorQueueMaxOrderKeys {
			return contributorQueueOrderArgs{}, fmt.Errorf("%s: order has %d keys; the endpoint takes at most %d", op, len(raw), contributorQueueMaxOrderKeys)
		}
		out.order = []string{}
		for _, item := range raw {
			s, ok := item.(string)
			if !ok {
				return contributorQueueOrderArgs{}, fmt.Errorf("%s: order must be a list of owner/repo#number keys", op)
			}
			key, err := contributorQueueKeyArg(op, "each order entry", s)
			if err != nil {
				return contributorQueueOrderArgs{}, err
			}
			if cqContains(out.order, key) {
				return contributorQueueOrderArgs{}, fmt.Errorf("%s: %s is in order twice", op, key)
			}
			out.order = append(out.order, key)
		}
	case "pin", "unpin":
		s, ok := args[out.form].(string)
		if !ok {
			return contributorQueueOrderArgs{}, fmt.Errorf("%s: %s must be an owner/repo#number key", op, out.form)
		}
		key, err := contributorQueueKeyArg(op, out.form, s)
		if err != nil {
			return contributorQueueOrderArgs{}, err
		}
		out.key = key
		if out.form == "pin" {
			pos, err := contributorQueuePosition(args["position"])
			if err != nil {
				return contributorQueueOrderArgs{}, err
			}
			out.position = pos
		}
	}
	return out, nil
}

func contributorQueuePosition(v any) (int, error) {
	var f float64
	switch x := v.(type) {
	case nil:
		return 0, nil
	case float64:
		f = x
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	default:
		return 0, fmt.Errorf("%s: position must be a whole number, 0 or more", WriteOpContributorQueueOrder)
	}
	if f < 0 || f != math.Trunc(f) || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, fmt.Errorf("%s: position must be a whole number, 0 or more", WriteOpContributorQueueOrder)
	}
	if f > contributorQueueMaxOrderKeys {
		f = contributorQueueMaxOrderKeys
	}
	return int(f), nil
}

// Preview without a provider checks the arguments and names the endpoint;
// write_preview always goes through PreviewWithProvider, and write_confirm
// uses this only to check the method and path.
func (contributorQueueOrderOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, err := parseContributorQueueOrderArgs(args); err != nil {
		return WritePreview{}, err
	}
	return WritePreview{
		Operation:           WriteOpContributorQueueOrder,
		Summary:             "Change the contributor queue's pinned order (current order not read)",
		Target:              "contributor queue",
		Request:             WriteRequest{Method: http.MethodPut, Path: contributorQueueOrderPath},
		WideningDisclosure:  "No widening: the pinned order changes which admissible issue is offered first, never which issues are admissible.",
		ConfirmationMessage: "Preview this change through write_preview to see the pinned order before and after.",
	}, nil
}

// ConfirmGuard reads the pinned order again at write_confirm. Stale means the
// stored pinned list is not exactly the list the preview read: someone saved
// another order (the card, another caller) in between, and sending this
// preview's list would overwrite their edit without anyone seeing it. Queue
// rows coming and going (work assigned, a cooldown ending) do not change the
// stored list, so they do not make a preview stale.
func (contributorQueueOrderOp) ConfirmGuard(ctx context.Context, _ map[string]any, provider Provider) (string, error) {
	st, err := readContributorQueueState(ctx, provider)
	if err != nil {
		return "", fmt.Errorf("write_confirm reads the pinned order again before writing, and could not; preview it again: %w", err)
	}
	return stateFingerprint(st.order)
}

func (contributorQueueOrderOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	op := WriteOpContributorQueueOrder
	a, err := parseContributorQueueOrderArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	st, err := readContributorQueueState(ctx, provider)
	if err != nil {
		return WritePreview{}, err
	}
	before := st.order
	var after, dropped []string
	var summary, confirm string
	switch a.form {
	case "order":
		for _, key := range a.order {
			if !st.known(key) {
				return WritePreview{}, unknownContributorQueueKey(op, key)
			}
		}
		after, dropped = a.order, []string{}
		summary = fmt.Sprintf("Replace the contributor queue's pinned order (%d keys)", len(after))
		confirm = "Confirm setting the contributor queue's pinned order to [" + listLabel(after) + "]."
	case "pin":
		if !st.known(a.key) {
			return WritePreview{}, unknownContributorQueueKey(op, a.key)
		}
		var base []string
		base, dropped = sanitizeContributorQueueOrder(before)
		base = cqRemove(base, a.key)
		pos := min(a.position, len(base))
		after = make([]string, 0, len(base)+1)
		after = append(after, base[:pos]...)
		after = append(after, a.key)
		after = append(after, base[pos:]...)
		summary = fmt.Sprintf("Pin %s at position %d of the contributor queue's pinned order", a.key, pos)
		confirm = fmt.Sprintf("Confirm pinning %s at position %d (0-based) of the contributor queue's pinned order.", a.key, pos)
	case "unpin":
		if !cqContains(before, a.key) {
			return WritePreview{}, fmt.Errorf("%s: %s is not pinned, so there is nothing to unpin; the pinned order is [%s]. Nothing was previewed", op, a.key, listLabel(before))
		}
		after, dropped = sanitizeContributorQueueOrder(before)
		after = cqRemove(after, a.key)
		summary = "Unpin " + a.key + " from the contributor queue's pinned order"
		confirm = "Confirm unpinning " + a.key + " from the contributor queue's pinned order."
	}
	if cqSameStrings(after, before) {
		return WritePreview{}, fmt.Errorf("%s changes nothing: the pinned order already reads [%s]", op, listLabel(before))
	}
	fingerprint, err := stateFingerprint(st.order)
	if err != nil {
		return WritePreview{}, err
	}
	stale := st.notOfferable(after)
	change := settingChange{Field: "contribute_queue_order", Before: before, After: after}
	effects := []string{
		change.effect(),
		"Hive saves it through PUT " + contributorQueueOrderPath + ", the save the Operations card's drag and move controls make. The endpoint replaces the whole pinned list with this one and records a contribute_queue_order audit entry.",
		"Pinned issues are offered first, in this order, and every other admissible issue follows in the default order. Pinning only changes offer priority: a pinned issue that fails admission, is in cooldown, is held, is in a disabled repository or is already being worked is still not offered.",
	}
	if len(stale) > 0 {
		effects = append(effects, "Pinned but not offerable now (held, withheld or gone from the queue), so skipped until it is offerable again: "+listLabel(stale)+".")
	}
	if len(dropped) > 0 {
		effects = append(effects, "The stored pinned list holds entries the endpoint does not accept ("+listLabel(dropped)+"); sending the list drops them.")
	}
	effects = append(effects, "This write assigns, holds, labels, approves and merges nothing.")
	details := map[string]any{"form": a.form, "before": before, "after": after, "not_offerable": stale}
	switch a.form {
	case "pin":
		details["pin"] = a.key
		details["position"] = cqIndexOf(after, a.key)
	case "unpin":
		details["unpin"] = a.key
	}
	if len(dropped) > 0 {
		details["dropped"] = dropped
	}
	return WritePreview{
		Operation:           op,
		Summary:             summary,
		Target:              "contributor queue",
		Request:             WriteRequest{Method: http.MethodPut, Path: contributorQueueOrderPath, Body: map[string]any{"order": after}},
		Effects:             effects,
		WideningDisclosure:  "No widening: the pinned order changes which admissible issue is offered first, never which issues are admissible.",
		ConfirmationMessage: confirm,
		Details:             details,
		StateFingerprint:    fingerprint,
	}, nil
}

func cqIndexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// ---- contributor_queue.hold --------------------------------------------------

type contributorQueueHoldOp struct{}

type contributorQueueHoldArgs struct {
	key    string
	held   bool
	reason string
}

func (contributorQueueHoldOp) Name() string { return WriteOpContributorQueueHold }
func (contributorQueueHoldOp) Description() string {
	return "Hold or resume one issue in the contributor queue through POST /api/contribute/queue/hold, the Operations card's Hold and Resume control. Takes key (owner/repo#number), held (true holds, false resumes) and, when holding, an optional reason (at most 200 characters) shown in the card's on-hold badge. A held issue is never offered to a contributor until resumed; unlike a cooldown it does not clear itself. The preview shows the issue's current hold state. A malformed key, or one the queue does not know, is refused. Owner or read-write: the endpoint refuses any other role."
}
func (contributorQueueHoldOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"key":    map[string]any{"type": "string", "pattern": contributorQueueKeyPattern.String(), "description": "The issue, owner/repo#number."},
		"held":   map[string]any{"type": "boolean", "description": "true holds the issue, false resumes it."},
		"reason": map[string]any{"type": "string", "maxLength": contributorQueueMaxHoldReason, "description": "Why it is held, shown in the card's on-hold badge. Only with held true."},
	}, "required": []string{"key", "held"}, "additionalProperties": false}
}

func parseContributorQueueHoldArgs(args map[string]any) (contributorQueueHoldArgs, error) {
	op := WriteOpContributorQueueHold
	if err := cqRejectUnknownArgs(op, args, "key", "held", "reason"); err != nil {
		return contributorQueueHoldArgs{}, err
	}
	raw, _ := args["key"].(string)
	key, err := contributorQueueKeyArg(op, "key", raw)
	if err != nil {
		return contributorQueueHoldArgs{}, err
	}
	held, ok := args["held"].(bool)
	if !ok {
		return contributorQueueHoldArgs{}, fmt.Errorf("%s: held is required: true to hold, false to resume", op)
	}
	var reason string
	if v, present := args["reason"]; present {
		s, ok := v.(string)
		if !ok {
			return contributorQueueHoldArgs{}, fmt.Errorf("%s: reason must be text", op)
		}
		reason = strings.TrimSpace(s)
	}
	if reason != "" && !held {
		return contributorQueueHoldArgs{}, fmt.Errorf("%s: reason goes with held true only; resuming drops the issue's reason", op)
	}
	if len(reason) > contributorQueueMaxHoldReason {
		return contributorQueueHoldArgs{}, fmt.Errorf("%s: reason is %d characters; the endpoint keeps at most %d, so shorten it", op, len(reason), contributorQueueMaxHoldReason)
	}
	return contributorQueueHoldArgs{key: key, held: held, reason: reason}, nil
}

func (a contributorQueueHoldArgs) request() WriteRequest {
	body := map[string]any{"key": a.key, "held": a.held}
	if a.held && a.reason != "" {
		body["reason"] = a.reason
	}
	return WriteRequest{Method: http.MethodPost, Path: contributorQueueHoldPath, Body: body}
}

func cqHoldStateLabel(held bool, reason string) string {
	switch {
	case !held:
		return "not held"
	case reason == "":
		return "held (no reason)"
	default:
		return "held (reason: " + reason + ")"
	}
}

func (contributorQueueHoldOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	a, err := parseContributorQueueHoldArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	return WritePreview{
		Operation:           WriteOpContributorQueueHold,
		Summary:             "Change the hold on " + a.key + " (current hold not read)",
		Target:              a.key,
		Request:             a.request(),
		WideningDisclosure:  "Potential widening: the current hold was not read, so this preview cannot say whether the issue becomes offerable.",
		ConfirmationMessage: "Preview this change through write_preview to see the issue's hold before and after.",
	}, nil
}

func (contributorQueueHoldOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	op := WriteOpContributorQueueHold
	a, err := parseContributorQueueHoldArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	st, err := readContributorQueueState(ctx, provider)
	if err != nil {
		return WritePreview{}, err
	}
	if !st.known(a.key) {
		return WritePreview{}, unknownContributorQueueKey(op, a.key)
	}
	isHeld := cqContains(st.hold, a.key)
	curReason := st.reasons[a.key]
	if a.held && isHeld && curReason == a.reason {
		return WritePreview{}, fmt.Errorf("%s changes nothing: %s is already %s", op, a.key, cqHoldStateLabel(true, curReason))
	}
	if !a.held && !isHeld {
		return WritePreview{}, fmt.Errorf("%s changes nothing: %s is not held, so there is nothing to resume", op, a.key)
	}
	before := cqHoldStateLabel(isHeld, curReason)
	after := cqHoldStateLabel(a.held, a.reason)
	effects := []string{
		a.key + ": " + before + " → " + after,
		"Hive saves it through POST " + contributorQueueHoldPath + ", the Operations card's Hold and Resume control, and records a contribute_queue_hold audit entry.",
	}
	var summary, disclosure, confirm string
	if a.held {
		summary = "Hold " + a.key + " in the contributor queue"
		confirm = "Confirm holding " + a.key + " in the contributor queue."
		effects = append(effects, "A held issue is never offered to a contributor until it is resumed; unlike a cooldown, a hold does not clear itself. The row stays on the card, greyed, with an on-hold badge. Its place in the pinned order, if any, is kept.")
		disclosure = "No widening: holding stops an issue from being offered."
	} else {
		summary = "Resume " + a.key + " in the contributor queue"
		confirm = "Confirm resuming " + a.key + " in the contributor queue."
		effects = append(effects, "The issue can be offered to contributors again once it passes admission, cooldown and the other checks; its hold reason is dropped.")
		disclosure = "Widening: " + a.key + " can be offered to contributors again once it passes admission."
	}
	if !a.held && !cqContains(st.heldRows, a.key) {
		effects = append(effects, a.key+" is not a queue row now (it is withheld, or no longer admissible), so resuming it does not put it in front of contributors until it is admissible again.")
	}
	effects = append(effects, "This write assigns, labels, approves and merges nothing.")
	return WritePreview{
		Operation:           op,
		Summary:             summary,
		Target:              a.key,
		Request:             a.request(),
		Effects:             effects,
		WideningDisclosure:  disclosure,
		ConfirmationMessage: confirm,
		Details:             map[string]any{"issue": a.key, "before": before, "after": after, "held": a.held},
	}, nil
}

// ---- contributor_queue.hold_clear --------------------------------------------

type contributorQueueHoldClearOp struct{}

func (contributorQueueHoldClearOp) Name() string { return WriteOpContributorQueueHoldClear }
func (contributorQueueHoldClearOp) Description() string {
	return "Resume every held issue in the contributor queue at once through POST /api/contribute/queue/hold/clear, the Operations card's Resume all control. Takes no arguments. The preview lists every issue it will resume, with its reason. write_confirm is refused if the hold set changed since the preview. Owner or read-write: the endpoint refuses any other role."
}
func (contributorQueueHoldClearOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}

func (contributorQueueHoldClearOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if err := cqRejectUnknownArgs(WriteOpContributorQueueHoldClear, args); err != nil {
		return WritePreview{}, err
	}
	return WritePreview{
		Operation:           WriteOpContributorQueueHoldClear,
		Summary:             "Resume every held issue in the contributor queue (current holds not read)",
		Target:              "contributor queue",
		Request:             WriteRequest{Method: http.MethodPost, Path: contributorQueueHoldClearPath},
		WideningDisclosure:  "Potential widening: the current holds were not read, so this preview cannot say which issues become offerable.",
		ConfirmationMessage: "Preview this change through write_preview to see every issue it would resume.",
	}, nil
}

// ConfirmGuard reads the hold set again at write_confirm. Stale means the set
// is not exactly the one the preview listed: resuming now would also resume
// an issue the preview never showed, or the list shown is out of date.
func (contributorQueueHoldClearOp) ConfirmGuard(ctx context.Context, _ map[string]any, provider Provider) (string, error) {
	st, err := readContributorQueueState(ctx, provider)
	if err != nil {
		return "", fmt.Errorf("write_confirm reads the hold set again before writing, and could not; preview it again: %w", err)
	}
	return stateFingerprint(st.hold)
}

func (contributorQueueHoldClearOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	op := WriteOpContributorQueueHoldClear
	if err := cqRejectUnknownArgs(op, args); err != nil {
		return WritePreview{}, err
	}
	st, err := readContributorQueueState(ctx, provider)
	if err != nil {
		return WritePreview{}, err
	}
	if len(st.hold) == 0 {
		return WritePreview{}, fmt.Errorf("%s changes nothing: no issue is held", op)
	}
	fingerprint, err := stateFingerprint(st.hold)
	if err != nil {
		return WritePreview{}, err
	}
	resume := make([]map[string]any, 0, len(st.hold))
	effects := make([]string, 0, len(st.hold)+3)
	for _, key := range st.hold {
		effects = append(effects, "Resumes "+key+": "+cqHoldStateLabel(true, st.reasons[key])+" → not held")
		row := map[string]any{"issue": key}
		if r := st.reasons[key]; r != "" {
			row["reason"] = r
		}
		resume = append(resume, row)
	}
	effects = append(effects,
		"Hive saves it through POST "+contributorQueueHoldClearPath+", the Operations card's Resume all control: it empties the hold set and every hold reason, and records a contribute_queue_hold_clear audit entry.",
		"This write assigns, labels, approves and merges nothing.",
	)
	n := len(st.hold)
	return WritePreview{
		Operation:           op,
		Summary:             fmt.Sprintf("Resume all %d held issues in the contributor queue", n),
		Target:              "contributor queue",
		Request:             WriteRequest{Method: http.MethodPost, Path: contributorQueueHoldClearPath},
		Effects:             effects,
		WideningDisclosure:  fmt.Sprintf("Widening: %d held issues can be offered to contributors again once they pass admission: %s.", n, listLabel(st.hold)),
		ConfirmationMessage: fmt.Sprintf("Confirm resuming all %d held issues: %s.", n, listLabel(st.hold)),
		Details:             map[string]any{"resume": resume},
		StateFingerprint:    fingerprint,
	}, nil
}
