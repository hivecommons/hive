const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const source = fs.readFileSync(path.join(__dirname, 'static-src/js/progressive.js'), 'utf8');
const html = fs.readFileSync(path.join(__dirname, 'static/index.html'), 'utf8');
const flush = async () => { for (let i = 0; i < 12; i++) await Promise.resolve(); };
const deferred = () => {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
};

function environment({ hidden = false, idle = true } = {}) {
  const turns = [], timers = [], errors = [], listeners = {}, intervals = new Map();
  let intervalID = 0;
  const window = { location: { hash: '' }, innerHeight: 900 };
  if (idle) window.requestIdleCallback = fn => { turns.push(fn); };
  const sections = new Map();
  const document = {
    hidden,
    getElementById: id => sections.get(id),
    addEventListener(type, fn) { (listeners[type] ||= []).push(fn); },
  };
  const statusGuard = html.match(/function dashboardStatusCanRender\(data\) \{[\s\S]*?\n    \}/);
  assert.ok(statusGuard);
  const intervalSource = html.slice(html.indexOf('    function dashboardSetInterval('), html.indexOf('    let historyData ='));
  assert.ok(intervalSource.includes('function dashboardRunInterval'));
  const api = new Function('window', 'document', 'setTimeout', 'console', 'setInterval', 'clearInterval',
    'let _statusSeqFloor = 0, _lastStatusSeq = 0, _statusInstance = "";\n' +
    'const DASHBOARD_INTERVALS = {};\n' + statusGuard[0] + '\n' + source + '\n' + intervalSource +
    '\nreturn {dashboardScheduleWork, dashboardRenderSection, dashboardProgressiveWake, dashboardSetInterval,' +
    'setStatus(floor, latest, instance) {_statusSeqFloor=floor; _lastStatusSeq=latest; _statusInstance=instance;}};'
  )(window, document, (fn, ms) => { timers.push({ fn, ms }); }, {
    error(...args) { errors.push(args); },
  }, (fn, ms) => { const id = ++intervalID; intervals.set(id, { fn, ms }); return id; },
  id => intervals.delete(id));
  return {
    api, window, document, sections, errors, turns, timers, intervals,
    start() { timers.shift().fn(); },
    async turn() { assert.ok(turns.length, 'an idle turn must be scheduled'); turns.shift()(); await flush(); },
    visibility(value) {
      document.hidden = value;
      for (const fn of listeners.visibilitychange || []) fn();
    },
  };
}

test('the generated dashboard carries the progressive scheduler', () => {
  assert.ok(html.includes(source));
  assert.ok(!html.includes("document.body.style.opacity='0'"));
  const afterPaint = html.slice(html.indexOf('function _afterPaint('), html.indexOf('const NAVBAR_CLOCK_TICK_MS'));
  assert.ok(afterPaint.includes('requestAnimationFrame'));
  assert.ok(!afterPaint.includes('_deferredLoadPromise'), 'navigation cannot await every background API');
});

test('background work yields, stays bounded through decoding, and coalesces repeated ticks', async () => {
  const e = environment();
  const gates = [deferred(), deferred(), deferred()];
  const started = [];
  const promises = gates.map((gate, index) =>
    e.api.dashboardScheduleWork('job-' + index, async () => {
      started.push(index);
      await gate.promise;
      return index;
    }));
  assert.equal(e.api.dashboardScheduleWork('job-0', () => assert.fail('duplicate flight')), promises[0]);
  assert.equal(e.turns.length, 0, 'critical status/config get a head start');
  e.start();
  await e.turn();
  await e.turn();
  assert.deepEqual(started, [0, 1]);
  assert.equal(e.turns.length, 0, 'no more than two unfinished jobs');
  gates[0].resolve();
  await flush();
  await e.turn();
  assert.deepEqual(started, [0, 1, 2]);
  gates[1].resolve();
  gates[2].resolve();
  assert.deepEqual(await Promise.all(promises), [0, 1, 2]);
  let refreshed = false;
  const again = e.api.dashboardScheduleWork('job-0', () => { refreshed = true; });
  await e.turn();
  await again;
  assert.equal(refreshed, true, 'settled jobs can refresh immediately after awaiting them');
});

