// #4476 — the container healthcheck probes BOTH listeners, so :3001 must answer
// /api/health without credentials, in every mode the proxy can boot in.
//
// The unit and the Compose service now run
//
//   curl -sf http://127.0.0.1:3002/api/health && curl -sf http://127.0.0.1:3001/api/health
//
// as their health command. 3002 is the Go API; 3001 is this proxy, which is
// what src/deploy/nginx.conf's `upstream hive_api { server hive:3001; }` points
// at and therefore what every operator actually reaches. Probing 3002 alone let
// hive.service report `healthy` for the two minutes in which the dashboard was
// refusing connections.
//
// That makes two properties load-bearing, and neither is obvious from the unit:
//
//  1. GET /api/health on :3001 must NOT require a credential. If it ever starts
//     answering 401, the healthcheck can never pass, Notify=healthy holds the
//     unit in `activating` for the whole TimeoutStartSec, and --rm then deletes
//     the container that held the evidence. That failure would look exactly
//     like #4367 and would ship green through every dry-run gate.
//  2. It must not require one in HOSTED mode either, where the proxy boots with
//     HIVE_DASHBOARD_TOKEN deliberately unset (identity comes from the hub
//     cookie). A probe that went red there would break every hosted hive.
//
// The mutating-endpoint assertion below is what keeps this test honest: it
// proves auth is genuinely enforced in the same process that serves the health
// endpoint unauthenticated, so a green run cannot mean "auth was simply off".

import { createServer, request as httpRequest } from 'http';
import { strict as assert } from 'assert';
import { spawn } from 'child_process';
import { fileURLToPath } from 'url';
import path from 'path';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const PROXY_PORT = 19061;
const GO_PORT = 19062;
const TTYD_PORT = 19063;
const TOKEN = 'health-probe-test-token';

let goServer = null;
const children = [];

function startMockGoApi() {
  const server = createServer((req, res) => {
    if (req.url === '/api/health') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end('{"status":"ok"}');
      return;
    }
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end('{"ok":true}');
  });
  return new Promise(resolve => server.listen(GO_PORT, () => resolve(server)));
}

