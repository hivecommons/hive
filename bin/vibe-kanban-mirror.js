#!/usr/bin/env node
'use strict';

// Report-only hive → vibe-kanban mirror (hivecommons/hive#10641, Phase 0).
//
// vibe-kanban (https://github.com/BloopAI/vibe-kanban) is a local kanban board
// for coding-agent CLIs. It has no issue intake, triage or admission; hive has
// no per-developer board. This side-car shows hive's already-admitted work for
// ONE repository on a vibe-kanban project so a developer can see it there.
//
// Direction is strictly one way. Hive stays the owner of admission and
// acceptance; the board is a view:
//
//   read   GET <hive>/api/contribute/queue   the ready-work queue (public, the
//                                            same set selectTask offers from;
//                                            held rows are operator-parked)
//          GET <hive>/api/contribute/fleet   in-flight leases (public)
//   write  vibe-kanban's MCP stdio server    create_issue / update_issue /
//          (`npx -y vibe-kanban@latest --mcp`) add_issue_tag
//
// Nothing is ever written back to hive, and nothing touches vibe-kanban's
// SQLite or its undocumented HTTP routes: the MCP server is the only surface
// upstream documents. vibe-kanban's Issue has no external-id column, so the
// idempotency key is the hive work key carried in a description trailer,
// `[hive-work-key: owner/repo#42]`, and found again with list_issues' search.
// The brackets make the match exact (`#4]` never matches `#42]`).
//
// Default OFF. It does nothing unless both VIBE_KANBAN_MCP_CMD and
// VIBE_KANBAN_PROJECT_ID are set. No credentials: both hive endpoints are
// public reads and the MCP server is local.
//
// Phase 1 (opt-in, VIBE_KANBAN_SHADOW_LOG): a developer picking a queued hive
// item up on the board (start_workspace moves the card forward, or a PR gets
// linked to it) is recorded as an `ext_work_shadow_observed` event in the
// extwork ProgressEvent shape, appended to a local JSONL file. Observation is
// read-only: hive dispatches nothing, the board is not changed, and a
// picked-up card is no longer pulled back to To do. Acceptance still goes
// through hive's own receipt/PR path.
//
// Run: node bin/vibe-kanban-mirror.js --once [--dry-run]
// Docs: src/docs/vibe-kanban.md

const fs = require('fs');
const { spawn } = require('child_process');

// Hive work state → vibe-kanban status key (the upstream TaskStatus spelling).
const HIVE_TO_BOARD = Object.freeze({
  queued: 'todo',
  leased: 'inprogress',
  review: 'inreview',
  merged: 'done',
  closed: 'done',
  parked: 'cancelled',
});

// Status key → project status NAME. update_issue takes the name (matched
// case-insensitively); these are the statuses a new vibe-kanban project is
// seeded with. VIBE_KANBAN_STATUS_NAMES overrides any of them for a project
// whose columns were renamed.
const DEFAULT_STATUS_NAMES = Object.freeze({
  todo: 'To do',
  inprogress: 'In progress',
  inreview: 'In review',
  done: 'Done',
  cancelled: 'Cancelled',
});

const HIVE_TAG = 'hive';
const BOARD_HOST = 'vibe-kanban';

// Mirrors pkg/extwork: the audit action for a shadow observation and the
// engine-neutral states an observation may report.
const EVENT_SHADOW_OBSERVED = 'ext_work_shadow_observed';
const STATE_RUNNING = 'running';
const STATE_WAITING = 'waiting';
const STATE_TERMINAL = 'terminal';

// Board status key → extwork state for a board-side pick-up.
const PICKUP_STATES = Object.freeze({
  inprogress: STATE_RUNNING,
  inreview: STATE_WAITING,
  done: STATE_TERMINAL,
});
const DEFAULT_INTERVAL_S = 300;
const MCP_PROTOCOL_VERSION = '2024-11-05';

