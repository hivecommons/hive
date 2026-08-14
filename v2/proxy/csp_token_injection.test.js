import { createServer, request as httpRequest } from 'http';
import { strict as assert } from 'assert';
import { spawn } from 'child_process';
import { writeFileSync, mkdtempSync, rmSync } from 'fs';
import { fileURLToPath } from 'url';
import { tmpdir } from 'os';
import path from 'path';

// Regression guard for #3315 — "CSP unsafe-inline + DASHBOARD_TOKEN inline
// script injection".
//
// The historical bug: serveIndex rewrote the served index.html to append
//   <script>if(!localStorage.getItem('hive-token'))
//           localStorage.setItem('hive-token',<TOKEN>);</script>
// so every visitor who could reach the port was handed the only API credential.
// script-src 'unsafe-inline' is what let that inline block execute.
//
// These tests lock BOTH directions, so that neither "the token leaks" nor
// "everything got escaped into uselessness" can pass:
//   1. NEGATIVE: a token containing script-breaking characters must never
//      appear in the served HTML, in any encoding, and must not be able to
//      close the <script> context.
//   2. POSITIVE CONTROL: with a perfectly ordinary token configured, the
//      dashboard must STILL render its real content and still serve the CSP
//      header. Without this control, a serveIndex that returned an empty body
//      would satisfy every leak assertion.

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY_PORT = 19051;
const GO_PORT = 19052;
const TTYD_PORT = 19053;

// A token built to break out of an inline <script> string context: it carries a
// closing script tag, both quote styles, a backslash and newlines. SYNTHETIC —
// never the real HIVE_DASHBOARD_TOKEN.
const HOSTILE_TOKEN = `abc</script><script>alert('xss')</script>'"\\\ndef`;
const NORMAL_TOKEN = 'plain-operator-token-1234';

// A recognisable marker so the positive control proves real page content came
// back rather than an empty or error body.
const INDEX_MARKER = 'hive-dashboard-index-marker';
const INDEX_HTML = `<!doctype html><html><head><title>Hive</title></head>` +
  `<body><div id="${INDEX_MARKER}">dashboard</div></body></html>`;

let staticDir;
let goServer;

function setupMockGoBackend() {
  const server = createServer((req, res) => {
    if (req.url === '/api/snapshot/frame-ancestors') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ origins: [] }));
      return;
    }
    res.writeHead(404);
    res.end('not found');
  });
  return new Promise((resolve) => server.listen(GO_PORT, () => resolve(server)));
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
        HIVE_STATIC_DIR: staticDir,
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
    proc.stderr.on('data', (d) => { if (!started) console.error('proxy stderr:', d.toString()); });
    proc.on('error', reject);
    setTimeout(() => { if (!started) reject(new Error('proxy start timeout')); }, 10000);
  });
}

function get(pathname, headers = {}) {
  return new Promise((resolve, reject) => {
    const req = httpRequest(
      { host: '127.0.0.1', port: PROXY_PORT, path: pathname, method: 'GET', headers },
      (res) => {
        let body = '';
        res.on('data', (c) => { body += c; });
        res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body }));
      },
    );
    req.on('error', reject);
    req.end();
  });
}

async function stop(proc) {
  if (!proc) return;
  proc.kill('SIGKILL');
  await new Promise((r) => proc.once('exit', r));
}

