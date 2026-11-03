package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15020: the hive held its own approved PR for the trust of
// an issue reporter and the row read "hold +1 · Needs human decision" with
// nothing saying who held it or why. The tracker now carries the hold's
// provenance from the stage store: the pill reads "Held by hive · reporter
// trust (issue #N by @X, contributor)" and the decision popover asks
// release / keep.

func reporterTrustHold15020(since time.Time) *stagelabels.HoldProvenance {
	return &stagelabels.HoldProvenance{By: stagelabels.HoldByHive, Source: stagelabels.HoldSourceReporterTrust,
		Reason: stagelabels.ReporterTrustReason(15017, "Karibhou", "CONTRIBUTOR"), Since: since, Issue: 15017, Reporter: "Karibhou", Association: "CONTRIBUTOR"}
}

func TestStageTrackerHoldProvenancePayload(t *testing.T) {
	now := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	heldAt := time.Date(2026, 10, 20, 17, 14, 14, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	key := stagelabels.Key("acme/console", 15020)
	store.Observe(key, stagelabels.NeedsDecision, "label:hold")
	store.NoteHold(key, reporterTrustHold15020(heldAt))
	keyPerson := stagelabels.Key("acme/console", 15021)
	store.Observe(keyPerson, stagelabels.Reviewed, "required-check-pending:build")
	store.NoteHold(keyPerson, &stagelabels.HoldProvenance{By: "clubanderson", Source: stagelabels.HoldSourceLabel, Since: heldAt})
	keyStale := stagelabels.Key("acme/console", 15022)
	store.Observe(keyStale, stagelabels.Reviewed, "queued")
	store.NoteHold(keyStale, reporterTrustHold15020(heldAt))
	s.statusMu.Lock()
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		FrontendPR{PullRequest: github.PullRequest{Number: 15020, Title: "fix(stage): hold provenance", Author: "hive-app[bot]", AppAuthored: true, Labels: []string{"stage/085-needs-decision", "hold", "needs-human", "author/hive", "role/trusted"}}},
		FrontendPR{PullRequest: github.PullRequest{Number: 15021, Title: "Parked by pat", Author: "hive-app[bot]", AppAuthored: true, Labels: []string{"stage/060-reviewed", "hold", "author/hive"}}},
		// The label is gone but the store still says held: no provenance rides a row without the label.
		FrontendPR{PullRequest: github.PullRequest{Number: 15022, Title: "Released", Author: "hive-app[bot]", AppAuthored: true, Labels: []string{"stage/060-reviewed", "author/hive"}}},
	)
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)

	it := trackerItem(t, resp, "pr", 15020)
	if it.Hold == nil || it.Hold.By != "hive" || it.Hold.Source != "reporter-trust" || it.Hold.Issue != 15017 || it.Hold.Reporter != "Karibhou" || it.Hold.Association != "contributor" {
		t.Fatalf("hold = %+v", it.Hold)
	}
	if it.Hold.Pill != "Held by hive · reporter trust (issue #15017 by @Karibhou, contributor)" || it.Hold.Since == nil || !it.Hold.Since.Equal(heldAt) {
		t.Fatalf("pill/since = %q %v", it.Hold.Pill, it.Hold.Since)
	}
	if !it.OnHold || !hasMod(it, modHold) {
		t.Fatalf("row must still wear the hold pill: on_hold=%v mods=%v", it.OnHold, it.Modifiers)
	}
	d := it.DecisionNeeded
	if d == nil {
		t.Fatal("a hive-held 085 row must carry a decision")
	}
	if !strings.Contains(d.Question, "The hive held this PR itself for reporter trust (issue #15017 by @Karibhou, contributor). Release the hold") || !strings.Contains(d.Question, "or keep it?") {
		t.Fatalf("question = %q", d.Question)
	}
	if d.AskedBy != decisionAskedByLane || d.BlockerReason != decisionBlockerHoldHive || !strings.Contains(d.Blocker, "The hive set `hold` on its own") || d.Since == nil || !d.Since.Equal(heldAt) {
		t.Fatalf("decision = %+v", d)
	}
	if len(d.Options) == 0 || d.Options[0].Action != decisionActionRelease || !d.Options[0].Enabled || d.Options[0].Label != "Release hold" {
		t.Fatalf("release must lead the options: %+v", d.Options)
	}
	if opt := decisionOptionByAction(t, d, decisionActionDelegate); opt.Enabled {
		t.Fatalf("Let hive decide must wait for the release: %+v", opt)
	}
	raw, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"hold":{"by":"hive","source":"reporter-trust","reason":"reporter trust (issue #15017 by @Karibhou, contributor)","since":"2026-10-20T17:14:14Z","issue":15017,"reporter":"Karibhou","association":"contributor","pill":"Held by hive · reporter trust (issue #15017 by @Karibhou, contributor)"}`) {
		t.Fatalf("payload hold = %s", raw)
	}

	person := trackerItem(t, resp, "pr", 15021)
	if person.Hold == nil || person.Hold.By != "clubanderson" || person.Hold.Pill != "Held by @clubanderson" || person.Hold.Source != "label" {
		t.Fatalf("person hold = %+v", person.Hold)
	}
	if person.DecisionNeeded != nil && strings.Contains(person.DecisionNeeded.Question, "held this PR itself") {
		t.Fatalf("a person's hold must not read as the hive's: %+v", person.DecisionNeeded)
	}
	if stale := trackerItem(t, resp, "pr", 15022); stale.Hold != nil {
		t.Fatalf("no hold label, no provenance: %+v", stale.Hold)
	}
	if clean := trackerItem(t, resp, "pr", 102); clean.Hold != nil {
		t.Fatalf("PR 102 must carry no hold: %+v", clean.Hold)
	}
}

// A person's hold on a 085 row names them in the blocker; an unattributed
// hold keeps the plain sentence.
func TestStageTrackerDecisionHeldBlockerNamesWhoHeld(t *testing.T) {
	since := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	it := StageTrackerItem{Repo: "acme/console", Number: 9, Kind: "pr", Stage: stagelabels.NeedsDecision.Number(), StageSince: &since, Reason: "label:hold", Modifiers: []string{modHold}}
	d := stageTrackerDecisionNeeded(it, nil, "")
	if d == nil || !strings.Contains(d.Blocker, decisionHeldBlockerPlain) || d.BlockerReason != "label:hold" {
		t.Fatalf("plain held decision = %+v", d)
	}
	it.Hold = &StageTrackerHold{By: "clubanderson", Source: "label", Pill: "Held by @clubanderson"}
	d = stageTrackerDecisionNeeded(it, nil, "")
	if d == nil || !strings.Contains(d.Blocker, "On hold (set by @clubanderson)") || strings.Contains(d.Question, "held this PR itself") {
		t.Fatalf("person-held decision = %+v", d)
	}
	// A hive hold the pass could only attribute to the label (no gate on
	// record) is named but asks no release/keep question of its own.
	it.Hold = &StageTrackerHold{By: "hive", Source: "label", Pill: "Held by hive"}
	d = stageTrackerDecisionNeeded(it, nil, "")
	if d == nil || !strings.Contains(d.Blocker, "On hold (set by the hive)") || strings.Contains(d.Question, "held this PR itself") {
		t.Fatalf("hive label-only decision = %+v", d)
	}
	if got := decisionPlainReason("contributor-lane:reporter-trust-hold"); !strings.Contains(got, "linked issue's reporter") {
		t.Fatalf("decisionPlainReason = %q", got)
	}
}

// The frontend: the hold pill names who held it and why, its hover text
// says since when, the nested PR mirror carries `hold`, and a hive-set hold
// pill opens the decision popover.
func TestStageTrackerHoldProvenanceJS(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"hold: lp.hold || null",
		"function stageTrackerHoldPill(item)",
		"function stageTrackerHoldTip(item, nowMs)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html lacks %q", want)
		}
	}
	if strings.Count(html, "hold: lp.hold || null") < 2 {
		t.Fatal("every nested-PR mirror must carry hold")
	}
	runStageTrackerRowActionsScript(t, strings.Join([]string{
		"const assert = require('assert');",
		"var _stageTracker = { stages: [{ number: 85, short: 'Needs decision' }] };",
		"function agentNowClockTime(ms, nowMs) { return 'T' + ms; }",
		"function agentNowAgo(ms) { return Math.round(ms / 60000) + 'm'; }",
		"function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }",
		"function stageTrackerItemKey(it) { return it.repo + '/' + it.kind + '#' + it.number; }",
		"function stageTrackerDecisionTriggerHtml(decision, inner, tip, cls) { return '<button class=\"' + cls + ' ham-decision-trigger\" data-ham-decision=\"' + esc(decision.key) + '\">' + inner + '</button>'; }",
		jsConstLine(t, html, "STAGE_TRACKER_MOD_PRIORITY"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_DRAFT_TIP"),
		stageTrackerSentinelJS(t, html),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerRegressionText"),
		jsFunc(t, html, "stageTrackerDecisionOf"),
		jsFunc(t, html, "stageTrackerHoldPill"),
		jsFunc(t, html, "stageTrackerHoldTip"),
		jsFunc(t, html, "stageTrackerChipsHtml"),
	}, "\n")+`