const USAGE = `Usage: node bin/vibe-kanban-mirror.js [--once] [--dry-run] [options]

Mirror hive's triaged queue for one repository onto a vibe-kanban project
(report-only: nothing is written back to hive).

Options:
  --once               run a single pass and exit (default: loop)
  --dry-run            read hive and the board, print the plan, change nothing
  --repo owner/name    repository to mirror            (VIBE_KANBAN_REPO)
  --hive-url URL       hive dashboard base URL         (HIVE_DASHBOARD_URL)
  --project-id UUID    vibe-kanban project             (VIBE_KANBAN_PROJECT_ID)
  --mcp-cmd CMD        vibe-kanban MCP server command  (VIBE_KANBAN_MCP_CMD)
  --interval SECONDS   loop interval, default ${DEFAULT_INTERVAL_S}  (VIBE_KANBAN_SYNC_INTERVAL_S)
  --shadow-log PATH    record board pick-ups of queued items as shadow
                       executions in this JSONL file (VIBE_KANBAN_SHADOW_LOG)
  -h, --help           show this help

The mirror is off unless VIBE_KANBAN_MCP_CMD and VIBE_KANBAN_PROJECT_ID are set.
VIBE_KANBAN_STATUS_NAMES (JSON, e.g. {"todo":"Backlog"}) renames board columns.`;

function parseArgs(argv, env = process.env) {
  const opts = {
    once: false,
    dryRun: false,
    help: false,
    repo: env.VIBE_KANBAN_REPO || '',
    hiveUrl: env.HIVE_DASHBOARD_URL || '',
    projectId: env.VIBE_KANBAN_PROJECT_ID || '',
    mcpCmd: env.VIBE_KANBAN_MCP_CMD || '',
    intervalS: Number(env.VIBE_KANBAN_SYNC_INTERVAL_S) || DEFAULT_INTERVAL_S,
    shadowLog: env.VIBE_KANBAN_SHADOW_LOG || '',
    statusNames: { ...DEFAULT_STATUS_NAMES },
  };
  if (env.VIBE_KANBAN_STATUS_NAMES) {
    let override;
    try {
      override = JSON.parse(env.VIBE_KANBAN_STATUS_NAMES);
    } catch (err) {
      throw new Error(`VIBE_KANBAN_STATUS_NAMES is not valid JSON: ${err.message}`);
    }
    for (const [key, name] of Object.entries(override || {})) {
      if (!(key in DEFAULT_STATUS_NAMES)) {
        throw new Error(`VIBE_KANBAN_STATUS_NAMES: unknown status key "${key}" (expected one of ${Object.keys(DEFAULT_STATUS_NAMES).join(', ')})`);
      }
      if (typeof name === 'string' && name.trim()) opts.statusNames[key] = name.trim();
    }
  }
  const value = (i, flag) => {
    if (i + 1 >= argv.length) throw new Error(`${flag} needs a value`);
    return argv[i + 1];
  };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    switch (arg) {
      case '--once': opts.once = true; break;
      case '--dry-run': opts.dryRun = true; break;
      case '-h':
      case '--help': opts.help = true; break;
      case '--repo': opts.repo = value(i, arg); i++; break;
      case '--hive-url': opts.hiveUrl = value(i, arg); i++; break;
      case '--project-id': opts.projectId = value(i, arg); i++; break;
      case '--mcp-cmd': opts.mcpCmd = value(i, arg); i++; break;
      case '--shadow-log': opts.shadowLog = value(i, arg); i++; break;
      case '--interval': {
        const n = Number(value(i, arg));
        if (!Number.isFinite(n) || n <= 0) throw new Error('--interval must be a positive number of seconds');
        opts.intervalS = n;
        i++;
        break;
      }
      default:
        throw new Error(`unknown argument: ${arg}`);
    }
  }
  return opts;
}

// Splits VIBE_KANBAN_MCP_CMD into argv without a shell: whitespace separates
// words, single or double quotes group them. No expansion of any kind.
function splitCommand(command) {
  const words = [];
  let cur = '';
  let quote = '';
  let inWord = false;
  for (const ch of String(command || '')) {
    if (quote) {
      if (ch === quote) quote = '';
      else cur += ch;
    } else if (ch === '"' || ch === "'") {
      quote = ch;
      inWord = true;
    } else if (/\s/.test(ch)) {
      if (inWord) words.push(cur);
      cur = '';
      inWord = false;
    } else {
      cur += ch;
      inWord = true;
    }
  }
  if (quote) throw new Error('unterminated quote in MCP command');
  if (inWord) words.push(cur);
  return words;
}

function boardStatusKey(state) {
  const key = HIVE_TO_BOARD[String(state || '').toLowerCase()];
  if (!key) throw new Error(`unknown hive work state "${state}"`);
  return key;
}

