package dashboard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The chip's queue facts (hivecommons/hive#15395): position / length, the
// head's state in plain words, the estimate and its basis, the stall flag.
func TestStageTrackerMergeTrainQueueFacts(t *testing.T) {
	now := time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)
	opts := mergeTrainQueueOptions{Now: now, Sweep: 5 * time.Minute, NextSweepAt: now.Add(40 * time.Second), StallWarn: time.Hour}
	since := func(ago time.Duration) *time.Time { at := now.Add(-ago); return &at }
	member := func(n, pos int, extra func(*StageTrackerItem)) *StageTrackerItem {
		it := &StageTrackerItem{Repo: "hivecommons/hive", Kind: "pr", Number: n, Stage: trackerNumberAwaitingMerge,
			MergeTrain: &StageTrackerMergeTrain{ID: "v5#15315", State: MergeGateStateQueuedBehind, Front: 15315, Position: pos, Size: 3}}
		if extra != nil {
			extra(it)
		}
		return it
	}
	soaking := func(left time.Duration) func(*StageTrackerItem) {
		return func(it *StageTrackerItem) {
			it.MergeGate = &StageTrackerMergeGate{State: "soaking", SoakRemainingSeconds: int64(left / time.Second)}
		}
	}
	headSoaking := &StageTrackerItem{Repo: "hivecommons/hive", Kind: "pr", Number: 15315, Stage: trackerNumberPreMergeSoak, StageSince: since(10 * time.Minute),
		MergeGate: &StageTrackerMergeGate{State: "soaking", SoakRemainingSeconds: 12 * 60}}
	headHuman := &StageTrackerItem{Repo: "hivecommons/hive", Kind: "pr", Number: 15315, Stage: trackerNumberReadyForHuman, StageSince: since(8 * time.Hour), HumanMergeReason: "untrusted-author",
		MergeGate: &StageTrackerMergeGate{State: MergeGateStateTrainFront}}
	headCI := &StageTrackerItem{Repo: "hivecommons/hive", Kind: "pr", Number: 15315, Stage: trackerNumberReviewed, StageSince: since(3 * time.Hour)}
	headMerging := &StageTrackerItem{Repo: "hivecommons/hive", Kind: "pr", Number: 15315, Stage: trackerNumberAwaitingMerge, StageSince: since(3 * time.Hour), MergeGate: &StageTrackerMergeGate{State: MergeGateStateMerging}}
	headReady := &StageTrackerItem{Repo: "hivecommons/hive", Kind: "pr", Number: 15315, Stage: trackerNumberAwaitingMerge, StageSince: since(20 * time.Minute), MergeGate: &StageTrackerMergeGate{State: MergeGateStateTrainFront}}

	cases := []struct {
		name      string
		self      *StageTrackerItem
		ahead     []*StageTrackerItem
		head      *StageTrackerItem
		opts      mergeTrainQueueOptions
		headState string
		reason    string
		etaS      int64
		basis     string
		stalled   bool
		stalledS  int64
	}{
		{name: "2nd behind a soaking head: head soak + next tick", self: member(15319, 2, nil), ahead: []*StageTrackerItem{headSoaking}, head: headSoaking, opts: opts,
			headState: mergeTrainHeadSoaking, reason: "soaking, 12m left", etaS: 12*60 + 40, basis: "1 ahead · head soaking 12m · next sweep in 40s"},
		{name: "3rd: head soak + one sweep for the 2nd + tick", self: member(15374, 3, nil), ahead: []*StageTrackerItem{headSoaking, member(15319, 2, nil)}, head: headSoaking, opts: opts,
			headState: mergeTrainHeadSoaking, reason: "soaking, 12m left", etaS: 12*60 + 5*60 + 40, basis: "2 ahead · head soaking 12m · next sweep in 40s"},
		{name: "3rd with a soaking 2nd: its soak, not a sweep", self: member(15374, 3, nil), ahead: []*StageTrackerItem{headSoaking, member(15319, 2, soaking(7*time.Minute))}, head: headSoaking, opts: opts,
			headState: mergeTrainHeadSoaking, reason: "soaking, 12m left", etaS: 12*60 + 7*60 + 40, basis: "2 ahead · head soaking 12m · next sweep in 40s"},
		{name: "own soak counts too", self: member(15319, 2, soaking(3*time.Minute)), ahead: []*StageTrackerItem{headSoaking}, head: headSoaking, opts: opts,
			headState: mergeTrainHeadSoaking, reason: "soaking, 12m left", etaS: 12*60 + 3*60 + 40, basis: "1 ahead · head soaking 12m · own soak 3m · next sweep in 40s"},
		{name: "a running sweep has no tick", self: member(15319, 2, func(it *StageTrackerItem) {
			it.MergeGate = &StageTrackerMergeGate{State: MergeGateStateQueuedBehind, SweepRunning: true}
		}), ahead: []*StageTrackerItem{headSoaking}, head: headSoaking, opts: opts,
			headState: mergeTrainHeadSoaking, reason: "soaking, 12m left", etaS: 12 * 60, basis: "1 ahead · head soaking 12m · sweep running"},
		{name: "ready head: one sweep each + tick", self: member(15374, 3, nil), ahead: []*StageTrackerItem{headReady, member(15319, 2, nil)}, head: headReady, opts: opts,
			headState: mergeTrainHeadReady, reason: "approved and green; merges on hive's next sweep", etaS: 2*5*60 + 40, basis: "2 ahead · next sweep in 40s"},
		{name: "no cadence armed yet: the default floor", self: member(15319, 2, nil), ahead: []*StageTrackerItem{headReady}, head: headReady, opts: mergeTrainQueueOptions{Now: now, StallWarn: time.Hour},
			headState: mergeTrainHeadReady, reason: "approved and green; merges on hive's next sweep", etaS: int64(mergeTrainDefaultSweepEvery / time.Second), basis: "1 ahead"},
		{name: "merging head: its turn is one sweep away", self: member(15319, 2, nil), ahead: []*StageTrackerItem{headMerging}, head: headMerging, opts: opts,
			headState: mergeTrainHeadMerging, reason: "merging now", etaS: 5*60 + 40, basis: "1 ahead · head merging · next sweep in 40s"},
		{name: "head on a human: blocker, no estimate, stalled past the window", self: member(15319, 2, nil), ahead: []*StageTrackerItem{headHuman}, head: headHuman, opts: opts,
			headState: mergeTrainHeadNeedsHuman, reason: "waiting for a maintainer (untrusted author)", stalled: true, stalledS: 8 * 3600},
		{name: "head on CI: blocker, no estimate", self: member(15319, 2, nil), ahead: []*StageTrackerItem{headCI}, head: headCI, opts: opts,
			headState: mergeTrainHeadCI, reason: "waiting for CI", stalled: true, stalledS: 3 * 3600},
		{name: "head not in view: unknown, no estimate, no stall", self: member(15319, 2, nil), ahead: nil, head: nil, opts: opts,
			headState: mergeTrainHeadUnknown, reason: "the head of the train is not in this view"},
		{name: "stall_warn 0 turns the warning off", self: member(15319, 2, nil), ahead: []*StageTrackerItem{headHuman}, head: headHuman, opts: mergeTrainQueueOptions{Now: now, Sweep: opts.Sweep},
			headState: mergeTrainHeadNeedsHuman, reason: "waiting for a maintainer (untrusted author)"},
		{name: "a member on a person has no estimate either", self: member(15319, 2, func(it *StageTrackerItem) { it.NeedsHuman = true }), ahead: []*StageTrackerItem{headSoaking}, head: headSoaking, opts: opts,
			headState: mergeTrainHeadSoaking, reason: "soaking, 12m left"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stageTrackerMergeTrainQueueFacts(tc.self, tc.ahead, tc.head, tc.opts, nil)
			mt := tc.self.MergeTrain
			if mt.Length != 3 || mt.HeadNumber != 15315 {
				t.Fatalf("length/head = %d/%d", mt.Length, mt.HeadNumber)
			}
			if mt.HeadState != tc.headState || mt.HeadReason != tc.reason {
				t.Fatalf("head = %q %q, want %q %q", mt.HeadState, mt.HeadReason, tc.headState, tc.reason)
			}
			if mt.EtaS != tc.etaS || mt.EtaBasis != tc.basis {
				t.Fatalf("eta_s = %d (%q), want %d (%q)", mt.EtaS, mt.EtaBasis, tc.etaS, tc.basis)
			}
			if tc.etaS > 0 && (mt.EtaAt == nil || !mt.EtaAt.Equal(now.Add(time.Duration(tc.etaS)*time.Second))) {
				t.Fatalf("eta_at = %v, want now+%ds", mt.EtaAt, tc.etaS)
			}
			if tc.etaS == 0 && mt.EtaAt != nil {
				t.Fatalf("no estimate must carry no eta_at: %v", mt.EtaAt)
			}
			if mt.Stalled != tc.stalled || mt.StalledS != tc.stalledS {
				t.Fatalf("stalled = %v/%ds, want %v/%ds", mt.Stalled, mt.StalledS, tc.stalled, tc.stalledS)
			}
		})
	}
}

