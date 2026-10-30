package dashboard

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestStageTrackerPreparationLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 30, 21, 0, 0, 0, time.UTC)
	ticket := func(n int, head string) automerge.BasePreparationMember {
		return automerge.BasePreparationMember{Number: n, HeadSHA: head, Since: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	}
	owner, waiter := ticket(7, "head7"), ticket(8, "head8")
	snapshot := automerge.BasePreparationSnapshot{Enabled: true, Present: true, Repo: "acme/widget", BaseRef: "v5", ObservedAt: now, Owner: &owner, Waiting: []automerge.BasePreparationMember{waiter}}
	s := &Server{deps: &Dependencies{Config: &config.Config{AutoMerge: config.AutoMergeConfig{RequireCurrentBase: []string{"acme/widget"}}}}}
	s.deps.BasePreparation = func(repo, base string) (automerge.BasePreparationSnapshot, error) {
		if repo != "acme/widget" || base != "v5" {
			t.Fatalf("wrong destination %q %q", repo, base)
		}
		return snapshot, nil
	}
	items := func() []StageTrackerItem {
		return []StageTrackerItem{
			{Repo: "Acme/Widget", Kind: "pr", Number: 7, BaseRef: "v5", Head: "head7", Stage: 50, Caption: "Review pending; check build still running"},
			{Repo: "acme/widget", Kind: "pr", Number: 8, BaseRef: "v5", Head: "head8", Stage: 50, Caption: "Waiting for a verdict on this head"},
			{Repo: "other/widget", Kind: "pr", Number: 7, BaseRef: "v5", Head: "head7", Stage: 50},
		}
	}
	rows := items()
	q := s.stageTrackerPreparation(rows, now)
	if len(q) != 1 || q[0].Owner.State != "current" || len(q[0].Waiting) != 1 || q[0].Waiting[0].Behind != 7 {
		t.Fatalf("owner/FIFO facts lost: %+v", q)
	}
	if rows[0].Preparation == nil || rows[1].Preparation == nil || rows[2].Preparation != nil {
		t.Fatal("annotations must match canonical repo, base, number, and head")
	}
	for _, row := range rows {
		if row.Stage != 50 || row.MergeTrain != nil {
			t.Fatalf("preparation fabricated a train or changed CI / Review: %+v", row)
		}
	}
	if !strings.Contains(rows[0].Preparation.Pipeline, "still running") || !strings.Contains(rows[1].Preparation.Pipeline, "verdict") {
		t.Fatal("pending CI/review must remain visible, not become ready")
	}
	snapshot.Owner, snapshot.Waiting = &waiter, nil
	rows = items()
	q = s.stageTrackerPreparation(rows, now)
	if q[0].Owner.Number != 8 || rows[0].Preparation != nil || rows[1].Preparation.Role != "owner" {
		t.Fatal("handoff retained the released owner")
	}
	snapshot.Owner = nil
	rows = items()
	q = s.stageTrackerPreparation(rows, now)
	if q[0].Owner != nil || rows[1].Preparation != nil || q[0].Message != "No recorded preparation tickets" {
		t.Fatal("release retained a preparation annotation")
	}
}

func TestStageTrackerPreparationUnknownAndInvalid(t *testing.T) {
	now := time.Date(2026, 10, 30, 21, 0, 0, 0, time.UTC)
	ticket := automerge.BasePreparationMember{Number: 7, HeadSHA: "head7", Since: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	for _, tc := range []struct {
		name     string
		edit     func(*StageTrackerItem, *automerge.BasePreparationSnapshot)
		readErr  error
		state    string
		annotate bool
	}{
		{name: "head changed", edit: func(it *StageTrackerItem, _ *automerge.BasePreparationSnapshot) { it.Head = "new-head" }, state: "unverified", annotate: true},
		{name: "head unknown", edit: func(it *StageTrackerItem, _ *automerge.BasePreparationSnapshot) { it.Head = "" }, state: "unverified", annotate: true},
		{name: "held", edit: func(it *StageTrackerItem, _ *automerge.BasePreparationSnapshot) { it.OnHold = true }, state: "inactive"},
		{name: "draft", edit: func(it *StageTrackerItem, _ *automerge.BasePreparationSnapshot) { it.Bucket = "draft" }, state: "inactive"},
		{name: "expired", edit: func(_ *StageTrackerItem, snap *automerge.BasePreparationSnapshot) { snap.Owner.ExpiresAt = now }, state: "inactive"},
		{name: "closed absent", edit: func(it *StageTrackerItem, _ *automerge.BasePreparationSnapshot) { it.Number = 8 }, state: "unverified"},
		{name: "expired absent", edit: func(it *StageTrackerItem, snap *automerge.BasePreparationSnapshot) {
			it.Number = 8
			snap.Owner.ExpiresAt = now
		}, state: "inactive"},
		{name: "store error", readErr: errors.New("unreadable"), state: "unavailable"},
		{name: "no snapshot", edit: func(_ *StageTrackerItem, snap *automerge.BasePreparationSnapshot) { snap.Present = false }, state: "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := ticket
			snap := automerge.BasePreparationSnapshot{Enabled: true, Present: true, ObservedAt: now, Owner: &owner}
			row := StageTrackerItem{Repo: "acme/widget", Kind: "pr", Number: 7, BaseRef: "v5", Head: "head7", Stage: 50}
			if tc.edit != nil {
				tc.edit(&row, &snap)
			}
			s := &Server{deps: &Dependencies{Config: &config.Config{AutoMerge: config.AutoMergeConfig{RequireCurrentBase: []string{"acme/widget"}}},
				BasePreparation: func(string, string) (automerge.BasePreparationSnapshot, error) { return snap, tc.readErr }}}
			rows := []StageTrackerItem{row}
			q := s.stageTrackerPreparation(rows, now)
			if len(q) != 1 {
				t.Fatalf("unknown queue silently omitted: %+v", q)
			}
			state := q[0].State
			if q[0].Owner != nil {
				state = q[0].Owner.State
				if q[0].Owner.State != "current" && q[0].Owner.Pipeline != "" {
					t.Fatal("mismatched head carried pipeline proof")
				}
			}
			if state != tc.state || (rows[0].Preparation != nil) != tc.annotate || rows[0].Stage != 50 || rows[0].MergeTrain != nil {
				t.Fatalf("state=%s row=%+v queue=%+v", state, rows[0], q[0])
			}
			s.deps.Config.AutoMerge.RequireCurrentBase = nil
			s.deps.BasePreparation = func(string, string) (automerge.BasePreparationSnapshot, error) {
				t.Fatal("inactive policy read storage")
				return snap, nil
			}
			if got := s.stageTrackerPreparation([]StageTrackerItem{row}, now); len(got) != 0 {
				t.Fatalf("inactive policy exposed stale queue: %+v", got)
			}
		})
	}
}

