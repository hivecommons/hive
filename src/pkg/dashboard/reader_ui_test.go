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
	for _, want := range []string{"visibilitychange", "prefers-reduced-motion: reduce", "'/api/matrix/lines?since='", "source.addEventListener('code-lines'", "'wheel'", "'touchstart'"} {
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
	for _, want := range []string{"white-space: pre-wrap; overflow-wrap: anywhere;", "body.reader-mode .reader-scroll { flex: 1 1 auto; min-height: 0; overflow-y: auto;", "font: 20px/1.5 monospace;"} {
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
  {seq: 1, repo: 'org/repo', pr: 2, head: 'abc', path: 'main.go', line: '<script>alert(1)</script>', at},
  {seq: 2, repo: 'org/repo', pr: 2, head: 'abc', path: 'main.go', line: '', at},
  {seq: 3, repo: 'org/repo', pr: 2, head: 'abc', path: 'main.go', line: '\tx := 1', at},
  {seq: 4, repo: 'org/repo', pr: 2, head: 'abc', path: 'main.go', line: '\tx := 1', at},
  {seq: 5, repo: 'org/repo', pr: 2, head: 'abc', path: 'main.go', line: '    old()', at},
  {seq: 6, repo: 'org/other', pr: 7, head: 'def', path: 'README.md', line: 'a very long line '.repeat(40), at},
];
async function scenario(query, opts = {}) {
  const docEvents = {}, timers = new Map(), requests = [], scrolls = [], sse = {};
  // page caps each response like the server does; seq always reports the newest line held.
  let id = 0, status = opts.status || 200, seq = 6, lines = first, page = opts.page || Infinity;
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
    fetch(url){requests.push(url); const body = {seq, source: 'pushed-lines', lines: lines.slice(0, page)}; lines = lines.slice(page);
      return Promise.resolve({status, ok: status === 200, json(){return Promise.resolve(body)}})},
    Date, Math});
  vm.runInContext(reader, c); c.hiveReaderStart = win.hiveReaderStart;
  vm.runInContext(boot, c);
  for (const fn of docEvents.DOMContentLoaded || []) fn();
  const flush = async () => {for (let i = 0; i < 12; i++) await Promise.resolve()};
  await flush();
  if (!win.HIVE_READER) {assert.equal(requests.length, 0); assert(!doc.getElementById('hive-reader')); return;}
  const byClass = n => elements.find(e => e.className === n);
  const feed = byClass('reader-feed'), statusEl = byClass('reader-status'), button = byClass('reader-pause');
  const fire = () => {assert.equal(timers.size, 1); const [k, fn] = [...timers][0]; timers.delete(k); fn()};
  const key = (key, code) => docEvents.keydown[0]({key, code, target: doc.body, preventDefault(){}});
  assert.equal(requests[0], '/api/matrix/lines?since=0');
  if (status !== 200) {
    assert.equal(feed.children.length, 0); assert.equal(timers.size, 0);
    assert(statusEl.textContent.includes(status === 404 ? 'not enabled' : 'Sign in'));
    return;
  }
  const meta = card => card.children[0].children, rows = card => card.children[1].children.map(r => r.textContent);
  const checkFirst = card => {
    // One contiguous region: blank, indented and repeated lines kept as text.
    assert.deepEqual(rows(card), ['+ <script>alert(1)</script>\n', '+ \n', '+ \tx := 1\n', '+ \tx := 1\n', '+     old()\n']);
    assert.equal(card.children[1].tagName, 'PRE');
    assert.deepEqual(card.children[1].children.map(r => r.className), Array(5).fill('reader-line reader-add'));
    const [repo, file, stage, time] = meta(card);
    assert.equal(repo.tagName, 'A'); assert.equal(repo.textContent, 'org/repo #2');
    assert.equal(repo.href, 'https://github.com/org/repo/pull/2'); assert.equal(repo.attrs.rel, 'noopener noreferrer');
    assert.equal(file.textContent, 'main.go'); assert.equal(stage.textContent, 'pushed');
    assert.equal(time.tagName, 'TIME'); assert.equal(time.attrs.datetime, '2026-11-03T12:00:00.000Z');
    assert(time.textContent.startsWith('observed '));
  };
  const checkSecond = card => {
    assert.equal(meta(card)[0].href, 'https://github.com/org/other/pull/7');
    assert.equal(meta(card)[1].textContent, 'README.md');
    assert.equal(rows(card)[0], '+ ' + 'a very long line '.repeat(40) + '\n');
  };
  win.hiveReaderAttach({addEventListener(n, fn){sse[n] = fn}});
  if (opts.page) {
    // A capped page resumes from the last returned line, not the newest seq,
    // so no line is skipped and the region stays one contiguous card.
    await flush();
    assert.deepEqual(requests, ['/api/matrix/lines?since=0', '/api/matrix/lines?since=2', '/api/matrix/lines?since=4']);
    assert.equal(feed.children.length, 2); checkFirst(feed.children[0]); checkSecond(feed.children[1]);
    return;
  }
  if (opts.reduced) {
    // Static and scrollable: everything shown at once, nothing scheduled or scrolled.
    assert.equal(feed.children.length, 2); assert.equal(timers.size, 0); assert.equal(scrolls.length, 0);
    checkFirst(feed.children[0]); checkSecond(feed.children[1]);
    lines = [{seq: 7, repo: 'org/repo', pr: 2, head: 'abc', path: 'b.go', line: 'next', at}]; seq = 7;
    sse['code-lines'](); await flush();
    assert.equal(feed.children.length, 3); assert.equal(timers.size, 0); assert.equal(scrolls.length, 0);
    return;
  }
  // The first paragraph is shown; the next waits for it to be read.
  assert.equal(feed.children.length, 1); checkFirst(feed.children[0]);
  assert.equal(timers.size, 1); assert.equal(scrolls.at(-1).behavior, 'smooth');
  assert(statusEl.textContent.includes('1 change waiting'));
  // Incoming work queues behind the paragraph being read.
  lines = [{seq: 7, repo: 'org/repo', pr: 2, head: 'abc', path: 'b.go', line: 'next', at}]; seq = 7;
  sse['code-lines'](); await flush();
  assert.equal(requests.at(-1), '/api/matrix/lines?since=6');
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
  // A hidden tab stops all motion.
  doc.hidden = true; docEvents.visibilitychange.forEach(fn => fn()); assert.equal(timers.size, 0);
  doc.hidden = false; docEvents.visibilitychange.forEach(fn => fn()); await flush();
  fire(); assert.equal(feed.children.length, 3); assert.equal(rows(feed.children[2])[0], '+ next\n');
  // Caught up: a quiet hold with no replay or filler.
  fire(); assert.equal(timers.size, 0); assert.equal(feed.children.length, 3);
  assert(statusEl.textContent.includes('Caught up'));
  sse['code-lines'](); await flush(); assert.equal(timers.size, 0); assert.equal(feed.children.length, 3);
  status = 403; sse['code-lines'](); await flush();
  assert.equal(feed.children.length, 0); assert(statusEl.textContent.includes('Sign in'));
}
(async () => {
  await scenario(''); await scenario('?view=reader'); await scenario('?kiosk=1'); await scenario('?kiosk=1&view=matrix');
  await scenario('?kiosk=1&view=reader');
  await scenario('?kiosk=1&view=reader', {reduced: true});
  await scenario('?kiosk=1&view=reader', {reduced: true, page: 2});
  for (const status of [404, 401, 403]) await scenario('?kiosk=1&view=reader', {status});
})().catch(e => {console.error(e); process.exit(1)});
`
