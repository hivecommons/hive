// Credentialed requests to /terminal must survive ttyd's
// one-request-per-connection behavior.
//
// ttyd (libwebsockets) does not serve a second HTTP request on a reused TCP
// connection — it hangs up. http-proxy-middleware's engine (httpxy) pools
// upstream sockets in its own keepAlive:true default agents whenever no
// explicit agent option is given, so back-to-back requests (the document,
// then /token, then the websocket handshake) deterministically picked the
// dead pooled socket and surfaced as 502 "Terminal unavailable" WITH THE
// CORRECT PASSWORD. Measured on a live hive: 401 → 502 → 200, repeating
// (every fresh-socket request pools a socket ttyd then kills, so every
// second request died). The fix gives ttydProxy a keepAlive:false agent;
// this test pins a run of consecutive requests through the real proxy
// against a mock ttyd reproducing the one-request-per-connection behavior.
//
// The proxy now presents ttyd's credential itself (HIVE_TTYD_CREDENTIAL /
// hive:<HIVE_DASHBOARD_TOKEN>) rather than relaying a browser basic-auth
// dialog, so the mock's 401 branch is the proxy FAILING to inject it.
import { createServer, request as httpRequest } from 'http';
import { strict as assert } from 'assert';
import { spawn } from 'child_process';
import { fileURLToPath } from 'url';
import path from 'path';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY_PORT = 19011;
const GO_PORT = 19012;
const TTYD_PORT = 19013;
const CRED = 'Basic ' + Buffer.from('hive:sekrit').toString('base64');

let goServer, ttydServer, proxyProcess;

// Minimal Go-API stand-in: the proxy only needs it to exist.
function setupMockGoBackend() {
  const server = createServer((req, res) => {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end('{}');
  });
  return new Promise(resolve => server.listen(GO_PORT, () => resolve(server)));
}

// Mock ttyd with the two behaviors under test: basic auth (401 without the
// credential, 200 with it) and libwebsockets' one-request-per-connection
// lifecycle — any second request arriving on a reused socket is destroyed
// without a response, which is what the proxy's pooled socket runs into.
function setupMockTtyd() {
  const served = new WeakSet();
  const server = createServer((req, res) => {
    if (served.has(req.socket)) {
      req.socket.destroy();
      return;
    }
    served.add(req.socket);
    if (req.headers.authorization !== CRED) {
      res.writeHead(401, { 'WWW-Authenticate': 'Basic realm="ttyd"' });
      res.end();
      return;
    }
    res.writeHead(200, { 'Content-Type': 'text/html' });
    res.end('ttyd-ok');
  });
  return new Promise(resolve => server.listen(TTYD_PORT, () => resolve(server)));
}

function startProxy() {
  return new Promise((resolve, reject) => {
    const proc = spawn('node', ['server.js'], {
      cwd: __dirname,
      env: {
        ...process.env,
        HIVE_PROXY_PORT: String(PROXY_PORT),
        HIVE_API_PORT: String(GO_PORT),
        HIVE_TTYD_PORT: String(TTYD_PORT),
        HIVE_DASHBOARD_TOKEN: '',
        HIVE_TTYD_CREDENTIAL: 'hive:sekrit',
        HIVE_STATIC_DIR: __dirname,
        NODE_ENV: 'test',
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

function get() {
  return new Promise((resolve, reject) => {
    const req = httpRequest({
      host: '127.0.0.1',
      port: PROXY_PORT,
      path: '/terminal/?arg=hive-scanner',
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
  proxyProcess = await startProxy();
  await new Promise(r => setTimeout(r, 500));

  // Before the fix, the second request was the 502: the first left a pooled
  // socket ttyd will not serve again.
  const first = await get();
  assert.equal(first.status, 200, `first GET must reach ttyd with the injected credential, got ${first.status}`);
  assert.equal(first.body, 'ttyd-ok');

  // Let the upstream socket settle into the agent's free pool — reuse of a
  // pooled (dead) socket is exactly the failure under test.
  await new Promise(r => setTimeout(r, 150));

  const second = await get();
  assert.equal(second.status, 200,
    `second GET must reach ttyd on a fresh connection, got ${second.status} ` +
    `(502 here means the proxy reused a pooled socket ttyd already considers dead)`);
  assert.equal(second.body, 'ttyd-ok');

  // And it must not be luck: every subsequent request pools a socket the
  // mock kills, so any reuse regression fails on one of these.
  for (let i = 0; i < 3; i++) {
    await new Promise(r => setTimeout(r, 150));
    const again = await get();
    assert.equal(again.status, 200, `repeat GET #${i + 1} got ${again.status}`);
  }

  console.log('ttyd_auth_retry: all assertions passed');
}

// Cleanup must run BEFORE process.exit — an exit inside .then() skips
// .finally(), leaving the spawned proxy orphaned on its port, where it
// silently serves every later run of this test regardless of what server.js
// on disk says (measured: exactly that produced a false pass of this test).
function cleanup() {
  if (proxyProcess) proxyProcess.kill();
  if (goServer) goServer.close();
  if (ttydServer) ttydServer.close();
}

main()
  .then(() => { cleanup(); process.exit(0); })
  .catch((err) => { console.error(err); cleanup(); process.exit(1); });
