#!/usr/bin/env node
// Mobile dashboard screenshot + overflow-report harness (#11808, part of #11807).
//
// Usage:
//   node src/scripts/dashboard-mobile-shots.mjs [--out <dir>] [--viewport <WxH@DPR|WxH>] [--theme light|dark]
//
// Loads src/pkg/dashboard/static/index.html in headless Chrome (CHROME_BIN or a
// system chrome/chromium, same launch as dashboard-tooltip-smoke.mjs) at
// 390x844@3, 375x667@2, 430x932@3 and 768x1024@2 plus the landscape phone
// viewports 844x390@3, 932x430@3, 667x375@2 and 740x360@3 (#11860), in light and
// dark themes, and screenshots: all sections collapsed, each section expanded
// alone, Settings, Knowledge Sources and the terminal dialog. Phone viewports send
// an iOS user agent (Android for 740x360) and touch emulation, so the
// pointer:coarse landscape tier in index.html applies.
//
// Output (default dir: dashboard-mobile-shots-out/):
//   <viewport>-<theme>-<state>.png   one screenshot per shot
//   overflow-report.json             array of findings:
//     { viewport, theme, state, selector, bbox:{x,y,width,height}, kind, detail }
//   kind: scroll-overflow | past-viewport | clipped-text | ellipsis-free-space | clipped-by-strip
//   clipped-by-strip: a topbar chip whose box pokes outside an overflow-clipping
//   ancestor strip (how the ≡ drawer toggle lost its left edge on phones, #15476).
//
// General overflow findings remain reporting-only. Preparation/activity acceptance
// violations exit 1; the existing workflow remains non-required/reporting-only.
// Chrome missing is a local SKIP but a canonical CI failure.
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { preparationRailLayoutFixture } from './fixtures/dashboard-preparation-rail.mjs';
import { activityDensityFixture } from './fixtures/dashboard-activity-density.mjs';

const CHROME_CANDIDATES = [
  process.env.CHROME_BIN,
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/Applications/Chromium.app/Contents/MacOS/Chromium',
  'google-chrome',
  'chromium',
].filter(Boolean);
const DEBUG_PORT = 9235;
const STARTUP_RETRIES = 80;
const STARTUP_DELAY_MS = 250;
const PAGE_SETTLE_MS = 1500;
const STATE_SETTLE_MS = 250;
const PROFILE_DIR = path.join(process.cwd(), '.chrome-dashboard-mobile-shots');
const IOS_UA = 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1';
const IPAD_UA = 'Mozilla/5.0 (iPad; CPU OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1';
const ANDROID_UA = 'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Mobile Safari/537.36';
const DESKTOP_UA = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36';
const ACTIVITY_DESKTOP_WIDTHS = [2400, 1440, 1110, 1024];

const VIEWPORTS = [
  { name: '390x844@3', width: 390, height: 844, dpr: 3, mobile: true, ua: IOS_UA },
  { name: '375x667@2', width: 375, height: 667, dpr: 2, mobile: true, ua: IOS_UA },
  { name: '430x932@3', width: 430, height: 932, dpr: 3, mobile: true, ua: IOS_UA },
  { name: '768x1024@2', width: 768, height: 1024, dpr: 2, mobile: true, ua: IPAD_UA },
  // Landscape phones: wider than the 768px phone breakpoint, under 500px tall.
  { name: '844x390@3', width: 844, height: 390, dpr: 3, mobile: true, ua: IOS_UA },
  { name: '932x430@3', width: 932, height: 430, dpr: 3, mobile: true, ua: IOS_UA },
  { name: '667x375@2', width: 667, height: 375, dpr: 2, mobile: true, ua: IOS_UA },
  { name: '740x360@3', width: 740, height: 360, dpr: 3, mobile: true, ua: ANDROID_UA },
  ...ACTIVITY_DESKTOP_WIDTHS.map(width => ({ name: width + 'x1200@1', width, height: 1200,
    dpr: 1, mobile: false, ua: DESKTOP_UA, activityOnly: true })),
];
const THEMES = ['light', 'dark'];

