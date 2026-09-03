'use strict';

// Pi contributor adapter contract (hivecommons/hive#5039).
//
// AGENT_MODEL is the one contributor-owned selection input and MUST be the
// canonical provider/model spelling Pi itself understands.  There is
// deliberately no HIVE/PI provider variable: provider-specific credentials
// stay in Pi's official environment variables or ~/.pi/agent/auth.json, and
// the hub remains the sole authority for task assignment.

const fs = require('fs');
const path = require('path');

const PROVIDER_MAX_LEN = 64;
const MODEL_MAX_LEN = 256;

// Official Pi provider credential variables. Values are inspected only for
// presence and exact-value redaction; they are never returned or logged.
const PROVIDER_CREDENTIAL_ENV = Object.freeze({
  anthropic: ['ANTHROPIC_API_KEY'],
  'azure-openai-responses': ['AZURE_OPENAI_API_KEY'],
  openai: ['OPENAI_API_KEY'],
  deepseek: ['DEEPSEEK_API_KEY'],
  google: ['GEMINI_API_KEY'],
  mistral: ['MISTRAL_API_KEY'],
  groq: ['GROQ_API_KEY'],
  cerebras: ['CEREBRAS_API_KEY'],
  'cloudflare-ai-gateway': ['CLOUDFLARE_API_KEY', 'CLOUDFLARE_ACCOUNT_ID', 'CLOUDFLARE_GATEWAY_ID'],
  'cloudflare-workers-ai': ['CLOUDFLARE_API_KEY', 'CLOUDFLARE_ACCOUNT_ID'],
  xai: ['XAI_API_KEY'],
  openrouter: ['OPENROUTER_API_KEY'],
  'vercel-ai-gateway': ['AI_GATEWAY_API_KEY'],
  zai: ['ZAI_API_KEY'],
  opencode: ['OPENCODE_API_KEY'],
  'opencode-go': ['OPENCODE_API_KEY'],
  huggingface: ['HF_TOKEN'],
  fireworks: ['FIREWORKS_API_KEY'],
  'kimi-coding': ['KIMI_API_KEY'],
  minimax: ['MINIMAX_API_KEY'],
  'minimax-cn': ['MINIMAX_CN_API_KEY'],
  xiaomi: ['XIAOMI_API_KEY'],
  'xiaomi-token-plan-cn': ['XIAOMI_TOKEN_PLAN_CN_API_KEY'],
  'xiaomi-token-plan-ams': ['XIAOMI_TOKEN_PLAN_AMS_API_KEY'],
  'xiaomi-token-plan-sgp': ['XIAOMI_TOKEN_PLAN_SGP_API_KEY'],
  // Ambient cloud credentials: presence means configured, never authenticated.
  'amazon-bedrock': ['AWS_PROFILE', 'AWS_ACCESS_KEY_ID', 'AWS_SECRET_ACCESS_KEY', 'AWS_SESSION_TOKEN', 'AWS_BEARER_TOKEN_BEDROCK', 'AWS_WEB_IDENTITY_TOKEN_FILE', 'AWS_REGION'],
  'google-vertex': ['GOOGLE_APPLICATION_CREDENTIALS', 'GOOGLE_CLOUD_PROJECT', 'GOOGLE_CLOUD_LOCATION'],
});

