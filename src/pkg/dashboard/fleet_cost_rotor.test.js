// The RUNNING tile's cost rotor (#15614): "$X this hour" → "$Y today" →
// "$Z this week" → "$W this month", ~4s each, pausing while hovered or
// hidden, instant under prefers-reduced-motion, never a second line. The
// rotor block is lifted straight out of index.html so this runs against the
// shipped source.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function pick(re, what) {
  const m = html.match(re);
  assert.ok(m, what + ' not found in index.html');
  return m[0];
}

const rotorSrc = pick(/\/\/ --- fleet cost rotor \(begin\) ---[\s\S]*?\/\/ --- fleet cost rotor \(end\) ---/, 'fleet cost rotor block');
const statSrc = pick(/function fleetRunStatHtml\(\) \{[\s\S]*?\n    \}\n/, 'fleetRunStatHtml');
const stripSrc = pick(/function fleetRunStripHtml\(\) \{[\s\S]*?\n    \}\n/, 'fleetRunStripHtml');

// Minimal DOM stand-in: one rotor whose phrases are plain objects with a
// classList, plus a document with fake timers and a visibility flag.
function makeEnv({ reducedMotion = false } = {}) {
  const timers = [];
  let now = 0;
  const phrases = [];
  const makePhrase = (key, cur) => {
    const cls = new Set(cur ? ['fcr-phrase', 'is-cur'] : ['fcr-phrase']);
    const attrs = cur ? {} : { 'aria-hidden': 'true' };
    return {
      key,
      classList: {
        toggle(c, on) { if (on) cls.add(c); else cls.delete(c); },
        contains: c => cls.has(c),
      },
      setAttribute(k, v) { attrs[k] = v; },
      removeAttribute(k) { delete attrs[k]; },
      attrs,
    };
  };
  const rotor = { querySelectorAll: sel => { assert.strictEqual(sel, '.fcr-phrase'); return phrases; } };
  const listeners = {};
  const document = {
    hidden: false,
    querySelectorAll: sel => { assert.strictEqual(sel, '[data-fleet-cost-rotor]'); return [rotor]; },
    addEventListener: (ev, fn) => { listeners[ev] = fn; },
  };
  const window = {
    _lastStatus: { fleetRun: { running: 3, hourUSD: 53.91, hourPriced: true, dayUSD: 120.5, weekUSD: 410, monthUSD: 1234.56, windowsPriced: true } },
    matchMedia: q => ({ matches: reducedMotion && q === '(prefers-reduced-motion: reduce)' }),
  };
  const setInterval = (fn, ms) => { const t = { fn, ms, next: now + ms }; timers.push(t); return t; };
  const advance = ms => {
    const until = now + ms;
    for (;;) {
      const due = timers.filter(t => t.next <= until).sort((a, b) => a.next - b.next)[0];
      if (!due) break;
      now = due.next; due.next += due.ms; due.fn();
    }
    now = until;
  };
  const fmtUSD = n => '$' + Number(n).toFixed(2);
  const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
  const dashboardSetInterval = (key, fn, ms) => { assert.strictEqual(key, 'fleetCostRotor'); return setInterval(fn, ms); };
  const api = new Function('window', 'document', 'dashboardSetInterval', 'fmtUSD', 'esc',
    rotorSrc + '\n' + statSrc + '\n' + stripSrc +
    '\nreturn { FLEET_COST_WINDOWS, FLEET_COST_DWELL_MS, _fleetCost, fleetCostPhrases, fleetCostRotorHtml, fleetCostTick, fleetRunStatHtml, fleetRunStripHtml, fleetCostTooltipLines };'
  )(window, document, dashboardSetInterval, fmtUSD, esc);
  api._fleetCost.clock = () => now;
  // Seed the fake phrases from the rendered markup so the DOM matches it.
  const rendered = api.fleetRunStatHtml();
  const keys = [...rendered.matchAll(/data-fleet-cost-window="(\w+)"/g)].map(m => m[1]);
  keys.forEach((k, i) => phrases.push(makePhrase(k, i === api._fleetCost.idx)));
  const current = () => phrases.filter(p => p.classList.contains('is-cur')).map(p => p.key);
  const outgoing = () => phrases.filter(p => p.classList.contains('is-out')).map(p => p.key);
  return { api, rendered, phrases, document, window, listeners, timers, advance, current, outgoing };
}

test('rotates hour → today → week → month → hour with a ~4s dwell', () => {
  const env = makeEnv();
  assert.strictEqual(env.api.FLEET_COST_DWELL_MS, 4000);
  assert.strictEqual(env.timers.length, 1, 'one interval timer, started on first render');
  assert.deepStrictEqual(env.current(), ['hour']);
  env.advance(3999);
  assert.deepStrictEqual(env.current(), ['hour'], 'no change before the dwell elapses');
  env.advance(1);
  assert.deepStrictEqual(env.current(), ['day']);
  assert.deepStrictEqual(env.outgoing(), ['hour'], 'outgoing phrase slides out');
  env.advance(4000);
  assert.deepStrictEqual(env.current(), ['week']);
  env.advance(4000);
  assert.deepStrictEqual(env.current(), ['month']);
  env.advance(4000);
  assert.deepStrictEqual(env.current(), ['hour'], 'wraps around');
  assert.deepStrictEqual(env.outgoing(), ['month']);
  // Exactly one phrase is exposed to assistive tech at any time.
  assert.strictEqual(env.phrases.filter(p => !p.attrs['aria-hidden']).length, 1);
});