const hold = { by: 'hive', source: 'reporter-trust', reason: 'reporter trust (issue #15017 by @Karibhou, contributor)', since: '2026-10-20T17:14:14Z', issue: 15017, reporter: 'Karibhou', association: 'contributor', pill: 'Held by hive · reporter trust (issue #15017 by @Karibhou, contributor)' };
const decision = { question: 'The hive held this PR itself for reporter trust (issue #15017 by @Karibhou, contributor). Release the hold, or keep it?', asked_by: 'the hive', options: [ { action: 'release', label: 'Release hold', enabled: true } ] };
const item = { repo: 'hivecommons/hive', kind: 'pr', number: 15020, stage: 85, modifiers: ['hold', 'needs-human'], hold, decision_needed: decision };
assert.strictEqual(stageTrackerHoldPill(item), hold.pill);
const pill = stageTrackerChipsHtml(item);
assert.ok(pill.includes('ham-mod-hold'), pill);
assert.ok(pill.includes('>Held by hive · reporter trust (issue #15017 by @Karibhou, contributor) +1<'), pill);
assert.ok(pill.includes('ham-decision-trigger') && pill.includes('data-ham-decision="hivecommons/hive/pr#15020"'), pill);
const tip = stageTrackerHoldTip(item, Date.parse('2026-10-20T18:04:14Z'));
assert.ok(tip.startsWith(hold.pill), tip);
assert.ok(tip.includes('The hive set this hold itself') && tip.includes('Release it'), tip);
assert.ok(tip.includes('Since T' + Date.parse(hold.since) + ' · 50m'), tip);
// A person's hold: named, plain tooltip, no popover trigger.
const person = { repo: 'o/r', kind: 'pr', number: 2, stage: 60, modifiers: ['hold'], hold: { by: 'clubanderson', source: 'label' } };
assert.strictEqual(stageTrackerHoldPill(person), 'Held by @clubanderson');
const ppill = stageTrackerChipsHtml(person);
assert.ok(ppill.includes('>Held by @clubanderson<') && !ppill.includes('ham-decision-trigger'), ppill);
assert.ok(ppill.includes('title="Held by @clubanderson"'), ppill);
// No provenance: the plain word stays, no tooltip is invented.
const plain = stageTrackerChipsHtml({ repo: 'o/r', kind: 'pr', number: 3, stage: 60, modifiers: ['hold', 'draft'] });
assert.ok(plain.includes('>hold +1<'), plain);
assert.strictEqual(stageTrackerHoldPill({ hold: null }), '');
assert.strictEqual(stageTrackerHoldTip({ hold: {} }, 0), '');
// Without the pill field the text is built from the parts.
assert.strictEqual(stageTrackerHoldPill({ hold: { by: 'hive', reason: 'r' } }), 'Held by hive · r');
`)
}