// Board status NAME → status key, the inverse of statusNames. Returns '' for a
// column the mirror does not know (for example Backlog).
function statusKeyForName(name, statusNames = DEFAULT_STATUS_NAMES) {
  const want = String(name || '').toLowerCase();
  if (!want) return '';
  for (const [key, n] of Object.entries(statusNames)) {
    if (String(n).toLowerCase() === want) return key;
  }
  return '';
}

function workKey(item) {
  if (item && item.key) return String(item.key);
  if (item && item.repo && item.number) return `${item.repo}#${item.number}`;
  return '';
}

function trailer(key) {
  return `[hive-work-key: ${key}]`;
}

function canonicalUrl(item) {
  if (item.url) return item.url;
  const github = !item.source_type || item.source_type === 'github';
  if (github && item.repo && item.number) return `https://github.com/${item.repo}/issues/${item.number}`;
  return '';
}

function boardTitle(item) {
  return (item.title && String(item.title).trim()) || item.key;
}

function boardDescription(item) {
  const lines = [];
  if (item.url) lines.push(item.url, '');
  lines.push(
    'Mirrored from hive (report-only). Hive owns admission and acceptance; changes made on this board are not written back.',
    '',
    trailer(item.key),
  );
  return lines.join('\n');
}

function sameRepo(a, b) {
  return String(a || '').toLowerCase() === String(b || '').toLowerCase();
}

async function fetchJSON(fetchImpl, url) {
  const res = await fetchImpl(url, { headers: { accept: 'application/json' } });
  if (!res.ok) throw new Error(`GET ${url} returned ${res.status}`);
  return res.json();
}

// Reads hive's admitted work for one repo through the existing public read
// path. A queued row is `queued`, or `parked` when the operator holds it; a
// fleet lease wins over the queue because it is the newer fact.
async function readHiveItems({ hiveUrl, repo, fetchImpl = globalThis.fetch }) {
  const base = String(hiveUrl).replace(/\/+$/, '');
  const [queue, fleet] = await Promise.all([
    fetchJSON(fetchImpl, `${base}/api/contribute/queue`),
    fetchJSON(fetchImpl, `${base}/api/contribute/fleet`),
  ]);
  const items = new Map();
  for (const q of (queue && queue.queue) || []) {
    if (!sameRepo(q.repo, repo)) continue;
    const key = workKey(q);
    if (!key) continue;
    items.set(key, {
      key,
      repo: q.repo,
      number: q.number || 0,
      title: q.title || '',
      url: canonicalUrl(q),
      state: q.held ? 'parked' : 'queued',
    });
  }
  for (const w of (fleet && fleet.work) || []) {
    if (!sameRepo(w.repo, repo) || !w.number) continue;
    if (w.kind && w.kind !== 'issue') continue;
    const key = workKey({ repo: w.repo, number: w.number });
    const prev = items.get(key) || {};
    items.set(key, {
      key,
      repo: w.repo,
      number: w.number,
      title: w.title || prev.title || '',
      url: prev.url || canonicalUrl(w),
      state: 'leased',
      taskId: w.task_id || '',
    });
  }
  return [...items.values()].sort((a, b) => a.key.localeCompare(b.key));
}

// Minimal MCP client over newline-delimited JSON-RPC on a child's stdio.
class McpStdioClient {
  constructor(command, { spawnImpl = spawn, env = process.env, timeoutMs = 60000 } = {}) {
    const argv = splitCommand(command);
    if (!argv.length) throw new Error('MCP command is empty');
    this.timeoutMs = timeoutMs;
    this.nextId = 1;
    this.pending = new Map();
    this.buf = '';
    this.stderr = '';
    this.exited = false;
    this.child = spawnImpl(argv[0], argv.slice(1), { stdio: ['pipe', 'pipe', 'pipe'], env });
    this.exitPromise = new Promise((resolve) => {
      this.child.on('exit', (code, signal) => {
        this.exited = true;
        const tail = this.stderr.trim().split('\n').pop();
        this._failAll(new Error(`MCP server exited (code ${code}, signal ${signal})${tail ? `: ${tail}` : ''}`));
        resolve();
      });
    });
    this.child.on('error', (err) => this._failAll(new Error(`cannot start MCP server: ${err.message}`)));
    this.child.stdin.on('error', () => {});
    this.child.stdout.setEncoding('utf8');
    this.child.stdout.on('data', (chunk) => this._onData(chunk));
    this.child.stderr.setEncoding('utf8');
    this.child.stderr.on('data', (chunk) => {
      this.stderr = (this.stderr + chunk).slice(-4000);
    });
  }