function parsePiModelSelection(raw) {
  if (typeof raw !== 'string' || raw.length === 0) {
    return { valid: false, state: 'missing', error: 'AGENT_MODEL is required for the Pi backend; use provider/model' };
  }
  if (raw !== raw.trim() || /[\x00-\x20\x7f]/.test(raw) || raw.length > MODEL_MAX_LEN) {
    return { valid: false, state: 'invalid', error: `invalid Pi AGENT_MODEL; expected one bounded provider/model token` };
  }
  const slash = raw.indexOf('/');
  if (slash <= 0 || slash === raw.length - 1) {
    return { valid: false, state: 'invalid', error: 'invalid Pi AGENT_MODEL; expected provider/model' };
  }
  const provider = raw.slice(0, slash);
  const model = raw.slice(slash + 1);
  if (provider.length > PROVIDER_MAX_LEN || !/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(provider)) {
    return { valid: false, state: 'invalid', error: 'invalid Pi provider in AGENT_MODEL' };
  }
  // This token is also used by the interactive shell launcher. Restrict it to
  // model-ID punctuation so a contributor preference can never become shell
  // syntax while retaining paths (OpenRouter), tags (Ollama), and revisions.
  if (!/^[A-Za-z0-9][A-Za-z0-9._/+:@~-]*$/.test(model)) {
    return { valid: false, state: 'invalid', error: 'invalid Pi model in AGENT_MODEL' };
  }
  return { valid: true, state: 'configured', provider, model, canonical: raw };
}

function piAgentDir(env = process.env) {
  return env.PI_CODING_AGENT_DIR || path.join(env.HOME || '', '.pi', 'agent');
}

function readJSON(file) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); } catch (_) { return null; }
}

function authEntryConfigured(entry) {
  if (typeof entry === 'string') return entry.trim().length > 0;
  if (!entry || typeof entry !== 'object') return false;
  return Object.values(entry).some(v => typeof v === 'string' && v.trim().length > 0);
}

function piCredentialConfiguration(selection, env = process.env) {
  if (!selection || !selection.valid) return 'unknown';
  const provider = selection.provider.toLowerCase();
  const auth = readJSON(path.join(piAgentDir(env), 'auth.json'));
  if (auth && authEntryConfigured(auth[provider])) return 'configured_unverified';

  const names = PROVIDER_CREDENTIAL_ENV[provider] || [];
  if (names.some(name => typeof env[name] === 'string' && env[name].trim().length > 0)) {
    return 'configured_unverified';
  }

  // A custom provider may carry its narrow auth reference in models.json. We
  // report only its presence; resolving or printing the value is Pi's job.
  const models = readJSON(path.join(piAgentDir(env), 'models.json'));
  const custom = models && models.providers && models.providers[selection.provider];
  if (custom && authEntryConfigured(custom.apiKey)) return 'configured_unverified';
  return 'missing';
}

function collectStrings(value, out) {
  if (typeof value === 'string') {
    if (value.length >= 4 && !value.startsWith('!') && !/^[A-Z][A-Z0-9_]*$/.test(value)) out.push(value);
    return;
  }
  if (Array.isArray(value)) return value.forEach(v => collectStrings(v, out));
  if (value && typeof value === 'object') Object.values(value).forEach(v => collectStrings(v, out));
}

function piCredentialValues(selection, env = process.env) {
  if (!selection || !selection.valid) return [];
  const provider = selection.provider.toLowerCase();
  const values = [];
  for (const name of PROVIDER_CREDENTIAL_ENV[provider] || []) {
    if (typeof env[name] === 'string' && env[name].length >= 4) values.push(env[name]);
  }
  const auth = readJSON(path.join(piAgentDir(env), 'auth.json'));
  if (auth) collectStrings(auth[provider], values);
  const models = readJSON(path.join(piAgentDir(env), 'models.json'));
  const custom = models && models.providers && models.providers[selection.provider];
  if (custom) collectStrings(custom.apiKey, values);
  return [...new Set(values)].sort((a, b) => b.length - a.length);
}

function providerCredentialEnvNames(selection) {
  if (!selection || !selection.valid) return [];
  return [...(PROVIDER_CREDENTIAL_ENV[selection.provider.toLowerCase()] || [])];
}

function unselectedProviderCredentialEnvNames(selection) {
  const selected = new Set(providerCredentialEnvNames(selection));
  return [...new Set(Object.values(PROVIDER_CREDENTIAL_ENV).flat())]
    .filter(name => !selected.has(name))
    .sort();
}

