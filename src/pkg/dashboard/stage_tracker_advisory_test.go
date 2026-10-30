package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#15381 as the enumeration saw it: labels help wanted,
// agent/dedupe, triage/advisory; a dedupe-checklist body with zero
// actionable findings.
var issue15381Labels = []string{"help wanted", "agent/dedupe", "triage/advisory"}

func issue15381Body(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "github", "testdata", "dedupe_checklist_15381.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStageTrackerAdvisoryFor(t *testing.T) {
	body := issue15381Body(t)
	adv := stageTrackerAdvisoryFor(issue15381Labels, body, nil)
	if adv == nil || adv.Kind != StageTrackerAdvisoryKind || adv.Source != stageTrackerAdvisorySourceLabel || !adv.Empty {
		t.Fatalf("#15381 from labels+body = %+v", adv)
	}
	if adv.Chip != "Advisory · nothing found · safe to close" || adv.Caption != StageTrackerAdvisoryEmptyCaption {
		t.Errorf("#15381 chip/caption = %q / %q", adv.Chip, adv.Caption)
	}
	// The sidelined path has no body, only the enumeration's hint.
	hint := github.IssueAdvisoryFromBody(body)
	if got := stageTrackerAdvisoryFor(issue15381Labels, "", hint); got == nil || !got.Empty || got.Chip != adv.Chip {
		t.Errorf("#15381 from labels+hint = %+v", got)
	}
	// Label alone, body unknown: an advisory, not known to be empty.
	if got := stageTrackerAdvisoryFor([]string{"Triage/Advisory"}, "", nil); got == nil || got.Empty || got.Chip != "Advisory · read & close" || got.Caption != "Advisory for maintainers · read, then close" {
		t.Errorf("label only = %+v", got)
	}
	// Marker without the label; verdict line without either.
	if got := stageTrackerAdvisoryFor(nil, "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: yes\n\n- [ ] #1 dup of #2\n", nil); got == nil || got.Source != stageTrackerAdvisorySourceChecklist || got.Empty {
		t.Errorf("marker only = %+v", got)
	}
	if got := stageTrackerAdvisoryFor(nil, "FYI\nDecision required: no\n", nil); got == nil || got.Source != stageTrackerAdvisorySourceVerdict {
		t.Errorf("verdict only = %+v", got)
	}
	if got := stageTrackerAdvisoryFor([]string{"kind/bug"}, "Please fix.", nil); got != nil {
		t.Errorf("ordinary issue = %+v", got)
	}
	// hivecommons/hive#15754: a checklist with task lines is the hive's
	// own work; the chip counts progress and never asks for a decision.
	working := "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: yes\n\n- [x] #1 — close as superseded by #4\n- [ ] #2 — close as superseded by #5\n- [ ] PR #3 add `Fixes #6`\n"
	if got := stageTrackerAdvisoryFor([]string{"agent/dedupe", "stage/045-needs-decision"}, working, nil); got == nil || got.Resolved != 1 || got.Total != 3 || !got.Working() ||
		got.Chip != "Advisory · hive is working the items (1/3 resolved)" || !strings.Contains(got.Caption, "(1/3 resolved)") {
		t.Errorf("working checklist = %+v", got)
	}
	done := strings.ReplaceAll(working, "- [ ]", "- [x]")
	if got := stageTrackerAdvisoryFor(nil, done, nil); got == nil || !got.AllResolved() || got.Chip != StageTrackerAdvisoryResolvedChip || got.Caption != StageTrackerAdvisoryResolvedCaption {
		t.Errorf("resolved checklist = %+v", got)
	}
}

