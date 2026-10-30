package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestStageTrackerSoakPayload: the `soak` clock a PR row carries wherever
// the lane holds its head for the soak window — the record's own soak
// (NoteSoak) at 060 and 095, the 095 reason fallback for records written
// before it existed, the head the soak was measured on, and nothing off
// the gate.
func TestStageTrackerSoakPayload(t *testing.T) {
	now := time.Date(2026, 10, 28, 20, 30, 0, 0, time.UTC)
	started := now.Add(-18 * time.Minute)
	window := 30 * time.Minute
	rec := func(stage stagelabels.Stage, reason string) stagelabels.Record {
		return stagelabels.Record{Stage: stage, Reason: reason, LastSeen: now.Add(-2 * time.Minute)}
	}
	withSoak := func(r stagelabels.Record, head string) stagelabels.Record {
		r.SoakHead, r.SoakStartedAt, r.SoakWindowSeconds = head, started, int64(window/time.Second)
		return r
	}

	// 095 with the record's soak: the clock is the record's.
	got := stageTrackerSoakFrom(withSoak(rec(stagelabels.PreMergeSoak, "contributor-lane:soaking(remaining=12m0s)"), "abc1234def"), true, "abc1234def", window, now)
	if got == nil || !got.StartedAt.Equal(started) || !got.Until.Equal(started.Add(window)) || got.WindowSeconds != 1800 || got.HeadSHA != "abc1234def" {
		t.Fatalf("095 soak: %+v", got)
	}
	// 060 reviewed while trust blocks the head: the same clock.
	got = stageTrackerSoakFrom(withSoak(rec(stagelabels.Reviewed, "contributor-lane:claim-tier-untrusted"), "abc1234def"), true, "abc1234def", window, now)
	if got == nil || got.WindowSeconds != 1800 || !got.Until.Equal(started.Add(window)) {
		t.Fatalf("060 soak: %+v", got)
	}
	// A new head pushed since: the clock still names the soaked head so
	// the SPA can say the soak restarted.
	got = stageTrackerSoakFrom(withSoak(rec(stagelabels.PreMergeSoak, "contributor-lane:soaking(remaining=12m0s)"), "abc1234def"), true, "fff9999", window, now)
	if got == nil || got.HeadSHA != "abc1234def" {
		t.Fatalf("new head keeps the soaked head: %+v", got)
	}
	// Legacy 095 record (no NoteSoak): the reason's remaining, aged from
	// the sweep, with the configured window.
	got = stageTrackerSoakFrom(rec(stagelabels.PreMergeSoak, "contributor-lane:soaking(remaining=12m0s)"), true, "abc1234def", window, now)
	if got == nil || !got.Until.Equal(now.Add(10*time.Minute)) || got.WindowSeconds != 1800 || !got.StartedAt.Equal(now.Add(10*time.Minute-window)) || got.HeadSHA != "abc1234def" {
		t.Fatalf("legacy 095: %+v", got)
	}
	// No window configured: the window is what is known — the reason's remaining.
	if got = stageTrackerSoakFrom(rec(stagelabels.PreMergeSoak, "contributor-lane:soaking(remaining=12m0s)"), true, "abc", 0, now); got == nil || got.WindowSeconds != 720 {
		t.Fatalf("legacy 095 without config: %+v", got)
	}
	// Off the gate, or no soak recorded at 060: nothing.
	for _, st := range []stagelabels.Stage{stagelabels.ReviewPending, stagelabels.Fixing, stagelabels.MergeQueue, stagelabels.ReadyForHumanMerge} {
		if got = stageTrackerSoakFrom(withSoak(rec(st, ""), "abc"), true, "abc", window, now); got != nil {
			t.Errorf("%v must carry no soak: %+v", st, got)
		}
	}
	if got = stageTrackerSoakFrom(rec(stagelabels.Reviewed, "check-pending"), true, "abc", window, now); got != nil {
		t.Errorf("060 without a soak: %+v", got)
	}
	if got = stageTrackerSoakFrom(stagelabels.Record{}, false, "abc", window, now); got != nil {
		t.Errorf("no record: %+v", got)
	}
}

