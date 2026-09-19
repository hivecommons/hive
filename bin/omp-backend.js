'use strict';

// omp (Oh My Pi) contributor adapter (hivecommons/hive#7678).
//
// Container mode copies each backend's host config into an ephemeral staging
// directory and mounts THAT (see the H6 / CWE-668 note in the Justfile's
// contribute-hive recipe). There was no omp case, so `just contribute-hive
// omp` started omp as a fresh install: its first-run setup wizard, no
// provider signed in, and the relay typed the hub's task into the wizard's
// authorization-code box. This module is the omp half of that staging.
//
// Where omp keeps its state (omp 18.2.x, verified on Linux against a host
// that local mode signs in from): everything lives under ~/.omp/agent/,
// honouring PI_CODING_AGENT_DIR the way pi does. Unlike pi there is NO
// auth.json — provider credentials (OAuth records and stored API keys alike)
// are rows in the SQLite store ~/.omp/agent/agent.db, table auth_credentials
// (provider TEXT, credential_type TEXT, data TEXT ...), one row per signed-in
// provider ("anthropic", "openai-codex", "google-antigravity", ...). Settings,
// including the default model, are ~/.omp/agent/config.yml, and the model
// catalogue cache is ~/.omp/agent/models.db.
//
// What gets staged is an allowlist, not the whole directory: ~/.omp also
// holds ~360 MB of extracted native modules (natives/), the host's daemon
// sockets and pids (run/), logs, and every host conversation transcript
// (agent/sessions/). None of that belongs in a throwaway container; omp
// recreates what it needs on first launch.
//
// The credential narrowing mirrors pi-backend.js --stage: the container gets
// only the selected provider's rows, never every provider the host happens
// to be signed into. Selection is AGENT_MODEL when it is spelled
// provider/model; otherwise the providers named by config.yml's modelRoles,
// which is what omp itself will run when no model is passed.

const fs = require('fs');
const path = require('path');
const { spawnSync } = require('child_process');

// Entries copied out of ~/.omp/agent/ into <stage>/agent/. A SQLite database
// in WAL mode may hold committed rows in its -wal sidecar that have not been
// checkpointed into the main file yet, so the sidecar travels with it; the
// -shm index is per-process scratch and is rebuilt on open.
const OMP_STAGED_AGENT_ENTRIES = Object.freeze([
  'config.yml',
  'agent.db',
  'agent.db-wal',
  'models.db',
  'models.db-wal',
]);

const OMP_AUTH_TABLE = 'auth_credentials';

function ompAgentDir(ompDir, env = process.env) {
  if (ompDir) return path.join(ompDir, 'agent');
  return env.PI_CODING_AGENT_DIR || path.join(env.HOME || '', '.omp', 'agent');
}

// parseOmpProvider returns the provider id from an omp model spelling, or
// null when the spelling does not name one. omp accepts fuzzy tokens
// ("opus", "gpt-5.2") as well as the canonical provider/model[:thinking]
// form; only the canonical form says which provider's credential a run
// needs. The provider charset matches pi's so a contributor preference can
// never become SQL or shell syntax downstream.
function parseOmpProvider(raw) {
  if (typeof raw !== 'string') return null;
  const value = raw.trim();
  const slash = value.indexOf('/');
  if (slash <= 0 || slash === value.length - 1) return null;
  const provider = value.slice(0, slash);
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(provider)) return null;
  return provider.toLowerCase();
}

