'use strict';

// Leveled local audit for the contributor relay (hivecommons/hive#13456,
// design in #13441 §3). Capture only: nothing here uploads anything.
//
// relay-activity.jsonl used to hold one row shape — a terminal task attempt.
// It now holds three classes of row, written through the same atomic-rewrite /
// 0600 file / 0700 directory / retention path:
//
//   terminal   the existing #13293 rows (`outcome`, no `event`), written at
//              every level, `off` included, so `hivectl hives activity` keeps
//              its counts.
//   lifecycle  `event` ∈ LIFECYCLE_EVENTS, written at `lifecycle` and above.
//   commands   `event: "command"`, one per GitHub-touching command the agent
//              ran, written at `commands` and above by the PATH shim
//              (relay-audit-shim.js); `reason`/`reason_source` at `reasoning`.
//
// Each level is a strict superset of the one below. Redaction happens before a
// row is built, at every level. Prompts, task payloads, pane text, GitHub
// tokens, MCP config and registration tokens are never written.
//
// Pure helpers only (plus the shim's spool append), so the relay, the shim and
// the tests share one implementation.

const fs = require('fs');
const path = require('path');

const AUDIT_LEVELS = Object.freeze(['off', 'lifecycle', 'commands', 'reasoning']);
const AUDIT_DEFAULT_LEVEL = 'lifecycle';
const LIFECYCLE_EVENTS = Object.freeze([
  'assigned', 'declined', 'token_received', 'token_renewed', 'mcp_configured',
  'pr_opened', 'revoked', 'blocked_on_human', 'stash_leftover',
]);
const COMMAND_EVENT = 'command';
const REASON_SOURCES = Object.freeze(['transcript', 'tool_description', 'hook', 'unavailable']);
// Decision Q5 on #13456: one preceding assistant turn, size-capped.
const REASON_MAX_BYTES = 2048;
const TRUNCATION_MARKER = ' …[truncated]';
const CMD_MAX_BYTES = 4096;
const FIELD_MAX_BYTES = 512;
const REDACTED = '[redacted]';

function normalizeAuditLevel(value) {
  const v = String(value == null ? '' : value).trim().toLowerCase();
  return AUDIT_LEVELS.includes(v) ? v : null;
}

function auditLevelRank(level) {
  const i = AUDIT_LEVELS.indexOf(normalizeAuditLevel(level) || AUDIT_DEFAULT_LEVEL);
  return i;
}

function auditLevelAtLeast(level, min) {
  return auditLevelRank(level) >= auditLevelRank(min);
}

