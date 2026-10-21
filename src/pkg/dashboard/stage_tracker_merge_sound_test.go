package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// TestStageTrackerRecentlyMerged pins the recently_merged side list the
// activity monitor's merge sound keys on: pr_merged audit entries inside the
// window, newest first, one per PR, number from the typed Target or the
// legacy number= detail pair; closes, older merges and unparseable entries
// are left out.
func TestStageTrackerRecentlyMerged(t *testing.T) {
	now := time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	ts := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }

	s.audit.LogRecordAt(ts(5*time.Minute), "system", github.AuditActionPRMerged, "method=squash, path=sweep", "governor", "acme/console", 101)
	s.audit.LogRecordAt(ts(2*time.Minute), "system", github.AuditActionPRMerged, "repo=acme/console, number=102, path=human, actor=alice", "governor", "", 0)
	s.audit.LogRecordAt(ts(2*time.Minute), "system", github.AuditActionPRMerged, "method=squash, path=relay", "governor", "acme/console", 101) // duplicate PR
	s.audit.LogRecordAt(ts(3*time.Minute), "system", github.AuditActionPRClosed, "path=relay, reason=observed_closed", "governor", "acme/console", 103)
	s.audit.LogRecordAt(ts(stageTrackerMergedWindow+time.Minute), "system", github.AuditActionPRMerged, "method=squash, path=sweep", "governor", "acme/console", 90)
	s.audit.LogRecordAt(ts(time.Minute), "system", github.AuditActionPRMerged, "path=sweep", "governor", "", 0) // no coordinates
	// The PR poller's terminal observation names the repo by its configured
	// short name (repo=console); Items carry acme/console. Both spellings of
	// one merge collapse onto the qualified key, newest first; a short name
	// no configured repo accounts for passes through untouched.
	s.audit.LogRecordAt(ts(90*time.Second), "system", github.AuditActionPRMerged, "repo=console, number=105, path=human, actor=alice", "governor", "console", 105)
	s.audit.LogRecordAt(ts(80*time.Second), "system", github.AuditActionPRMerged, "method=squash, path=sweep", "governor", "acme/console", 105)
	s.audit.LogRecordAt(ts(70*time.Second), "system", github.AuditActionPRMerged, "repo=elsewhere, number=1, path=human, actor=bob", "governor", "elsewhere", 1)

	got := s.buildStageTracker(s.status, store, now).RecentlyMerged
	want := []StageTrackerMerged{
		{Repo: "elsewhere", Number: 1, MergedAt: now.Add(-70 * time.Second)},
		{Repo: "acme/console", Number: 105, MergedAt: now.Add(-90 * time.Second)},
		{Repo: "acme/console", Number: 102, MergedAt: now.Add(-2 * time.Minute)},
		{Repo: "acme/console", Number: 101, MergedAt: now.Add(-5 * time.Minute)},
	}
	if len(got) != len(want) {
		t.Fatalf("recently_merged = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Repo != want[i].Repo || got[i].Number != want[i].Number || !got[i].MergedAt.Equal(want[i].MergedAt) {
			t.Fatalf("recently_merged[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The handler builds against the wall clock, so give it one merge inside
	// its own window and check the field rides the JSON payload.
	s.audit.LogRecordAt(time.Now().UTC().Format(time.RFC3339), "system", github.AuditActionPRMerged, "method=squash, path=queue", "governor", "acme/console", 104)
	req := httptest.NewRequest(http.MethodGet, "/api/stage-tracker", nil)
	rr := httptest.NewRecorder()
	s.handleStageTracker(rr, req)
	var live struct {
		RecentlyMerged []StageTrackerMerged `json:"recently_merged"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &live); err != nil {
		t.Fatal(err)
	}
	if len(live.RecentlyMerged) != 1 || live.RecentlyMerged[0].Number != 104 || live.RecentlyMerged[0].Repo != "acme/console" {
		t.Fatalf("handler recently_merged = %+v", live.RecentlyMerged)
	}

	// A server with no audit log (or no merges) omits the field entirely.
	if (&Server{}).stageTrackerRecentlyMerged(now, nil) != nil {
		t.Fatalf("nil audit must yield nil")
	}
	s2, store2 := stageTrackerFixture(t, now)
	raw, _ := json.Marshal(s2.buildStageTracker(s2.status, store2, now))
	if strings.Contains(string(raw), `"recently_merged"`) {
		t.Fatalf("recently_merged must be omitted when empty: %s", raw)
	}
}

// TestStageTrackerMergeSoundWiring pins the opt-in merge sound's toolbar
// chip, its persistence key, the synthesis path (Web Audio only — no asset,
// no URL) and the guards the owner asked for: no sound on a hidden tab, a
// capped number of zaps per refresh, a modest master gain.
func TestStageTrackerMergeSoundWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<button type="button" class="ham-chip ham-sound" id="activity-monitor-sound" data-action="hamSoundToggle" aria-pressed="false" aria-label="Sound off — turn on (merge zap + new-item buzz; plays a test of each)" title="Sound off — turn on (merge zap + new-item buzz; plays a test of each)">🔇</button>`,
		`.ham-sound {`,
		`.ham-sound[aria-pressed="true"] {`,
		`const STAGE_TRACKER_SOUND_KEY = 'hive.tracker.sound';`,
		`const STAGE_TRACKER_SOUND_MAX_PLAYS = 2;`,
		`const STAGE_TRACKER_SOUND_MASTER_GAIN = 0.3;`,
		`localStorage.getItem(STAGE_TRACKER_SOUND_KEY) === '1'`,
		`localStorage.setItem(STAGE_TRACKER_SOUND_KEY, _stageTrackerSoundOn ? '1' : '0')`,
		`const STAGE_TRACKER_BUZZ_MASTER_GAIN = 0.2;`,
		`function stageTrackerMergeState(d)`,
		`function stageTrackerCarryOpen(prev, next)`,
		`function stageTrackerNewlyMerged(prev, next)`,
		`function stageTrackerNoteMerges(d)`,
		`function stageTrackerOpenKeys(d)`,
		`function stageTrackerNewlyOpened(prev, next)`,
		`function stageTrackerNoteOpened(d)`,
		`function hamSoundToggle()`,
		`if (data && typeof data === 'object') { _stageTracker = data; stageTrackerNoteMerges(data); stageTrackerNoteOpened(data); }`,
		`if (_stageTrackerSoundOn && !document.hidden) stageTrackerPlayZap(`,
		`if (_stageTrackerSoundOn && !document.hidden) stageTrackerPlayBuzz(`,
		`window.AudioContext || window.webkitAudioContext`,
		`if (_stageTrackerAudioCtx.state === 'suspended')`,
		`if (ctx.state === 'suspended') { stageTrackerSoundPrime(); return false; }`,
		`band.type = 'bandpass';`,
		`ctx.createBufferSource()`,
		`osc.type = 'sine';`,
		`osc.type = 'sawtooth';`,
		`low.type = 'lowpass';`,
		`depth.connect(trem.gain);`,
		`b.textContent = on ? '🔊' : '🔇';`,
		`{ stageTrackerLoadFilter(); stageTrackerSoundInit(); }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing merge sound wiring %q", want)
		}
	}
	for _, forbid := range []string{`<audio`, `new Audio(`, `.mp3`, `.ogg`, `.wav`} {
		if strings.Contains(jsFunc(t, html, "stageTrackerZapVoice")+jsFunc(t, html, "stageTrackerBuzzVoice")+jsFunc(t, html, "stageTrackerPlayVoices"), forbid) {
			t.Errorf("merge sound must be synthesised, found %q", forbid)
		}
	}
}

// TestStageTrackerNewlyMergedDetector runs the pure detector under node:
// first payload never fires; open → merged fires once per PR (terminal stage
// or recently_merged); PRs already merged, PRs that vanish without a
// recently_merged entry (closed), issues and never-seen PRs never fire; the
// zapped set and the per-refresh cap bound the sound.
func TestStageTrackerNewlyMergedDetector(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — merge sound detector was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		"const window = {}; const document = { hidden: false };",
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_MAX_PLAYS"),
		jsFunc(t, html, "stageTrackerPRKey"),
		jsFunc(t, html, "stageTrackerMergeState"),
		jsFunc(t, html, "stageTrackerCarryOpen"),
		jsFunc(t, html, "stageTrackerNewlyMerged"),
		jsFunc(t, html, "stageTrackerNoteMerges"),
		"var _stageTrackerMergeState = null; var _stageTrackerZapped = {}; var _stageTrackerSoundOn = true;",
		"const plays = []; function stageTrackerPlayZap(n) { plays.push(n); return true; }",
	}, "\n") + `
const P = (n, stage, extra) => Object.assign({ kind: 'pr', repo: 'a/b', number: n, stage }, extra || {});
const I = (n, stage) => ({ kind: 'issue', repo: 'a/b', number: n, stage });
const first = { items: [P(1, 50), P(2, 80), P(3, 100), I(9, 20), P(4, STAGE_TRACKER_TERMINAL)], recently_merged: [{ repo: 'a/b', number: 7 }] };
assert.deepStrictEqual(stageTrackerMergeState(first), { 'a/b/pr#1': false, 'a/b/pr#2': false, 'a/b/pr#3': false, 'a/b/pr#4': true, 'a/b/pr#7': true });
assert.deepStrictEqual(stageTrackerNewlyMerged(null, stageTrackerMergeState(first)), [], 'no previous payload → nothing');
assert.deepStrictEqual(stageTrackerNewlyMerged(stageTrackerMergeState(first), stageTrackerMergeState(first)), [], 'same payload → nothing');
// #1 merges via terminal stage, #2 via recently_merged while still listed, #3 vanishes
// with a recently_merged entry (merged), issue #9 closes (vanishes), #4 stays merged,
// #8 appears already merged.
const second = { items: [P(1, STAGE_TRACKER_TERMINAL), P(2, 80), P(4, STAGE_TRACKER_TERMINAL), P(8, STAGE_TRACKER_TERMINAL)], recently_merged: [{ repo: 'a/b', number: 2 }, { repo: 'a/b', number: 3 }, { repo: 'a/b', number: 7 }, { repo: 'a/b', number: 8 }] };
assert.deepStrictEqual(stageTrackerNewlyMerged(stageTrackerMergeState(first), stageTrackerMergeState(second)), ['a/b/pr#1', 'a/b/pr#2', 'a/b/pr#3']);
// A PR that vanishes WITHOUT a recently_merged entry was closed, not merged.
const closed = { items: [P(2, 80), P(3, 100)], recently_merged: [] };
assert.deepStrictEqual(stageTrackerNewlyMerged(stageTrackerMergeState(first), stageTrackerMergeState(closed)), []);
// Repo scoping: same number in another repo is a different PR.
assert.deepStrictEqual(stageTrackerNewlyMerged({ 'a/b/pr#5': false }, { 'c/d/pr#5': true }), []);
assert.deepStrictEqual(stageTrackerMergeState(null), {});
assert.deepStrictEqual(stageTrackerMergeState({ items: 'nope' }), {});

// Stateful wrapper: first load silent, then one coalesced play (capped), no replay.
stageTrackerNoteMerges(first);
assert.deepStrictEqual(plays, []);
stageTrackerNoteMerges(second);
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS], 'three merges in one refresh → one call, capped count');
stageTrackerNoteMerges(second);
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS], 'a re-render of the same payload never replays');
// #2 is still listed open at 80 (snapshot lag); when it finally drops out it must not zap again.
stageTrackerNoteMerges({ items: [P(4, STAGE_TRACKER_TERMINAL)], recently_merged: [{ repo: 'a/b', number: 2 }, { repo: 'a/b', number: 3 }] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS]);
// Sound off: detector still advances, nothing plays.
_stageTrackerSoundOn = false;
stageTrackerNoteMerges({ items: [P(10, 50)] });
stageTrackerNoteMerges({ items: [P(10, STAGE_TRACKER_TERMINAL)] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS]);
assert.ok(_stageTrackerZapped['a/b/pr#10'], 'the zapped set is kept even when muted so unmuting later does not replay');
// Hidden tab: silent.
_stageTrackerSoundOn = true; document.hidden = true;
stageTrackerNoteMerges({ items: [P(11, 50)] });
stageTrackerNoteMerges({ items: [P(11, STAGE_TRACKER_TERMINAL)] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS]);
document.hidden = false;
stageTrackerNoteMerges({ items: [P(12, 50)] });
stageTrackerNoteMerges({ items: [], recently_merged: [{ repo: 'a/b', number: 12 }] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS, 1]);

// Regression (owner: "not hearing the zap"): recently_merged names the repo
// by its short name while the item carried owner/name.
assert.deepStrictEqual(stageTrackerMergeState({ items: [P(20, 50)], recently_merged: [{ repo: 'b', number: 20 }] }), { 'a/b/pr#20': true });
assert.deepStrictEqual(stageTrackerMergeState({ items: [P(20, 50), { kind: 'pr', repo: 'c/b', number: 1, stage: 50 }], recently_merged: [{ repo: 'b', number: 20 }] }),
  { 'a/b/pr#20': false, 'c/b/pr#1': false, 'b/pr#20': true }, 'an ambiguous short name is left alone');
stageTrackerNoteMerges({ items: [P(21, 50)] });
stageTrackerNoteMerges({ items: [], recently_merged: [{ repo: 'b', number: 21 }] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS, 1], 'short-name entry with no item left to resolve it cannot match…');
stageTrackerNoteMerges({ items: [P(22, 50)] });
stageTrackerNoteMerges({ items: [P(22, 50)], recently_merged: [{ repo: 'b', number: 22 }] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS, 1, 1], '…but one that arrives while the item is still listed does');

// Regression: the PR drops out of items one payload before the audit log
// lists its merge (webhook-patched snapshot vs poller). Carried as open,
// it still zaps when recently_merged catches up — once.
stageTrackerNoteMerges({ items: [P(30, 50), P(31, 60)] });
stageTrackerNoteMerges({ items: [P(31, 60)] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS, 1, 1], 'vanished without a merge entry: nothing yet');
assert.strictEqual(_stageTrackerMergeState['a/b/pr#30'], false, 'carried as still open');
stageTrackerNoteMerges({ items: [P(31, 60)], recently_merged: [{ repo: 'a/b', number: 30 }] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS, 1, 1, 1], 'late recently_merged entry zaps');
stageTrackerNoteMerges({ items: [P(31, 60)], recently_merged: [{ repo: 'a/b', number: 30 }] });
stageTrackerNoteMerges({ items: [P(31, 60)] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS, 1, 1, 1], 'no replay once the entry ages out');
assert.deepStrictEqual(stageTrackerCarryOpen(null, { x: true }), { x: true });
assert.deepStrictEqual(stageTrackerCarryOpen({ a: false, b: true, c: false }, { c: true }), { a: false, c: true }, 'merged keys are not carried, open ones are');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node merge sound detector fixture failed: %v\n%s", err, out)
	}
}

