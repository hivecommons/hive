import { createServer, request as httpRequest } from 'http';
import { strict as assert } from 'assert';
import { spawn } from 'child_process';
import { writeFileSync, mkdtempSync, rmSync, readFileSync } from 'fs';
import { fileURLToPath } from 'url';
import { tmpdir } from 'os';
import { createHash } from 'crypto';
import path from 'path';

// Covers the script-src half of kubestellar/hive#3848 (#3907) on the proxy —
// the Node-side counterpart of csp_script_src_test.go, mirroring ADR-0016:
//
//   script-src      'self' …                  <- CSP2 fallback, now without
//                                               'unsafe-inline' (#3848 closed
//                                               the attribute half) and still
//                                               hash-free (a hash here changes
//                                               semantics on hash-aware
//                                               pre-CSP3 browsers)
//   script-src-elem 'self' cdn 'sha256-…'     <- inline <script> elements:
//                                               CLOSED — only the SPA's own
//                                               inline scripts can execute
//   script-src-attr 'none'                    <- on*= attributes: CLOSED by
//                                               the #3848 event-delegation
//                                               refactor
//
// Go-rendered documents (/contribute, /leaderboard, /snapshot) and ttyd's UI
// (/terminal) keep the blanket policy: the Go upstream stamps its own
// per-document hashes, and http-proxy-middleware copies upstream headers over
// ours — which test 3 PROVES empirically rather than assumes.

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY_PORT = 19061;
const GO_PORT = 19062;
const TTYD_PORT = 19063;

// The mock SPA carries two DISTINCT inline scripts (so the hash count is
// checkable), one external script (which must NOT be hashed — it is covered by
// URL sources), and an inline on*= handler (covered by script-src-attr).
const INLINE_ONE = "console.log('hive-inline-one');";
const INLINE_TWO = "window.__hive = 'inline-two';\nconsole.log(window.__hive);";
const INDEX_MARKER = 'hive-dashboard-index-marker';
const INDEX_HTML = `<!doctype html><html><head><title>Hive</title>
<script>${INLINE_ONE}</script>
<script src="/app.js"></script>
</head><body onload="console.log('handler')"><div id="${INDEX_MARKER}">dashboard</div>
<script>${INLINE_TWO}</script>
</body></html>`;

const sha256Source = (s) => `'sha256-${createHash('sha256').update(s, 'utf8').digest('base64')}'`;

// A recognisable upstream CSP so test 3 can prove upstream-wins on proxied docs.
const UPSTREAM_CSP = "default-src 'self'; script-src-elem 'self' 'sha256-UPSTREAM-SENTINEL='";

let staticDir;
let goServer;

