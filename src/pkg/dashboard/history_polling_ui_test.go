package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDashboardHistoryConditionalPolling(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: history polling behavior was not executed")
	}
	html := indexHTML(t)
	var script strings.Builder
	script.WriteString(`const assert = require('node:assert/strict');
const _historyValidators = new Map();
let _costHistory = [], _costHistoryLoading = false, _costHistoryInstance = null;
let _statusInstance = 'first';
const COST_HISTORY_POINTS = 8640;
`)
	for _, name := range []string{"capDashboardArray", "fetchDashboardHistory", "fetchCostHistory"} {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(historyPollingAssertions)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("history polling failed: %v\n%s", err, out)
	}
}

const historyPollingAssertions = `
(async () => {
  let queue = [], calls = [], parses = 0;
  global.fetch = async (url, options) => {
    calls.push({url, options});
    assert.ok(queue.length, 'unexpected request');
    const next = queue.shift();
    if (next.error) throw new Error('network');
    if (next.onFetch) next.onFetch();
    return {
      status: next.status || 200,
      ok: !next.status || next.status === 200,
      headers: { get: name => name === 'ETag' ? next.etag : null },
      json: async () => { parses++; assert.notEqual(next.status, 304); return next.body; },
    };
  };
  queue.push({body: [1], etag: 'W/"facts"'});
  assert.deepEqual(await fetchDashboardHistory('/api/knowledge/fact-history'), [1]);
  queue.push({status: 304});
  assert.equal(await fetchDashboardHistory('/api/knowledge/fact-history'), null);
  assert.equal(parses, 1, '304 must not be parsed');
  assert.equal(calls.at(-1).options.headers['If-None-Match'], 'W/"facts"');
  queue.push({status: 500, etag: 'bad'});
  await assert.rejects(fetchDashboardHistory('/api/knowledge/fact-history'));
  assert.equal(_historyValidators.get('/api/knowledge/fact-history').etag, 'W/"facts"');

  // Every counter step, including a reset, survives the bootstrap and delta.
  queue.push({body: [{t: 1, usd: 100}, {t: 2, usd: 5}], etag: '"initial"'});
  await fetchCostHistory();
  assert.equal(calls.at(-1).url, '/api/cost/history?max_points=8640');
  queue.push({body: [{t: 3, usd: 10}], etag: '"delta"'});
  await fetchCostHistory();
  assert.equal(calls.at(-1).url, '/api/cost/history?max_points=8640&since=2');
  assert.equal(calls.at(-1).options.headers['If-None-Match'], undefined, 'changed cursor must not reuse a validator');
  assert.deepEqual(_costHistory.map(e => e.usd), [100, 5, 10]);
  queue.push({body: [], etag: '"empty"'});
  await fetchCostHistory();
  queue.push({status: 304});
  await fetchCostHistory();
  assert.equal(calls.at(-1).options.headers['If-None-Match'], '"empty"');
  assert.equal(_costHistory.length, 3);
  assert.equal(_historyValidators.size, 2, 'cursor changes must not grow the cache');
  queue.push({error: true});
  await fetchCostHistory();
  assert.equal(_costHistory.length, 3, 'failed reads must retain good data');
  assert.equal(_costHistoryLoading, false);

  // A restarted server can restore a shorter/different ring. Bootstrap it
  // rather than continuing an obsolete cursor (including an in-flight race).
  _statusInstance = 'second';
  queue.push({body: [{t: 2, usd: 9}], etag: '"restored"'});
  await fetchCostHistory();
  assert.equal(calls.at(-1).url, '/api/cost/history?max_points=8640');
  assert.deepEqual(calls.at(-1).options.headers, {});
  assert.deepEqual(_costHistory, [{t: 2, usd: 9}]);
  queue.push({body: [{t: 3, usd: 11}], onFetch: () => { _statusInstance = 'third'; }});
  await fetchCostHistory();
  assert.deepEqual(_costHistory, [{t: 2, usd: 9}], 'old process delta raced restart');
  queue.push({body: [], etag: '"reset"'});
  await fetchCostHistory();
  assert.equal(calls.at(-1).url, '/api/cost/history?max_points=8640');
  assert.deepEqual(_costHistory, []);

  // Catch-up is bounded and overlapping polls cannot duplicate points.
  _costHistory = Array.from({length: 8640}, (_, i) => ({t: i + 1, usd: i}));
  queue.push({body: [{t: 8641, usd: 8641}]});
  const before = calls.length;
  await Promise.all([fetchCostHistory(), fetchCostHistory()]);
  assert.equal(calls.length, before + 1);
  assert.equal(_costHistory.length, 8640);
  assert.equal(_costHistory[0].t, 2);
  assert.equal(_costHistory.at(-1).t, 8641);
  assert.equal(queue.length, 0);
})().catch(error => { console.error(error); process.exit(1); });
`
