package adminmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// cqProvider answers the contributor queue's GET as the hive does (a JSON
// round trip of the Operations card payload, with ?controls=1's stored
// lists for the state read) and records writes.
type cqProvider struct {
	rows       []string // offerable rows, in card order
	withheld   []string
	order      []string
	hold       []string
	reasons    map[string]string
	noControls bool
	readErr    error
	reads      []string
	requests   []WriteRequest
}

func cqRow(key string, held bool, reason string) map[string]any {
	repo, num, _ := strings.Cut(key, "#")
	n, _ := strconv.Atoi(num)
	row := map[string]any{"key": key, "repo": repo, "number": n, "title": "issue " + num}
	if held {
		row["held"] = true
		if reason != "" {
			row["held_reason"] = reason
		}
	}
	return row
}

func (p *cqProvider) Read(_ context.Context, tool string, _ map[string]any) (any, error) {
	p.reads = append(p.reads, tool)
	if p.readErr != nil {
		return nil, p.readErr
	}
	queue := []any{}
	for _, k := range p.rows {
		queue = append(queue, cqRow(k, false, ""))
	}
	for _, k := range p.hold {
		queue = append(queue, cqRow(k, true, p.reasons[k]))
	}
	withheld := []any{}
	for _, k := range p.withheld {
		withheld = append(withheld, map[string]any{"key": k, "reason": "in_flight"})
	}
	body := map[string]any{"queue": queue, "queue_total": len(p.rows), "held_total": len(p.hold), "withheld": withheld}
	if tool == ContributorQueueStateRead && !p.noControls {
		reasons := []any{}
		for _, k := range p.hold {
			if r := p.reasons[k]; r != "" {
				reasons = append(reasons, map[string]any{"key": k, "reason": r})
			}
		}
		body["controls"] = map[string]any{"order": append([]string{}, p.order...), "hold": append([]string{}, p.hold...), "hold_reasons": reasons}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal(data, &out)
	return out, err
}

func (p *cqProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	p.requests = append(p.requests, req)
	return map[string]any{"ok": true}, nil
}

func newCQProvider() *cqProvider {
	return &cqProvider{
		rows:     []string{"acme/widgets#1", "acme/widgets#2", "acme/widgets#3", "acme/widgets#4"},
		withheld: []string{"acme/widgets#9"},
		order:    []string{"acme/widgets#3", "acme/widgets#1"},
		hold:     []string{"acme/widgets#7"},
		reasons:  map[string]string{"acme/widgets#7": "waiting on a design call"},
	}
}

func cqHandler(p *cqProvider) *Handler {
	return NewHandler(p, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
}

func cqPreview(t *testing.T, h *Handler, op string, args map[string]any) (WritePreview, string) {
	t.Helper()
	return capPreview(t, h, op, args)
}

func cqOrderOf(t *testing.T, req WriteRequest) []string {
	t.Helper()
	body, ok := req.Body.(map[string]any)
	if !ok {
		t.Fatalf("order body = %#v", req.Body)
	}
	return stringListValueLoose(body["order"])
}

func TestContributorQueueToolIsListedReadOnlyAndRoutesToTheCardsGET(t *testing.T) {
	if !AllowedTool(ToolContributorQueue) {
		t.Fatal("contributor_queue is not an allowed tool")
	}
	if AllowedTool(ContributorQueueStateRead) {
		t.Fatal("the previews' state read must not be callable as a tool")
	}
	var found bool
	for _, def := range Tools() {
		if def["name"] != ToolContributorQueue {
			continue
		}
		found = true
		if ann, _ := def["annotations"].(map[string]any); ann["readOnlyHint"] != true {
			t.Fatalf("annotations = %#v", ann)
		}
		props, _ := def["inputSchema"].(map[string]any)["properties"].(map[string]any)
		for _, arg := range []string{"limit", "offset", "withheld"} {
			if _, ok := props[arg]; !ok {
				t.Errorf("contributor_queue schema lacks %s", arg)
			}
		}
	}
	if !found {
		t.Fatal("contributor_queue not listed")
	}
	for tool, want := range map[string]string{ToolContributorQueue: "/api/contribute/queue", ContributorQueueStateRead: "/api/contribute/queue?controls=1&withheld=1"} {
		if got, err := ContributorQueueReadPath(tool, map[string]any{}); err != nil || got != want {
			t.Errorf("%s path = %q, %v; want %q", tool, got, err, want)
		}
	}
	if got, _ := ContributorQueueReadPath(ToolContributorQueue, map[string]any{"withheld": true}); got != "/api/contribute/queue?withheld=1" {
		t.Errorf("withheld path = %q", got)
	}
	if _, err := ContributorQueueReadPath(ToolContributorQueue, map[string]any{"withheld": "yes"}); err == nil {
		t.Error("a non-boolean withheld was accepted")
	}
}

// The tool returns the card's rows in the card's order, numbered, with held
// rows and their reasons, and pages them like the band tools.
func TestContributorQueueReadKeepsCardOrderAndPages(t *testing.T) {
	p := newCQProvider()
	p.rows = []string{"acme/widgets#3", "acme/widgets#1", "acme/widgets#2"} // pinned #3, #1 first
	data, err := p.Read(context.Background(), ToolContributorQueue, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ContributorQueueReadResult(ToolContributorQueue, data, map[string]any{"limit": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	rows := m["rows"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["key"] != "acme/widgets#3" || rows[0].(map[string]any)["position"] != 1 || rows[1].(map[string]any)["key"] != "acme/widgets#1" {
		t.Fatalf("first page = %#v", rows)
	}
	if m["next_offset"] != 2 || m["rows_total"] != 4 || m["rows_truncated"] != true {
		t.Fatalf("paging = next %v total %v truncated %v", m["next_offset"], m["rows_total"], m["rows_truncated"])
	}
	if _, ok := m["queue"]; ok {
		t.Fatal("the raw queue must not ride along with the paged rows")
	}
	out, _ = ContributorQueueReadResult(ToolContributorQueue, data, map[string]any{"limit": float64(2), "offset": float64(2)})
	rows = out.(map[string]any)["rows"].([]any)
	last := rows[len(rows)-1].(map[string]any)
	if len(rows) != 2 || last["key"] != "acme/widgets#7" || last["held"] != true || last["held_reason"] != "waiting on a design call" || last["position"] != 4 {
		t.Fatalf("second page = %#v, want the held row last with its reason", rows)
	}
	if _, ok := out.(map[string]any)["next_offset"]; ok {
		t.Fatal("next_offset on the last page")
	}
	refusal, _ := ContributorQueueReadResult(ToolContributorQueue, data, map[string]any{"offset": float64(-1)})
	if r, ok := refusal.(RefusalData); !ok || r.Kind != RefusalKindInvalidArgument {
		t.Fatalf("negative offset = %#v", refusal)
	}
}

func TestContributorQueueWriteOpsRegisteredOnTheCardsEndpoints(t *testing.T) {
	for name, path := range map[string]string{
		WriteOpContributorQueueOrder:     "PUT /api/contribute/queue/order",
		WriteOpContributorQueueHold:      "POST /api/contribute/queue/hold",
		WriteOpContributorQueueHoldClear: "POST /api/contribute/queue/hold/clear",
	} {
		op := mustOp(t, name)
		if !strings.Contains(op.Description(), path) || !strings.Contains(op.Description(), "Owner or read-write") {
			t.Errorf("%s description does not name %s and the role rule", name, path)
		}
		if _, ok := op.(providerPreviewOp); !ok {
			t.Errorf("%s does not read the queue before previewing", name)
		}
	}
	for _, name := range []string{WriteOpContributorQueueOrder, WriteOpContributorQueueHoldClear} {
		if _, ok := mustOp(t, name).(confirmGuardOp); !ok {
			t.Errorf("%s has no ConfirmGuard, so a stale preview could be confirmed", name)
		}
	}
}

// order: the preview shows the pinned list before and after, and the
// confirm PUTs exactly the new list.
func TestContributorQueueOrderFullListPreviewAndConfirm(t *testing.T) {
	p := newCQProvider()
	h := cqHandler(p)
	preview, id := cqPreview(t, h, WriteOpContributorQueueOrder, map[string]any{"order": []any{"acme/widgets#2", "acme/widgets#9", "acme/widgets#3"}})
	if preview.Request.Method != http.MethodPut || preview.Request.Path != "/api/contribute/queue/order" {
		t.Fatalf("request = %s %s", preview.Request.Method, preview.Request.Path)
	}
	if got := bodyJSON(t, preview.Request.Body); got != `{"order":["acme/widgets#2","acme/widgets#9","acme/widgets#3"]}` {
		t.Fatalf("body = %s", got)
	}
	effects := strings.Join(preview.Effects, "\n")
	if !strings.Contains(effects, "contribute_queue_order: [acme/widgets#3, acme/widgets#1] → [acme/widgets#2, acme/widgets#9, acme/widgets#3]") {
		t.Fatalf("effects do not show before and after:\n%s", effects)
	}
	// #9 is a withheld row: known, but not offered while it is withheld.
	if !strings.Contains(effects, "not offerable now") || !strings.Contains(effects, "acme/widgets#9") {
		t.Errorf("effects do not name the pin that is not offerable:\n%s", effects)
	}
	if preview.Details["before"] == nil || preview.Details["after"] == nil {
		t.Fatalf("details = %#v", preview.Details)
	}
	if isWidening(preview) {
		t.Fatalf("reordering is not widening: %s", preview.WideningDisclosure)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || !sameOrder(cqOrderOf(t, p.requests[0]), []string{"acme/widgets#2", "acme/widgets#9", "acme/widgets#3"}) {
		t.Fatalf("executed %+v", p.requests)
	}
}

func sameOrder(a, b []string) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

// pin moves an existing key and inserts a new one at the given position;
// unpin removes one. Neither needs the rest of the list retyped.
func TestContributorQueuePinAndUnpinEditTheCurrentList(t *testing.T) {
	for name, tc := range map[string]struct {
		args map[string]any
		want []string
	}{
		"insert at the front by default": {map[string]any{"pin": "acme/widgets#2"}, []string{"acme/widgets#2", "acme/widgets#3", "acme/widgets#1"}},
		"insert at a position":           {map[string]any{"pin": "acme/widgets#2", "position": float64(1)}, []string{"acme/widgets#3", "acme/widgets#2", "acme/widgets#1"}},
		"position past the end is last":  {map[string]any{"pin": "acme/widgets#4", "position": float64(99)}, []string{"acme/widgets#3", "acme/widgets#1", "acme/widgets#4"}},
		"move an existing key":           {map[string]any{"pin": "acme/widgets#1", "position": float64(0)}, []string{"acme/widgets#1", "acme/widgets#3"}},
		"pin a held issue":               {map[string]any{"pin": "acme/widgets#7", "position": float64(2)}, []string{"acme/widgets#3", "acme/widgets#1", "acme/widgets#7"}},
		"unpin":                          {map[string]any{"unpin": "acme/widgets#3"}, []string{"acme/widgets#1"}},
	} {
		t.Run(name, func(t *testing.T) {
			p := newCQProvider()
			h := cqHandler(p)
			preview, id := cqPreview(t, h, WriteOpContributorQueueOrder, tc.args)
			if got := cqOrderOf(t, preview.Request); !sameOrder(got, tc.want) {
				t.Fatalf("new list = %v, want %v", got, tc.want)
			}
			if !strings.Contains(strings.Join(preview.Effects, "\n"), "[acme/widgets#3, acme/widgets#1] → ") {
				t.Fatalf("effects do not show the current list:\n%s", strings.Join(preview.Effects, "\n"))
			}
			if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
				t.Fatalf("confirm: %v", err)
			}
			if len(p.requests) != 1 || !sameOrder(cqOrderOf(t, p.requests[0]), tc.want) {
				t.Fatalf("executed %+v, want %v", p.requests, tc.want)
			}
		})
	}
}

// A stored entry the endpoint does not accept is dropped when pin or unpin
// sends the list back, and the preview says so.
func TestContributorQueuePinDisclosesDroppedStoredEntries(t *testing.T) {
	p := newCQProvider()
	p.order = []string{"acme/widgets#3", "acme/widgets!OPS-1"}
	preview, _ := cqPreview(t, cqHandler(p), WriteOpContributorQueueOrder, map[string]any{"pin": "acme/widgets#2", "position": float64(5)})
	if got := cqOrderOf(t, preview.Request); !sameOrder(got, []string{"acme/widgets#3", "acme/widgets#2"}) {
		t.Fatalf("new list = %v", got)
	}
	if !strings.Contains(strings.Join(preview.Effects, "\n"), "does not accept (acme/widgets!OPS-1)") {
		t.Fatalf("effects do not disclose the dropped entry:\n%s", strings.Join(preview.Effects, "\n"))
	}
}

// Stale: the pinned list changed between preview and confirm. The confirm
// is refused and nothing is written; queue rows changing alone is not stale.
func TestContributorQueueOrderRefusesStalePreview(t *testing.T) {
	p := newCQProvider()
	h := cqHandler(p)
	_, id := cqPreview(t, h, WriteOpContributorQueueOrder, map[string]any{"pin": "acme/widgets#2"})
	p.order = []string{"acme/widgets#4"} // someone drags the card in between
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("stale confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirm wrote %+v", p.requests)
	}

	_, id = cqPreview(t, h, WriteOpContributorQueueOrder, map[string]any{"pin": "acme/widgets#2"})
	p.rows = p.rows[1:] // a row is assigned and leaves the queue: not stale
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm after a row left the queue: %v", err)
	}
	if len(p.requests) != 1 || !sameOrder(cqOrderOf(t, p.requests[0]), []string{"acme/widgets#2", "acme/widgets#4"}) {
		t.Fatalf("executed %+v", p.requests)
	}
}

// Malformed and unknown keys are refused in the preview, and nothing is
// minted or written.
func TestContributorQueueOpsRejectBadArgsAndUnknownKeys(t *testing.T) {
	p := newCQProvider()
	h := cqHandler(p)
	for name, tc := range map[string]struct {
		op   string
		args map[string]any
		want string
	}{
		"no form":                 {WriteOpContributorQueueOrder, map[string]any{}, "exactly one of order, pin or unpin"},
		"two forms":               {WriteOpContributorQueueOrder, map[string]any{"pin": "acme/widgets#1", "unpin": "acme/widgets#3"}, "exactly one of"},
		"typo":                    {WriteOpContributorQueueOrder, map[string]any{"pinn": "acme/widgets#1"}, `does not take "pinn"`},
		"position without pin":    {WriteOpContributorQueueOrder, map[string]any{"unpin": "acme/widgets#3", "position": float64(1)}, "position goes with pin only"},
		"negative position":       {WriteOpContributorQueueOrder, map[string]any{"pin": "acme/widgets#1", "position": float64(-1)}, "whole number"},
		"fractional position":     {WriteOpContributorQueueOrder, map[string]any{"pin": "acme/widgets#1", "position": 1.5}, "whole number"},
		"malformed pin":           {WriteOpContributorQueueOrder, map[string]any{"pin": "acme/widgets 12"}, "is not an issue key"},
		"bare number":             {WriteOpContributorQueueOrder, map[string]any{"pin": "#12"}, "is not an issue key"},
		"malformed in order":      {WriteOpContributorQueueOrder, map[string]any{"order": []any{"acme/widgets#1", "widgets#2"}}, "is not an issue key"},
		"duplicate in order":      {WriteOpContributorQueueOrder, map[string]any{"order": []any{"acme/widgets#1", "acme/widgets#1"}}, "twice"},
		"unknown pin":             {WriteOpContributorQueueOrder, map[string]any{"pin": "acme/widgets#404"}, "not in this hive's contributor queue"},
		"unknown in order":        {WriteOpContributorQueueOrder, map[string]any{"order": []any{"acme/widgets#1", "acme/gadgets#5"}}, "acme/gadgets#5 is not in this hive's contributor queue"},
		"unpin not pinned":        {WriteOpContributorQueueOrder, map[string]any{"unpin": "acme/widgets#2"}, "is not pinned"},
		"no change":               {WriteOpContributorQueueOrder, map[string]any{"pin": "acme/widgets#3"}, "changes nothing"},
		"hold malformed":          {WriteOpContributorQueueHold, map[string]any{"key": "acme#1", "held": true}, "is not an issue key"},
		"hold unknown":            {WriteOpContributorQueueHold, map[string]any{"key": "acme/widgets#404", "held": true}, "not in this hive's contributor queue"},
		"hold without held":       {WriteOpContributorQueueHold, map[string]any{"key": "acme/widgets#1"}, "held is required"},
		"resume with a reason":    {WriteOpContributorQueueHold, map[string]any{"key": "acme/widgets#7", "held": false, "reason": "x"}, "reason goes with held true only"},
		"reason too long":         {WriteOpContributorQueueHold, map[string]any{"key": "acme/widgets#1", "held": true, "reason": strings.Repeat("r", 201)}, "at most 200"},
		"resume an unheld issue":  {WriteOpContributorQueueHold, map[string]any{"key": "acme/widgets#1", "held": false}, "is not held"},
		"hold again, same reason": {WriteOpContributorQueueHold, map[string]any{"key": "acme/widgets#7", "held": true, "reason": "waiting on a design call"}, "changes nothing"},
		"hold_clear with args":    {WriteOpContributorQueueHoldClear, map[string]any{"key": "acme/widgets#7"}, "takes no arguments"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": tc.op, "args": tc.args})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to say %q", err, tc.want)
			}
		})
	}
	if pending, _ := h.PendingStore.List(context.Background()); len(pending) != 0 {
		t.Fatalf("a refused preview minted a confirmation: %+v", pending)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a refused preview wrote %+v", p.requests)
	}

	p.order, p.hold = nil, nil
	if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpContributorQueueHoldClear, "args": map[string]any{}}); err == nil || !strings.Contains(err.Error(), "no issue is held") {
		t.Fatalf("hold_clear with nothing held: err = %v", err)
	}
}

