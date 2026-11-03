package dashboard

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The owner's screenshot (dark theme, Activity Monitor): after "Approve &
// merge" the merge-gate chevron grew amber stripes and "Queuing for merge…"
// sat on them in muted grey — unreadable — while "Ready for human merge ·
// untrusted author" still showed beside it, wrapped to four lines. While an
// action is in flight a lit chevron shows ONE caption, on an opaque plate
// that meets WCAG AA in both themes, on one line.

const stageTrackerActingHideRule = `.cue-acting > .ham-strip-row > .ham-strip > .ham-seg.active:not(.cap) > :not(.cue-acting-cap) { display: none; }`

// stageTrackerActingPlateRule is the plate: page surface under page text,
// above the stripes and the pulse overlay, one line with an ellipsis.
const stageTrackerActingPlateRule = `.ham-seg.active > .cue-acting-cap { display: inline-block; position: relative; z-index: 1; margin-left: 0; max-width: 100%; min-width: 0; box-sizing: border-box; padding: var(--sp-1) var(--sp-2); border-radius: var(--r-pill); background: var(--surface-0); color: var(--text); font-weight: var(--fw-medium); line-height: 1.2; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }`

// wcagMinContrastAA is the WCAG 2.x AA ratio for normal text.
const wcagMinContrastAA = 4.5

func TestStageTrackerActingCaptionCSS(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{stageTrackerActingHideRule, stageTrackerActingPlateRule} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks the acting-caption rule %q", want)
		}
	}
	// The hide rule must come after the generic .ham-seg-t / icon display
	// rules so it wins on equal specificity, and the plate after the
	// generic .cue-acting-cap rule it overrides.
	if strings.Index(html, stageTrackerActingHideRule) < strings.Index(html, `.ham-seg-t { white-space: normal;`) {
		t.Error("the acting hide rule must follow the .ham-seg-t rule")
	}
	if strings.Index(html, stageTrackerActingPlateRule) < strings.Index(html, `.cue-acting-cap { display: inline-flex;`) {
		t.Error("the acting plate rule must follow the generic .cue-acting-cap rule")
	}
}

