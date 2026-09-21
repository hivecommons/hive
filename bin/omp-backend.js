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
//
// A staged OAuth record is a COPY, and OAuth refresh tokens are single-use
// (hivecommons/hive#7922): the first client to refresh gets a new refresh
// token and the old one is revoked. So once omp inside the container has
// refreshed — its access token expires mid-run — the host's row holds a
// revoked token. The host's own omp then fails its next refresh and marks the
// row `disabled_cause = "oauth refresh failed: … invalid_grant …"`, and every
// later launch stages either the revoked token or the disabled row; omp in
// the container ignores a disabled row and silently falls back to whatever
// other provider it can reach. Two things here close that:
//   --sync-back  writes the selected provider's credential row back over the
//                host row it was staged from, only when the container's copy
//                is strictly newer (syncBackOmp below). contribute-hive runs
//                it on a timer while the container runs and once more, after
//                the container has stopped, before the stage dir is deleted.
//   --stage      refuses to launch when a selected provider's stored
//                credential is disabled, naming the cause and the remedy
//                (sign in again on the host), instead of starting a container
//                that will fall back silently.

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

// How long a read or write waits on a store another process is writing to:
// the host's omp, or — for the timer-driven sync-back (#7922) — the
// container's, through the bind mount. Without it a read that lands on
// omp's own transaction fails outright with SQLITE_BUSY.
const OMP_BUSY_TIMEOUT_MS = 5000;

// printableCause makes a disabled_cause safe to print. The store may have
// been written by an untrusted container (the staged copy, and a host row
// --sync-back wrote from it), and the cause is the one column that reaches
// the host's terminal: strip control characters (terminal escapes included)
// and cap the length, so the worst it can be is a long string.
function printableCause(value) {
  if (value === null || value === undefined) return '';
  // eslint-disable-next-line no-control-regex
  const text = String(value).replace(/[\x00-\x1f\x7f-\x9f]/g, ' ').trim();
  return text.length > 400 ? `${text.slice(0, 400)}…` : text;
}

// queryRows runs one read-only SELECT against dbFile through whichever
// SQLite the host has (node:sqlite, else the sqlite3 CLI) and returns the
// rows as plain objects. The SQL is passed to the CLI on stdin, never argv,
// so a statement can carry a credential value without it showing up in the
// process table.
function queryRows(dbFile, sql) {
  const sqlite = nodeSqlite();
  if (sqlite) {
    const db = new sqlite.DatabaseSync(dbFile, { readOnly: true });
    try {
      db.exec(`PRAGMA busy_timeout = ${OMP_BUSY_TIMEOUT_MS}`);
      return db.prepare(sql).all();
    } finally { db.close(); }
  }
  // `.timeout` is the shell's own command, so unlike a PRAGMA it prints no
  // row of its own into the -json output.
  const r = spawnSync('sqlite3', ['-readonly', '-json', dbFile], { encoding: 'utf8', input: `.timeout ${OMP_BUSY_TIMEOUT_MS}\n${sql};\n` });
  if (r.error || r.status !== 0) throw new Error(`sqlite3 could not read ${dbFile}: ${(r.stderr || r.error?.message || '').trim()}`);
  return r.stdout.trim() ? JSON.parse(r.stdout) : [];
}

// execStatements runs write statements against dbFile in order. Same stdin
// rule as queryRows: the sync-back's UPDATE carries the credential itself.
function execStatements(dbFile, statements) {
  const sqlite = nodeSqlite();
  if (sqlite) {
    const db = new sqlite.DatabaseSync(dbFile);
    try {
      db.exec(`PRAGMA busy_timeout = ${OMP_BUSY_TIMEOUT_MS}`);
      for (const sql of statements) db.exec(sql);
    } finally { db.close(); }
    return;
  }
  const r = spawnSync('sqlite3', [dbFile], { encoding: 'utf8', input: `.timeout ${OMP_BUSY_TIMEOUT_MS}\n${statements.join(';\n')};\n` });
  if (r.error || r.status !== 0) throw new Error(`sqlite3 could not write ${dbFile}: ${(r.stderr || r.error?.message || '').trim()}`);
}

// hasDisabledCauseColumn: omp added `disabled_cause` to auth_credentials
// alongside its refresh-failure handling. A store from an older omp has no
// such column; a row there can only ever count as usable, and the column is
// left out of what --sync-back writes.
function hasDisabledCauseColumn(dbFile) {
  return queryRows(dbFile, `PRAGMA table_info(${OMP_AUTH_TABLE})`).some((c) => String(c.name) === 'disabled_cause');
}

