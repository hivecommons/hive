package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// The agents grid used to be rebuilt with `el.innerHTML = html` on every
// status tick. That destroyed every card node, so the browser's scroll anchor
// was lost (Chrome) and — because the cards' terminal tails (.doing) grow or
// shrink with each refresh — rows scrolled above the viewport changed height
// and pushed the row the reader was looking at. Safari has no scroll
// anchoring at all, so there the page visibly jumped on every refresh. These
// tests pin the in-place patching and the card-level scroll anchor that
// replaced it.

// TestAgentsGridPatchesInPlace asserts renderAgents patches the existing
// cards through morphAgentGrid rather than replacing the grid's innerHTML,
// follows the terminal tail only when the reader was already at the bottom,
// and re-focuses without scrolling the window.
func TestAgentsGridPatchesInPlace(t *testing.T) {
	html := indexHTML(t)
	body := jsFunc(t, html, "renderAgents")
	for _, snippet := range []string{
		"const tailFollow = agentTailFollowState(el);",
		"morphAgentGrid(el, html);",
		"if (!(key in tailFollow) || tailFollow[key]) d.scrollTop = d.scrollHeight;",
		"prev.focus({ preventScroll: true })",
	} {
		if !strings.Contains(body, snippet) {
			t.Errorf("renderAgents is missing %q — agents refresh regressed to a layout-shifting rebuild", snippet)
		}
	}
	for _, banned := range []string{"el.innerHTML = html", "scrollIntoView("} {
		if strings.Contains(body, banned) {
			t.Errorf("renderAgents contains %q — a refresh must never rebuild the grid or move the window", banned)
		}
	}
	if !strings.Contains(html, "const AGENT_TAIL_BOTTOM_SLACK_PX = 4;") {
		t.Error("index.html is missing AGENT_TAIL_BOTTOM_SLACK_PX")
	}
}

// TestScrollAnchorUsesFirstVisibleAgentCard asserts the render-time scroll
// anchor descends from the section to the first agent card still in view
// once the reader has scrolled into the section, and restores by card key so
// a rebuilt card is still found.
func TestScrollAnchorUsesFirstVisibleAgentCard(t *testing.T) {
	html := indexHTML(t)
	capture := jsFunc(t, html, "captureScrollAnchor")
	for _, snippet := range []string{
		"if (anchor.el && anchor.top < 0) {",
		"anchor.el.querySelectorAll('.agent-card[data-agent]')",
		"anchor.cardKey = cards[i].getAttribute('data-agent'); anchor.cardTop = rect.top; break;",
	} {
		if !strings.Contains(capture, snippet) {
			t.Errorf("captureScrollAnchor is missing %q", snippet)
		}
	}
	restore := jsFunc(t, html, "restoreScrollAnchor")
	for _, snippet := range []string{
		"if (anchor.cardKey && anchor.el && anchor.el.isConnected) {",
		"const delta = card.getBoundingClientRect().top - anchor.cardTop;",
	} {
		if !strings.Contains(restore, snippet) {
			t.Errorf("restoreScrollAnchor is missing %q", snippet)
		}
	}
}

