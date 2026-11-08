package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// The "agent pane · N lines" disclosure on an Operations contributor card must
// stay open across the 4-second fleet poll, which rebuilds every card with
// innerHTML, and a text selection inside it must survive that poll.

// jsSnippet returns the source from the first occurrence of start through the
// first following occurrence of end, inclusive.
func jsSnippet(t *testing.T, body, start, end string) string {
	t.Helper()
	i := strings.Index(body, start)
	if i < 0 {
		t.Fatalf("operations page missing %q", start)
	}
	j := strings.Index(body[i:], end)
	if j < 0 {
		t.Fatalf("operations page: no %q after %q", end, start)
	}
	return body[i : i+j+len(end)]
}

// paneDOM is a small fake of the #clanker-list element: each innerHTML write
// turns every rendered pane <details> into a fresh node with its own <pre>,
// the way a real rebuild does, and counts the writes.
const paneDOM = `
const assert = require('node:assert/strict');
var ccDeferredClankers = null, ccKnownClankers = {}, adminEnabled = false;
const handlers = {}, docHandlers = {};
function onEl(id, ev, fn) { handlers[id + ':' + ev] = fn; }
let writes = 0, nodes = [];
function paneNode(key, open) {
  const d = {key, open, getAttribute(a) { return a === 'data-pane' ? key : null; },
    classList: {contains(c) { return c === 'clanker-pane'; }}};
  d.pre = {nodeType: 1, scrollTop: 0, closest(s) { return s === '.clanker-pane' ? d : null; }};
  d.querySelector = s => s === 'pre' ? d.pre : null;
  return d;
}
const fleet = {
  html: '',
  querySelector() { return null; },
  querySelectorAll(s) { assert.equal(s, 'details.clanker-pane[open]'); return nodes.filter(n => n.open); },
  contains(n) { return n === fleet || nodes.some(d => d.pre === n); },
  set innerHTML(v) {
    writes++; this.html = v;
    nodes = [...v.matchAll(/<details class="clanker-pane" data-pane="([^"]*)"( open)?>/g)].map(m => paneNode(m[1], !!m[2]));
  }
};
const document = {
  getElementById(id) { return id === 'clanker-list' ? fleet : {}; },
  addEventListener(ev, fn) { docHandlers[ev] = fn; }
};
function ccUpdateArmy() {}
function ccRenderInterestRoster() {}
function rel() { return '21m ago'; }
function tierBadge() { return ''; }
function ccAdvisorLabel() { return ''; }
function capabilityLine() { return ''; }
function protocolLine() { return ''; }
function knowledgeLine() { return ''; }
function clankerInterestsLine() { return ''; }
function clankerFailureLine() { return ''; }
function clankerCapacityLine() { return ''; }
function clankerInFlightLines() { return ''; }
function clankerIdleLine() { return ''; }
function clankerPendingReworkLine() { return ''; }
function tail(tag) { return Array.from({length: 15}, (_, i) => tag + ' line ' + (i + 1)); }
function card(tag) { return [{github_username: 'KyleRankin', role: 'scanner', current_task: {repo: 'projectbluefin/finpilot', number: 636}, pane_tail: tail(tag)}]; }
// toggle drives the page's own toggle listener the way the browser does when
// the viewer clicks the pane's summary.
function toggle(open) {
  const m = fleet.html.match(/<details class="clanker-pane" data-pane="([^"]*)"/);
  assert.ok(m, 'no agent pane rendered');
  const d = nodes.find(n => n.key === m[1]);
  d.open = open;
  handlers['clanker-list:toggle']({target: d});
}
`