// "Running late": a member still waiting twice past its first estimate at
// the same position reads late; a new position starts a fresh estimate;
// a blocked spell forgets the mark.
func TestStageTrackerMergeTrainEtaLate(t *testing.T) {
	now := time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)
	head := &StageTrackerItem{Repo: "hivecommons/hive", Kind: "pr", Number: 15315, Stage: trackerNumberAwaitingMerge, MergeGate: &StageTrackerMergeGate{State: MergeGateStateTrainFront}}
	self := &StageTrackerItem{Repo: "hivecommons/hive", Kind: "pr", Number: 15319, Stage: trackerNumberAwaitingMerge,
		MergeTrain: &StageTrackerMergeTrain{ID: "v5#15315", State: MergeGateStateQueuedBehind, Front: 15315, Position: 2, Size: 2}}
	var mem mergeTrainMemory
	at := func(d time.Duration) mergeTrainQueueOptions {
		return mergeTrainQueueOptions{Now: now.Add(d), Sweep: 5 * time.Minute}
	}
	run := func(d time.Duration) *StageTrackerMergeTrain {
		stageTrackerMergeTrainQueueFacts(self, []*StageTrackerItem{head}, head, at(d), &mem)
		return self.MergeTrain
	}
	first := now.Add(5 * time.Minute)
	if mt := run(0); mt.EtaS != 300 || mt.Late || !mt.EtaAt.Equal(first) {
		t.Fatalf("first estimate = %+v", mt)
	}
	// Two polls later the fresh estimate would end 2m later; the anchor
	// holds so the countdown does not reset.
	if mt := run(2 * time.Minute); !mt.EtaAt.Equal(first) || mt.Late {
		t.Fatalf("anchored eta_at = %v, want %v (%+v)", mt.EtaAt, first, mt)
	}
	if mt := run(9 * time.Minute); mt.Late || !mt.EtaAt.Equal(now.Add(14*time.Minute)) {
		t.Fatalf("under 2x the first estimate is not late; a passed anchor re-reads fresh: %+v", mt)
	}
	if mt := run(11 * time.Minute); !mt.Late || mt.EtaS != 300 {
		t.Fatalf("past 2x the first estimate reads late, estimate still shown: %+v", mt)
	}
	// A materially larger estimate (the head's soak restarted) re-anchors.
	head.Soak = &StageTrackerSoak{Until: now.Add(11*time.Minute + 30*time.Second + 30*time.Minute)}
	if mt := run(11*time.Minute + 30*time.Second); mt.Late || !mt.EtaAt.Equal(now.Add(11*time.Minute+30*time.Second+30*time.Minute)) {
		t.Fatalf("grown estimate re-anchors: %+v", mt)
	}
	head.Soak = nil
	// Moving up resets the mark.
	self.MergeTrain.Position = 1
	if mt := run(12 * time.Minute); mt.Late {
		t.Fatalf("a new position starts over: %+v", mt)
	}
	// A blocked spell forgets it; unblocked, the clock starts again.
	self.NeedsHuman = true
	if mt := run(13 * time.Minute); mt.EtaS != 0 || mt.Late {
		t.Fatalf("blocked member: %+v", mt)
	}
	self.NeedsHuman = false
	if mt := run(40 * time.Minute); mt.Late {
		t.Fatalf("fresh estimate after a block: %+v", mt)
	}
}

