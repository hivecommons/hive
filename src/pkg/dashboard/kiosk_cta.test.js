// Kiosk call-to-action badge (hivecommons/hive#16236): boot.js turns the
// badge on for `?kiosk=1` only, `&cta=off` opts out, and the stylesheet keeps
// #kiosk-cta hidden without body.kiosk-cta-on so the operator dashboard never
// shows it. Runs against the shipped index.html.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

test('hiveKioskCTAOn: kiosk=1 only, cta=off opts out', () => {
  const m = html.match(/window\.hiveKioskCTAOn=function\(href\)\{[\s\S]*?\n  \};\n/);
  assert.ok(m, 'hiveKioskCTAOn not found in index.html');
  const window = {};
  new Function('window', m[0])(window);
  const on = window.hiveKioskCTAOn;
  assert.strictEqual(on('https://hive.example/?kiosk=1#section-activity-monitor-section'), true);
  assert.strictEqual(on('https://hive.example/?kiosk=1&view=matrix&sound=off'), true);
  assert.strictEqual(on('https://hive.example/?kiosk=1&cta=off'), false);
  assert.strictEqual(on('https://hive.example/'), false);
  assert.strictEqual(on('https://hive.example/?display=1'), false);
  assert.strictEqual(on('https://hive.example/?popout=1'), false);
  assert.strictEqual(on('not a url'), false);
});

test('badge wiring: placeholder outside the root, hidden without the body class, lifted and dimmed in matrix', () => {
  assert.ok(html.includes("if(window.hiveKioskCTAOn(window.location.href)) document.body.classList.add('kiosk-cta-on');"));
  const root = html.indexOf('<!-- /#hive-dashboard-root -->');
  const ph = html.indexOf('<!-- hive-kiosk-cta -->');
  assert.ok(root > 0 && ph > root, 'placeholder must follow the dashboard root');
  assert.ok(html.includes('#kiosk-cta { display: none; }'));
  assert.ok(html.includes('body.kiosk-cta-on #kiosk-cta {'));
  assert.ok(html.includes('body.matrix-mode #kiosk-cta { position: fixed; right: 1rem; bottom: 1rem; z-index: 100001; opacity: .85; }'));
});
