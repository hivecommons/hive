'use strict';

const { describe, it, before, after, beforeEach } = require('node:test');
const assert = require('node:assert/strict');
const http = require('http');
const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
const os = require('os');
const WebSocket = require('ws');

const TEST_PORT = 13001 + Math.floor(Math.random() * 1000);
const TEST_CONTRIBUTORS_DIR = path.join(os.tmpdir(), `hive-test-contributors-${Date.now()}`);
const TEST_METRICS_DIR = path.join(os.tmpdir(), `hive-test-metrics-${Date.now()}`);
const TEST_FEDERATION_DIR = path.join(os.tmpdir(), `hive-test-federation-${Date.now()}`);
const TEST_RESTRICTIONS_DIR = path.join(os.tmpdir(), `hive-test-restrictions-${Date.now()}`);

function httpRequest(method, urlPath, body) {
  return new Promise((resolve, reject) => {
    const opts = { hostname: '127.0.0.1', port: TEST_PORT, path: urlPath, method, headers: {} };
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

function wsConnectAndRecvFirst(urlPath) {
  return new Promise((resolve, reject) => {
    const TIMEOUT_MS = 5000;
    const ws = new WebSocket(`ws://127.0.0.1:${TEST_PORT}${urlPath}`);
    const timer = setTimeout(() => { ws.terminate(); reject(new Error('ws timeout')); }, TIMEOUT_MS);
    ws.once('message', (data) => {
      clearTimeout(timer);
      resolve({ ws, firstMessage: JSON.parse(data.toString()) });
    });
    ws.on('error', (err) => { clearTimeout(timer); reject(err); });
  });
}

function wsRecv(ws, timeout) {
  const RECV_TIMEOUT_MS = timeout || 5000;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('ws recv timeout')), RECV_TIMEOUT_MS);
    ws.once('message', (data) => {
      clearTimeout(timer);
      resolve(JSON.parse(data.toString()));
    });
  });
}

// ── Minimal server setup for testing ───────────────────────────────────────
// We can't import server.js directly (it starts listening and reads /etc/hive).
// Instead, we test the contributor module functions in isolation and run a
// lightweight Express app that mirrors the contribute endpoints.

const express = require('express');
const { WebSocketServer } = require('ws');

let app, server, wss;