// listProviderRows reads the non-secret columns of every credential row:
// which provider, what kind, and — #7922 — whether omp has disabled it and
// why. disabled_cause is what omp writes when a refresh fails ("oauth refresh
// failed: ... invalid_grant ..."); a row carrying one is a sign-in the
// container cannot use, and staging it only moves the failure into a pane
// nobody is watching.
function listProviderRows(dbFile) {
  const causeColumn = hasDisabledCauseColumn(dbFile) ? 'disabled_cause' : 'NULL AS disabled_cause';
  const sql = `SELECT provider, credential_type, ${causeColumn} FROM ${OMP_AUTH_TABLE} ORDER BY provider, credential_type`;
  return queryRows(dbFile, sql).map((r) => ({
    provider: String(r.provider),
    credentialType: String(r.credential_type),
    disabledCause: printableCause(r.disabled_cause),
  }));
}

// deleteProvidersNotIn narrows the staged store to `keep` and makes the
// narrowing PHYSICAL, not just logical (#7682). A plain DELETE leaves the
// dropped rows byte-for-byte in SQLite's free pages (secure_delete defaults
// off) and committed frames survive in the -wal sidecar, so the container
// could carve unselected providers' tokens out of the mounted file. So:
//   secure_delete=ON   — zero the row bytes as they are deleted;
//   journal_mode=DELETE — checkpoint the -wal into the main file and remove
//                         the sidecar (staged copies are never shared, so WAL
//                         buys nothing here);
//   VACUUM             — rebuild the file with no free pages at all.
function deleteProvidersNotIn(dbFile, keep) {
  const placeholders = keep.map(quoteSql).join(', ');
  const del = keep.length
    ? `DELETE FROM ${OMP_AUTH_TABLE} WHERE lower(provider) NOT IN (${placeholders})`
    : `DELETE FROM ${OMP_AUTH_TABLE}`;
  const statements = ['PRAGMA secure_delete = ON', 'PRAGMA journal_mode = DELETE', del, 'VACUUM'];
  const sqlite = nodeSqlite();
  if (sqlite) {
    const db = new sqlite.DatabaseSync(dbFile);
    try { for (const sql of statements) db.exec(sql); } finally { db.close(); }
  } else {
    const r = spawnSync('sqlite3', [dbFile, statements.join('; ')], { encoding: 'utf8' });
    if (r.error || r.status !== 0) throw new Error(`sqlite3 could not narrow ${dbFile}: ${(r.stderr || r.error?.message || '').trim()}`);
  }
  // journal_mode=DELETE removes the sidecars on close; make sure nothing that
  // predates the narrowing is left beside the file the container mounts.
  for (const sidecar of [`${dbFile}-wal`, `${dbFile}-shm`, `${dbFile}-journal`]) {
    fs.rmSync(sidecar, { force: true });
  }
}

// freelistPageCount reports how many free (unreferenced, possibly still
// populated) pages the store holds. After VACUUM it must be zero; anything
// else means bytes of deleted rows may still be in the file.
function freelistPageCount(dbFile) {
  const sql = 'PRAGMA freelist_count';
  const sqlite = nodeSqlite();
  if (sqlite) {
    const db = new sqlite.DatabaseSync(dbFile, { readOnly: true });
    try {
      const row = db.prepare(sql).get();
      return Number(row ? Object.values(row)[0] : NaN);
    } finally { db.close(); }
  }
  const r = spawnSync('sqlite3', ['-readonly', dbFile, sql], { encoding: 'utf8' });
  if (r.error || r.status !== 0) throw new Error(`sqlite3 could not inspect ${dbFile}: ${(r.stderr || r.error?.message || '').trim()}`);
  return Number(r.stdout.trim());
}

// credentialBlobsNotIn returns the raw `data` values of the rows that
// deleteProvidersNotIn(dbFile, keep) will drop, so the caller can prove
// afterwards that those exact bytes no longer exist anywhere in the file.
// The values are held in memory only for the length of the staging call and
// are never logged or reported.
function credentialBlobsNotIn(dbFile, keep) {
  const placeholders = keep.map(quoteSql).join(', ');
  const sql = keep.length
    ? `SELECT data FROM ${OMP_AUTH_TABLE} WHERE lower(provider) NOT IN (${placeholders})`
    : `SELECT data FROM ${OMP_AUTH_TABLE}`;
  const sqlite = nodeSqlite();
  if (sqlite) {
    const db = new sqlite.DatabaseSync(dbFile, { readOnly: true });
    try { return db.prepare(sql).all().map((r) => String(r.data)); } finally { db.close(); }
  }
  const r = spawnSync('sqlite3', ['-readonly', '-json', dbFile, sql], { encoding: 'utf8' });
  if (r.error || r.status !== 0) throw new Error(`sqlite3 could not read ${dbFile}: ${(r.stderr || r.error?.message || '').trim()}`);
  const rows = r.stdout.trim() ? JSON.parse(r.stdout) : [];
  return rows.map((row) => String(row.data));
}

