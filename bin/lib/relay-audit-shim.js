#!/usr/bin/env node
'use strict';

// gh/git PATH shim for the relay's `commands` audit level
// (hivecommons/hive#13456). The relay writes two tiny sh wrappers into its
// owner-only audit directory and prepends that directory to the PATH of the
// CLI it launches — never to a shell rc or the image. Each wrapper execs:
//
//   node relay-audit-shim.js <state-file> <gh|git> <args...>
//
// The shim runs the real binary with the caller's stdio, waits, and then
// appends one redacted row to the relay's spool. It is NOT a gate: it never
// blocks, rewrites or delays a command beyond the exec hop, and any failure
// to record is swallowed so the command's own exit status always wins.

const { spawnSync } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');
const audit = require('./relay-audit.js');

function readState(file) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); } catch (_) { return null; }
}

function main(argv) {
  const stateFile = argv[2] || '';
  const op = argv[3] === 'git' ? 'git' : 'gh';
  const args = argv.slice(4);
  const state = readState(stateFile);
  const shimDir = stateFile ? path.join(path.dirname(stateFile), 'bin') : '';
  const real = audit.resolveRealBinary(op, state, process.env.PATH, shimDir);
  if (!real) {
    process.stderr.write(`${op}: command not found\n`);
    return 127;
  }
  // The terminal delivers Ctrl-C to the whole foreground group; the real
  // binary decides what it means. The shim just outlives it to report.
  const ignore = () => {};
  const signals = ['SIGINT', 'SIGQUIT', 'SIGTERM', 'SIGHUP'];
  for (const s of signals) { try { process.on(s, ignore); } catch (_) {} }
  const startedAt = Date.now();
  const result = spawnSync(real, args, { stdio: 'inherit' });
  const durationMs = Date.now() - startedAt;
  let exit = result.status;
  if (result.error) exit = result.error.code === 'ENOENT' ? 127 : 126;
  if (result.signal) exit = 128 + (os.constants.signals[result.signal] || 0);
  audit.recordShimCommand({ state, op, args, startedAt, durationMs, exit, cwd: process.cwd(), pid: process.pid });
  if (result.signal) {
    for (const s of signals) { try { process.removeListener(s, ignore); } catch (_) {} }
    try { process.kill(process.pid, result.signal); } catch (_) {}
  }
  return exit == null ? 1 : exit;
}

if (require.main === module) {
  process.exitCode = main(process.argv);
}

module.exports = { main };
