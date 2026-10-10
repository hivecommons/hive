#!/usr/bin/env node
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
const DEBUG_PORT = 9234;
const STARTUP_RETRIES = 80;
const STARTUP_DELAY_MS = 250;
const PAGE_SETTLE_MS = 1500;
const MAX_TRIGGER_DISTANCE_PX = 12;
const PROFILE_DIR = path.join(process.cwd(), '.chrome-dashboard-tooltip-smoke');

function chromePath() {
  for (const candidate of CHROME_CANDIDATES) {
    if (candidate.includes(path.sep) && fs.existsSync(candidate)) return candidate;
    if (!candidate.includes(path.sep)) return candidate;
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
    await new Promise((resolve) => setTimeout(resolve, STARTUP_DELAY_MS));
  }
  throw new Error('Chrome did not expose a debugging tab');
}

async function main() {
  const bin = chromePath();
  if (!bin) {
    console.log('SKIP: Chrome/Chromium not found');
    return;
  }
  fs.rmSync(PROFILE_DIR, { recursive: true, force: true });
  const chrome = spawn(bin, [
    '--headless=new',
    '--disable-gpu',
    `--remote-debugging-port=${DEBUG_PORT}`,
    `--user-data-dir=${PROFILE_DIR}`,
    '--window-size=1440,1000',
    'about:blank',
  ], { stdio: 'ignore' });

  async function cleanup() {
    chrome.kill('SIGTERM');
    await new Promise((resolve) => chrome.once('exit', resolve));
    fs.rmSync(PROFILE_DIR, { recursive: true, force: true });
  }

  try {
    const tabs = await waitForTabs();
    const page = tabs.find((tab) => tab.type === 'page') || tabs[0];
    const socket = new WebSocket(page.webSocketDebuggerUrl);
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

    await send('Page.enable');
    await send('Runtime.enable');
    await send('Page.navigate', { url: `file://${process.cwd()}/src/pkg/dashboard/static/index.html` });
    await new Promise((resolve) => setTimeout(resolve, PAGE_SETTLE_MS));
    const expression = `(() => {
      const MAX_TRIGGER_DISTANCE_PX = ${MAX_TRIGGER_DISTANCE_PX};
      function distanceFor(label, marginTop, marginLeft) {
        const host = document.createElement('div');
        host.style.cssText = 'margin:' + marginTop + 'px 0 0 ' + marginLeft + 'px';
        host.innerHTML = '<label>' + label + ' <a class="config-info section-help-mark" href="#" aria-describedby="tip-' + label + '">?<span id="tip-' + label + '" class="config-tooltip" role="tooltip">' + label + ' help text</span></a></label>';
        document.getElementById('config-body').appendChild(host);
        const trigger = host.querySelector('.config-info');
        trigger.dispatchEvent(new MouseEvent('mouseover', { bubbles: true }));
        const portal = document.getElementById('config-tooltip-portal');
        const tr = trigger.getBoundingClientRect();
        const pr = portal.getBoundingClientRect();
        const verticalGap = pr.top >= tr.bottom ? pr.top - tr.bottom : tr.top - pr.bottom;
        const centerDelta = Math.abs((pr.left + pr.width / 2) - (tr.left + tr.width / 2));
        return { label, verticalGap: +verticalGap.toFixed(2), centerDelta: +centerDelta.toFixed(2), ok: verticalGap <= MAX_TRIGGER_DISTANCE_PX };
      }
      document.documentElement.classList.add('modal-open');
      document.body.classList.add('modal-open');
      const overlay = document.getElementById('config-overlay');
      overlay.classList.remove('hidden');
      overlay.style.display = 'flex';
      document.getElementById('config-body').innerHTML = '<div style="height:1px"></div>';
      document.getElementById('config-body').scrollTop = 0;
      return [
        distanceFor('ALIASES', 20, 320),
        distanceFor('BACKEND', 80, 40),
        distanceFor('KNOWLEDGE', 160, 760),
        distanceFor('COMPLIANCE', 240, 520),
      ];
    })()`;
    const result = await send('Runtime.evaluate', { expression, returnByValue: true });
    if (result.result.exceptionDetails) throw new Error(result.result.exceptionDetails.exception.description);
    const failures = result.result.result.value.filter((row) => !row.ok);
    console.log(JSON.stringify(result.result.result.value, null, 2));
    if (failures.length) throw new Error('Tooltip too far from trigger: ' + JSON.stringify(failures));
    socket.close();
  } finally {
    await cleanup();
  }
}

main().catch((err) => {
  console.error(err.message || err);
  process.exit(1);
});