function parseArgs(argv) {
  const opts = { out: path.join(process.cwd(), 'dashboard-mobile-shots-out'), viewport: '', theme: '' };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    const [key, inline] = arg.split('=', 2);
    const value = () => (inline !== undefined ? inline : argv[++i]);
    if (key === '--out') opts.out = path.resolve(value());
    else if (key === '--viewport') opts.viewport = String(value() || '');
    else if (key === '--theme') opts.theme = String(value() || '');
    else throw new Error(`unknown argument: ${arg}`);
  }
  if (opts.theme && !THEMES.includes(opts.theme)) throw new Error(`--theme must be one of ${THEMES.join(', ')}`);
  return opts;
}

function selectViewports(filter) {
  if (!filter) return VIEWPORTS;
  const want = filter.replace(/@.*$/, '');
  const picked = VIEWPORTS.filter((v) => v.name === filter || v.name.replace(/@.*$/, '') === want);
  if (!picked.length) throw new Error(`--viewport ${filter} matches none of ${VIEWPORTS.map((v) => v.name).join(', ')}`);
  return picked;
}

function onPath(name) {
  const exts = process.platform === 'win32' ? ['.exe', '.cmd', ''] : [''];
  for (const dir of (process.env.PATH || '').split(path.delimiter)) {
    if (!dir) continue;
    for (const ext of exts) {
      const full = path.join(dir, name + ext);
      try {
        fs.accessSync(full, fs.constants.X_OK);
        return full;
      } catch (_) {}
    }
  }
  return '';
}

// Resolves bare names against PATH so a missing browser is a clean SKIP, not an
// unhandled spawn ENOENT (the self-hosted runners ship no Chrome, #11810).
function chromePath() {
  for (const candidate of CHROME_CANDIDATES) {
    if (candidate.includes(path.sep)) {
      if (fs.existsSync(candidate)) return candidate;
      continue;
    }
    const found = onPath(candidate);
    if (found) return found;
  }
  return '';
}

function getJSON(pathname) {
  return new Promise((resolve, reject) => {
    const req = http.get({ host: '127.0.0.1', port: DEBUG_PORT, path: pathname }, (res) => {
      let body = '';
      res.on('data', (chunk) => { body += chunk; });
      res.on('end', () => {
        try { resolve(JSON.parse(body)); } catch (err) { reject(err); }
      });
    });
    req.on('error', reject);
  });
}