// A hive that cannot report the stored lists refuses the preview instead of
// guessing them from the rows.
func TestContributorQueueOpsRefuseWithoutTheStoredLists(t *testing.T) {
	p := newCQProvider()
	p.noControls = true
	_, err := cqHandler(p).PreviewWrite(context.Background(), map[string]any{"operation": WriteOpContributorQueueOrder, "args": map[string]any{"pin": "acme/widgets#2"}})
	if err == nil || !strings.Contains(err.Error(), "controls=1") {
		t.Fatalf("err = %v", err)
	}
	p = newCQProvider()
	p.readErr = errors.New("Hive API returned HTTP 403: your permissions on this hive are read-only")
	_, err = cqHandler(p).PreviewWrite(context.Background(), map[string]any{"operation": WriteOpContributorQueueHold, "args": map[string]any{"key": "acme/widgets#1", "held": true}})
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("err = %v, want the endpoint's refusal", err)
	}
}

func TestContributorQueueHoldPreviewAndConfirm(t *testing.T) {
	p := newCQProvider()
	h := cqHandler(p)
	preview, id := cqPreview(t, h, WriteOpContributorQueueHold, map[string]any{"key": "acme/widgets#2", "held": true, "reason": "needs a maintainer first"})
	if preview.Request.Method != http.MethodPost || preview.Request.Path != "/api/contribute/queue/hold" {
		t.Fatalf("request = %s %s", preview.Request.Method, preview.Request.Path)
	}
	if !strings.Contains(strings.Join(preview.Effects, "\n"), "acme/widgets#2: not held → held (reason: needs a maintainer first)") {
		t.Fatalf("effects:\n%s", strings.Join(preview.Effects, "\n"))
	}
	if isWidening(preview) {
		t.Fatalf("holding is not widening: %s", preview.WideningDisclosure)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || p.requests[0].Path != "/api/contribute/queue/hold" || bodyJSON(t, p.requests[0].Body) != `{"held":true,"key":"acme/widgets#2","reason":"needs a maintainer first"}` {
		t.Fatalf("executed %+v", p.requests)
	}

	preview, id = cqPreview(t, h, WriteOpContributorQueueHold, map[string]any{"key": "acme/widgets#7", "held": false})
	if !strings.Contains(strings.Join(preview.Effects, "\n"), "acme/widgets#7: held (reason: waiting on a design call) → not held") || !isWidening(preview) {
		t.Fatalf("resume preview: %s\n%s", preview.WideningDisclosure, strings.Join(preview.Effects, "\n"))
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 2 || bodyJSON(t, p.requests[1].Body) != `{"held":false,"key":"acme/widgets#7"}` {
		t.Fatalf("executed %+v", p.requests)
	}
}

// The issue key in the hold body is stored with the confirmation as it is:
// a field named "key" is masked as a credential unless it is an issue key.
func TestContributorQueueHoldKeySurvivesThePendingScrub(t *testing.T) {
	if pendingValueNeedsFreshConfirm(map[string]any{"key": "acme/widgets#2", "held": true}) {
		t.Fatal("an issue key under \"key\" is masked as a credential")
	}
	if !pendingValueNeedsFreshConfirm(map[string]any{"key": "sk-live-0123456789abcdef"}) {
		t.Fatal("a non-issue value under \"key\" is no longer masked")
	}
}

func TestContributorQueueHoldClearListsEveryKeyAndRefusesStale(t *testing.T) {
	p := newCQProvider()
	p.hold = []string{"acme/widgets#7", "acme/widgets#8"}
	h := cqHandler(p)
	preview, id := cqPreview(t, h, WriteOpContributorQueueHoldClear, map[string]any{})
	if preview.Request.Method != http.MethodPost || preview.Request.Path != "/api/contribute/queue/hold/clear" || preview.Request.Body != nil {
		t.Fatalf("request = %+v", preview.Request)
	}
	effects := strings.Join(preview.Effects, "\n")
	for _, want := range []string{"Resumes acme/widgets#7: held (reason: waiting on a design call) → not held", "Resumes acme/widgets#8: held (no reason) → not held"} {
		if !strings.Contains(effects, want) {
			t.Errorf("effects lack %q:\n%s", want, effects)
		}
	}
	if !isWidening(preview) || !strings.Contains(preview.WideningDisclosure, "acme/widgets#7, acme/widgets#8") {
		t.Fatalf("disclosure = %s", preview.WideningDisclosure)
	}

	p.hold = append(p.hold, "acme/widgets#4") // held after the preview: it would be resumed unseen
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("stale confirm err = %v", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirm wrote %+v", p.requests)
	}

	_, id = cqPreview(t, h, WriteOpContributorQueueHoldClear, map[string]any{})
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || p.requests[0].Method != http.MethodPost || p.requests[0].Path != "/api/contribute/queue/hold/clear" {
		t.Fatalf("executed %+v", p.requests)
	}
}

// The confirmation is stored and executed through the file store (as the
// dashboard endpoint does), so the order list and the hold key survive a
// JSON round trip and the scrub.
func TestContributorQueueOpsConfirmThroughTheFileStore(t *testing.T) {
	p := newCQProvider()
	h := NewHandler(p, WithWritesEnabled(true), WithPendingStore(NewFilePendingStore(t.TempDir()+"/pending.json")), WithHiveID("hive-a"))
	_, id := cqPreview(t, h, WriteOpContributorQueueOrder, map[string]any{"pin": "acme/widgets#4", "position": float64(1)})
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("order confirm: %v", err)
	}
	_, id = cqPreview(t, h, WriteOpContributorQueueHold, map[string]any{"key": "acme/widgets#4", "held": true})
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("hold confirm: %v", err)
	}
	if len(p.requests) != 2 || bodyJSON(t, p.requests[0].Body) != `{"order":["acme/widgets#3","acme/widgets#4","acme/widgets#1"]}` || bodyJSON(t, p.requests[1].Body) != `{"held":true,"key":"acme/widgets#4"}` {
		t.Fatalf("executed %+v", p.requests)
	}
}