// physicalResidueCount scans the raw bytes of the staged store (and any
// sidecar) for the deleted credential blobs. Returns how many of them are
// still recoverable; the caller reports the count, never the values.
function physicalResidueCount(dbFile, blobs) {
  const files = [dbFile, `${dbFile}-wal`, `${dbFile}-journal`].filter((f) => fs.existsSync(f));
  const bytes = Buffer.concat(files.map((f) => fs.readFileSync(f)));
  return blobs.filter((blob) => blob.length > 0 && bytes.includes(Buffer.from(blob))).length;
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
    disabledProviders: [],
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
  // #7922: a kept provider whose row omp has disabled. The container would
  // start with "No API key found for <provider>" and fall back to whatever
  // provider lists any model — on the observed host, an unreachable local
  // ollama — while the relay keeps reporting the selected model to the hub.
  const kept = new Set(report.keptProviders);
  report.disabledProviders = rows
    .filter((r) => r.disabledCause && kept.has(r.provider.toLowerCase()))
    .map((r) => ({ provider: r.provider.toLowerCase(), cause: r.disabledCause }));
  for (const d of report.disabledProviders) {
    report.warnings.push(`the ${d.provider} sign-in on this host is disabled — ${d.cause}. Run omp on the host and /login to ${d.provider} again; until then the container would start with no usable ${d.provider} credential.`);
  }
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
  // #7922: refuse to launch on a disabled sign-in rather than ship it. The
  // failure is the operator's to fix on the host (/login), and a launch that
  // proceeds ends in a silent provider fallback the relay cannot see.
  if (report.disabledProviders.length) {
    const named = report.disabledProviders.map((d) => `${d.provider} (${d.cause})`).join('; ');
    throw new Error(`the selected omp sign-in is disabled on this host: ${named}. Run omp on the host and /login again, then relaunch.`);
  }
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
    const doomed = credentialBlobsNotIn(stagedDb, report.selection.providers);
    deleteProvidersNotIn(stagedDb, report.selection.providers);
    // Re-read the store the container will mount and refuse to ship it if an
    // unselected provider survived (a sidecar replayed on open, a schema this
    // code does not understand): the invariant is what matters, not the
    // DELETE having run.
    const remaining = [...new Set(listProviderRows(stagedDb).map((r) => r.provider.toLowerCase()))].sort();
    const allowed = new Set(report.selection.providers);
    const leaked = remaining.filter((p) => !allowed.has(p));
    if (leaked.length) throw new Error(`staged omp credential store still holds unselected providers: ${leaked.join(', ')}`);
    // ...and physically (#7682): no free pages that could still carry the
    // deleted rows' bytes, no WAL/journal sidecar carrying their frames, and
    // no deleted provider's name anywhere in the raw file.
    const freePages = freelistPageCount(stagedDb);
    if (freePages !== 0) throw new Error(`staged omp credential store still has ${freePages} free page(s) that may hold deleted credential bytes`);
    for (const sidecar of ['agent.db-wal', 'agent.db-shm', 'agent.db-journal']) {
      if (fs.existsSync(path.join(dstAgent, sidecar))) throw new Error(`staged omp credential store still has a ${sidecar} sidecar that may hold deleted credential frames`);
    }
    report.staged = report.staged.filter((entry) => entry !== 'agent.db-wal');
    const residue = physicalResidueCount(stagedDb, doomed);
    if (residue) throw new Error(`staged omp credential store still holds the bytes of ${residue} deleted credential(s)`);
    report.keptProviders = remaining;
  }
  return report;
}

// ── Sync back (contribute-hive cleanup, container mode) ─────────────────────