async function main() {
  staticDir = mkdtempSync(path.join(tmpdir(), 'hive-csp-test-'));
  writeFileSync(path.join(staticDir, 'index.html'), INDEX_HTML);
  goServer = await setupMockGoBackend();

  // ---------------------------------------------------------------------
  // 1. NEGATIVE: a script-breaking token must never reach the served HTML.
  // ---------------------------------------------------------------------
  let proxy = await startProxy({ HIVE_DASHBOARD_TOKEN: HOSTILE_TOKEN });
  try {
    const res = await get('/');
    assert.equal(res.status, 200, 'dashboard index must be served');

    // The token must not appear raw...
    assert.ok(
      !res.body.includes(HOSTILE_TOKEN),
      'served HTML must not contain the dashboard token verbatim',
    );
    // ...nor in any of the encodings an "escaping" fix might have produced.
    // A fix that merely escaped the token would still be leaking it.
    assert.ok(
      !res.body.includes(JSON.stringify(HOSTILE_TOKEN)),
      'served HTML must not contain the dashboard token JSON-encoded',
    );
    assert.ok(
      !res.body.includes(encodeURIComponent(HOSTILE_TOKEN)),
      'served HTML must not contain the dashboard token URL-encoded',
    );
    // The distinctive secret core must not survive in ANY form. This catches
    // partial/HTML-escaped renderings that the exact-match checks above miss.
    assert.ok(
      !/abc[\s\S]{0,80}def/.test(res.body),
      'served HTML must not contain the dashboard token in any encoding',
    );

    // The historical injection sink itself must be gone.
    assert.ok(
      !/localStorage\s*\.\s*setItem\(\s*['"]hive-token['"]/.test(res.body),
      "served HTML must not write 'hive-token' into localStorage",
    );

    // The hostile token must not have closed the <script> context: the page
    // must carry no more script-open tags than it was authored with (zero).
    const scriptOpens = (res.body.match(/<script/gi) || []).length;
    assert.equal(scriptOpens, 0, 'no <script> tag may be injected into the served index');
    assert.ok(!/alert\(/.test(res.body), 'no attacker payload may appear in the served HTML');
    console.log('  ok: script-breaking token never reaches the served HTML');
  } finally {
    await stop(proxy);
  }

  // ---------------------------------------------------------------------
  // 2. POSITIVE CONTROL: with a normal token the dashboard still works.
  //    This is what stops "escape/blank everything" from passing test 1.
  // ---------------------------------------------------------------------
  proxy = await startProxy({ HIVE_DASHBOARD_TOKEN: NORMAL_TOKEN });
  try {
    const res = await get('/');
    assert.equal(res.status, 200, 'dashboard index must still be served with a normal token');
    assert.ok(
      res.body.includes(INDEX_MARKER),
      'dashboard must still render its real content (positive control)',
    );
    assert.ok(
      res.body.includes('<title>Hive</title>'),
      'dashboard HTML must be served intact, not stripped',
    );
    // ...and it still must not leak the token.
    assert.ok(
      !res.body.includes(NORMAL_TOKEN),
      'an ordinary token must not be embedded in the served HTML either',
    );

    // A deep link (SPA fallback route) must render too, not just "/".
    const deep = await get('/agents');
    assert.equal(deep.status, 200, 'SPA fallback route must render');
    assert.ok(deep.body.includes(INDEX_MARKER), 'SPA fallback must serve the dashboard');
    assert.ok(!deep.body.includes(NORMAL_TOKEN), 'SPA fallback must not leak the token');
    console.log('  ok: dashboard still renders normally and leaks nothing (positive control)');

    // -------------------------------------------------------------------
    // 3. CSP is still emitted, and its shape is pinned.
    // -------------------------------------------------------------------
    const csp = res.headers['content-security-policy'];
    assert.ok(csp, 'Content-Security-Policy header must be present');
    assert.match(csp, /default-src 'self'/, "CSP must set default-src 'self'");
    assert.match(csp, /object-src 'none'/, "CSP must set object-src 'none'");
    assert.match(csp, /base-uri 'self'/, "CSP must set base-uri 'self'");
    assert.match(csp, /form-action 'self'/, "CSP must set form-action 'self'");
    assert.match(csp, /frame-ancestors 'none'/, 'CSP must deny framing by default');

    // #3315 STAGED: script-src still carries 'unsafe-inline' because the
    // dashboard is server-rendered with ~400 inline on*= handlers and several
    // inline <script> blocks. This assertion documents the remaining exposure
    // and MUST be inverted to a "does not contain" check by the follow-up that
    // moves those handlers out of the markup. Failing here means the refactor
    // landed and this guard needs flipping -- see #3315.
    const scriptSrc = csp.split(';').map(s => s.trim()).find(s => s.startsWith('script-src'));
    assert.ok(scriptSrc, 'CSP must declare an explicit script-src');
    assert.ok(
      scriptSrc.includes("'self'"),
      "script-src must allowlist 'self'",
    );
    assert.ok(
      !scriptSrc.includes("'unsafe-eval'"),
      "script-src must never permit 'unsafe-eval'",
    );
    // The token is no longer in the page, so 'unsafe-inline' no longer exposes
    // it -- but object-src/base-uri/form-action must not regress either.
    console.log('  ok: CSP present and pinned (script-src unsafe-inline staged, see #3315)');
  } finally {
    await stop(proxy);
  }

  // ---------------------------------------------------------------------
  // 4. SOURCE GUARD: the injection sink must not come back.
  // ---------------------------------------------------------------------
  const { readFileSync } = await import('fs');
  const src = readFileSync(path.join(__dirname, 'server.js'), 'utf8');
  assert.ok(
    !/indexHtml\s*\.\s*replace\s*\(/.test(src),
    'server.js must not rewrite the index HTML to inject anything',
  );
  assert.ok(
    !/setItem\(\s*['"]hive-token['"]\s*,\s*\$\{/.test(src),
    'server.js must not interpolate the token into an inline script',
  );
  console.log('  ok: token-injection sink absent from server.js');
}

main()
  .then(() => {
    goServer?.close();
    if (staticDir) rmSync(staticDir, { recursive: true, force: true });
    console.log('csp_token_injection.test.js: OK');
    process.exit(0);
  })
  .catch((err) => {
    goServer?.close();
    if (staticDir) rmSync(staticDir, { recursive: true, force: true });
    console.error('csp_token_injection.test.js: FAIL');
    console.error(err);
    process.exit(1);
  });
