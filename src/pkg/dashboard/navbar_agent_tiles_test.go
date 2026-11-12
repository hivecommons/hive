package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// navbarAgentCSSBlock returns the body of the first CSS block in html whose
// text starts with header, from its opening brace to the matching close.
func navbarAgentCSSBlock(t *testing.T, html, header string) string {
	t.Helper()
	start := strings.Index(html, header)
	if start < 0 {
		t.Fatalf("static/index.html has no %q block", header)
	}
	open := strings.Index(html[start:], "{")
	if open < 0 {
		t.Fatalf("%q block has no opening brace", header)
	}
	depth := 0
	for i := start + open; i < len(html); i++ {
		switch html[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return html[start+open+1 : i]
			}
		}
	}
	t.Fatalf("%q block is unterminated", header)
	return ""
}

// navbarAgentTileCell reads the grid column and row a part of the navbar tile
// is placed in. The base rule is read first, then the same selector inside
// variant (a container-query block), which wins where it sets a value.
func navbarAgentTileCell(html, variant, part string) (col, row string) {
	selector := ".agent-navbar-tile " + part + " {"
	for _, css := range []string{html, variant} {
		i := strings.Index(css, selector)
		if i < 0 {
			continue
		}
		rule, _, _ := strings.Cut(css[i+len(selector):], "}")
		for _, decl := range strings.Split(rule, ";") {
			name, value, ok := strings.Cut(decl, ":")
			if !ok {
				continue
			}
			switch strings.TrimSpace(name) {
			case "grid-column":
				col = strings.TrimSpace(value)
			case "grid-row":
				row = strings.TrimSpace(value)
			}
		}
	}
	return col, row
}

// The navbar tile is a grid. In the one-line variant (centre column at most
// 560px) the label moved up to row 1 while staying in column 2, which is the
// status dot's cell, so the dot was drawn over the end of "RUNNING NOW". The
// name, the label and the dot must each have a cell of their own wherever
// they are shown.
func TestNavbarAgentTileDotAndLabelHaveOwnCells(t *testing.T) {
	html := indexHTML(t)
	oneLine := navbarAgentCSSBlock(t, html, "@container navbar-center (max-width: 560px)")
	dotOnly := navbarAgentCSSBlock(t, html, "@container navbar-center (max-width: 440px)")

	for _, tc := range []struct {
		name    string
		variant string
		parts   []string
	}{
		{"default", "", []string{".agent-tile-name", ".agent-tile-dot", ".agent-tile-next", ".agent-up-next"}},
		// .agent-tile-next is display:none from 560px down.
		{"at most 560px", oneLine, []string{".agent-tile-name", ".agent-tile-dot", ".agent-up-next"}},
	} {
		seen := map[string]string{}
		for _, part := range tc.parts {
			col, row := navbarAgentTileCell(html, tc.variant, part)
			if col == "" || row == "" {
				t.Errorf("%s: %s has no explicit grid cell (column %q, row %q)", tc.name, part, col, row)
				continue
			}
			cell := col + "/" + row
			if other, taken := seen[cell]; taken {
				t.Errorf("%s: %s and %s share grid cell column %s, row %s, so one is drawn over the other", tc.name, other, part, col, row)
			}
			seen[cell] = part
		}
	}

	for _, want := range []string{
		".agent-navbar-tile { min-width: 104px; grid-template-columns: max-content auto auto; }",
		".agent-navbar-tile .agent-tile-next { display: none; }",
	} {
		if !strings.Contains(oneLine, want) {
			t.Errorf("one-line navbar tile variant missing %q", want)
		}
	}
	// From 440px down the label is hidden, so the dot goes back to the second
	// of two columns instead of sitting after an empty one.
	for _, want := range []string{
		".agent-navbar-tile { min-width: 82px; grid-template-columns: max-content auto; }",
		".agent-navbar-tile .agent-up-next { display: none; }",
		".agent-navbar-tile .agent-tile-dot { grid-column: 2; }",
	} {
		if !strings.Contains(dotOnly, want) {
			t.Errorf("dot-only navbar tile variant missing %q", want)
		}
	}
}

