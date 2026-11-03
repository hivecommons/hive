'use strict';

const assert = require('node:assert/strict');
const { test } = require('node:test');
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const { randomUUID } = require('node:crypto');
const { execFileSync } = require('node:child_process');
const mlx = require('./mlx-backend.js');
const pi = require('./pi-backend.js');
const extension = require('./mlx-pi-extension.js');

const MODEL = 'mlx-community/tool-coder-4bit';
const FINGERPRINT = '0.31.0-0.31.0-macOS-test-arm64';
const HOST = {
  platform: 'darwin', arch: 'arm64', macos: '14.0',
  pythonArch: 'arm64', pythonPlatform: 'darwin', pythonVersion: [3, 10],
  mlx: '0.31.0', mlxLM: '0.31.0',
};
const CALL = { index: 0, id: 'check-1', type: 'function', function: { name: 'hive_mlx_check', arguments: '{"value":"hive_mlx_tool_check"}' } };

function stream(delta, finish = 'stop', fingerprint = FINGERPRINT) {
  return `data: ${JSON.stringify({ system_fingerprint: fingerprint, choices: [{ delta, finish_reason: finish }] })}\n\ndata: [DONE]\n\n`;
}

function fixture(t) {
  const dir = path.join(__dirname, `.mlx-fixture-${randomUUID()}`);
  fs.mkdirSync(dir);
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return { HOME: dir, PI_CODING_AGENT_DIR: path.join(dir, '.pi', 'agent') };
}

function fakeServer(options = {}) {
  const calls = [];
  return {
    calls,
    fetch: async (url, init) => {
      calls.push({ url, init });
      assert.equal(init.redirect, 'error');
      if (init.method === 'GET') {
        return new Response(JSON.stringify({ data: (options.models || [MODEL]).map(id => ({ id })) }));
      }
      const request = JSON.parse(init.body);
      assert.equal(request.model, options.model || MODEL);
      assert.equal(request.stream, true);
      assert.equal(request.max_tokens, 256);
      assert.equal(request.tools[0].function.name, 'hive_mlx_check');
      if (request.messages.length === 1) {
        return new Response(options.prose
          ? stream({ content: 'I would call the tool' })
          : stream({ tool_calls: [CALL] }, 'tool_calls', options.fingerprint || FINGERPRINT));
      }
      assert.equal(request.messages[1].tool_calls[0].id, 'check-1');
      assert.equal(request.messages[2].role, 'tool');
      assert.equal(request.messages[2].tool_call_id, 'check-1');
      assert.equal(request.messages[2].content, 'hive_mlx_tool_check');
      return new Response(stream({ content: options.badRoundTrip ? 'no' : 'hive_mlx_tool_check' }));
    },
  };
}

test('Apple Silicon/native Python/macOS compatibility is explicit and optional', () => {
  assert.equal(mlx.validatePlatform(HOST), HOST);
  for (const [override, error] of [
    [{ platform: 'linux' }, /Apple Silicon/],
    [{ arch: 'x64' }, /Rosetta/],
    [{ macos: '13.7' }, /macOS 14/],
    [{ macos: '' }, /macOS 14/],
    [{ pythonArch: 'x86_64' }, /natively/],
    [{ pythonPlatform: 'linux' }, /natively/],
    [{ pythonVersion: [3, 9] }, /Python >=3.10/],
    [{ mlxLM: null }, /virtualenv/],
    [{ mlx: null }, /virtualenv/],
  ]) assert.throws(() => mlx.validatePlatform({ ...HOST, ...override }), error);
  // Other Pi providers never inspect a Python environment or an endpoint.
  assert.doesNotThrow(() => mlx.checkRuntime('anthropic/claude-sonnet-4-6', {}));
  assert.equal(pi.parsePiModelSelection('openai/gpt-4o').canonical, 'openai/gpt-4o');
  assert.equal(pi.parsePiModelSelection('openai//absolute').valid, false);
  assert.equal(pi.parsePiModelSelection('mlx//Users/contributor/models/coder').valid, true);
  assert.equal(pi.parsePiModelSelection('mlx//Users/a b/coder').valid, false);
});

test('endpoint is strictly loopback; credentials, DNS, paths and hosted fallbacks are refused', () => {
  assert.equal(mlx.endpointURL('http://127.0.0.1:8080/v1/'), mlx.DEFAULT_ENDPOINT);
  assert.equal(mlx.endpointURL('http://[::1]:8080/v1'), 'http://[::1]:8080/v1');
  for (const endpoint of [
    'https://api.openai.com/v1', 'http://0.0.0.0:8080/v1',
    'http://localhost:8080/v1', 'http://host.docker.internal:8080/v1',
    'http://secret@127.0.0.1:8080/v1', 'http://127.0.0.1/v1?key=x',
    'http://127.0.0.1/v1#x', 'http://127.0.0.1/other', 'invalid',
  ]) assert.throws(() => mlx.endpointURL(endpoint), /endpoint/);
});