// The stall line reaches the activity log once per hour per train, not
// once per sweep nor once per member.
func TestStageTrackerMergeTrainStallLogHourly(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	now := time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)
	mt := func() *StageTrackerMergeTrain {
		return &StageTrackerMergeTrain{ID: "v5#15315", Front: 15315, Size: 3, HeadNumber: 15315, HeadState: mergeTrainHeadNeedsHuman, HeadReason: "waiting for a maintainer", Stalled: true, StalledS: 4 * 3600}
	}
	items := []StageTrackerItem{
		{Repo: "hivecommons/hive", Kind: "pr", Number: 15319, MergeTrain: mt()},
		{Repo: "hivecommons/hive", Kind: "pr", Number: 15374, MergeTrain: mt()},
		{Repo: "hivecommons/hive", Kind: "pr", Number: 15400},
	}
	var log mergeTrainMemory
	stageTrackerMergeTrainStallWarnings(&log, items, now)
	stageTrackerMergeTrainStallWarnings(&log, items, now.Add(10*time.Minute))
	if n := strings.Count(buf.String(), "merge train stalled"); n != 1 {
		t.Fatalf("lines after two sweeps in 10m = %d, want 1:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "head=15315") || !strings.Contains(buf.String(), "stalled_for=4h") || !strings.Contains(buf.String(), "2 PR(s) wait behind it") {
		t.Fatalf("line = %s", buf.String())
	}
	stageTrackerMergeTrainStallWarnings(&log, items, now.Add(61*time.Minute))
	if n := strings.Count(buf.String(), "merge train stalled"); n != 2 {
		t.Fatalf("lines after the hour = %d, want 2", n)
	}
	items[0].MergeTrain.Stalled, items[1].MergeTrain.Stalled = false, false
	stageTrackerMergeTrainStallWarnings(&log, items, now.Add(3*time.Hour))
	if n := strings.Count(buf.String(), "merge train stalled"); n != 2 {
		t.Fatalf("a recovered train must not log: %d", n)
	}
}

