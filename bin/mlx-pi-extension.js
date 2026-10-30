'use strict';

const { configuration, endpointURL } = require('./mlx-backend.js');
const { parsePiModelSelection } = require('./pi-backend.js');

function localFetch(endpoint, fetcher = globalThis.fetch) {
  const allowed = `${endpointURL(endpoint)}/chat/completions`;
  return (input, init) => {
    const url = typeof input === 'string' || input instanceof URL ? String(input) : input.url;
    if (url !== allowed) throw new Error('MLX inference request left its configured loopback endpoint');
    // Apply this to Pi's REAL inference transport, not just the setup probe.
    // In particular a 307/308 must never forward task prompts to another host.
    return fetcher(input, { ...init, redirect: 'error' });
  };
}

function registerMlx(pi, api, env = process.env, fetcher = globalThis.fetch) {
  const selection = parsePiModelSelection(env.AGENT_MODEL || '');
  const provider = configuration(selection, env);
  const guardedFetch = localFetch(provider.baseUrl, fetcher);
  pi.registerProvider('mlx', {
    ...provider,
    streamSimple(model, context, options) {
      if (model.provider !== 'mlx' || model.id !== selection.model || model.baseUrl !== provider.baseUrl) {
        throw new Error('MLX model changed; reconfigure and restart the contributor relay');
      }
      // Reuse Pi's maintained OpenAI message/tool conversion, stream decoder,
      // usage accounting and AbortSignal handling. Do not build a new executor.
      return api.streamSimple({ ...model, api: 'openai-completions' }, context, {
        ...options, fetch: guardedFetch,
      });
    },
  });
}

module.exports = function mlxExtension(pi) {
  // Pi 1.4.0 resolves this through its extension compatibility entrypoint,
  // including in standalone distributions. No global Pi dependency is needed
  // by ClankeR's relay or its test runner.
  const { openAICompletionsApi, PI_AI_VERSION } = require('@earendil-works/pi-ai');
  const [major, minor] = String(PI_AI_VERSION || '').split('.').map(Number);
  if (typeof openAICompletionsApi !== 'function' || !(major > 1 || (major === 1 && minor >= 4))) {
    throw new Error('ClankeR MLX requires Pi >=1.4.0 with custom fetch transport support; update your Pi installation');
  }
  registerMlx(pi, openAICompletionsApi());
};
module.exports.localFetch = localFetch;
module.exports.registerMlx = registerMlx;