// TestStageTrackerSoakInPayload: buildStageTracker stamps `soak` on the PR
// row (from the record's NoteSoak, with the configured window) and mirrors
// it onto the issue's nested linked PR; the JSON shape is the one the SPA
// reads.
func TestStageTrackerSoakInPayload(t *testing.T) {
	now := time.Date(2026, 10, 28, 20, 30, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	s.sweepState = func() automerge.SweepState { return automerge.SweepState{NextFloorAt: now.Add(90 * time.Second)} }
	if s.deps == nil {
		s.deps = &Dependencies{}
	}
	s.deps.Config = &config.Config{}
	s.deps.Config.AutoMerge.ContributorLane.SoakMinutes = 45

	// PR 102 (095) has no NoteSoak yet: the reason fallback with the 45m window.
	pr := trackerItem(t, s.buildStageTracker(s.status, store, now), "pr", 102)
	if pr.Soak == nil || pr.Soak.WindowSeconds != 45*60 || !pr.Soak.Until.Equal(now.Add(7*time.Minute)) {
		t.Fatalf("PR 102 soak from reason: %+v", pr.Soak)
	}
	// The pass records the soak: the record's clock wins.
	started := now.Add(-20 * time.Minute)
	store.NoteSoak(stagelabels.Key("acme/console", 102), "abc1234def", started, 30*time.Minute)
	resp := s.buildStageTracker(s.status, store, now)
	pr = trackerItem(t, resp, "pr", 102)
	if pr.Soak == nil || !pr.Soak.StartedAt.Equal(started) || !pr.Soak.Until.Equal(started.Add(30*time.Minute)) || pr.Soak.WindowSeconds != 1800 || pr.Soak.HeadSHA != "abc1234def" {
		t.Fatalf("PR 102 soak: %+v", pr.Soak)
	}
	raw, err := json.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"soak":{`, `"started_at":"2026-10-28T20:10:00Z"`, `"until":"2026-10-28T20:40:00Z"`, `"window_s":1800`, `"head_sha":"abc1234def"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("PR 102 json missing %s: %s", want, raw)
		}
	}
	// Issue 7 mirrors PR 102.
	issue := trackerItem(t, resp, "issue", 7)
	var lp *StageTrackerLinkedPR
	for i := range issue.LinkedPRs {
		if issue.LinkedPRs[i].Number == 102 {
			lp = &issue.LinkedPRs[i]
		}
	}
	if lp == nil || lp.Soak == nil || lp.Soak.HeadSHA != "abc1234def" {
		t.Fatalf("nested PR 102 must mirror soak: %+v", issue.LinkedPRs)
	}
	// Off the gate: no soak.
	if off := trackerItem(t, resp, "pr", 103); off.Soak != nil {
		t.Errorf("PR 103 (050) must carry no soak: %+v", off.Soak)
	}
	// A 060 record the pass marked soaking (trust blocks the head) carries
	// the clock too.
	store.Observe(stagelabels.Key("acme/console", 103), stagelabels.Reviewed, "contributor-lane:claim-tier-untrusted")
	store.NoteSoak(stagelabels.Key("acme/console", 103), "0123456789", started, 30*time.Minute)
	s.status.Repos[0].OpenPrs[2] = FrontendPR{PullRequest: github.PullRequest{Number: 103, Title: "Relay PR", Author: "zoe", HiveAttributed: true, HeadSHA: "0123456789", Labels: []string{"stage/060-reviewed"}}}
	s.UpdateStatus(s.status)
	pr = trackerItem(t, s.buildStageTracker(s.status, store, now), "pr", 103)
	if pr.Stage != 60 || pr.Soak == nil || pr.Soak.HeadSHA != "0123456789" || pr.Soak.WindowSeconds != 1800 {
		t.Fatalf("PR 103 at 060 soak: stage=%d soak=%+v", pr.Stage, pr.Soak)
	}
}

