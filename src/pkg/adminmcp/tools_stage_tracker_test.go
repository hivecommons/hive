package adminmcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// stageTrackerFixture is a /api/stage-tracker answer as JSON decodes it.
func stageTrackerFixture(t *testing.T) map[string]any {
	t.Helper()
	raw := `{
	  "generated_at": "2026-11-06T02:00:00Z", "source": "fresh", "hive_id": "bluefin",
	  "open_issues": 2, "open_prs": 2, "needs_human": 1, "on_hold": 1,
	  "stages": [{"number": 40, "slug": "covered-by-pr", "label": "Covered by PR", "short": "PR", "dwell_seconds": 3600},
	             {"number": 10, "slug": "actionable", "label": "Actionable", "dwell_seconds": 600}],
	  "items": [
	    {"repo": "projectbluefin/utah-packages", "number": 488, "kind": "issue", "title": "Package the gimp batch", "stage": 40,
	     "linked_prs": [{"number": 892, "title": "gimp batch"}], "history": [{"number": 10}], "ci_activity": {"x": 1}},
	    {"repo": "projectbluefin/utah-packages", "number": 892, "kind": "pr", "title": "gimp batch", "stage": 40, "head": "abc123",
	     "reason": "contributor-lane:review", "caption": "review pending", "sentinel_reason": "", "nested_under": 488,
	     "waiting_on_you": true, "wait_kind": "approve-needed"},
	    {"repo": "projectbluefin/chairlift", "number": 639, "kind": "issue", "title": "Settings crash", "stage": 10, "needs_human": true,
	     "decision_needed": {"question": "Which fix?"}, "blocker": {"kind": "workflows"}},
	    {"repo": "projectbluefin/chairlift", "number": 488, "kind": "pr", "title": "Unrelated", "stage": 10, "on_hold": true}
	  ],
	  "recently_merged": [{"number": 1}, {"number": 2}, {"number": 3}]
	}`
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func readBoard(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	got, err := StageTrackerReadResult(stageTrackerFixture(t), args)
	if err != nil {
		t.Fatal(err)
	}
	out, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("result = %#v", got)
	}
	return out
}

func boardRefs(out map[string]any) []string {
	var refs []string
	for _, raw := range out["items"].([]any) {
		it := raw.(map[string]any)
		refs = append(refs, stringField(it, "repo")+"#"+strings.TrimSpace(strings.Split(jsonString(it["number"]), ".")[0]))
	}
	return refs
}

func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestStageTrackerReadFilters(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want []string
	}{
		{"all", map[string]any{}, []string{"projectbluefin/utah-packages#488", "projectbluefin/utah-packages#892", "projectbluefin/chairlift#639", "projectbluefin/chairlift#488"}},
		{"repo", map[string]any{"repo": "ProjectBluefin/Chairlift"}, []string{"projectbluefin/chairlift#639", "projectbluefin/chairlift#488"}},
		{"kind", map[string]any{"kind": "pr"}, []string{"projectbluefin/utah-packages#892", "projectbluefin/chairlift#488"}},
		{"number is the item only", map[string]any{"number": float64(892)}, []string{"projectbluefin/utah-packages#892"}},
		{"number across repos", map[string]any{"number": float64(488)}, []string{"projectbluefin/utah-packages#488", "projectbluefin/chairlift#488"}},
		// The search box: a number also matches a linked item.
		{"q number matches linked pr", map[string]any{"q": "#892"}, []string{"projectbluefin/utah-packages#488", "projectbluefin/utah-packages#892"}},
		{"q title", map[string]any{"q": "CRASH"}, []string{"projectbluefin/chairlift#639"}},
		{"stage", map[string]any{"stage": float64(10)}, []string{"projectbluefin/chairlift#639", "projectbluefin/chairlift#488"}},
		{"needs_human", map[string]any{"needs_human": true}, []string{"projectbluefin/chairlift#639"}},
		{"on_hold false", map[string]any{"on_hold": false, "repo": "projectbluefin/chairlift"}, []string{"projectbluefin/chairlift#639"}},
		{"waiting_on_you", map[string]any{"waiting_on_you": true}, []string{"projectbluefin/utah-packages#892"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := readBoard(t, tc.args)
			if got := boardRefs(out); !slices.Equal(got, tc.want) {
				t.Fatalf("items = %v, want %v", got, tc.want)
			}
			if out["matched"] != len(tc.want) || out["total_items"] != 4 {
				t.Fatalf("matched=%v total=%v", out["matched"], out["total_items"])
			}
		})
	}
}