func TestStageTrackerPreparationRendering(t *testing.T) {
	html := indexHTML(t)
	var js strings.Builder
	js.WriteString("const assert = require('assert'); function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }\n")
	for _, name := range []string{"stageTrackerPreparationWord", "stageTrackerPreparationAge", "stageTrackerPreparationChipHtml", "stageTrackerPreparationPopHtml", "stageTrackerQueueTrains", "stageTrackerQueuePopHtml", "stageTrackerQueueSync", "stageTrackerQueueChip"} {
		js.WriteString(jsFunc(t, html, name) + "\n")
	}
	js.WriteString(`
const now = Date.parse('2026-10-30T21:00:00Z');
const owner = {role:'owner',state:'current',number:7,head_sha:'exact7',current_head:'exact7',since:'2026-10-30T20:50:00Z',pipeline:'Review pending; check build still running'};
const waiter = {role:'waiting',state:'current',number:8,head_sha:'exact8',current_head:'exact8',behind:7,since:'2026-10-30T20:55:00Z',pipeline:'Waiting for verdict'};
const queues = [{repo:'acme/widget',base_ref:'v5',observed_at:'2026-10-30T21:00:00Z',state:'observed',owner,waiting:[waiter]}];
const pop = stageTrackerQueuePopHtml([],now,queues);
assert.ok(pop.includes('Preparing base') && pop.includes('Waiting for preparation behind #7'),pop);
assert.ok(pop.includes('still running') && pop.includes('Waiting for verdict') && pop.includes('exact7') && pop.includes('10m'),pop);
assert.ok(pop.includes('No merge train right now') && pop.includes('not ready train seats'),pop);
assert.ok(pop.indexOf('#7') < pop.indexOf('#8'), 'persisted FIFO order');
assert.ok(stageTrackerPreparationChipHtml({preparation:owner}).includes('Preparing base'));
assert.strictEqual(stageTrackerPreparationChipHtml({preparation:{...owner,state:'inactive'}}),'');
assert.ok(stageTrackerPreparationWord({...owner,state:'unverified',detail:'Head changed'}).includes('Head changed'));
assert.ok(stageTrackerPreparationPopHtml([{repo:'acme/widget',state:'unavailable',message:'Preparation unknown: read failed'}],now).includes('Preparation unknown: read failed'));
assert.ok(!stageTrackerPreparationPopHtml([{...queues[0],owner:{...owner,pipeline:'<script>'}}],now).includes('<script>'));
var _stageTrackerQueueItems = [], _stageTrackerPreparationQueues = [];
const count = {textContent:''};
const chip = {hidden:true,title:'',querySelector:()=>count,classList:{toggle(){}}};
const document = {getElementById:()=>chip};
function stageTrackerQueuePopDraw(){}
stageTrackerQueueSync([],now,queues);
assert.strictEqual(chip.hidden,false);
assert.strictEqual(count.textContent,'0 train PRs · 2 prep tickets');
stageTrackerQueueSync([],now,[{state:'unavailable'}]);
assert.strictEqual(count.textContent,'0 train PRs · prep unknown');
stageTrackerQueueSync([],now,[]);
assert.strictEqual(chip.hidden,true);
`)
	runStageTrackerRowActionsScript(t, js.String())
}

func TestStageTrackerPreparationObservationDoesNotBustETag(t *testing.T) {
	now := time.Date(2026, 10, 30, 21, 0, 0, 0, time.UTC)
	for _, state := range []string{"observed", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			s, _ := stageTrackerFixture(t, now)
			s.deps = testDeps(t)
			message := "unchanged preparation evidence"
			s.stageTrackerBuildFn = func(_ *StatusPayload, _ *stagelabels.Store, at time.Time) StageTrackerResponse {
				return StageTrackerResponse{GeneratedAt: at, BasePreparation: []StageTrackerPreparationQueue{
					{Repo: "acme/widget", BaseRef: "v5", State: state, Message: message, ObservedAt: at},
				}}
			}
			first, ok := s.encodeStageTracker(nil, now, time.Minute)
			if !ok {
				t.Fatal("first encode failed")
			}
			next, ok := s.encodeStageTracker(nil, now.Add(time.Minute), time.Minute)
			if !ok || first.etag != next.etag || string(first.body) == string(next.body) {
				t.Fatal("read timestamps must remain in response without changing unchanged-state ETag")
			}
			message = "changed lifecycle evidence"
			changed, ok := s.encodeStageTracker(nil, now.Add(2*time.Minute), time.Minute)
			if !ok || first.etag == changed.etag {
				t.Fatal("preparation lifecycle changes must invalidate ETag")
			}
		})
	}
}
