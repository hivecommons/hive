package dashboard

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestReaderUIWiring(t *testing.T) {
	html := indexHTML(t)
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile("static-src/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	js := read("js/reader.js")
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "new EventSource", "Math.random", "AudioContext", "new Audio"} {
		if strings.Contains(js, forbidden) {
			t.Errorf("reader.js contains forbidden sink/effect %q", forbidden)
		}
	}
	for _, want := range []string{"visibilitychange", "prefers-reduced-motion: reduce", "'&format=regions'", "source.addEventListener('code-lines'", "'wheel'", "'touchstart'", "'\\u2212 '"} {
		if !strings.Contains(js, want) {
			t.Errorf("reader.js missing %q", want)
		}
	}
	for _, want := range []string{"window.HIVE_READER = kiosk && view === 'reader';", "document.body.classList.add('reader-mode');", "hiveReaderStart();", "if (window.HIVE_READER) window.hiveReaderAttach(source);", `id="oc-reader-tab" href="?kiosk=1&amp;view=reader"`, "getElementById('oc-reader-tab')"} {
		if !strings.Contains(html, want) {
			t.Errorf("generated page missing %q", want)
		}
	}
	// The Matrix entry point stays until the cutover (#16477).
	if !strings.Contains(html, `id="oc-matrix-tab"`) || !strings.Contains(html, "window.HIVE_MATRIX = kiosk && view === 'matrix';") {
		t.Error("generated page lost the Matrix entry point")
	}
	start := strings.Index(html, `id="oc-reader-tab"`)
	open := html[strings.LastIndex(html[:start], "<a "):]
	open = open[:strings.Index(open, ">")]
	if !strings.Contains(open, " hidden") {
		t.Errorf("reader header tab %q must be hidden until the status payload enables it", open)
	}
	css := read("css/reader.css")
	// Long text wraps at a fixed size and the feed scrolls natively.
	for _, want := range []string{"white-space: pre-wrap; overflow-wrap: anywhere;", "body.reader-mode .reader-scroll { flex: 1 1 auto; min-height: 0; overflow-y: auto;", "font: 20px/1.5 monospace;", "body.reader-mode .reader-context { color: var(--reader-text); }"} {
		if !strings.Contains(css, want) {
			t.Errorf("reader.css missing %q", want)
		}
	}
	for _, forbidden := range []string{"text-overflow", "vw;", "line-clamp", "nowrap"} {
		if strings.Contains(css, forbidden) {
			t.Errorf("reader.css must not shrink or truncate text (%q)", forbidden)
		}
	}
	for _, rule := range strings.Split(css, "}") {
		selector, _, ok := strings.Cut(strings.TrimSpace(rule), "{")
		if !ok {
			continue
		}
		for _, part := range strings.Split(selector, ",") {
			if !strings.HasPrefix(strings.TrimSpace(part), "body.reader-mode ") {
				t.Errorf("unscoped reader selector %q", part)
			}
		}
	}
	manifest := read("manifest.txt")
	for _, file := range []string{"js/reader.js", "css/reader.css"} {
		if !strings.Contains(manifest, file+"\n") {
			t.Errorf("manifest missing %s", file)
		}
		if !strings.Contains(html, read(file)) {
			t.Errorf("generated page is missing current %s", file)
		}
	}
}

// Exercise rendering, pacing, pause and reduced motion with deterministic
// DOM/clock doubles; no browser, network or real timers required.
func TestReaderUIBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	cmd := exec.Command(node, "-e", readerBrowserTest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reader browser behavior: %v\n%s", err, out)
	}
}