// hivecommons/hive#15734 as the owner saw it: a dedupe checklist parked at
// 045 with needs-human. The tracker never parks an advisory: stage 0, no
// decision block, no blocker, no needs-human, progress in the chip.
func TestStageTrackerAdvisoryChecklistNeverParked(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	created := now.Add(-26 * time.Hour)
	body := "<!-- hive:dedupe-checklist repo=acme/console -->\nDecision required: yes\n\n## Near-duplicates\n\n- [x] #15312 — close as superseded by #15646\n- [ ] #15433 — close as superseded by #15647\n- [ ] PR #15725 add `Fixes #15700` to its body\n"
	labels := []string{"help wanted", "agent/dedupe", "stage/045-needs-decision", "needs-human"}

	s.statusMu.Lock()
	repo := &s.status.Repos[0]
	repo.NonActionableIssues = []any{
		github.RepoWorkIssue{Repo: "acme/console", Number: 15734, Title: "triage: duplicate and overlapping open issues/PRs to consolidate", Author: "hive-app[bot]",
			Labels: labels, Bucket: "needs_decision", Reason: "needs-human", CreatedAt: created, UpdatedAt: created,
			Advisory: github.IssueAdvisoryFromBody(body)},
	}
	s.statusMu.Unlock()

	resp := s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, resp, "issue", 15734)
	if it.Advisory == nil || it.Advisory.Resolved != 1 || it.Advisory.Total != 3 {
		t.Fatalf("#15734 advisory = %+v", it.Advisory)
	}
	if it.Stage != 0 || it.StageSlug != "" || it.DecisionNeeded != nil || it.Blocker != nil || it.NeedsHuman {
		t.Errorf("#15734 parked: stage=%d slug=%q decision=%+v blocker=%+v needs_human=%v", it.Stage, it.StageSlug, it.DecisionNeeded, it.Blocker, it.NeedsHuman)
	}
	if hasMod(it, modNeedsHuman) || hasMod(it, modNeedsDecision) || stageTrackerNeedsAttention(it) {
		t.Errorf("#15734 mods = %v attention=%v", it.Modifiers, stageTrackerNeedsAttention(it))
	}
	if it.Advisory.Chip != "Advisory · hive is working the items (1/3 resolved)" || it.Caption != it.Advisory.Caption {
		t.Errorf("#15734 chip/caption = %q / %q", it.Advisory.Chip, it.Caption)
	}
	if it.StageActor == nil || it.StageActor.Kind != StageActorHuman || !strings.Contains(it.StageActor.Why, "ticks each item") {
		t.Errorf("#15734 actor = %+v", it.StageActor)
	}
}

// An advisory row in the built tracker: advisory block set, "read, then
// close" caption, no stalled/attention/needs-human accounting, no 000-new
// block, human actor with nothing waiting.
func TestStageTrackerAdvisoryRow(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	created := now.Add(-3 * time.Hour)
	body := issue15381Body(t)

	s.statusMu.Lock()
	repo := &s.status.Repos[0]
	repo.NonActionableIssues = []any{
		github.RepoWorkIssue{Repo: "acme/console", Number: 15381, Title: "triage: duplicate and overlapping open issues/PRs to consolidate", Author: "hive-app[bot]",
			Labels: issue15381Labels, Bucket: "hive_advisory", Reason: "triage/advisory", CreatedAt: created, UpdatedAt: created,
			Advisory: github.IssueAdvisoryFromBody(body)},
		github.RepoWorkIssue{Repo: "acme/console", Number: 15382, Title: "Advisory with actions pending", Author: "hive-app[bot]",
			Labels: []string{"triage/advisory"}, Bucket: "hive_advisory", Reason: "triage/advisory", CreatedAt: created},
	}
	s.statusMu.Unlock()

	resp := s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, resp, "issue", 15381)
	if it.Advisory == nil || !it.Advisory.Empty || it.Advisory.Kind != "advisory" {
		t.Fatalf("#15381 advisory = %+v", it.Advisory)
	}
	if it.Caption != "Advisory for maintainers · nothing found · safe to close" || it.Advisory.Chip != "Advisory · nothing found · safe to close" {
		t.Errorf("#15381 caption/chip = %q / %q", it.Caption, it.Advisory.Chip)
	}
	if it.Kind != "issue" || it.Stage != 0 || it.New != nil || it.StageSlug == StageTrackerNewSlug {
		t.Errorf("#15381 kind/stage/new = %q/%d/%+v slug=%q", it.Kind, it.Stage, it.New, it.StageSlug)
	}
	if it.NeedsHuman || stageTrackerNeedsAttention(it) || hasMod(it, modStalled) {
		t.Errorf("#15381 counted as work: needs_human=%v attention=%v mods=%v", it.NeedsHuman, stageTrackerNeedsAttention(it), it.Modifiers)
	}
	if it.StageActor == nil || it.StageActor.Kind != StageActorHuman || !strings.Contains(it.StageActor.Why, "nothing waits on it") {
		t.Errorf("#15381 actor = %+v", it.StageActor)
	}
	other := trackerItem(t, resp, "issue", 15382)
	if other.Advisory == nil || other.Advisory.Empty || other.Caption != "Advisory for maintainers · read, then close" || other.Advisory.Chip != "Advisory · read & close" {
		t.Errorf("label-only advisory = %+v caption=%q", other.Advisory, other.Caption)
	}
	for _, n := range []int{1, 2, 3} {
		if plain, ok := trackerItemOK(resp, "issue", n); ok && plain.Advisory != nil {
			t.Errorf("ordinary issue #%d classified as advisory: %+v", n, plain.Advisory)
		}
	}
}

