'use strict';

// MLX-LM supplies inference; Pi supplies the coding tools and task lifecycle.
// Only the explicit serve command starts a server; nothing installs packages
// or downloads weights.
const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');
const { createHash } = require('crypto');
const { parsePiModelSelection } = require('./pi-backend.js');

const DEFAULT_ENDPOINT = 'http://127.0.0.1:8080/v1';
const HTTP_TIMEOUT_MS = 10000;
const PROBE_TIMEOUT_MS = 120000;
const MAX_RESPONSE_BYTES = 1024 * 1024;
const PROBE_MAX_TOKENS = 256;
const DEFAULT_CONTEXT = 8192;
const DEFAULT_MAX_TOKENS = 2048;
const DEFAULT_HTTP_PORT = '80';
const PROBE_VALUE = 'hive_mlx_tool_check';
const COMPAT = Object.freeze({
  supportsStore: false,
  supportsDeveloperRole: false,
  supportsReasoningEffort: false,
  maxTokensField: 'max_tokens',
});

function endpointURL(raw) {
  let url;
  try { url = new URL(raw); } catch (_) { throw new Error('MLX endpoint must be http://127.0.0.1:<port>/v1'); }
  // Literal loopback only: no DNS resolution, credentials, redirects or public
  // listeners. Docker Desktop host networking keeps this exact host boundary.
  if (url.protocol !== 'http:' || !['127.0.0.1', '[::1]'].includes(url.hostname) ||
      url.username || url.password || url.search || url.hash || !/^\/v1\/?$/.test(url.pathname)) {
    throw new Error('MLX requires a literal loopback HTTP endpoint ending in /v1; never expose MLX-LM publicly');
  }
  return url.href.replace(/\/$/, '');
}

function validatePlatform(info) {
  if (info.platform !== 'darwin' || info.arch !== 'arm64') {
    throw new Error('Apple MLX requires a native arm64 process on an Apple Silicon Mac; Intel, Rosetta and Linux hosts are unsupported');
  }
  if (Number(String(info.macos).split('.')[0]) < 14 || !/^\d+\./.test(info.macos)) {
    throw new Error('Apple MLX requires macOS 14 or newer');
  }
  if (info.pythonArch !== 'arm64' || info.pythonPlatform !== 'darwin') {
    throw new Error('MLX Python must run natively on Apple Silicon, not under Rosetta; select it with HIVE_MLX_PYTHON');
  }
  const [major, minor] = info.pythonVersion || [];
  if (!(major > 3 || (major === 3 && minor >= 10))) {
    throw new Error('MLX requires native Python >=3.10; select your virtualenv Python with HIVE_MLX_PYTHON');
  }
  if (!info.mlx || !info.mlxLM) {
    throw new Error('MLX/MLX-LM is missing in HIVE_MLX_PYTHON; install mlx-lm in your own virtualenv (no global pip install)');
  }
  return info;
}

function hostInfo(env = process.env) {
  if (process.platform !== 'darwin' || process.arch !== 'arm64') {
    throw new Error('Apple MLX requires a native arm64 process on an Apple Silicon Mac; Intel, Rosetta and Linux hosts are unsupported');
  }
  let python;
  try {
    // Metadata only: importing mlx can initialize the GPU.
    python = JSON.parse(execFileSync(env.HIVE_MLX_PYTHON || 'python3', ['-c', [
      'import sys, platform, json, importlib.metadata as m',
      'def version(name):',
      ' try: return m.version(name)',
      ' except m.PackageNotFoundError: return None',
      'print(json.dumps(dict(pythonArch=platform.machine(), pythonPlatform=sys.platform, pythonVersion=list(sys.version_info[:2]), mlx=version("mlx"), mlxLM=version("mlx-lm"))))',
    ].join('\n')], { encoding: 'utf8', timeout: HTTP_TIMEOUT_MS, stdio: ['ignore', 'pipe', 'pipe'] }));
  } catch (_) {
    throw new Error('Cannot inspect MLX Python; set HIVE_MLX_PYTHON to your native Python >=3.10 virtualenv executable');
  }
  return validatePlatform({
    ...python,
    platform: process.platform,
    arch: process.arch,
    macos: process.platform === 'darwin'
      ? execFileSync('sw_vers', ['-productVersion'], { encoding: 'utf8', timeout: HTTP_TIMEOUT_MS }).trim() : '',
  });
}