// Three tiles wider than the centre column used to scroll sideways: the third
// tile was cut off and a scrollbar showed under the navbar. The strip now
// wraps and is capped at one row, so a tile either shows whole or not at all.
func TestNavbarAgentStripNeverScrolls(t *testing.T) {
	html := indexHTML(t)
	strip := navbarAgentCSSBlock(t, html, ".agent-navbar-upnext {")
	for _, banned := range []string{"overflow-x: auto", "overflow: auto", "overflow-x: scroll", "overflow: scroll", "scrollbar-width"} {
		if strings.Contains(strip, banned) {
			t.Errorf("navbar agent strip must not scroll, but its rule has %q", banned)
		}
	}
	for _, want := range []string{
		"flex-wrap: wrap;",
		"align-content: flex-start;",
		"overflow: hidden;",
		"max-height: var(--agent-navbar-row-cap);",
		// The row gap is the cap itself, so a wrapped row starts below the
		// visible row whatever height the tiles turn out to have.
		"gap: var(--agent-navbar-row-cap) var(--sp-2);",
	} {
		if !strings.Contains(strip, want) {
			t.Errorf("navbar agent strip rule missing %q", want)
		}
	}
	more := navbarAgentCSSBlock(t, html, "@container navbar-center (width > 340px)")
	for _, want := range []string{
		".agent-navbar-upnext[data-agent-more]::after { content: attr(data-agent-more); display: inline-flex; flex: 0 0 auto; }",
		".agent-navbar-upnext--measuring::after { order: -1; }",
	} {
		if !strings.Contains(more, want) {
			t.Errorf("navbar agent count chip rule missing %q", want)
		}
	}
	for _, want := range []string{
		".agent-navbar-tile[data-agent-overflow] { display: none; }",
		// Names are still never shortened in the navbar (#10671, #10914): a
		// tile that does not fit is dropped, not squeezed.
		".agent-navbar-tile { flex: 0 0 auto; min-width: 136px;",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("navbar agent strip contract missing %q", want)
		}
	}
	// The fit runs after every render, after a tile finishes leaving, after
	// the countdown text changes width, and when the centre column is resized.
	for _, tc := range []struct{ fn, want string }{
		{"renderAgentNavbarUpNext", "fitAgentNavbarTiles(wrap);"},
		{"renderAgentNavbarUpNext", "ensureAgentNavbarFitObserver(wrap);"},
		{"refreshAgentNavbarUpNextCountdowns", "fitAgentNavbarTiles(wrap);"},
		{"renderAgentNavbarTileDiff", "window.setTimeout(() => { tile.remove(); fitAgentNavbarTiles(wrap); }, AGENT_NAVBAR_EXIT_MS);"},
		{"ensureAgentNavbarFitObserver", "requestAnimationFrame(() => fitAgentNavbarTiles(wrap));"},
	} {
		if body := jsFunc(t, html, tc.fn); !strings.Contains(body, tc.want) {
			t.Errorf("%s() missing %q", tc.fn, tc.want)
		}
	}
}