func trackerItemOK(resp StageTrackerResponse, kind string, number int) (StageTrackerItem, bool) {
	for _, it := range resp.Items {
		if it.Kind == kind && it.Number == number {
			return it, true
		}
	}
	return StageTrackerItem{}, false
}

// The SPA side (hivecommons/hive#15381): the Open chevron of an advisory
// row is its chip with no "· since Nh" and no stale tint, the headline is
// the chip, the row leads with Close (read) which posts the existing
// decision path with `close-read`, and the attention / needs-human filters
// skip it.
func TestStageTrackerAdvisoryChipAndAction(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".ham-seg.active.ham-open-advisory {",
		"'decide:close-read': 'Close now'",
		"function stageTrackerAdvisoryOf(item)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing %q", want)
		}
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH — the advisory chip/action was NOT executed by this run")
	}
	// Rendering: the chip, the headline, the filters.
	runStageTrackerRowActionsScript(t, "const assert = require('assert');\nfunction esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }\n"+strings.Join([]string{
		"function agentNowClockTime(ms) { return 'T' + ms; }",
		"function agentNowAgo(ms) { return Math.round(ms / 60000) + ' min ago'; }",
		"const STAGE_TRACKER_MOTION_ACTIVE = 'active', STAGE_TRACKER_MOTION_STATIC = 'static';",
		"function stageTrackerMotion() { return STAGE_TRACKER_MOTION_STATIC; }",
		jsConstLine(t, html, "STAGE_TRACKER_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIL_HOPS"),
		jsConstLine(t, html, "STAGE_TRACKER_PR_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED"),
		jsConstLine(t, html, "STAGE_TRACKER_POSSIBLY_EXISTS"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTIVE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_DRAFT_TIP"),
		jsConstLine(t, html, "STAGE_TRACKER_OPEN_STALE_DAYS"),
		jsConstLine(t, html, "STAGE_TRACKER_NEW_GATE_MARK"),
		jsConstLine(t, html, "STAGE_TRACKER_ATTENTION_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_HUMAN_MODS"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_MODS"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_AUTHOR_STAGES"),
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS"),
		stageTrackerManualMergeJS(t, html),
		stageTrackerSentinelJS(t, html),
		jsFunc(t, html, "stageTrackerIcon"),
		jsFunc(t, html, "stageTrackerHoldLabels"),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerSoakLeft"),
		jsFunc(t, html, "stageTrackerBlockedTip"),
		jsFunc(t, html, "stageTrackerBlockedOnPR"),
		jsFunc(t, html, "stageTrackerStaleLabelsText"),
		jsFunc(t, html, "stageTrackerRegressionText"),
		jsFunc(t, html, "stageTrackerTrailText"),
		jsFunc(t, html, "stageTrackerHopName"),
		jsFunc(t, html, "stageTrackerOpenSince"),
		jsFunc(t, html, "stageTrackerShortAgo"),
		jsFunc(t, html, "stageTrackerAdvisoryOf"),
		jsFunc(t, html, "stageTrackerNewState"),
		jsFunc(t, html, "stageTrackerNewLines"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR_NONE"),
		jsFunc(t, html, "stageTrackerStageActor"),
		jsFunc(t, html, "stageTrackerActorKind"),
		jsFunc(t, html, "stageTrackerActorTip"),
		jsFunc(t, html, "stageTrackerHeadline"),
		jsFunc(t, html, "stageTrackerSegments"),
		jsFunc(t, html, "stageTrackerStripItems"),
		jsFunc(t, html, "stageTrackerItemOnHold"),
		jsFunc(t, html, "stageTrackerItemNeedsHuman"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
	}, "\n")+`
var _stageTracker = { stages: [] };
const now = Date.parse('2026-10-29T12:00:00Z');
const stages = [{ number: 10, label: 'Actionable', short: 'Actionable' }, { number: 20, label: 'Claimed', short: 'Claimed' }, { number: 30, label: 'PR in progress', short: 'PR in progress' }, { number: 50, label: 'Review pending', short: 'CI / Review' }, { number: 80, label: 'Awaiting merge admission', short: 'Merge gate' }, { number: 100, label: 'Merge queue', short: 'Merge queue' }];
const empty = { repo: 'hivecommons/hive', number: 15381, kind: 'issue', title: 'triage: duplicate and overlapping open issues/PRs to consolidate', author: 'hivecommons-hive[bot]', author_kind: 'hive',
  stage: 0, created_at: '2026-10-29T09:00:00Z', stage_since: '2026-10-26T09:00:00Z', labels: ['help wanted', 'agent/dedupe', 'triage/advisory'], bucket: 'hive_advisory', modifiers: [],
  caption: 'Advisory for maintainers · nothing found · safe to close',
  advisory: { kind: 'advisory', source: 'label', empty: true, chip: 'Advisory · nothing found · safe to close', caption: 'Advisory for maintainers · nothing found · safe to close' },
  stage_actor: { kind: 'human', why: 'advisory for maintainers — read it, then close it; nothing waits on it and no agent picks it up' } };
const plainAdvisory = Object.assign({}, empty, { number: 15382, advisory: { kind: 'advisory', source: 'label', chip: 'Advisory · read & close', caption: 'Advisory for maintainers · read, then close' } });

// Chip: the advisory word, no "· since", no stale tint even after days.
let segs = stageTrackerSegments(empty, stages, now);
assert.strictEqual(segs[0].state, 'active');
assert.strictEqual(segs[0].short, 'Advisory · nothing found · safe to close', segs[0].short);
assert.ok(segs[0].advisory && !segs[0].isNew && segs[0].openSince === null, JSON.stringify(segs[0]));
assert.strictEqual(stageTrackerSegments(plainAdvisory, stages, now)[0].short, 'Advisory · read & close');
// Owner: "if something is advisory then the chevron should say advisory instead of open".
for (const row of [empty, plainAdvisory]) {
  const word = stageTrackerSegments(row, stages, now)[0].short;
  assert.ok(word.startsWith('Advisory'), 'chevron must start with Advisory: ' + word);
  assert.ok(!/^Open\b/.test(word) && !word.includes('· since'), 'chevron must not say Open · since: ' + word);
}
let strip = stageTrackerStripItems(empty, stages, null, now, {});
assert.ok(/<li class="ham-seg active ham-open-advisory"/.test(strip), strip);
assert.ok(!strip.includes('ham-open-stale') && !strip.includes('since 3d') && !strip.includes('since 3h'), strip);
assert.ok(strip.includes('<span class="ham-seg-t">Advisory · nothing found · safe to close</span>'), strip);
assert.ok(/title="Advisory for maintainers · nothing found · safe to close/.test(strip), strip);
assert.strictEqual(stageTrackerHeadline(empty), 'Advisory · nothing found · safe to close');
assert.strictEqual(stageTrackerHeadline(plainAdvisory), 'Advisory · read & close');
assert.strictEqual(stageTrackerActorKind(empty), 'human');
// Not counted as attention or needs-human.
assert.strictEqual(stageTrackerNeedsAttention(empty), false);
assert.strictEqual(stageTrackerItemNeedsHuman(empty), false);
// An ordinary Open row still says since / stale.
const ordinary = { repo: 'o/r', number: 2, kind: 'issue', stage: 0, caption: 'No stage recorded yet', stage_since: '2026-10-26T09:00:00Z', labels: [], modifiers: [] };
const os = stageTrackerSegments(ordinary, stages, now)[0];
assert.strictEqual(os.short, 'Open · since 3d', os.short);
assert.ok(os.openSince && os.openSince.stale);
assert.strictEqual(stageTrackerHeadline(ordinary), 'Open');

`)
	// Row action: Close (read) leads and posts the existing decision path.
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+`
const empty = { repo: 'hivecommons/hive', number: 15381, kind: 'issue', title: 'triage: duplicate and overlapping open issues/PRs to consolidate', author: 'hivecommons-hive[bot]', author_kind: 'hive',
  stage: 0, created_at: '2026-10-29T09:00:00Z', stage_since: '2026-10-26T09:00:00Z', labels: ['help wanted', 'agent/dedupe', 'triage/advisory'], bucket: 'hive_advisory', modifiers: [],
  caption: 'Advisory for maintainers · nothing found · safe to close',
  advisory: { kind: 'advisory', source: 'label', empty: true, chip: 'Advisory · nothing found · safe to close', caption: 'Advisory for maintainers · nothing found · safe to close' },
  stage_actor: { kind: 'human', why: 'advisory for maintainers — read it, then close it; nothing waits on it and no agent picks it up' } };
const plainAdvisory = Object.assign({}, empty, { number: 15382, advisory: { kind: 'advisory', source: 'label', chip: 'Advisory · read & close', caption: 'Advisory for maintainers · read, then close' } });
const ordinary = { repo: 'o/r', number: 2, kind: 'issue', stage: 0, caption: 'No stage recorded yet', stage_since: '2026-10-26T09:00:00Z', labels: [], modifiers: [] };

// Row action: Close (read) leads, posts the existing decision path with close-read.
const ids = as => as.map(a => a.id);
const ctx = { user: true, canHold: true, canHuman: true };
const as = stageTrackerRowActions(empty, ctx);
assert.deepStrictEqual(ids(as), ['decide:close-read', 'hold', 'retry', 'needs-human', 'open', 'copy'], ids(as).join(','));
assert.strictEqual(as[0].label, 'Close now');
assert.ok(as[0].lead && as[0].enabled, JSON.stringify(as[0]));
assert.ok(as[0].hint.startsWith('nothing found'), as[0].hint);
assert.ok(stageTrackerRowActions(plainAdvisory, ctx)[0].hint.startsWith('advisory acknowledged'));
// hivecommons/hive#15754: a checklist the hive is working says closing is optional.
const working = Object.assign({}, empty, { number: 15734, advisory: { kind: 'advisory', source: 'dedupe-checklist', resolved: 1, total: 3, chip: 'Advisory · hive is working the items (1/3 resolved)', caption: 'x' } });
const wa = stageTrackerRowActions(working, ctx);
assert.strictEqual(wa[0].id, 'decide:close-read'); assert.strictEqual(wa[0].label, 'Close now');
assert.strictEqual(wa[0].hint, 'optional · hive is working 1/3 items; close anyway');
assert.ok(stageTrackerAdvisoryCloseMessage([working], '#15734', 'me').includes('(1/3 resolved)'));
assert.ok(stageTrackerAdvisoryCloseMessage([empty], '#15381', 'me').includes('as read'));
const ro = stageTrackerRowActions(empty, { user: false, canHold: true });
assert.strictEqual(ro[0].enabled, false); assert.strictEqual(ro[0].why, STAGE_TRACKER_WHY_WRITE);
assert.strictEqual(ids(stageTrackerRowActions(ordinary, ctx)).indexOf('decide:close-read'), -1, 'no Close now on an ordinary issue');
const req = stageTrackerActionRequest(empty, 'decide:close-read', {});
assert.deepStrictEqual(req, { url: '/api/repos/hivecommons/hive/items/15381/decision', body: { decision: 'close-read', comment: '', type: 'issue' } });
assert.strictEqual(stageTrackerActionLabel('decide:close-read'), 'Close now');
assert.strictEqual(stageTrackerActingCaption('decide:close-read', empty), 'Closing…');
(async () => {
  assert.deepStrictEqual(await stageTrackerActionPayload([empty], 'decide:close-read'), {});
  assert.strictEqual(dialogs.length, 1);
  assert.strictEqual(dialogs[0].kind, 'confirm');
  assert.ok(dialogs[0].cfg.message.includes('hivecommons/hive#15381') && dialogs[0].cfg.message.includes('closes as completed'), dialogs[0].cfg.message);
  dialogAnswer = false;
  assert.strictEqual(await stageTrackerActionPayload([empty], 'decide:close-read'), null);
})().catch(e => { console.error(e); process.exit(1); });
`)
}