  _write(msg) {
    if (this.exited) throw new Error('MCP server is not running');
    this.child.stdin.write(`${JSON.stringify(msg)}\n`);
  }

  _failAll(err) {
    for (const p of this.pending.values()) {
      clearTimeout(p.timer);
      p.reject(err);
    }
    this.pending.clear();
  }

  _onData(chunk) {
    this.buf += chunk;
    let nl;
    while ((nl = this.buf.indexOf('\n')) >= 0) {
      const line = this.buf.slice(0, nl).trim();
      this.buf = this.buf.slice(nl + 1);
      if (!line) continue;
      let msg;
      try {
        msg = JSON.parse(line);
      } catch {
        continue;
      }
      if (msg.method) {
        // A server-initiated request. Answer ping; refuse anything else so the
        // server is never left waiting on us.
        if (msg.id !== undefined && msg.id !== null) {
          const reply = msg.method === 'ping'
            ? { jsonrpc: '2.0', id: msg.id, result: {} }
            : { jsonrpc: '2.0', id: msg.id, error: { code: -32601, message: `method not supported: ${msg.method}` } };
          try { this._write(reply); } catch { /* server gone; exit handler reports it */ }
        }
        continue;
      }
      const p = this.pending.get(msg.id);
      if (!p) continue;
      this.pending.delete(msg.id);
      clearTimeout(p.timer);
      if (msg.error) p.reject(new Error(`MCP ${p.method} failed: ${msg.error.message || JSON.stringify(msg.error)}`));
      else p.resolve(msg.result);
    }
  }

  request(method, params) {
    return new Promise((resolve, reject) => {
      const id = this.nextId++;
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`MCP ${method} timed out after ${this.timeoutMs}ms`));
      }, this.timeoutMs);
      if (timer.unref) timer.unref();
      this.pending.set(id, { resolve, reject, timer, method });
      try {
        this._write({ jsonrpc: '2.0', id, method, params });
      } catch (err) {
        this.pending.delete(id);
        clearTimeout(timer);
        reject(err);
      }
    });
  }

  async initialize() {
    await this.request('initialize', {
      protocolVersion: MCP_PROTOCOL_VERSION,
      capabilities: {},
      clientInfo: { name: 'hive-vibe-kanban-mirror', version: '1' },
    });
    this._write({ jsonrpc: '2.0', method: 'notifications/initialized' });
  }

  async callTool(name, args) {
    const res = (await this.request('tools/call', { name, arguments: args })) || {};
    const text = (res.content || []).filter((c) => c && c.type === 'text').map((c) => c.text).join('\n');
    let data = res.structuredContent;
    if (data === undefined) {
      try {
        data = JSON.parse(text);
      } catch {
        data = text;
      }
    }
    if (res.isError) {
      const detail = data && typeof data === 'object' && data.error
        ? `${data.error}${data.details ? ` (${data.details})` : ''}`
        : text;
      throw new Error(`${name}: ${detail || 'tool error'}`);
    }
    return data;
  }

  async close() {
    if (this.exited) return;
    try { this.child.stdin.end(); } catch { /* already closed */ }
    const timer = setTimeout(() => {
      if (!this.exited) this.child.kill('SIGTERM');
    }, 2000);
    if (timer.unref) timer.unref();
    await this.exitPromise;
    clearTimeout(timer);
  }
}

