package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// When the navbar centre column is narrow the up-next strip collapses to a
// single summary control. It used to be a CSS ::after pseudo-element whose
// text was capped at 3 (the tile slice) and which could not receive clicks
// (#18110). These tests execute the real render functions against a tiny fake
// DOM so the count, the action wiring and the "no button when empty" rule are
// exercised, not just pattern-matched.
func TestAgentNavbarSummaryButtonCountAndAction(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: agent navbar summary behavior was not executed")
	}
	html := indexHTML(t)
	block := jsFunc(t, html, "agentNavbarSummaryLabel") + "\n" +
		jsFunc(t, html, "renderAgentNavbarSummary") + "\n" +
		jsFunc(t, html, "renderAgentNavbarUpNext")

	script := `
class El {
  constructor(tag) { this.tagName = tag.toUpperCase(); this.children = []; this.attrs = {}; this.className = ''; this._text = ''; this.parentNode = null; this.hidden = false; this.dataset = {}; }
  setAttribute(k, v) { if (k === 'class') this.className = String(v); else this.attrs[k] = String(v); }
  getAttribute(k) { if (k === 'class') return this.className; return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; }
  appendChild(c) { if (c.parentNode) c.parentNode.removeChild(c); c.parentNode = this; this.children.push(c); return c; }
  insertBefore(c, ref) { if (!ref) return this.appendChild(c); if (c.parentNode) c.parentNode.removeChild(c); const i = this.children.indexOf(ref); c.parentNode = this; this.children.splice(i, 0, c); return c; }
  removeChild(c) { this.children = this.children.filter(x => x !== c); c.parentNode = null; return c; }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  get lastElementChild() { return this.children[this.children.length - 1] || null; }
  get textContent() { return this.children.length ? this.children.map(c => c.textContent).join('') : this._text; }
  set textContent(v) { this.children = []; this._text = String(v); }
  querySelector(sel) { const cls = sel.replace(/^\./, ''); for (const c of this.children) { if (c.className.split(/\s+/).includes(cls)) return c; const d = c.querySelector(sel); if (d) return d; } return null; }
  querySelectorAll(sel) { const cls = sel.replace(/^\./, ''); const out = []; const walk = el => el.children.forEach(c => { if (c.className.split(/\s+/).includes(cls)) out.push(c); walk(c); }); walk(this); return out; }
}
const wrap = new El('span');
wrap.setAttribute('id', 'agent-navbar-upnext');
const document = { getElementById: id => id === 'agent-navbar-upnext' ? wrap : null, createElement: tag => new El(tag) };
const window = { _lastStatus: { governor: {} } };
function agentTileStates(agents) { return agents.map(a => ({ id: a.name, agent: a })); }
function agentNavbarHasSchedule(a) { return a.scheduled !== false; }
// Mirror the real diff's index-based insertBefore so tile placement relative
// to the summary button is exercised.
function renderAgentNavbarTileDiff(w, list) {
  w.querySelectorAll('.agent-navbar-tile').forEach(t => t.remove());
  list.forEach((item, i) => { const tile = new El('button'); tile.className = 'agent-navbar-tile'; tile.setAttribute('data-agent-key', item.id); w.insertBefore(tile, w.children[i] || null); });
}
function ensureAgentsCollapsedCountdownTicker() {}
// v6 measures the strip after every render; layout is out of scope here.
function fitAgentNavbarTiles() { return 0; }
function ensureAgentNavbarFitObserver() {}
` + block + `
function agents(n, extraUnscheduled) {
  const out = [];
  for (let i = 0; i < n; i++) out.push({ name: 'a' + i });
  for (let i = 0; i < (extraUnscheduled || 0); i++) out.push({ name: 'off' + i, scheduled: false });
  return out;
}
function fail(msg) { throw new Error(msg); }
function summary() { return wrap.querySelector('.agent-navbar-summary'); }
function check(name, n, extra, wantText) {
  renderAgentNavbarUpNext(agents(n, extra), 0);
  const btn = summary();
  if (!btn) fail(name + ': summary button missing');
  if (btn.tagName !== 'BUTTON' || btn.getAttribute('type') !== 'button') fail(name + ': summary is not a type=button <button>');
  if (btn.getAttribute('data-action') !== 'openAgentsUpNextPanel') fail(name + ': data-action = ' + btn.getAttribute('data-action'));
  const full = btn.querySelector('.agent-navbar-summary-full');
  if (!full || full.textContent !== wantText) fail(name + ': text = ' + (full && full.textContent) + ', want ' + wantText);
  if (!btn.querySelector('.agent-navbar-summary-short')) fail(name + ': narrow "Next" span missing');
  if (btn.textContent.includes('▾')) fail(name + ': summary still shows a dropdown caret');
  const title = btn.getAttribute('title') || '';
  if (!title.startsWith(wantText + ' in the run order') || btn.getAttribute('aria-label') !== title) fail(name + ': title/aria-label = ' + title);
  if (wrap.hidden) fail(name + ': strip hidden with scheduled agents');
  if (wrap.lastElementChild !== btn) fail(name + ': summary is not the last child of the strip');
  const tiles = wrap.querySelectorAll('.agent-navbar-tile');
  if (tiles.length !== Math.min(n, 3)) fail(name + ': tiles = ' + tiles.length);
  if (wrap.querySelectorAll('.agent-navbar-summary').length !== 1) fail(name + ': duplicate summary buttons');
}
check('nine scheduled agents are counted past the 3-tile cap', 9, 2, '9 agents');
check('a single scheduled agent reads singular', 1, 0, '1 agent');
check('two scheduled agents read plural', 2, 3, '2 agents');
renderAgentNavbarUpNext(agents(0, 4), 0);
if (summary()) fail('no scheduled agents must not render a summary button');
if (!wrap.hidden) fail('no scheduled agents must hide the strip');
check('the button comes back after an empty render', 4, 0, '4 agents');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent navbar summary JS failed: %v\n%s", err, out)
	}
}

func TestAgentNavbarSummaryStaticCSS(t *testing.T) {
	html := indexHTML(t)
	for _, gone := range []string{
		`content: attr(data-agent-summary)`,
		`.agent-navbar-upnext::after`,
		`agents ▾`,
	} {
		if strings.Contains(html, gone) {
			t.Fatalf("static/index.html still contains the unclickable pseudo-element pill %q", gone)
		}
	}
	base := cssRule(t, html, ".agent-navbar-summary")
	for _, want := range []string{"display: none", "cursor: pointer", "min-height: calc(var(--nav-chip-h) + var(--sp-4))"} {
		if !strings.Contains(base, want) {
			t.Fatalf(".agent-navbar-summary rule missing %q: %s", want, base)
		}
	}
	if !strings.Contains(html, ".agent-navbar-summary:hover, .agent-navbar-summary:focus-visible {") {
		t.Fatal(".agent-navbar-summary has no :focus-visible style")
	}
	containerBlock := func(query string) string {
		i := strings.Index(html, "@container navbar-center ("+query+") {")
		if i < 0 {
			t.Fatalf("static/index.html missing @container navbar-center (%s)", query)
		}
		end := strings.Index(html[i:], "\n    }")
		if end < 0 {
			t.Fatalf("unterminated @container navbar-center (%s)", query)
		}
		return html[i : i+end]
	}
	if q := containerBlock("max-width: 340px"); !strings.Contains(q, ".agent-navbar-upnext .agent-navbar-summary { display: inline-flex; }") {
		t.Fatalf("340px container query does not show the summary button:\n%s", q)
	}
	q := containerBlock("max-width: 220px")
	for _, want := range []string{
		".agent-navbar-summary .agent-navbar-summary-full { display: none; }",
		".agent-navbar-summary .agent-navbar-summary-short { display: inline; }",
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("220px container query missing %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "content:") {
		t.Fatalf("220px container query must toggle a span, not generate content:\n%s", q)
	}
}