func TestStageTrackerReadStageCountsDescribeWholeBoard(t *testing.T) {
	out := readBoard(t, map[string]any{"repo": "projectbluefin/chairlift"})
	stages := out["stages"].([]map[string]any)
	if len(stages) != 2 || intField(stages[0], "number") != 10 || stages[0]["count"] != 2 || stages[1]["count"] != 2 {
		t.Fatalf("stages = %#v", stages)
	}
	if _, ok := stages[1]["short"]; ok {
		t.Fatalf("stage rows keep only number, slug, label, dwell and count: %#v", stages[1])
	}
	if out["hive_id"] != "bluefin" || out["needs_human"] != float64(1) {
		t.Fatalf("board counts missing: %#v", out)
	}
}

func TestStageTrackerReadSummaryAndFullViews(t *testing.T) {
	summary := readBoard(t, map[string]any{"number": float64(488), "repo": "projectbluefin/utah-packages"})
	item := summary["items"].([]any)[0].(map[string]any)
	if _, ok := item["history"]; ok {
		t.Fatalf("summary keeps history: %#v", item)
	}
	if _, ok := item["linked_prs"]; !ok {
		t.Fatalf("summary drops linked_prs: %#v", item)
	}
	full := readBoard(t, map[string]any{"number": float64(488), "repo": "projectbluefin/utah-packages", "view": "full"})
	item = full["items"].([]any)[0].(map[string]any)
	if _, ok := item["history"]; !ok || full["view"] != "full" {
		t.Fatalf("full view drops history: %#v", item)
	}
}

func TestStageTrackerReadPagesAndRecent(t *testing.T) {
	first := readBoard(t, map[string]any{"limit": float64(3), "recent": true})
	if len(first["items"].([]any)) != 3 || first["next_offset"] != 3 {
		t.Fatalf("first page = %#v", first)
	}
	if len(first["recently_merged"].([]any)) != 3 {
		t.Fatalf("recent = %#v", first["recently_merged"])
	}
	second := readBoard(t, map[string]any{"limit": float64(3), "offset": float64(3)})
	if got := boardRefs(second); !slices.Equal(got, []string{"projectbluefin/chairlift#488"}) {
		t.Fatalf("second page = %v", got)
	}
	if _, ok := second["next_offset"]; ok {
		t.Fatalf("last page has next_offset: %#v", second)
	}
	if _, ok := second["recently_merged"]; ok {
		t.Fatal("recently_merged returned without recent")
	}
}

func TestStageTrackerReadRefusesBadArguments(t *testing.T) {
	for _, args := range []map[string]any{{"kind": "epic"}, {"view": "wide"}, {"offset": float64(-1)}, {"number": "x"}} {
		got, err := StageTrackerReadResult(stageTrackerFixture(t), args)
		if err != nil {
			t.Fatal(err)
		}
		if r, ok := got.(RefusalData); !ok || r.Kind != RefusalKindInvalidArgument {
			t.Fatalf("args %v: result = %#v", args, got)
		}
	}
}

// boardProvider serves stage_tracker_read from the fixture, as both transports
// do, so a preview reads the item off the board.
type boardProvider struct {
	t     *testing.T
	err   error
	calls int
}