test('the selected section wins before visible panels, then background panels', async () => {
  const e = environment();
  const order = [];
  e.window.location.hash = '#section-activity-monitor-section';
  e.sections.set('overview-section', {
    getClientRects: () => [{}],
    getBoundingClientRect: () => ({ top: 10, bottom: 300 }),
  });
  const a = e.api.dashboardScheduleWork('knowledge-stats', () => order.push('background'));
  const b = e.api.dashboardScheduleWork('overview-kpi-history', () => order.push('visible'));
  const c = e.api.dashboardScheduleWork('stage-tracker', () => order.push('selected'));
  e.start();
  while (e.turns.length) await e.turn();
  await Promise.all([a, b, c]);
  assert.deepEqual(order, ['selected', 'visible', 'background']);
});

test('priority is recomputed after navigation while other work is in flight', async () => {
  const e = environment();
  const gate = deferred(), order = [];
  e.api.dashboardScheduleWork('first', () => gate.promise);
  e.api.dashboardScheduleWork('second', () => gate.promise);
  e.api.dashboardScheduleWork('knowledge-stats', () => order.push('knowledge'));
  e.api.dashboardScheduleWork('stage-tracker', () => order.push('tracker'));
  e.start();
  await e.turn();
  await e.turn();
  e.window.location.hash = '#section-activity-monitor-section';
  gate.resolve();
  await flush();
  await e.turn();
  assert.equal(order[0], 'tracker');
  while (e.turns.length) await e.turn();
});

test('hidden tabs retain jobs without starting them and resume once', async () => {
  const e = environment({ hidden: true });
  let runs = 0;
  const pending = e.api.dashboardScheduleWork('cost', () => runs++);
  e.start();
  assert.equal(e.turns.length, 0);
  e.visibility(false);
  e.visibility(true);
  await e.turn();
  assert.equal(runs, 0, 'a tab can become hidden after scheduling');
  e.visibility(false);
  await e.turn();
  await pending;
  assert.equal(runs, 1);
});

test('synchronous failures reject, release their slot, and do not strand other panels', async () => {
  const e = environment();
  const rejected = assert.rejects(e.api.dashboardScheduleWork('bad', () => {
    throw new Error('broken panel');
  }), /broken panel/);
  let ran = false;
  const good = e.api.dashboardScheduleWork('good', () => { ran = true; });
  e.start();
  while (e.turns.length) await e.turn();
  await rejected;
  await good;
  assert.equal(ran, true);
});

test('visibility resume and overlapping poll ticks use the same bounded flights', async () => {
  const e = environment(), gate = deferred(), started = [];
  for (const key of ['cost', 'git-version', 'stage-tracker']) {
    e.api.dashboardSetInterval(key, async () => {
      started.push(key);
      await gate.promise;
    }, 1000);
  }
  let watchdog = 0;
  e.api.dashboardSetInterval('sse-watchdog', () => watchdog++, 1000);
  e.visibility(true);
  assert.equal(e.intervals.size, 0);
  e.visibility(false);
  assert.equal(watchdog, 1, 'liveness must not wait behind background panels');
  e.start();
  await e.turn();
  await e.turn();
  assert.equal(started.length, 2);
  for (const timer of e.intervals.values()) timer.fn();
  await flush();
  assert.equal(started.length, 2, 'new ticks cannot overlap existing panel requests');
  gate.resolve();
  await flush();
  while (e.turns.length) await e.turn();
  assert.equal(started.length, 3);
  assert.equal(new Set(started).size, 3);
});

test('rendering coalesces snapshots and paints one section per idle turn', async () => {
  const e = environment(), rendered = [];
  e.api.dashboardRenderSection('tokens', ['token-panel'], () => rendered.push('stale'));
  e.api.dashboardRenderSection('tokens', ['token-panel'], () => rendered.push('latest'));
  e.api.dashboardRenderSection('tracker', ['activity-monitor-section'], () => rendered.push('tracker'));
  e.window.location.hash = '#section-activity-monitor-section';
  await e.turn();
  assert.deepEqual(rendered, ['tracker']);
  await e.turn();
  assert.deepEqual(rendered, ['tracker', 'latest']);
});

