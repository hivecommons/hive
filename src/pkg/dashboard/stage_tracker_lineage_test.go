package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// #15226 (@thatguywob, via a clanker relay) went red, the contributor
// stayed silent, the hive adopted it as #15308 and closed it. The copy's
// row must say so; the original (when rendered) must point at the copy.
func TestStageTrackerLineageIndex(t *testing.T) {
	adoptedAt := time.Date(2026, 10, 29, 4, 56, 0, 0, time.UTC)
	store := stagelabels.NewStore("", func() time.Time { return adoptedAt })
	orig := stagelabels.Key("hivecommons/hive", 15226)
	store.Observe(orig, stagelabels.ReturnedToContributor, "contributor-lane:ci-red")
	store.NoteAuthor(orig, "thatguywob")
	store.MarkAdopted(orig, "0a1b2c3", 15308, adoptedAt)
	store.MarkSupersededClosedFirst(orig, 15308, false, adoptedAt)
	// An in-place adoption names no second PR: no lineage.
	inPlace := stagelabels.Key("hivecommons/hive", 15400)
	store.Observe(inPlace, stagelabels.AdoptedByHive, "contributor-lane:adopted")
	store.MarkAdoptedInPlace(inPlace, "ffff000", automerge.AdoptReasonOffline, adoptedAt)

	idx := stageTrackerLineageIndex(store, "")
	if len(idx) != 2 {
		t.Fatalf("index = %+v", idx)
	}
	copyL, ok := idx[stagelabels.Key("hivecommons/hive", 15308)]
	if !ok || copyL.Kind != lineageAdopted || copyL.FromNumber != 15226 || copyL.FromAuthor != "thatguywob" || copyL.FromAuthorKind != authorKindClanker ||
		copyL.Reason != lineageReasonCIRedOffline || copyL.At == nil || !copyL.At.Equal(adoptedAt) || copyL.FromURL != "https://github.com/hivecommons/hive/pull/15226" {
		t.Fatalf("copy lineage = %+v", copyL)
	}
	origL, ok := idx[orig]
	if !ok || origL.Kind != lineageSuperseded || origL.ToNumber != 15308 || origL.ToURL != "https://github.com/hivecommons/hive/pull/15308" || origL.FromNumber != 0 {
		t.Fatalf("original lineage = %+v", origL)
	}
	if got := stageTrackerLineageIndex(nil, ""); len(got) != 0 {
		t.Fatalf("nil store = %+v", got)
	}
}

func TestStageTrackerLineageReasonsAndHead(t *testing.T) {
	for in, want := range map[string]string{
		"": lineageReasonCIRedOffline, automerge.AdoptReasonOffline: lineageReasonCIRedOffline, automerge.AdoptReasonGrace: lineageReasonCIRedOffline,
		automerge.AdoptReasonUnregistered: lineageReasonCIRedUnregistered, automerge.AdoptReasonOperator: lineageReasonOperatorAsked,
		automerge.AdoptReasonOperatorForced: lineageReasonOperatorAsked, automerge.AdoptReasonDCORehome: automerge.AdoptReasonDCORehome,
	} {
		if got := stageTrackerLineageReason(in); got != want {
			t.Errorf("reason(%q) = %q, want %q", in, got, want)
		}
	}
	// A copy whose record is gone still names its original off the branch.
	l := stageTrackerLineageFor(map[string]StageTrackerLineage{}, "hivecommons/hive", 15308, "hive/adopt-15226", "")
	if l == nil || l.Kind != lineageAdopted || l.FromNumber != 15226 || l.FromAuthor != "" {
		t.Fatalf("head lineage = %+v", l)
	}
	if stageTrackerLineageFor(map[string]StageTrackerLineage{}, "hivecommons/hive", 1, "feat/x", "") != nil {
		t.Fatal("plain branch must carry no lineage")
	}
	// With the original still a row, its author fields win over the guess.
	items := []StageTrackerItem{
		{Kind: "pr", Repo: "hivecommons/hive", Number: 15308, Lineage: &StageTrackerLineage{Kind: lineageAdopted, FromNumber: 15226}},
		{Kind: "pr", Repo: "hivecommons/hive", Number: 15226, Author: "thatguywob", AuthorKind: authorKindHuman},
	}
	stageTrackerLineageFillAuthors(items)
	if got := items[0].Lineage; got.FromAuthor != "thatguywob" || got.FromAuthorKind != authorKindHuman {
		t.Fatalf("filled = %+v", got)
	}
}