func (p *boardProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	p.calls++
	if tool != ToolStageTrackerRead {
		p.t.Fatalf("preview read %q", tool)
	}
	if p.err != nil {
		return nil, p.err
	}
	return StageTrackerReadResult(stageTrackerFixture(p.t), args)
}

func previewOp(t *testing.T, name string, args map[string]any, provider Provider) (WritePreview, error) {
	t.Helper()
	op, ok := DefaultWriteRegistry().Get(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	if po, ok := op.(providerPreviewOp); ok {
		return po.PreviewWithProvider(context.Background(), args, provider)
	}
	return op.Preview(context.Background(), args)
}

func itemArgs(repo string, number int, extra map[string]any) map[string]any {
	owner, name, _ := strings.Cut(repo, "/")
	args := map[string]any{"owner": owner, "repo": name, "number": float64(number)}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

func TestStageTrackerWriteOpsAreRegistered(t *testing.T) {
	reg := DefaultWriteRegistry()
	for _, name := range []string{WriteOpRepositoryRescan, WriteOpRepositoryItemAcceptTriage, WriteOpRepositoryItemNeedsHuman, WriteOpRepositoryItemBlockerRecheck, WriteOpRepositoryItemBlockerHandoff, WriteOpRepositoryItemBlockerClosePart, WriteOpRepositoryItemDecision, WriteOpRepositoryItemDelegate} {
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("%s not registered", name)
		}
	}
	for _, tool := range []string{ToolStageTrackerRead, ToolDelegatedDecisionStatus} {
		if !AllowedTool(tool) {
			t.Fatalf("%s not allowed", tool)
		}
		found := false
		for _, def := range Tools() {
			if def["name"] == tool {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s missing from tools/list", tool)
		}
	}
}

func TestItemOpsSendTheDashboardRequests(t *testing.T) {
	p := &boardProvider{t: t}
	cases := []struct {
		op   string
		args map[string]any
		path string
		body map[string]any
	}{
		{WriteOpRepositoryRescan, map[string]any{}, "/api/repos/rescan", map[string]any{}},
		{WriteOpRepositoryItemAcceptTriage, itemArgs("projectbluefin/chairlift", 639, nil), "/api/repos/projectbluefin/chairlift/items/639/accept-triage", map[string]any{}},
		{WriteOpRepositoryItemNeedsHuman, itemArgs("projectbluefin/chairlift", 639, map[string]any{"reason": "owner call"}), "/api/review/pipeline/projectbluefin/chairlift/639/send-to-human", map[string]any{"reason": "owner call"}},
		{WriteOpRepositoryItemBlockerRecheck, itemArgs("projectbluefin/chairlift", 639, nil), "/api/repos/projectbluefin/chairlift/items/639/blocker/recheck", map[string]any{}},
		{WriteOpRepositoryItemBlockerHandoff, itemArgs("projectbluefin/chairlift", 639, nil), "/api/repos/projectbluefin/chairlift/items/639/blocker/handoff", map[string]any{}},
		{WriteOpRepositoryItemBlockerClosePart, itemArgs("projectbluefin/chairlift", 639, nil), "/api/repos/projectbluefin/chairlift/items/639/blocker/close-partial", map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			pv, err := previewOp(t, tc.op, tc.args, p)
			if err != nil {
				t.Fatal(err)
			}
			if pv.Request.Method != "POST" || pv.Request.Path != tc.path || jsonString(pv.Request.Body) != jsonString(tc.body) {
				t.Fatalf("request = %+v", pv.Request)
			}
			if err := rejectUnpersistablePreview(tc.op, tc.args, pv); err != nil {
				t.Fatalf("preview cannot be stored for confirmation: %v", err)
			}
		})
	}
}

func TestItemPreviewShowsTheBoard(t *testing.T) {
	pv, err := previewOp(t, WriteOpRepositoryItemBlockerRecheck, itemArgs("projectbluefin/chairlift", 639, nil), &boardProvider{t: t})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Details["on_board"] != true || pv.Details["decision_question"] != "Which fix?" || pv.Details["blocker"] == nil {
		t.Fatalf("details = %#v", pv.Details)
	}
	off, err := previewOp(t, WriteOpRepositoryItemBlockerRecheck, itemArgs("projectbluefin/chairlift", 9999, nil), &boardProvider{t: t})
	if err != nil {
		t.Fatal(err)
	}
	if off.Details["on_board"] != false || !strings.Contains(strings.Join(off.Effects, " "), "not on the Activity monitor") {
		t.Fatalf("off-board preview = %#v", off)
	}
}

func TestIssueOnlyOpsRefusePullRequests(t *testing.T) {
	for _, op := range []string{WriteOpRepositoryItemAcceptTriage, WriteOpRepositoryItemBlockerRecheck, WriteOpRepositoryItemBlockerHandoff, WriteOpRepositoryItemBlockerClosePart} {
		if _, err := previewOp(t, op, itemArgs("projectbluefin/utah-packages", 892, nil), &boardProvider{t: t}); err == nil || !strings.Contains(err.Error(), "pull request") {
			t.Fatalf("%s on a PR: err = %v", op, err)
		}
	}
}

func TestNeedsHumanRequiresReason(t *testing.T) {
	if _, err := previewOp(t, WriteOpRepositoryItemNeedsHuman, itemArgs("projectbluefin/chairlift", 639, nil), nil); err == nil {
		t.Fatal("needs-human without a reason previewed")
	}
}

func TestItemDecisionRequests(t *testing.T) {
	p := &boardProvider{t: t}
	cases := []struct {
		name string
		args map[string]any
		body map[string]any
	}{
		{"approve issue", itemArgs("projectbluefin/chairlift", 639, map[string]any{"decision": "approve"}), map[string]any{"decision": "approve", "comment": "", "type": "issue"}},
		{"approve pr reads kind off the board", itemArgs("projectbluefin/utah-packages", 892, map[string]any{"decision": "approve"}), map[string]any{"decision": "approve", "comment": "", "type": "pr"}},
		{"direction", itemArgs("projectbluefin/chairlift", 639, map[string]any{"decision": "direction", "comment": "use option B"}), map[string]any{"decision": "direction", "comment": "use option B", "type": "issue"}},
		{"close superseded", itemArgs("projectbluefin/chairlift", 639, map[string]any{"decision": "close-superseded", "superseded_by": "#700"}), map[string]any{"decision": "close-superseded", "comment": "", "type": "issue", "superseded_by": "#700"}},
		{"adopt now", itemArgs("projectbluefin/utah-packages", 892, map[string]any{"decision": "adopt-now"}), map[string]any{"decision": "adopt", "force": true, "comment": "", "type": "pr"}},
		{"reroute defaults to any", itemArgs("projectbluefin/utah-packages", 892, map[string]any{"decision": "reroute"}), map[string]any{"decision": "reroute", "to": "any", "comment": "", "type": "pr"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pv, err := previewOp(t, WriteOpRepositoryItemDecision, tc.args, p)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(pv.Request.Path, "/decision") || jsonString(pv.Request.Body) != jsonString(tc.body) {
				t.Fatalf("request = %s %s", pv.Request.Path, jsonString(pv.Request.Body))
			}
			if err := rejectUnpersistablePreview(WriteOpRepositoryItemDecision, tc.args, pv); err != nil {
				t.Fatalf("preview cannot be stored for confirmation: %v", err)
			}
		})
	}
}

func TestItemDecisionRefusals(t *testing.T) {
	p := &boardProvider{t: t}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"merge-now", itemArgs("projectbluefin/utah-packages", 892, map[string]any{"decision": "merge-now"}), "queue-automerge"},
		{"queue-merge", itemArgs("projectbluefin/utah-packages", 892, map[string]any{"decision": "queue-merge"}), "queue-automerge"},
		{"unknown", itemArgs("projectbluefin/utah-packages", 892, map[string]any{"decision": "ship-it"}), "must be one of"},
		{"direction needs text", itemArgs("projectbluefin/chairlift", 639, map[string]any{"decision": "direction"}), "direction text"},
		{"close-issue on a pr", itemArgs("projectbluefin/utah-packages", 892, map[string]any{"decision": "close-issue"}), "pull request"},
		{"off board needs type", itemArgs("projectbluefin/chairlift", 9999, map[string]any{"decision": "approve"}), "type is required"},
		{"bad type", itemArgs("projectbluefin/chairlift", 639, map[string]any{"decision": "approve", "type": "epic"}), "issue or pr"},
		{"comment too long", itemArgs("projectbluefin/chairlift", 639, map[string]any{"decision": "direction", "comment": strings.Repeat("x", 4001)}), "exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := previewOp(t, WriteOpRepositoryItemDecision, tc.args, p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if p.calls == 0 {
		t.Fatal("decision previews never read the board")
	}
}