// TestOpsAgentPaneStaysOpenAcrossFleetPoll runs the real renderClankers,
// clankerPaneBlock and toggle listener. Before the fix the row key was
// declared after the pane block read it, so the pane rendered with an empty
// data-pane, the toggle listener ignored it, and every poll closed it.
func TestOpsAgentPaneStaysOpenAcrossFleetPoll(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required")
	}
	body := opsPageBody(t)
	script := paneDOM + `
function ccPaneSelectionActive() { return false; }
function ccPaneScrollsRead() { return {}; }
function ccPaneScrollsRestore() {}
` + jsFunc(t, body, "esc") + "\n" +
		jsSnippet(t, body, "var ccOpenPanes={};", "\n") +
		jsFunc(t, body, "clankerPaneBlock") + "\n" +
		jsSnippet(t, body, "onEl('clanker-list','toggle',", "},true);") + "\n" +
		jsFunc(t, body, "renderClankers") + `
renderClankers(card('first'));
assert.match(fleet.html, /<summary>agent pane &middot; 15 lines<\/summary>/, 'pane summary');
assert.match(fleet.html, /<details class="clanker-pane" data-pane="kylerankin">/, 'pane is keyed by the contributor');
toggle(true);
renderClankers(card('second'));
assert.match(fleet.html, /<details class="clanker-pane" data-pane="kylerankin" open>/, 'pane closed on the next fleet poll');
assert.match(fleet.html, /second line 15/, 'the open pane shows the fresh tail');
renderClankers(card('third'));
assert.match(fleet.html, /data-pane="kylerankin" open>/, 'pane closed on a later poll');
toggle(false);
renderClankers(card('fourth'));
assert.match(fleet.html, /<details class="clanker-pane" data-pane="kylerankin">/, 'a pane the viewer closed reopened');
`
	out, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("agent pane open state: %v\n%s", err, out)
	}
}

// TestOpsAgentPaneKeepsSelectionAndScroll: while the viewer has text selected
// in an agent pane the card rebuild waits (the summaries do not), the waiting
// update is drawn once the selection goes, and an open pane's scroll offset
// is carried across a rebuild.
func TestOpsAgentPaneKeepsSelectionAndScroll(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required")
	}
	body := opsPageBody(t)
	for _, want := range []string{
		"if(el.querySelector('.op-msg-form')||ccPaneSelectionActive(el)){ccDeferredClankers=list;return;}",
		"var paneScrolls=ccPaneScrollsRead(el);",
		"ccPaneScrollsRestore(el,paneScrolls);",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderClankers missing %q", want)
		}
	}
	script := paneDOM + `
let sel = null;
const window = {getSelection() { return sel; }};
` + jsFunc(t, body, "esc") + "\n" +
		jsSnippet(t, body, "var ccOpenPanes={};", "\n") +
		jsFunc(t, body, "clankerPaneBlock") + "\n" +
		jsSnippet(t, body, "onEl('clanker-list','toggle',", "},true);") + "\n" +
		jsFunc(t, body, "ccPaneSelectionActive") + "\n" +
		jsFunc(t, body, "ccPaneScrollsRead") + "\n" +
		jsFunc(t, body, "ccPaneScrollsRestore") + "\n" +
		jsSnippet(t, body, "document.addEventListener('selectionchange',", "\n});") + "\n" +
		jsFunc(t, body, "renderClankers") + `
renderClankers(card('a'));
toggle(true);
renderClankers(card('b'));
// Scrolled inside the open tail: the rebuilt <pre> keeps the offset.
nodes[0].pre.scrollTop = 40;
renderClankers(card('c'));
assert.equal(nodes[0].pre.scrollTop, 40, 'scroll offset lost on rebuild');
// Text selected inside the pane: the poll does not rebuild the card.
const textNode = {nodeType: 3, parentNode: nodes[0].pre};
sel = {isCollapsed: false, rangeCount: 1, anchorNode: textNode, focusNode: textNode};
const before = writes;
renderClankers(card('d'));
renderClankers(card('e'));
assert.equal(writes, before, 'a poll rebuilt the card under the selection');
assert.match(fleet.html, /c line 15/);
docHandlers.selectionchange();
assert.equal(writes, before, 'selectionchange redrew while the selection is still held');
// Selection released: the newest held update is drawn, pane still open.
sel = {isCollapsed: true, rangeCount: 1, anchorNode: textNode, focusNode: textNode};
docHandlers.selectionchange();
assert.equal(writes, before + 1, 'held update not drawn after the selection went');
assert.equal(ccDeferredClankers, null);
assert.match(fleet.html, /e line 15/);
assert.match(fleet.html, /data-pane="kylerankin" open>/);
// A selection elsewhere on the page does not hold the cards.
const outside = {nodeType: 1, closest() { return null; }};
sel = {isCollapsed: false, rangeCount: 1, anchorNode: outside, focusNode: outside};
renderClankers(card('f'));
assert.equal(writes, before + 2, 'a selection outside the panes held the cards');
docHandlers.selectionchange();
assert.equal(writes, before + 2, 'selectionchange redrew with nothing waiting');
`
	out, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("agent pane selection and scroll: %v\n%s", err, out)
	}
}
