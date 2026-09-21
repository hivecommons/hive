'use strict';

const { describe, it, before, after } = require('node:test');
const assert = require('node:assert/strict');
const http = require('http');
const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
const express = require('express');

const TEST_PORT = 14001 + Math.floor(Math.random() * 1000);
const OWNER_TOKEN = 'owner-token-for-nous-authz-tests';

function secureCompare(a, b) {
  const left = Buffer.from(String(a || ''));
  const right = Buffer.from(String(b || ''));
  return left.length > 0 && left.length === right.length && crypto.timingSafeEqual(left, right);
}

function bearerMatchesToken(headerValue, token) {
  const value = String(headerValue || '').trim();
  return secureCompare(value, 'Bearer ' + token) || secureCompare(value, token);
}

function requireLegacyOwnerRole(req, res) {
  const token = OWNER_TOKEN;
  const role = String(req.get('X-Hive-Role') || '').trim().toLowerCase();
  const proxyProof = req.get('X-Hive-Proxy-Auth');
  const internal = req.get('X-Hive-Internal');
  const bearer = req.get('Authorization');

  if (token && (bearerMatchesToken(bearer, token) || secureCompare(internal, token) || (role === 'owner' && secureCompare(proxyProof, token)))) {
    return true;
  }

  res.status(403).json({ error: 'owner access required' });
  return false;
}

function httpRequest(method, urlPath, body, headers = {}) {
  return new Promise((resolve, reject) => {
    const opts = { hostname: '127.0.0.1', port: TEST_PORT, path: urlPath, method, headers: { ...headers } };
    if (body) {
      const data = JSON.stringify(body);
      opts.headers['Content-Type'] = 'application/json';
      opts.headers['Content-Length'] = Buffer.byteLength(data);
    }
    const req = http.request(opts, (res) => {
      let chunks = '';
      res.on('data', (d) => { chunks += d; });
      res.on('end', () => {
        try { resolve({ status: res.statusCode, body: JSON.parse(chunks) }); }
        catch (_) { resolve({ status: res.statusCode, body: chunks }); }
      });
    });
    req.on('error', reject);
    if (body) req.write(JSON.stringify(body));
    req.end();
  });
}

function createTestServer() {
  const app = express();
  app.use(express.json());

  const gate = (handler) => (req, res) => {
    if (!requireLegacyOwnerRole(req, res)) return;
    handler(req, res);
  };

  app.post('/api/nous/approve', gate((_req, res) => res.json({ ok: true, experimentId: 'exp-1' })));
  app.post('/api/nous/abort', gate((_req, res) => res.json({ ok: true, aborted: 'exp-1' })));
  app.put('/api/nous/mode', gate((req, res) => res.json({ ok: true, mode: req.body.mode, previousMode: 'observe' })));
  app.put('/api/nous/scope', gate((req, res) => res.json({ ok: true, scope: req.body.scope, previousScope: 'governor' })));
  app.put('/api/nous/gate-decision', gate((_req, res) => res.json({ ok: true })));
  app.post('/api/nous/gate-respond', gate((req, res) => res.json({ ok: true, decision: req.body.decision })));

  return app.listen(TEST_PORT);
}

const mutationCases = [
  ['POST', '/api/nous/approve', {}],
  ['POST', '/api/nous/abort', {}],
  ['PUT', '/api/nous/mode', { mode: 'suggest' }],
  ['PUT', '/api/nous/scope', { scope: 'repo' }],
  ['PUT', '/api/nous/gate-decision', { decision: 'approve', reason: 'test' }],
  ['POST', '/api/nous/gate-respond', { decision: 'approve' }],
];

describe('legacy Nous mutation authorization', () => {
  let server;

  before(async () => {
    server = createTestServer();
    await new Promise((resolve) => { server.on('listening', resolve); });
  });

  after(() => {
    if (server) server.close();
  });


  it('keeps the real legacy server routes behind requireLegacyOwnerRole', () => {
    const serverSource = fs.readFileSync(path.join(__dirname, '..', 'server.js'), 'utf8');
    for (const [_method, route] of mutationCases) {
      const routeIndex = serverSource.indexOf(route);
      assert.notEqual(routeIndex, -1, `${route} route missing from server.js`);
      const routeBody = serverSource.slice(routeIndex, routeIndex + 260);
      assert.match(routeBody, /requireLegacyOwnerRole\(req, res\)/, `${route} must require owner role`);
    }
  });


  it('keeps the gate client compatible with the protected gate-decision route', () => {
    const gateSource = fs.readFileSync(path.join(__dirname, '..', '..', 'bin', 'nous-hive-gate.py'), 'utf8');
    assert.match(gateSource, /HIVE_DASHBOARD_TOKEN/);
    assert.match(gateSource, /Authorization/);
    assert.match(gateSource, /Bearer /);
  });

  for (const [method, route, body] of mutationCases) {
    it(`rejects anonymous ${method} ${route}`, async () => {
      const res = await httpRequest(method, route, body);
      assert.equal(res.status, 403);
      assert.match(res.body.error, /owner access required/);
    });

    it(`accepts owner bearer for ${method} ${route}`, async () => {
      const res = await httpRequest(method, route, body, { Authorization: 'Bearer ' + OWNER_TOKEN });
      assert.equal(res.status, 200);
      assert.equal(res.body.ok, true);
    });
  }

  it('accepts proof-verified owner proxy identity', async () => {
    const res = await httpRequest('POST', '/api/nous/approve', {}, {
      'X-Hive-Role': 'owner',
      'X-Hive-Proxy-Auth': OWNER_TOKEN,
    });
    assert.equal(res.status, 200);
    assert.equal(res.body.ok, true);
  });
});
