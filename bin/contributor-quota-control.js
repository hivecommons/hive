#!/usr/bin/env node
'use strict';

// Out-of-band contributor quota control (hivecommons/hive#6953).
//
// The relay almost always runs detached — a background process, a container, a
// K8s pod — so #6833's scoped overrides cannot depend on typing into its stdin.
// This is the reliable command path #6833 asks for: it drops a scoped override
// into the SAME per-pool directory the relay reads on every guard evaluation
// and every retry tick, so a `continue-once` written here reaches a relay that
// has no terminal at all. The launcher's attached banner and this command share
// exactly one mechanism (the pool file), so there is no second code path to
// drift out of step.
//
// It writes only what the relay published about its own hold (window id, reset
// epoch, session id) — never a provider credential, raw account identifier, or
// billing figure (#6833 privacy).

const store = require('./lib/quota-pool-store.js');

function usage(msg) {
  if (msg) console.error(`Error: ${msg}\n`);
  console.error(`Usage: contributor-quota-control <action>

Actions:
  continue-once          Admit exactly one task, then re-evaluate the guard.
  continue-until-reset   Admit until the currently guarded window resets.
  disable-session        Turn the guard off for the running relay session.
  pause-until-reset      Stay paused even if quota recovers, until the window resets.
  resume | clear         Remove any active override.
  status                 Print what the relay last published about its hold.

Reads the same pool directory the relay uses:
  HIVE_CONTRIBUTOR_QUOTA_POOL_DIR      (required) shared pool directory
  HIVE_CONTRIBUTOR_QUOTA_POOL_ACCOUNT  (optional) account component of the pool key
  AGENT_BACKEND                        (optional) backend name component of the pool key`);
  process.exit(msg ? 2 : 0);
}

function main(argv) {
  const action = (argv[0] || '').trim();
  if (!action || action === '-h' || action === '--help' || action === 'help') return usage();

  const dir = (process.env.HIVE_CONTRIBUTOR_QUOTA_POOL_DIR || '').trim();
  if (!dir) return usage('HIVE_CONTRIBUTOR_QUOTA_POOL_DIR is not set; the relay must run with a pool directory for the command path to reach it.');

  const poolKey = store.derivePoolKey({
    backend: process.env.AGENT_BACKEND || '',
    account: process.env.HIVE_CONTRIBUTOR_QUOTA_POOL_ACCOUNT || '',
  });
  const pool = { dir, poolKey };
  const status = store.readStatus(pool);

  // window-scoped controls need the EXACT window/epoch the relay is holding on,
  // so expiry is mechanical rather than a guess. Without a published hold there
  // is nothing to scope to — refuse rather than write an override that can never
  // match and would silently do nothing.
  function requireWindow() {
    if (!status || !status.window_id || !Number.isFinite(Number(status.reset_epoch))) {
      console.error('No guarded window is currently published for this pool. ' +
        'The relay publishes one only while it is holding on a window reserve, ' +
        'so there is nothing for a window-scoped override to pin to right now.');
      process.exit(3);
    }
    return { window_id: status.window_id, reset_epoch: Number(status.reset_epoch) };
  }

  switch (action) {
    case 'continue-once':
      store.writeOverride(pool, { directive: 'continue_once' });
      console.log('Override written: continue-once. The relay will admit one task, then re-check the guard.');
      break;
    case 'continue-until-reset': {
      const w = requireWindow();
      store.writeOverride(pool, { directive: 'continue_until_reset', window_id: w.window_id, reset_epoch: w.reset_epoch });
      console.log(`Override written: continue-until-reset for window ${w.window_id} ` +
        `(expires when it resets at ${new Date(w.reset_epoch).toISOString()}).`);
      break;
    }
    case 'pause-until-reset': {
      const w = requireWindow();
      store.writeOverride(pool, { directive: 'pause_until_reset', window_id: w.window_id, reset_epoch: w.reset_epoch });
      console.log(`Override written: pause-until-reset for window ${w.window_id}. ` +
        'The relay stays paused even if quota recovers, until that window resets.');
      break;
    }
    case 'disable-session': {
      if (!status || !status.session_id) {
        console.error('No relay session is currently published for this pool. ' +
          'The relay publishes its session id while holding; start it (or wait for a hold) first.');
        process.exit(3);
      }
      store.writeOverride(pool, { directive: 'disable_session', session_id: status.session_id });
      console.log(`Override written: disable-session for ${status.session_id}. ` +
        'The guard is off for that session; it re-arms when the session exits.');
      break;
    }
    case 'resume':
    case 'clear':
      store.clearOverride(pool);
      console.log('Any active override has been cleared. The guard evaluates on its own reserve again.');
      break;
    case 'status': {
      const ov = store.readOverride(pool);
      console.log(JSON.stringify({ pool_key: poolKey, hold: status || null, override: ov || null }, null, 2));
      break;
    }
    default:
      return usage(`unknown action: ${action}`);
  }
}

if (require.main === module) {
  try {
    main(process.argv.slice(2));
  } catch (e) {
    console.error(`contributor-quota-control failed: ${e.message}`);
    process.exit(1);
  }
}

module.exports = { main };
