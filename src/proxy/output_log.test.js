// #11944 — the dashboard auth proxy's output is kept as the "proxy" log
// source (see output_log.js), with each line's time and stream, while still
// reaching the container's stdout/stderr exactly as before.
//
// The unit half drives OutputLog directly: record shape, line splitting,
// rotation to 5 files, age pruning and file modes, all of which the Go reader
// in src/pkg/logread depends on. The process half spawns the real server.js
// and checks the [SECURITY] refusal (stderr, exit 1) and the startup line
// (stdout) both land in proxy.log and on the child's own streams.

import { strict as assert } from 'assert';
import { spawn } from 'child_process';
import fs from 'fs';
import os from 'os';
import path from 'path';
import { fileURLToPath } from 'url';
import {
  OutputLog,
  installOutputLog,
  outputDir,
  backupName,
  backupStamp,
  OUTPUT_DIR_ENV,
  DEFAULT_OUTPUT_DIR,
  MAX_FILES,
  MAX_AGE_MS,
  LINE_MAX_BYTES,
} from './output_log.js';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY_PORT = 19151;
const GO_PORT = 19152;
const TTYD_PORT = 19153;

let failures = 0;
async function test(name, fn) {
  try {
    await fn();
    console.log(`  ok  ${name}`);
  } catch (err) {
    failures++;
    console.error(`  FAIL ${name}\n${err && err.stack}`);
  }
}

function tempDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'proxy-output-log-'));
}

function records(file) {
  return fs.readFileSync(file, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l));
}

function mode(p) {
  return fs.statSync(p).mode & 0o777;
}

await test('outputDir defaults and honours the env override', () => {
  assert.equal(outputDir({}), DEFAULT_OUTPUT_DIR);
  assert.equal(outputDir({ [OUTPUT_DIR_ENV]: '  ' }), DEFAULT_OUTPUT_DIR);
  assert.equal(outputDir({ [OUTPUT_DIR_ENV]: ' /x/logs ' }), '/x/logs');
});

await test('backup names round-trip with the Go format', () => {
  const ms = Date.UTC(2026, 0, 2, 3, 4, 5, 6);
  const name = backupName('proxy', ms);
  assert.equal(name, 'proxy-2026-01-02T03-04-05.006.log');
  assert.equal(backupStamp('proxy', name), ms);
  assert.equal(backupStamp('proxy', `${name}.gz`), ms);
  assert.ok(Number.isNaN(backupStamp('proxy', 'proxy.log')));
  assert.ok(Number.isNaN(backupStamp('proxy', 'hive-2026-01-02T03-04-05.006.log')));
  assert.ok(Number.isNaN(backupStamp('hive', 'hive-output.log')));
});

await test('records carry time, stream and text; dir 0700, file 0600', () => {
  const dir = path.join(tempDir(), 'logs');
  const log = new OutputLog(dir, 'proxy', { now: () => Date.UTC(2026, 0, 2) });
  log.writeStream('stdout', 'one\ntw');
  log.writeStream('stdout', Buffer.from('o\r\n'));
  log.writeStream('stderr', 'partial');
  log.close();
  assert.equal(mode(dir), 0o700);
  assert.equal(mode(path.join(dir, 'proxy.log')), 0o600);
  assert.deepEqual(records(path.join(dir, 'proxy.log')), [
    { time: '2026-01-02T00:00:00.000Z', stream: 'stdout', text: 'one' },
    { time: '2026-01-02T00:00:00.000Z', stream: 'stdout', text: 'two' },
    { time: '2026-01-02T00:00:00.000Z', stream: 'stderr', text: 'partial' },
  ]);
  log.writeRecord('ignored after close\n');
});

await test('over-long lines are split', () => {
  const dir = tempDir();
  const log = new OutputLog(dir, 'proxy');
  log.writeStream('stdout', 'x'.repeat(LINE_MAX_BYTES + 10));
  log.close();
  const recs = records(path.join(dir, 'proxy.log'));
  assert.equal(recs.length, 2);
  assert.equal(recs[0].text.length, LINE_MAX_BYTES);
  assert.equal(recs[1].text.length, 10);
});