// The chip beside the PR number: "adopted from @thatguywob's #15226 · 6h",
// linked to the original, the author's kind as the icon only; the clause
// a caption or the Let-hive-decide preview adds after the PR number.
func TestStageTrackerLineageChipJS(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".ham-lineage {",
		"lineage: lp.lineage || null",
		"stageTrackerLineageChipHtml(item, nowMs)",
		"stageTrackerLineageChipHtml(lp, nowMs)",
		"stageTrackerLineageChipHtml(pr, nowMs) : '') ||",
		"stageTrackerRefOf(item) + (typeof stageTrackerLineageClause === 'function' ? stageTrackerLineageClause(item) : '')",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "stageTrackerAgoText"),
		jsConstBlock(t, html, "STAGE_TRACKER_LINEAGE_REASON_TIP"),
		jsFunc(t, html, "stageTrackerLineageText"),
		jsFunc(t, html, "stageTrackerSupersededOriginalClause"),
		jsFunc(t, html, "stageTrackerLineageTip"),
		jsFunc(t, html, "stageTrackerLineageChipHtml"),
		jsFunc(t, html, "stageTrackerLineageClause"),
		"function stageTrackerIcon(key, cls) { return '<svg class=\"' + cls + '\"></svg>'; }",
		`
const nowMs = Date.parse('2026-10-29T10:56:00Z');
const lineage = { kind: 'adopted', from_number: 15226, from_url: 'https://github.com/hivecommons/hive/pull/15226', from_author: 'thatguywob', from_author_kind: 'clanker', at: '2026-10-29T04:56:00Z', reason: 'ci-red-contributor-offline' };
const copy = { kind: 'pr', repo: 'hivecommons/hive', number: 15308, author_kind: 'hive', lineage };
assert.strictEqual(stageTrackerLineageText(copy, nowMs), "adopted from @thatguywob's #15226 · 6h");
assert.ok(stageTrackerLineageText(copy, nowMs).split(/\s+/).length <= 12);
const chip = stageTrackerLineageChipHtml(copy, nowMs);
assert.ok(chip.includes('href="https://github.com/hivecommons/hive/pull/15226"'), chip);
assert.ok(chip.includes('ham-lineage'), chip);
assert.ok(chip.includes('ham-author-clanker'), chip);
assert.ok(!/>[^<]*clanker[^<]*</.test(chip), 'author kind must be an icon, not a word: ' + chip);
assert.ok(chip.includes("title=\"Hive took this PR over after CI went red and @thatguywob&#39;s agent didn&#39;t respond. Original commits kept.\"") || chip.includes("title=\"Hive took this PR over after CI went red and @thatguywob's agent didn't respond. Original commits kept.\""), chip);
assert.strictEqual(stageTrackerLineageClause(copy), ' (adopted from #15226)');
assert.strictEqual(stageTrackerLineageClause({ kind: 'issue', number: 15198, linked_prs: [{ number: 15308, lineage }] }), ' (adopted from #15226)');
assert.strictEqual(stageTrackerLineageClause({ kind: 'pr', number: 1 }), '');
assert.strictEqual(stageTrackerLineageChipHtml({ kind: 'pr', number: 1 }, nowMs), '');
const orig = { kind: 'pr', number: 15226, lineage: { kind: 'superseded', to_number: 15308, to_url: 'https://github.com/hivecommons/hive/pull/15308', at: '2026-10-29T04:56:00Z' } };
assert.strictEqual(stageTrackerLineageText(orig, nowMs), 'superseded by #15308 · 6h');
assert.ok(stageTrackerLineageChipHtml(orig, nowMs).includes('href="https://github.com/hivecommons/hive/pull/15308"'));
assert.strictEqual(stageTrackerLineageClause(orig), '');
assert.strictEqual(stageTrackerLineageText({ lineage: { kind: 'adopted', from_number: 7 } }, nowMs), 'adopted from #7');
assert.ok(stageTrackerLineageTip({ lineage: { kind: 'adopted', from_number: 7, reason: 'operator-asked' } }).includes("maintainer's request"));
`,
	}, "\n")
	runStageTrackerRowActionsScript(t, script)
}
