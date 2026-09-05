package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #5822: the ACMM Evaluation panel reported ONE fleet-wide level, computed as
// the best case across every watched repo — a criterion counts as passed if any
// repo has it. A repo meeting none of the 43 criteria therefore did not move the
// headline at all, so the operator could not see it was there. The per-repo
// drill-down was half-wired: selecting a repo moved the level bars while the
// four headline tiles above them kept reporting the aggregate.
//
// These tests pin the four halves of the fix: the tiles follow the selector,
// the mixed scope is labelled rather than silent, the aggregate says what it
// is, and the ranking actually ranks.

// jsFunc extracts a top-level function declaration from index.html by
// brace-matching from its signature.
//
// It exists so the ordering rule can be EXECUTED (see
// TestACMMRepoRankingOrdersWeakestFirst) rather than only pattern-matched. A
// test that asserts a sort comparator appears verbatim catches a rule that was
// changed and misses one that was always wrong, which for a ranking is the
// failure that matters.
func jsFunc(t *testing.T, html, name string) string {
	t.Helper()
	start := strings.Index(html, "function "+name+"(")
	if start < 0 {
		t.Fatalf("index.html does not define %s()", name)
	}
	depth := 0
	for i := strings.Index(html[start:], "{") + start; i < len(html); i++ {
		switch html[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return html[start : i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces extracting %s() from index.html", name)
	return ""
}

// TestACMMRepoRankingOrdersWeakestFirst runs the panel's own ranking helpers
// against a fixture whose correct order is known by construction.
//
// The population is deliberately not sorted by any single field: "empty" has
// the largest gap and the lowest level, "strong" the smallest gap and the
// highest level, and "middling" sits between them, so a comparator that keyed
// on the wrong thing — percentage, level alone, name, or the raw payload order
// — produces a different sequence and this fails.
//
// Skips when node is unavailable. The skip is loud on purpose: a silently
// skipped behavioural test is exactly the "fails green" shape this repo's
// structure tests exist to avoid, so a run that did not execute the rule must
// say so rather than report a pass.
func TestACMMRepoRankingOrdersWeakestFirst(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the ACMM ranking rule was NOT executed by this run; the structure tests below still ran")
	}

	html := indexHTML(t)
	script := jsFunc(t, html, "acmmFirstFailingLevel") + "\n" +
		jsFunc(t, html, "acmmRepoGapRows") + "\n" + acmmRankingAssertions

	dir := t.TempDir()
	path := filepath.Join(dir, "ranking.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("ACMM ranking behaviour check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

// acmmRankingAssertions is the fixture and the expectations, kept as data so
// the extracted functions are the only code under test.
const acmmRankingAssertions = `
const lv = (level, passed) => ({ level, name: 'L' + level, passed });
const repos = [
  // Deliberately in an order that is neither the answer nor its reverse.
  { repo: 'strong', codebase_level: 4, criteria_passed: 40, criteria_total: 43,
    levels: [lv(1,true), lv(2,true), lv(3,true), lv(4,true), lv(5,false)] },
  { repo: 'empty', codebase_level: 0, criteria_passed: 0, criteria_total: 43,
    levels: [lv(1,false), lv(2,false)] },
  { repo: 'middling', codebase_level: 2, criteria_passed: 25, criteria_total: 43,
    levels: [lv(1,true), lv(2,true), lv(3,false)] },
];

let fails = 0;
function check(name, got, want) {
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    fails++;
    console.log('FAIL ' + name + '\n  got  ' + JSON.stringify(got) + '\n  want ' + JSON.stringify(want));
  }
}

const rows = acmmRepoGapRows(repos);
// The whole point of the panel change: the repo that meets nothing is FIRST.
check('weakest repo ranks first', rows.map(r => r.repo), ['empty', 'middling', 'strong']);
check('gap is unmet criteria', rows.map(r => r.gap), [43, 18, 3]);
check('percentages', rows.map(r => r.pct), [0, 58, 93]);
// Levels pass in order, so the FIRST failure is what a repo is blocked at.
check('blocked at the first failing level', rows.map(r => r.blocked ? r.blocked.level : null), [1, 3, 5]);

// A repo that passed every level has no blocker; reporting one would invent a
// problem on a healthy repo.
check('no blocker when every level passes', acmmFirstFailingLevel({ levels: [lv(1,true), lv(2,true)] }), null);

// A repo with no criteria at all must not divide by zero into NaN.
check('zero criteria yields 0%', acmmRepoGapRows([{ repo: 'z', criteria_passed: 0, criteria_total: 0, levels: [] }])[0].pct, 0);

// Determinism: this table is read repeatedly, and an order that shuffles
// between identical renders reads as churn that is not there.
check('equal gap and level break on name', acmmRepoGapRows([
  { repo: 'zulu', codebase_level: 2, criteria_passed: 20, criteria_total: 43, levels: [] },
  { repo: 'alpha', codebase_level: 2, criteria_passed: 20, criteria_total: 43, levels: [] },
]).map(r => r.repo), ['alpha', 'zulu']);

// Equal gap, different level: stuck lower is worse.
check('equal gap breaks on the lower level', acmmRepoGapRows([
  { repo: 'higher', codebase_level: 3, criteria_passed: 20, criteria_total: 43, levels: [] },
  { repo: 'lower', codebase_level: 1, criteria_passed: 20, criteria_total: 43, levels: [] },
]).map(r => r.repo), ['lower', 'higher']);

process.exit(fails ? 1 : 0);
`

// TestACMMTilesFollowTheRepoSelector pins the reported defect: the four
// headline tiles were written from the aggregate before the active view was
// resolved, so a repo click moved the bars below them and nothing else.
func TestACMMTilesFollowTheRepoSelector(t *testing.T) {
	html := indexHTML(t)

	// The exact defective writes. Each one is a tile reading the aggregate
	// where it must read the active view.
	for _, dead := range []string{
		"setIfChanged(document.getElementById('acmm-codebase-level'), fmtLevel(d.codebase_level));",
		"setIfChanged(document.getElementById('acmm-overall-level'), fmtLevel(d.overall_level));",
		"'<span class=\"stat-num\">' + d.criteria_passed + '/' + d.criteria_total + '</span>');",
	} {
		if strings.Contains(html, dead) {
			t.Errorf("a headline tile still reads the aggregate: %s\nclicking a repo chip would move the level bars and leave this tile on hive-wide numbers", dead)
		}
	}

	for _, live := range []string{
		"setIfChanged(document.getElementById('acmm-codebase-level'), fmtLevel(codebaseLevel));",
		"const codebaseLevel = pick(active.codebase_level, d.codebase_level);",
		"pick(active.criteria_passed, d.criteria_passed)",
	} {
		if !strings.Contains(html, live) {
			t.Errorf("index.html does not contain %q — the tiles do not follow the repo selection", live)
		}
	}

	// Operational Autonomy measures the hive, not a repository, so it must
	// stay on the aggregate — the mixed scope is deliberate and is labelled
	// below, not accidental.
	if !strings.Contains(html, "setIfChanged(document.getElementById('acmm-ops-level'), fmtLevel(d.operational_level));") {
		t.Error("Operational Autonomy no longer reads the hive-wide value — it is not a per-repo measurement")
	}

	// Overall is min(codebase, operational). The server computes it against the
	// UNION, so a selected repo needs it recomputed or the tile silently keeps
	// the fleet-wide reading beside a per-repo one.
	if !strings.Contains(html, "const overallLevel = scoped ? Math.min(codebaseLevel, d.operational_level) : d.overall_level;") {
		t.Error("Overall ACMM is not recomputed for a selected repo — it would disagree with the Codebase tile next to it")
	}
}

// TestACMMActiveViewIsResolvedBeforeTheTiles pins the ORDERING that caused the
// bug, not only its symptom. The active view being computed after the tile
// writes is what made the two halves of the card disagree; a future edit that
// moves it back down would reintroduce the same defect while every
// value-reading assertion above still passed.
func TestACMMActiveViewIsResolvedBeforeTheTiles(t *testing.T) {
	html := indexHTML(t)

	body := html[strings.Index(html, "function acmmRenderCard()"):]
	resolve := strings.Index(body, "const active = acmmGetActiveData()")
	firstTile := strings.Index(body, "document.getElementById('acmm-codebase-level')")

	if resolve < 0 {
		t.Fatal("acmmRenderCard never resolves the active view")
	}
	if firstTile < 0 {
		t.Fatal("acmmRenderCard never writes the codebase tile")
	}
	if resolve > firstTile {
		t.Error("acmmRenderCard resolves the active repo view AFTER writing the tiles — the tiles cannot follow a selection that does not exist yet")
	}

	// The second, now-redundant resolution below the tiles is gone: two calls
	// would let the halves drift apart again.
	if n := strings.Count(body[:strings.Index(body, "function fetchACMMEvaluation")], "acmmGetActiveData()"); n != 1 {
		t.Errorf("acmmRenderCard resolves the active view %d times, want exactly 1 — a second resolution is how the tiles and the bars came to disagree", n)
	}
}

// TestACMMPanelStatesTheAggregateIsAUnion pins that the panel itself says what
// the number means. Before this it was mentioned only inside the Codebase
// Readiness dialog, and only when more than one repo was configured — so an
// operator reading L3 on the card reasonably assumed every watched repo was
// at L3.
func TestACMMPanelStatesTheAggregateIsAUnion(t *testing.T) {
	html := indexHTML(t)

	if !strings.Contains(html, `id="acmm-scope-note"`) {
		t.Fatal("the panel has no scope note element — the union definition still lives only in a dialog")
	}
	for _, phrase := range []string{
		"a criterion counts as passed if <em>any</em> repo has it",
		"does not lower these numbers",
	} {
		if !strings.Contains(html, phrase) {
			t.Errorf("the scope note does not say %q — naming the aggregate without defining it is what made the headline misleading", phrase)
		}
	}

	// The mixed scope must be visible on the tiles themselves. An unlabelled
	// tile that ignores the click is indistinguishable from one that failed to
	// follow it.
	for _, tag := range []string{"acmm-lbl-codebase", "acmm-lbl-ops", "acmm-lbl-overall", "acmm-lbl-criteria"} {
		if !strings.Contains(html, `id="`+tag+`"`) {
			t.Errorf("tile label %s has no id — the renderer cannot stamp its scope", tag)
		}
	}
	if !strings.Contains(html, "scopeTag('hive-wide'") {
		t.Error("Operational Autonomy is not marked hive-wide when a repo is selected — the tile would silently ignore the selection")
	}
}

// TestACMMRepoTableAndChipsShowTheGap pins the answer to "which repo is lacking
// and by how much" being available WITHOUT clicking: finding the weakest repo
// used to mean opening the Codebase Readiness dialog and expanding each repo
// row one at a time, with nothing sorted by gap.
func TestACMMRepoTableAndChipsShowTheGap(t *testing.T) {
	html := indexHTML(t)

	if !strings.Contains(html, `id="acmm-repo-table"`) {
		t.Fatal("the panel has no per-repo table — the ranking has nowhere to render")
	}
	if !strings.Contains(html, "acmmRenderRepoTable(repos);") {
		t.Error("acmmRenderCard never renders the per-repo table")
	}
	if !strings.Contains(html, "Per-repo readiness — weakest first") {
		t.Error("the table does not state its ordering, so a reader cannot tell the first row is the worst")
	}
	// The chips carried a level but not the ratio, so "by how much" was still
	// N clicks away for N repos.
	if !strings.Contains(html, "btn.textContent = rr.repo + ' (L' + rr.codebase_level + ' · ' + rr.criteria_passed + '/' + rr.criteria_total + ')';") {
		t.Error("the repo chips do not carry the passed/total ratio")
	}
	// Rows select their repo, so the table and the selector are one control.
	if !strings.Contains(html, `data-action="acmmSelectRepo"`) {
		t.Error("table rows do not select their repo — the ranking would be read-only trivia next to a selector that ignores it")
	}
	// "weakest" is only claimed when the repos actually differ.
	if !strings.Contains(html, "const spread = rows.length > 1 && rows[0].gap > rows[rows.length - 1].gap;") {
		t.Error("the weakest badge is not gated on an actual spread — on a level fleet it would invent a laggard")
	}
}

// TestACMMRepoSelectorHasOneDisplayDeclaration pins the nit: the selector
// carried both display:none and display:flex in one style attribute, so the
// later declaration won and it rendered visible — holding only the "All
// (aggregate)" chip — until the first evaluation response hid it.
func TestACMMRepoSelectorHasOneDisplayDeclaration(t *testing.T) {
	html := indexHTML(t)

	const marker = `id="acmm-repo-selector" style="`
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatal("the repo selector element is missing")
	}
	style := html[i+len(marker):]
	style = style[:strings.Index(style, `"`)]

	if n := strings.Count(style, "display:"); n != 1 {
		t.Errorf("the repo selector's style has %d display declarations (%q), want exactly 1 — the later one wins and the selector flashes visible before the first fetch", n, style)
	}
	if !strings.Contains(style, "display:none") {
		t.Errorf("the repo selector does not start hidden (%q) — it would render with only the aggregate chip until data arrives", style)
	}
}