// TestStageTrackerMergeQueueChipNode: a train member's chip reads its queue
// position and nothing else — the same label whether or not the merge
// sweep is running (the sweep is the spinning glyph and a tooltip line) —
// and the sticky bar's Merge queue popover lists the train in order.
func TestStageTrackerMergeQueueChipNode(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="activity-monitor-queue" data-action="hamQueueToggle" data-stop="1" aria-haspopup="dialog" aria-expanded="false" aria-controls="activity-monitor-queue-pop"`,
		`<div class="ham-merged-pop ham-queue-pop" id="activity-monitor-queue-pop" role="dialog" aria-label="Merge queue" hidden></div>`,
		`if (typeof stageTrackerQueueSync === 'function') stageTrackerQueueSync(d && d.items, Date.now());`,
		`.ham-queue-stalled { color: var(--yellow); }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing merge-queue wiring %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, "function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }\nconst assert = require('assert');\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_GATE_WORD")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_ICON_PATH")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_ICON_TITLE")+"\n"+
		jsFunc(t, html, "stageTrackerTrainIconHtml")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_GATE_REASON_WORDS")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS")+"\n"+
		jsFunc(t, html, "stageTrackerIcon")+"\n"+
		jsFunc(t, html, "stageTrackerAgoText")+"\n"+
		jsFunc(t, html, "stageTrackerSoakLeft")+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateStateOf")+"\n"+
		jsFunc(t, html, "stageTrackerGateReasonText")+"\n"+
		stageTrackerMergeGateInfoJS(t, html)+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateWordHtml")+"\n"+
		jsFunc(t, html, "stageTrackerGateCueHtml")+"\n"+
		jsFunc(t, html, "stageTrackerGateCountdownSync")+"\n"+
		jsFunc(t, html, "stageTrackerQueueTrains")+"\n"+
		jsFunc(t, html, "stageTrackerQueueMemberState")+"\n"+
		jsFunc(t, html, "stageTrackerQueuePopHtml")+"\n"+`
const now = Date.parse('2026-10-29T15:06:00Z');
const mk = (n, title, mt, gate) => ({ repo: 'hivecommons/hive', number: n, kind: 'pr', title, url: 'https://github.com/hivecommons/hive/pull/' + n, stage: 80, modifiers: [],
  merge_gate: Object.assign({ state: mt.state, reason: 'behind #15315 in merge train', last_sweep_at: '2026-10-29T15:05:30Z' }, gate || {}),
  merge_train: Object.assign({ id: 'v5#15315', front: 15315, size: 3, length: 3, head_number: 15315, overlap_files: ['fleetreport.go'] }, mt) });
const headFacts = { head_state: 'needs-human', head_reason: 'waiting for a maintainer (untrusted author)', eta_s: 0 };
const quiet = mk(15319, 'fleetreport: slog feed', Object.assign({ state: 'queued-behind', position: 2, ahead: [15315] }, headFacts), { sweep_running: false });
const busy = mk(15319, 'fleetreport: slog feed', Object.assign({ state: 'queued-behind', position: 2, ahead: [15315] }, headFacts), { sweep_running: true });
const a = stageTrackerMergeGateInfo(quiet, now), b = stageTrackerMergeGateInfo(busy, now);
// Stability: the sweep flag changes neither the word nor the detail.
assert.strictEqual(a.word, 'Merge queued · 2nd');
assert.strictEqual(b.word, a.word, 'sweep_running must not change the chip label');
assert.strictEqual(a.detail, ''); assert.strictEqual(b.detail, '');
assert.strictEqual(stageTrackerMergeGateWordHtml(quiet, now), stageTrackerMergeGateWordHtml(busy, now), 'identical chip markup');
assert.ok(!/queued-behind|front|merge sweep running/i.test(a.word + a.detail + b.word + b.detail), 'no slug, no "front", no sweep words in the label');
// The sweep lives in the glyph and the tooltip.
assert.strictEqual(a.cue, 'waiting'); assert.strictEqual(b.cue, 'running');
assert.ok(b.tip.includes('Checking now…') && !a.tip.includes('Checking now…'), b.tip);
assert.ok(a.tip.startsWith('2nd of 3 in the merge queue · behind #15315 (waiting for a maintainer (untrusted author)) · no estimate until #15315 moves. Hive merges one at a time, in order.'), a.tip);
assert.ok(a.tip.includes('Last checked'), a.tip);
assert.ok(stageTrackerGateCueHtml(busy, now).includes('cue-spin') && !stageTrackerGateCueHtml(quiet, now).includes('cue-spin'));
// A soaking head: the estimate is the head's soak plus one slot per PR ahead.
const soakHead = mk(15374, 'tracker: queue', { state: 'queued-behind', position: 3, ahead: [15315, 15319], head_state: 'soaking', head_reason: 'soaking, 12m left', eta_s: 25 * 60 }, { sweep_running: true });
const s = stageTrackerMergeGateInfo(soakHead, now);
assert.strictEqual(s.word, 'Merge queued · 3rd');
assert.ok(s.tip.startsWith('3rd of 3 in the merge queue · behind #15315 (soaking, 12m left) · ETA ~25m. Hive merges one at a time, in order. Checking now…'), s.tip);
// Stalled: the chip says so, names the head and how long; the markup wears the stalled class.
const stalled = mk(15374, 'tracker: queue', Object.assign({ state: 'queued-behind', position: 3, ahead: [15315, 15319], stalled: true, stalled_s: 3 * 3600 }, headFacts), { sweep_running: true });
const st = stageTrackerMergeGateInfo(stalled, now);
assert.strictEqual(st.word, 'Merge queue · stalled — waiting on #15315 (needs maintainer merge) · 3h');
assert.ok(stageTrackerMergeGateWordHtml(stalled, now).includes('class="ham-seg-t ham-queue-stalled"'));
assert.strictEqual(stageTrackerMergeGateInfo(mk(15374, 't', Object.assign({ state: 'queued-behind', position: 3, stalled: true, stalled_s: 3 * 3600 }, headFacts), { sweep_running: false }), now).word, st.word, 'stalled label is sweep-stable too');
// The front row reads its place too.
const head = mk(15315, 'fleetreport: head', Object.assign({ state: 'front', position: 1, ahead: [] }, headFacts));
assert.strictEqual(stageTrackerMergeGateInfo(head, now).word, 'Merge queued · 1st');
assert.ok(stageTrackerMergeGateInfo(head, now).tip.startsWith('1st of 3 in the merge queue · merges next (waiting for a maintainer (untrusted author)). Hive merges one at a time, in order.'), stageTrackerMergeGateInfo(head, now).tip);
// hive's own #15406 read the bare slug "front" while the front sat in its
// soak (the mirrored soak clock returned the raw state). A front in soak
// keeps its place word and its countdown (own soak left + next sweep).
const frontSoak = mk(15406, 'in soak at the front', { state: 'front', position: 1, size: 4, length: 4, head_number: 15406, front: 15406, ahead: [], head_state: 'soaking', head_reason: 'soaking, 12m left', eta_s: 760, eta_at: new Date(now + 760 * 1000).toISOString(), eta_basis: 'own soak 12m · next sweep in 40s' },
  { state: 'front', soak_until: new Date(now + 12 * 60 * 1000).toISOString(), soak_started_at: new Date(now - 18 * 60 * 1000).toISOString(), sweep_running: true });
const fs = stageTrackerMergeGateInfo(frontSoak, now);
assert.strictEqual(fs.word, 'Merge queued · 1st', fs.word); assert.strictEqual(fs.detail, '~13m');
assert.strictEqual(stageTrackerMergeGateInfo(Object.assign({}, frontSoak, { merge_gate: Object.assign({}, frontSoak.merge_gate, { sweep_running: false }) }), now).word, fs.word);
assert.strictEqual(stageTrackerMergeGateWordHtml(frontSoak, now).replace(/<[^>]+>/g, ''), 'Merge queued · 1st · ~13m');
// Countdown to auto-merge: eta_at drives "· ~4m" on the chip; the detail carries
// the clock so the shared tick keeps counting and a re-render lands on the same
// instant (no reset on poll).
const etaAt = new Date(now + 4 * 60 * 1000).toISOString();
const timed = mk(15319, 'fleetreport: slog feed', { state: 'queued-behind', position: 2, ahead: [15315], head_state: 'soaking', head_reason: 'soaking, 3m left', eta_s: 240, eta_at: etaAt, eta_basis: '1 ahead · head soaking 3m · next sweep in 40s' }, { sweep_running: false });
const ti = stageTrackerMergeGateInfo(timed, now);
assert.strictEqual(ti.word, 'Merge queued · 2nd'); assert.strictEqual(ti.detail, '~4m'); assert.strictEqual(ti.until, now + 4 * 60 * 1000);
assert.ok(ti.tip.includes('· ETA ~4m (1 ahead · head soaking 3m · next sweep in 40s).'), ti.tip);
const chipHtml = stageTrackerMergeGateWordHtml(timed, now);
assert.ok(chipHtml.includes('>Merge queued · 2nd <span class="ham-gate-detail" data-ham-until="' + (now + 240000) + '" data-ham-until-kind="queued-behind">· ~4m</span>'), chipHtml);
assert.strictEqual(chipHtml.replace(/<[^>]+>/g, ''), 'Merge queued · 2nd · ~4m');
// The same payload 90s later (a poll re-render) reads 2m30s, not 4m again.
assert.strictEqual(stageTrackerMergeGateInfo(timed, now + 90 * 1000).detail, '~3m');
assert.strictEqual(stageTrackerMergeGateInfo(timed, now + 150 * 1000).detail, '~2m');
// The shared tick redoes the detail node in place.
const node = { attrs: { 'data-ham-until': String(now + 240000), 'data-ham-until-kind': 'queued-behind' }, textContent: '· ~4m', getAttribute(k) { return this.attrs[k] === undefined ? null : this.attrs[k]; } };
const el = { querySelectorAll() { return [node]; } };
stageTrackerGateCountdownSync(el, now + 150 * 1000); assert.strictEqual(node.textContent, '· ~2m');
stageTrackerGateCountdownSync(el, now + 5 * 60 * 1000); assert.strictEqual(node.textContent, '· any sweep now');
// Running late: no clock, the words instead; the estimate stays in the hover.
const late = mk(15319, 'fleetreport: slog feed', { state: 'queued-behind', position: 2, ahead: [15315], head_state: 'ready', head_reason: 'approved and green; merges on hive\'s next sweep', eta_s: 300, eta_at: etaAt, eta_basis: '1 ahead', late: true });
const li = stageTrackerMergeGateInfo(late, now);
assert.strictEqual(li.detail, 'running late'); assert.strictEqual(li.until, 0);
assert.ok(li.tip.includes('Running late — estimated ~5m, still waiting (1 ahead).'), li.tip);
// Blocked (head on a person / CI): no clock, the blocker instead.
assert.strictEqual(stageTrackerMergeGateInfo(quiet, now).detail, '');
assert.strictEqual(stageTrackerMergeGateInfo(stalled, now).detail, '');
assert.strictEqual(stageTrackerMergeGateInfo(mk(15319, 't', { state: 'queued-behind', position: 2, head_state: 'ci', head_reason: 'waiting for CI', eta_s: 0 }), now).detail, '');
// The popover: trains in merge order with #, title, state words and ETA; no slugs.
const other = { repo: 'hivecommons/hive', number: 1, kind: 'pr', stage: 50, title: 'no train' };
const pop = stageTrackerQueuePopHtml([soakHead, other, head, quiet], now);
assert.ok(stageTrackerQueuePopHtml([timed, head], now).includes('ETA ~4m') && stageTrackerQueuePopHtml([late, head], now).includes('running late'));
assert.ok(pop.includes('<span>Merge queue</span>') && pop.includes('3 PRs in 1 train'), pop);
const order = [...pop.matchAll(/pull\/(\d+)"/g)].map(m => m[1]);
assert.deepStrictEqual(order, ['15315', '15319', '15374'], pop);
assert.ok(pop.includes('1. <a') && pop.includes('>fleetreport: head<') && pop.includes('waiting for a maintainer (untrusted author)'), pop);
assert.ok(pop.includes('3. <a') && pop.includes('ETA ~25m'), pop);
assert.ok(pop.includes('head #15315'), pop);
assert.ok(!/queued-behind|rebase-exhausted/.test(pop), 'popover has no slugs');
assert.ok(stageTrackerQueuePopHtml([other], now).includes('No merge train right now'));
console.log('merge queue chip ok');
`)
}