// HIVE_RELAY_AUDIT_LEVEL holds either one level for every hive or, as
// `hivectl hives set <name> --audit-level` projects it, one entry per HIVE_HUB
// entry (an empty slot is the default). A malformed value never stops the
// relay: it falls back to the default and reports why.
function auditLevelsFromEnv(raw, count) {
  const fallback = Array(count).fill(AUDIT_DEFAULT_LEVEL);
  const text = String(raw == null ? '' : raw).trim().replace(/^(['"])(.*)\1$/, '$2');
  if (!text) return { levels: fallback };
  const parts = text.split(',').map(s => s.trim());
  if (parts.length === 1) {
    const level = normalizeAuditLevel(parts[0]);
    if (level) return { levels: Array(count).fill(level) };
    return { levels: fallback, error: `HIVE_RELAY_AUDIT_LEVEL=${parts[0]} is not one of ${AUDIT_LEVELS.join('|')}; using ${AUDIT_DEFAULT_LEVEL}` };
  }
  if (parts.length !== count) {
    return { levels: fallback, error: `HIVE_RELAY_AUDIT_LEVEL lists ${parts.length} level(s) for ${count} hive(s); using ${AUDIT_DEFAULT_LEVEL}` };
  }
  const levels = parts.map(p => (p === '' ? AUDIT_DEFAULT_LEVEL : normalizeAuditLevel(p)));
  if (levels.some(l => !l)) {
    return { levels: levels.map(l => l || AUDIT_DEFAULT_LEVEL), error: `HIVE_RELAY_AUDIT_LEVEL has an entry that is not one of ${AUDIT_LEVELS.join('|')}; using ${AUDIT_DEFAULT_LEVEL} for it` };
  }
  return { levels };
}

// ── Redaction ───────────────────────────────────────────────────────────────

// The relay's RELAY_SECRET_PATTERNS cross a process boundary to reach the
// shim, so they travel as {category, source, flags}.
function serializeSecretPatterns(patterns) {
  return (patterns || []).map(({ category, pattern }) => ({ category, source: pattern.source, flags: pattern.flags }));
}

function compileSecretPatterns(list) {
  const out = [];
  for (const p of Array.isArray(list) ? list : []) {
    try {
      if (p && typeof p.source === 'string') {
        out.push({ category: String(p.category || ''), pattern: new RegExp(p.source, String(p.flags || 'g')) });
      }
    } catch (_) {}
  }
  return out;
}

function truncateBytes(text, maxBytes) {
  const s = String(text);
  if (Buffer.byteLength(s, 'utf8') <= maxBytes) return s;
  const room = Math.max(0, maxBytes - Buffer.byteLength(TRUNCATION_MARKER, 'utf8'));
  return Buffer.from(s, 'utf8').subarray(0, room).toString('utf8').replace(/\uFFFD+$/, '') + TRUNCATION_MARKER;
}

// redactAuditText replaces credential-bearing values inside one string:
// Authorization headers, GH_TOKEN=/GITHUB_TOKEN= assignments, --token values,
// x-access-token (and any user:password) URLs, then every secret pattern the
// relay already scrubs from pane output.
function redactAuditText(text, patterns) {
  let out = String(text == null ? '' : text);
  out = out.replace(/(authorization["']?\s*[:=]\s*)[^\n]*/gi, `$1${REDACTED}`);
  out = out.replace(/\b((?:GH|GITHUB|GH_ENTERPRISE|GITHUB_ENTERPRISE)_TOKEN\s*=\s*)("[^"]*"|'[^']*'|[^\s"']+)/g, `$1${REDACTED}`);
  out = out.replace(/(--token(?:=|\s+))("[^"]*"|'[^']*'|\S+)/gi, `$1${REDACTED}`);
  out = out.replace(/(x-access-token:)[^@\s/]*@/gi, `$1${REDACTED}@`);
  out = out.replace(/(:\/\/[^/\s:@]+:)(?!\[redacted\]@)[^@\s/]+@/g, `$1${REDACTED}@`);
  for (const { category, pattern } of patterns || []) {
    out = out.replace(pattern, category === 'api-key' ? `$1${REDACTED}` : REDACTED);
  }
  return out;
}

// redactAuditArgs redacts an argv. Value-taking flags are handled per element
// so `--token X` loses X even though X alone matches no pattern; `gh secret
// set --body` values are secrets by definition.
function redactAuditArgs(op, args, patterns) {
  const list = (args || []).map(a => String(a));
  const secretCmd = op === 'gh' && list[0] === 'secret';
  const out = [];
  let redactNext = false;
  for (const a of list) {
    if (redactNext) { out.push(REDACTED); redactNext = false; continue; }
    if (/^--token$/i.test(a) || (secretCmd && /^(--body|-b)$/.test(a))) {
      out.push(a);
      redactNext = true;
      continue;
    }
    if (secretCmd && /^(--body=|-b=?).+/s.test(a)) {
      out.push(a.replace(/^(--body=|-b=?).*$/s, `$1${REDACTED}`));
      continue;
    }
    out.push(redactAuditText(a, patterns));
  }
  return out;
}

// ── Which commands are GitHub-touching ──────────────────────────────────────

const GIT_GLOBAL_WITH_VALUE = new Set(['-C', '-c', '--git-dir', '--work-tree', '--namespace', '--exec-path', '--config-env', '--super-prefix']);
// Decision Q6 on #13456: `commands` is about writes (git push, gh); fetch,
// clone, ls-remote (and pull, which fetches) are recorded only at `reasoning`.
const GIT_WRITE_SUBCOMMANDS = new Set(['push']);
const GIT_READ_SUBCOMMANDS = new Set(['fetch', 'clone', 'ls-remote', 'pull']);
const GH_LOCAL_ONLY = new Set(['--version', 'version', '--help', '-h', 'help', 'completion']);
const GIT_PUSH_OPTS_WITH_VALUE = new Set(['-o', '--push-option', '--repo', '--receive-pack', '--exec']);

function gitSubcommand(args) {
  const list = args || [];
  for (let i = 0; i < list.length; i++) {
    const a = String(list[i]);
    if (GIT_GLOBAL_WITH_VALUE.has(a)) { i++; continue; }
    if (a.startsWith('-')) continue;
    return { sub: a, index: i };
  }
  return null;
}

// auditCommandMinLevel is the lowest level that records this invocation, or
// null when it does not touch GitHub at all.
function auditCommandMinLevel(op, args) {
  const list = args || [];
  if (op === 'gh') {
    if (list.length === 0 || GH_LOCAL_ONLY.has(String(list[0]))) return null;
    return 'commands';
  }
  if (op === 'git') {
    const sub = gitSubcommand(list);
    if (!sub) return null;
    if (GIT_WRITE_SUBCOMMANDS.has(sub.sub)) return 'commands';
    if (GIT_READ_SUBCOMMANDS.has(sub.sub)) return 'reasoning';
  }
  return null;
}

function gitRemoteAndRefspec(args) {
  const sub = gitSubcommand(args);
  if (!sub) return {};
  const positionals = [];
  for (let i = sub.index + 1; i < args.length; i++) {
    const a = String(args[i]);
    if (GIT_PUSH_OPTS_WITH_VALUE.has(a)) { i++; continue; }
    if (a === '--') continue;
    if (a.startsWith('-')) continue;
    positionals.push(a);
  }
  const out = {};
  if (positionals.length > 0) out.remote = positionals[0];
  if (positionals.length > 1) out.refspec = positionals.slice(1).join(' ');
  return out;
}

function relativeCwd(cwd, base) {
  if (!cwd) return undefined;
  if (!base) return undefined;
  const rel = path.relative(base, cwd);
  return rel === '' ? '.' : rel;
}

// buildCommandRow builds the redacted row for one finished command. The row
// shape follows bin/gh-wrapper.sh's token-access.jsonl line (`ts`, `op`,
// `cmd`) so one parser serves both logs.
function buildCommandRow({ op, args, patterns, state, startedAt, durationMs, exit, cwd, pid }) {
  const minLevel = auditCommandMinLevel(op, args);
  if (!minLevel) return null;
  const redacted = redactAuditArgs(op, args, patterns);
  const row = {
    v: 2,
    level: minLevel,
    event: COMMAND_EVENT,
    ts: new Date(startedAt).toISOString(),
    task_id: state && state.task_id != null ? String(state.task_id) : undefined,
    task_gen: state && state.task_gen != null ? state.task_gen : undefined,
    op,
    cmd: truncateBytes([op, ...redacted].join(' '), CMD_MAX_BYTES),
    cwd: relativeCwd(cwd, state && state.checkout),
    exit: Number.isInteger(exit) ? exit : null,
    duration_ms: Math.max(0, Math.round(Number(durationMs) || 0)),
    pid: Number.isInteger(pid) ? pid : undefined,
    capture: 'shim',
  };
  if (op === 'git') {
    const { remote, refspec } = gitRemoteAndRefspec(args);
    if (remote) row.remote = truncateBytes(redactAuditText(remote, patterns), FIELD_MAX_BYTES);
    if (refspec) row.refspec = truncateBytes(redactAuditText(refspec, patterns), FIELD_MAX_BYTES);
  }
  return row;
}

// ── Shim side ───────────────────────────────────────────────────────────────

function isExecutable(file) {
  try {
    const st = fs.statSync(file);
    if (!st.isFile()) return false;
    fs.accessSync(file, fs.constants.X_OK);
    return true;
  } catch (_) { return false; }
}

// resolveRealBinary finds the binary the shim stands in for: the path the
// relay resolved when it installed the shim, else the first match on PATH
// that is not the shim directory itself.
function resolveRealBinary(op, state, envPath, shimDir) {
  const own = shimDir ? path.resolve(shimDir) : '';
  const recorded = state && state.real && state.real[op];
  if (recorded && path.resolve(path.dirname(recorded)) !== own && isExecutable(recorded)) return recorded;
  for (const dir of String(envPath || '').split(path.delimiter)) {
    if (!dir || path.resolve(dir) === own) continue;
    const candidate = path.join(dir, op);
    if (isExecutable(candidate)) return candidate;
  }
  return '';
}

// recordShimCommand appends one redacted row to the spool the relay drains.
// Gated on the CURRENT task's level, read from the state file the relay keeps:
// with no task, or a level below what this command needs, nothing is written.
// Never throws — a logging failure must not change the command's outcome.
function recordShimCommand({ state, op, args, startedAt, durationMs, exit, cwd, pid }) {
  try {
    if (!state || state.task_id == null || !state.spool) return false;
    const minLevel = auditCommandMinLevel(op, args);
    if (!minLevel || !auditLevelAtLeast(state.level, minLevel) || !auditLevelAtLeast(state.level, 'commands')) return false;
    const row = buildCommandRow({
      op, args, patterns: compileSecretPatterns(state.secret_patterns), state, startedAt, durationMs, exit, cwd, pid,
    });
    if (!row) return false;
    fs.appendFileSync(state.spool, JSON.stringify(row) + '\n', { mode: 0o600 });
    return true;
  } catch (_) {
    return false;
  }
}

// ── Reasoning: bind a command to the preceding assistant turn ───────────────

function entryTime(obj) {
  const t = Date.parse(obj && obj.timestamp);
  return Number.isFinite(t) ? t : NaN;
}

// parseTranscriptEntries normalises the tail of a CLI session file into
// assistant text turns and tool calls, oldest first. User turns, tool output
// and everything else are dropped here, so they cannot reach a row.
function parseTranscriptEntries(backend, lines) {
  const out = [];
  for (const line of lines || []) {
    let obj = line;
    if (typeof line === 'string') {
      const trimmed = line.trim();
      if (!trimmed) continue;
      try { obj = JSON.parse(trimmed); } catch (_) { continue; }
    }
    if (!obj || typeof obj !== 'object') continue;
    const ts = entryTime(obj);
    if (backend === 'claude' || backend === 'litellm') {
      if (obj.type !== 'assistant' || !obj.message || !Array.isArray(obj.message.content)) continue;
      for (const block of obj.message.content) {
        if (!block || typeof block !== 'object') continue;
        if (block.type === 'text' && typeof block.text === 'string') out.push({ kind: 'text', text: block.text, ts });
        if (block.type === 'tool_use') {
          const input = block.input || {};
          out.push({ kind: 'tool', id: String(block.id || ''), command: typeof input.command === 'string' ? input.command : '', description: typeof input.description === 'string' ? input.description : '', ts });
        }
      }
    } else if (backend === 'copilot') {
      const data = obj.data || {};
      if (obj.type === 'assistant.message') {
        if (typeof data.content === 'string' && data.content.trim()) out.push({ kind: 'text', text: data.content, ts });
        for (const req of Array.isArray(data.toolRequests) ? data.toolRequests : []) {
          const a = (req && req.arguments) || {};
          out.push({ kind: 'tool', id: String(req.toolCallId || ''), command: typeof a.command === 'string' ? a.command : '', description: typeof a.description === 'string' ? a.description : '', ts });
        }
      } else if (obj.type === 'tool.execution_start') {
        const a = data.arguments || {};
        if (out.some(e => e.kind === 'tool' && e.id && e.id === String(data.toolCallId || ''))) continue;
        out.push({ kind: 'tool', id: String(data.toolCallId || ''), command: typeof a.command === 'string' ? a.command : '', description: typeof a.description === 'string' ? a.description : '', ts });
      }
    }
  }
  return out;
}

function commandNeedle(row) {
  const parts = String(row.cmd || '').split(/\s+/).filter(Boolean);
  if (row.op === 'git') {
    const sub = gitSubcommand(parts.slice(1));
    return sub ? `git ${sub.sub}` : 'git';
  }
  return parts.slice(0, 2).join(' ');
}

// Clock skew allowance between the shim's start time and the transcript's
// record of the tool call that launched it.
const TRANSCRIPT_SKEW_MS = 5000;

// bindCommandReason reconciles a shim row with the transcript (tool_call_id)
// and, when includeReason, attaches the nearest preceding assistant turn —
// exactly one turn, at most REASON_MAX_BYTES, redacted.
function bindCommandReason(entries, row, { includeReason, patterns } = {}) {
  const result = {};
  const t = Date.parse(row.ts);
  const eligible = (e) => !Number.isFinite(t) || !Number.isFinite(e.ts) || e.ts <= t + TRANSCRIPT_SKEW_MS;
  const needle = commandNeedle(row);
  let limit = 0;
  for (let i = 0; i < (entries || []).length; i++) if (eligible(entries[i])) limit = i + 1;
  let callIdx = -1;
  for (let i = limit - 1; i >= 0; i--) {
    const e = entries[i];
    if (e.kind === 'tool' && needle && e.command.replace(/\s+/g, ' ').includes(needle)) { callIdx = i; break; }
  }
  if (callIdx >= 0) {
    if (entries[callIdx].id) result.tool_call_id = entries[callIdx].id;
    result.capture = 'shim+transcript';
  }
  if (!includeReason) return result;
  let text = '';
  for (let i = (callIdx >= 0 ? callIdx : limit) - 1; i >= 0; i--) {
    if (entries[i].kind === 'text' && entries[i].text.trim()) { text = entries[i].text.trim(); break; }
  }
  if (text) {
    result.reason = truncateBytes(redactAuditText(text, patterns), REASON_MAX_BYTES);
    result.reason_source = 'transcript';
  } else if (callIdx >= 0 && entries[callIdx].description.trim()) {
    result.reason = truncateBytes(redactAuditText(entries[callIdx].description.trim(), patterns), REASON_MAX_BYTES);
    result.reason_source = 'tool_description';
  } else {
    result.reason_source = 'unavailable';
  }
  return result;
}

// ── Store pruning ───────────────────────────────────────────────────────────

function auditRowClass(r) {
  if (r && typeof r.event === 'string') return r.event === COMMAND_EVENT ? 'commands' : 'lifecycle';
  return 'terminal';
}

function auditRowTime(r) {
  return Date.parse(auditRowClass(r) === 'terminal' ? r.ended_at : r.ts);
}

// isAuditRow accepts the rows this store owns: a terminal row carries `key`
// (as in #13293), a leveled row carries `event`.
function isAuditRow(r) {
  return !!r && typeof r === 'object' && (typeof r.key === 'string' || typeof r.event === 'string');
}

// pruneAuditRows applies retention, then a separate cap per class so chatty
// command rows can never evict lifecycle history or terminal attempts:
// maxTerminal and maxLifecycle count rows, maxCommandBytes bounds the command
// rows' serialized size. The newest rows win, order is preserved.
function pruneAuditRows(rows, { now = Date.now(), retentionDays, maxTerminal, maxLifecycle, maxCommandBytes }) {
  const cutoff = now - retentionDays * 24 * 60 * 60 * 1000;
  const kept = rows.filter(r => {
    const t = auditRowTime(r);
    return !Number.isFinite(t) || t >= cutoff;
  });
  const keep = new Array(kept.length).fill(true);
  let terminal = 0, lifecycle = 0, commandBytes = 0;
  for (let i = kept.length - 1; i >= 0; i--) {
    const cls = auditRowClass(kept[i]);
    if (cls === 'terminal') keep[i] = ++terminal <= maxTerminal;
    else if (cls === 'lifecycle') keep[i] = ++lifecycle <= maxLifecycle;
    else {
      commandBytes += Buffer.byteLength(JSON.stringify(kept[i]), 'utf8') + 1;
      keep[i] = commandBytes <= maxCommandBytes;
    }
  }
  return kept.filter((_, i) => keep[i]);
}

module.exports = {
  AUDIT_LEVELS,
  AUDIT_DEFAULT_LEVEL,
  LIFECYCLE_EVENTS,
  COMMAND_EVENT,
  REASON_SOURCES,
  REASON_MAX_BYTES,
  TRUNCATION_MARKER,
  REDACTED,
  normalizeAuditLevel,
  auditLevelRank,
  auditLevelAtLeast,
  auditLevelsFromEnv,
  serializeSecretPatterns,
  compileSecretPatterns,
  truncateBytes,
  redactAuditText,
  redactAuditArgs,
  gitSubcommand,
  auditCommandMinLevel,
  gitRemoteAndRefspec,
  buildCommandRow,
  resolveRealBinary,
  recordShimCommand,
  parseTranscriptEntries,
  bindCommandReason,
  auditRowClass,
  isAuditRow,
  pruneAuditRows,
};