test('explicit setup preserves other providers and persists verified Pi configuration', async t => {
  const env = fixture(t);
  fs.mkdirSync(env.PI_CODING_AGENT_DIR, { recursive: true });
  const original = { providers: { anthropic: { apiKey: 'do-not-copy-me' } }, other: 'preserved' };
  fs.writeFileSync(path.join(env.PI_CODING_AGENT_DIR, 'models.json'), JSON.stringify(original));
  const server = fakeServer();
  assert.equal(await mlx.configure(MODEL, env, server.fetch, () => HOST), `mlx/${MODEL}`);
  assert.equal(server.calls.length, 3);
  const config = JSON.parse(fs.readFileSync(path.join(env.PI_CODING_AGENT_DIR, 'models.json')));
  assert.deepEqual(config.providers.anthropic, original.providers.anthropic);
  assert.equal(config.other, 'preserved');
  assert.equal(config.providers.mlx.api, 'hive-mlx');
  assert.equal(config.providers.mlx.compat.maxTokensField, 'max_tokens');
  assert.equal(config.providers.mlx.models[0].id, MODEL);
  assert.equal(config.providers.mlx.models[0].contextWindow, 8192);
  assert.equal(fs.statSync(path.join(env.PI_CODING_AGENT_DIR, 'models.json')).mode & 0o777, 0o600);
  assert.equal(pi.piCredentialConfiguration(pi.parsePiModelSelection(`mlx/${MODEL}`), env), 'configured_unverified');
  await mlx.check(`mlx/${MODEL}`, env, server.fetch, null);
  assert.equal(server.calls.length, 4, 'startup makes only a model-list request, never inference');
  pi.narrowPiStage(path.join(env.HOME, '.pi'), pi.parsePiModelSelection(`mlx/${MODEL}`));
  assert.deepEqual(Object.keys(JSON.parse(fs.readFileSync(path.join(env.PI_CODING_AGENT_DIR, 'models.json'))).providers), ['mlx']);
  await mlx.check(`mlx/${MODEL}`, env, server.fetch, null);
});

test('setup rejects missing models, prose-only models, broken tools and wrong runtime without writing config', async t => {
  for (const [options, error] of [
    [{ models: ['other'] }, /not listed/],
    [{ prose: true }, /streaming tool-call/],
    [{ badRoundTrip: true }, /tool-result/],
    [{ fingerprint: 'not-mlx' }, /fingerprint/],
  ]) {
    const env = fixture(t);
    await assert.rejects(mlx.configure(MODEL, env, fakeServer(options).fetch, () => HOST), error);
    assert.equal(fs.existsSync(path.join(env.PI_CODING_AGENT_DIR, 'models.json')), false);
  }
});

test('startup fails closed for unconfigured, tampered, missing and unreachable models', async t => {
  const env = fixture(t);
  await assert.rejects(mlx.check(`mlx/${MODEL}`, env, fakeServer().fetch, null), /not configured/);
  await mlx.configure(MODEL, env, fakeServer().fetch, () => HOST);
  await assert.rejects(mlx.check(`mlx/${MODEL}`, env, fakeServer({ models: [] }).fetch, null), /missing/);
  await assert.rejects(mlx.check(`mlx/${MODEL}`, env, async () => { throw new Error('connection refused'); }, null), /local endpoint request failed/);
  const configFile = path.join(env.PI_CODING_AGENT_DIR, 'models.json');
  const config = JSON.parse(fs.readFileSync(configFile));
  config.providers.mlx.baseUrl = 'https://api.openai.com/v1';
  fs.writeFileSync(configFile, JSON.stringify(config));
  await assert.rejects(mlx.check(`mlx/${MODEL}`, env, fakeServer().fetch, null), /configuration changed/);
});

test('stream parser assembles fragmented tool arguments and rejects truncated/error streams', () => {
  const fragments = [
    { index: 0, id: 'check-1', function: { name: 'hive_mlx_check', arguments: '{"value":' } },
    { index: 0, function: { arguments: '"hive_mlx_tool_check"}' } },
  ].map(call => stream({ tool_calls: [call] }, 'tool_calls').replace('data: [DONE]\n\n', '')).join('');
  const parsed = mlx.parseStream(`: keepalive\n\n${fragments}data: [DONE]\n\n`);
  assert.equal(parsed.calls[0].function.arguments, CALL.function.arguments);
  assert.throws(() => mlx.parseStream(fragments), /without \[DONE\]/);
  assert.throws(() => mlx.parseStream('data: {"error":"bad"}\n\n'), /streaming error/);
});

