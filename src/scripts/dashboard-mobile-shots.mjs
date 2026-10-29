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
// This is a reporting tool, not a gate: it exits 0 even when findings exist and
// exits 1 only when the harness itself errors. Chrome missing prints SKIP and exits 0.
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';

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
    // Chrome helpers can still finish profile writes after the browser exits.
    fs.rmSync(PROFILE_DIR, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
  }

  const report = [];
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

    await send('Page.enable');
    await send('Runtime.enable');

    // Each state callback is best-effort: a missing helper yields a skipped shot, not a harness error.
    const setThemeJS = (theme) => `document.body.classList.toggle('light-mode', ${theme === 'light'}); document.documentElement.style.colorScheme = '${theme}'; true`;
    const resetJS = `(() => {
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
      await send('Emulation.setTouchEmulationEnabled', { enabled: true });
      for (const theme of themes) {
        await send('Page.navigate', { url: `file://${process.cwd()}/src/pkg/dashboard/static/index.html` });
        await sleep(PAGE_SETTLE_MS);
        await evaluate(setThemeJS(theme));

        const sectionIDs = await evaluate(collapseAllJS);
        const states = [{ name: 'collapsed', run: async () => true }];
        for (const sid of sectionIDs) {
          states.push({ name: `expanded-${sid}`, run: async () => { await evaluate(collapseAllJS); return evaluate(expandOneJS(sid)); } });
        }
        states.push({ name: 'settings', run: async () => { await evaluate(collapseAllJS); return evaluate(settingsJS); } });
        states.push({ name: 'knowledge-sources', run: async () => { await evaluate(collapseAllJS); return evaluate(knowledgeJS); } });
        states.push({ name: 'terminal', run: async () => { await evaluate(collapseAllJS); return evaluate(terminalJS); } });

        for (const state of states) {
          await evaluate(resetJS);
          const ok = await state.run();
          if (!ok) {
            console.log(`skip ${vp.name} ${theme} ${state.name}: not available`);
            continue;
          }
          await sleep(STATE_SETTLE_MS);
          const findings = await evaluate(SCAN_EXPRESSION);
          for (const f of findings) report.push({ viewport: vp.name, theme, state: state.name, ...f });
          const shot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
          if (shot.error) throw new Error(shot.error.message);
          const file = `${vp.name}-${theme}-${state.name}.png`.replace(/[^A-Za-z0-9._@-]/g, '_');
          fs.writeFileSync(path.join(opts.out, file), Buffer.from(shot.result.data, 'base64'));
          console.log(`shot ${file}: ${findings.length} finding(s)`);
        }
      }
    }
  } finally {
    if (socket) socket.close();
    await cleanup();
    fs.writeFileSync(path.join(opts.out, 'overflow-report.json'), JSON.stringify(report, null, 2) + '\n');
  }
  console.log(`wrote ${report.length} finding(s) to ${path.join(opts.out, 'overflow-report.json')}`);
}

main().catch((err) => {
  console.error(err.message || err);
  process.exit(1);
});