// TestMorphDomNodeKeepsNodesAndState runs morphDomNode against a minimal DOM
// stand-in and checks the properties the fix depends on: kept node identity,
// patched text and attributes, preserved JS-set inline style, and form
// controls swapped only when their markup changed.
func TestMorphDomNodeKeepsNodesAndState(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — morphDomNode was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		jsFunc(t, html, "morphDomNode"),
		jsFunc(t, html, "morphDomAttributes"),
	}, "\n") + `
class N {
  constructor(tag, attrs, kids) { this.nodeType = 1; this.tagName = tag; this._attrs = Object.assign({}, attrs || {}); this.childNodes = kids || []; this.childNodes.forEach(k => k.parentNode = this); this.parentNode = null; this.scrollTop = 0; }
  get attributes() { return Object.keys(this._attrs).map(name => ({ name, value: this._attrs[name] })); }
  getAttribute(n) { return n in this._attrs ? this._attrs[n] : null; }
  setAttribute(n, v) { this._attrs[n] = String(v); }
  hasAttribute(n) { return n in this._attrs; }
  removeAttribute(n) { delete this._attrs[n]; }
  appendChild(c) { if (c.parentNode) c.remove(); c.parentNode = this; this.childNodes.push(c); return c; }
  remove() { if (!this.parentNode) return; const p = this.parentNode; p.childNodes.splice(p.childNodes.indexOf(this), 1); this.parentNode = null; }
  replaceWith(o) { const p = this.parentNode; const i = p.childNodes.indexOf(this); if (o.parentNode) o.remove(); p.childNodes[i] = o; o.parentNode = p; this.parentNode = null; }
  get outerHTML() { const a = Object.keys(this._attrs).sort().map(k => ' ' + k + '="' + this._attrs[k] + '"').join(''); return '<' + this.tagName + a + '>' + this.childNodes.map(c => c.outerHTML).join('') + '</' + this.tagName + '>'; }
}
class T { constructor(v) { this.nodeType = 3; this.nodeValue = v; this.parentNode = null; } remove() { const p = this.parentNode; p.childNodes.splice(p.childNodes.indexOf(this), 1); this.parentNode = null; } replaceWith(o) { const p = this.parentNode; p.childNodes[p.childNodes.indexOf(this)] = o; o.parentNode = p; } get outerHTML() { return this.nodeValue; } }
const fail = (m) => { console.log('FAIL ' + m); process.exitCode = 1; };

// 1. text + attribute patch keeps the node and its live state
const doing = new N('DIV', { class: 'doing' }, [new T('line 1')]);
doing.scrollTop = 42;
const details = new N('DIV', { class: 'agent-card-details', 'data-agent': 'a', style: 'height: 300px' }, [doing]);
const card = new N('DIV', { class: 'agent-card working', 'data-agent': 'a' }, [new N('DIV', { class: 'agent-state working' }, [new T('working')]), details]);
const root = new N('DIV', {}, [card]);
const next = new N('DIV', { class: 'agent-card idle', 'data-agent': 'a', 'data-agent-span': '2' }, [new N('DIV', { class: 'agent-state idle' }, [new T('idle')]), new N('DIV', { class: 'agent-card-details', 'data-agent': 'a' }, [new N('DIV', { class: 'doing' }, [new T('line 1\nline 2')])])]);
morphDomNode(card, next);
if (root.childNodes[0] !== card) fail('card node was replaced instead of patched');
if (card.getAttribute('class') !== 'agent-card idle' || card.getAttribute('data-agent-span') !== '2') fail('card attributes not synced: ' + card.outerHTML);
if (card.childNodes[1] !== details || card.childNodes[1].childNodes[0] !== doing) fail('nested nodes were replaced');
if (doing.childNodes[0].nodeValue !== 'line 1\nline 2') fail('text not patched');
if (doing.scrollTop !== 42) fail('doing scrollTop lost');
if (details.getAttribute('style') !== 'height: 300px') fail('JS-set inline style was stripped');
if (card.childNodes[0].getAttribute('class') !== 'agent-state idle') fail('state class not synced');

// 2. attribute removal + child add/remove
const el2 = new N('SPAN', { title: 'x', 'data-status': 'ok' }, [new T('a'), new T('b')]);
new N('DIV', {}, [el2]);
morphDomNode(el2, new N('SPAN', { title: 'y' }, [new T('a')]));
if (el2.hasAttribute('data-status') || el2.getAttribute('title') !== 'y' || el2.childNodes.length !== 1) fail('attr removal / child trim failed: ' + el2.outerHTML);
morphDomNode(el2, new N('SPAN', { title: 'y' }, [new T('a'), new N('B', {}, [new T('c')])]));
if (el2.childNodes.length !== 2 || el2.childNodes[1].tagName !== 'B') fail('child append failed');

// 3. tag change replaces; form controls swap only when markup differs
const wrap = new N('DIV', {}, [new N('SPAN', {}, [new T('✓ clean')])]);
morphDomNode(wrap.childNodes[0], new N('EM', {}, [new T('⛔ 3 blocked')]));
if (wrap.childNodes[0].tagName !== 'EM') fail('tag change did not replace');
const inp = new N('INPUT', { class: 'kick-prompt', id: 'kick-prompt-a' }, []);
inp.value = 'typed';
new N('DIV', {}, [inp]);
morphDomNode(inp, new N('INPUT', { class: 'kick-prompt', id: 'kick-prompt-a' }, []));
if (inp.parentNode === null || inp.value !== 'typed') fail('unchanged input was swapped');
const sel = new N('SELECT', { class: 'backend-select' }, [new N('OPTION', { value: 'claude' }, [])]);
const selWrap = new N('DIV', {}, [sel]);
morphDomNode(sel, new N('SELECT', { class: 'backend-select' }, [new N('OPTION', { value: 'copilot' }, [])]));
if (selWrap.childNodes[0] === sel) fail('changed select was not swapped');
console.log('ok');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") || strings.Contains(string(out), "FAIL") {
		t.Fatalf("morphDomNode behaviour regressed: %v\n%s", err, out)
	}
}
