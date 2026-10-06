package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestContributeVersionFooter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: footer behavior was not executed")
	}
	body := renderContributePage(t)
	if !strings.Contains(body, `role="status">Hive version unavailable</span>`) {
		t.Fatal("footer must have a settled fallback even before JavaScript runs")
	}
	script := jsFunc(t, body, "ccLoadVersion") + `
const assert = require('node:assert/strict');
const el = {textContent: ''};
const document = {getElementById: () => el};
let timeout, cleared, signal, response;
function setTimeout(fn, ms) { assert.equal(ms, 10000); timeout = fn; return 1; }
function clearTimeout(id) { assert.equal(id, 1); cleared = true; }
function fetch(url, options) {
  assert.equal(url, '/api/contribute/status');
  signal = options.signal;
  return response();
}
const valid = {hub: 'online', served_sha: 'abc1234'};
const ok = data => ({ok: true, json: async () => data});
async function run(fn, expected) {
  response = fn; cleared = false;
  const pending = ccLoadVersion();
  assert.equal(el.textContent, 'Loading Hive version...');
  await pending;
  assert.equal(el.textContent, expected);
  assert.ok(cleared, 'settlement clears the deadline');
}
(async () => {
  await run(() => ok(valid), 'Hive build abc1234');
  await run(() => ok({...valid, served_sha: ' abc1234 '}), 'Hive build abc1234');
  const unavailable = 'Hive version unavailable';
  for (const status of [401, 403, 500]) {
    await run(() => ({ok: false, status, json() {throw Error('must not parse HTTP errors');}}), unavailable);
  }
  await run(() => Promise.reject(Error('offline')), unavailable);
  await run(() => {throw Error('synchronous fetch error');}, unavailable);
  await run(() => ({ok: true, json: async () => {throw SyntaxError('HTML login page');}}), unavailable);
  for (const data of [null, {}, {...valid, served_sha: ''}, {...valid, served_sha: '  '}, {...valid, served_sha: 42}, {...valid, served_sha: 'unknown'}]) {
    await run(() => ok(data), unavailable);
  }
  await run(() => ok({...valid, served_sha: '<img src=x>'}), 'Hive build <img src=x>');
  // Both stalled headers and stalled JSON must settle; late success cannot undo timeout.
  for (const stalledBody of [false, true]) {
    let resolve;
    const stalled = new Promise(r => {resolve = r;});
    response = () => stalledBody ? {ok: true, json: () => stalled} : stalled;
    cleared = false;
    const pending = ccLoadVersion();
    await Promise.resolve();
    await Promise.resolve();
    timeout();
    assert.equal(el.textContent, unavailable);
    assert.ok(signal.aborted);
    assert.ok(cleared);
    resolve(stalledBody ? valid : ok(valid));
    await pending;
    assert.equal(el.textContent, unavailable);
  }
})().catch(err => {console.error(err); process.exitCode = 1;});
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("contribute version footer: %v\n%s", err, out)
	}
}