// ConfirmWrite re-runs Preview with no provider and compares the path: an
// item whose kind came off the board must still preview there.
func TestItemDecisionConfirmRecheckNeedsNoBoard(t *testing.T) {
	args := itemArgs("projectbluefin/utah-packages", 892, map[string]any{"decision": "approve"})
	withBoard, err := previewOp(t, WriteOpRepositoryItemDecision, args, &boardProvider{t: t})
	if err != nil {
		t.Fatal(err)
	}
	op, _ := DefaultWriteRegistry().Get(WriteOpRepositoryItemDecision)
	bare, err := op.Preview(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if bare.Request.Path != withBoard.Request.Path || bare.Request.Method != withBoard.Request.Method {
		t.Fatalf("re-check path %s != %s", bare.Request.Path, withBoard.Request.Path)
	}
}

func TestItemDelegateNeverMerges(t *testing.T) {
	args := itemArgs("projectbluefin/utah-packages", 892, nil)
	pv, err := previewOp(t, WriteOpRepositoryItemDelegate, args, &boardProvider{t: t})
	if err != nil {
		t.Fatal(err)
	}
	body := pv.Request.Body.(map[string]any)
	want := map[string]any{"type": "pr", "no_merge": true, "reason": "contributor-lane:review", "caption": "review pending", "sentinel_reason": "", "head": "abc123"}
	if jsonString(body) != jsonString(want) || !strings.HasSuffix(pv.Request.Path, "/decide-delegated") {
		t.Fatalf("request = %s %s", pv.Request.Path, jsonString(body))
	}
	if !strings.Contains(strings.Join(pv.Effects, " "), "Never merges") {
		t.Fatalf("effects = %v", pv.Effects)
	}
	if err := rejectUnpersistablePreview(WriteOpRepositoryItemDelegate, args, pv); err != nil {
		t.Fatalf("preview cannot be stored for confirmation: %v", err)
	}
	// The provider-less re-check still sends no_merge.
	op, _ := DefaultWriteRegistry().Get(WriteOpRepositoryItemDelegate)
	bare, err := op.Preview(context.Background(), itemArgs("projectbluefin/utah-packages", 892, nil))
	if err != nil || bare.Request.Body.(map[string]any)["no_merge"] != true {
		t.Fatalf("bare preview = %+v err=%v", bare.Request, err)
	}
}

func TestItemDelegateNestedPRsAndRerun(t *testing.T) {
	p := &boardProvider{t: t}
	pv, err := previewOp(t, WriteOpRepositoryItemDelegate, itemArgs("projectbluefin/utah-packages", 488, map[string]any{"prs": []any{float64(892), float64(893)}, "rerun": true}), p)
	if err != nil {
		t.Fatal(err)
	}
	body := pv.Request.Body.(map[string]any)
	if jsonString(body["prs"]) != "[892,893]" || body["force"] != true || body["type"] != "issue" {
		t.Fatalf("body = %s", jsonString(body))
	}
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{itemArgs("projectbluefin/utah-packages", 488, map[string]any{"prs": []any{float64(892)}}), "two or more"},
		{itemArgs("projectbluefin/utah-packages", 488, map[string]any{"prs": []any{1.5, float64(2)}}), "positive integers"},
		{itemArgs("projectbluefin/utah-packages", 892, map[string]any{"prs": []any{float64(1), float64(2)}}), "pull request"},
	} {
		if _, err := previewOp(t, WriteOpRepositoryItemDelegate, tc.args, p); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("args %v: err = %v", tc.args, err)
		}
	}
}