test('second render after a status poll keeps the current window', () => {
  const env = makeEnv();
  env.advance(8000);
  assert.deepStrictEqual(env.current(), ['week']);
  const again = env.api.fleetRunStatHtml();
  assert.match(again, /class="fcr-phrase is-cur" data-fleet-cost-window="week"/);
  assert.strictEqual(env.timers.length, 1, 'repaint does not start a second timer');
  // The strip twin shares the index.
  assert.match(env.api.fleetRunStripHtml(), /class="fcr-phrase is-cur" data-fleet-cost-window="week"/);
});

test('pauses while hovered or hidden and resumes in step', () => {
  const env = makeEnv();
  const host = { closest: sel => (sel === '[data-fleet-cost-host]' ? host : null) };
  const elsewhere = { closest: () => null };
  env.listeners.pointerover({ target: host });
  env.advance(12000);
  assert.deepStrictEqual(env.current(), ['hour'], 'hover holds the phrase');
  env.listeners.pointerout({ relatedTarget: elsewhere });
  env.advance(4000);
  assert.deepStrictEqual(env.current(), ['day'], 'one step after leaving, not three');
  env.document.hidden = true;
  env.advance(8000);
  assert.deepStrictEqual(env.current(), ['day'], 'hidden document holds the phrase');
  env.document.hidden = false;
  env.advance(4000);
  assert.deepStrictEqual(env.current(), ['week']);
  // A brief hide: the interval registry re-fires the tick on
  // visibilitychange, which must not cut the current dwell short.
  env.document.hidden = true;
  env.advance(1000);
  env.document.hidden = false;
  env.api.fleetCostTick();
  assert.deepStrictEqual(env.current(), ['week'], 'early re-fire after a short hide is held');
  env.advance(3000);
  assert.deepStrictEqual(env.current(), ['month'], 'next step lands on the original cadence');
});

test('prefers-reduced-motion swaps instantly with no outgoing phrase', () => {
  const env = makeEnv({ reducedMotion: true });
  env.advance(4000);
  assert.deepStrictEqual(env.current(), ['day']);
  assert.deepStrictEqual(env.outgoing(), [], 'no slide-out class under reduced motion');
  assert.match(html, /@media \(prefers-reduced-motion: reduce\) \{\s*\.fleet-cost-rotor \.fcr-phrase \{ transition: none;/);
});

test('markup: fixed agent count, four phrases in one rotor, tooltip lists all four', () => {
  const env = makeEnv();
  const r = env.rendered;
  assert.match(r, /<span class="fleet-run-count">3 · <\/span><span class="fleet-cost-rotor" data-fleet-cost-rotor/);
  assert.strictEqual((r.match(/class="fcr-phrase/g) || []).length, 4);
  assert.strictEqual((r.match(/data-fleet-cost-rotor/g) || []).length, 1, 'all phrases share one rotor cell');
  assert.match(r, /\$53\.91<\/span> <span class="fcr-window">this hour</);
  assert.match(r, /\$120\.50<\/span> <span class="fcr-window">today</);
  assert.match(r, /\$410\.00<\/span> <span class="fcr-window">this week</);
  assert.match(r, /\$1234\.56<\/span> <span class="fcr-window">this month</);
  assert.strictEqual(env.api.fleetCostTooltipLines(env.window._lastStatus.fleetRun), '$53.91 this hour · $120.50 today · $410.00 this week · $1234.56 this month');
  assert.match(r, /title="RUNNING[^"]*\$53\.91 this hour · \$120\.50 today · \$410\.00 this week · \$1234\.56 this month/);
  // Unpriced windows read "$–" rather than a guess.
  const unpriced = env.api.fleetCostPhrases({ hourUSD: 1, hourPriced: true, windowsPriced: false }).map(p => p.text);
  assert.deepStrictEqual(unpriced, ['$1.00 this hour', '$– today', '$– this week', '$– this month']);
});

test('never a second line: nowrap + ellipsis on the tile, phrases share one grid area', () => {
  const css = html.match(/<style\b[^>]*>([\s\S]*?)<\/style>/)[1];
  assert.match(css, /\.fleet-run-stat \.val, \.fleet-run-stat \.gm-val \{ white-space: nowrap; overflow: hidden; text-overflow: ellipsis; min-width: 0; \}/);
  assert.match(css, /\.fleet-cost-rotor \{ display: inline-grid; grid-template-areas: 'phrase';/);
  assert.match(css, /\.fleet-cost-rotor \.fcr-phrase \{ grid-area: phrase; white-space: nowrap; overflow: hidden; text-overflow: ellipsis;/);
  // Amber/text tokens only in the rotor rules — no raw colours.
  const rotorCss = css.match(/\.fleet-run-stat[\s\S]*?prefers-reduced-motion: reduce\) \{[\s\S]*?\}\s*\}/)[0];
  assert.doesNotMatch(rotorCss, /#[0-9a-fA-F]{3,8}\b|rgba?\(|hsla?\(/);
  assert.match(rotorCss, /var\(--amber\)/);
  assert.match(rotorCss, /var\(--text\)/);
  assert.doesNotMatch(makeEnv().rendered, /<br/);
});
