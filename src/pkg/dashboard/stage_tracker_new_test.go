package dashboard

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15094: hive's hive did not show hivecommons/hive#15092
// (a human contributor's issue, opened 20:38Z) in the Activity Monitor
// until the governor republished at 20:45Z — the `opened` re-read only
// rewrote rows the status already listed. An open read for an item no
// list carries must now insert it, and the row must read as 000-new.
func TestWebhookOpenedItemInsertedIntoTracker(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	s, store := stageTrackerFixture(t, now)
	s.statusMu.Lock()
	s.status.Governor.NextKickAt = now.Add(7 * time.Minute).UTC().Format(time.RFC3339)
	if s.status.Repos[0].WorkBreakdown == nil {
		s.status.Repos[0].WorkBreakdown = &github.RepoWorkBreakdown{}
	}
	s.statusMu.Unlock()
	opened := now.Add(-90 * time.Second)

	triage := github.RepoWorkIssue{Repo: "acme/console", Number: 15092, Title: "Add a thing", Author: "kanrenibm", AuthorIsHuman: true, Labels: []string{"enhancement", "help wanted"}, CreatedAt: opened, UpdatedAt: opened, Bucket: "reporter_triage", Reason: "Awaiting maintainer triage label"}
	n := s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{
		// A contributor's issue the reporter gate has not admitted: the
		// classifier offers the reporter_triage row.
		{Repo: "acme/console", Number: 15092, State: "open", Title: "Add a thing", Author: "kanrenibm", Labels: []string{"enhancement", "help wanted"}, UpdatedAt: opened,
			Patch: github.WebhookItemPatch{Bucket: github.WebhookBucketUnknown, WorkIssue: &triage}},
		// A maintainer's issue the classifier admitted outright.
		{Repo: "acme/console", Number: 15095, State: "open", Title: "Maintainer ask", Author: "pat", UpdatedAt: opened,
			Patch: github.WebhookItemPatch{Bucket: github.WebhookBucketActionable, Issue: &github.Issue{Repo: "acme/console", Number: 15095, Title: "Maintainer ask", Author: "pat", AuthorIsHuman: true, CreatedAt: opened, UpdatedAt: opened}}},
		// A PR with no classification at all (the read carried no patch).
		{Repo: "acme/console", Number: 15096, IsPR: true, State: "open", Title: "Fresh PR", Author: "sam", UpdatedAt: opened},
	})
	if n != 3 {
		t.Fatalf("applyWebhookItemSnapshots inserted %d rows, want 3", n)
	}
	s.statusMu.RLock()
	repo := s.status.Repos[0]
	s.statusMu.RUnlock()
	if repo.WorkBreakdown == nil || repo.WorkBreakdown.Issues.ReporterTriage != 1 {
		t.Fatalf("issue breakdown after insert = %+v, want reporter_triage 1", repo.WorkBreakdown)
	}

	resp := s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, resp, "issue", 15092)
	if it.Stage != 0 || it.StageSlug != StageTrackerNewSlug || it.New == nil {
		t.Fatalf("#15092 = stage %d slug %q new %+v, want 000-new", it.Stage, it.StageSlug, it.New)
	}
	if it.New.NextMover != StageTrackerMoverMaintainer || it.Caption != stageTrackerTriageNeededCaption || it.Actor != StageTrackerMoverMaintainer {
		t.Fatalf("#15092 caption %q actor %q new %+v, want the maintainer's triage as next mover", it.Caption, it.Actor, it.New)
	}
	// A sidelined row keeps the reporter's kind: the work-bucket copy of the
	// issue carries AuthorIsHuman, so the row does not read as an unknown
	// author just because the gate parked it.
	if it.AuthorKind != authorKindHuman {
		t.Fatalf("#15092 author_kind = %q, want %q on a reporter_triage row", it.AuthorKind, authorKindHuman)
	}
	if it.StageSince == nil || !it.StageSince.Equal(opened) {
		t.Fatalf("#15092 since = %v, want the opened time %v", it.StageSince, opened)
	}
	if len(it.New.Gates) != 3 || it.New.Gates[0].Name != "triage" || it.New.Gates[0].State != StageTrackerGateNext || it.New.Gates[1].Name != "actionable" || it.New.Gates[2].Name != "claimed" {
		t.Fatalf("#15092 gates = %+v, want triage (next) → actionable → claimed", it.New.Gates)
	}
	// The hold is decided before the awaiting label lands (hivecommons/hive#15437
	// read "New" with labels: null): the row carries the needs-triage
	// modifier the label would yield, so the Open chevron reads "Triage
	// needed" and Accept triage leads.
	if !slices.Contains(it.Modifiers, modNeedsTriage) {
		t.Fatalf("#15092 modifiers = %v, want %s on a reporter_triage row without the label", it.Modifiers, modNeedsTriage)
	}
	if !it.New.TriagePending() {
		t.Fatalf("#15092 new = %+v, want TriagePending", it.New)
	}
	if it.New.NextPassAt == nil || it.New.NextPassInSeconds != 7*60 {
		t.Fatalf("#15092 next pass = %v / %ds caption %q, want the governor's kick in 7m", it.New.NextPassAt, it.New.NextPassInSeconds, it.Caption)
	}

	it = trackerItem(t, resp, "issue", 15095)
	if it.New == nil || it.New.NextMover != StageTrackerMoverHive || it.Caption != "New · waiting for the stage-label pass (→ 010 actionable), then the scanner (next pass in 7m)" {
		t.Fatalf("#15095 = %+v caption %q, want the stage-label pass as next mover", it.New, it.Caption)
	}
	if slices.Contains(it.Modifiers, modNeedsTriage) || it.New.TriagePending() {
		t.Fatalf("#15095 (admitted reporter) modifiers = %v, must not read triage needed", it.Modifiers)
	}
	if it.New.Gates[0].Name != "triage" || it.New.Gates[0].State != StageTrackerGateDone {
		t.Fatalf("#15095 gates = %+v, want triage already done", it.New.Gates)
	}

	it = trackerItem(t, resp, "pr", 15096)
	if it.New == nil || it.New.NextMover != StageTrackerMoverHive || !strings.Contains(it.Caption, "→ 050 review pending") || it.Bucket != "other" || it.BucketReason != webhookUnclassifiedReason {
		t.Fatalf("#15096 = %+v bucket %q/%q caption %q", it.New, it.Bucket, it.BucketReason, it.Caption)
	}

	// Rows that are placed stay as they were: a held issue, a stalled PR,
	// a claimed issue, a filtered sidelined row are not "new".
	for _, probe := range []struct {
		kind   string
		number int
	}{{"issue", 7}, {"issue", 8}, {"issue", 13}, {"pr", 101}, {"pr", 102}, {"pr", 103}} {
		if got := trackerItem(t, resp, probe.kind, probe.number); got.New != nil || got.StageSlug == StageTrackerNewSlug {
			t.Fatalf("%s #%d read as new: %+v", probe.kind, probe.number, got)
		}
	}

	// The governor's next publish, built from an enumeration that began
	// before the deliveries, does not list the three; the replay keeps them.
	fresh, _ := stageTrackerFixture(t, now)
	fresh.statusMu.RLock()
	republished := *fresh.status
	fresh.statusMu.RUnlock()
	s.UpdateStatus(&republished)
	again := s.buildStageTracker(s.status, store, now)
	for _, probe := range []struct {
		kind   string
		number int
	}{{"issue", 15092}, {"issue", 15095}, {"pr", 15096}} {
		if got := trackerItem(t, again, probe.kind, probe.number); got.New == nil {
			t.Fatalf("%s #%d lost on republish: %+v", probe.kind, probe.number, got)
		}
	}
}