func TestBoardReadFailureNeedsExplicitType(t *testing.T) {
	p := &boardProvider{t: t, err: errors.New("stage tracker unavailable")}
	if _, err := previewOp(t, WriteOpRepositoryItemDelegate, itemArgs("projectbluefin/chairlift", 639, nil), p); err == nil || !strings.Contains(err.Error(), "type is required") {
		t.Fatalf("err = %v", err)
	}
	pv, err := previewOp(t, WriteOpRepositoryItemDelegate, itemArgs("projectbluefin/chairlift", 639, map[string]any{"type": "issue"}), p)
	if err != nil || pv.Details["on_board"] != false {
		t.Fatalf("explicit type: %+v err=%v", pv.Details, err)
	}
}

func TestDelegatedDecisionStatusPath(t *testing.T) {
	path, err := DelegatedDecisionStatusPath(itemArgs("projectbluefin/chair lift", 7, nil))
	if err != nil || path != "/api/repos/projectbluefin/chair%20lift/items/7/decide-delegated" {
		t.Fatalf("path = %q err=%v", path, err)
	}
	if _, err := DelegatedDecisionStatusPath(map[string]any{"owner": "o", "repo": "r"}); err == nil {
		t.Fatal("missing number accepted")
	}
}

type recordingWriter struct{ reqs []WriteRequest }