// TestStageTrackerNewlyOpenedDetector runs the new-item buzz detector under
// node: the first populated payload never buzzes (a page load would buzz for
// everything open), a key that appears later buzzes once, N new items in one
// refresh coalesce into one capped call, the buzzed set stops replays, and
// the sound/hidden gates match the merge zap's.
func TestStageTrackerNewlyOpenedDetector(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — new-item buzz detector was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		"const window = {}; const document = { hidden: false };",
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_MAX_PLAYS"),
		jsFunc(t, html, "stageTrackerItemKey"),
		jsFunc(t, html, "stageTrackerOpenKeys"),
		jsFunc(t, html, "stageTrackerNewlyOpened"),
		jsFunc(t, html, "stageTrackerNoteOpened"),
		"var _stageTrackerOpenState = null; var _stageTrackerBuzzed = {}; var _stageTrackerSoundOn = true;",
		"const plays = []; function stageTrackerPlayBuzz(n) { plays.push(n); return true; }",
	}, "\n") + `
const P = n => ({ kind: 'pr', repo: 'a/b', number: n, stage: 50 });
const I = n => ({ kind: 'issue', repo: 'a/b', number: n, stage: 20 });
assert.deepStrictEqual(stageTrackerOpenKeys({ items: [P(1), I(1), { kind: 'issue', repo: 'a/b' }, null] }), { 'a/b/pr#1': true, 'a/b/issue#1': true });
assert.deepStrictEqual(stageTrackerOpenKeys(null), {});
assert.deepStrictEqual(stageTrackerOpenKeys({ items: 'nope' }), {});
assert.deepStrictEqual(stageTrackerNewlyOpened(null, { x: true }), [], 'no previous payload → nothing');
assert.deepStrictEqual(stageTrackerNewlyOpened({}, { x: true }), [], 'empty previous payload (first populated load) → nothing');
assert.deepStrictEqual(stageTrackerNewlyOpened({ x: true }, { x: true, y: true, a: true }), ['a', 'y']);
assert.deepStrictEqual(stageTrackerNewlyOpened({ x: true, y: true }, { x: true }), [], 'closed items are not new');

// Stateful wrapper: initializing payload, then the first real one, both silent.
stageTrackerNoteOpened({ status: 'initializing' });
stageTrackerNoteOpened({ items: [P(1), I(2)] });
assert.deepStrictEqual(plays, []);
// Same payload: nothing. Then three new items → one capped call.
stageTrackerNoteOpened({ items: [P(1), I(2)] });
stageTrackerNoteOpened({ items: [P(1), I(2), P(3), I(4), I(5)] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS]);
// Re-render of the same payload, and an item that drops out then returns: no replay.
stageTrackerNoteOpened({ items: [P(1), I(2), P(3), I(4), I(5)] });
stageTrackerNoteOpened({ items: [P(1), I(2), P(3), I(4)] });
stageTrackerNoteOpened({ items: [P(1), I(2), P(3), I(4), I(5)] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS]);
// One new item → one buzz. Same number, other kind/repo → distinct keys.
stageTrackerNoteOpened({ items: [P(1), I(2), P(3), I(4), I(5), P(6)] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS, 1]);
stageTrackerNoteOpened({ items: [P(1), I(2), P(3), I(4), I(5), P(6), I(6), { kind: 'pr', repo: 'c/d', number: 6, stage: 50 }] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS, 1, 2]);
// Sound off: detector advances, nothing plays, the buzzed set is kept.
_stageTrackerSoundOn = false;
stageTrackerNoteOpened({ items: [P(1), P(7)] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS, 1, 2]);
assert.ok(_stageTrackerBuzzed['a/b/pr#7']);
// Hidden tab: silent.
_stageTrackerSoundOn = true; document.hidden = true;
stageTrackerNoteOpened({ items: [P(1), P(7), P(8)] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS, 1, 2]);
document.hidden = false;
stageTrackerNoteOpened({ items: [P(1), P(7), P(8), P(9)] });
assert.deepStrictEqual(plays, [STAGE_TRACKER_SOUND_MAX_PLAYS, 1, 2, 1]);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node new-item buzz detector fixture failed: %v\n%s", err, out)
	}
}