const readerBrowserTest = `
const assert = require('assert');
const fs = require('fs');
const vm = require('vm');
const reader = fs.readFileSync('static-src/js/reader.js', 'utf8');
const boot = fs.readFileSync('static-src/js/boot.js', 'utf8').split('})();')[0] + '})();';
const at = '2026-11-03T12:00:00Z';
const first = [
  {seq: 1, repo: 'org/repo', pr: 2, head: 'abc', path: 'main.go', line: '<script>alert(1)</script>', op: '+', at},
  {seq: 2, repo: 'org/repo', pr: 2, head: 'abc', path: 'main.go', line: '', at},
  {seq: 3, repo: 'org/repo', pr: 2, head: 'abc', path: 'main.go', line: '\tx := 1', at},
  {seq: 4, repo: 'org/repo', pr: 2, head: 'abc', path: 'main.go', line: '\tx := 1', at},
  {seq: 5, repo: 'org/repo', pr: 2, head: 'abc', path: 'main.go', line: '    old()', op: '-', at},
  {seq: 6, repo: 'org/repo', pr: 2, head: 'abc', path: 'main.go', line: '\tunchanged()', op: ' ', at},
  {seq: 7, repo: 'org/repo', pr: 2, head: 'abc', path: 'main.go', line: '', op: ' ', at},
  {seq: 8, repo: 'org/other', pr: 7, head: 'def', path: 'README.md', line: 'a very long line '.repeat(40), pr_url: 'https://github.com/org/other/pull/7', at},
];
async function scenario(query, opts = {}) {
  const docEvents = {}, timers = new Map(), requests = [], scrolls = [], sse = {};
  let id = 0, status = opts.status || 200, seq = 8, lines = first;
  const elements = [];
  function el(tag) { const e = {tagName: tag.toUpperCase(), children: [], listeners: {}, attrs: {}, className: '',
    textContent: '', hidden: false, scrollTop: 0, offsetHeight: 10, classList: {add(){}},
    setAttribute(k, v){this.attrs[k] = v}, append(...a){this.children.push(...a)}, appendChild(a){this.children.push(a); return a},
    removeChild(a){this.children.splice(this.children.indexOf(a), 1)}, replaceChildren(...a){this.children = a},
    addEventListener(n, fn){this.listeners[n] = fn}, scrollIntoView(o){scrolls.push(o)}};
    elements.push(e); return e; }
  const doc = {body: el('body'), hidden: false, createElement: el,
    getElementById(id){return elements.find(e => e.id === id)},
    addEventListener(n, fn){(docEvents[n] ||= []).push(fn)}};
  const motion = {matches: !!opts.reduced, addEventListener(n, fn){this.change = fn}};
  const win = {location: {href: 'https://hive.test/' + query}, matchMedia(){return motion}, addEventListener(){}};
  const c = vm.createContext({window: win, document: doc, URL, location: {hash: ''}, history: {replaceState(){}},
    localStorage: {getItem(){return null}, setItem(){}},
    requestAnimationFrame(){throw new Error('reader must not animate frames')},
    setTimeout(fn){timers.set(++id, fn); return id}, clearTimeout(i){timers.delete(i)}, setInterval(){}, dashboardSetInterval(){},
    fetch(url){requests.push(url); const regions = [];
      lines.forEach(line => {
        let last = regions[regions.length - 1];
        if (!last || last.path !== line.path || last.repo !== line.repo) regions.push(last = {...line, seq: line.seq, lines: []});
        last.lines.push({op: line.op || '+', line: line.line});
      });
      let body = {seq, latest: seq, epoch: 'policy-1', source: 'Pushed PR', regions}; lines = [];
      if (opts.pages && opts.pages.length) body = opts.pages.shift();
      return Promise.resolve({status, ok: status === 200, json(){return Promise.resolve(body)}})},
    Date, Math});
  vm.runInContext(reader, c); c.hiveReaderStart = win.hiveReaderStart;
  vm.runInContext(boot, c);
  for (const fn of docEvents.DOMContentLoaded || []) fn();
  const flush = async () => {for (let i = 0; i < 64; i++) await Promise.resolve()};
  await flush();
  if (!win.HIVE_READER) {assert.equal(requests.length, 0); assert(!doc.getElementById('hive-reader')); return;}
  const byClass = n => elements.find(e => e.className === n);
  const feed = byClass('reader-feed'), statusEl = byClass('reader-status'), button = byClass('reader-pause');
  const fire = () => {assert.equal(timers.size, 1); const [k, fn] = [...timers][0]; timers.delete(k); fn()};
  const key = (key, code) => docEvents.keydown[0]({key, code, target: doc.body, preventDefault(){}});
  assert.equal(requests[0], '/api/matrix/lines?since=0&format=regions');
  if (status !== 200) {
    assert.equal(feed.children.length, 0); assert.equal(timers.size, 0);
    assert(statusEl.textContent.includes(status === 404 ? 'not enabled' : 'Sign in'));
    return;
  }
  const meta = card => card.children[0].children, rows = card => card.children[1].children.map(r => r.textContent);
  if (opts.boundaries) {
    // Two complete regions in one file remain separate across pages. The
    // reader follows continuation rather than skipping to source.latest.
    assert.equal(requests.length, 2);
    assert.equal(requests[1], '/api/matrix/lines?since=1&format=regions');
    assert.equal(feed.children.length, 2);
    assert.deepEqual(rows(feed.children[0]), ['+ one\n']);
    assert.deepEqual(rows(feed.children[1]), ['− two\n']);
    assert.equal(meta(feed.children[1])[3].textContent, 'Buffered snapshot');
    win.hiveReaderAttach({addEventListener(n, fn){sse[n] = fn}});
    opts.pages.push({seq: 3, latest: 3, epoch: 'policy-2', regions: [{repo: 'org/repo', pr: 2, path: 'main.go', seq: 3, at, lines: [{op: '+', line: 'new policy'}]}]});
    sse['code-lines'](); await flush();
    assert.equal(feed.children.length, 1);
    assert.deepEqual(rows(feed.children[0]), ['+ new policy\n']);
    byClass('reader-pause reader-replay').listeners.click();
    assert.equal(feed.children.length, 1); // old policy is gone from replay too
    assert.deepEqual(rows(feed.children[0]), ['+ new policy\n']);
    // Busy feeds evict whole regions under line, text and viewport bounds.
    const base = {repo: 'org/repo', pr: 2, path: 'main.go', at};
    opts.pages.push({seq: 10, latest: 10, epoch: 'policy-2', regions: Array.from({length: 7}, (_, i) => ({...base, seq: i + 4, lines: Array.from({length: 200}, () => ({op: '+', line: 'bounded line'}))}))});
    sse['code-lines'](); await flush();
    assert(feed.children.reduce((n, card) => n + rows(card).length, 0) <= 1200);
    assert(feed.children.every(card => rows(card).length === 1 || rows(card).length === 200));
    opts.pages.push({seq: 50, latest: 50, epoch: 'policy-2', regions: Array.from({length: 40}, (_, i) => ({...base, seq: i + 11, lines: [{op: '+', line: 'one'}]}))});
    sse['code-lines'](); await flush(); assert.equal(feed.children.length, 32);
    opts.pages.push({seq: 70, latest: 70, epoch: 'policy-2', regions: Array.from({length: 20}, (_, i) => ({...base, seq: i + 51, lines: [{op: '+', line: 'safe '.repeat(3000)}]}))});
    sse['code-lines'](); await flush();
    assert(feed.children.reduce((n, card) => n + rows(card).join('').length * 2, 0) < 256 * 1024);
    status = 404; sse['code-lines'](); await flush();
    assert.equal(feed.children.length, 0);
    return;
  }

  const checkFirst = card => {
    // One contiguous region: blank, indented and repeated lines kept as text.
    // Context retains its space prefix and stays neutral, including blank lines.
    assert.deepEqual(rows(card), ['+ <script>alert(1)</script>\n', '+ \n', '+ \tx := 1\n', '+ \tx := 1\n', '\u2212     old()\n', '  \tunchanged()\n', '  \n']);
    assert.equal(card.children[1].tagName, 'PRE');
    assert.deepEqual(card.children[1].children.map(r => r.className), ['reader-line reader-add', 'reader-line reader-add', 'reader-line reader-add', 'reader-line reader-add', 'reader-line reader-del', 'reader-line reader-context', 'reader-line reader-context']);
    const [repo, file, stage, delivery, time] = meta(card);
    assert.equal(repo.tagName, 'A'); assert.equal(repo.textContent, 'org/repo #2');
    assert.equal(repo.href, 'https://github.com/org/repo/pull/2'); assert.equal(repo.attrs.rel, 'noopener noreferrer');
    assert.equal(file.textContent, 'main.go'); assert.equal(stage.textContent, 'Pushed PR'); assert.equal(delivery.textContent, 'Buffered snapshot');
    assert.equal(time.tagName, 'TIME'); assert.equal(time.attrs.datetime, '2026-11-03T12:00:00.000Z');
    assert(time.textContent.startsWith('observed '));
  };
  const checkSecond = card => {
    assert.equal(meta(card)[0].href, 'https://github.com/org/other/pull/7');
    assert.equal(meta(card)[1].textContent, 'README.md');
    assert.equal(rows(card)[0], '+ ' + 'a very long line '.repeat(40) + '\n');
  };
  win.hiveReaderAttach({addEventListener(n, fn){sse[n] = fn}});
  if (opts.reduced) {
    // Static and scrollable: everything shown at once, nothing scheduled or scrolled.
    assert.equal(feed.children.length, 2); assert.equal(timers.size, 0); assert.equal(scrolls.length, 0);
    checkFirst(feed.children[0]); checkSecond(feed.children[1]);
    lines = [{seq: 9, repo: 'org/repo', pr: 2, head: 'abc', path: 'b.go', line: 'next', at}]; seq = 9;
    sse['code-lines'](); await flush();
    assert.equal(feed.children.length, 3); assert.equal(timers.size, 0); assert.equal(scrolls.length, 0);
    return;
  }
  // The first paragraph is shown; the next waits for it to be read.
  assert.equal(feed.children.length, 1); checkFirst(feed.children[0]);
  assert.equal(timers.size, 1); assert.equal(scrolls.at(-1).behavior, 'smooth');
  assert(statusEl.textContent.includes('1 change waiting'));
  // Incoming work queues behind the paragraph being read.
  lines = [{seq: 9, repo: 'org/repo', pr: 2, head: 'abc', path: 'b.go', line: 'next', at}]; seq = 9;
  sse['code-lines'](); await flush();
  assert.equal(requests.at(-1), '/api/matrix/lines?since=8&format=regions');
  assert.equal(feed.children.length, 1); assert.equal(timers.size, 1);
  // P pauses and resumes; the button mirrors it.
  key('p'); assert.equal(timers.size, 0); assert.equal(button.textContent, 'Resume');
  assert.equal(button.attrs['aria-pressed'], 'true'); assert(statusEl.textContent.startsWith('paused'));
  key(' ', 'Space'); assert.equal(timers.size, 1); assert.equal(button.textContent, 'Pause');
  fire(); assert.equal(feed.children.length, 2); checkSecond(feed.children[1]);
  // Wheel, touch and navigation keys pause for inspection; the button resumes.
  byClass('reader-scroll').listeners.wheel(); assert.equal(timers.size, 0); assert.equal(button.textContent, 'Resume');
  button.listeners.click(); assert.equal(timers.size, 1);
  byClass('reader-scroll').listeners.touchstart(); assert.equal(timers.size, 0);
  button.listeners.click(); key('PageDown'); assert.equal(timers.size, 0);
  button.listeners.click(); assert.equal(timers.size, 1);
  // Quiet hold after consuming the queued paragraph; no automatic replay.
  fire(); assert.equal(feed.children.length, 3); assert.equal(rows(feed.children[2])[0], '+ next\n');
  fire(); assert.equal(timers.size, 0); assert(statusEl.textContent.includes('Caught up'));
  sse['code-lines'](); await flush(); assert.equal(timers.size, 0); assert.equal(feed.children.length, 3);
  // Replay requires a deliberate click and is labelled on every card.
  byClass('reader-pause reader-replay').listeners.click();
  assert.equal(feed.children.length, 1); assert.equal(meta(feed.children[0])[3].textContent, 'Replay · buffered snapshot');
  doc.hidden = true; docEvents.visibilitychange.forEach(fn => fn());
  assert.equal(timers.size, 0); assert.equal(feed.children.length, 0);
  doc.hidden = false; docEvents.visibilitychange.forEach(fn => fn()); await flush();
  assert.equal(feed.children.length, 0); assert(statusEl.textContent.includes('No buffered checkpoints'));
  assert(byClass('reader-freshness').textContent.includes('empty does not mean no work'));
  status = 403; sse['code-lines'](); await flush();
  assert.equal(feed.children.length, 0); assert(statusEl.textContent.includes('Sign in'));
}
(async () => {
  await scenario(''); await scenario('?view=reader'); await scenario('?kiosk=1'); await scenario('?kiosk=1&view=matrix');
  await scenario('?kiosk=1&view=reader');
  await scenario('?kiosk=1&view=reader', {reduced: true});
  const region = {repo: 'org/repo', pr: 2, path: 'main.go', at};
  await scenario('?kiosk=1&view=reader', {reduced: true, boundaries: true, pages: [
    {seq: 1, latest: 2, epoch: 'policy-1', regions: [{...region, seq: 1, lines: [{op: '+', line: 'one'}]}]},
    {seq: 2, latest: 2, epoch: 'policy-1', regions: [{...region, seq: 2, lines: [{op: '-', line: 'two'}]}]},
  ]});
  for (const status of [404, 401, 403]) await scenario('?kiosk=1&view=reader', {status});
})().catch(e => {console.error(e); process.exit(1)});
`