await test('rotation keeps at most 5 files and appends across opens', () => {
  const dir = tempDir();
  let now = Date.UTC(2026, 0, 2);
  const clock = () => (now += 1);
  const first = new OutputLog(dir, 'proxy', { maxBytes: 200, now: clock });
  first.writeStream('stdout', 'before restart\n');
  first.close();
  const log = new OutputLog(dir, 'proxy', { maxBytes: 200, now: clock });
  for (let i = 0; i < 40; i++) {
    log.writeStream('stdout', `line ${i}\n`);
  }
  log.close();
  const names = fs.readdirSync(dir).sort();
  assert.equal(names.length, MAX_FILES, names.join(','));
  assert.ok(names.includes('proxy.log'));
  for (const n of names) assert.equal(mode(path.join(dir, n)), 0o600, n);
  assert.equal(records(path.join(dir, names.filter((n) => n !== 'proxy.log').sort()[0])).length > 0, true);
  const current = records(path.join(dir, 'proxy.log'));
  assert.equal(current[current.length - 1].text, 'line 39');
});

await test('backups older than 7 days and a stale current file are pruned on open', () => {
  const dir = tempDir();
  const now = Date.UTC(2026, 0, 20);
  const old = path.join(dir, backupName('proxy', now - MAX_AGE_MS - 60000));
  const fresh = path.join(dir, backupName('proxy', now - 60000));
  const other = path.join(dir, 'hive-output.log');
  fs.writeFileSync(old, 'old\n');
  fs.writeFileSync(fresh, 'fresh\n');
  fs.writeFileSync(other, 'other\n');
  const current = path.join(dir, 'proxy.log');
  fs.writeFileSync(current, 'stale\n');
  const staleSec = (now - MAX_AGE_MS - 60000) / 1000;
  fs.utimesSync(current, staleSec, staleSec);
  const log = new OutputLog(dir, 'proxy', { now: () => now });
  log.close();
  assert.ok(!fs.existsSync(old), 'old backup kept');
  assert.ok(fs.existsSync(fresh), 'fresh backup removed');
  assert.ok(fs.existsSync(other), 'another source was touched');
  assert.equal(fs.statSync(current).size, 0);
});

await test('OutputLog refuses an empty dir or name and an unusable dir', () => {
  assert.throws(() => new OutputLog('', 'proxy'));
  assert.throws(() => new OutputLog(tempDir(), ''));
  const blocker = path.join(tempDir(), 'file');
  fs.writeFileSync(blocker, '');
  assert.throws(() => new OutputLog(path.join(blocker, 'logs'), 'proxy'));
});

function fakeProcess(env) {
  const out = { stdout: [], stderr: [] };
  const handlers = {};
  const proc = {
    env,
    stdout: { write(chunk) { out.stdout.push(String(chunk)); return true; } },
    stderr: { write(chunk) { out.stderr.push(String(chunk)); return 'orig'; } },
    on(ev, fn) { handlers[ev] = fn; },
  };
  return { proc, out, handlers };
}

await test('installOutputLog tees both streams without changing them', () => {
  const dir = tempDir();
  const { proc, out, handlers } = fakeProcess({ [OUTPUT_DIR_ENV]: dir, NODE_ENV: 'test' });
  const log = installOutputLog(proc);
  assert.ok(log);
  assert.equal(proc.stdout.write('hello\n'), true);
  assert.equal(proc.stderr.write(Buffer.from('oops\n')), 'orig');
  proc.stdout.write(new Uint8Array([104, 105, 10]));
  proc.stdout.write({ not: 'a chunk' });
  proc.stderr.write('tail');
  handlers.exit();
  assert.deepEqual(out.stdout, ['hello\n', '104,105,10', '[object Object]']);
  assert.deepEqual(out.stderr, ['oops\n', 'tail']);
  assert.deepEqual(records(path.join(dir, 'proxy.log')).map((r) => [r.stream, r.text]), [
    ['stdout', 'hello'],
    ['stderr', 'oops'],
    ['stdout', 'hi'],
    ['stderr', 'tail'],
  ]);
});