// An out-of-order open read behind a remembered close does not re-list
// the item (the `unlabeled` read after the `closed` one), and a stale open
// read is not an insert either.
func TestWebhookOpenedInsertRespectsRememberedClose(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	s, _ := stageTrackerFixture(t, now)
	closedAt := now.Add(-time.Minute)
	if n := s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{Repo: "acme/console", Number: 8, State: "closed", UpdatedAt: closedAt}}); n == 0 {
		t.Fatal("close did not evict #8")
	}
	if n := s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{Repo: "acme/console", Number: 8, State: "open", Title: "stale", UpdatedAt: closedAt.Add(-time.Minute)}}); n != 0 {
		t.Fatalf("stale open read behind the close inserted %d rows", n)
	}
	if n := s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{Repo: "acme/console", Number: 8, State: "open", Title: "no timestamp"}}); n != 0 {
		t.Fatalf("untimed open read behind the close inserted %d rows", n)
	}
	if n := s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{Repo: "acme/console", Number: 8, State: "open", Title: "reopened", UpdatedAt: closedAt.Add(time.Minute)}}); n != 1 {
		t.Fatalf("a reopen newer than the close must re-list: %d", n)
	}
}

// The 000-new derivation on its own: who moves next, the caption, and what
// is not new (hold, blocked, draft, a stage record, a placed bucket).
func TestStageTrackerNewState(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-3 * time.Minute)
	gov := FrontendGovernor{NextKickAt: now.Add(5 * time.Minute).UTC().Format(time.RFC3339)}
	filter := &config.IssueFilterConfig{ReporterTrust: config.ReporterTrustConfig{UntrustedRequireLabels: []string{"triage/accepted", "kind/approved"}}}
	base := StageTrackerItem{Kind: "issue", Stage: 0, CreatedAt: &opened}

	n := stageTrackerNewState(base, false, filter, gov, now)
	if n == nil || n.NextMover != StageTrackerMoverHive || n.Text != "New · waiting for the stage-label pass (→ 010 actionable), then the scanner (next pass in 5m)" {
		t.Fatalf("plain actionable-list issue = %+v", n)
	}
	triage := base
	triage.Bucket = "reporter_triage"
	n = stageTrackerNewState(triage, false, filter, gov, now)
	if n == nil || n.NextMover != StageTrackerMoverMaintainer || n.Text != stageTrackerTriageNeededCaption || n.NextMoverText != "a maintainer's triage (add `triage/accepted` or `kind/approved`)" {
		t.Fatalf("reporter_triage issue = %+v", n)
	}
	// The accept label already on (the row menu's Accept triage, before the
	// gate's next poll moves the bucket): the hive is the next mover and the
	// triage gate reads done (hivecommons/hive#15215).
	accepted := triage
	accepted.Labels = []string{"enhancement", "Kind/Approved"}
	n = stageTrackerNewState(accepted, false, filter, gov, now)
	if n == nil || n.NextMover != StageTrackerMoverHive || n.Text != "New · waiting for the stage-label pass (→ 010 actionable), then the scanner (next pass in 5m)" {
		t.Fatalf("reporter_triage issue with the accept label = %+v", n)
	}
	if len(n.Gates) != 3 || n.Gates[0].State != StageTrackerGateDone || n.Gates[0].Text != "triage accepted (kind/approved)" || n.Gates[1].State != StageTrackerGateNext {
		t.Fatalf("accepted gates = %+v", n.Gates)
	}
	accepted.Modifiers = []string{modNeedsTriage}
	if n = stageTrackerNewState(accepted, false, filter, gov, now); n == nil || n.NextMover != StageTrackerMoverHive {
		t.Fatalf("a stale needs-triage beside the accept label = %+v", n)
	}
	labelled := base
	labelled.Modifiers = []string{modNeedsTriage}
	if n = stageTrackerNewState(labelled, false, nil, FrontendGovernor{}, now); n == nil || n.NextMover != StageTrackerMoverMaintainer || n.Text != stageTrackerTriageNeededCaption || n.NextPassAt != nil {
		t.Fatalf("needs-triage issue without a governor kick = %+v", n)
	}
	pr := StageTrackerItem{Kind: "pr", Stage: 0, CreatedAt: &opened}
	if n = stageTrackerNewState(pr, false, filter, gov, now); n == nil || n.Text != "New · waiting for the stage-label pass (→ 050 review pending) (next pass in 5m)" {
		t.Fatalf("plain PR = %+v", n)
	}
	due := FrontendGovernor{NextKickAt: now.Add(-time.Second).UTC().Format(time.RFC3339)}
	if n = stageTrackerNewState(pr, false, filter, due, now); n == nil || !strings.HasSuffix(n.Text, "(next pass due now)") || n.NextPassInSeconds != 0 {
		t.Fatalf("overdue kick = %+v", n)
	}

	notNew := map[string]StageTrackerItem{
		"stage":    {Kind: "issue", Stage: 10},
		"linked":   {Kind: "issue", LinkedPR: 15223},
		"links":    {Kind: "issue", LinkedPRs: []StageTrackerLinkedPR{{Number: 15223}}},
		"hold":     {Kind: "issue", Stage: 0, Hold: &StageTrackerHold{By: "pat"}, Modifiers: []string{modHold}},
		"blocked":  {Kind: "issue", Stage: 0, BlockedOn: []string{"#1"}, Modifiers: []string{modBlocked}},
		"draft":    {Kind: "pr", Stage: 0, Modifiers: []string{modDraft}},
		"filtered": {Kind: "issue", Stage: 0, Bucket: "filtered"},
		"advisory": {Kind: "issue", Stage: 0, Bucket: "hive_advisory"},
		"other":    {Kind: "issue", Stage: 0, Bucket: "other", BucketReason: "something the scanner said"},
	}
	for name, it := range notNew {
		if got := stageTrackerNewState(it, false, filter, gov, now); got != nil {
			t.Errorf("%s read as new: %+v", name, got)
		}
	}
	if got := stageTrackerNewState(base, true, filter, gov, now); got != nil {
		t.Errorf("a row with a store record read as new: %+v", got)
	}
	forge := base
	forge.Bucket, forge.BucketReason = "other", forgeUnclassifiedReason
	if got := stageTrackerNewState(forge, false, filter, gov, now); got == nil {
		t.Error("a forge-cache seeded row the scanner has not classified is new")
	}
	_ = stagelabels.None
}