// Decides whether a board issue shows a developer picking up a hive item that
// hive itself has not dispatched. Only `queued` items qualify: a leased item is
// already hive's own execution, and a parked one is held by the operator.
// Returns null when there is nothing to observe.
function observePickup({ item, issue, projectId, statusNames = DEFAULT_STATUS_NAMES }) {
  if (!item || !issue || item.state !== 'queued') return null;
  const boardKey = statusKeyForName(issue.status, statusNames);
  const prUrl = issue.latest_pr_url || '';
  const prStatus = String(issue.latest_pr_status || '').toLowerCase();
  let state = PICKUP_STATES[boardKey] || '';
  if (prStatus === 'merged' || prStatus === 'closed') state = STATE_TERMINAL;
  else if (prUrl && !state) state = STATE_WAITING;
  if (!state) return null;
  const fields = {
    external_start: false,
    host: BOARD_HOST,
    work_key: item.key,
    board_issue_id: issue.id,
    board_status: issue.status || '',
  };
  if (issue.simple_id) fields.simple_id = issue.simple_id;
  if (prUrl) fields.latest_pr_url = prUrl;
  if (prStatus) fields.latest_pr_status = prStatus;
  return {
    action: EVENT_SHADOW_OBSERVED,
    execution_key: `${BOARD_HOST}:${projectId}/${issue.id}`,
    assignment_id: item.taskId || '',
    state,
    fields,
  };
}

// What makes two observations of the same execution the same fact.
function observationFingerprint(ev) {
  const f = ev.fields || {};
  return JSON.stringify([ev.state, f.board_status || '', f.latest_pr_url || '', f.latest_pr_status || '']);
}

// Appends shadow observations to a local JSONL file, one extwork
// ProgressEvent per line plus `observed_at`. It remembers the last fact per
// execution key (read back from the file on start), so an unchanged pick-up is
// recorded once, not on every pass.
class ShadowLog {
  constructor(filePath, { now = () => new Date() } = {}) {
    this.filePath = filePath;
    this.now = now;
    this.last = new Map();
    let text = '';
    try {
      text = fs.readFileSync(filePath, 'utf8');
    } catch (err) {
      if (err.code !== 'ENOENT') throw new Error(`cannot read shadow log ${filePath}: ${err.message}`);
    }
    for (const line of text.split('\n')) {
      if (!line.trim()) continue;
      let ev;
      try {
        ev = JSON.parse(line);
      } catch {
        continue;
      }
      if (ev && ev.action === EVENT_SHADOW_OBSERVED && ev.execution_key) {
        this.last.set(ev.execution_key, observationFingerprint(ev));
      }
    }
  }

  isNew(ev) {
    return this.last.get(ev.execution_key) !== observationFingerprint(ev);
  }

  record(ev) {
    if (!this.isNew(ev)) return false;
    fs.appendFileSync(this.filePath, `${JSON.stringify({ ...ev, observed_at: this.now().toISOString() })}\n`);
    this.last.set(ev.execution_key, observationFingerprint(ev));
    return true;
  }
}

// One mirror pass. Idempotent: an item already on the board (found by its
// work-key trailer) is updated only when its status or title drifted.
async function syncOnce({ items, client, projectId, statusNames = DEFAULT_STATUS_NAMES, dryRun = false, shadowLog = null, log = console.log }) {
  const summary = { created: 0, updated: 0, unchanged: 0, errors: 0, observed: 0 };
  const tags = await client.callTool('list_tags', { project_id: projectId });
  const hiveTag = ((tags && tags.tags) || []).find((t) => String(t.name).toLowerCase() === HIVE_TAG);
  if (!hiveTag) {
    log(`warning: project ${projectId} has no "${HIVE_TAG}" tag; create one in vibe-kanban to tag mirrored issues`);
  }
  const verb = dryRun ? 'would ' : '';
  for (const item of items) {
    try {
      const status = statusNames[boardStatusKey(item.state)];
      const title = boardTitle(item);
      const found = await client.callTool('list_issues', { project_id: projectId, search: trailer(item.key), limit: 5 });
      const matches = (found && found.issues) || [];
      if (matches.length > 1) {
        log(`warning: ${matches.length} board issues carry ${trailer(item.key)}; updating ${matches[0].id} only`);
      }
      const existing = matches[0];
      if (!existing) {
        if (!dryRun) {
          const created = await client.callTool('create_issue', {
            project_id: projectId,
            title,
            description: boardDescription(item),
          });
          const issueId = created && created.issue_id;
          if (!issueId) throw new Error('create_issue returned no issue_id');
          await client.callTool('update_issue', { issue_id: issueId, status });
          if (hiveTag) await client.callTool('add_issue_tag', { issue_id: issueId, tag_id: hiveTag.id });
        }
        log(`${verb}create ${item.key} → ${status}`);
        summary.created++;
        continue;
      }
      const patch = {};
      const pickup = shadowLog ? observePickup({ item, issue: existing, projectId, statusNames }) : null;
      if (pickup && shadowLog.isNew(pickup)) {
        if (!dryRun) shadowLog.record(pickup);
        log(`${dryRun ? 'would record' : 'recorded'} shadow pick-up ${item.key} (${existing.id}) ${pickup.state}, board "${existing.status}"`);
        summary.observed++;
      }
      // A developer's pick-up is left on the board; anything else drifts back
      // to the hive-mapped column.
      const keepBoardStatus = pickup && PICKUP_STATES[statusKeyForName(existing.status, statusNames)];
      if (!keepBoardStatus && String(existing.status || '').toLowerCase() !== status.toLowerCase()) patch.status = status;
      if (existing.title !== title) patch.title = title;
      if (Object.keys(patch).length === 0) {
        summary.unchanged++;
        continue;
      }
      if (!dryRun) await client.callTool('update_issue', { issue_id: existing.id, ...patch });
      const changes = Object.entries(patch).map(([k, v]) => `${k}=${JSON.stringify(v)}`).join(' ');
      log(`${verb}update ${item.key} (${existing.id}) ${changes}`);
      summary.updated++;
    } catch (err) {
      summary.errors++;
      log(`error: ${item.key}: ${err.message}`);
    }
  }
  return summary;
}

