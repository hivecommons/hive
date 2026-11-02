package dashboard

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMatrixUIWiring(t *testing.T) {
	html := indexHTML(t)
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile("static-src/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	js := read("js/matrix.js")
	for _, forbidden := range []string{"innerHTML", "insertAdjacentHTML", "document.write", "new EventSource"} {
		if strings.Contains(js, forbidden) {
			t.Errorf("matrix.js contains forbidden sink/stream %q", forbidden)
		}
	}
	for _, want := range []string{"visibilitychange", "prefers-reduced-motion: reduce", "cancelAnimationFrame", "text.textContent = line.line", "source.addEventListener('code-lines'", "hive.matrix.", "mx.", "'ghost'", "'terminal'", "effect === 'ghost'", "effect === 'terminal'"} {
		if !strings.Contains(js, want) {
			t.Errorf("matrix.js missing %q", want)
		}
	}
	for _, want := range []string{"window.HIVE_MATRIX = kiosk && view === 'matrix';", "if(window.HIVE_MATRIX){", "hiveMatrixStart();", "if (window.HIVE_MATRIX) window.hiveMatrixAttach(source);"} {
		if !strings.Contains(html, want) {
			t.Errorf("generated page missing %q", want)
		}
	}
	css := read("css/matrix.css")
	for _, rule := range strings.Split(css, "}") {
		selector, _, ok := strings.Cut(strings.TrimSpace(rule), "{")
		if !ok {
			continue
		}
		for _, part := range strings.Split(selector, ",") {
			if !strings.HasPrefix(strings.TrimSpace(part), "body.matrix-mode ") {
				t.Errorf("unscoped Matrix selector %q", part)
			}
		}
	}
	manifest := read("manifest.txt")
	for _, file := range []string{"js/matrix.js", "css/matrix.css"} {
		if !strings.Contains(manifest, file+"\n") {
			t.Errorf("manifest missing %s", file)
		}
		if !strings.Contains(html, read(file)) {
			t.Errorf("generated page is missing current %s", file)
		}
	}
}

// Exercise browser lifecycle and pull behavior with deterministic DOM/clock
// doubles. No browser dependencies, network or real animation timers required.
func TestMatrixUIBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	cmd := exec.Command(node, "-e", matrixBrowserTest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Matrix browser behavior: %v\n%s", err, out)
	}
}