// TestStageTrackerSoakNode: the SPA's soak clock — "Soaking · 12m left"
// counting down in place, "Soaking · complete" past the window and
// "Mergeable · merging now" once the lane's pass is on the soaked head,
// "Soak restarted · new head" when the row's head is not the soaked one,
// the 060 variant keeping its own word with "soak 12m left", the hover
// sentence with the rule, the live time left and the end time, and the
// tick swapping that sentence into the row's titles. The sweep being on
// the PR never hides the clock; the glyph spins only for "Mergeable".
func TestStageTrackerSoakNode(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`data-ham-soak="1"`, `data-ham-soak-tip="${esc(info.soak)}"`,
		`if (n.getAttribute('data-ham-soak') === '1') { stageTrackerSoakTick(n, nowMs); continue; }`,
		`const clock = stageTrackerSoakClock(src, item, nowMs);`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing soak-clock wiring %q", want)
		}
	}
	// No new spinner markup: the soak words set a cue, the glyph owns the spin.
	if strings.Contains(jsFunc(t, html, "stageTrackerSoakWords"), "cue-spin") || strings.Contains(jsFunc(t, html, "stageTrackerSoakTick"), "cue-spin") {
		t.Error("the soak clock must not add its own spinning glyph")
	}
	// The owner's words (hivecommons/hive#15575): never "sweep" on a soaking row.
	if src := jsFunc(t, html, "stageTrackerSoakWords"); strings.Contains(src, "sweep ") || strings.Contains(src, "ready —") {
		t.Error("stageTrackerSoakWords must not say \"merge sweep\" or \"ready —\"")
	}
	runStageTrackerRowActionsScript(t, "function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_GATE_WORD")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_GATE_REASON_WORDS")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS")+"\n"+
		jsFunc(t, html, "stageTrackerIcon")+"\n"+
		jsFunc(t, html, "stageTrackerAgoText")+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateStateOf")+"\n"+
		jsFunc(t, html, "stageTrackerShortLeft")+"\n"+
		jsFunc(t, html, "stageTrackerGateReasonText")+"\n"+
		stageTrackerMergeGateInfoJS(t, html)+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateWordHtml")+"\n"+
		jsFunc(t, html, "stageTrackerGateCueHtml")+"\n"+
		jsFunc(t, html, "stageTrackerGateCountdownSync")+"\n"+`
const assert = require('assert');
const now = Date.parse('2026-10-28T20:30:00Z');
const iso = ms => new Date(ms).toISOString();
const ends = stageTrackerClockText(now + 12 * 60000 + 30000);
assert.ok(/\d:\d\d/.test(ends), 'clock text: ' + ends);

// #15174 at 095: admitted, the sweep is on it now, 12m30s of the 30m window left.
const soaking = { repo: 'hivecommons/hive', number: 15174, kind: 'pr', stage: 95, head: 'abc1234def0',
  soak: { started_at: iso(now - 17.5 * 60000), until: iso(now + 12.5 * 60000), window_s: 1800, head_sha: 'abc1234def0' },
  merge_gate: { state: 'soaking', sweep_running: true, last_sweep_at: iso(now - 20000) } };
let info = stageTrackerMergeGateInfo(soaking, now);
assert.strictEqual(info.word, 'Soaking'); assert.strictEqual(info.detail, '13m left', 'the sweep running never hides the clock');
assert.strictEqual(info.cue, 'soaking');
assert.strictEqual(info.tip, 'Head abc1234 must sit 30 minutes without new commits before the hive merges it · 12m 30s left · soak ends ' + ends + ' Last checked 20s ago.');
assert.strictEqual(info.until, now + 12.5 * 60000);
const cue = stageTrackerGateCueHtml(soaking, now);
assert.ok(!cue.includes('cue-spin') && cue.includes('data-gate-cue="soaking"'), 'static glyph: ' + cue);
let html = stageTrackerMergeGateWordHtml(soaking, now);
assert.ok(html.includes('data-ham-soak="1" data-ham-until="' + (now + 12.5 * 60000) + '" data-ham-until-kind="soaking" data-ham-soak-window="1800" data-ham-soak-head="abc1234def0" data-ham-soak-restarted="0"'), html);
assert.ok(html.endsWith('>· 13m left</span></span>'), html);

// Past the window and the lane's pass has not come: "Soaking · complete"; the next pass is hover-only.
const soaked = Object.assign({}, soaking, { soak: Object.assign({}, soaking.soak, { until: iso(now - 60000) }), merge_gate: { state: 'soaking', last_sweep_at: iso(now - 20000), next_sweep_at: iso(now + 3 * 60000) } });
info = stageTrackerMergeGateInfo(soaked, now);
assert.strictEqual(info.word, 'Soaking'); assert.strictEqual(info.detail, 'complete'); assert.strictEqual(info.cue, 'soaking');
assert.strictEqual(info.until, now - 60000, 'the clock stays on the node so the tick can keep the next-sweep words fresh');
assert.ok(info.tip.startsWith('Head abc1234 sat 30 minutes without new commits — soaked since ' + stageTrackerClockText(now - 60000) + '. Soak complete — merges on the next pass (next pass in 3m).'), info.tip);
// The lane's pass on a soaked head: Mergeable, spinning, "merging now".
const mergeable = stageTrackerMergeGateInfo(Object.assign({}, soaked, { merge_gate: { state: 'soaking', sweep_running: true } }), now);
assert.strictEqual(mergeable.word, 'Mergeable'); assert.strictEqual(mergeable.detail, 'merging now'); assert.strictEqual(mergeable.cue, 'running');
assert.ok(mergeable.tip.endsWith('. Mergeable — merging now.'), mergeable.tip);
assert.ok(stageTrackerGateCueHtml(Object.assign({}, soaked, { merge_gate: { state: 'soaking', sweep_running: true } }), now).includes('cue-spin'));
assert.ok(stageTrackerMergeGateWordHtml(Object.assign({}, soaked, { merge_gate: { state: 'soaking', sweep_running: true } }), now).startsWith('<span class="ham-seg-t">Mergeable <span class="ham-gate-detail"'));
assert.strictEqual(stageTrackerMergeGateInfo(Object.assign({}, soaked, { merge_gate: { state: 'soaking' } }), now).detail, 'complete');
assert.ok(stageTrackerMergeGateInfo(Object.assign({}, soaked, { merge_gate: { state: 'soaking' } }), now).tip.includes('Soak complete — merges on the next pass.'));

// A push since the soak was measured: the soak restarted on the new head.
const pushed = Object.assign({}, soaking, { head: 'ffff0000aaaa' });
info = stageTrackerMergeGateInfo(pushed, now);
assert.strictEqual(info.word, 'Soak restarted'); assert.strictEqual(info.detail, 'new head'); assert.strictEqual(info.until, 0);
assert.ok(info.tip.startsWith('Head abc1234 was replaced by a new push, so the soak starts over on the new head: it must sit 30 minutes without new commits before the hive merges it.'), info.tip);
assert.ok(stageTrackerMergeGateWordHtml(pushed, now).startsWith('<span class="ham-seg-t">Soak restarted <span class="ham-gate-detail" data-ham-soak="1"'));

// 060 reviewed while trust blocks the head (RouteHuman "soak Nm remaining"): the
// stage keeps its word, the detail is the same clock.
const blocked = { repo: 'hivecommons/hive', number: 15180, kind: 'pr', stage: 60, head: '0123456789a', reason: 'contributor-lane:claim-tier-untrusted',
  soak: { started_at: iso(now - 5 * 60000), until: iso(now + 25 * 60000), window_s: 1800, head_sha: '0123456789a' },
  merge_gate: { state: 'gate-pending', reason: 'contributor-lane:claim-tier-untrusted', next_sweep_at: iso(now + 42000) } };
info = stageTrackerMergeGateInfo(blocked, now);
assert.strictEqual(info.word, 'Review passed'); assert.strictEqual(info.detail, 'soak 25m left'); assert.strictEqual(info.cue, 'soaking');
assert.ok(info.tip.startsWith('Head 0123456 must sit 30 minutes without new commits before the hive merges it · 25m left · soak ends '), info.tip);
assert.strictEqual(stageTrackerMergeGateInfo(Object.assign({}, blocked, { soak: Object.assign({}, blocked.soak, { until: iso(now - 1000) }) }), now).detail, 'soak complete');
// Without a soak clock a 060 row reads as before.
assert.strictEqual(stageTrackerMergeGateInfo(Object.assign({}, blocked, { soak: undefined }), now).detail, 'contributor-lane:claim-tier-untrusted');

// An issue row mirrors its covering PR's clock.
const issue = { repo: 'hivecommons/hive', number: 9, kind: 'issue', stage: 95, reason: 'open PR #15174 covers it (PR at 095-pre-merge-soak)', linked_prs: [soaking] };
assert.strictEqual(stageTrackerMergeGateInfo(issue, now).detail, '13m left');

// The tick: the words and the hover follow the clock, in place, no re-render.
const attrsOf = h => { const o = {}; h.replace(/ (data-[a-z-]+)="([^"]*)"/g, (m, k, v) => { o[k] = v.replace(/&quot;/g, '"').replace(/&lt;/g, '<').replace(/&amp;/g, '&'); }); return o; };
const node = (h, li) => ({ attrs: attrsOf(h), textContent: '', getAttribute(k) { return this.attrs[k] === undefined ? null : this.attrs[k]; }, setAttribute(k, v) { this.attrs[k] = v; }, closest() { return li; } });
const tip0 = stageTrackerMergeGateInfo(soaking, now).tip.replace(' Last checked 20s ago.', '');
const glyph = { attrs: { title: tip0, 'aria-label': tip0 }, getAttribute(k) { return this.attrs[k] === undefined ? null : this.attrs[k]; }, setAttribute(k, v) { this.attrs[k] = v; } };
const li = { attrs: { title: 'Soaking — ' + tip0 + ' Last checked 20s ago.' }, getAttribute(k) { return this.attrs[k] === undefined ? null : this.attrs[k]; }, setAttribute(k, v) { this.attrs[k] = v; }, querySelectorAll() { return [glyph]; } };
const n = node(stageTrackerMergeGateWordHtml(soaking, now), li);
assert.strictEqual(n.attrs['data-ham-soak-tip'], tip0);
stageTrackerGateCountdownSync({ querySelectorAll: () => [n] }, now + 5 * 60000);
assert.strictEqual(n.textContent, '· 8m left');
const tip5 = 'Head abc1234 must sit 30 minutes without new commits before the hive merges it · 7m 30s left · soak ends ' + ends;
assert.strictEqual(li.attrs.title, 'Soaking — ' + tip5 + ' Last checked 20s ago.', 'the chevron hover carries the live time left');
assert.strictEqual(glyph.attrs.title, tip5); assert.strictEqual(glyph.attrs['aria-label'], tip5);
assert.strictEqual(n.attrs['data-ham-soak-tip'], tip5);
// Past the window the pill flips to Mergeable (the pass was on it) without a payload — the word too.
const wordNode = { nodeType: 3, textContent: 'Soaking ' };
n.previousSibling = wordNode;
stageTrackerGateCountdownSync({ querySelectorAll: () => [n] }, now + 13 * 60000);
assert.strictEqual(n.textContent, '· merging now');
assert.strictEqual(wordNode.textContent, 'Mergeable ');
assert.ok(li.attrs.title.startsWith('Soaking — Head abc1234 sat 30 minutes without new commits — soaked since ' + ends + '. Mergeable — merging now.'), li.attrs.title);
// Short / long forms.
assert.deepStrictEqual([stageTrackerSoakLeftText(45000), stageTrackerSoakLeftText(750000), stageTrackerSoakLeftText(3900000), stageTrackerSoakLeftText(0)], ['45s', '12m 30s', '1h 05m', '']);
console.log('soak clock ok');
`)
}