test('HTTP seam handles actual local streaming responses and refuses redirects', async t => {
  const server = http.createServer((req, res) => {
    if (req.url === '/v1/models') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ data: [{ id: MODEL }] }));
    } else {
      res.writeHead(302, { Location: 'https://api.openai.com/v1/models' });
      res.end();
    }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => server.close(resolve)));
  const endpoint = `http://127.0.0.1:${server.address().port}/v1`;
  assert.deepEqual(await mlx.availableModels(endpoint), [MODEL]);
  await assert.rejects(mlx.verifyTools(endpoint, MODEL), /local endpoint request failed/);
});

test('model paths and token budgets are contributor owned and no default model is selected', async t => {
  const env = fixture(t);
  for (const model of ['', 'default_model', 'model;touch-elsewhere']) {
    await assert.rejects(mlx.configure(model, env, fakeServer().fetch, () => HOST));
  }
  await assert.rejects(mlx.configure(MODEL, { ...env, HIVE_MLX_CONTEXT_WINDOW: 'bad' }, fakeServer().fetch, () => HOST), /positive integers/);
  await assert.rejects(mlx.configure(MODEL, { ...env, HIVE_MLX_MAX_TOKENS: '8192' }, fakeServer().fetch, () => HOST), /smaller/);
  const model = '/Users/contributor/models/coder';
  const server = fakeServer({ model, models: [model] });
  assert.equal(await mlx.configure(model, env, server.fetch, () => HOST), `mlx/${model}`);
});

test('runtime preflight is a bounded subprocess and reports failures without fallback', () => {
  const env = { HOME: '/contributor' };
  const calls = [];
  mlx.checkRuntime(`mlx/${MODEL}`, env, (...args) => { calls.push(args); });
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], process.execPath);
  assert.deepEqual(calls[0][1].slice(1), ['check', `mlx/${MODEL}`, process.platform === 'darwin' ? '--host' : '--endpoint-only']);
  assert.equal(calls[0][2].env, env);
  assert.equal(calls[0][2].timeout, 20000);
  const error = Object.assign(new Error('failed'), { stderr: Buffer.from('MLX local endpoint unreachable') });
  assert.throws(() => mlx.checkRuntime(`mlx/${MODEL}`, env, () => { throw error; }), /endpoint unreachable/);
});

test('real Pi provider delegate forces loopback-only fetch and preserves tools and cancellation', async t => {
  const env = fixture(t);
  env.AGENT_MODEL = `mlx/${MODEL}`;
  await mlx.configure(MODEL, env, fakeServer().fetch, () => HOST);
  const delegateCalls = [];
  const fetchCalls = [];
  let registered;
  const streamResult = { marker: 'Pi stream and tool executor' };
  extension.registerMlx({ registerProvider(name, config) {
    assert.equal(name, 'mlx');
    registered = config;
  } }, { streamSimple(...args) { delegateCalls.push(args); return streamResult; } }, env,
  async (...args) => { fetchCalls.push(args); return new Response('ok'); });
  assert.equal(registered.api, 'hive-mlx');
  const model = { provider: 'mlx', id: MODEL, baseUrl: mlx.DEFAULT_ENDPOINT, api: 'hive-mlx' };
  const context = { messages: [{ role: 'toolResult', content: [{ type: 'text', text: 'edited file' }] }] };
  const controller = new AbortController();
  const options = { signal: controller.signal, onPayload() {}, fetch() { throw new Error('must not override guard'); } };
  assert.equal(registered.streamSimple(model, context, options), streamResult);
  const [effectiveModel, actualContext, actualOptions] = delegateCalls[0];
  assert.equal(effectiveModel.api, 'openai-completions');
  assert.equal(actualContext, context, 'Pi retains the full tool transcript');
  assert.equal(actualOptions.signal, controller.signal);
  assert.equal(actualOptions.onPayload, options.onPayload);
  await actualOptions.fetch(`${mlx.DEFAULT_ENDPOINT}/chat/completions`, { redirect: 'follow', method: 'POST' });
  assert.equal(fetchCalls[0][1].redirect, 'error');
  assert.throws(() => actualOptions.fetch('https://api.openai.com/v1/chat/completions'), /loopback/);
  assert.throws(() => registered.streamSimple({ ...model, id: 'another' }, context, options), /model changed/);
});

