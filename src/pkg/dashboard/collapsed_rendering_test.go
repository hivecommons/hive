package dashboard

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestCollapsedBodiesSkipRendering(t *testing.T) {
	html := indexHTML(t)
	// Both body variants must skip their descendants, without hiding the
	// deliberately visible summaries in collapsed-with-headline bodies.
	for _, selector := range []string{".dash-card-body", ".section-body"} {
		rule := regexp.MustCompile(regexp.QuoteMeta(selector+".collapsed:not(.collapsed-with-headline)") + `[^{}]*\{[^}]*content-visibility:\s*hidden;`)
		if !rule.MatchString(html) {
			t.Fatalf("%s lacks headline-safe rendering containment", selector)
		}
		base := regexp.MustCompile(regexp.QuoteMeta(selector) + `\s*\{[^}]*transition:[^;]*content-visibility 200ms`)
		if !base.MatchString(html) {
			t.Fatalf("%s must transition content visibility alongside height and opacity", selector)
		}
	}
	if !strings.Contains(html, ".dash-card-body, .section-body { transition-behavior: allow-discrete; }") {
		t.Fatal("content must stay rendered through collapse and become visible at expansion start")
	}
}

func TestCollapsedBodiesRestoreLayoutBeforeSparklineReplay(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: collapsed body layout regression was not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
const SECTION_EXPANDED_MAX_HEIGHT = 'none';
function classes() {
  const values = new Set();
  return {
    add: (...names) => names.forEach(name => values.add(name)),
    remove: (...names) => names.forEach(name => values.delete(name)),
    contains: name => values.has(name),
    toggle: (name, on) => on ? values.add(name) : values.delete(name),
  };
}
let measured = 0, released = 0, replayed = 0;
const body = {
  classList: classes(), style: {}, children: [], headline: false,
  querySelectorAll(selector) {
    return this.headline && selector === '.sec-headline,[data-collapsed-keep]'
      ? [{ closest: () => this }] : [];
  },
  get scrollHeight() {
    assert.equal(this.classList.contains('collapsed'), false,
      'remove rendering containment before measuring content');
    measured++;
    return 480;
  },
  get offsetHeight() { return 480; },
};
function releaseSectionMaxHeightAfterTransition(target) {
  assert.equal(target.style.maxHeight, '480px');
  released++;
}
const section = { classList: classes(), getAttribute: () => null };
const document = {
  getElementById: () => section,
  querySelector: selector => selector.includes('.dash-card-body,') ? body : null,
};
function replaySparklinesIn(root, opts) {
  assert.equal(root, section);
  assert.equal(body.classList.contains('collapsed'), false);
  assert.equal(body.style.maxHeight, '480px');
  assert.equal(measured, replayed === 0 ? 1 : 3);
  assert.equal(opts.force, true);
  replayed++;
}
` + jsFunc(t, html, "sectionHeadlineSelector") + "\n" +
		jsFunc(t, html, "clearCollapsedHeadlineVisibility") + "\n" +
		jsFunc(t, html, "applyCollapsedHeadlineVisibility") + "\n" +
		jsFunc(t, html, "applySectionBodyCollapse") + "\n" +
		jsFunc(t, html, "toggleSection") + `
// Restoring persisted collapse must not force layout of hidden descendants.
applySectionBodyCollapse(body, true, false);
assert.equal(body.classList.contains('collapsed'), true);
assert.equal(measured, 0);
// Expanding restores visibility before scrollHeight, then replays sparklines.
toggleSection('fixture', { collapsed: false, persist: false });
`
	// Repeat an animated close/open, then cover non-animated and headline paths.
	script += `
assert.equal(replayed, 1);
assert.equal(released, 1);
applySectionBodyCollapse(body, true, true);
assert.equal(body.classList.contains('collapsed'), true);
assert.equal(replayed, 1);
toggleSection('fixture', { collapsed: false, persist: false });
assert.equal(replayed, 2);
assert.equal(released, 2);
// Non-animated expansion also restores the uncapped body.
applySectionBodyCollapse(body, true, false);
applySectionBodyCollapse(body, false, false);
assert.equal(body.style.maxHeight, 'none');
assert.equal(body.classList.contains('collapsed'), false);
// Summaries are exempt from content-visibility:hidden, even on animated close.
body.headline = true;
const beforeHeadline = measured;
applySectionBodyCollapse(body, true, true);
assert.equal(body.classList.contains('collapsed-with-headline'), true);
assert.equal(body.style.maxHeight, 'none');
assert.equal(measured, beforeHeadline);
applySectionBodyCollapse(body, false, false);
assert.equal(body.classList.contains('collapsed-with-headline'), false);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("collapsed body layout regression failed: %v\n%s", err, out)
	}
}
