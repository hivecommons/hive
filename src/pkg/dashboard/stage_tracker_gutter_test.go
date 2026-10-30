package dashboard

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestStageTrackerGutterVisibilityRule pins the gutter's reveal rule in the
// CSS (the owner's complaint on #13535 was that the checkbox + ⋯ appeared
// on an unpredictable subset of rows): the controls rest at the dimmed
// token opacity on every row — never opacity: 0 — go to full emphasis on
// the hovered / focused row, on a selected row, on every row while a
// selection exists or the context menu is open, and the only transition is
// the hover fade on the existing ≤ 120 ms token. Nothing keys on scroll,
// IntersectionObserver or row index.
func TestStageTrackerGutterVisibilityRule(t *testing.T) {
	html := indexHTML(t)
	start := strings.Index(html, "/* Row actions: a left gutter holds the select checkbox")
	end := strings.Index(html, ".ham-menu { position: fixed;")
	if start < 0 || end < start {
		t.Fatal("row actions gutter CSS block not found")
	}
	css := html[start:end]
	rest := regexp.MustCompile(`\.ham-sel, \.ham-kebab \{[^}]*opacity: var\(--ham-tool-rest-opacity\);[^}]*transition: opacity var\(--ham-tool-fade-ms\);`)
	if !rest.MatchString(css) {
		t.Errorf("gutter controls must rest at var(--ham-tool-rest-opacity) and fade on --ham-tool-fade-ms only:\n%s", css)
	}
	if strings.Contains(css, "opacity: 0;") || strings.Contains(css, "opacity: 0 ") || strings.Contains(css, "opacity:0") {
		t.Errorf("gutter controls must never be hidden at rest (opacity: 0):\n%s", css)
	}
	full := regexp.MustCompile(`([^{}]*)\{ opacity: 1; \}`).FindAllStringSubmatch(css, -1)
	var selectors []string
	for _, m := range full {
		for _, s := range strings.Split(m[1], ",") {
			selectors = append(selectors, strings.TrimSpace(s))
		}
	}
	joined := strings.Join(selectors, "\n")
	for _, want := range []string{
		".ham-row:hover .ham-sel", ".ham-row:hover .ham-kebab",
		".ham-row:focus-within .ham-sel", ".ham-row:focus-within .ham-kebab",
		".ham-row.ham-selected .ham-sel", ".ham-row.ham-selected .ham-kebab",
		".ham-has-sel .ham-sel", ".ham-has-sel .ham-kebab",
		".ham-menu-open .ham-sel", ".ham-menu-open .ham-kebab",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("gutter full-emphasis rule lacks selector %q; have:\n%s", want, joined)
		}
	}
	for _, banned := range []string{"IntersectionObserver", "nth-child", "nth-of-type", "scroll"} {
		if strings.Contains(css, banned) {
			t.Errorf("gutter reveal must not depend on %q", banned)
		}
	}
	if !strings.Contains(css, "@media (prefers-reduced-motion: reduce) { .ham-sel, .ham-kebab { transition: none; } }") {
		t.Error("gutter fade must be off under prefers-reduced-motion")
	}
	// The menu toggles .ham-menu-open on the panel, in both directions.
	for _, want := range []string{
		"function stageTrackerSetMenuOpen(el, on)",
		"stageTrackerSetMenuOpen(document.getElementById('activity-monitor-panel'), true);",
		"stageTrackerSetMenuOpen(document.getElementById('activity-monitor-panel'), false);",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	// Tokens: both token files carry the rest opacity and a fade ≤ 120 ms;
	// the theme allow-list knows the new token (ADR-0018).
	for _, path := range []string{"static/tokens.css", "../hub/static/tokens.css"} {
		tokens := gutterTestFile(t, path)
		m := regexp.MustCompile(`--ham-tool-rest-opacity: (0?\.\d+);`).FindStringSubmatch(tokens)
		if m == nil {
			t.Errorf("%s lacks --ham-tool-rest-opacity", path)
		} else if v, _ := strconv.ParseFloat(m[1], 64); v <= 0 || v >= 1 {
			t.Errorf("%s: --ham-tool-rest-opacity must dim, not hide or saturate; got %s", path, m[1])
		}
		fm := regexp.MustCompile(`--ham-tool-fade-ms: (\d+)ms;`).FindStringSubmatch(tokens)
		if fm == nil {
			t.Errorf("%s: --ham-tool-fade-ms must be a literal duration", path)
		} else if ms, _ := strconv.Atoi(fm[1]); ms > 120 {
			t.Errorf("%s: --ham-tool-fade-ms must be ≤ 120ms, got %sms", path, fm[1])
		}
	}
	if theme := gutterTestFile(t, "theme/theme.go"); !strings.Contains(theme, `"--ham-tool-rest-opacity": {}`) {
		t.Error("theme.go token allow-list lacks --ham-tool-rest-opacity")
	}
}

func gutterTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestStageTrackerGutterOnEveryRowAfterReconcile is the DOM invariant: every
// selectable row (issue or PR — not the column header, not a group:owner/repo
// header) carries exactly one .ham-row-tools gutter after any render path —
// the first render, a kept node, a node replaced in place, a new row
// entering mid-list, a row kept while mid-move (deferred) and a row stamped
// with a move — across many random snapshots, and the header rows never
// grow one. The rows are the real stageTrackerRowHtml / GroupHeaderHtml
// output fed to the real stageTrackerReconcile.
func TestStageTrackerGutterOnEveryRowAfterReconcile(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — gutter invariant was NOT executed by this run")
	}
	html := indexHTML(t)
	script := stageTrackerRepoGroupingHarness(t, html) + strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_MAX_ROWS"),
		jsConstLine(t, html, "STAGE_TRACKER_EXIT_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_ENTER_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_HEADER_KEY"),
		"var _stageTrackerRowSrc = new WeakMap();",
		jsFunc(t, html, "stageTrackerAfter"),
		jsFunc(t, html, "stageTrackerRowSrcKey"),
		jsFunc(t, html, "stageTrackerRowHasMove"),
		jsFunc(t, html, "stageTrackerRowAnimating"),
		jsFunc(t, html, "stageTrackerRowNode"),
		jsFunc(t, html, "stageTrackerRowSrcOf"),
		jsFunc(t, html, "stageTrackerRowMotionOn"),
		jsFunc(t, html, "stageTrackerRowExit"),
		jsFunc(t, html, "stageTrackerRowEnter"),
		jsFunc(t, html, "stageTrackerReconcile"),
		jsFunc(t, html, "stageTrackerSelectableKey"),
		jsFunc(t, html, "stageTrackerSelectionPrune"),
		jsFunc(t, html, "stageTrackerSubNodes"),
		jsFunc(t, html, "stageTrackerSubKeyOf"),
		jsFunc(t, html, "stageTrackerApplySelection"),
		jsFunc(t, html, "stageTrackerRowKeys"),
		jsFunc(t, html, "stageTrackerSetMenuOpen"),
	}, "\n") + `
global.setTimeout = () => 0;
function classes() { const set = new Set(); return { set, contains: c => set.has(c), add(...cs) { cs.forEach(c => set.add(c)); }, remove(...cs) { cs.forEach(c => set.delete(c)); } }; }
// A node remembers the HTML it was built from; querySelector answers the two
// questions the reconcile and the selection ask of it.
function base(htmlSrc) {
  const n = { parentNode: null, children: [], dataset: {}, classList: classes(), offsetHeight: 40, html: htmlSrc || '', animating: false,
    style: { setProperty() {} }, addEventListener() {},
    remove() { if (n.parentNode) { const c = n.parentNode.children; c.splice(c.indexOf(n), 1); n.parentNode = null; } },
    insertBefore(x, ref) { x.remove(); const i = ref ? n.children.indexOf(ref) : n.children.length; n.children.splice(i, 0, x); x.parentNode = n; },
    replaceChild(x, old) { const i = n.children.indexOf(old); x.remove(); n.children[i] = x; x.parentNode = n; old.parentNode = null; },
    querySelector(sel) {
      if (sel === '.ham-strip[data-ham-phase]') return n.animating ? {} : null;
      if (sel === '.ham-sel') return n.html.includes('class="ham-sel"') ? n.sel : null;
      return null;
    } };
  const a = {}; n.sel = { attrs: a, setAttribute(k, v) { a[k] = v; }, getAttribute(k) { return a[k]; } };
  return n;
}
global.document = { createElement() { const t = { content: {} }; Object.defineProperty(t, 'innerHTML', { set(v) { t.content.firstElementChild = base(v); } }); return t; } };

// Deterministic PRNG so a failure reproduces.
let seed = 0x5eed;
const rnd = () => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed / 0x7fffffff; };
const pick = a => a[Math.floor(rnd() * a.length)];
const repos = ['hivecommons/hive', 'hivecommons/pluk', 'hivecommons/dibs', 'hivecommons/docs'];
const stageNums = [10, 15, 20, 30, 45, 50, 70, 80, 85];

// The pool of items a snapshot draws from: issues (some covered by one or
// several PRs), standalone PRs (with and without a linked issue).
const pool = [];
let num = 100;
for (let i = 0; i < 18; i++) {
  const repo = pick(repos);
  if (rnd() < 0.5) {
    const it = I(repo, num++, pick(stageNums));
    const n = Math.floor(rnd() * 3);
    it.linked_prs = [];
    for (let j = 0; j < n; j++) it.linked_prs.push({ number: num++, url: 'https://x/' + num, stage: pick([30, 50, 70, 80, 85]), title: 'PR ' + num, author: 'bot', author_kind: 'hive' });
    pool.push(it);
  } else {
    const pr = P(repo, num++, pick([30, 50, 70, 80, 85]));
    if (rnd() < 0.6) pr.linked_issues = [{ number: num++, title: 'Issue ' + num, url: 'https://x/' + num, state: pick(['open', 'closed']) }];
    pool.push(pr);
  }
}

function snapshot(tick) {
  const ctx = { nowMs: now + tick * 1000, repoChips: { primary: 'hivecommons/hive', active: '' } };
  const items = pool.filter(() => rnd() < 0.8).map(it => {
    const c = Object.assign({}, it);
    if (rnd() < 0.2) c.stage = pick(stageNums);                       // a real change → node replaced
    if (rnd() < 0.15) c.stage_since = new Date(now - 60000 * Math.floor(rnd() * 500)).toISOString(); // "since" ticks
    if (rnd() < 0.1) c.modifiers = ['hold'];
    return c;
  });
  const grouped = tick % 3 === 1;
  const rows = [{ key: STAGE_TRACKER_HEADER_KEY, html: '<div class="ham-cols">cols</div>' }];
  const entries = [];
  if (grouped) {
    for (const g of stageTrackerGroups(items, { primary: 'hivecommons/hive', collapsed: {}, stages })) { entries.push({ group: g }); for (const it of g.rows) entries.push({ item: it }); }
  } else for (const it of items) entries.push({ item: it });
  for (const e of entries) {
    if (e.group) { rows.push({ key: e.group.key, html: stageTrackerGroupHeaderHtml(e.group, stages, ctx) }); continue; }
    const it = e.item;
    let h = stageTrackerRowHtml(it, stages, ctx);
    // Some rows arrive stamped with a move (the render after a column change).
    if (rnd() < 0.1) h = h.replace('aria-label="Pipeline progress"', 'aria-label="Pipeline progress" data-ham-prev-col="1" data-ham-col="3"');
    rows.push({ key: stageTrackerItemKey(it), html: h });
  }
  return rows;
}

const toolsRe = /<span class="ham-row-tools"><button type="button" class="ham-sel" role="checkbox"/g;
function check(el, label) {
  const live = el.children.filter(c => !c.classList.contains('ham-leaving'));
  const selectable = live.filter(c => stageTrackerSelectableKey(c.dataset.hamKey));
  // One gutter per row plus one per nested PR sub-row (each PR under an
  // issue is selected / acted on by itself).
  const subs = c => (c.html.match(/data-ham-sub-key="/g) || []).length;
  const withTools = live.filter(c => (c.html.match(toolsRe) || []).length === 1 + subs(c));
  assert.strictEqual(withTools.length, selectable.length, label + ': rows=' + selectable.length + ' rowsWithTools=' + withTools.length + ' missing=' + selectable.filter(c => (c.html.match(toolsRe) || []).length !== 1 + subs(c)).map(c => c.dataset.hamKey));
  for (const c of selectable) {
    assert.ok(c.html.indexOf('class="ham-row-tools"') < c.html.indexOf('class="ham-row-head"'), label + ': the gutter leads the row ' + c.dataset.hamKey);
    assert.ok(c.html.includes('class="ham-kebab"'), label + ': ⋯ present on ' + c.dataset.hamKey);
    assert.ok(!c.html.includes('class="ham-row-tools" style') && !c.html.includes('opacity'), label + ': no inline visibility on ' + c.dataset.hamKey);
  }
  for (const c of live.filter(c => !stageTrackerSelectableKey(c.dataset.hamKey))) {
    assert.ok(!c.html.includes('ham-row-tools'), label + ': header ' + c.dataset.hamKey + ' has no gutter');
  }
}

const el = base();
const totals = { kept: 0, replaced: 0, entered: 0, exited: 0, deferred: 0 };
let sel = new Set();
for (let tick = 0; tick < 60; tick++) {
  // Freeze a few nodes mid-move so the deferral path is exercised.
  for (const c of el.children) c.animating = rnd() < 0.15;
  const rows = snapshot(tick);
  const r = stageTrackerReconcile(el, rows, { effect: pick(['', 'wash', 'glide']), hidden: tick % 7 === 6 });
  for (const k of Object.keys(totals)) totals[k] += r[k];
  // The live page's post-render: prune + paint the selection.
  if (tick === 10) { const keys = stageTrackerRowKeys(el); sel = new Set(keys.slice(0, 2)); }
  sel = stageTrackerSelectionPrune(sel, stageTrackerRowKeys(el)); stageTrackerApplySelection(el, sel);
  check(el, 'tick ' + tick);
}
assert.ok(totals.kept > 0 && totals.replaced > 0 && totals.entered > 0 && totals.exited > 0 && totals.deferred > 0, 'every reconcile path was exercised: ' + JSON.stringify(totals));

// Selection / menu emphasis rides classes on the panel, no re-render.
assert.ok(el.classList.contains('ham-has-sel') || sel.size === 0);
stageTrackerSetMenuOpen(el, true); assert.ok(el.classList.contains('ham-menu-open'));
stageTrackerSetMenuOpen(el, false); assert.ok(!el.classList.contains('ham-menu-open'));
stageTrackerSetMenuOpen(null, true); // tolerant of a missing panel
`
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("node gutter invariant fixture failed: %v\n%s", err, out)
	}
}
