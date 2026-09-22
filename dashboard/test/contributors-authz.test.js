'use strict';

// #8165: the legacy contributor-management REST surface must (1) never serve
// the stored plaintext registration token and (2) require owner authorization
// on the trust/revoke mutations — parity with the Go dashboard, which blanks
// TokenPlain before every response and fail-closes requireContributorWrite.
//
// Same shape as nous-authz.test.js: the authz/sanitizer logic is replicated
// here against a stub app, and a source-level guard pins the real server.js
// routes to the same helpers so a regression there fails this suite.

const { describe, it, before, after } = require('node:test');
const assert = require('node:assert/strict');
const http = require('http');
const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
const express = require('express');

const TEST_PORT = 15001 + Math.floor(Math.random() * 1000);
const OWNER_TOKEN = 'owner-token-for-contributor-authz-tests';

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

function publicContributorView(profile) {
  const { registration_token_plain, ...rest } = profile;
  return rest;
}

const PROFILES = [
  {
    contributor_id: 'contrib-1',
    github_username: 'alice',
    trust_tier: 'contributor',
    registration_token: crypto.createHash('sha256').update('alice-plain-token').digest('hex'),
    registration_token_plain: 'alice-plain-token',
  },
  {
    contributor_id: 'contrib-2',
    github_username: 'bob',
    trust_tier: 'trusted',
    registration_token: crypto.createHash('sha256').update('bob-plain-token').digest('hex'),
    registration_token_plain: 'bob-plain-token',
  },
];

function createTestServer() {
  const app = express();
  app.use(express.json());

  app.get('/api/contributors', (_req, res) => {
    res.json({ contributors: PROFILES.map(p => ({ ...publicContributorView(p), active: false })) });
  });

  app.get('/api/contributors/:id', (req, res) => {
    const profile = PROFILES.find(p => p.contributor_id === req.params.id || p.github_username === req.params.id);
    if (!profile) return res.status(404).json({ error: 'Contributor not found' });
    res.json({ ...publicContributorView(profile), active: false, currentTask: null, lastTmuxOutput: [] });
  });

  app.put('/api/contributors/:id/trust', (req, res) => {
    if (!requireLegacyOwnerRole(req, res)) return;
    res.json({ ok: true, trust_tier: req.body.tier });
  });

  app.post('/api/contributors/:id/revoke', (req, res) => {
    if (!requireLegacyOwnerRole(req, res)) return;
    res.json({ ok: true });
  });

  return app.listen(TEST_PORT);
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

describe('legacy contributor management authorization and token hygiene', () => {
  let server;

  before(async () => {
    server = createTestServer();
    await new Promise((resolve) => { server.on('listening', resolve); });
  });

  after(() => {
    if (server) server.close();
  });

  it('never serves registration_token_plain from the contributor list', async () => {
    const res = await httpRequest('GET', '/api/contributors');
    assert.equal(res.status, 200);
    assert.equal(res.body.contributors.length, 2);
    for (const c of res.body.contributors) {
      assert.equal('registration_token_plain' in c, false, `${c.github_username} leaked plaintext token`);
    }
    assert.doesNotMatch(JSON.stringify(res.body), /-plain-token/);
  });

  it('never serves registration_token_plain from a single contributor record', async () => {
    const res = await httpRequest('GET', '/api/contributors/alice');
    assert.equal(res.status, 200);
    assert.equal(res.body.github_username, 'alice');
    assert.equal('registration_token_plain' in res.body, false);
    assert.doesNotMatch(JSON.stringify(res.body), /alice-plain-token/);
  });

  const mutationCases = [
    ['PUT', '/api/contributors/contrib-1/trust', { tier: 'advisor' }],
    ['POST', '/api/contributors/contrib-1/revoke', {}],
  ];

  for (const [method, route, body] of mutationCases) {
    it(`rejects anonymous ${method} ${route}`, async () => {
      const res = await httpRequest(method, route, body);
      assert.equal(res.status, 403);
      assert.match(res.body.error, /owner access required/);
    });

    it(`rejects a self-asserted owner role without proxy proof for ${method} ${route}`, async () => {
      const res = await httpRequest(method, route, body, { 'X-Hive-Role': 'owner' });
      assert.equal(res.status, 403);
    });

    it(`accepts owner bearer for ${method} ${route}`, async () => {
      const res = await httpRequest(method, route, body, { Authorization: 'Bearer ' + OWNER_TOKEN });
      assert.equal(res.status, 200);
      assert.equal(res.body.ok, true);
    });
  }

  it('keeps the real legacy server mutation routes behind requireLegacyOwnerRole', () => {
    const serverSource = fs.readFileSync(path.join(__dirname, '..', 'server.js'), 'utf8');
    for (const route of ['/api/contributors/:id/trust', '/api/contributors/:id/revoke']) {
      const routeIndex = serverSource.indexOf(route);
      assert.notEqual(routeIndex, -1, `${route} route missing from server.js`);
      const routeBody = serverSource.slice(routeIndex, routeIndex + 260);
      assert.match(routeBody, /requireLegacyOwnerRole\(req, res\)/, `${route} must require owner role`);
    }
  });

  it('keeps the real legacy server GET routes on the sanitized contributor view', () => {
    const serverSource = fs.readFileSync(path.join(__dirname, '..', 'server.js'), 'utf8');
    assert.match(serverSource, /function publicContributorView\(/, 'publicContributorView sanitizer missing from server.js');
    for (const route of ["app.get('/api/contributors'", "app.get('/api/contributors/:id'"]) {
      const routeIndex = serverSource.indexOf(route);
      assert.notEqual(routeIndex, -1, `${route} route missing from server.js`);
      const routeBody = serverSource.slice(routeIndex, routeIndex + 600);
      assert.match(routeBody, /publicContributorView\(/, `${route} must serve the sanitized view`);
      assert.doesNotMatch(routeBody, /\.\.\.profile\b|\.\.\.p\b/, `${route} must not spread the raw stored profile`);
    }
  });
});