async function waitForTabs() {
  for (let i = 0; i < STARTUP_RETRIES; i++) {
    try {
      const tabs = await getJSON('/json/list');
      if (tabs.length) return tabs;
    } catch (_) {}
    await sleep(STARTUP_DELAY_MS);
  }
  throw new Error('Chrome did not expose a debugging tab');
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// Runs inside the page; returns findings for the current DOM at the current viewport.
const SCAN_EXPRESSION = `(() => {
  const vw = document.documentElement.clientWidth;
  const findings = [];
  const round = (n) => Math.round(n * 10) / 10;
  function pathFor(el) {
    const parts = [];
    for (let n = el; n && n.nodeType === 1 && n !== document.documentElement; n = n.parentElement) {
      let part = n.tagName.toLowerCase();
      if (n.id) { parts.unshift(part + '#' + n.id); break; }
      const cls = Array.from(n.classList).slice(0, 2).join('.');
      if (cls) part += '.' + cls;
      parts.unshift(part);
    }
    return parts.join(' > ');
  }
  function visible(el, cs, r) {
    return cs.display !== 'none' && cs.visibility !== 'hidden' && r.width > 0 && r.height > 0;
  }
  function hasOwnText(el) {
    return Array.from(el.childNodes).some((c) => c.nodeType === 3 && c.textContent.trim());
  }
  function tileFor(el) {
    for (let n = el.parentElement; n && n !== document.body; n = n.parentElement) {
      const cs = getComputedStyle(n);
      if (typeof n.className === "string" && /tile|card/.test(n.className)) return n;
      if (cs.display.includes('flex') || cs.display.includes('grid')) return n;
    }
    return el.parentElement;
  }
  function add(el, kind, r, detail) {
    findings.push({
      selector: pathFor(el),
      bbox: { x: round(r.left + scrollX), y: round(r.top + scrollY), width: round(r.width), height: round(r.height) },
      kind,
      detail,
    });
  }
  for (const el of document.body.querySelectorAll('*')) {
    if (el.closest('svg') && el.tagName.toLowerCase() !== 'svg') continue;
    const cs = getComputedStyle(el);
    const r = el.getBoundingClientRect();
    if (!visible(el, cs, r)) continue;
    const ox = cs.overflowX;
    if (el.scrollWidth > el.clientWidth + 1 && el.clientWidth > 0 && ox !== 'auto' && ox !== 'scroll') {
      add(el, 'scroll-overflow', r, 'scrollWidth ' + el.scrollWidth + ' > clientWidth ' + el.clientWidth + ', overflow-x ' + ox);
    }
    if (r.right > vw + 1 || r.left < -1) {
      add(el, 'past-viewport', r, 'right ' + round(r.right) + ' / left ' + round(r.left) + ' vs viewport ' + vw);
    }
    if (el.matches('.oc-topbar .nav-chip')) {
      for (let n = el.parentElement; n && !n.classList.contains('oc-topbar'); n = n.parentElement) {
        const nox = getComputedStyle(n).overflowX;
        if (nox !== 'hidden' && nox !== 'clip') continue;
        const nr = n.getBoundingClientRect();
        if (r.left < nr.left - 1 || r.right > nr.right + 1) {
          add(el, 'clipped-by-strip', r, 'chip spans ' + round(r.left) + '..' + round(r.right) + ' but overflow:' + nox + ' ancestor ' + pathFor(n) + ' spans ' + round(nr.left) + '..' + round(nr.right));
          break;
        }
      }
    }
    if (hasOwnText(el) && el.scrollWidth > el.clientWidth + 1 && el.clientWidth > 0) {
      if (cs.textOverflow === 'ellipsis' && ox === 'hidden') {
        const tile = tileFor(el);
        if (tile) {
          const tr = tile.getBoundingClientRect();
          const free = tr.right - r.right;
          if (free > 4) add(el, 'ellipsis-free-space', r, 'ellipsised text while its container has ' + round(free) + 'px free to the right');
        }
      } else if (ox === 'hidden' || ox === 'clip') {
        add(el, 'clipped-text', r, 'text clipped by overflow:' + ox + ' (scrollWidth ' + el.scrollWidth + ' > clientWidth ' + el.clientWidth + ')');
      }
    }
  }
  return findings;
})()`;

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  const viewports = selectViewports(opts.viewport);
  const themes = opts.theme ? [opts.theme] : THEMES;
  const bin = chromePath();
  if (!bin) {
    if (process.env.CI) throw new Error('Canonical browser proof requires Chrome/Chromium; none found');
    console.log('SKIP: Chrome/Chromium not found (set CHROME_BIN or install google-chrome/chromium)');
    return;
  }
  fs.mkdirSync(opts.out, { recursive: true });
  fs.rmSync(PROFILE_DIR, { recursive: true, force: true });
  const chrome = spawn(bin, [
    '--headless=new',
    '--disable-gpu',
    '--no-sandbox',
    `--remote-debugging-port=${DEBUG_PORT}`,
    `--user-data-dir=${PROFILE_DIR}`,
    '--window-size=1440,1000',
    'about:blank',
  ], { stdio: 'ignore' });
  chrome.on('error', (err) => {
    console.error(`ERROR: failed to launch ${bin}: ${err.message}`);
    process.exit(1);
  });

  async function cleanup() {
    chrome.kill('SIGTERM');
    await new Promise((resolve) => chrome.once('exit', resolve));
    // Chrome helpers can still finish profile writes after the browser exits,
    // so removal can race (ENOTEMPTY on Default/). The profile is scratch:
    // retry generously, then warn instead of failing an otherwise-good run.
    try {
      fs.rmSync(PROFILE_DIR, { recursive: true, force: true, maxRetries: 20, retryDelay: 250 });
    } catch (err) {
      console.warn(`WARN: could not remove ${PROFILE_DIR}: ${err.message}`);
    }
  }

  const report = [];
  const preparationRailReport = [];
  const activityReport = [];
  const activityFailures = [];
  const fixtureRoot = path.join(process.cwd(), 'src/pkg/dashboard/webstatic/testdata');
  const density = JSON.parse(fs.readFileSync(path.join(fixtureRoot, 'tracker-activity-density.json'), 'utf8'));
  const probes = ['tracker-header-geometry', 'tracker-decision-interaction'].map(name => ({
    name, source: fs.readFileSync(path.join(fixtureRoot, name + '.js'), 'utf8'),
  }));
  const event = process.env.GITHUB_EVENT_PATH ? JSON.parse(fs.readFileSync(process.env.GITHUB_EVENT_PATH, 'utf8')) : {};
  const provenance = { head: event.pull_request?.head?.sha || process.env.GITHUB_SHA || 'unpublished-local',
    checkoutSHA: process.env.GITHUB_SHA || 'unpublished-local',
    generatedAssetSHA256: createHash('sha256').update(fs.readFileSync('src/pkg/dashboard/static/index.html')).digest('hex'),
    mockEvidence: true, fixtureSource: density.source };
  let socket;
  try {
    const tabs = await waitForTabs();
    const page = tabs.find((tab) => tab.type === 'page') || tabs[0];
    socket = new WebSocket(page.webSocketDebuggerUrl);
    let id = 0;
    const pending = new Map();
    socket.onmessage = (event) => {
      const message = JSON.parse(event.data);
      if (message.id && pending.has(message.id)) {
        pending.get(message.id)(message);
        pending.delete(message.id);
      }
    };
    await new Promise((resolve) => { socket.onopen = resolve; });
    const send = (method, params = {}) => new Promise((resolve) => {
      const requestID = ++id;
      pending.set(requestID, resolve);
      socket.send(JSON.stringify({ id: requestID, method, params }));
    });
    const evaluate = async (expression) => {
      const result = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
      if (result.error) throw new Error(result.error.message);
      if (result.result.exceptionDetails) throw new Error(result.result.exceptionDetails.exception?.description || 'evaluate failed');
      return result.result.result.value;
    };
    const screenshot = async (file, viewportOnly = false) => {
      const viewport = viewportOnly ? await evaluate(`({
        x: scrollX, y: scrollY, width: innerWidth, height: innerHeight,
        clientWidth: document.documentElement.clientWidth,
        clientHeight: document.documentElement.clientHeight
      })`) : null;
      const clip = viewport ? { x: viewport.x, y: viewport.y, width: viewport.width, height: viewport.height, scale: 1 } : undefined;
      const shot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: !viewportOnly, ...(clip ? { clip } : {}) });
      if (shot.error) throw new Error(shot.error.message);
      fs.writeFileSync(path.join(opts.out, file), Buffer.from(shot.result.data, 'base64'));
      if (!viewport) return;
      const pixels = await evaluate(`(async () => {
        const image = new Image();
        image.src = ${JSON.stringify('data:image/png;base64,')} + ${JSON.stringify(shot.result.data)};
        await image.decode();
        const canvas = document.createElement('canvas');
        canvas.width = image.naturalWidth; canvas.height = image.naturalHeight;
        const context = canvas.getContext('2d', { willReadFrequently: true });
        context.drawImage(image, 0, 0);
        const width = Math.min(canvas.width, Math.floor(${viewport.clientWidth} * canvas.width / ${viewport.width}));
        const height = Math.min(canvas.height, Math.floor(${viewport.clientHeight} * canvas.height / ${viewport.height}));
        const data = context.getImageData(0, 0, width, height).data;
        let painted = false;
        for (let pixel = 4; pixel < data.length; pixel += 4) {
          if (data[pixel] !== data[0] || data[pixel + 1] !== data[1] ||
              data[pixel + 2] !== data[2] || data[pixel + 3] !== data[3]) { painted = true; break; }
        }
        return { painted, width: canvas.width, height: canvas.height, inspectedWidth: width, inspectedHeight: height };
      })()`);
      return { file, viewport, clip, ...pixels,
        failures: pixels.painted ? [] : ['Viewport capture contains only a uniform background; DOM assertions are not visual proof'] };
    };
    const nativeDecisionInput = async prefix => {
      const failures = [];
      const point = await evaluate(`(async () => {
        stageTrackerCloseDecisionPop(false);
        const trigger = [...document.querySelectorAll('.ham-chevron-decision .ham-decision-trigger')]
          .find(button => Number(button.closest('[data-ham-stage]')?.dataset.hamStage) === 80);
        if (!trigger) throw new Error('Missing native stage-80 decision trigger');
        trigger.scrollIntoView({ block: 'center', inline: 'nearest', behavior: 'instant' });
        await new Promise(requestAnimationFrame);
        const help = document.querySelector('.ham-chevron-info.config-info');
        help.focus({ preventScroll: true });
        const box = trigger.getBoundingClientRect();
        const portal = document.getElementById('config-tooltip-portal');
        const target = document.elementFromPoint((box.left + box.right) / 2, (box.top + box.bottom) / 2);
        return { x: (box.left + box.right) / 2, y: (box.top + box.bottom) / 2,
          documentFocused: document.hasFocus(), helpFocused: document.activeElement === help,
          triggerReachable: target === trigger || trigger.contains(target),
          target: target?.className || target?.tagName || null,
          tooltipWasOpen: !!portal && !portal.hidden && getComputedStyle(portal).display !== 'none' };
      })()`);
      if (!point.tooltipWasOpen) failures.push('Native pointer precondition: ordinary tooltip was not open');
      if (!point.triggerReachable) failures.push('Native pointer precondition: trigger is intercepted by ' + point.target);
      const moved = await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: point.x, y: point.y, pointerType: 'mouse' });
      if (moved.error) throw new Error(moved.error.message);
      await sleep(STATE_SETTLE_MS);
      const inspect = async mode => {
        const result = await evaluate(`(() => {
          const dialogs = [...document.querySelectorAll('.ham-decision-pop')];
          const portal = document.getElementById('config-tooltip-portal');
          const tolerance = 1;
          const actions = [...(dialogs[0]?.querySelectorAll('.ham-decision-act') || [])].map(action => {
            const box = action.getBoundingClientRect();
            const target = document.elementFromPoint((box.left + box.right) / 2, (box.top + box.bottom) / 2);
            return { label: action.textContent.trim(), box: box.toJSON(),
              target: target?.className || target?.tagName || null,
              reachable: box.width > 0 && box.height > 0 && box.left >= -tolerance &&
                box.right <= innerWidth + tolerance && box.top >= -tolerance &&
                box.bottom <= innerHeight + tolerance && (target === action || action.contains(target)) };
          });
          return { dialogs: dialogs.length,
            tooltipOpen: !!portal && !portal.hidden && getComputedStyle(portal).display !== 'none',
            focusInside: !!dialogs[0]?.contains(document.activeElement), actions };
        })()`);
        if (result.dialogs !== 1 || result.tooltipOpen) failures.push(mode + ': one unobscured decision dialog required');
        if (mode !== 'native pointer' && !result.focusInside) failures.push(mode + ': keyboard must enter the existing dialog');
        if (!result.actions.length || result.actions.some(action => !action.reachable)) {
          failures.push(mode + ': every native decision action must be reachable and unobscured: ' + JSON.stringify(result.actions));
        }
        return { mode, ...result };
      };
      const observations = [await inspect('native pointer')];
      const pointerCapture = await screenshot(prefix + '-decision-pointer.png', true);
      failures.push(...pointerCapture.failures);
      const keys = [{ key: 'ArrowDown', code: 'ArrowDown', keyCode: 40 },
        { key: 'Enter', code: 'Enter', keyCode: 13 }, { key: ' ', code: 'Space', keyCode: 32 }];
      for (const { key, code, keyCode } of keys) {
        await evaluate(`(() => {
          const trigger = [...document.querySelectorAll('.ham-chevron-decision .ham-decision-trigger')]
            .find(button => Number(button.closest('[data-ham-stage]')?.dataset.hamStage) === 80);
          trigger.focus({ preventScroll: true });
          return true;
        })()`);
        for (const type of ['keyDown', 'keyUp']) {
          const pressed = await send('Input.dispatchKeyEvent', { type, key, code, windowsVirtualKeyCode: keyCode });
          if (pressed.error) throw new Error(pressed.error.message);
        }
        observations.push(await inspect('native keyboard ' + JSON.stringify(key)));
      }
      const keyboardCapture = await screenshot(prefix + '-decision-keyboard.png', true);
      failures.push(...keyboardCapture.failures);
      await evaluate('stageTrackerCloseDecisionPop(false); true');
      return { pointerPrecondition: point, observations, captures: [pointerCapture, keyboardCapture], failures };
    };

    await send('Page.enable');
    await send('Runtime.enable');
    // Headless input must own focus for native focusin and keyboard delivery.
    await send('Page.bringToFront');
    await send('Emulation.setFocusEmulationEnabled', { enabled: true });

    // Each state callback is best-effort: a missing helper yields a skipped shot, not a harness error.
    const setThemeJS = (theme) => `document.body.classList.toggle('light-mode', ${theme === 'light'}); document.documentElement.style.colorScheme = '${theme}'; true`;
    const resetJS = `(() => {
      window.__activityDensityCleanup?.();
      document.getElementById('preparation-rail-layout-fixture')?.remove();
      // The preceding terminal state owns a native dialog and capture listener.
      // Close it through its API, not by hiding/removing its DOM.
      document.querySelectorAll('.hive-dialog-overlay').forEach((el) => {
        if (typeof el.hiveDialogClose !== 'function') throw new Error('Cannot clean up prior fixture dialog');
        el.hiveDialogClose();
      });
      if (document.querySelector('.hive-dialog-overlay')) throw new Error('Prior fixture dialog survived native cleanup');
      document.querySelectorAll('.config-overlay, .modal-overlay').forEach((el) => { el.classList.add('hidden'); });
      document.documentElement.classList.remove('modal-open'); document.body.classList.remove('modal-open');
      return true;
    })()`;
    const collapseAllJS = `(() => {
      const ids = Array.from(document.querySelectorAll('[data-dashboard-section]')).map((el) => el.id).filter(Boolean);
      for (const sid of ids) { try { toggleSection(sid, { collapsed: true, persist: false, animate: false }); } catch (_) {} }
      window.scrollTo(0, 0);
      return ids;
    })()`;
    const expandOneJS = (sid) => `(() => {
      try { toggleSection(${JSON.stringify(sid)}, { collapsed: false, persist: false, animate: false }); } catch (_) { return false; }
      const el = document.getElementById(${JSON.stringify(sid)});
      if (el) el.scrollIntoView();
      return true;
    })()`;
    const settingsJS = `(() => { if (typeof openConfigDialog !== 'function') return false; openConfigDialog('governor'); return true; })()`;
    const knowledgeJS = `(() => { if (typeof kbOpenSubscriptions !== 'function') return false; kbOpenSubscriptions(); return true; })()`;
    const terminalJS = `(() => {
      if (typeof hiveExportViewer !== 'function') return false;
      hiveExportViewer({ title: 'Terminal URL for agent', text: 'https://hive.example/terminal/agent?code=000000', allowOpen: true });
      return true;
    })()`;

    for (const vp of viewports) {
      const landscape = vp.width > vp.height;
      await send('Emulation.setDeviceMetricsOverride', {
        width: vp.width, height: vp.height, deviceScaleFactor: vp.dpr, mobile: vp.mobile,
        screenOrientation: { type: landscape ? 'landscapePrimary' : 'portraitPrimary', angle: landscape ? 90 : 0 },
      });
      await send('Emulation.setUserAgentOverride', { userAgent: vp.ua });
      await send('Emulation.setTouchEmulationEnabled', { enabled: vp.mobile });
      for (const theme of themes) {
        await send('Page.navigate', { url: `file://${process.cwd()}/src/pkg/dashboard/static/index.html` });
        await sleep(PAGE_SETTLE_MS);
        // Absolute production URLs cannot load from file://. Use the same
        // checked-in sidecars rather than silently testing without tokens.
        await evaluate(`Promise.all(${JSON.stringify(['tokens.css', 'components.css'].map(name => ({
          name, url: `file://${process.cwd()}/src/pkg/dashboard/static/${name}`,
        })))}.map(({name, url}) => new Promise((resolve, reject) => {
          const link = document.querySelector('link[href="/' + name + '"]');
          if (!link) return reject(new Error('Missing production stylesheet ' + name));
          link.onload = () => resolve(true);
          link.onerror = () => reject(new Error('Cannot load production stylesheet ' + name));
          link.href = url;
        })))`);
        await evaluate(setThemeJS(theme));

        const sectionIDs = await evaluate(collapseAllJS);
        const states = [{ name: 'collapsed', run: async () => true }];
        for (const sid of sectionIDs) {
          states.push({ name: `expanded-${sid}`, run: async () => { await evaluate(collapseAllJS); return evaluate(expandOneJS(sid)); } });
        }
        states.push({ name: 'settings', run: async () => { await evaluate(collapseAllJS); return evaluate(settingsJS); } });
        states.push({ name: 'knowledge-sources', run: async () => { await evaluate(collapseAllJS); return evaluate(knowledgeJS); } });
        states.push({ name: 'terminal', run: async () => { await evaluate(collapseAllJS); return evaluate(terminalJS); } });
        states.push({ name: 'preparation-rail', run: async () => {
          await evaluate(collapseAllJS);
          if (!await evaluate(expandOneJS('activity-monitor-section'))) throw new Error('Cannot expand actual Activity Monitor');
          const result = await evaluate('(' + preparationRailLayoutFixture.toString() + ')()');
          preparationRailReport.push({ viewport: vp.name, theme, ...result });
          if (result.failures.length) throw new Error('Preparation rail layout: ' + result.failures.join('; '));
          return true;
        } });
        if (vp.activityOnly) states.length = 0;
        states.push({ name: 'activity-density', run: async () => {
          await evaluate(collapseAllJS);
          if (!await evaluate(expandOneJS('activity-monitor-section'))) throw new Error('Cannot expand actual Activity Monitor');
          const fixture = await evaluate('(' + activityDensityFixture.toString() + ')(' + JSON.stringify(density) + ')');
          await sleep(STATE_SETTLE_MS);
          const capture = await screenshot(`${vp.name}-${theme}-activity-density-viewport.png`, true);
          const results = [{ probe: 'viewport-paint', ok: capture.painted, result: capture }];
          if (!capture.painted) activityFailures.push(vp.name + ' ' + theme + ' blank viewport capture');
          for (const probe of probes) {
            const result = await evaluate(`(async () => {
              try { return { ok: true, result: await (${probe.source})({ dedicatedPreview: true }) }; }
              catch (error) {
                let result;
                try { result = JSON.parse(error.message); } catch (_) { result = { failures: [String(error.stack || error)] }; }
                return { ok: false, result };
              }
            })()`);
            results.push({ probe: probe.name, ...result });
            if (!result.ok) activityFailures.push(vp.name + ' ' + theme + ' ' + probe.name);
          }
          let native;
          try { native = await nativeDecisionInput(`${vp.name}-${theme}-activity`); }
          catch (error) { native = { failures: [String(error.stack || error)] }; }
          const nativeOK = native.failures.length === 0;
          results.push({ probe: 'native-CDP-pointer-keyboard', ok: nativeOK, result: native });
          if (!nativeOK) activityFailures.push(vp.name + ' ' + theme + ' native input');
          const result = { viewport: vp.name, theme, provenance, fixture, results };
          activityReport.push(result);
          fs.writeFileSync(path.join(opts.out, `${vp.name}-${theme}-activity-probes.json`), JSON.stringify(result, null, 2) + '\n');
          return true;
        } });

        for (const state of states) {
          await evaluate(resetJS);
          let ok;
          try { ok = await state.run(); }
          catch (error) {
            if (state.name !== 'activity-density') throw error;
            activityFailures.push(vp.name + ' ' + theme + ' fixture/harness');
            const result = { viewport: vp.name, theme, provenance, failures: [String(error.stack || error)] };
            activityReport.push(result);
            fs.writeFileSync(path.join(opts.out, `${vp.name}-${theme}-activity-probes.json`), JSON.stringify(result, null, 2) + '\n');
            ok = true; // Capture the broken fixture too; fail after all viewports.
          }
          if (!ok) {
            console.log(`skip ${vp.name} ${theme} ${state.name}: not available`);
            continue;
          }
          await sleep(STATE_SETTLE_MS);
          const findings = await evaluate(SCAN_EXPRESSION);
          for (const f of findings) report.push({ viewport: vp.name, theme, state: state.name, ...f });
          const file = `${vp.name}-${theme}-${state.name}.png`.replace(/[^A-Za-z0-9._@-]/g, '_');
          await screenshot(file);
          console.log(`shot ${file}: ${findings.length} finding(s)`);
        }
        await evaluate(resetJS);
      }
    }
  } finally {
    if (socket) socket.close();
    fs.writeFileSync(path.join(opts.out, 'overflow-report.json'), JSON.stringify(report, null, 2) + '\n');
    fs.writeFileSync(path.join(opts.out, 'preparation-rail-layout.json'), JSON.stringify(preparationRailReport, null, 2) + '\n');
    fs.writeFileSync(path.join(opts.out, 'activity-layout.json'), JSON.stringify({ provenance, runs: activityReport }, null, 2) + '\n');
    await cleanup();
  }
  console.log(`wrote ${report.length} finding(s) to ${path.join(opts.out, 'overflow-report.json')}`);
  if (activityFailures.length) throw new Error('Activity browser acceptance failed: ' + activityFailures.join('; '));
}

main().catch((err) => {
  console.error(err.message || err);
  process.exit(1);
});