// configModelRoleProviders reads the providers named under `modelRoles:` in
// omp's config.yml (e.g. `default: openai-codex/gpt-5.6:max`). A deliberately
// small YAML subset: one top-level `modelRoles:` mapping whose scalar values
// are model spellings. Anything else in the file is ignored.
function configModelRoleProviders(configFile) {
  let text;
  try { text = fs.readFileSync(configFile, 'utf8'); } catch (_) { return []; }
  const providers = new Set();
  let inRoles = false;
  for (const rawLine of text.split('\n')) {
    const line = rawLine.replace(/\s+#.*$/, '').replace(/\r$/, '');
    if (line.trim() === '') continue;
    if (/^modelRoles:\s*$/.test(line)) { inRoles = true; continue; }
    if (inRoles) {
      if (!/^\s/.test(line)) { inRoles = false; continue; }
      const m = /^\s+[A-Za-z0-9_-]+:\s*(.+?)\s*$/.exec(line);
      if (!m) continue;
      const provider = parseOmpProvider(m[1].replace(/^["']|["']$/g, ''));
      if (provider) providers.add(provider);
    }
  }
  return [...providers].sort();
}

// ── SQLite access ────────────────────────────────────────────────────────────
//
// node:sqlite ships with Node 22.13+ and needs no install. Older hosts fall
// back to the sqlite3 CLI when present. With neither, the credential store
// cannot be inspected or narrowed, and the callers below say so rather than
// mounting every provider's credential or silently mounting none.

// HIVE_OMP_BACKEND_SQLITE=sqlite3 forces the CLI path so the fallback can be
// exercised on a Node that has node:sqlite (the test suite does this).
function nodeSqlite() {
  if (process.env.HIVE_OMP_BACKEND_SQLITE === 'sqlite3') return null;
  try { return require('node:sqlite'); } catch (_) { return null; }
}

function sqliteCliAvailable() {
  const r = spawnSync('sqlite3', ['-version'], { encoding: 'utf8' });
  return !r.error && r.status === 0;
}

function sqliteBackend() {
  if (nodeSqlite()) return 'node:sqlite';
  if (sqliteCliAvailable()) return 'sqlite3';
  return null;
}

function quoteSql(value) {
  return `'${String(value).replace(/'/g, "''")}'`;
}

function listProviderRows(dbFile) {
  const sql = `SELECT provider, credential_type FROM ${OMP_AUTH_TABLE} ORDER BY provider, credential_type`;
  const sqlite = nodeSqlite();
  if (sqlite) {
    const db = new sqlite.DatabaseSync(dbFile, { readOnly: true });
    try {
      return db.prepare(sql).all().map((r) => ({ provider: String(r.provider), credentialType: String(r.credential_type) }));
    } finally { db.close(); }
  }
  const r = spawnSync('sqlite3', ['-readonly', '-json', dbFile, sql], { encoding: 'utf8' });
  if (r.error || r.status !== 0) throw new Error(`sqlite3 could not read ${dbFile}: ${(r.stderr || r.error?.message || '').trim()}`);
  const rows = r.stdout.trim() ? JSON.parse(r.stdout) : [];
  return rows.map((row) => ({ provider: String(row.provider), credentialType: String(row.credential_type) }));
}

function deleteProvidersNotIn(dbFile, keep) {
  const placeholders = keep.map(quoteSql).join(', ');
  const sql = keep.length
    ? `DELETE FROM ${OMP_AUTH_TABLE} WHERE lower(provider) NOT IN (${placeholders})`
    : `DELETE FROM ${OMP_AUTH_TABLE}`;
  const sqlite = nodeSqlite();
  if (sqlite) {
    const db = new sqlite.DatabaseSync(dbFile);
    try { db.exec(sql); } finally { db.close(); }
    return;
  }
  const r = spawnSync('sqlite3', [dbFile, sql], { encoding: 'utf8' });
  if (r.error || r.status !== 0) throw new Error(`sqlite3 could not narrow ${dbFile}: ${(r.stderr || r.error?.message || '').trim()}`);
}

// ── Selection ────────────────────────────────────────────────────────────────

// ompProviderSelection decides which providers' credentials a run needs.
//   source 'model'  — AGENT_MODEL named a provider; that one only (pi's rule).
//   source 'config' — no provider in AGENT_MODEL; the providers config.yml's
//                     modelRoles name, which is what omp runs by default.
//   source 'none'   — nothing selects a provider; nothing is narrowed.
function ompProviderSelection(model, configFile) {
  const fromModel = parseOmpProvider(model);
  if (fromModel) return { source: 'model', providers: [fromModel], fuzzyModel: false };
  const fromConfig = configModelRoleProviders(configFile);
  const fuzzyModel = typeof model === 'string' && model.trim() !== '';
  if (fromConfig.length) return { source: 'config', providers: fromConfig, fuzzyModel };
  return { source: 'none', providers: [], fuzzyModel };
}

// ── Describe (contribute-setup / contribute-check) ───────────────────────────

// describeOmpHost reports what container mode would stage from ompDir, without
// copying anything and without touching credential values: which providers
// have a stored credential, which of them the selection keeps, and anything
// that would leave the container at the login wizard.
function describeOmpHost(ompDir, model) {
  const agentDir = ompAgentDir(ompDir);
  const dbFile = path.join(agentDir, 'agent.db');
  const configFile = path.join(agentDir, 'config.yml');
  const report = {
    agentDir,
    agentDirPresent: fs.existsSync(agentDir),
    configPresent: fs.existsSync(configFile),
    credentialStorePresent: fs.existsSync(dbFile),
    sqlite: sqliteBackend(),
    selection: ompProviderSelection(model, configFile),
    storedProviders: [],
    keptProviders: [],
    warnings: [],
  };
  if (!report.agentDirPresent) {
    report.warnings.push(`${agentDir} does not exist: omp has never been set up on this host, so container mode has nothing to stage and omp will open its setup wizard.`);
    return report;
  }
  if (!report.credentialStorePresent) {
    report.warnings.push(`${dbFile} is missing: no provider is signed in on this host, so the container would start at omp's setup wizard.`);
    return report;
  }
  if (!report.sqlite) {
    report.warnings.push('neither node:sqlite (Node 22.13+) nor a sqlite3 CLI is available, so the stored credentials cannot be inspected or narrowed for the container.');
    return report;
  }
  let rows;
  try { rows = listProviderRows(dbFile); } catch (err) {
    report.warnings.push(`could not read ${dbFile}: ${err.message}`);
    return report;
  }
  report.storedProviders = [...new Set(rows.map((r) => r.provider.toLowerCase()))].sort();
  const keep = new Set(report.selection.providers);
  report.keptProviders = report.selection.source === 'none'
    ? report.storedProviders
    : report.storedProviders.filter((p) => keep.has(p));
  if (report.storedProviders.length === 0) {
    report.warnings.push('no provider credential is stored on this host; the container would start at omp\'s setup wizard.');
  } else if (report.selection.source !== 'none' && report.keptProviders.length === 0) {
    report.warnings.push(`no stored credential matches the selected provider(s) ${report.selection.providers.join(', ')}; stored: ${report.storedProviders.join(', ')}. Sign in to that provider on this host, or pick a signed-in one with AGENT_MODEL=provider/model.`);
  }
  if (report.selection.fuzzyModel) {
    report.warnings.push(`AGENT_MODEL=${JSON.stringify(String(model))} does not name a provider (spell it provider/model to select one); credentials follow config.yml's modelRoles instead.`);
  }
  return report;
}

function describeLines(report) {
  const lines = [];
  lines.push(`omp state: ${report.agentDir}${report.agentDirPresent ? '' : ' (absent)'}`);
  if (report.agentDirPresent) {
    lines.push(`  config.yml: ${report.configPresent ? 'present' : 'absent'}; agent.db (credential store): ${report.credentialStorePresent ? 'present' : 'absent'}`);
  }
  if (report.storedProviders.length) {
    lines.push(`  signed-in providers: ${report.storedProviders.join(', ')}`);
    const how = report.selection.source === 'model' ? 'AGENT_MODEL'
      : report.selection.source === 'config' ? 'config.yml modelRoles' : 'no selection, all kept';
    lines.push(`  container mode stages: ${report.keptProviders.length ? report.keptProviders.join(', ') : '(none)'} (selected by ${how})`);
  }
  for (const w of report.warnings) lines.push(`  WARNING: ${w}`);
  return lines;
}

// ── Stage (contribute-hive, container mode) ──────────────────────────────────

// stageOmp copies the allowlisted entries from ompDir/agent into
// stageDir/agent and narrows the staged credential store to the selected
// providers. Returns the describe report for the host plus what was staged.
// Throws when the store exists but cannot be narrowed: mounting every
// provider's credential would violate least privilege, and silently mounting
// none would recreate the wizard this exists to prevent.
function stageOmp(ompDir, stageDir, model) {
  const report = describeOmpHost(ompDir, model);
  const srcAgent = report.agentDir;
  const dstAgent = path.join(stageDir, 'agent');
  report.staged = [];
  if (!report.agentDirPresent) return report;
  fs.mkdirSync(dstAgent, { recursive: true, mode: 0o700 });
  for (const entry of OMP_STAGED_AGENT_ENTRIES) {
    const src = path.join(srcAgent, entry);
    if (!fs.existsSync(src) || !fs.statSync(src).isFile()) continue;
    const dst = path.join(dstAgent, entry);
    fs.copyFileSync(src, dst);
    fs.chmodSync(dst, 0o600);
    report.staged.push(entry);
  }
  const stagedDb = path.join(dstAgent, 'agent.db');
  if (!fs.existsSync(stagedDb)) return report;
  if (!report.sqlite) {
    throw new Error('cannot narrow the staged omp credential store: install Node 22.13+ (node:sqlite) or a sqlite3 CLI.');
  }
  if (report.selection.source !== 'none') {
    deleteProvidersNotIn(stagedDb, report.selection.providers);
    // Re-read the store the container will mount and refuse to ship it if an
    // unselected provider survived (a sidecar replayed on open, a schema this
    // code does not understand): the invariant is what matters, not the
    // DELETE having run.
    const remaining = [...new Set(listProviderRows(stagedDb).map((r) => r.provider.toLowerCase()))].sort();
    const allowed = new Set(report.selection.providers);
    const leaked = remaining.filter((p) => !allowed.has(p));
    if (leaked.length) throw new Error(`staged omp credential store still holds unselected providers: ${leaked.join(', ')}`);
    report.keptProviders = remaining;
  }
  return report;
}

module.exports = {
  OMP_STAGED_AGENT_ENTRIES,
  parseOmpProvider,
  configModelRoleProviders,
  ompProviderSelection,
  describeOmpHost,
  describeLines,
  stageOmp,
  sqliteBackend,
};

if (require.main === module) {
  const [command, ...rest] = process.argv.slice(2);
  if (command === '--describe') {
    // --describe <omp-dir> [AGENT_MODEL]
    const [ompDir, model] = rest;
    const report = describeOmpHost(ompDir || path.join(process.env.HOME || '', '.omp'), model || '');
    process.stdout.write(`${describeLines(report).join('\n')}\n`);
    process.exit(0);
  }
  if (command === '--stage') {
    // --stage <omp-dir> <stage-dir> [AGENT_MODEL]
    const [ompDir, stageDir, model] = rest;
    if (!ompDir || !stageDir) {
      process.stderr.write('usage: omp-backend.js --stage <omp-dir> <stage-dir> [AGENT_MODEL]\n');
      process.exit(2);
    }
    let report;
    try { report = stageOmp(ompDir, stageDir, model || ''); } catch (err) {
      process.stderr.write(`ERROR: ${err.message}\n`);
      process.exit(1);
    }
    process.stdout.write(`${describeLines(report).join('\n')}\n`);
    process.exit(0);
  }
  process.stderr.write('usage: omp-backend.js --describe <omp-dir> [AGENT_MODEL] | --stage <omp-dir> <stage-dir> [AGENT_MODEL]\n');
  process.exit(2);
}
