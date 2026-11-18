#!/usr/bin/env node
// UI render-evidence harness (#18915).
//
// Usage:
//   node src/scripts/dashboard-render-evidence.mjs [--out <dir>] [--changed-files <file>]
//        [--pr <number>] [--head <sha>]
//
// Serves the BUILT dashboard pages from this checkout on 127.0.0.1, answers
// every /api/* request from synthetic fixtures under
// src/scripts/fixtures/render-evidence/api/ (a request with no fixture gets a
// 404), and screenshots the pages and sections the PR touches at desktop and
// phone width in light and dark themes. Chrome is started with every
// non-loopback host resolved to NOTFOUND, so nothing can reach a real hive or
// any other network, and no credential is read or needed.
//
// It NEVER fails the job for screenshot trouble: any problem becomes a
// "partial" or "neutral" status with a reason in index.json, and the process
// exits 0. Capturing stops starting new shots once RENDER_EVIDENCE_BUDGET_MS
// has elapsed.
//
// Output (default dir: render-evidence-out/):
//   <page>-<section>-<viewport>-<theme>.png
//   index.json  (see buildIndex in lib/render-evidence.mjs)
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { waitForDashboardPaint } from './fixtures/dashboard-browser-readiness.mjs';
import {
  RENDER_EVIDENCE_BUDGET_MS, RENDER_EVIDENCE_SHOT_TIMEOUT_MS, VIEWPORTS, THEMES,
  affectedPages, planSections, sectionIdsInHTML, shotFileName, classifyStatus, buildIndex,
} from './lib/render-evidence.mjs';

const CHROME_CANDIDATES = [
  process.env.CHROME_BIN,
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  'google-chrome',
  'chromium',
].filter(Boolean);
const DEBUG_PORT = 9236;
const STARTUP_RETRIES = 80;
const STARTUP_DELAY_MS = 250;
const PAGE_SETTLE_MS = 1500;
const SECTION_SETTLE_MS = 250;
const FIXTURE_DIR = path.join('src', 'scripts', 'fixtures', 'render-evidence', 'api');
const CONTENT_TYPES = { '.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8', '.json': 'application/json', '.png': 'image/png',
  '.svg': 'image/svg+xml', '.webmanifest': 'application/manifest+json' };

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function parseArgs(argv) {
  const opts = { out: path.join(process.cwd(), 'render-evidence-out'), changed: '', pr: 0, head: process.env.GITHUB_SHA || '' };
  for (let i = 0; i < argv.length; i++) {
    const [key, inline] = argv[i].split('=', 2);
    const value = () => (inline !== undefined ? inline : argv[++i]);
    if (key === '--out') opts.out = path.resolve(value());
    else if (key === '--changed-files') opts.changed = String(value() || '');
    else if (key === '--pr') opts.pr = parseInt(value(), 10) || 0;
    else if (key === '--head') opts.head = String(value() || '');
    else throw new Error(`unknown argument: ${argv[i]}`);
  }
  return opts;
}

function chromePath() {
  for (const candidate of CHROME_CANDIDATES) {
    if (candidate.includes(path.sep)) {
      if (fs.existsSync(candidate)) return candidate;
      continue;
    }
    for (const dir of (process.env.PATH || '').split(path.delimiter)) {
      if (!dir) continue;
      const full = path.join(dir, candidate);
      try { fs.accessSync(full, fs.constants.X_OK); return full; } catch (_) { /* keep looking */ }
    }
  }
  return '';
}

// Serves one page's static root and the API fixtures. `current` is switched
// per page so one server and one Chrome cover every page.
function startServer(current) {
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, 'http://127.0.0.1');
    const pathname = decodeURIComponent(url.pathname);
    if (pathname.startsWith('/api/')) {
      const rel = path.normalize(pathname.slice('/api/'.length));
      const file = path.join(FIXTURE_DIR, rel + '.json');
      const inside = !rel.startsWith('..') && !path.isAbsolute(rel);
      if (req.method === 'GET' && inside && fs.existsSync(file)) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(fs.readFileSync(file));
        return;
      }
      res.writeHead(404, { 'Content-Type': 'application/json' });
      res.end('{"error":"no render-evidence fixture for this route"}');
      return;
    }
    const page = current.page;
    let file = pathname === '/' ? page.file : path.join(page.staticRoot, path.normalize(pathname));
    const root = path.resolve(page.staticRoot);
    const resolved = path.resolve(file);
    const allowed = resolved === path.resolve(page.file) || resolved.startsWith(root + path.sep);
    if (!allowed || !fs.existsSync(resolved) || fs.statSync(resolved).isDirectory()) {
      res.writeHead(404);
      res.end();
      return;
    }
    res.writeHead(200, { 'Content-Type': CONTENT_TYPES[path.extname(resolved)] || 'application/octet-stream' });
    res.end(fs.readFileSync(resolved));
  });
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve(server)));
}