// The Open column renders a 000-new row as "New · since Nm" in its own
// colour, with the next mover and the gates on hover.
func TestStageTrackerNewRowRendering(t *testing.T) {
	html := indexHTML(t)
	for _, fn := range []string{"stageTrackerNewState", "stageTrackerNewLines"} {
		if _, ok := jsFuncOptional(html, fn); !ok {
			t.Fatalf("%s missing from the SPA", fn)
		}
	}
	if !strings.Contains(html, ".ham-seg.active.ham-open-new {") {
		t.Fatal("no .ham-open-new rule in the stylesheet")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the 000-new rendering was NOT executed by this run")
	}
	script := strings.Join([]string{
		"const assert = require('assert');",
		"function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }",
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
		jsFunc(t, html, "stageTrackerNewState"),
		jsFunc(t, html, "stageTrackerOpenTriageNeeded"),
		jsFunc(t, html, "stageTrackerNewLines"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR_NONE"),
		jsFunc(t, html, "stageTrackerStageActor"),
		jsFunc(t, html, "stageTrackerActorKind"),
		jsFunc(t, html, "stageTrackerActorTip"),
		jsFunc(t, html, "stageTrackerHeadline"),
		jsFunc(t, html, "stageTrackerSegments"),
		jsFunc(t, html, "stageTrackerStripItems"),
		jsFunc(t, html, "stageTrackerTooltip"),
	}, "\n") + `
var _stageTracker = { stages: [] };
const now = Date.parse('2026-10-20T12:00:00Z');
const stages = [{ number: 10, label: 'Actionable', short: 'Actionable' }, { number: 20, label: 'Claimed', short: 'Claimed' }, { number: 30, label: 'PR in progress', short: 'PR in progress' }, { number: 50, label: 'Review pending', short: 'CI / Review' }, { number: 80, label: 'Awaiting merge admission', short: 'Merge gate' }, { number: 100, label: 'Merge queue', short: 'Merge queue' }];
const fresh = { repo: 'hivecommons/hive', number: 15092, kind: 'issue', title: 'Add a thing', author: 'kanrenibm', author_kind: 'human', stage: 0, stage_slug: '000-new',
  stage_since: '2026-10-20T11:52:00Z', created_at: '2026-10-20T11:52:00Z', labels: ['enhancement', 'help wanted'], bucket: 'reporter_triage',
  caption: 'Triage needed — accept or close',
  new: { since: '2026-10-20T11:52:00Z', next_mover: 'maintainer', next_mover_text: 'a maintainer\'s triage (add ` + "`triage/accepted`" + `)', next_pass_at: '2026-10-20T12:07:00Z', next_pass_in_s: 420,
    gates: [{ name: 'triage', state: 'next', text: 'a maintainer accepts triage (triage/accepted)' }, { name: 'actionable', state: 'later', text: 'the stage-label pass marks it 010 actionable' }, { name: 'claimed', state: 'later', text: 'the scanner claims it (020)' }],
    text: 'Triage needed — accept or close' } };

// A reporter-trust hold reads "Triage needed" in the Open column even
// before the needs-triage label has landed (hivecommons/hive#15437: the row
// said "New" and the ask lived only in the caption).
assert.strictEqual(stageTrackerHeadline(fresh), 'Triage needed');
assert.strictEqual(stageTrackerActorKind(fresh), 'human', 'a maintainer-gated new issue asks for a human move');
const segs = stageTrackerSegments(fresh, stages, now);
assert.strictEqual(segs[0].state, 'active');
assert.ok(segs[0].isNew, 'the Open segment is flagged new');
assert.strictEqual(segs[0].short, 'Triage needed · since 8m', segs[0].short);
const strip = stageTrackerStripItems(fresh, stages, null, now, {});
assert.ok(/<li class="ham-seg active ham-open-new"/.test(strip), strip);
assert.ok(strip.includes('<span class="ham-seg-t">Triage needed · since 8m</span>'), strip);
const chevronTip = /title="([^"]*)"/.exec(strip)[1];
assert.ok(chevronTip.startsWith('Triage needed — Next: a maintainer&#39;s triage') || chevronTip.startsWith('Triage needed — Next: a maintainer\'s triage'), chevronTip);
// With the label on (modifiers carry needs-triage) the word is the same.
const labelled = Object.assign({}, fresh, { labels: ['needs-triage'], modifiers: ['needs-triage'] });
assert.strictEqual(stageTrackerSegments(labelled, stages, now)[0].short, 'Triage needed · since 8m');
assert.strictEqual(stageTrackerHeadline(labelled), 'Triage needed');
// An admitted reporter's new issue still reads New.
const admitted = Object.assign({}, fresh, { bucket: '', new: Object.assign({}, fresh.new, { next_mover: 'hive', gates: [{ name: 'triage', state: 'done' }, { name: 'actionable', state: 'next' }] }) });
assert.strictEqual(stageTrackerSegments(admitted, stages, now)[0].short, 'New · since 8m');
assert.strictEqual(stageTrackerHeadline(admitted), 'New');
assert.ok(chevronTip.includes('next hive pass in 7m'), chevronTip);
assert.ok(chevronTip.includes('Gates ahead: triage (next) → actionable → claimed'), chevronTip);
assert.ok(chevronTip.includes('Next move: human'), chevronTip);
const tip = stageTrackerTooltip(fresh, now);
const lines = tip.split('\n');
assert.strictEqual(lines[0], 'Triage needed');
assert.ok(lines.some(l => l.startsWith('Next: a maintainer') && l.endsWith('next hive pass in 7m')), tip);
assert.ok(lines.includes('Gates ahead: triage (next) → actionable → claimed'), tip);
assert.ok(lines.includes('  claimed: the scanner claims it (020)'), tip);
assert.ok(lines.some(l => l.startsWith('Since T')), tip);

// A hive-moved new PR: the hive is the next mover, the triage gate already done reads with a tick.
const pr = { repo: 'hivecommons/hive', number: 15096, kind: 'pr', title: 'Fresh PR', stage: 0, stage_slug: '000-new', created_at: '2026-10-20T11:58:30Z',
  new: { next_mover: 'hive', next_mover_text: 'the stage-label pass (→ 050 review pending)', next_pass_at: '2026-10-20T11:59:00Z', gates: [{ name: 'review', state: 'next' }, { name: 'merge-gate', state: 'later' }], text: 'New · waiting for the stage-label pass (→ 050 review pending) (next pass due now)' } };
assert.strictEqual(stageTrackerActorKind(pr), 'hive');
assert.ok(stageTrackerNewLines(pr, now)[0].endsWith('next hive pass due now'), stageTrackerNewLines(pr, now));
assert.strictEqual(stageTrackerNewLines({ stage: 0, new: { next_mover: 'hive', gates: [{ name: 'triage', state: 'done' }, { name: 'actionable', state: 'next' }] } }, now)[1], 'Gates ahead: triage ✓ → actionable (next)');

// Not new: a plain Open row, a held row, a draft PR, a staged row — the word and the class stay as they were.
for (const it of [
  { kind: 'issue', stage: 0, caption: 'No stage recorded yet', labels: ['needs-triage'], modifiers: ['needs-triage'] },
  { kind: 'pr', stage: 0, modifiers: ['hold'], labels: ['hold'], new: { next_mover: 'hive' } },
  { kind: 'pr', stage: 0, modifiers: ['draft'], new: { next_mover: 'hive' } },
  { kind: 'issue', stage: 10, new: { next_mover: 'hive' } },
]) {
  const s = stageTrackerStripItems(it, stages, null, now, {});
  assert.ok(!s.includes('ham-open-new'), JSON.stringify(it) + ' → ' + s);
  assert.ok(!s.includes('>New'), JSON.stringify(it) + ' → ' + s);
  assert.notStrictEqual(stageTrackerHeadline(it), 'New');
}
assert.strictEqual(stageTrackerHeadline({ kind: 'issue', stage: 0, caption: 'No stage recorded yet', labels: ['needs-triage'], modifiers: ['needs-triage'] }), 'Triage needed');
assert.deepStrictEqual(stageTrackerNewLines({ stage: 0 }, now), []);
`
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