func (w *recordingWriter) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	w.reqs = append(w.reqs, req)
	return map[string]any{"ok": true}, nil
}

// A confirmation runs as the actor that previewed it: another caller's
// confirm is refused and leaves it usable by its owner.
func TestConfirmationIsBoundToThePreviewingActor(t *testing.T) {
	store := NewMemoryPendingStore()
	writer := &recordingWriter{}
	now := time.Now().UTC()
	as := func(actor string) *Handler {
		return NewHandler(nil, WithWritesEnabled(true), WithPendingStore(store), WithWriteClient(writer), WithActor(actor), WithClock(func() time.Time { return now }))
	}
	res, err := as("alice").PreviewWrite(context.Background(), map[string]any{"operation": WriteOpRepositoryRescan, "args": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.(map[string]any)["confirmation_id"].(string)
	if _, err := as("bob").ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationActor) {
		t.Fatalf("bob's confirm err = %v", err)
	}
	if _, err := as("").ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationActor) {
		t.Fatalf("anonymous confirm err = %v", err)
	}
	if len(writer.reqs) != 0 {
		t.Fatalf("a refused confirm wrote: %v", writer.reqs)
	}
	if _, err := as("alice").ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("alice's confirm err = %v", err)
	}
	if len(writer.reqs) != 1 || writer.reqs[0].Path != "/api/repos/rescan" {
		t.Fatalf("writes = %v", writer.reqs)
	}
}

func TestPendingActorPersists(t *testing.T) {
	store := NewFilePendingStore(t.TempDir() + "/pending.json")
	if err := store.Put(context.Background(), PendingConfirmation{ID: "a", Actor: "alice", Hive: "h", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Take(context.Background(), "a")
	if err != nil || got.Actor != "alice" {
		t.Fatalf("got %+v err=%v", got, err)
	}
}