function rewriteJSON(file, value) {
  const temporary = `${file}.hive-${process.pid}`;
  fs.writeFileSync(temporary, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
  fs.renameSync(temporary, file);
}

function onlySelectedProvider(entries, provider) {
  if (!entries || typeof entries !== 'object' || Array.isArray(entries)) return entries;
  const wanted = provider.toLowerCase();
  return Object.fromEntries(Object.entries(entries).filter(([name]) => name.toLowerCase() === wanted));
}

// The contributor container gets an ephemeral Pi config copy. Narrow the two
// official credential-bearing maps before mounting it so selecting one provider
// cannot expose auth material for every other provider in the host profile.
function narrowPiStage(stageDir, selection) {
  if (!selection || !selection.valid) throw new Error(selection?.error || 'invalid Pi selection');
  const authFile = path.join(stageDir, 'agent', 'auth.json');
  const auth = readJSON(authFile);
  if (fs.existsSync(authFile) && (!auth || typeof auth !== 'object' || Array.isArray(auth))) {
    throw new Error('refusing to stage malformed Pi auth.json');
  }
  if (auth && typeof auth === 'object' && !Array.isArray(auth)) {
    rewriteJSON(authFile, onlySelectedProvider(auth, selection.provider));
  }
  const modelsFile = path.join(stageDir, 'agent', 'models.json');
  const models = readJSON(modelsFile);
  if (fs.existsSync(modelsFile) && (!models || typeof models !== 'object' || Array.isArray(models))) {
    throw new Error('refusing to stage malformed Pi models.json');
  }
  if (models && typeof models === 'object' && !Array.isArray(models) && models.providers) {
    rewriteJSON(modelsFile, { ...models, providers: onlySelectedProvider(models.providers, selection.provider) });
  }
}

function redactPiCredentials(text, selection, env = process.env) {
  let out = String(text || '');
  for (const value of piCredentialValues(selection, env)) out = out.split(value).join('***REDACTED***');
  return out;
}

function piReadiness(selection, cliPresent, invocation = 'untested', env = process.env) {
  return {
    pi_binary: cliPresent ? 'present' : 'unavailable',
    pi_configuration: selection && selection.valid ? 'configured' : ((selection && selection.state) || 'missing'),
    // A configured secret is not authentication proof. Only a successful real
    // provider invocation advances this stage to verified.
    pi_authentication: invocation === 'succeeded' ? 'verified' : piCredentialConfiguration(selection, env),
    pi_invocation: invocation,
  };
}

module.exports = {
  parsePiModelSelection,
  piCredentialConfiguration,
  piCredentialValues,
  providerCredentialEnvNames,
  unselectedProviderCredentialEnvNames,
  narrowPiStage,
  redactPiCredentials,
  piReadiness,
  PROVIDER_CREDENTIAL_ENV,
};

if (require.main === module) {
  const command = process.argv[2] || '';
  const selectionArg = command.startsWith('--') ? process.argv[3] : command;
  const selection = parsePiModelSelection(selectionArg || '');
  if (!selection.valid) {
    process.stderr.write(`${selection.error}\n`);
    process.exit(2);
  }
  if (command === '--env-names') {
    process.stdout.write(`${providerCredentialEnvNames(selection).join('\n')}\n`);
    process.exit(0);
  }
  if (command === '--unselected-env-names') {
    process.stdout.write(`${unselectedProviderCredentialEnvNames(selection).join('\n')}\n`);
    process.exit(0);
  }
  if (command === '--stage') {
    const stageDir = process.argv[4] || '';
    if (!stageDir) {
      process.stderr.write('Pi stage directory is required\n');
      process.exit(2);
    }
    narrowPiStage(stageDir, selection);
    process.exit(0);
  }
  // Machine-readable and credential-free: useful to shell entrypoints without
  // teaching them a second parser that can drift from relay restart behavior.
  process.stdout.write(`${JSON.stringify({ provider: selection.provider, model: selection.canonical, configuration: selection.state, authentication: piCredentialConfiguration(selection) })}\n`);
}