async function request(endpoint, route, body, timeout = HTTP_TIMEOUT_MS, fetcher = fetch) {
  const url = endpointURL(endpoint);
  try {
    const response = await fetcher(`${url}/${route}`, {
      method: body ? 'POST' : 'GET',
      headers: body ? { 'Content-Type': 'application/json' } : {},
      ...(body ? { body: JSON.stringify(body) } : {}),
      signal: AbortSignal.timeout(timeout),
      redirect: 'error',
    });
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    const reader = response.body.getReader();
    const chunks = [];
    let size = 0;
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        size += value.length;
        if (size > MAX_RESPONSE_BYTES) throw new Error('response exceeds safety limit');
        chunks.push(Buffer.from(value));
      }
    } finally { await reader.cancel(); }
    return Buffer.concat(chunks).toString('utf8');
  } catch (error) {
    throw new Error(`MLX local endpoint request failed (${error.name === 'TimeoutError' ? 'timed out' : error.message}); start your MLX-LM server and check Docker Desktop host networking if containerized`);
  }
}

async function availableModels(endpoint, fetcher) {
  const body = JSON.parse(await request(endpoint, 'models', null, HTTP_TIMEOUT_MS, fetcher));
  if (!Array.isArray(body.data) || !body.data.every(model => typeof model.id === 'string')) {
    throw new Error('MLX /v1/models returned an invalid model list');
  }
  return body.data.map(model => model.id);
}

function parseStream(text) {
  const calls = new Map();
  let content = '', fingerprint = '', finish = '', done = false;
  for (const line of text.split(/\r?\n/)) {
    if (!line.startsWith('data:')) continue;
    const data = line.slice(5).trim();
    if (data === '[DONE]') { done = true; continue; }
    const chunk = JSON.parse(data);
    if (chunk.error) throw new Error('MLX returned a streaming error');
    fingerprint = chunk.system_fingerprint || fingerprint;
    for (const choice of chunk.choices || []) {
      finish = choice.finish_reason || finish;
      content += choice.delta?.content || '';
      for (const call of choice.delta?.tool_calls || []) {
        if (!Number.isInteger(call.index)) throw new Error('MLX tool-call stream has no index');
        const prior = calls.get(call.index) || { id: '', type: 'function', function: { name: '', arguments: '' } };
        prior.id += call.id || '';
        prior.function.name += call.function?.name || '';
        prior.function.arguments += call.function?.arguments || '';
        calls.set(call.index, prior);
      }
    }
  }
  if (!done) throw new Error('MLX stream ended without [DONE]');
  return { calls: [...calls.values()], content, fingerprint, finish };
}

async function verifyTools(endpoint, model, fetcher) {
  const tools = [{
    type: 'function',
    function: {
      name: 'hive_mlx_check',
      description: 'Echo a value to verify structured tool calling. Call this tool exactly once.',
      parameters: { type: 'object', properties: { value: { type: 'string' } }, required: ['value'], additionalProperties: false },
    },
  }];
  const messages = [{ role: 'user', content: `Call hive_mlx_check with value "${PROBE_VALUE}". Do not answer in prose.` }];
  const complete = async () => parseStream(await request(endpoint, 'chat/completions', {
    model, messages, tools, stream: true, max_tokens: PROBE_MAX_TOKENS, temperature: 0,
  }, PROBE_TIMEOUT_MS, fetcher));
  const first = await complete();
  const call = first.calls[0];
  if (first.finish !== 'tool_calls' || first.calls.length !== 1 || !call?.id ||
      call.function.name !== 'hive_mlx_check' || JSON.parse(call.function.arguments).value !== PROBE_VALUE) {
    throw new Error('Selected MLX model/runtime failed the streaming tool-call check; use a model with a supported MLX-LM tool parser/chat template');
  }
  messages.push({ role: 'assistant', content: first.content || null, tool_calls: first.calls });
  messages.push({ role: 'tool', tool_call_id: call.id, content: PROBE_VALUE });
  messages.push({ role: 'user', content: 'Return the tool result verbatim, without calling any more tools.' });
  const second = await complete();
  if (second.calls.length || second.finish !== 'stop' || second.content.trim() !== PROBE_VALUE) {
    throw new Error('Selected MLX model/runtime failed the tool-result round trip');
  }
  return first.fingerprint;
}