// A replica ("adjudicator-2") is a copy of its base agent and keeps the base's
// displayName, so two tiles read "adjudicator". The tile, its title and its
// aria-label must say which copy it is; an agent that is not a copy is
// rendered exactly as before.
func TestAgentTilesMarkReplicas(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: the agent tile replica marker was NOT executed by this run")
	}
	html := indexHTML(t)
	start := strings.Index(html, "function agentCollapsedParseNextKick")
	if start < 0 {
		t.Fatal("agent tile helper block not found")
	}
	end := strings.Index(html[start:], "    function refreshAgentsCollapsedCountdowns")
	if end < 0 {
		t.Fatal("agent tile helper block end not found")
	}
	script := `const assert = require('node:assert/strict');
function agentIsDisabled(a) { return !!a && a.enabled === false; }
function _agentNeedsLogin() { return false; }
function formatDashboardDateTime() { return 'local time'; }
` + jsFunc(t, html, "esc") + `
` + html[start:start+end] + `
` + jsFunc(t, html, "agentNavbarTileHtml") + `
const now = Date.parse('2026-10-06T12:00:00Z');
const base = { name: 'adjudicator', displayName: 'adjudicator', state: 'running', busy: 'working', replicaIndex: 1, replicaCount: 2 };
const replica = { name: 'adjudicator-2', displayName: 'adjudicator', state: 'running', busy: 'working', replicaBase: 'adjudicator', replicaIndex: 2, replicaCount: 2 };
const solo = { name: 'reviewer', displayName: 'reviewer', state: 'running', busy: 'idle', nextKickIn: 'in 5m', replicaIndex: 1, replicaCount: 1 };
const head = { name: 'reviewer-fast-1', displayName: 'reviewer', state: 'running', busy: 'idle', replicaBase: 'reviewer', replicaIndex: 1, replicaCount: 1 };

assert.equal(agentTileReplicaInfo(null), null);
assert.equal(agentTileReplicaInfo(base), null);
assert.equal(agentTileReplicaInfo(solo), null);
assert.deepEqual(agentTileReplicaInfo(replica), { marker: '2', detail: 'replica 2 of 2' });
// A review head is also a copy of its base, numbered from 1 under its alias.
assert.deepEqual(agentTileReplicaInfo(head), { marker: 'fast-1', detail: 'fast-1' });
// No usable count, and a name that does not start with the base: the index still marks it.
assert.deepEqual(agentTileReplicaInfo({ name: 'other', replicaBase: 'adjudicator', replicaIndex: 3 }), { marker: '3', detail: 'replica 3' });

for (const [kind, render] of [['navbar', agentNavbarTileHtml], ['collapsed', agentCollapsedTileHtml]]) {
  const copy = render(replica, now, {});
  assert.ok(copy.includes('<span class="agent-tile-name agent-tile-name--replica"><span class="agent-tile-name-base">adjudicator</span><span class="agent-tile-replica">·2</span></span>'), kind + ' replica name: ' + copy);
  assert.ok(copy.includes('title="adjudicator · replica 2 of 2 '), kind + ' replica title: ' + copy);
  assert.ok(copy.includes('aria-label="adjudicator · replica 2 of 2 '), kind + ' replica aria-label: ' + copy);

  const reviewHead = render(head, now, {});
  assert.ok(reviewHead.includes('<span class="agent-tile-name-base">reviewer</span><span class="agent-tile-replica">·fast-1</span>'), kind + ' review head name: ' + reviewHead);
  assert.ok(reviewHead.includes('title="reviewer · fast-1 '), kind + ' review head title: ' + reviewHead);

  for (const [agent, name] of [[base, 'adjudicator'], [solo, 'reviewer']]) {
    const plain = render(agent, now, {});
    assert.ok(plain.includes('<span class="agent-tile-name">' + name + '</span>'), kind + ' plain name: ' + plain);
    assert.ok(plain.includes('title="' + name + ' '), kind + ' plain title: ' + plain);
    assert.ok(plain.includes('aria-label="' + name + ' '), kind + ' plain aria-label: ' + plain);
    assert.ok(!plain.includes('replica'), kind + ' plain tile must carry no replica marker: ' + plain);
    assert.ok(!plain.includes('·'), kind + ' plain tile must carry no marker dot: ' + plain);
  }
}
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("agent tile replica marker failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

// fitAgentNavbarTiles is run against a small model of the strip's layout: one
// row that wraps, tiles that never shrink, a count chip that is first in the
// row while the fit is measured and last afterwards. The model stands in for
// a browser; it checks the two-pass count, not the CSS.
func TestNavbarAgentStripDropsTilesThatDoNotFit(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: the navbar agent strip fit was NOT executed by this run")
	}
	html := indexHTML(t)
	start := strings.Index(html, "const AGENT_NAVBAR_STRIP_TITLE")
	if start < 0 {
		t.Fatal("navbar agent strip fit block not found")
	}
	end := strings.Index(html[start:], "    function ensureAgentNavbarFitObserver")
	if end < 0 {
		t.Fatal("navbar agent strip fit block end not found")
	}
	script := `const assert = require('node:assert/strict');