await test('installOutputLog is off under the test harness and when the dir is unusable', () => {
  assert.equal(installOutputLog(fakeProcess({ NODE_ENV: 'test' }).proc), null);
  const blocker = path.join(tempDir(), 'file');
  fs.writeFileSync(blocker, '');
  const { proc, out } = fakeProcess({ [OUTPUT_DIR_ENV]: path.join(blocker, 'logs') });
  assert.equal(installOutputLog(proc), null);
  proc.stdout.write('still printed\n');
  assert.deepEqual(out.stdout, ['still printed\n']);
});

function runProxy(env, until) {
  return new Promise((resolve, reject) => {
    const proc = spawn('node', ['server.js'], { cwd: __dirname, env, stdio: ['ignore', 'pipe', 'pipe'] });
    let stdout = '';
    let stderr = '';
    const timer = setTimeout(() => { proc.kill('SIGKILL'); reject(new Error(`proxy timeout\nstdout=${stdout}\nstderr=${stderr}`)); }, 15000);
    const check = () => {
      if (until && until(stdout)) proc.kill('SIGTERM');
    };
    proc.stdout.on('data', (d) => { stdout += d; check(); });
    proc.stderr.on('data', (d) => { stderr += d; });
    proc.on('error', reject);
    proc.on('exit', (code, signal) => { clearTimeout(timer); resolve({ code, signal, stdout, stderr }); });
  });
}

function baseEnv(dir) {
  // A clean environment: an inherited HIVE_SESSION_KEY / HIVE_HUB_SECRET /
  // HIVE_SESSION_PUBLIC_KEY would make the proxy hosted and skip the refusal.
  return {
    PATH: process.env.PATH || '',
    HOME: process.env.HOME || '',
    HIVE_PROXY_PORT: String(PROXY_PORT),
    HIVE_API_PORT: String(GO_PORT),
    HIVE_TTYD_PORT: String(TTYD_PORT),
    HIVE_STATIC_DIR: __dirname,
    [OUTPUT_DIR_ENV]: dir,
  };
}

await test('the [SECURITY] refusal keeps exit code 1 and lands in stderr and proxy.log', async () => {
  const dir = tempDir();
  const res = await runProxy({ ...baseEnv(dir), HIVE_DASHBOARD_TOKEN: '' });
  assert.equal(res.code, 1);
  assert.match(res.stderr, /\[SECURITY\] HIVE_DASHBOARD_TOKEN is not set/);
  const recs = records(path.join(dir, 'proxy.log'));
  const refusal = recs.find((r) => r.text.includes('[SECURITY]'));
  assert.ok(refusal, JSON.stringify(recs));
  assert.equal(refusal.stream, 'stderr');
  assert.ok(!Number.isNaN(Date.parse(refusal.time)));
  assert.equal(mode(path.join(dir, 'proxy.log')), 0o600);
  assert.equal(mode(dir), 0o700);
});

await test('the startup line lands in stdout and proxy.log', async () => {
  const dir = path.join(tempDir(), 'logs');
  const res = await runProxy(
    { ...baseEnv(dir), HIVE_DASHBOARD_TOKEN: 'output-log-test-token', NODE_ENV: 'test' },
    (out) => out.includes('[hive-proxy] Dashboard proxy on'),
  );
  assert.match(res.stdout, /\[hive-proxy\] Dashboard proxy on :19151/);
  const recs = records(path.join(dir, 'proxy.log'));
  const started = recs.find((r) => r.text.includes('[hive-proxy] Dashboard proxy on'));
  assert.ok(started, JSON.stringify(recs));
  assert.equal(started.stream, 'stdout');
});

if (failures > 0) {
  console.error(`${failures} output_log test(s) failed`);
  process.exit(1);
}
console.log('output_log tests passed');