function agentDir(env = process.env) {
  return env.PI_CODING_AGENT_DIR || path.join(env.HOME, '.pi', 'agent');
}

function readJSON(file) {
  return JSON.parse(fs.readFileSync(file, 'utf8'));
}

function digest(provider) {
  return createHash('sha256').update(JSON.stringify(provider)).digest('hex');
}

function tokenBudget(raw, fallback) {
  const value = Number(raw || fallback);
  if (!Number.isSafeInteger(value) || value <= 0) throw new Error('MLX context/output token limits must be positive integers');
  return value;
}

function serve(model, env = process.env, inspectHost = hostInfo, launch = execFileSync) {
  const selection = parsePiModelSelection(`mlx/${model || ''}`);
  if (!selection.valid || model === 'default_model') throw new Error('Choose an explicit, already-downloaded MLX model ID or absolute path');
  inspectHost(env);
  if (path.isAbsolute(model)) {
    try {
      if (!fs.statSync(model).isDirectory()) throw new Error('not a directory');
    } catch (_) { throw new Error('MLX model directory is missing; download the model yourself before running contribute-mlx-serve'); }
  }
  const endpoint = new URL(endpointURL(env.HIVE_MLX_ENDPOINT || DEFAULT_ENDPOINT));
  launch(env.HIVE_MLX_PYTHON || 'python3', [
    '-m', 'mlx_lm.server', '--model', model,
    '--host', endpoint.hostname.replace(/^\[|\]$/g, ''),
    '--port', endpoint.port || DEFAULT_HTTP_PORT,
  ], { env: { ...env, HF_HUB_OFFLINE: '1' }, stdio: 'inherit' });
}

async function configure(model, env = process.env, fetcher, inspectHost = hostInfo) {
  if (typeof model !== 'string' || !model) throw new Error('An explicit MLX model ID or absolute path is required');
  const selection = parsePiModelSelection(`mlx/${model}`);
  if (!selection.valid || model === 'default_model') throw new Error('Choose an explicit MLX model ID or absolute path without whitespace, not default_model');
  const info = inspectHost(env);
  const endpoint = endpointURL(env.HIVE_MLX_ENDPOINT || DEFAULT_ENDPOINT);
  if (!(await availableModels(endpoint, fetcher)).includes(model)) {
    throw new Error('MLX model is not listed by /v1/models; download it yourself or start the server with its local absolute path first');
  }
  const contextWindow = tokenBudget(env.HIVE_MLX_CONTEXT_WINDOW, DEFAULT_CONTEXT);
  const maxTokens = tokenBudget(env.HIVE_MLX_MAX_TOKENS, DEFAULT_MAX_TOKENS);
  if (maxTokens >= contextWindow) throw new Error('HIVE_MLX_MAX_TOKENS must be smaller than HIVE_MLX_CONTEXT_WINDOW');
  const dir = agentDir(env);
  const file = path.join(dir, 'models.json');
  const models = fs.existsSync(file) ? readJSON(file) : { providers: {} };
  if (!models || typeof models !== 'object' || Array.isArray(models) ||
      (models.providers !== undefined && (!models.providers || typeof models.providers !== 'object' || Array.isArray(models.providers)))) {
    throw new Error('Refusing to overwrite malformed Pi models.json');
  }
  models.providers ||= {};
  // Deliberately opt-in: the configure CLI requires --verify-tools. Only this
  // explicit operation runs two bounded inference requests, never startup.
  const fingerprint = await verifyTools(endpoint, model, fetcher);
  if (!fingerprint.startsWith(`${info.mlxLM}-${info.mlx}-`)) {
    throw new Error('Endpoint fingerprint does not match your MLX-LM/MLX environment; select the server virtualenv with HIVE_MLX_PYTHON');
  }
  const provider = {
    baseUrl: endpoint, api: 'hive-mlx', apiKey: 'mlx-local', compat: COMPAT,
    models: [{ id: model, name: model, reasoning: false, input: ['text'], contextWindow, maxTokens,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }],
  };
  models.providers.mlx = provider;
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
  // A stale receipt cannot authorize a half-written configuration.
  fs.writeFileSync(file, `${JSON.stringify(models, null, 2)}\n`, { mode: 0o600 });
  fs.chmodSync(file, 0o600);
  fs.writeFileSync(path.join(dir, 'hive-mlx.json'), `${JSON.stringify({
    platform: 'darwin', arch: 'arm64', model, providerDigest: digest(provider),
    mlx: info.mlx, mlxLM: info.mlxLM, fingerprint, verifiedAt: new Date().toISOString(),
  }, null, 2)}\n`, { mode: 0o600 });
  fs.chmodSync(path.join(dir, 'hive-mlx.json'), 0o600);
  return selection.canonical;
}