function getJSON(pathname) {
  return new Promise((resolve, reject) => {
    const req = http.get({ host: '127.0.0.1', port: DEBUG_PORT, path: pathname }, (res) => {
      let body = '';
      res.on('data', (chunk) => { body += chunk; });
      res.on('end', () => { try { resolve(JSON.parse(body)); } catch (err) { reject(err); } });
    });
    req.on('error', reject);
  });
}

async function waitForTab() {
  for (let i = 0; i < STARTUP_RETRIES; i++) {
    try {
      const tabs = await getJSON('/json/list');
      const page = tabs.find((t) => t.type === 'page');
      if (page) return page;
    } catch (_) { /* Chrome still starting */ }
    await sleep(STARTUP_DELAY_MS);
  }
  throw new Error('Chrome did not expose a debugging tab');
}

function withTimeout(promise, ms, what) {
  let timer;
  const timeout = new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(what + ' timed out')), ms); });
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}

const setThemeJS = (theme) => `(() => {
  const light = ${theme === 'light'};
  document.documentElement.setAttribute('data-theme', ${JSON.stringify(theme)});
  document.documentElement.classList.toggle('light-mode', light);
  document.body.classList.toggle('light-mode', light);
  document.documentElement.style.colorScheme = ${JSON.stringify(theme)};
  return true;
})()`;
const collapseAllJS = `(() => {
  const ids = Array.from(document.querySelectorAll('[data-dashboard-section]')).map((el) => el.id).filter(Boolean);
  for (const sid of ids) { try { toggleSection(sid, { collapsed: true, persist: false, animate: false }); } catch (_) {} }
  window.scrollTo(0, 0);
  return ids;
})()`;
const expandOneJS = (sid) => `(async () => {
  if (${JSON.stringify(sid)} === 'kick-outcomes') {
    // Isolated component evidence using the same API response and production renderer.
    const data = await (await fetch('/api/kick-outcomes?agent=fixture-agent')).json();
    const panel = document.getElementById('oc-agent-detail');
    if (!panel || typeof kickOutcomesHtml !== 'function') return false;
    panel.className = 'oc-agent-detail active';
    panel.innerHTML = '<section class="kick-outcomes"><h3>Recent kick outcomes — fixture agent</h3><div class="kick-outcomes-body">' + kickOutcomesHtml(data) + '</div></section>';
    panel.scrollIntoView();
    return true;
  }
  try { toggleSection(${JSON.stringify(sid)}, { collapsed: false, persist: false, animate: false }); } catch (_) { return false; }
  const el = document.getElementById(${JSON.stringify(sid)});
  if (!el) return false;
  el.scrollIntoView();
  return true;
})()`;

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  fs.mkdirSync(opts.out, { recursive: true });
  const started = Date.now();
  const shots = [];
  const skipped = [];
  let planned = 0;
  let budgetExceeded = false;
  let harnessError = '';

  const changedFiles = opts.changed && fs.existsSync(opts.changed)
    ? fs.readFileSync(opts.changed, 'utf8').split('\n').map((l) => l.trim()).filter(Boolean)
    : [];
  const pages = affectedPages(changedFiles);

  const writeIndex = () => {
    const { status, reason } = classifyStatus({ planned, captured: shots.length, budgetExceeded, harnessError });
    const index = buildIndex({ pr: opts.pr, head: opts.head, status, reason, shots, skipped,
      budgetMs: RENDER_EVIDENCE_BUDGET_MS, elapsedMs: Date.now() - started });
    fs.writeFileSync(path.join(opts.out, 'index.json'), JSON.stringify(index, null, 2) + '\n');
    console.log(`render evidence: ${status}${reason ? ' (' + reason + ')' : ''}, ${shots.length} shot(s)`);
  };

  if (!pages.length) {
    writeIndex();
    return;
  }

  const bin = chromePath();
  if (!bin) {
    planned = 1;
    harnessError = 'Chrome or Chromium is not available on this runner';
    writeIndex();
    return;
  }

  const current = { page: pages[0] };
  const server = await startServer(current);
  const baseURL = `http://127.0.0.1:${server.address().port}/`;
  const profile = path.join(process.cwd(), '.chrome-render-evidence');
  fs.rmSync(profile, { recursive: true, force: true });
  const chrome = spawn(bin, [
    '--headless=new', '--disable-gpu', '--no-sandbox',
    `--remote-debugging-port=${DEBUG_PORT}`,
    `--user-data-dir=${profile}`,
    // No network but this machine: every other host fails to resolve.
    '--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1',
    '--window-size=1440,1000',
    'about:blank',
  ], { stdio: 'ignore' });
  chrome.on('error', (err) => { harnessError = `could not launch Chrome: ${err.message}`; });

  let socket;
  try {
    const tab = await waitForTab();
    socket = new WebSocket(tab.webSocketDebuggerUrl);
    let id = 0;
    const pending = new Map();
    socket.onmessage = (event) => {
      const message = JSON.parse(event.data);
      if (message.id && pending.has(message.id)) { pending.get(message.id)(message); pending.delete(message.id); }
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
      if (result.result.exceptionDetails) throw new Error('page script failed');
      return result.result.result.value;
    };
    await send('Page.enable');
    await send('Runtime.enable');

    // Plan first so "planned" is known even if the budget cuts capturing short.
    const jobs = [];
    for (const page of pages) {
      const html = fs.existsSync(page.file) ? fs.readFileSync(page.file, 'utf8') : '';
      const sectionIds = page.sections ? sectionIdsInHTML(html) : [];
      const sections = page.sections
        ? planSections(changedFiles, sectionIds, (f) => fs.readFileSync(f, 'utf8'))
        : [];
      for (const vp of VIEWPORTS) for (const theme of THEMES) {
        jobs.push({ page, vp, theme, section: null });
        for (const section of sections) jobs.push({ page, vp, theme, section });
      }
    }
    planned = jobs.length;

    let lastKey = '';
    for (const job of jobs) {
      if (Date.now() - started > RENDER_EVIDENCE_BUDGET_MS) { budgetExceeded = true; break; }
      const { page, vp, theme, section } = job;
      const what = `${page.id} ${section || 'overview'} ${vp.id} ${theme}`;
      try {
        await withTimeout((async () => {
          const key = `${page.id}|${vp.id}|${theme}`;
          if (key !== lastKey) {
            current.page = page;
            await send('Emulation.setDeviceMetricsOverride', { width: vp.width, height: vp.height,
              deviceScaleFactor: vp.dpr, mobile: vp.mobile });
            await send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: theme }] });
            await send('Page.navigate', { url: baseURL });
            await sleep(PAGE_SETTLE_MS);
            await evaluate(setThemeJS(theme));
            lastKey = key;
          }
          if (page.sections) {
            await evaluate(collapseAllJS);
            if (section && !await evaluate(expandOneJS(section))) throw new Error('section is not present or cannot expand');
          }
          await sleep(SECTION_SETTLE_MS);
          await evaluate('(' + waitForDashboardPaint.toString() + ')({ dedicatedPreview: true })');
          const shot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: !section });
          if (shot.error) throw new Error(shot.error.message);
          const file = shotFileName({ page: page.id, section, viewport: vp.id, theme });
          const data = Buffer.from(shot.result.data, 'base64');
          fs.writeFileSync(path.join(opts.out, file), data);
          shots.push({ file, page: page.id, section: section || 'overview', viewport: vp.id, theme, bytes: data.length });
        })(), RENDER_EVIDENCE_SHOT_TIMEOUT_MS, what);
        console.log(`shot ${what}`);
      } catch (error) {
        lastKey = '';
        skipped.push({ what, reason: String(error && error.message || error).slice(0, 120) });
        console.log(`skip ${what}: ${error && error.message}`);
      }
    }
  } catch (error) {
    harnessError = String(error && error.message || error).slice(0, 160);
  } finally {
    if (socket) { try { socket.close(); } catch (_) { /* already closed */ } }
    chrome.kill('SIGTERM');
    server.close();
    try { fs.rmSync(profile, { recursive: true, force: true, maxRetries: 10, retryDelay: 250 }); } catch (_) { /* scratch */ }
    writeIndex();
  }
}

main().catch((err) => {
  // Never fail the PR for evidence trouble: record it and exit 0.
  console.error(err && err.message || err);
  try {
    const out = path.join(process.cwd(), 'render-evidence-out');
    fs.mkdirSync(out, { recursive: true });
    const index = buildIndex({ pr: 0, head: '', status: 'neutral', reason: String(err && err.message || err).slice(0, 160),
      shots: [], skipped: [], budgetMs: RENDER_EVIDENCE_BUDGET_MS, elapsedMs: 0 });
    fs.writeFileSync(path.join(out, 'index.json'), JSON.stringify(index, null, 2) + '\n');
  } catch (_) { /* nothing more to do */ }
  process.exit(0);
});