const matrixBrowserTest = `
const assert = require('assert');
const fs = require('fs');
const vm = require('vm');
const matrix = fs.readFileSync('static-src/js/matrix.js', 'utf8');
const boot = fs.readFileSync('static-src/js/boot.js', 'utf8').split('})();')[0] + '})();';
async function scenario(query, reduced = false, status = 200) {
  const events = {}, docEvents = {}, raf = new Map(), timers = new Map(), requests = [], draws = [];
  let id = 0, responseStatus = status, responseSeq = 1;
  const context = {clearRect(){}, fillRect(){}, fillText(...a){draws.push(a)}, drawImage(...a){draws.push(a)}};
  const elements = [];
  function el(tag) { const e = {tagName: tag.toUpperCase(), children: [], style: {setProperty(){}},
    classList: {add(){}}, setAttribute(){}, append(...a){this.children.push(...a)},
    appendChild(a){this.children.push(a)}, replaceChildren(...a){this.children = a}, getContext(){return context}};
    elements.push(e); return e; }
  const body = el('body');
  const doc = {body, hidden: false, createElement: el,
    getElementById(id){return elements.find(e => e.id === id)},
    addEventListener(n, fn){(docEvents[n] ||= []).push(fn)}};
  const motion = {matches: reduced, addEventListener(n, fn){this.change = fn}};
  const win = {location: {href: 'https://hive.test/' + query}, innerWidth: 800, innerHeight: 600,
    matchMedia(){return motion}, addEventListener(){}};
  const c = vm.createContext({window: win, document: doc, URL, location: {hash: '#activity'},
    history: {replaceState(){}}, localStorage: {getItem(){return null}, setItem(){}},
    requestAnimationFrame(fn){raf.set(++id, fn); return id}, cancelAnimationFrame(i){raf.delete(i)},
    setTimeout(fn){timers.set(++id, fn); return id}, clearTimeout(i){timers.delete(i)}, setInterval(){}, dashboardSetInterval(){},
    fetch(url){requests.push(url); return Promise.resolve({status: responseStatus, ok: responseStatus === 200,
      json(){return Promise.resolve({seq: responseSeq, lines: [{line: '<script>alert(1)</script>', repo: 'org/repo', pr: 2, path: 'main.go', at: new Date().toISOString()}]})}})},
    Date, Math});
  vm.runInContext(matrix, c); c.hiveMatrixStart = win.hiveMatrixStart;
  vm.runInContext(boot, c);
  for (const fn of docEvents.DOMContentLoaded || []) fn();
  const flush = async () => {for(let i=0;i<12;i++) await Promise.resolve()};
  await flush();
  if (!win.HIVE_MATRIX) {assert.equal(requests.length, 0); assert(!doc.getElementById('hive-matrix')); return;}
  const byClass = n => elements.find(e => e.className === n);
  assert.equal(requests[0], '/api/matrix/lines?since=0');
  assert(byClass('matrix-labels').children[0].textContent.includes('public allowlisted repos'));
  if (status !== 200) {
    assert.equal(raf.size, 0); assert(byClass('matrix-canvas').hidden);
    assert(byClass('matrix-notice').textContent.includes(status === 404 ? 'not enabled' : 'Sign in'));
    return;
  }
  if (reduced) {
    assert.equal(raf.size, 0);
    assert.equal(byClass('matrix-list').children[0].children[1].textContent, '<script>alert(1)</script>');
    assert(timers.size > 0);
  } else {
    assert.equal(raf.size, 1);
    for (const time of [1, 100]) {const fn = [...raf.values()][0]; raf.clear(); fn(time)}
    assert(draws.length > 0);
    if (/cascade|ghost|terminal/.test(query)) assert(draws.some(a => a[0] === 'org/repo#2 · main.go'));
    if (query.includes('ghost')) assert(draws.some(a => a[0] === 'main.go'));
    if (query.includes('terminal')) assert(draws.some(a => a[0] === '+ <script>alert(1)</script>'));
  }
  doc.hidden = true; docEvents.visibilitychange.forEach(fn => fn());
  assert.equal(raf.size, 0); assert.equal(timers.size, 0);
  assert.equal(byClass('matrix-labels').children[2].textContent, 'paused');
  doc.hidden = false; docEvents.visibilitychange.forEach(fn => fn()); await flush();
  docEvents.keydown[0]({key: 'p', target: el('body'), preventDefault(){}});
  assert.equal(raf.size, 0); assert.equal(timers.size, 0);
  win.hiveMatrixAttach({addEventListener(n, fn){events[n] = fn}});
  responseSeq = 2; events['code-lines'](); await flush();
  assert.equal(requests.at(-1), '/api/matrix/lines?since=1');
  responseStatus = 403; events['code-lines'](); await flush();
  assert(byClass('matrix-canvas').hidden); assert.equal(byClass('matrix-list').children.length, 0);
}
(async () => {
  await scenario(''); await scenario('?view=matrix'); await scenario('?kiosk=1');
  await scenario('?kiosk=1&view=matrix');
  await scenario('?kiosk=1&view=matrix&mx.effect=cascade');
  await scenario('?kiosk=1&view=matrix&mx.effect=ghost');
  await scenario('?kiosk=1&view=matrix&mx.effect=terminal');
  await scenario('?kiosk=1&view=matrix', true);
  for (const status of [404, 401, 403]) await scenario('?kiosk=1&view=matrix', false, status);
})().catch(e => {console.error(e); process.exit(1)});
`