test('Pi inference transport rejects redirects after successful setup', async t => {
  const server = http.createServer((req, res) => {
    res.writeHead(307, { Location: 'https://api.openai.com/v1/chat/completions' });
    res.end();
  });

  test('explicit serve recipe uses native Python, loopback and offline weights without changing RAM settings', t => {
    const env = fixture(t);
    const model = path.join(env.HOME, 'model');
    fs.mkdirSync(model);
    const calls = [];
    mlx.serve(model, { ...env, HIVE_MLX_PYTHON: '/chosen/python', HIVE_MLX_ENDPOINT: 'http://127.0.0.1:8081/v1' },
      () => HOST, (...args) => calls.push(args));
    assert.equal(calls[0][0], '/chosen/python');
    assert.deepEqual(calls[0][1], ['-m', 'mlx_lm.server', '--model', model, '--host', '127.0.0.1', '--port', '8081']);
    assert.equal(calls[0][2].env.HF_HUB_OFFLINE, '1');
    assert.equal(calls[0][2].stdio, 'inherit');
    assert.throws(() => mlx.serve(path.join(model, 'missing'), env, () => HOST, () => assert.fail('must not launch')), /directory is missing/);
    assert.throws(() => mlx.serve(model, { ...env, HIVE_MLX_ENDPOINT: 'http://0.0.0.0:8080/v1' }, () => HOST), /loopback/);
    assert.throws(() => mlx.serve(model, env, () => { throw new Error('unsupported Rosetta Python'); }, () => assert.fail('must not launch')), /Rosetta/);
  });

  test('MLX just launcher remains a Pi wrapper with explicit model/runtime refusal', () => {
    const recipes = fs.readFileSync(path.join(__dirname, '..', 'Justfile'), 'utf8');
    const launcher = recipes.split('contribute-mlx mode="docker":')[1].split('\n\n')[0];
    assert.match(launcher, /\$\{AGENT_MODEL:-\}" != mlx\/\*/);
    assert.match(launcher, /contribute-hive pi "\$1"/);
    assert.match(launcher, /export HIVE_CONTAINER_RUNTIME=docker/);
    assert.match(launcher, /Kubernetes cannot reach Mac loopback/);
    assert.match(recipes, /contribute-mlx-serve model:\n    node bin\/mlx-backend\.js serve "\$1"/);
  });

  test('MLX launcher preserves explicit model across both saved-profile loads without changing generic Pi defaults', t => {
    const env = fixture(t);
    const config = path.join(env.HOME, 'contributor.env');
    fs.writeFileSync(config, 'AGENT_MODEL=anthropic/saved-hosted-model\nHIVE_CONTAINER_RUNTIME=podman\n');
    const recipes = fs.readFileSync(path.join(__dirname, '..', 'Justfile'), 'utf8');
    const launcher = recipes.split('contribute-mlx mode="docker":')[1].split('\n\n')[0]
      .replaceAll('{{config_dir}}', env.HOME)
      .replace('{{justfile_directory()}}', path.join(__dirname, '..'));
    const guard = recipes.split('# MLX-only launch override:')[1].split('# End MLX-only launch override.')[0];
    const nested = guard.slice(guard.indexOf('\n'));
    const script = `just() {
      local _MLX_LAUNCH_MODEL="\${HIVE_MLX_LAUNCH_MODEL:-}"
      source "${config}"
      BACKEND=pi
      ${nested}
      printf '%s' "$AGENT_MODEL"
    }\n${launcher}`;
    const result = execFileSync('bash', ['-s', '--', 'docker'], {
      input: script, encoding: 'utf8',
      env: { ...process.env, ...env, AGENT_MODEL: `mlx/${MODEL}`, HIVE_CONTAINER_RUNTIME: 'docker' },
    });
    assert.equal(result, `mlx/${MODEL}`);
    const ordinary = execFileSync('bash', ['-s'], {
      input: `AGENT_MODEL=anthropic/saved-hosted-model; BACKEND=pi; _MLX_LAUNCH_MODEL='';\n${nested}\nprintf '%s' "$AGENT_MODEL"`,
      encoding: 'utf8',
    });
    assert.equal(ordinary, 'anthropic/saved-hosted-model');
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => server.close(resolve)));
  const endpoint = `http://127.0.0.1:${server.address().port}/v1`;
  const guarded = extension.localFetch(endpoint);
  await assert.rejects(guarded(`${endpoint}/chat/completions`, { method: 'POST', body: 'private task' }));
});