// syncBackOmp copies a refreshed credential out of the staged store into the
// host's (hivecommons/hive#7922): after the container has stopped, and —
// because the staged store is a bind mount the host can read in place — on a
// timer while it runs, so the window in which the host holds a revoked token
// is minutes rather than the whole run. contribute-hive kills the timer
// before the exit pass, so the two never run over the same row at once; each
// pass is idempotent and monotonic either way (strictly-newer only).
//
// The container runs against a COPY of an OAuth record, and OAuth refresh
// tokens are single-use: the first client to refresh receives a new refresh
// token and the old one is revoked. A container that ran long enough for its
// access token to expire refreshed, and from that moment the host's row held
// a revoked refresh token — the host's own omp broke, and every later launch
// staged the dead token ("Refresh token not found or invalid", then "No API
// key found for anthropic", then a silent fallback to a model the container
// could not reach). Writing the container's newer row back closes that
// window for every clean exit; a crash mid-run can still lose the refresh.
//
// The H6 boundary is kept as narrow as the fix allows: only rows the host
// ALREADY holds for the selected providers are touched, only their
// credential columns (data, disabled_cause, updated_at), and only when the
// staged row is strictly newer. Nothing is inserted, no other provider is
// read, config.yml and models.db never travel back. What the container can
// do to the host through this is overwrite a credential it was already
// holding — which it could revoke anyway.
function syncBackOmp(ompDir, stageDir, model) {
  const report = describeOmpHost(ompDir, model);
  report.syncedProviders = [];
  report.skipped = [];
  const hostDb = path.join(report.agentDir, 'agent.db');
  const stagedDb = path.join(stageDir, 'agent', 'agent.db');
  if (!fs.existsSync(stagedDb) || !fs.existsSync(hostDb) || !report.sqlite) return report;
  const keep = report.selection.source === 'none' ? null : new Set(report.selection.providers);
  // An older omp store has no disabled_cause column (it arrived with omp's
  // refresh-failure handling); read NULL for it and leave it out of the write.
  const withCause = hasDisabledCauseColumn(hostDb) && hasDisabledCauseColumn(stagedDb);
  const cols = `provider, credential_type, identity_key, data, ${withCause ? 'disabled_cause' : 'NULL AS disabled_cause'}, updated_at`;
  const staged = queryRows(stagedDb, `SELECT ${cols} FROM ${OMP_AUTH_TABLE}`);
  const host = queryRows(hostDb, `SELECT ${cols} FROM ${OMP_AUTH_TABLE}`);
  const rowKey = (r) => `${String(r.provider).toLowerCase()}\u0000${r.credential_type}\u0000${r.identity_key == null ? '' : r.identity_key}`;
  const hostByKey = new Map(host.map((r) => [rowKey(r), r]));
  const statements = [];
  for (const s of staged) {
    const provider = String(s.provider).toLowerCase();
    if (keep && !keep.has(provider)) { report.skipped.push(`${provider}: not a selected provider`); continue; }
    const h = hostByKey.get(rowKey(s));
    if (!h) { report.skipped.push(`${provider}: no matching row on the host`); continue; }
    if (!(Number(s.updated_at) > Number(h.updated_at))) { report.skipped.push(`${provider}: host row is as new or newer`); continue; }
    const where = `lower(provider) = ${quoteSql(provider)} AND credential_type = ${quoteSql(s.credential_type)} AND ` +
      (s.identity_key == null ? 'identity_key IS NULL' : `identity_key = ${quoteSql(s.identity_key)}`) +
      ` AND updated_at < ${Number(s.updated_at)}`;
    const cause = withCause ? `, disabled_cause = ${s.disabled_cause == null ? 'NULL' : quoteSql(s.disabled_cause)}` : '';
    statements.push(`UPDATE ${OMP_AUTH_TABLE} SET data = ${quoteSql(s.data)}${cause}, updated_at = ${Number(s.updated_at)} WHERE ${where}`);
    report.syncedProviders.push(provider);
  }
  if (statements.length) execStatements(hostDb, ['BEGIN', ...statements, 'COMMIT']);
  return report;
}

function syncBackLines(report) {
  const lines = [];
  if (report.syncedProviders && report.syncedProviders.length) {
    lines.push(`omp sign-in refreshed in the container written back to ${report.agentDir}: ${report.syncedProviders.join(', ')} (#7922)`);
  } else {
    lines.push(`omp sign-in: nothing newer in the container to write back to ${report.agentDir}`);
  }
  return lines;
}

module.exports = {
  OMP_STAGED_AGENT_ENTRIES,
  parseOmpProvider,
  configModelRoleProviders,
  ompProviderSelection,
  describeOmpHost,
  describeLines,
  stageOmp,
  syncBackOmp,
  syncBackLines,
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
  if (command === '--sync-back') {
    // --sync-back <omp-dir> <stage-dir> [AGENT_MODEL]
    const [ompDir, stageDir, model] = rest;
    if (!ompDir || !stageDir) {
      process.stderr.write('usage: omp-backend.js --sync-back <omp-dir> <stage-dir> [AGENT_MODEL]\n');
      process.exit(2);
    }
    let report;
    try { report = syncBackOmp(ompDir, stageDir, model || ''); } catch (err) {
      process.stderr.write(`ERROR: ${err.message}\n`);
      process.exit(1);
    }
    process.stdout.write(`${syncBackLines(report).join('\n')}\n`);
    process.exit(0);
  }
  process.stderr.write('usage: omp-backend.js --describe <omp-dir> [AGENT_MODEL] | --stage <omp-dir> <stage-dir> [AGENT_MODEL] | --sync-back <omp-dir> <stage-dir> [AGENT_MODEL]\n');
  process.exit(2);
}