` + html[start:start+end] + `
const ROW = 36, GAP = 4, PAD = 4, CHIP = 34;
function strip(room, widths) {
  const center = {};
  const wrap = {
    attrs: {}, classes: new Set(), tiles: [], offsetTop: 6, offsetLeft: 100, clientHeight: ROW,
    setAttribute(k, v) { this.attrs[k] = String(v); },
    removeAttribute(k) { delete this.attrs[k]; },
    classList: { add(c) { wrap.classes.add(c); }, remove(c) { wrap.classes.delete(c); } },
    querySelectorAll() { return wrap.tiles; },
    get clientWidth() { return place(); },
  };
  // Lays the row out and returns the strip's width: its content when that
  // fits the room, the room otherwise.
  function place() {
    const chip = { width: CHIP };
    const laidOut = wrap.tiles.filter(tile => !('data-agent-overflow' in tile.attrs));
    let items = laidOut;
    if ('data-agent-more' in wrap.attrs) items = wrap.classes.has('agent-navbar-upnext--measuring') ? [chip].concat(laidOut) : laidOut.concat([chip]);
    const content = items.reduce((sum, item) => sum + item.width, 0) + GAP * Math.max(0, items.length - 1);
    const width = Math.min(room.available, content + PAD);
    let x = 0, row = 0;
    items.forEach(item => {
      if (x > 0 && x + item.width > width - PAD) { row += 1; x = 0; }
      item.x = x;
      item.row = row;
      x += item.width + GAP;
    });
    return width;
  }
  wrap.tiles = widths.map((width, id) => {
    const tile = {
      id, width, attrs: {}, exiting: false, x: 0, row: 0,
      setAttribute(k, v) { this.attrs[k] = String(v); },
      removeAttribute(k) { delete this.attrs[k]; },
      classList: { contains(c) { return c === 'exiting' && tile.exiting; } },
      get offsetParent() { return room.rendered !== false && !('data-agent-overflow' in tile.attrs) ? center : null; },
      get offsetTop() { if (!tile.offsetParent) return 0; place(); return wrap.offsetTop + tile.row * 2 * ROW; },
      get offsetLeft() { if (!tile.offsetParent) return 0; place(); return wrap.offsetLeft + tile.x; },
      get offsetWidth() { return tile.offsetParent ? tile.width : 0; },
    };
    return tile;
  });
  return wrap;
}
function shown(wrap) { return wrap.tiles.filter(tile => !('data-agent-overflow' in tile.attrs)).map(tile => tile.id); }
const TITLE = 'Upcoming agent run order';

// Everything fits: nothing is dropped and there is no count.
let room = { available: 600 };
let wrap = strip(room, [165, 165, 165]);
assert.equal(fitAgentNavbarTiles(wrap), 0);
assert.deepEqual(shown(wrap), [0, 1, 2]);
assert.equal('data-agent-more' in wrap.attrs, false);
assert.equal(wrap.attrs.title, TITLE);
assert.equal(wrap.attrs['aria-label'], TITLE);

// The third tile does not fit: it is dropped whole and counted.
room = { available: 440 };
wrap = strip(room, [165, 165, 165]);
assert.equal(fitAgentNavbarTiles(wrap), 1);
assert.deepEqual(shown(wrap), [0, 1]);
assert.equal(wrap.attrs['data-agent-more'], '+1');
assert.equal(wrap.attrs.title, TITLE + ' (1 more in the Agents section)');
assert.equal(wrap.attrs['aria-label'], wrap.attrs.title);
assert.equal(wrap.classes.has('agent-navbar-upnext--measuring'), false);

// The bar widens: the dropped tile comes back and the count goes away.
room.available = 600;
assert.equal(fitAgentNavbarTiles(wrap), 0);
assert.deepEqual(shown(wrap), [0, 1, 2]);
assert.equal('data-agent-more' in wrap.attrs, false);
assert.equal(wrap.attrs.title, TITLE);

// The count needs room too: two tiles fit alone, but only one beside the count.
room = { available: 360 };
wrap = strip(room, [165, 165, 165]);
assert.equal(fitAgentNavbarTiles(wrap), 2);
assert.deepEqual(shown(wrap), [0]);
assert.equal(wrap.attrs['data-agent-more'], '+2');
assert.equal(wrap.attrs.title, TITLE + ' (2 more in the Agents section)');

// One tile wider than the strip is dropped, not cut off.
room = { available: 120 };
wrap = strip(room, [165]);
assert.equal(fitAgentNavbarTiles(wrap), 1);
assert.deepEqual(shown(wrap), []);
assert.equal(wrap.attrs['data-agent-more'], '+1');

// A tile that is fading out still takes room, but is neither counted nor marked.
room = { available: 440 };
wrap = strip(room, [165, 165, 165]);
wrap.tiles[2].exiting = true;
assert.equal(fitAgentNavbarTiles(wrap), 0);
assert.equal('data-agent-overflow' in wrap.tiles[2].attrs, false);
assert.equal('data-agent-more' in wrap.attrs, false);

// A strip that is not rendered at this width has nothing to measure.
room = { available: 100, rendered: false };
wrap = strip(room, [165, 165, 165]);
assert.equal(fitAgentNavbarTiles(wrap), 0);
assert.deepEqual(shown(wrap), [0, 1, 2]);
assert.equal('data-agent-more' in wrap.attrs, false);
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("navbar agent strip fit failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