// TestStageTrackerActingCaptionContrast computes the plate's contrast from
// the token values the CSS resolves to in each theme: --text on --surface-0
// must clear AA. The old rendering (--text-muted straight on the --brand
// chevron) is recorded as the failing contrast it was.
func TestStageTrackerActingCaptionContrast(t *testing.T) {
	tokens, err := os.ReadFile("static/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	dark := cssBlockVars(t, string(tokens), ":root {")
	light := cssBlockVars(t, string(tokens), `[data-theme="light"],`)
	for name, vars := range map[string]map[string]string{"dark": dark, "light": light} {
		plate := contrastRatio(t, vars["--text"], vars["--surface-0"])
		if plate < wcagMinContrastAA {
			t.Errorf("%s theme: caption plate --text %s on --surface-0 %s is %.2f:1, want ≥ %.1f:1", name, vars["--text"], vars["--surface-0"], plate, wcagMinContrastAA)
		}
		t.Logf("%s theme: acting caption plate contrast %.2f:1", name, plate)
		before := contrastRatio(t, vars["--text-muted"], vars["--brand"])
		if before >= wcagMinContrastAA {
			t.Logf("%s theme: the old muted-on-amber caption would have been %.2f:1", name, before)
		}
	}
}

// cssBlockVars returns the custom properties declared in the block that
// starts with the given selector line.
func cssBlockVars(t *testing.T, css, selector string) map[string]string {
	t.Helper()
	i := strings.Index(css, selector)
	if i < 0 {
		t.Fatalf("tokens.css lacks block %q", selector)
	}
	body := css[i:]
	body = body[strings.Index(body, "{")+1:]
	body = body[:strings.Index(body, "}")]
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`(--[\w-]+)\s*:\s*([^;]+);`).FindAllStringSubmatch(body, -1) {
		out[m[1]] = strings.TrimSpace(m[2])
	}
	return out
}

// contrastRatio is WCAG 2.x relative-luminance contrast of two #rrggbb colours.
func contrastRatio(t *testing.T, fg, bg string) float64 {
	t.Helper()
	l1, l2 := relLuminance(t, fg), relLuminance(t, bg)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

func relLuminance(t *testing.T, hex string) float64 {
	t.Helper()
	hex = strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(hex) != 6 {
		t.Fatalf("expected a #rrggbb token value, got %q", hex)
	}
	ch := func(s string) float64 {
		v, err := strconv.ParseUint(s, 16, 8)
		if err != nil {
			t.Fatal(err)
		}
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(hex[0:2]) + 0.7152*ch(hex[2:4]) + 0.0722*ch(hex[4:6])
}

// TestStageTrackerActingCaptionNode: the rendered row nests the lit chevron
// exactly where the hide rule reaches it (row > .ham-strip-row > .ham-strip
// > .ham-seg.active), on the merge gate and on a decision step alike; and
// stageTrackerPendingSync leaves a chevron that already carries its word and
// glyphs with exactly one caption, whose siblings are all hidden by the
// rule — one label, never two.
func TestStageTrackerActingCaptionNode(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerDecidedUIJS(t, html)+`
window._hiveUser = 'clubanderson';
const gatePR = { number: 15102, title: 'fix', url: 'u2', stage: 90, author: 'Karibhou', author_kind: 'clanker', modifiers: ['untrusted-author'], human_merge_reason_text: 'untrusted author', manual_merge: true };
const gateIssue = { repo: 'hivecommons/hive', number: 15100, kind: 'issue', title: 't', url: 'u0', stage: 90, linked_prs: [gatePR] };
const decideIssue = { repo: 'hivecommons/hive', number: 15101, kind: 'issue', title: 't', url: 'u1', stage: 85, caption: 'Needs human decision', modifiers: ['needs-decision'] };
// Direct-child chain the hide rule walks from the keyed row (.ham-row gets
// its data-ham-key at reconcile; a nested .ham-sub carries data-ham-sub-key)
// > .ham-strip-row > ol.ham-strip > li.ham-seg.active (not the cap). Every
// lit chevron in the markup must be reachable that way — a lit chevron
// wrapped in anything else would keep both labels.
const chain = /<div class="(?:ham-row|ham-sub)[^"]*"[^>]*>(?:(?!<div class="(?:ham-row|ham-sub)[ "])[\s\S])*?<div class="ham-strip-row">\s*<ol class="ham-strip"[^>]*>(?:<li class="ham-seg (?!active)[^"]*"[^>]*>[\s\S]*?<\/li>)*<li class="ham-seg active(?! cap)[^"]*"/g;
for (const [label, item] of [['merge gate', gateIssue], ['decision', decideIssue]]) {
  const row = stageTrackerRowHtml(item, stages, ctx);
  const lit = (row.match(/<li class="ham-seg active(?! cap)[^"]*"/g) || []).length;
  const reachable = (row.match(chain) || []).length;
  assert.ok(lit > 0 && reachable === lit, label + ': ' + lit + ' lit chevron(s), ' + reachable + ' at row > .ham-strip-row > .ham-strip > .ham-seg.active:\n' + row);
  // The word is a child of the lit chevron, so the hide rule reaches it.
  assert.ok(/<li class="ham-seg active[^"]*"[^>]*>(?:<[^l][^>]*>|<svg[\s\S]*?<\/svg>)*?<(?:span|button) class="(?:ham-seg-t|ham-detour|ham-decision-trigger)/.test(row), label + ': the word is a direct child of the lit chevron:\n' + row);
}

// A chevron that carries its word, gate glyph and actor glyph: after the
// sync, the caption is one extra child; every other child is what the rule
// hides, and a re-sync adds nothing.
const r = hamStubEl('div'); r.setAttribute('data-ham-sub-key', 'hivecommons/hive/pr#15102');
const stripRow = hamStubEl('div'); stripRow.classList.add('ham-strip-row'); r.appendChild(stripRow);
const ol = hamStubEl('ol'); ol.classList.add('ham-strip'); stripRow.appendChild(ol);
const seg = hamStubEl('li'); seg.classList.add('ham-seg'); seg.classList.add('active'); ol.appendChild(seg);
for (const c of ['ham-gate-ico', 'ham-seg-t', 'ham-ico ham-actor-ico']) { const e = hamStubEl('span'); e.className = c; seg.appendChild(e); }
stripRow.querySelector = (sel) => sel === ':scope > .ham-strip > .ham-seg.active:not(.cap)' ? seg : (sel === ':scope > .cue-acting-cap' ? stripRow.children.find(c => c.classList.contains('cue-acting-cap')) || null : null);
hamPanel.children.length = 0; hamPanel.appendChild(r);
stageTrackerSetPending(['hivecommons/hive/pr#15102'], 'Queuing for merge…', now); stageTrackerPendingSync(hamPanel);
assert.ok(r.classList.contains('cue-acting'), 'the keyed row wears .cue-acting (the hide rule starts there)');
const caps = seg.children.filter(c => c.classList.contains('cue-acting-cap'));
assert.strictEqual(caps.length, 1, 'exactly one caption');
assert.strictEqual(caps[0].lastChild.nodeValue, 'Queuing for merge…');
assert.strictEqual(caps[0].getAttribute('role'), 'status');
const capEls = caps[0].children.filter(c => c.classList); // the stub lists the text node too
assert.strictEqual(capEls.length, 1, 'spinner + text only'); assert.ok(capEls[0].classList.contains('cue-acting-spin'));
const shown = seg.children.filter(c => !c.classList.contains('cue-acting-cap'));
assert.deepStrictEqual(shown.map(c => c.className), ['ham-gate-ico', 'ham-seg-t', 'ham-ico ham-actor-ico'], 'the word and glyphs stay in the DOM (the rule hides them) — nothing else is added beside the caption');
stageTrackerPendingSync(hamPanel);
assert.strictEqual(seg.children.filter(c => c.classList.contains('cue-acting-cap')).length, 1, 'a re-sync does not add a second caption');
_stageTrackerPending.length = 0; stageTrackerPendingSync(hamPanel);
assert.ok(!r.classList.contains('cue-acting') && !seg.children.some(c => c.classList.contains('cue-acting-cap')), 'word and glyphs come back alone once the action lands');
`)
}