test('hidden render snapshots stay bounded and resume with the newest state', async () => {
  const e = environment({ hidden: true });
  let rendered;
  for (let i = 0; i < 100; i++) {
    e.api.dashboardRenderSection('agents', ['agents-section'], () => { rendered = i; });
  }
  assert.equal(e.turns.length, 0);
  e.visibility(false);
  await e.turn();
  assert.equal(rendered, 99);
  assert.equal(e.turns.length, 0);
});

test('a broken renderer is reported without blocking another section', async () => {
  const e = environment();
  let ran = false;
  e.api.dashboardRenderSection('bad', [], () => { throw new Error('bad render'); });
  e.api.dashboardRenderSection('good', [], () => { ran = true; });
  await e.turn();
  assert.equal(e.errors.length, 1);
  await e.turn();
  assert.equal(ran, true);
});

test('queued snapshots recheck mutation floors, ordering, and server instance at execution', async () => {
  const e = environment(), rendered = [];
  e.api.dashboardRenderSection('old', [], () => rendered.push('old'), { statusSeq: 4, statusInstance: 'a' });
  e.api.setStatus(5, 5, 'a');
  await e.turn();
  assert.deepEqual(rendered, [], 'an optimistic mutation cannot be reverted by queued old data');
  e.api.dashboardRenderSection('old-instance', [], () => rendered.push('old-instance'), { statusSeq: 100, statusInstance: 'a' });
  e.api.setStatus(0, 1, 'b');
  await e.turn();
  assert.deepEqual(rendered, [], 'a restarted server invalidates queued snapshots from its predecessor');
  e.api.dashboardRenderSection('older', [], () => rendered.push('older'), { statusSeq: 1, statusInstance: 'b' });
  e.api.setStatus(0, 2, 'b');
  await e.turn();
  assert.deepEqual(rendered, []);
  e.api.dashboardRenderSection('fresh', [], () => rendered.push('fresh'), { statusSeq: 2, statusInstance: 'b' });
  await e.turn();
  assert.deepEqual(rendered, ['fresh']);
  e.api.dashboardRenderSection('legacy', [], () => rendered.push('legacy'), {});
  await e.turn();
  assert.deepEqual(rendered, ['fresh', 'legacy']);
});

test('a queued worker redraw survives newer status snapshots with unchanged workers', async () => {
  const e = environment();
  const redraw = html.match(/dashboardRenderSection\('tracker-workers',[^\n]+/);
  assert.ok(redraw);
  let latestWorkers = ['working'], rendered;
  const snapshot = { statusSeq: 1, statusInstance: 'a' };
  new Function('dashboardRenderSection', 'renderStageTracker', 'data', redraw[0])(
    e.api.dashboardRenderSection, () => { rendered = latestWorkers; }, snapshot,
  );
  e.api.setStatus(0, 2, 'a');
  latestWorkers = ['working', 'newest'];
  await e.turn();
  assert.deepEqual(rendered, ['working', 'newest']);
});

test('browsers without requestIdleCallback still yield with bounded work', async () => {
  const e = environment({ idle: false });
  let ran = false;
  const result = e.api.dashboardScheduleWork('cost', () => { ran = true; });
  e.start();
  assert.equal(ran, false);
  assert.equal(e.timers.length, 1);
  assert.ok(e.timers[0].ms > 0);
  e.timers.shift().fn();
  await result;
  assert.equal(ran, true);
});

test('backend discovery retains its slot until each model request is decoded', async () => {
  const discovery = html.match(/async function refreshBackendDiscovery\(force\) \{[\s\S]*?\n    \}/);
  assert.ok(discovery);
  const calls = [], gate = deferred();
  const run = new Function('INFERENCE_BACKENDS', 'fetchInferenceModels', 'fetchConfiguredGateways',
    discovery[0] + '\nreturn refreshBackendDiscovery(true);'
  )(['one', 'two'], async (backend, force) => {
    assert.equal(force, true);
    calls.push(backend);
    if (backend === 'one') await gate.promise;
  }, async () => { calls.push('gateways'); });
  await flush();
  assert.deepEqual(calls, ['one']);
  gate.resolve();
  await run;
  assert.deepEqual(calls, ['one', 'two', 'gateways']);
});