function startProxy(extraEnv = {}) {
  return new Promise((resolve, reject) => {
    const proc = spawn('node', ['server.js'], {
      cwd: __dirname,
      env: {
        ...process.env,
        HIVE_PROXY_PORT: String(PROXY_PORT),
        HIVE_API_PORT: String(GO_PORT),
        HIVE_TTYD_PORT: String(TTYD_PORT),
        HIVE_DASHBOARD_TOKEN: '',
        HIVE_STATIC_DIR: __dirname,
        NODE_ENV: 'test',
        ...extraEnv,
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    children.push(proc);
    let started = false;
    proc.stdout.on('data', (d) => {
      if (!started && d.toString().includes('hive-proxy')) {
        started = true;
        resolve(proc);
      }
    });
    proc.stderr.on('data', (d) => { if (!started) console.error('proxy stderr:', d.toString()); });
    proc.on('error', reject);
    setTimeout(() => { if (!started) reject(new Error('proxy start timeout')); }, 10000);
  });
}

// Boots the proxy and resolves with its exit code instead of waiting for a
// listener — for the case where it is expected to refuse to start.
function startProxyExpectingExit(extraEnv = {}) {
  return new Promise((resolve, reject) => {
    const proc = spawn('node', ['server.js'], {
      cwd: __dirname,
      env: {
        ...process.env,
        HIVE_PROXY_PORT: String(PROXY_PORT),
        HIVE_API_PORT: String(GO_PORT),
        HIVE_TTYD_PORT: String(TTYD_PORT),
        HIVE_DASHBOARD_TOKEN: '',
        HIVE_STATIC_DIR: __dirname,
        ...extraEnv,
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    children.push(proc);
    let stderr = '';
    proc.stderr.on('data', (d) => { stderr += d.toString(); });
    proc.on('error', reject);
    proc.on('exit', (code) => resolve({ code, stderr }));
    setTimeout(() => { proc.kill('SIGKILL'); reject(new Error('proxy did not exit')); }, 10000);
  });
}

function stopProxy(proc) {
  return new Promise((resolve) => {
    if (!proc || proc.exitCode !== null) return resolve();
    proc.on('exit', () => resolve());
    proc.kill('SIGKILL');
  });
}

// One request. `connect` means nothing was listening — the exact shape of the
// #4476 failure, and what `curl -sf` reports as exit 7.
function req(method, path, headers = {}) {
  return new Promise((resolve, reject) => {
    const r = httpRequest(
      { host: '127.0.0.1', port: PROXY_PORT, path, method, headers },
      (res) => {
        let body = '';
        res.on('data', (c) => { body += c; });
        res.on('end', () => resolve({ status: res.statusCode, body }));
      },
    );
    r.on('error', (err) => {
      if (err.code === 'ECONNREFUSED') return resolve({ status: 0, body: '', refused: true });
      reject(err);
    });
    r.end();
  });
}

const results = [];
async function check(name, fn) {
  try {
    await fn();
    results.push([true, name]);
    console.log(`  PASS  ${name}`);
  } catch (err) {
    results.push([false, name]);
    console.error(`  FAIL  ${name}: ${err.message}`);
  }
}

async function main() {
  goServer = await startMockGoApi();

  // ── self-hosted: HIVE_DASHBOARD_TOKEN set, which is what the Quadlet doc
  //    tells an operator to put in %E/hive/hive.env.
  let proxy = await startProxy({ HIVE_DASHBOARD_TOKEN: TOKEN });

  await check('self-hosted: GET /api/health on the proxy port answers 200 with no credential', async () => {
    const res = await req('GET', '/api/health');
    assert.equal(res.status, 200, `expected 200, got ${res.status} — the healthcheck would never pass`);
    assert.match(res.body, /"status"\s*:\s*"ok"/);
  });

  await check('self-hosted: a mutating endpoint still requires the token', async () => {
    const res = await req('POST', '/api/agents');
    assert.equal(res.status, 401, `expected 401, got ${res.status} — auth is off, so the assertion above proves nothing`);
  });

  await stopProxy(proxy);

  // ── hosted: no token by design; identity is the hub cookie. NODE_ENV is left
  //    unset here so this exercises the real boot path, not the test opt-out.
  proxy = await startProxy({ HIVE_HUB_SECRET: 'hub-secret-for-test', HIVE_ID: 'test-hive', NODE_ENV: '' });

  await check('hosted: GET /api/health answers 200 with no token configured at all', async () => {
    const res = await req('GET', '/api/health');
    assert.equal(res.status, 200, `expected 200, got ${res.status} — the probe would go red on every hosted hive`);
  });

  await stopProxy(proxy);

  // ── the #4476 failure itself: self-hosted with no token. The proxy refuses to
  //    start, so :3001 has no listener while the Go API on :3002 answers 200.
  //    A probe that reads only 3002 calls this healthy.
  await check('self-hosted with no token: the proxy refuses to start, leaving :3001 dead', async () => {
    const { code, stderr } = await startProxyExpectingExit({});
    assert.equal(code, 1, `expected exit 1, got ${code}`);
    assert.match(stderr, /HIVE_DASHBOARD_TOKEN/);
    const res = await req('GET', '/api/health');
    assert.equal(res.refused, true, 'expected connection refused on the proxy port');
  });

  // ── and the probe must see an unreachable Go API through the proxy, since
  //    that is the path nginx takes.
  proxy = await startProxy({ HIVE_DASHBOARD_TOKEN: TOKEN });
  await new Promise((resolve) => goServer.close(resolve));
  goServer = null;

  await check('proxy reports a non-2xx when the Go API behind it is unreachable', async () => {
    const res = await req('GET', '/api/health');
    assert.ok(res.status >= 500, `expected 5xx, got ${res.status} — curl -sf must fail here`);
  });

  await stopProxy(proxy);

  const failed = results.filter(([passed]) => !passed).length;
  console.log(`\n${results.length - failed}/${results.length} health-probe assertions passed`);
  if (failed) process.exit(1);
}

main()
  .catch((err) => { console.error(err); process.exitCode = 1; })
  .finally(async () => {
    for (const proc of children) await stopProxy(proc);
    if (goServer) await new Promise((resolve) => goServer.close(resolve));
  });