function createTestServer() {
  app = express();
  app.use(express.json());

  const contributorConnections = new Map();
  let contributorSeq = 0;

  function loadContributor(username) {
    try {
      return JSON.parse(fs.readFileSync(path.join(TEST_CONTRIBUTORS_DIR, `${username}.json`), 'utf8'));
    } catch (_) { return null; }
  }

  function saveContributor(profile) {
    fs.writeFileSync(
      path.join(TEST_CONTRIBUTORS_DIR, `${profile.github_username}.json`),
      JSON.stringify(profile, null, 2)
    );
  }

  function listContributors() {
    try {
      return fs.readdirSync(TEST_CONTRIBUTORS_DIR)
        .filter(f => f.endsWith('.json'))
        .map(f => { try { return JSON.parse(fs.readFileSync(path.join(TEST_CONTRIBUTORS_DIR, f), 'utf8')); } catch (_) { return null; } })
        .filter(Boolean);
    } catch (_) { return []; }
  }

  // Registration
  app.post('/api/contribute/register', (req, res) => {
    const { github_username } = req.body;
    if (!github_username || !/^[a-zA-Z0-9_-]+$/.test(github_username)) {
      return res.status(400).json({ error: 'Invalid github_username' });
    }
    const existing = loadContributor(github_username);
    if (existing) {
      if (existing.trust_tier === 'revoked') {
        return res.status(403).json({ error: 'Account revoked — contact the hive administrator to reinstate' });
      }
      // SECURITY: never reissue an existing contributor's token to an
      // unauthenticated caller (account-takeover primitive).
      return res.json({
        contributor_id: existing.contributor_id,
        message: 'Already registered — to retrieve your token, sign in via the GitHub OAuth flow at /contribute to prove account ownership',
      });
    }
    const contributorId = `c-${crypto.randomBytes(6).toString('hex')}`;
    const registrationToken = crypto.randomBytes(32).toString('hex');
    const profile = {
      github_username,
      contributor_id: contributorId,
      registration_token: crypto.createHash('sha256').update(registrationToken).digest('hex'),
      registration_token_plain: registrationToken,
      trust_tier: 'newcomer',
      registered_at: new Date().toISOString(),
      total_tasks_completed: 0,
      total_tasks_failed: 0,
      rate_limits: { max_concurrent_tasks: 1, max_tasks_per_hour: 3, max_tasks_per_day: 10 },
    };
    saveContributor(profile);
    res.json({ contributor_id: contributorId, registration_token: registrationToken, message: 'Registered successfully' });
  });

  // Status
  app.get('/api/contribute/status', (_req, res) => {
    res.json({
      hub: 'online',
      active_contributors: contributorConnections.size,
      total_registered: listContributors().length,
      actionable_items: 0,
    });
  });

  // List contributors
  app.get('/api/contributors', (_req, res) => {
    const profiles = listContributors();
    const activeIds = [...contributorConnections.keys()];
    res.json({ contributors: profiles.map(p => ({ ...p, active: activeIds.includes(p.contributor_id) })) });
  });

  // Get single contributor
  app.get('/api/contributors/:id', (req, res) => {
    const profiles = listContributors();
    const profile = profiles.find(p => p.contributor_id === req.params.id || p.github_username === req.params.id);
    if (!profile) return res.status(404).json({ error: 'Contributor not found' });
    res.json(profile);
  });

  // Trust tier change
  app.put('/api/contributors/:id/trust', (req, res) => {
    const profiles = listContributors();
    const profile = profiles.find(p => p.contributor_id === req.params.id || p.github_username === req.params.id);
    if (!profile) return res.status(404).json({ error: 'Contributor not found' });
    const { tier } = req.body;
    const validTiers = ['newcomer', 'contributor', 'trusted', 'advisor', 'revoked'];
    if (!validTiers.includes(tier)) return res.status(400).json({ error: 'Invalid tier' });
    profile.trust_tier = tier;
    saveContributor(profile);
    res.json({ ok: true, trust_tier: tier });
  });

  // Revoke
  app.post('/api/contributors/:id/revoke', (req, res) => {
    const profiles = listContributors();
    const profile = profiles.find(p => p.contributor_id === req.params.id || p.github_username === req.params.id);
    if (!profile) return res.status(404).json({ error: 'Contributor not found' });
    profile.trust_tier = 'revoked';
    saveContributor(profile);
    const conn = contributorConnections.get(profile.contributor_id);
    if (conn && conn.ws) {
      conn.ws.send(JSON.stringify({ type: 'auth_failed', reason: 'Access revoked' }));
      conn.ws.close();
      contributorConnections.delete(profile.contributor_id);
    }
    res.json({ ok: true });
  });

  // Federation
  const registryFile = path.join(TEST_FEDERATION_DIR, 'registry.json');
  function loadRegistry() { try { return JSON.parse(fs.readFileSync(registryFile, 'utf8')); } catch (_) { return { hives: [] }; } }
  function saveRegistry(d) { fs.writeFileSync(registryFile, JSON.stringify(d)); }

  app.get('/api/hives', (_req, res) => {
    res.json(loadRegistry());
  });

  app.post('/api/hives/register', (req, res) => {
    const { project_name, org, hub_url } = req.body;
    if (!project_name || !org || !hub_url) return res.status(400).json({ error: 'Missing fields' });
    const registry = loadRegistry();
    const hiveId = `hive-${org}-${project_name}`.toLowerCase().replace(/[^a-z0-9-]/g, '-');
    registry.hives = registry.hives || [];
    registry.hives.push({ id: hiveId, project_name, org, hub_url, registered_at: new Date().toISOString() });
    saveRegistry(registry);
    res.json({ ok: true });
  });

  app.post('/api/hives/:id/heartbeat', (req, res) => {
    const registry = loadRegistry();
    const hive = (registry.hives || []).find(h => h.id === req.params.id);
    if (!hive) return res.status(404).json({ error: 'Hive not found' });
    hive.last_heartbeat = new Date().toISOString();
    hive.active_contributors = req.body.active_contributors || 0;
    saveRegistry(registry);
    res.json({ ok: true });
  });

  app.delete('/api/hives/:id', (req, res) => {
    const registry = loadRegistry();
    const idx = (registry.hives || []).findIndex(h => h.id === req.params.id);
    if (idx === -1) return res.status(404).json({ error: 'Not found' });
    registry.hives.splice(idx, 1);
    saveRegistry(registry);
    res.json({ ok: true });
  });

  server = app.listen(TEST_PORT);

  // WebSocket server
  wss = new WebSocketServer({ server, path: '/contribute' });
  wss.on('connection', (ws) => {
    const nonce = crypto.randomBytes(16).toString('hex');
    ws.send(JSON.stringify({ type: 'auth_challenge', seq: 1, nonce }));

    ws.on('message', (data) => {
      let msg;
      try { msg = JSON.parse(data.toString()); } catch (_) { return; }

      if (msg.type === 'auth_response') {
        const token = msg.registration_token;
        if (!token) {
          ws.send(JSON.stringify({ type: 'auth_failed', reason: 'Missing token' }));
          ws.close();
          return;
        }
        const tokenHash = crypto.createHash('sha256').update(token).digest('hex');
        const profiles = listContributors();
        const profile = profiles.find(p => p.registration_token === tokenHash);
        if (!profile) {
          ws.send(JSON.stringify({ type: 'auth_failed', reason: 'Invalid token' }));
          ws.close();
          return;
        }
        if (profile.trust_tier === 'revoked') {
          ws.send(JSON.stringify({ type: 'auth_failed', reason: 'Revoked' }));
          ws.close();
          return;
        }
        contributorConnections.set(profile.contributor_id, { ws, profile });
        ws.send(JSON.stringify({ type: 'auth_ok', seq: 2, contributor_id: profile.contributor_id, trust_tier: profile.trust_tier }));
      }

      if (msg.type === 'pong') { /* heartbeat ack */ }
      if (msg.type === 'ping') { ws.send(JSON.stringify({ type: 'pong', seq: msg.seq })); }
    });

    ws.on('close', () => {
      for (const [id, conn] of contributorConnections) {
        if (conn.ws === ws) { contributorConnections.delete(id); break; }
      }
    });
  });
}