async function runPass(opts, { fetchImpl = globalThis.fetch, spawnImpl = spawn, env = process.env, log = console.log } = {}) {
  const items = await readHiveItems({ hiveUrl: opts.hiveUrl, repo: opts.repo, fetchImpl });
  const shadowLog = opts.shadowLog ? new ShadowLog(opts.shadowLog) : null;
  const client = new McpStdioClient(opts.mcpCmd, { spawnImpl, env });
  try {
    await client.initialize();
    const summary = await syncOnce({
      items,
      client,
      projectId: opts.projectId,
      statusNames: opts.statusNames,
      dryRun: opts.dryRun,
      shadowLog,
      log,
    });
    const observed = shadowLog ? `, shadow pick-ups ${summary.observed}` : '';
    log(`${opts.dryRun ? 'dry-run: ' : ''}${opts.repo}: ${items.length} hive item(s); created ${summary.created}, updated ${summary.updated}, unchanged ${summary.unchanged}, errors ${summary.errors}${observed}`);
    return summary;
  } finally {
    await client.close();
  }
}

async function main(argv = process.argv.slice(2), env = process.env, deps = {}) {
  const log = deps.log || console.log;
  let opts;
  try {
    opts = parseArgs(argv, env);
  } catch (err) {
    log(`error: ${err.message}`);
    return 2;
  }
  if (opts.help) {
    log(USAGE);
    return 0;
  }
  if (!opts.mcpCmd || !opts.projectId) {
    log('vibe-kanban mirror is off: set VIBE_KANBAN_MCP_CMD and VIBE_KANBAN_PROJECT_ID to enable it');
    return 0;
  }
  if (!opts.hiveUrl) {
    log('error: set HIVE_DASHBOARD_URL or --hive-url to the hive dashboard to read from');
    return 2;
  }
  if (!/^[^/\s]+\/[^/\s]+$/.test(opts.repo)) {
    log('error: set VIBE_KANBAN_REPO or --repo to the one owner/name repository to mirror');
    return 2;
  }
  const passDeps = { ...deps, env, log };
  if (opts.once) {
    try {
      const summary = await runPass(opts, passDeps);
      return summary.errors > 0 ? 1 : 0;
    } catch (err) {
      log(`error: ${err.message}`);
      return 1;
    }
  }
  for (;;) {
    try {
      await runPass(opts, passDeps);
    } catch (err) {
      log(`error: ${err.message}`);
    }
    await new Promise((resolve) => setTimeout(resolve, opts.intervalS * 1000));
  }
}

module.exports = {
  HIVE_TO_BOARD,
  DEFAULT_STATUS_NAMES,
  EVENT_SHADOW_OBSERVED,
  McpStdioClient,
  ShadowLog,
  boardDescription,
  boardStatusKey,
  main,
  observePickup,
  parseArgs,
  readHiveItems,
  splitCommand,
  statusKeyForName,
  syncOnce,
  trailer,
};

if (require.main === module) {
  main().then((code) => process.exit(code));
}