function configuration(selection, env = process.env) {
  if (!selection.valid || selection.provider !== 'mlx') throw new Error('MLX selection must use mlx/<model>');
  let provider, receipt;
  try {
    provider = readJSON(path.join(agentDir(env), 'models.json')).providers.mlx;
    receipt = readJSON(path.join(agentDir(env), 'hive-mlx.json'));
  } catch (_) { throw new Error('MLX is not configured; run just contribute-mlx-setup <model> --verify-tools on the Mac host first'); }
  if (receipt.platform !== 'darwin' || receipt.arch !== 'arm64' || receipt.model !== selection.model ||
      receipt.providerDigest !== digest(provider)) {
    throw new Error('MLX configuration changed or lacks a matching tool-check receipt; rerun contribute-mlx-setup --verify-tools');
  }
  endpointURL(provider.baseUrl);
  return provider;
}

async function check(raw, env = process.env, fetcher, inspectHost = hostInfo) {
  const selection = parsePiModelSelection(raw);
  const provider = configuration(selection, env);
  if (inspectHost) inspectHost(env);
  if (!(await availableModels(provider.baseUrl, fetcher)).includes(selection.model)) {
    throw new Error('Configured MLX model is missing from /v1/models; restore it and rerun contribute-mlx-setup --verify-tools');
  }
}

function checkRuntime(raw, env = process.env, run = execFileSync) {
  if (!raw.startsWith('mlx/')) return;
  try {
    run(process.execPath, [__filename, 'check', raw, process.platform === 'darwin' ? '--host' : '--endpoint-only'], {
      env, encoding: 'utf8', timeout: HTTP_TIMEOUT_MS * 2, stdio: ['ignore', 'pipe', 'pipe'],
    });
  } catch (error) {
    throw new Error(String(error.stderr || 'MLX preflight failed; check the local server and container host networking').trim());
  }
}

module.exports = { endpointURL, validatePlatform, hostInfo, availableModels, parseStream, verifyTools, configure, configuration, check, checkRuntime, serve, DEFAULT_ENDPOINT };

if (require.main === module) {
  (async () => {
    const [command, model, flag] = process.argv.slice(2);
    if (command === 'configure' && flag === '--verify-tools') {
      const selected = await configure(model);
      console.log(`MLX streaming tool round trip verified. Select AGENT_BACKEND=pi AGENT_MODEL=${selected}`);
    } else if (command === 'serve') {
      serve(model);
    } else if (command === 'check') {
      await check(model, process.env, undefined, flag === '--endpoint-only' ? null : hostInfo);
      console.log('MLX endpoint/model ready; coding execution is provided by Pi, not MLX-LM');
    } else {
      throw new Error('Usage: mlx-backend.js configure <model> --verify-tools (explicitly runs two bounded inference requests), serve <already-downloaded-model> (foreground, offline, loopback), or check mlx/<model>');
    }
  })().catch(error => { console.error(error.message); process.exitCode = 1; });
}
