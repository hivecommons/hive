'use strict';

// Cross-process quota-pool store + scoped override channel (hivecommons/hive#6953).
//
// #6833 requires two things the in-process guard could not give: guard state
// keyed by the local quota POOL (so two relays on one provider account share
// one reserve instead of each holding their own), and out-of-band scoped
// overrides that reach a DETACHED relay — the relay almost never has a stdin to
// prompt on. The issue's own design note observes these are one mechanism: a
// per-pool file on disk both carries the shared reservation state and is where
// `just contribute-quota continue-once` drops an override. This module is that
// file layer. It is deliberately pure of relay wiring so it can be unit-tested
// and reused by the control CLI without loading the whole relay.
//
// Everything here is fail-closed by construction: a torn/absent/garbage file
// reads as "no state", which upstream translates to a HOLD, never an admit. The
// only thing this layer ever authorizes is an override a human explicitly wrote.

const fs = require('fs');
const os = require('os');
const path = require('path');
const crypto = require('crypto');

// derivePoolKey turns backend + account identity into a stable, opaque key.
//
// #6833's privacy criterion forbids logging raw account identifiers or
// credentials, and this key is used in FILE NAMES — the most quietly leaky
// surface there is. So the raw hint never survives: it is hashed and truncated,
// and the account component is the only thing that varies the key, so two
// relays authenticated to the SAME account land on the same pool while two
// different accounts (or an unset hint) do not silently collide into one shared
// reserve. An unset account still keys off the backend, which is the safe
// direction: same-backend relays on one host share, which cannot oversubscribe
// MORE than the true pool, only less.
function derivePoolKey({ backend = '', account = '' } = {}) {
  const material = `${String(backend).trim().toLowerCase()}\0${String(account).trim()}`;
  return crypto.createHash('sha256').update(material).digest('hex').slice(0, 16);
}

// atomicWriteJson writes to a unique temp sibling then renames. rename() is
// atomic within a filesystem, so a concurrent reader sees either the old file
// or the new one, never a half-written one — the torn-file crash #6951/#6833
// call out is unreachable through this writer. Same-directory temp guarantees
// the rename stays on one filesystem.
function atomicWriteJson(file, obj) {
  const dir = path.dirname(file);
  fs.mkdirSync(dir, { recursive: true });
  const tmp = path.join(dir, `.${path.basename(file)}.tmp-${process.pid}-${crypto.randomBytes(6).toString('hex')}`);
  fs.writeFileSync(tmp, JSON.stringify(obj), { mode: 0o600 });
  fs.renameSync(tmp, file);
}

// readJson never throws (fail-closed): an unreadable or malformed file is
// indistinguishable from an absent one — "no state" — because trusting a
// partially written file is exactly the bug class this store exists to remove.
function readJson(file) {
  let raw;
  try {
    raw = fs.readFileSync(file, 'utf8');
  } catch (_) {
    return null;
  }
  try {
    const v = JSON.parse(raw);
    return v && typeof v === 'object' && !Array.isArray(v) ? v : null;
  } catch (_) {
    return null;
  }
}

function overrideFile(store) {
  return path.join(store.dir, `${store.poolKey}.override.json`);
}

function statusFile(store) {
  return path.join(store.dir, `${store.poolKey}.status.json`);
}

function reservationFile(store, relayId) {
  // A relay owns exactly its own reservation file, so two relays never
  // read-modify-write one shared document and lose each other's update — the
  // peer scan just lists the directory. relayId is sanitized because it lands
  // in a filename.
  const safe = String(relayId).replace(/[^A-Za-z0-9._-]/g, '_');
  return path.join(store.dir, `${store.poolKey}.reservation.${safe}.json`);
}

// ── Overrides ────────────────────────────────────────────────────────────────

const OVERRIDE_DIRECTIVES = new Set([
  'continue_once',        // admit exactly one assignment, then re-evaluate
  'continue_until_reset', // admit until this window's reset epoch changes
  'disable_session',      // admit for the rest of THIS relay session
  'pause_until_reset',    // hold even if a reading clears, until reset
]);

function writeOverride(store, override) {
  if (!override || !OVERRIDE_DIRECTIVES.has(override.directive)) {
    throw new Error(`unknown quota override directive: ${override && override.directive}`);
  }
  atomicWriteJson(overrideFile(store), { ...override, created_at: override.created_at || Date.now() });
}

function readOverride(store) {
  const ov = readJson(overrideFile(store));
  if (!ov || !OVERRIDE_DIRECTIVES.has(ov.directive)) return null;
  return ov;
}

function clearOverride(store) {
  try { fs.unlinkSync(overrideFile(store)); } catch (_) {}
}

// ── Hold status (what the control CLI needs to scope an override) ─────────────
//
// A detached relay is the thing that knows which window is guarding and when it
// resets; the human running `just contribute-quota continue-until-reset` does
// not. The relay publishes that here whenever it holds, so the CLI can pin an
// until-reset override to the EXACT window/epoch rather than guessing — that is
// what makes until-reset expiry mechanical instead of advisory. It carries no
// account identifier, credential, or billing figure: only the window id, its
// reset epoch, the remaining/required percentages, and the session id the
// disable-session control needs to target this process.
function writeStatus(store, status) {
  atomicWriteJson(statusFile(store), { ...status, updated_at: Date.now() });
}

function readStatus(store) {
  return readJson(statusFile(store));
}

function clearStatus(store) {
  try { fs.unlinkSync(statusFile(store)); } catch (_) {}
}

// ── Reservations (the anti-oversubscription mechanism) ────────────────────────

function writeReservation(store, relayId, expiresAt) {
  atomicWriteJson(reservationFile(store, relayId), { relay_id: relayId, expires_at: expiresAt });
}

function releaseReservation(store, relayId) {
  try { fs.unlinkSync(reservationFile(store, relayId)); } catch (_) {}
}

// activePeerReservations lists reservations held by OTHER relays in this pool
// that have not expired. A relay's own reservation is excluded so it does not
// hold against itself. Expired files (a crashed relay that never released) are
// swept, so a dead peer cannot wedge the pool past its lease.
function activePeerReservations(store, relayId, now = Date.now()) {
  let entries;
  try {
    entries = fs.readdirSync(store.dir);
  } catch (_) {
    return [];
  }
  const prefix = `${store.poolKey}.reservation.`;
  const peers = [];
  for (const name of entries) {
    if (!name.startsWith(prefix) || !name.endsWith('.json')) continue;
    const full = path.join(store.dir, name);
    const rec = readJson(full);
    if (!rec || typeof rec.expires_at !== 'number') { continue; }
    if (rec.expires_at <= now) { try { fs.unlinkSync(full); } catch (_) {} continue; }
    if (rec.relay_id === relayId) continue;
    peers.push(rec);
  }
  return peers;
}

// tmpStoreDir is a test helper: a throwaway pool directory under the OS temp
// root, used only by unit tests and never by the relay.
function tmpStoreDir(label = 'hive-6953') {
  return fs.mkdtempSync(path.join(os.tmpdir(), `${label}-`));
}

module.exports = {
  OVERRIDE_DIRECTIVES,
  derivePoolKey,
  atomicWriteJson,
  readJson,
  overrideFile,
  statusFile,
  reservationFile,
  writeOverride,
  readOverride,
  clearOverride,
  writeStatus,
  readStatus,
  clearStatus,
  writeReservation,
  releaseReservation,
  activePeerReservations,
  tmpStoreDir,
};