// mergeTrainStateSlugs is every merge_train / merge_gate train state the
// server can emit; the SPA must word each one (hivecommons/hive#15395 —
// hive's own #15406 once read the bare slug "front").
var mergeTrainStateSlugs = []string{
	prstage.MergeTrainStateFront,
	prstage.MergeTrainStateQueuedBehind,
	prstage.MergeTrainStateRebasing,
	prstage.MergeTrainStateRebaseExhausted,
	prstage.MergeTrainStateDuplicateOf,
}

// mergeTrainHeadStates is every head_state a merge train reports on its
// members; STAGE_TRACKER_TRAIN_HEAD_STALL_WORD must word each one.
var mergeTrainHeadStates = []string{
	mergeTrainHeadNeedsHuman, mergeTrainHeadCI, mergeTrainHeadSoaking, mergeTrainHeadReady,
	mergeTrainHeadMerging, mergeTrainHeadFixing, mergeTrainHeadUnknown,
}

// TestStageTrackerMergeTrainPayloadStates: every merge_train the tracker
// emits carries a state from the documented set and, once the queue facts
// are on it, a head_state from the documented set — the SPA's word tables
// are keyed on exactly these (hivecommons/hive#15530's chevrons read empty).
func TestStageTrackerMergeTrainPayloadStates(t *testing.T) {
	now := time.Date(2026, 10, 29, 15, 6, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	overlap := []string{"tracker-row.js", "index.html"}
	store.Observe(stagelabels.Key("acme/console", 15425), stagelabels.ReadyForHumanMerge, "")
	for i, n := range []int{15529, 15530} {
		store.Observe(stagelabels.Key("acme/console", n), stagelabels.AwaitingMergeAdmission,
			prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 15425, Position: i + 2, Size: 3, OverlapFiles: overlap}.Reason())
	}
	s.statusMu.Lock()
	for _, n := range []int{15425, 15529, 15530} {
		stage := "stage/080-awaiting-merge-admission"
		if n == 15425 {
			stage = "stage/090-ready-for-human-merge"
		}
		s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
			FrontendPR{PullRequest: github.PullRequest{Number: n, Title: "Tracker change", Author: "someone", BaseRef: "v5", Labels: []string{stage}}})
	}
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)
	seen := 0
	for _, it := range resp.Items {
		mt := it.MergeTrain
		if mt == nil {
			continue
		}
		seen++
		if !slices.Contains(mergeTrainStateSlugs, mt.State) {
			t.Errorf("#%d merge_train.state %q is not a documented train state %v", it.Number, mt.State, mergeTrainStateSlugs)
		}
		if mt.HeadState != "" && !slices.Contains(mergeTrainHeadStates, mt.HeadState) {
			t.Errorf("#%d merge_train.head_state %q is not a documented head state %v", it.Number, mt.HeadState, mergeTrainHeadStates)
		}
		if mt.Position <= 0 || mt.Size <= 0 {
			t.Errorf("#%d merge_train has no place: %+v", it.Number, mt)
		}
	}
	if seen < 3 {
		t.Fatalf("expected the head and both riders to carry merge_train, saw %d", seen)
	}
}

