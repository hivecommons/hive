// The GitHub App setup callback must reach the Go API through the gateway.
//
// After the operator installs the App, GitHub redirects their browser to
// /gh-setup?installation_id=...&setup_action=install. The Go API's
// GET /gh-setup handler verifies the installation and persists the
// installation_id. Before this route existed the gateway had no proxy for
// /gh-setup, so the request fell through to the SPA fallback: the dashboard
// rendered at the /gh-setup URL, the Go handler never ran, and the
// documented setup flow silently did nothing — measured on a live gateway
// deployment, where the operator had to paste the installation ID by hand.
// This test drives the exact redirect request through the real proxy and
// pins that it reaches the backend with its query string intact.
import { createServer, request as httpRequest } from 'http';
import { strict as assert } from 'assert';
import { spawn } from 'child_process';
import { fileURLToPath } from 'url';
import path from 'path';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY_PORT = 19031;
const GO_PORT = 19032;
const TTYD_PORT = 19033;

let goServer, ttydServer, proxyProcess;
const seenRequests = [];

// Go-API stand-in that implements the real handler's observable behavior:
// record the request, answer 303 to /?ghSetup=ok — the redirect the browser
// must receive for the flow to complete.
function setupMockGoBackend() {
  const server = createServer((req, res) => {
    if (req.url.startsWith('/gh-setup')) {
      seenRequests.push(req.url);
      res.writeHead(303, { Location: '/?ghSetup=ok' });
      res.end();
      return;
    }
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end('{}');
  });
  return new Promise(resolve => server.listen(GO_PORT, () => resolve(server)));
}

function setupMockTtyd() {
  const server = createServer((req, res) => {
    res.writeHead(200);
    res.end('ttyd');
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

function get(reqPath) {
  return new Promise((resolve, reject) => {
    const req = httpRequest({
      host: '127.0.0.1',
      port: PROXY_PORT,
      path: reqPath,
    }, (res) => {
      let body = '';
      res.on('data', d => { body += d; });
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body }));
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

  // The exact URL shape GitHub redirects to after an App install.
  const callback = '/gh-setup?installation_id=156191969&setup_action=install';
  const res = await get(callback);

  // Without the proxy route this fell through to the SPA fallback (a 200
  // with HTML, or a 404 from static) and the backend never saw the request.
  assert.equal(res.status, 303,
    `gh-setup callback must be proxied to the Go API and return its redirect, got ${res.status}`);
  assert.equal(res.headers.location, '/?ghSetup=ok',
    `redirect must carry the Go handler's Location, got ${res.headers.location}`);
  assert.equal(seenRequests.length, 1, 'backend must have seen exactly one gh-setup request');
  assert.equal(seenRequests[0], callback,
    `query string must survive the proxy intact, backend saw ${seenRequests[0]}`);

  console.log('gh_setup_proxy: all assertions passed');
}

// Cleanup must run BEFORE process.exit — an exit inside .then() skips
// .finally(), leaving the spawned proxy orphaned on its port where it serves
// later runs of this test regardless of what server.js on disk says.
function cleanup() {
  if (proxyProcess) proxyProcess.kill();
  if (goServer) goServer.close();
  if (ttydServer) ttydServer.close();
}

main()
  .then(() => { cleanup(); process.exit(0); })
  .catch((err) => { console.error(err); cleanup(); process.exit(1); });
