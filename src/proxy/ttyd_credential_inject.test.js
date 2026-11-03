// /terminal must present ttyd's own basic-auth credential on every forwarded
// request. deploy/entrypoint.sh starts ttyd with -c hive:<HIVE_DASHBOARD_TOKEN>
// (or an explicit HIVE_TTYD_CREDENTIAL); the proxy used to forward requests
// without any Authorization, so the browser the proxy had already
// authenticated was re-challenged by ttyd with a native user/password dialog.
//
// Pins, through the real proxy against a mock ttyd:
//   1. HIVE_TTYD_CREDENTIAL → forwarded as Basic <base64(credential)>
//   2. a client-supplied Authorization is replaced, never forwarded
//   3. with no credential configured, no Authorization reaches ttyd
import { createServer, request as httpRequest } from 'http';
import { strict as assert } from 'assert';
import { spawn } from 'child_process';
import { fileURLToPath } from 'url';
import path from 'path';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY_PORT = 19021;
const GO_PORT = 19022;
const TTYD_PORT = 19023;
const CREDENTIAL = 'ops:override-pass';
const EXPECTED = 'Basic ' + Buffer.from(CREDENTIAL).toString('base64');

let goServer, ttydServer, proxyProcess;
let lastAuthorization;

function setupMockGoBackend() {
  const server = createServer((req, res) => {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end('{}');
  });
  return new Promise(resolve => server.listen(GO_PORT, () => resolve(server)));
}

function setupMockTtyd() {
  const server = createServer((req, res) => {
    lastAuthorization = req.headers.authorization;
    res.writeHead(200, { 'Content-Type': 'text/html' });
    res.end('ttyd-ok');
  });
  return new Promise(resolve => server.listen(TTYD_PORT, () => resolve(server)));
}

function startProxy(extraEnv) {
  return new Promise((resolve, reject) => {
    const proc = spawn('node', ['server.js'], {
      cwd: __dirname,
      env: {
        ...process.env,
        HIVE_PROXY_PORT: String(PROXY_PORT),
        HIVE_API_PORT: String(GO_PORT),
        HIVE_TTYD_PORT: String(TTYD_PORT),
        HIVE_DASHBOARD_TOKEN: '',
        HIVE_TTYD_CREDENTIAL: '',
        HIVE_STATIC_DIR: __dirname,
        NODE_ENV: 'test',
        ...extraEnv,
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let started = false;
    proc.stdout.on('data', (d) => {
      if (!started && d.toString().includes('hive-proxy')) {
        started = true;
        resolve(proc);
      }
    });
    proc.on('error', reject);
    setTimeout(() => { if (!started) reject(new Error('proxy start timeout')); }, 10000);
  });
}

function stopProxy() {
  return new Promise((resolve) => {
    if (!proxyProcess) { resolve(); return; }
    proxyProcess.once('exit', () => resolve());
    proxyProcess.kill();
  });
}

function get(headers) {
  return new Promise((resolve, reject) => {
    const req = httpRequest({
      host: '127.0.0.1',
      port: PROXY_PORT,
      path: '/terminal/?arg=hive-scanner',
      headers,
    }, (res) => {
      let body = '';
      res.on('data', d => { body += d; });
      res.on('end', () => resolve({ status: res.statusCode, body }));
    });
    req.on('error', reject);
    req.end();
  });
}

async function main() {
  goServer = await setupMockGoBackend();
  ttydServer = await setupMockTtyd();

  proxyProcess = await startProxy({ HIVE_TTYD_CREDENTIAL: CREDENTIAL });
  let r = await get({});
  assert.equal(r.status, 200, `credentialed GET got ${r.status}`);
  assert.equal(lastAuthorization, EXPECTED, 'ttyd must receive the configured credential');

  r = await get({ Authorization: 'Bearer browser-dashboard-token' });
  assert.equal(r.status, 200, `GET with client Authorization got ${r.status}`);
  assert.equal(lastAuthorization, EXPECTED, 'client Authorization must be replaced, not forwarded');
  await stopProxy();

  proxyProcess = await startProxy({});
  r = await get({ Authorization: 'Basic ' + Buffer.from('hive:guess').toString('base64') });
  assert.equal(r.status, 200, `uncredentialed GET got ${r.status}`);
  assert.equal(lastAuthorization, undefined, 'no credential configured → no Authorization forwarded');

  console.log('ttyd_credential_inject: all assertions passed');
}

function cleanup() {
  if (proxyProcess) proxyProcess.kill();
  if (goServer) goServer.close();
  if (ttydServer) ttydServer.close();
}

main()
  .then(() => { cleanup(); process.exit(0); })
  .catch((err) => { console.error(err); cleanup(); process.exit(1); });
