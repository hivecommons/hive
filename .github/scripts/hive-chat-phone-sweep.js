#!/usr/bin/env node
// Headless phone-viewport sweep of Hive Chat (hivecommons/hive#11852).
// Sends every suggestion chip, every cheat-sheet chip and every `!help`
// command through #hiveChatInput/#hiveChatSend and fails when one gets no
// reply or an error-shaped reply. Env: HIVE_URL (required), HIVE_TOKEN
// (owner dashboard token) or HIVE_SESSION (seeded hive_session cookie),
// SWEEP_OUT (screenshot dir), SWEEP_REPLY_TIMEOUT_MS.
const fs = require('fs');
const path = require('path');
const { chromium, devices } = require('playwright');

const base = (process.env.HIVE_URL || '').replace(/\/+$/, '');
const out = process.env.SWEEP_OUT || 'hive-chat-sweep';
const replyTimeout = Number(process.env.SWEEP_REPLY_TIMEOUT_MS || 30000);
const BAD = [
  [/no deterministic handler/i, 'no deterministic handler'],
  [/HTTP [45]\d\d\b/, 'HTTP 4xx/5xx in reply'],
  [/:\s*`\s*`/, 'empty-argument error'],
];

(async () => {
  if (!base) throw new Error('HIVE_URL is required');
  fs.mkdirSync(out, { recursive: true });
  const browser = await chromium.launch();
  const ctx = await browser.newContext({ ...devices['Pixel 7'] });
  if (process.env.HIVE_TOKEN) await ctx.addInitScript(t => localStorage.setItem('hive-token', t), process.env.HIVE_TOKEN);
  if (process.env.HIVE_SESSION) await ctx.addCookies([{ name: 'hive_session', value: process.env.HIVE_SESSION, url: base }]);
  const page = await ctx.newPage();
  const httpErrors = [];
  page.on('response', r => { if (new URL(r.url()).pathname.startsWith('/api/chat') && r.status() >= 400) httpErrors.push(r.status() + ' ' + r.url()); });
  await page.goto(base + '/', { waitUntil: 'domcontentloaded' });
  await page.click('#hiveChatFab');
  await page.waitForSelector('#hiveChatInput', { state: 'visible' });

  const failures = [];
  const send = async (q, i) => {
    await page.$$eval('#hiveChatMessages .chat-msg', els => els.forEach(e => { e.dataset.sweepSeen = '1'; }));
    httpErrors.length = 0;
    await page.fill('#hiveChatInput', q);
    await page.click('#hiveChatSend');
    const fresh = '#hiveChatMessages .chat-msg:not(.user):not(.peer):not(.thinking):not([data-sweep-seen])';
    let text = '';
    try {
      await page.waitForSelector(fresh, { timeout: replyTimeout });
      await page.waitForSelector('#hiveChatSend:not([disabled])', { timeout: replyTimeout });
      text = (await page.$$eval(fresh, els => els.map(e => e.innerText).join('\n'))).trim();
    } catch (_) { failures.push(`${q}: no reply within ${replyTimeout}ms`); }
    for (const [re, why] of BAD) if (re.test(text)) failures.push(`${q}: ${why}: ${text.slice(0, 200)}`);
    if (httpErrors.length) failures.push(`${q}: ${httpErrors.join(', ')}`);
    await page.screenshot({ path: path.join(out, `${String(i).padStart(2, '0')}-${q.replace(/[^a-z0-9]+/gi, '_').slice(0, 40)}.png`) });
    console.log(`--- ${q}\n${text.slice(0, 400)}`);
    return text;
  };

  const chips = await page.$$eval('[data-chat-suggestion], .hive-chat-example',
    els => els.map(e => e.getAttribute('data-chat-suggestion') || e.getAttribute('data-chat-example') || ''));
  const help = await send('!help', 0);
  const commands = [...help.matchAll(/^\s*`(![a-z][a-z-]*)/gim)].map(m => m[1].toLowerCase());
  const queue = [...new Set([...chips.filter(Boolean), ...commands])];
  if (!commands.length) failures.push('!help: listed no commands');
  for (let i = 0; i < queue.length; i++) await send(queue[i], i + 1);

  // Layout numbers are recorded only; enforcing minimums waits on #11836.
  for (const [w, h] of [[412, 839], [412, 480]]) {
    await page.setViewportSize({ width: w, height: h });
    await page.waitForTimeout(300);
    const listH = await page.$eval('#hiveChatMessages', el => el.getBoundingClientRect().height);
    console.log(`layout: #hiveChatMessages height at ${w}x${h} = ${Math.round(listH)}px`);
    await page.screenshot({ path: path.join(out, `layout-${w}x${h}.png`) });
  }
  await browser.close();
  console.log(`swept ${queue.length + 1} prompts (${chips.length} chips, ${commands.length} commands)`);
  if (failures.length) {
    failures.forEach(f => console.log(`::error::${f.replace(/\r?\n/g, ' ')}`));
    process.exit(1);
  }
})().catch(err => { console.error(err); process.exit(1); });