function setupMockGoBackend() {
  const server = createServer((req, res) => {
    if (req.url === '/api/snapshot/frame-ancestors') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ origins: [] }));
      return;
    }
    if (req.url.startsWith('/contribute')) {
      res.writeHead(200, {
        'Content-Type': 'text/html; charset=utf-8',
        'Content-Security-Policy': UPSTREAM_CSP,
      });
      res.end('<!doctype html><html><body>contribute page</body></html>');
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

const directive = (csp, name) =>
  (csp || '').split(';').map((s) => s.trim()).find((s) => s === name || s.startsWith(`${name} `)) || '';

async function stop(proc) {
  if (!proc) return;
  proc.kill('SIGKILL');
  await new Promise((r) => proc.once('exit', r));
}

async function main() {
  staticDir = mkdtempSync(path.join(tmpdir(), 'hive-csp-elem-test-'));
  writeFileSync(path.join(staticDir, 'index.html'), INDEX_HTML);
  goServer = await setupMockGoBackend();

  const proxy = await startProxy();
  try {
    // -------------------------------------------------------------------
    // 1. The SPA document is served under the scoped policy, every one of
    //    its inline scripts hash-allowlisted, and the element half CLOSED.
    // -------------------------------------------------------------------
    const res = await get('/');
    assert.equal(res.status, 200, 'dashboard index must be served');
    assert.ok(res.body.includes(INDEX_MARKER), 'positive control: real page content came back');

    const csp = res.headers['content-security-policy'];
    assert.ok(csp, 'Content-Security-Policy header must be present');

    const elem = directive(csp, 'script-src-elem');
    assert.ok(elem, 'CSP must declare script-src-elem (#3848 part 1, ADR-0016)');
    assert.ok(!elem.includes("'unsafe-inline'"),
      `script-src-elem must not carry 'unsafe-inline' — the element half is CLOSED: ${elem}`);
    assert.ok(elem.includes("'self'"), `script-src-elem must allowlist 'self': ${elem}`);
    assert.ok(elem.includes('https://cdn.redoc.ly'),
      `script-src-elem must keep the pinned Redoc CDN for /api-docs: ${elem}`);
    for (const inline of [INLINE_ONE, INLINE_TWO]) {
      assert.ok(elem.includes(sha256Source(inline)),
        `served inline script not allowlisted (${sha256Source(inline)}) — the SPA would render BLANK: ${elem}`);
    }
    // The external script must be covered by URL sources, never hashed: hash
    // count equals the INLINE count exactly.
    const hashCount = (elem.match(/'sha256-[A-Za-z0-9+/]+=*'/g) || []).length;
    assert.equal(hashCount, 2,
      `script-src-elem must carry exactly the 2 inline-script hashes, got ${hashCount}: ${elem}`);

    const attr = directive(csp, 'script-src-attr');
    assert.ok(attr.includes("'none'") && !attr.includes("'unsafe-inline'"),
      `script-src-attr must be 'none' after the #3848 event-delegation refactor — ` +
      `inline on*= handlers are gone and must never come back: ${csp}`);

    // The CSP2 fallback: 'unsafe-inline' dropped with #3848, and
    // LOAD-BEARINGLY hash-free — hashes belong only in script-src-elem.
    const fallback = directive(csp, 'script-src');
    assert.ok(fallback.includes("'self'") && !fallback.includes("'unsafe-inline'"),
      `script-src fallback must keep 'self' and drop 'unsafe-inline' after #3848: ${fallback}`);
    assert.ok(!/'sha256-/.test(fallback),
      `script-src fallback must NEVER carry hashes (disables 'unsafe-inline' on ` +
      `hash-aware pre-CSP3 browsers): ${fallback}`);
    assert.ok(!fallback.includes("'unsafe-eval'"), 'script-src must never permit unsafe-eval');
    console.log('  ok: SPA served hash-allowlisted; element half closed, attr staged, fallback intact');

    // -------------------------------------------------------------------
    // 2. SPA deep links get the same scoped policy (same document served).
    // -------------------------------------------------------------------
    const deep = await get('/agents');
    assert.equal(deep.status, 200, 'SPA fallback route must render');
    assert.equal(directive(deep.headers['content-security-policy'], 'script-src-elem'), elem,
      'deep-link routes must carry the same script-src-elem allowlist');
    console.log('  ok: deep-link routes carry the same allowlist');

    // -------------------------------------------------------------------
    // 3. Proxied Go-rendered documents: the proxy sends the BLANKET policy
    //    (no elem directive it cannot compute), and the upstream's own CSP
    //    header wins end-to-end — proven, not assumed.
    // -------------------------------------------------------------------
    const contribute = await get('/contribute');
    assert.equal(contribute.status, 200, 'contribute page must proxy through');
    assert.equal(contribute.headers['content-security-policy'], UPSTREAM_CSP,
      'the Go upstream CSP must reach the client verbatim on proxied documents — ' +
      'http-proxy-middleware stopped copying upstream headers over ours, which the ' +
      'blanket-policy design depends on');
    console.log('  ok: upstream Go CSP wins on proxied documents (verified, not assumed)');

    // -------------------------------------------------------------------
    // 4. /api-docs (rendered by the proxy, zero inline scripts): scoped
    //    policy applies, and Redoc's pinned CDN stays loadable.
    // -------------------------------------------------------------------
    const docs = await get('/api-docs');
    assert.equal(docs.status, 200, 'api-docs must render');
    const docsElem = directive(docs.headers['content-security-policy'], 'script-src-elem');
    assert.ok(docsElem.includes('https://cdn.redoc.ly'),
      `api-docs must keep the SRI-pinned Redoc CDN loadable: ${docsElem}`);
    const docsInlineScripts = (docs.body.match(/<script\b(?![^>]*\ssrc\s*=)[^>]*>/gi) || []).length;
    assert.equal(docsInlineScripts, 0,
      'api-docs must carry no inline scripts — its policy hashes none');
    console.log('  ok: api-docs stays functional under the closed element half');

    // -------------------------------------------------------------------
    // 5. SOURCE GUARD: the fallback directive in server.js must stay literal
    //    and hash-free, so a refactor cannot quietly merge the hash list into
    //    it and trip the CSP2 'unsafe-inline'-suppression footgun.
    // -------------------------------------------------------------------
    const src = readFileSync(path.join(__dirname, 'server.js'), 'utf8');
    assert.ok(src.includes(`"script-src 'self' https://cdn.redoc.ly"`),
      'server.js must keep the literal, hash-free script-src fallback');
    assert.ok(/script-src-attr 'none'/.test(src),
      'server.js must declare script-src-attr explicitly');
    console.log('  ok: source guard — fallback literal and hash-free');
  } finally {
    await stop(proxy);
  }
}

main()
  .then(() => {
    goServer?.close();
    if (staticDir) rmSync(staticDir, { recursive: true, force: true });
    console.log('csp_script_src_elem.test.js: OK');
    process.exit(0);
  })
  .catch((err) => {
    goServer?.close();
    if (staticDir) rmSync(staticDir, { recursive: true, force: true });
    console.error('csp_script_src_elem.test.js: FAIL');
    console.error(err);
    process.exit(1);
  });