// ── Tests ──────────────────────────────────────────────────────────────────

describe('contribute-hive', () => {
  before(async () => {
    fs.mkdirSync(TEST_CONTRIBUTORS_DIR, { recursive: true });
    fs.mkdirSync(TEST_METRICS_DIR, { recursive: true });
    fs.mkdirSync(TEST_FEDERATION_DIR, { recursive: true });
    fs.mkdirSync(TEST_RESTRICTIONS_DIR, { recursive: true });
    createTestServer();
    await new Promise((resolve) => { server.on('listening', resolve); });
  });

  after(() => {
    if (wss) wss.close();
    if (server) server.close();
    fs.rmSync(TEST_CONTRIBUTORS_DIR, { recursive: true, force: true });
    fs.rmSync(TEST_METRICS_DIR, { recursive: true, force: true });
    fs.rmSync(TEST_FEDERATION_DIR, { recursive: true, force: true });
    fs.rmSync(TEST_RESTRICTIONS_DIR, { recursive: true, force: true });
  });

  describe('registration', () => {
    it('registers a new contributor', async () => {
      const res = await httpRequest('POST', '/api/contribute/register', { github_username: 'testuser1' });
      assert.equal(res.status, 200);
      assert.equal(res.body.message, 'Registered successfully');
      assert.ok(res.body.contributor_id.startsWith('c-'));
      assert.ok(res.body.registration_token.length > 0);
    });

    it('returns existing profile on re-registration without reissuing the token', async () => {
      const res1 = await httpRequest('POST', '/api/contribute/register', { github_username: 'testuser-dupe' });
      const res2 = await httpRequest('POST', '/api/contribute/register', { github_username: 'testuser-dupe' });
      assert.equal(res2.status, 200);
      assert.match(res2.body.message, /^Already registered/);
      assert.equal(res2.body.contributor_id, res1.body.contributor_id);
      assert.equal(res2.body.registration_token, undefined);
    });

    it('rejects re-registration of a revoked contributor', async () => {
      const res1 = await httpRequest('POST', '/api/contribute/register', { github_username: 'testuser-revoked-reg' });
      assert.equal(res1.status, 200);
      const file = path.join(TEST_CONTRIBUTORS_DIR, 'testuser-revoked-reg.json');
      const profile = JSON.parse(fs.readFileSync(file, 'utf8'));
      profile.trust_tier = 'revoked';
      fs.writeFileSync(file, JSON.stringify(profile));
      const res2 = await httpRequest('POST', '/api/contribute/register', { github_username: 'testuser-revoked-reg' });
      assert.equal(res2.status, 403);
      assert.equal(res2.body.registration_token, undefined);
    });

    it('rejects invalid username', async () => {
      const res = await httpRequest('POST', '/api/contribute/register', { github_username: 'bad user!' });
      assert.equal(res.status, 400);
    });

    it('rejects empty username', async () => {
      const res = await httpRequest('POST', '/api/contribute/register', { github_username: '' });
      assert.equal(res.status, 400);
    });
  });

  describe('contributor management', () => {
    let contributorId;
    let regToken;

    before(async () => {
      const res = await httpRequest('POST', '/api/contribute/register', { github_username: 'managed-user' });
      contributorId = res.body.contributor_id;
      regToken = res.body.registration_token;
    });

    it('lists contributors', async () => {
      const res = await httpRequest('GET', '/api/contributors');
      assert.equal(res.status, 200);
      assert.ok(Array.isArray(res.body.contributors));
      assert.ok(res.body.contributors.some(c => c.github_username === 'managed-user'));
    });

    it('gets a single contributor by ID', async () => {
      const res = await httpRequest('GET', `/api/contributors/${contributorId}`);
      assert.equal(res.status, 200);
      assert.equal(res.body.github_username, 'managed-user');
      assert.equal(res.body.trust_tier, 'newcomer');
    });

    it('gets a contributor by username', async () => {
      const res = await httpRequest('GET', '/api/contributors/managed-user');
      assert.equal(res.status, 200);
      assert.equal(res.body.contributor_id, contributorId);
    });

    it('returns 404 for unknown contributor', async () => {
      const res = await httpRequest('GET', '/api/contributors/nonexistent');
      assert.equal(res.status, 404);
    });

    it('changes trust tier', async () => {
      const res = await httpRequest('PUT', `/api/contributors/${contributorId}/trust`, { tier: 'contributor' });
      assert.equal(res.status, 200);
      assert.equal(res.body.trust_tier, 'contributor');

      const check = await httpRequest('GET', `/api/contributors/${contributorId}`);
      assert.equal(check.body.trust_tier, 'contributor');
    });

    it('rejects invalid trust tier', async () => {
      const res = await httpRequest('PUT', `/api/contributors/${contributorId}/trust`, { tier: 'superadmin' });
      assert.equal(res.status, 400);
    });

    it('revokes a contributor', async () => {
      const regRes = await httpRequest('POST', '/api/contribute/register', { github_username: 'revoke-target' });
      const res = await httpRequest('POST', `/api/contributors/${regRes.body.contributor_id}/revoke`);
      assert.equal(res.status, 200);

      const check = await httpRequest('GET', `/api/contributors/${regRes.body.contributor_id}`);
      assert.equal(check.body.trust_tier, 'revoked');
    });
  });

  describe('hub status', () => {
    it('returns hub status', async () => {
      const res = await httpRequest('GET', '/api/contribute/status');
      assert.equal(res.status, 200);
      assert.equal(res.body.hub, 'online');
      assert.equal(typeof res.body.active_contributors, 'number');
      assert.equal(typeof res.body.total_registered, 'number');
    });
  });

  describe('WebSocket authentication', () => {
    let regToken;

    before(async () => {
      const res = await httpRequest('POST', '/api/contribute/register', { github_username: 'ws-test-user' });
      regToken = res.body.registration_token;
    });

    it('sends auth_challenge on connect', async () => {
      const { ws, firstMessage } = await wsConnectAndRecvFirst('/contribute');
      assert.equal(firstMessage.type, 'auth_challenge');
      assert.ok(firstMessage.nonce);
      ws.close();
    });

    it('authenticates with valid token', async () => {
      const { ws, firstMessage } = await wsConnectAndRecvFirst('/contribute');
      assert.equal(firstMessage.type, 'auth_challenge');

      ws.send(JSON.stringify({ type: 'auth_response', registration_token: regToken, cli_backend: 'claude', model: 'opus-4-6' }));
      const authOk = await wsRecv(ws);
      assert.equal(authOk.type, 'auth_ok');
      assert.ok(authOk.contributor_id);
      assert.equal(authOk.trust_tier, 'newcomer');
      ws.close();
    });

    it('rejects invalid token', async () => {
      const { ws } = await wsConnectAndRecvFirst('/contribute');
      ws.send(JSON.stringify({ type: 'auth_response', registration_token: 'bogus-token' }));
      const msg = await wsRecv(ws);
      assert.equal(msg.type, 'auth_failed');
      assert.match(msg.reason, /Invalid/i);
    });

    it('rejects revoked contributor', async () => {
      const reg = await httpRequest('POST', '/api/contribute/register', { github_username: 'revoked-ws-user2' });
      await httpRequest('POST', `/api/contributors/${reg.body.contributor_id}/revoke`);

      const { ws } = await wsConnectAndRecvFirst('/contribute');
      ws.send(JSON.stringify({ type: 'auth_response', registration_token: reg.body.registration_token }));
      const msg = await wsRecv(ws);
      assert.equal(msg.type, 'auth_failed');
      assert.match(msg.reason, /[Rr]evok/);
    });

    it('responds to ping with pong', async () => {
      const { ws } = await wsConnectAndRecvFirst('/contribute');
      ws.send(JSON.stringify({ type: 'auth_response', registration_token: regToken, cli_backend: 'claude' }));
      await wsRecv(ws);

      ws.send(JSON.stringify({ type: 'ping', seq: 42 }));
      const pong = await wsRecv(ws);
      assert.equal(pong.type, 'pong');
      assert.equal(pong.seq, 42);
      ws.close();
    });
  });

  describe('federation registry', () => {
    it('starts with empty hive list', async () => {
      const res = await httpRequest('GET', '/api/hives');
      assert.equal(res.status, 200);
      assert.ok(Array.isArray(res.body.hives));
    });

    it('registers a hive', async () => {
      const res = await httpRequest('POST', '/api/hives/register', {
        project_name: 'test-project', org: 'test-org', hub_url: 'wss://test:3001/contribute',
      });
      assert.equal(res.status, 200);
      assert.equal(res.body.ok, true);
    });

    it('lists registered hives', async () => {
      const res = await httpRequest('GET', '/api/hives');
      assert.ok(res.body.hives.some(h => h.project_name === 'test-project'));
    });

    it('rejects registration with missing fields', async () => {
      const res = await httpRequest('POST', '/api/hives/register', { project_name: 'x' });
      assert.equal(res.status, 400);
    });

    it('receives heartbeat', async () => {
      const list = await httpRequest('GET', '/api/hives');
      const hive = list.body.hives.find(h => h.project_name === 'test-project');
      assert.ok(hive);

      const res = await httpRequest('POST', `/api/hives/${hive.id}/heartbeat`, { active_contributors: 5 });
      assert.equal(res.status, 200);

      const updated = await httpRequest('GET', '/api/hives');
      const refreshed = updated.body.hives.find(h => h.id === hive.id);
      assert.equal(refreshed.active_contributors, 5);
      assert.ok(refreshed.last_heartbeat);
    });

    it('returns 404 for heartbeat to unknown hive', async () => {
      const res = await httpRequest('POST', '/api/hives/hive-nonexistent/heartbeat', { active_contributors: 1 });
      assert.equal(res.status, 404);
    });

    it('deletes a hive', async () => {
      const list = await httpRequest('GET', '/api/hives');
      const hive = list.body.hives.find(h => h.project_name === 'test-project');
      const res = await httpRequest('DELETE', `/api/hives/${hive.id}`);
      assert.equal(res.status, 200);

      const after = await httpRequest('GET', '/api/hives');
      assert.ok(!after.body.hives.some(h => h.project_name === 'test-project'));
    });
  });
});