// TestStageTrackerMergeTrainStateSlugsNeverLeak renders every train state
// through the chip words (with and without a soak clock and a running
// sweep) and fails if a slug shows up where a person reads.
func TestStageTrackerMergeTrainStateSlugsNeverLeak(t *testing.T) {
	html := indexHTML(t)
	table := jsConstLine(t, html, "STAGE_TRACKER_TRAIN_STATE_WORD")
	for _, slug := range mergeTrainStateSlugs {
		if !strings.Contains(table, "'"+slug+"'") && !strings.Contains(table, " "+slug+":") {
			t.Errorf("STAGE_TRACKER_TRAIN_STATE_WORD has no words for %q: %s", slug, table)
		}
	}
	slugsJSON, _ := json.Marshal(mergeTrainStateSlugs)
	headStatesJSON, _ := json.Marshal(mergeTrainHeadStates)
	stallTable := jsConstLine(t, html, "STAGE_TRACKER_TRAIN_HEAD_STALL_WORD")
	for _, hs := range mergeTrainHeadStates {
		if !strings.Contains(stallTable, "'"+hs+"'") && !strings.Contains(stallTable, " "+hs+":") {
			t.Errorf("STAGE_TRACKER_TRAIN_HEAD_STALL_WORD has no words for head_state %q: %s", hs, stallTable)
		}
	}
	runStageTrackerRowActionsScript(t, "function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }\nconst assert = require('assert');\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_GATE_WORD")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_ICON_PATH")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_ICON_TITLE")+"\n"+
		jsFunc(t, html, "stageTrackerTrainIconHtml")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_GATE_REASON_WORDS")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS")+"\n"+
		jsFunc(t, html, "stageTrackerIcon")+"\n"+
		jsFunc(t, html, "stageTrackerAgoText")+"\n"+
		jsFunc(t, html, "stageTrackerSoakLeft")+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateStateOf")+"\n"+
		jsFunc(t, html, "stageTrackerGateReasonText")+"\n"+
		stageTrackerMergeGateInfoJS(t, html)+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateWordHtml")+"\n"+
		jsFunc(t, html, "stageTrackerGateCueHtml")+"\n"+
		jsFunc(t, html, "stageTrackerQueueMemberState")+"\n"+`
const slugs = `+string(slugsJSON)+`;
const headStates = `+string(headStatesJSON)+`;
const now = Date.parse('2026-10-29T15:06:00Z');
// A leak is the slug standing alone (the whole label or one " · " segment,
// case-sensitive: slugs are lowercase, "Rebasing · attempt 1/2" is words)
// or a hyphenated slug anywhere.
const leaks = (s, state) => { s = String(s || ''); return s.trim() === state || s.split(/\s*·\s*/).some(seg => seg.trim() === state) || (state.includes('-') && s.includes(state)); };
// Owner (hive #15400 / #15425 rows): neither "front" nor "queued-behind"
// anywhere a person reads, whatever the state.
const ownerBanned = s => /front|queued-behind/i.test(String(s || ''));
const soak = { soak_until: new Date(now + 9 * 60 * 1000).toISOString(), soak_started_at: new Date(now - 60 * 1000).toISOString() };
for (const state of slugs) {
  for (const sweep of [true, false]) for (const clock of [null, soak]) {
    // A held duplicate rides no train: the gate alone says "duplicate-of:<n>".
    const dup = state === 'duplicate-of';
    const mt = { id: 'v5#15406', state, front: 15406, head_number: 15406, position: state === 'front' ? 1 : 2, size: 4, length: 4, attempt: 1, max_attempts: 2, ahead: state === 'front' ? [] : [15406] };
    const item = { repo: 'hivecommons/hive', number: 15407, kind: 'pr', title: 't', stage: 80, modifiers: [], merge_train: dup ? undefined : mt,
      merge_gate: Object.assign({ state: dup ? state + ':15402' : state, sweep_running: sweep, duplicate_of: dup ? 15402 : 0, duplicate_issue: dup ? 15396 : 0 }, clock || {}) };
    const word = dup ? stageTrackerMergeGateStateWord(item.merge_gate.state) : stageTrackerTrainWord(item);
    assert.ok(word && !leaks(word, state), state + ' train word: ' + JSON.stringify(word));
    const info = stageTrackerMergeGateInfo(item, now);
    assert.ok(info.word && !leaks(info.word, state) && !leaks(info.detail, state), state + ' chip: ' + JSON.stringify(info));
    assert.ok(!leaks(stageTrackerMergeGateWordHtml(item, now).replace(/<[^>]+>/g, ''), state), state + ' html');
    assert.ok(!leaks(stageTrackerQueueMemberState(item, now), state), state + ' popover');
    for (const [what, text] of [['word', word], ['chip', info.word + ' ' + info.detail], ['hover', info.tip], ['popover', stageTrackerQueueMemberState(item, now)]]) assert.ok(!ownerBanned(text), state + ' ' + what + ' says front/queued-behind: ' + JSON.stringify(text));
  }
}
// The word table covers the slugs and an unmapped state still spells out.
for (const state of slugs) assert.ok(STAGE_TRACKER_TRAIN_STATE_WORD[state], state);
// Ratchet (hivecommons/hive#15530: two riders behind an untrusted-author
// head drew amber chevrons with no words): every train state × stalled or
// not × every head_state × with and without a position yields words on the
// chevron — stageTrackerTrainWord, the gate chip and its markup stripped
// of tags — never '' and never the slug.
for (const state of slugs.filter(s => s !== 'duplicate-of')) for (const stalled of [false, true]) for (const hs of [...headStates, '']) for (const pos of [0, 1, 2, 3]) {
  const mt = { id: 'v5#15425', state, front: 15425, head_number: 15425, size: 3, length: 3, attempt: 1, max_attempts: 2, head_state: hs, head_reason: hs === 'needs-human' ? 'waiting for a maintainer (untrusted author)' : '', stalled, stalled_s: stalled ? 4851 : 0 };
  if (pos) mt.position = pos;
  const item = { repo: 'hivecommons/hive', number: 15530, kind: 'pr', title: 't', stage: 80, modifiers: stalled ? ['stalled'] : [], merge_train: mt, merge_gate: { state, sweep_running: true, last_sweep_at: '2026-10-29T15:05:30Z' } };
  const label = state + (stalled ? '/stalled' : '') + '/' + (hs || 'no-head') + '/pos' + pos;
  const word = stageTrackerTrainWord(item);
  assert.ok(word && word.trim() && !leaks(word, state), label + ' train word: ' + JSON.stringify(word));
  const info = stageTrackerMergeGateInfo(item, now);
  assert.ok(info && info.word && info.word.trim(), label + ' chip word: ' + JSON.stringify(info));
  const text = stageTrackerMergeGateWordHtml(item, now).replace(/<[^>]+>/g, '').trim();
  assert.ok(text && !leaks(text, state), label + ' chip html: ' + JSON.stringify(text));
  if (stalled && (state === 'front' || state === 'queued-behind')) assert.ok(word.startsWith(STAGE_TRACKER_TRAIN_STALLED_LEAD) && word.includes('1h21m'), label + ': ' + word);
}
// The owner's two rows (#15529 2nd, #15530 3rd behind #15425, head needs
// a maintainer): the chevron says what it waits on and why.
const mk = (n, title, mt, gate) => ({ repo: 'hivecommons/hive', number: n, kind: 'pr', title, stage: 80, modifiers: mt.stalled ? ['stalled'] : [], merge_train: mt, merge_gate: Object.assign({ state: mt.state, sweep_running: true }, gate || {}) });
const rider = mk(15530, 'issue-row linked PR', { state: 'queued-behind', position: 3, ahead: [15425, 15529], front: 15425, head_number: 15425, id: 'v5#15425', head_state: 'needs-human', head_reason: 'waiting for a maintainer (untrusted author)', stalled: true, stalled_s: 4851 }, { sweep_running: true });
assert.strictEqual(stageTrackerTrainWord(rider), 'Merge queue · stalled — waiting on #15425 (needs maintainer merge) · 1h21m');
assert.strictEqual(stageTrackerMergeGateWordHtml(rider, now).replace(/<[^>]+>/g, ''), 'Merge queue · stalled — waiting on #15425 (needs maintainer merge) · 1h21m');
// The head's own row says what it needs, not "waiting on" itself.
const headRow = mk(15425, 'head', { state: 'front', position: 1, ahead: [], front: 15425, head_number: 15425, id: 'v5#15425', head_state: 'needs-human', head_reason: 'waiting for a maintainer (untrusted author)', stalled: true, stalled_s: 4851 });
assert.strictEqual(stageTrackerTrainWord(headRow), 'Merge queue · stalled — needs maintainer merge · 1h21m');
// A head on a human decision reads the decision, not a merge.
assert.strictEqual(stageTrackerTrainStallWhy({ head_state: 'needs-human', head_reason: 'waiting for a human decision' }), 'needs a human decision');
// An unmapped head_state falls back to the head's plain-word reason.
assert.strictEqual(stageTrackerTrainStallWhy({ head_state: 'something-new', head_reason: 'doing a thing; details' }), 'doing a thing');
assert.strictEqual(stageTrackerMergeGateStateWord('front'), 'Merge queued');
assert.strictEqual(stageTrackerMergeGateStateWord('duplicate-of:15402'), 'Duplicate of #15402');
// The held duplicate's chip: "Duplicate of #15402 · needs a pick", the hover names the issue and the pick rule.
const heldDup = { repo: 'hivecommons/hive', number: 15407, kind: 'pr', title: 't', stage: 80, modifiers: ['needs-decision'], merge_gate: { state: 'duplicate-of:15402', duplicate_of: 15402, duplicate_issue: 15396, sweep_running: true } };
const hd = stageTrackerMergeGateInfo(heldDup, now);
assert.strictEqual(hd.word, 'Duplicate of #15402'); assert.strictEqual(hd.detail, 'needs a pick'); assert.strictEqual(hd.until, 0); assert.strictEqual(hd.duplicateOf, 15402);
assert.ok(hd.tip.startsWith('This PR and #15402 both fix #15396. Hive\'s merge queue carries one PR per issue and kept #15402'), hd.tip);
assert.strictEqual(stageTrackerMergeGateWordHtml(heldDup, now).replace(/<[^>]+>/g, ''), 'Duplicate of #15402 · needs a pick');
heldDup.merge_gate.sweep_running = false;
assert.strictEqual(stageTrackerMergeGateInfo(heldDup, now).word + ' · ' + stageTrackerMergeGateInfo(heldDup, now).detail, 'Duplicate of #15402 · needs a pick', 'sweep-stable');
assert.strictEqual(stageTrackerMergeGateStateWord('some-new-state'), 'Some new state');
`)
}
