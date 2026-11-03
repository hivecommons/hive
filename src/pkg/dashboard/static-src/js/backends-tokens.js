    // ── Centralized backend/model config (mirrors backends.conf) ──────────
    // 'openrouter' is a first-class inference method (an OpenAI-compatible gateway
    // preset), so it appears in every backend/method picker even before a matching
    // Model Gateway is configured — the user picks it, then funds/configures it.
    const KNOWN_BACKENDS = ['claude', 'copilot', 'bob', 'gemini', 'agy', 'omp', 'codex', 'goose', 'aider', 'vllm', 'llm-d', 'litellm', 'openrouter', 'watsonx'];
    const FREE_BACKENDS = ['copilot', 'goose'];
    const INFERENCE_BACKENDS = ['vllm', 'llm-d', 'litellm', 'openrouter', 'watsonx'];
    let CONFIG_BACKEND_OPTIONS = [];
    function includesNameCI(list, name) {
      const want = String(name || '').toLowerCase();
      return !!want && list.some(v => String(v || '').toLowerCase() === want);
    }
    // Names of Model Gateways configured under Settings → Model Gateways
    // (e.g. 'openrouter'). Keep every configured spelling here so agents that
    // reference a gateway with different casing than a built-in backend still
    // classify as gateway-backed; backendOptionList() does display-only de-dupe.
    let _configuredGateways = [];
    function isInferenceBackendName(backend) {
      return INFERENCE_BACKENDS.includes(backend) || includesNameCI(_configuredGateways, backend);
    }
    function canonicalInferenceBackendName(backend) {
      const want = String(backend || '').toLowerCase();
      return _configuredGateways.find(g => String(g || '').toLowerCase() === want) || backend;
    }
    // Combined option list for the backend/method dropdowns: KNOWN_BACKENDS
    // first (stable order), then any configured gateway names not already in
    // KNOWN_BACKENDS (dedup — e.g. a gateway literally named 'litellm').
    function backendOptionList() {
      return KNOWN_BACKENDS.concat(_configuredGateways.filter(g => !includesNameCI(KNOWN_BACKENDS, g)));
    }
    // Fetch configured gateway names from the existing governor endpoint. Kept
    // resilient: any failure leaves _configuredGateways as-is (dropdowns still
    // render KNOWN_BACKENDS). Excludes names already in KNOWN_BACKENDS.
    async function fetchConfiguredGateways() {
      try {
        const res = await fetch('/api/config/governor/gateways');
        if (!res.ok) return;
        const data = await res.json();
        const next = (data.gateways || [])
          .map(g => g.name)
          .filter(name => name);
        const changed = JSON.stringify(next) !== JSON.stringify(_configuredGateways);
        _configuredGateways = next;
        if (changed && window._lastAgents) renderAgents(window._lastAgents);
      } catch (e) { /* keep prior list; dropdowns fall back to KNOWN_BACKENDS */ }
    }
    // Fallback list only — the server now discovers claude models live via
    // GET api.anthropic.com/v1/models (see claudeStaticModels in
    // pkg/dashboard/cli_models.go; keep the two lists in sync). Refreshed
    // 2026-08-03 from a live /v1/models response, in API order, plus the bare
    // aliases the claude CLI accepts as --model values; Sonnet 5.5 added per
    // the Claude Code 2.1.284 changelog (#9804).
    const CLAUDE_CLI_MODELS = [
      { value: 'claude-opus-5-5', label: 'Opus 5.5' },
      { value: 'claude-opus-5', label: 'Opus 5' },
      { value: 'claude-sonnet-5-5', label: 'Sonnet 5.5' },
      { value: 'claude-sonnet-5', label: 'Sonnet 5' },
      { value: 'claude-fable-5', label: 'Fable 5' },
      { value: 'claude-opus-4-8', label: 'Opus 4.8' },
      { value: 'claude-opus-4-7', label: 'Opus 4.7' },
      { value: 'claude-sonnet-4-6', label: 'Sonnet 4.6' },
      { value: 'claude-opus-4-6', label: 'Opus 4.6' },
      { value: 'claude-opus-4-5-20251101', label: 'Opus 4.5' },
      { value: 'claude-haiku-4-5-20251001', label: 'Haiku 4.5' },
      { value: 'claude-sonnet-4-5-20250929', label: 'Sonnet 4.5' },
      { value: 'claude-opus-4-1-20250805', label: 'Opus 4.1' },
      { value: 'opus', label: 'Opus (alias)' },
      { value: 'sonnet', label: 'Sonnet (alias)' },
      { value: 'haiku', label: 'Haiku (alias)' },
    ];
    const COPILOT_CLI_MODELS = [
      // Auto sentinel: `copilot --model auto` lets the Copilot CLI pick/adjust
      // the optimal model per task. Not a concrete model id — treated as an
      // always-valid selection that reconcileModelsAfterDiscovery never heals.
      { value: 'auto', label: 'Auto (Copilot picks)' },
      { value: 'mai-code-1-flash-picker', label: 'MAI Code 1 Flash' },
      // The -5 family is DASHED in copilot CLI nomenclature (claude-fable-5,
      // not claude-fable.5) while the 4.x family is DOTTED — keep in sync with
      // copilotCLIAcceptedModels in pkg/agent/copilot_models.go (#4262).
      { value: 'claude-opus-5', label: 'Opus 5' },
      { value: 'claude-sonnet-5', label: 'Sonnet 5' },
      { value: 'claude-fable-5', label: 'Fable 5' },
      { value: 'claude-opus-4.6', label: 'Opus 4.6' },
      { value: 'claude-sonnet-4.6', label: 'Sonnet 4.6' },
      { value: 'claude-sonnet-4.5', label: 'Sonnet 4.5' },
      { value: 'claude-haiku-4.5', label: 'Haiku 4.5' },
      { value: 'gemini-flash-3.5', label: 'Gemini Flash 3.5' },
      { value: 'gemini-2.5-pro', label: 'Gemini 2.5 Pro' },
      { value: 'gpt-5.6-sol', label: 'GPT-5.6 Sol' },
      { value: 'gpt-5.6-terra', label: 'GPT-5.6 Terra' },
      { value: 'gpt-5.6-luna', label: 'GPT-5.6 Luna' },
      { value: 'gpt-5.5', label: 'GPT-5.5' },
      { value: 'gpt-5.4', label: 'GPT-5.4' },
      { value: 'gpt-5.2', label: 'GPT-5.2' },
    ];
    const GEMINI_CLI_MODELS = [
      { value: 'gemini-3-pro-preview', label: 'Gemini 3 Pro Preview' },
      { value: 'gemini-3-flash-preview', label: 'Gemini 3 Flash Preview' },
      { value: 'gemini-2.5-pro', label: 'Gemini 2.5 Pro' },
      { value: 'gemini-2.5-flash', label: 'Gemini 2.5 Flash' },
    ];
    // Codex (OpenAI CLI) exposes its own OpenAI-only models. Claude models are
    // rejected by Codex with a ChatGPT account ("model is not supported when
    // using Codex with a ChatGPT account"), so codex must NOT fall through to
    // the copilot list. The server discovers the live catalog via
    // `codex app-server` model/list (per-CLI-version, baked into the binary);
    // this is only the pre-discovery fallback. Snapshot of codex 0.165.0
    // (live model/list capture, 2026-10-27; the 0.159.0 capture was #9804):
    // visible ids in picker order (GPT-6.1 Sol is the default; GPT-6.1 Terra
    // arrived in 0.165.0; GPT-6 Sol and Luna arrived in 0.156.1, #8417), then
    // hidden ids — hidden entries are still valid --model values. gpt-5.2 and
    // gpt-5.4-mini were retired in 0.155.0; gpt-5.4 was retired between
    // 0.156.1 and 0.159.0.
    // Keep in sync with codexStaticModels in cli_models.go.
    const CODEX_CLI_MODELS = [
      { value: 'gpt-6.1-sol', label: 'GPT-6.1 Sol' },
      { value: 'gpt-6-astra', label: 'GPT-6 Astra' },
      { value: 'gpt-6-sol', label: 'GPT-6 Sol' },
      { value: 'gpt-6-luna', label: 'GPT-6 Luna' },
      { value: 'gpt-5.6-sol', label: 'GPT-5.6 Sol' },
      { value: 'gpt-6.1-terra', label: 'GPT-6.1 Terra' },
      { value: 'gpt-5.6-terra', label: 'GPT-5.6 Terra' },
      { value: 'gpt-5.6-luna', label: 'GPT-5.6 Luna' },
      { value: 'gpt-5.5', label: 'GPT-5.5' },
      { value: 'gpt-daybreak-blue-latest', label: 'GPT Daybreak Blue' },
      { value: 'gpt-daybreak-red-latest', label: 'GPT Daybreak Red' },
      { value: 'codex-auto-review', label: 'Codex Auto Review' },
    ];
    // bob (IBM bobshell) selects its own model and has NO usable --model flag:
    // launching `bob --model <concrete-id>` makes every prompt die with
    // "Cannot read properties of undefined (reading 'maxTokens')" (verified
    // live on a spoke), while the same bob with no --model runs inference fine.
    // So bob gets exactly ONE option — the auto sentinel — and the launch path
    // (bobLaunchCmd) passes no --model at all. This value is a display/stored
    // placeholder, never a command-line argument.
    const BOB_CLI_MODELS = [
      { value: 'auto', label: 'Auto (bob picks)' },
    ];
    const GOOSE_CLI_MODELS = [
      { value: 'default', label: 'Default' },
    ];
    // Google Antigravity exposes an account-aware catalog through `agy models`.
    // This is the first-paint / failed-probe fallback, captured from agy 1.1.18
    // on 2026-08-31 in the CLI's own order. Keep in sync with agyStaticModels
    // in cli_models_agy.go.
    const AGY_CLI_MODELS = [
      { value: 'gemini-3.7-flash-high', label: 'Gemini 3.7 Flash (High)' },
      { value: 'gemini-3.7-flash-medium', label: 'Gemini 3.7 Flash (Medium)' },
      { value: 'gemini-3.7-flash-low', label: 'Gemini 3.7 Flash (Low)' },
      { value: 'gemini-3.6-flash-high', label: 'Gemini 3.6 Flash (High)' },
      { value: 'gemini-3.6-flash-medium', label: 'Gemini 3.6 Flash (Medium)' },
      { value: 'gemini-3.6-flash-low', label: 'Gemini 3.6 Flash (Low)' },
      { value: 'gemini-3.1-pro-high', label: 'Gemini 3.1 Pro (High)' },
      { value: 'gemini-3.1-pro-low', label: 'Gemini 3.1 Pro (Low)' },
      { value: 'claude-sonnet-4-6', label: 'Claude Sonnet 4.6 (Thinking)' },
      { value: 'claude-opus-4-6-thinking', label: 'Claude Opus 4.6 (Thinking)' },
      { value: 'gpt-oss-120b-medium', label: 'GPT-OSS 120B (Medium)' },
    ];
    // Oh My Pi is a multi-provider CLI; selectors keep their provider prefix.
    // Keep in sync with ompStaticModels in pkg/dashboard/cli_models_omp.go.
    const OMP_CLI_MODELS = [
      { value: 'openai-codex/gpt-5.6-terra', label: 'openai-codex/gpt-5.6-terra' },
      { value: 'openai-codex/gpt-5.6-sol', label: 'openai-codex/gpt-5.6-sol' },
      { value: 'openai-codex/gpt-5.6-luna', label: 'openai-codex/gpt-5.6-luna' },
      { value: 'anthropic/claude-opus-5', label: 'anthropic/claude-opus-5' },
      { value: 'anthropic/claude-fable-5', label: 'anthropic/claude-fable-5' },
      { value: 'anthropic/claude-sonnet-5-5', label: 'anthropic/claude-sonnet-5-5' },
      { value: 'anthropic/claude-sonnet-5', label: 'anthropic/claude-sonnet-5' },
      { value: 'google-antigravity/gemini-3.7-flash', label: 'google-antigravity/gemini-3.7-flash' },
    ];
    const KNOWN_MODELS = COPILOT_CLI_MODELS;
    // CLI_BACKENDS gates which /api/config/backends entries populate
    // BACKEND_MODELS. Every CLI with a server-served inventory must appear
    // here; otherwise its entry is dropped and modelsForBackend falls back to
    // the pre-discovery catalog (or, for an omitted unknown CLI, Copilot's).
    const CLI_BACKENDS = ['claude', 'copilot', 'bob', 'gemini', 'agy', 'omp', 'goose', 'codex'];
    // BACKEND_MODELS holds the live-discovered model list for every backend
    // (inference AND cli), keyed by backend id. INFERENCE_MODELS is kept as
    // an alias to the same object for backward compatibility with existing
    // call sites that read/write it directly (e.g. SSE push handling).
    const BACKEND_MODELS = {};
    const INFERENCE_MODELS = BACKEND_MODELS;
    const INFERENCE_MODEL_CACHE_TTL_MS = 30000;
    const _inferenceModelCache = {};
    // Hardcoded fallbacks per CLI backend, used only until /api/config/backends
    // responds for the first time (so a dropdown is never empty on paint).
    const CLI_FALLBACK_MODELS = {
      claude: CLAUDE_CLI_MODELS,
      copilot: COPILOT_CLI_MODELS,
      bob: BOB_CLI_MODELS,
      gemini: GEMINI_CLI_MODELS,
      agy: AGY_CLI_MODELS,
      omp: OMP_CLI_MODELS,
      goose: GOOSE_CLI_MODELS,
      codex: CODEX_CLI_MODELS,
    };
    function backendLabel(b) {
      // Configured gateways (e.g. openrouter) are inference/gateway backends,
      // so they carry the same ⚡ marker as vllm/llm-d/litellm.
      if (isInferenceBackendName(b)) return b + ' ⚡';
      if (b === 'agy') return 'Google Antigravity (agy)';
      if (b === 'omp') return 'Oh My Pi (omp)';
      return b;
    }
    // Auto-select sentinels: values meaning "let the CLI pick its own model"
    // rather than pinning a concrete id. They are never in a discovered model
    // catalog, so they must be exempted from model auto-heal — otherwise
    // reconcileModelsAfterDiscovery rewrites them to a concrete discovered id.
    //
    // How each reaches the CLI differs, and the difference is load-bearing:
    //   - copilot: `auto` flows VERBATIM through the launch path as
    //     `--model auto`. It is a real, supported Copilot CLI flag value.
    //   - bob: `auto` is NEVER passed on the command line. bob has no working
    //     --model flag (a concrete id breaks inference with "Cannot read
    //     properties of undefined (reading 'maxTokens')"), and `bob --model
    //     auto` is untested/presumed unsafe. bobLaunchCmd omits --model
    //     entirely; `auto` exists only as the stored/displayed selection.
    const AUTO_MODEL_SENTINELS = { copilot: ['auto'], bob: ['auto'] };
    function isAutoModelSentinel(backend, model) {
      return (AUTO_MODEL_SENTINELS[backend] || []).includes(model);
    }
    // Backends with a reasoning-effort control and the values each accepts.
    // Mirrors config.ReasoningEffortsByBackend server-side: codex is launched
    // with -c model_reasoning_effort="<v>", agy with --effort <v>, and claude
    // with --effort <v> (hivecommons/hive#8377; unset leaves Claude Code at
    // its own default). Backends absent here have no effort flag, so they get
    // no dropdown at all.
    const BACKEND_REASONING_EFFORTS = {
      codex: ['minimal', 'low', 'medium', 'high', 'xhigh'],
      agy: ['low', 'medium', 'high'],
      claude: ['low', 'medium', 'high', 'xhigh', 'max'],
    };
    const MODEL_REASONING_EFFORTS = {};

    function reasoningEffortsForAgent(a) {
      if (!a) return [];
      if (a.cli === 'omp') {
        const perModel = MODEL_REASONING_EFFORTS.omp || {};
        const model = a.model || '';
        if (perModel[model]) return perModel[model];
        const tail = model.split('/').pop();
        for (const [id, levels] of Object.entries(perModel)) {
          if (id === model || id.split('/').pop() === tail) return levels || [];
        }
        return [];
      }
      return BACKEND_REASONING_EFFORTS[a.cli] || [];
    }
    // contributeEffortFloorOptions returns the effort-floor dropdown options:
    // the union of the per-backend effort vocabularies
    // (BACKEND_REASONING_EFFORTS), ordered on the server's canonical ladder,
    // always keeping the currently-configured floor even if unknown.
    function contributeEffortFloorOptions(effortLadder, minEffort) {
      const known = new Set();
      for (const b in BACKEND_REASONING_EFFORTS) {
        (BACKEND_REASONING_EFFORTS[b] || []).forEach(function(e) { known.add(e); });
      }
      return effortLadder.filter(function(e) { return known.has(e) || e === minEffort; });
    }
    // effortSelectHtml renders the reasoning-effort dropdown beside the model
    // dropdown, only for backends that have an effort control. "default"
    // clears the stored effort back to the backend's own default (the server
    // treats the path value "default" as clear — a path segment cannot be
    // empty).
    function effortSelectHtml(a) {
      const efforts = reasoningEffortsForAgent(a);
      if (!efforts.length) return '';
      const cur = a.reasoningEffort || '';
      return `<select class="hv-btn btn-secondary btn backend-select" data-change-action="switchEffort" data-agent="${esc(a.name)}" title="Set this agent's reasoning effort (restarts the session). 'default' uses the backend's own default effort.">
            <option value="" disabled selected>${cur ? esc(cur) : 'effort'} ▾</option>
            <option value="default"${cur ? '' : ' disabled'}>default</option>
            ${efforts.map(e => `<option value="${e}"${e === cur ? ' disabled' : ''}>${e}</option>`).join('')}
          </select>`;
    }
    // Backends whose model list is a single fixed option, so there is nothing
    // for a user to choose and nothing meaningful to store per agent.
    const SINGLE_OPTION_BACKENDS = ['bob'];
    function modelsForBackend(backend) {
      // Single-option backends are authoritative and never overridden by
      // discovery: even if the server (or a stale cached response) offered a
      // longer list, bob cannot honor a model choice, so the local one-option
      // list wins. This also guarantees bob never reaches the copilot fallback.
      if (SINGLE_OPTION_BACKENDS.includes(backend)) {
        return CLI_FALLBACK_MODELS[backend] || [];
      }
      const discovered = BACKEND_MODELS[backend] || BACKEND_MODELS[canonicalInferenceBackendName(backend)];
      if (discovered && discovered.length) return discovered;
      if (isInferenceBackendName(backend)) return [];
      // NOTE (latent trap): any UNKNOWN backend still falls through to the
      // copilot catalog and is offered models it may not support — the exact
      // bug this change fixes for bob. Left as-is deliberately: gateway
      // backends rely on a non-empty first paint, so narrowing it needs its
      // own change. Every known CLI backend is now explicit in
      // CLI_FALLBACK_MODELS, so nothing shipped reaches this line by accident.
      return CLI_FALLBACK_MODELS[backend] || COPILOT_CLI_MODELS;
    }
    // _modelIdMatches: an agent's stored model may lack (or carry) the
    // gateway provider prefix that discovery reports (e.g. stored "gpt-4o"
    // vs discovered "Azure/gpt-4o", or vice-versa). Match on the full id,
    // the suffix after the last slash, or the normalized dotted/hyphenated
    // version so the current model preselects regardless of prefix.
    function _modelIdMatches(optionValue, current) {
      if (!current) return false;
      if (optionValue === current) return true;
      const optTail = optionValue.split('/').pop();
      const curTail = current.split('/').pop();
      if (optTail === curTail) return true;
      return _modelsEqual(optTail, curTail);
    }
    // modelOptionsHtml builds the <option> list for a model dropdown:
    // discovered/known models first (already discovered-first from the API),
    // the current model preselected (prefix-insensitive), and — if the
    // current model is not in the list — a preselected fallback option so
    // the dropdown never silently drops an agent's active selection.
    function modelOptionsHtml(backend, currentModel) {
      const models = modelsForBackend(backend);
      // SINGLE_OPTION_BACKENDS offer exactly one valid selection, so an agent's
      // stored model carries no information — and bob agents created before
      // this change hold a stale concrete id (e.g. "claude-sonnet-4.6") that
      // bob cannot honor. Substituting the sole option as the "current" value
      // both preselects it (with the usual " ▾" caret) and starves the
      // "(current)" branch below, which would otherwise resurrect the stale id
      // as a pinned, preselected option.
      const soleOption = SINGLE_OPTION_BACKENDS.includes(backend) && models.length === 1
        ? models[0].value
        : '';
      const cur = soleOption || currentModel || '';
      let matched = false;
      const opts = models.map(m => {
        const sel = !matched && _modelIdMatches(m.value, cur);
        if (sel) matched = true;
        // The selected option's label doubles as the button's visible text
        // (native <select> renders it in place of the placeholder), so it
        // carries the same " ▾" caret as the method button for a
        // consistent "<value> ▾" look across both dropdown buttons.
        return `<option value="${esc(m.value)}"${sel ? ' selected' : ''}>${esc(m.label)}${sel ? ' ▾' : ''}</option>`;
      });
      if (cur && !matched) {
        opts.unshift(`<option value="${esc(cur)}" selected>${esc(cur.split('/').pop())} (current) ▾</option>`);
      }
      return opts.join('');
    }
    async function fetchInferenceModels(backend, force) {
      const now = Date.now();
      const cached = _inferenceModelCache[backend];
      if (!force && cached && (now - cached.ts) < INFERENCE_MODEL_CACHE_TTL_MS) {
        return cached.models;
      }
      try {
        const resp = await fetch(`/api/inference/models/${encodeURIComponent(backend)}`);
        // A non-OK response (404 for a backend with no registered endpoint —
        // e.g. watsonx/litellm before any gateway is configured) is NOT a
        // discovery result. Caching it as {models:[], fallback:false} made the
        // maybeOpenMethodConfig/ensureGatewayMethodConfigured guards read it
        // as a successful live discovery and silently skip the Model Gateways
        // redirect when the operator picked the method (#2778 regression).
        if (!resp.ok) {
          return INFERENCE_MODELS[backend] || [];
        }
        const data = await resp.json();
        // data.fallback: endpoint discovery failed and the server returned
        // static common aliases — mark them as unverified against the key.
        // data.entitled: the server already narrowed data.models to the set
        // the configured key is entitled to (some gateways advertise the full
        // catalog on /v1/models but scope the key to a subset that 403s at
        // inference). The dropdown therefore shows only usable models; the
        // label notes the scope so users know why non-entitled models are gone.
        const labelSuffix = data.fallback ? ' (common alias, unverified)'
          : (data.entitled ? ' (your key)' : '');
        const models = (data.models || []).map(m => ({ value: m, label: m.split('/').pop() + labelSuffix, suffix: labelSuffix }));
        INFERENCE_MODELS[backend] = models;
        _inferenceModelCache[backend] = {
          models, ts: now, fallback: !!data.fallback, entitled: !!data.entitled,
          // default_model: what an agent on this backend uses when it names
          // none; entitledModels: the FULL set the gateway's team may use, so
          // a stored model outside it can be flagged (#9945).
          defaultModel: data.default_model || '',
          entitledModels: Array.isArray(data.entitled_models) ? data.entitled_models : null,
        };
        // A real (non-fallback) discovery may have changed the entitlement-
        // filtered model set — reconcile agents/defaults whose selected model
        // is no longer available. No-op when the list is a static fallback or
        // when every current selection is still present (idempotent).
        // data.partial: some of the backend's endpoints answered and others
        // did not, so the list is a floor, not a census — offer it in the
        // dropdown, but never let it retire a selection, because auto-heal
        // rewrites the agent's model and relaunches its session (#4438).
        reconcileModelsAfterDiscovery(backend, models, !!data.fallback || !!data.partial);
        return models;
      } catch (e) {
        return INFERENCE_MODELS[backend] || [];
      }
    }
    function refreshBackendDiscovery(force) {
      INFERENCE_BACKENDS.forEach(b => fetchInferenceModels(b, force));
      // Load configured gateway names so the backend/method pickers list them
      // (e.g. openrouter) alongside KNOWN_BACKENDS after the first paint.
      fetchConfiguredGateways();
    }

    const CLI_MODEL_CACHE_TTL_MS = 30000;
    let _cliModelsFetchedAt = 0;

    // BACKEND_MODEL_NOTICES holds, per backend id, the server's explanation
    // for why a static list is being served when the reason is one the owner
    // can act on — today: the backend rejected this account's credential or
    // entitlement (#6500). The option labels carry the short form; this map
    // carries the full sentence onto the dropdown's tooltip, so the answer is
    // one hover away from where the owner is already looking instead of only
    // in a server log.
    const BACKEND_MODEL_NOTICES = {};

    // applyModelNoticeTitle puts a backend's notice detail on a model <select>
    // as its tooltip, and clears any stale one when the backend recovers.
    function applyModelNoticeTitle(sel, backend) {
      if (!sel) return;
      const notice = BACKEND_MODEL_NOTICES[backend];
      if (notice && notice.detail) sel.title = notice.detail;
      else if (sel.title) sel.removeAttribute('title');
    }

    // setBackendModelNotice records a backend's notice and refreshes the
    // tooltip on every model dropdown already on screen for that backend —
    // the agent cards are rendered independently of this fetch, so a notice
    // that arrives after they paint still has to reach them.
    // formatStaleCatalogAge renders how long ago a replayed CLI catalog was
    // discovered ("40s ago", "12m ago", "3h ago") from the server's RFC3339
    // stale_at; '' when unparsable.
    function formatStaleCatalogAge(iso) {
      const t = Date.parse(iso);
      if (!Number.isFinite(t)) return '';
      const sec = Math.max(0, Math.floor((Date.now() - t) / 1000));
      if (sec < 60) return `${sec}s ago`;
      if (sec < 3600) return `${Math.floor(sec / 60)}m ago`;
      return `${Math.floor(sec / 3600)}h ago`;
    }
    function setBackendModelNotice(backend, notice) {
      if (notice) BACKEND_MODEL_NOTICES[backend] = notice;
      else delete BACKEND_MODEL_NOTICES[backend];
      (window._lastAgents || []).forEach(a => {
        if (!a || a.cli !== backend) return;
        document.querySelectorAll(
          `select[data-change-action="switchModel"][data-agent="${a.name}"]`
        ).forEach(sel => applyModelNoticeTitle(sel, backend));
      });
    }
    // fetchBackendsConfig hits the same /api/config/backends endpoint that
    // powers backend discovery server-side (PR #1843) and populates
    // BACKEND_MODELS for the CLI backends (copilot/claude/gemini/goose) with
    // the live-discovered model ids, exactly as fetchInferenceModels does
    // for vllm/llm-d/litellm. Until this resolves, modelsForBackend() serves
    // CLI_FALLBACK_MODELS so a dropdown is never empty on first paint.
    async function fetchBackendsConfig(force) {
      const now = Date.now();
      if (!force && _cliModelsFetchedAt && (now - _cliModelsFetchedAt) < CLI_MODEL_CACHE_TTL_MS) {
        return;
      }
      try {
        const resp = await fetch('/api/config/backends');
        const list = await resp.json();
        _cliModelsFetchedAt = now;
        CONFIG_BACKEND_OPTIONS = (list || []).map(b => b && b.id).filter(Boolean);
        if (document.getElementById('rotation-providers-table')) renderConfigTab('Features');
        (list || []).forEach(b => {
          if (!CLI_BACKENDS.includes(b.id)) return; // inference backends handled by fetchInferenceModels
          // b.notice: discovery did not merely fail, it was REJECTED — the
          // backend told hive this account cannot run inference (a Copilot
          // seat that is no longer licensed, #6500). Say that instead of
          // "(common alias, unverified)", which reads as a hive-side probe
          // hiccup and let an owner with dead agents conclude the model
          // configuration was fine. The list is still offered: blanking it
          // would leave nothing configurable, and a seat can come back.
          // b.discovery: discovery FAILED (or degraded — the list is live
          // but not the CLI's catalog) and the server says why. Render an
          // explicit error state, not a plausible-looking static list:
          // on the projectbluefin spoke a fallback catalog was indistinguishable
          // from a real one for two days while auto-heal downgraded every
          // agent off it (#7384). An entitlement notice (#6500) still wins
          // the label — it is the more specific diagnosis.
          // b.stale: the server is replaying the last CLI catalog it
          // discovered because the current probe failed transiently (#11865).
          // That IS the CLI's list, so it is not "degraded"; the label says
          // it is cached and the tooltip says why the refresh failed.
          const discoveryFailed = !!(b.discovery && b.discovery.ok === false);
          const staleAge = b.stale && b.stale_at ? formatStaleCatalogAge(b.stale_at) : '';
          const labelSuffix = (b.notice && b.notice.label) ? ` (${b.notice.label})`
            : (discoveryFailed && b.stale) ? ` (cached CLI catalog${staleAge ? ', ' + staleAge : ''} — refresh failing)`
            : discoveryFailed ? (b.degraded
                ? ' (⚠ model discovery degraded — not the CLI catalog)'
                : ' (⚠ model discovery failed — showing fallback list)')
            : (b.fallback ? ' (common alias, unverified)' : '');
          if (b.notice && b.notice.detail) {
            console.warn(`[models] ${b.id}: ${b.notice.detail}`);
          } else if (discoveryFailed) {
            console.warn(`[models] ${b.id}: model discovery ${b.stale ? 'serving cached catalog' : 'failed'} — ${b.discovery.error}`);
          }
          // The tooltip carries the diagnostic text itself — the exact
          // string ("Could not find a @github/copilot platform package…" vs
          // "Not authenticated" vs "sdk helper timed out after 20s") an
          // operator previously had to read off a server log to tell the
          // failures apart.
          setBackendModelNotice(b.id, b.notice || (discoveryFailed ? (b.stale ? {
            class: 'discovery',
            label: 'cached CLI catalog — refresh failing',
            detail: `ℹ this is the CLI catalog discovered ${staleAge || 'earlier'}; the latest refresh failed and the server is holding the last good list. ${b.discovery.error}`,
          } : {
            class: 'discovery',
            label: b.degraded ? 'model discovery degraded' : 'model discovery failed',
            detail: `⚠ model discovery ${b.degraded ? 'degraded' : 'failed'} — ${b.degraded ? 'this list is live but is not the catalog the CLI uses' : 'showing the static fallback list'}. ${b.discovery.error}`,
          }) : null));
          if (b.reasoning_efforts) MODEL_REASONING_EFFORTS[b.id] = b.reasoning_efforts;
          const models = (b.models || []).map(m => ({ value: m, label: (b.id === 'omp' ? m : m.split('/').pop()) + labelSuffix, suffix: labelSuffix }));
          if (!models.length) return; // keep prior (possibly fallback) list rather than blanking the dropdown
          BACKEND_MODELS[b.id] = models;
          // A degraded list is not authority either: absence from it proves
          // nothing about what the CLI can run, so auto-heal sits it out
          // exactly as it sits out a static fallback (#7384).
          // A stale replay is the CLI's catalog but not freshly confirmed;
          // auto-heal waits for a live probe too (#11865).
          reconcileModelsAfterDiscovery(b.id, models, !!b.fallback || !!b.degraded || !!b.stale);
        });
      } catch (e) {
        // Leave BACKEND_MODELS / fallbacks untouched — retried on next refresh hook.
      }
    }


    // reconcileModelsAfterDiscovery auto-heals selections that a key swap
    // (or endpoint change) stripped out: when a REAL discovery (not the
    // static fallback) returns a non-empty model set, any agent on this
    // backend whose selected model is GENUINELY no longer in the set is
    // switched to the first available model, via the same switchModel()
    // path the dropdown uses (so it persists + relaunches, not a cosmetic
    // change).
    //
    // Auto-heal policy: a model may switch automatically in exactly two
    // cases — (a) unpinned + governor auto-selection (server-side; pins
    // block that path there), or (b) the current model is genuinely gone
    // from discovery. Case (b) applies to every backend, CLI included, and
    // may heal even a PINNED model (a pinned-but-dead model would leave the
    // agent broken) — but only under the strict confirmation rules below.
    //
    // "Genuinely gone" is enforced three ways, because live CLI discovery
    // is nondeterministic: the Copilot /models endpoint was observed live
    // returning 31 vs 35 chat models for the SAME token across calls
    // seconds apart, with rollout-gated ids (e.g. claude-opus-4.6,
    // claude-fable-5) dropping out of individual samples. Treating one
    // sample as authoritative stampeded every agent — pinned included —
    // onto the first model of a bad sample, repeatedly (#1845 regression):
    //   1. availability uses the SAME tolerant matcher as the dropdown
    //      preselect (_modelIdMatches: exact, prefix-stripped tail, and
    //      dotted/hyphenated version normalization) — a model present in
    //      any id spelling is KEPT, never switched;
    //   2. absence must persist for MODEL_HEAL_CONFIRM_MS across repeated
    //      discovery refreshes (spanning multiple independent server-side
    //      probes) — a lone bad sample warns on the console and heals
    //      nothing; reappearance resets the clock;
    //   3. a heal fires at most once per (backend, agent, model) until that
    //      model is seen available again — repeated refresh ticks with an
    //      unchanged list can never re-fire a switch/restart loop.
    // Strictly: keep the current model when it is still present — no
    // re-select, no toast, no relaunch. Idempotent: repeated refreshes with
    // a compatible list never mutate a valid selection.

    // How long a model must be CONTINUOUSLY absent from successful
    // discoveries before auto-heal may act. Longer than two server-side
    // discovery cache TTLs (cliModelCacheTTL = 30s), so confirmation always
    // spans at least two independent upstream probes, not one cached bad
    // sample re-served within its TTL.
    const MODEL_HEAL_CONFIRM_MS = 65000;
    // First-seen-absent timestamps: `${backend}|${agent}` -> {model, since}.
    // Cleared the moment the model shows up as available again.
    const _modelAbsenceSince = {};
    // Fired-heal guard: `${backend}|${agent}|${model}` -> true. Ensures one
    // switch (one session restart) per genuine disappearance; cleared only
    // when the model is observed available again.
    const _modelHealFired = {};

    // Auto-heal HEAL TARGETS, per backend. Appearing in a discovery catalog is
    // NOT proof the backend's CLI can run a model, and auto-heal pins an agent
    // to its target for real work — so the target is drawn from a known-good
    // set rather than from `available[0]` (#5338).
    //
    // The claude catalog is the strict case that motivated this: it is
    // discovered from api.anthropic.com/v1/models, which enumerates what the
    // ACCOUNT may call over the HTTP API — a superset of what the bundled
    // `claude` CLI can drive. A model the CLI advertises but whose adapter
    // cannot build a request still lists cleanly, and because /v1/models is
    // newest-first, that model lands at available[0] — exactly the slot the
    // old code auto-assigned. On the reporting hive that pinned the only
    // write-capable agent to a model whose every call died in the CLI's
    // request builder, and re-applied it at each boot, defeating config-file
    // remediation.
    //
    // The bare aliases are the safe targets: `claude --model opus|sonnet|haiku`
    // resolves through the CLI's OWN current-model mapping, so the CLI can only
    // ever select something it can actually drive. They are also in
    // claudeAlwaysIncludeModels (cli_models.go), so they are present in every
    // served claude list — live-discovered or static fallback.
    //
    // Backends absent from this map keep the previous first-available
    // behavior: this narrows the blast radius to the backend the failure was
    // reported against rather than re-tuning heal targets fleet-wide.
    const MODEL_HEAL_PREFERRED_TARGETS = {
      claude: ['sonnet', 'opus', 'haiku'],
    };

    // healTargetFor picks the model auto-heal should switch an agent TO.
    // Returns '' when no vetted target is available, which SUPPRESSES the heal
    // — leaving a model that is merely absent from one catalog beats pinning
    // the agent to one its CLI cannot run.
    function healTargetFor(backend, available) {
      const preferred = MODEL_HEAL_PREFERRED_TARGETS[backend];
      if (!preferred) return available[0] || '';
      for (const want of preferred) {
        const hit = available.find(v => _modelIdMatches(v, want));
        if (hit) return hit;
      }
      return '';
    }

    function reconcileModelsAfterDiscovery(backend, models, fallback) {
      // Guard: only act on a genuinely successful discovery. Never switch
      // based on unverified static aliases or an empty list.
      if (fallback || !models || !models.length) return;
      const available = models.map(m => m.value);
      const first = healTargetFor(backend, available);
      const isAvailable = (m) => available.some(v => _modelIdMatches(v, m));
      const now = Date.now();

      // _confirmAbsent implements rules 2+3 above for one keyed selection.
      // Returns true only when the model has been absent long enough AND
      // this disappearance has not already been healed.
      const _confirmAbsent = (key, model) => {
        const firedKey = `${key}|${model}`;
        const rec = _modelAbsenceSince[key];
        if (!rec || rec.model !== model) {
          // First observation of this absence — start the clock, warn, keep.
          _modelAbsenceSince[key] = { model, since: now };
          console.warn(`model ${model} missing from ${backend} discovery (${available.length} models) — keeping selection; auto-heal only if still absent after ${Math.round(MODEL_HEAL_CONFIRM_MS / 1000)}s`);
          return false;
        }
        if (now - rec.since < MODEL_HEAL_CONFIRM_MS) return false;
        if (_modelHealFired[firedKey]) return false; // already healed this disappearance
        _modelHealFired[firedKey] = true;
        delete _modelAbsenceSince[key];
        return true;
      };
      // _markAvailable clears absence/fired state so a future GENUINE
      // disappearance of the same model is eligible to heal again.
      const _markAvailable = (key, model) => {
        const rec = _modelAbsenceSince[key];
        if (rec && rec.model === model) delete _modelAbsenceSince[key];
        delete _modelHealFired[`${key}|${model}`];
      };

      // 1) Agents whose effective method is this backend.
      (window._lastAgents || []).forEach(a => {
        if (a.cli !== backend) return;
        if (!a.model) return;                 // no explicit model — nothing to heal
        if (isAutoModelSentinel(backend, a.model)) return; // auto-select value — never a concrete id, never healed
        const key = `${backend}|${a.name}`;
        if (isAvailable(a.model)) { _markAvailable(key, a.model); return; } // still valid — leave untouched (idempotent)
        // No vetted heal target for this backend — keep the current model.
        // Checked BEFORE _confirmAbsent so the absence clock and the
        // once-per-disappearance guard are not consumed by a heal that cannot
        // fire; a target appearing later still gets its chance (#5338).
        if (!first) {
          console.warn(`model ${a.model} missing from ${backend} discovery but no vetted heal target is available — keeping selection`);
          return;
        }
        if (!_confirmAbsent(key, a.model)) return; // unconfirmed or already healed — no switch
        const label = a.displayName || a.name;
        const pinNote = (a.pinnedModel || a.pinnedBoth) ? ' (was pinned — model gone from provider)' : '';
        showToast(`${label}: model ${a.model.split('/').pop()} no longer available on ${backend} — switched to ${first.split('/').pop()}${pinNote}`, 'info');
        switchModel(a.name, first);           // persists + relaunches via /api/model
      });

      // 2) LiteLLM Default Model — only when the config dialog holds governor
      // data (that is where the saved default is available to the UI). If the
      // saved default is gone from the new set, persist the first available.
      // Same confirmation rules as agent heals — a lone bad sample or a
      // repeated refresh tick never rewrites the saved default.
      if (backend === 'litellm' && _configState.type === 'governor') {
        const saved = (_configState.data.litellm || {}).defaultModel;
        const defKey = `${backend}|__defaultModel__`;
        if (saved && isAvailable(saved)) {
          _markAvailable(defKey, saved);
        } else if (saved && first && _confirmAbsent(defKey, saved)) {
          _configState.data.litellm.defaultModel = first;
          fetch('/api/config/governor/litellm', {
            method: 'PUT',
            headers: _inceptionAuthHeaders(),
            body: JSON.stringify({ defaultModel: first })
          }).then(() => {
            showToast(`Default LiteLLM model ${saved.split('/').pop()} unavailable on new key — set to ${first.split('/').pop()}`, 'info');
            refreshLitellmDefaultModelField();
          }).catch(() => {});
        }
      }
    }

    const _lastKnownModels = {};
    const _dismissedModelBanners = new Set();

    // Called from render() on every SSE push — updates inference model
    // dropdowns instantly when vLLM/llm-d endpoints come online or change.
    function updateInferenceModelsFromSSE(inferenceBackends) {
      if (!inferenceBackends) return;
      for (const b of inferenceBackends) {
        /* models_fallback means the list is NOT an authoritative census:
           /v1/models discovery FAILED and the server substituted static
           aliases (or the HIVE_*_MODELS env list), or only SOME of the
           backend's endpoints answered. None of that is evidence a model
           was added or removed — diffing against such a list flooded the
           dashboard with ~25 sticky "Model removed" toasts when a litellm
           key lost /v1/models access (#4426), and again when a multi-gateway
           sweep came back partial (#4438). Keep the last REAL list as the
           diff baseline so recovery diffs stay accurate, and skip the toast
           machinery entirely. */
        if (b.models_fallback) {
          const fallbackModels = (b.models || []).map(m => ({ value: m, label: m.split('/').pop() }));
          INFERENCE_MODELS[b.id] = fallbackModels;
          _inferenceModelCache[b.id] = { models: fallbackModels, ts: Date.now() };
          if (b.id === 'litellm') refreshLitellmDefaultModelField();
          continue;
        }
        const currentIds = (b.models || []).sort();
        const prevIds = (_lastKnownModels[b.id] || []).sort();

        /* An EMPTY live list is a discovery hole the server did not flag,
           never a backend that genuinely offers nothing: diffing it would
           declare every known model removed at once — the worst-case toast
           storm. Refuse the diff, and keep the last real baseline AND its
           dropdown rather than blanking the model picker (#4438). */
        if (prevIds.length > 0 && currentIds.length === 0) continue;

        if (prevIds.length > 0 && JSON.stringify(currentIds) !== JSON.stringify(prevIds)) {
          const added = currentIds.filter(m => !prevIds.includes(m));
          const removed = prevIds.filter(m => !currentIds.includes(m));

          /* Model-change toasts name the FULL id, provider prefix included.
             Stripping it (m.split('/').pop()) rendered "openai/claude-
             sonnet-4-6" as bare "claude-sonnet-4-6" — spelled identically to
             the id an agent is configured with, so an operator could not tell
             a prefixed variant's departure from their own model vanishing,
             nor find the named model anywhere in the gateway's list (#4438). */
          added.forEach(m => {
            const key = `added:${b.id}:${m}`;
            if (_dismissedModelBanners.has(key)) return;
            showToast(`New model available on ${b.id}: ${m}`, 'success');
            _dismissedModelBanners.add(key);
          });

          removed.forEach(m => {
            /* Dedupe like additions, and auto-dismiss after 20s: a removed
               model is worth a glance, not a permanent sticky requiring a
               click per model — bulk removals otherwise wallpaper the UI. */
            const key = `removed:${b.id}:${m}`;
            if (_dismissedModelBanners.has(key)) return;
            _dismissedModelBanners.add(key);
            showToast(`Model removed from ${b.id}: ${m}`, 'error', false, TOAST_MODEL_CHANGE_MS);
            if (window._lastAgents) {
              (window._lastAgents || []).filter(a => a.cli === b.id && a.model === m).forEach(a => {
                showToast(`⚠ ${a.displayName || a.name} uses removed model ${m} — update its model selection`, 'error', false, TOAST_MODEL_CHANGE_MS);
              });
            }
          });
        }

        _lastKnownModels[b.id] = currentIds;
        const models = currentIds.map(m => ({ value: m, label: m.split('/').pop() }));
        INFERENCE_MODELS[b.id] = models;
        _inferenceModelCache[b.id] = { models, ts: Date.now() };
        // If the Governor→LiteLLM tab is open when a live "models pulled"
        // push arrives, re-render the Default Model dropdown against the
        // same freshly-discovered data (no separate fetch).
        if (b.id === 'litellm') refreshLitellmDefaultModelField();
      }
    }

    // _normalizeModel collapses the "." vs "-" separator drift between the
    // catalog and CLI spellings of the SAME model id (claude-fable.5 vs
    // claude-fable-5, claude-opus-4.6 vs claude-opus-4-6, #4262). Applied to
    // BOTH sides of every comparison, so mapping all dots to dashes is a pure
    // equivalence key — never a display or launch value.
    function _normalizeModel(m) { return (m || '').replace(/\./g, '-'); }
    function _modelsEqual(a, b) { return _normalizeModel(a) === _normalizeModel(b); }

    function _modelTier(model) {
      const m = (model || '').toLowerCase();
      // Claude model families
      if (m.includes('haiku')) return 'haiku';
      if (m.includes('opus')) return 'opus';
      if (m.includes('sonnet')) return 'sonnet';
      // Non-Claude models
      if (m.startsWith('gpt-')) return 'gpt';
      if (m.startsWith('gemini-')) return 'gemini';
      if (m.startsWith('deepseek-') || m.startsWith('deepseek/')) return 'deepseek';
      // Neutral fallback for unrecognized models
      return 'unknown';
    }

    function cliChip(cli, pinned, agent) {
      if (!cli || cli === '?') return '?';
      const tier = FREE_BACKENDS.includes(cli) ? 'free' : 'paid';
      const pin = pinned ? ' \u{1F4CC}' : '';
      const pinTitle = pinned ? ' (pinned — click to unpin)' : '';
      const click = pinned && agent ? ` data-action="togglePin" data-agent="${esc(agent)}" data-pin-type="cli" data-unpin="true" style="cursor:pointer"` : '';
      return `<span class="model-chip ${tier}" title="cli: ${cli}${pinTitle}"${click}>${cli}${pin}</span>`;
    }

    function backendChip(backend) {
      if (!backend || backend === 'unknown') return '';
      const tier = FREE_BACKENDS.includes(backend) ? 'free' : 'paid';
      return ` <span class="model-chip ${tier}" title="billing: ${backend}">${backend}</span>`;
    }

    function modelChip(model, reason, pinned, agent) {
      if (!model || model === '?' || model === 'unknown') return '?';
      const normalized = model.toLowerCase().replace(/\s+/g, '-');
      const shortModel = normalized.replace(/^claude-/, '').replace(/-(\d[\d-]*\d)$/, (_, v) => '-' + v.replace(/-/g, '.'));
      const tier = _modelTier(normalized);
      const chipCls = tier;
      const isBudgetOverride = reason && (reason.includes('budget_downgrade') || reason.includes('budget_critical'));
      const overrideIcon = isBudgetOverride ? ' ⬇' : '';
      const overrideTitle = isBudgetOverride ? ` (${reason})` : '';
      const pin = pinned ? ' \u{1F4CC}' : '';
      const pinTitle = pinned ? ' (pinned — click to unpin)' : '';
      const click = pinned && agent ? ` data-action="togglePin" data-agent="${esc(agent)}" data-pin-type="model" data-unpin="true" style="cursor:pointer"` : '';
      return `<span class="model-chip ${chipCls}" title="${esc(model)}${esc(overrideTitle)}${pinTitle}"${click}>${esc(shortModel)}${overrideIcon}${pin}</span>`;
    }

    function buildCadenceAdvisor(byAgent) {
      const agents = window._lastAgents || [];
      if (!agents.length || !byAgent) return '';

      const MINUTES_PER_HOUR = 60;
      const HOURS_PER_WEEK = 168;
      const sortedNames = agents.map(a => a.name).sort((a, b) => _getAgentSortOrder(a) - _getAgentSortOrder(b));
      const rows = [];
      let totalWeeklyBurn = 0;

      for (const name of sortedNames) {
        const agentData = agents.find(a => a.name === name);
        const tokenData = byAgent[name];
        if (!agentData || !tokenData) continue;

        const avg = tokenData.avgPerSession || 0;
        const cadenceMin = parseCadenceMinutes(agentData.cadence);
        const isOff = agentHeldByMode(agentData);
        const isOnDemand = agentData.onDemand === true;
        // paused = the live paused flag (governor-mode pauses keep the
        // configured cadence string, so the string alone is not enough)
        // OR a non-numeric cadence ('paused'/'off'/'on demand').
        const isPaused = (agentData.paused === true && !isOff) || (!cadenceMin && !isOff);
        const passesPerHour = (isPaused || isOff) ? 0 : MINUTES_PER_HOUR / cadenceMin;
        const weeklyBurn = avg * passesPerHour * HOURS_PER_WEEK;
        totalWeeklyBurn += weeklyBurn;

        rows.push({ name, avg, cadenceMin, isPaused, isOff, isOnDemand, passesPerHour, weeklyBurn, cli: agentData.cli });
      }

      if (totalWeeklyBurn === 0) return '';

      const tips = [];
      const sorted = [...rows].filter(r => r.weeklyBurn > 0).sort((a, b) => b.weeklyBurn - a.weeklyBurn);

      if (sorted.length > 0) {
        const top = sorted[0];
        const topPct = Math.round((top.weeklyBurn / totalWeeklyBurn) * 100);
        if (topPct > 60) {
          const DOUBLE_CADENCE = 2;
          tips.push(`<b>${top.name}</b> uses ${topPct}% of weekly tokens. Doubling interval to ${top.cadenceMin * DOUBLE_CADENCE}m would halve its burn.`);
        }
      }

      const architect = rows.find(r => r.name === 'architect');
      if (architect) {
        if (architect.isPaused || architect.isOff) {
          tips.push(`<b>architect</b> is ${architect.isPaused ? 'paused' : 'off'} — no architect work happening. Consider enabling at 1h cadence.`);
        } else if (architect.weeklyBurn > 0) {
          const architectPct = Math.round((architect.weeklyBurn / totalWeeklyBurn) * 100);
          if (architectPct < 15 && architect.cadenceMin > 15) {
            const HALVE_CADENCE = 2;
            tips.push(`<b>architect</b> is only ${architectPct}% of burn. Could increase to ${Math.max(15, Math.floor(architect.cadenceMin / HALVE_CADENCE))}m for more architect work.`);
          }
        }
      }

      for (const r of rows) {
        if (r.cli === 'copilot' && r.avg === 0 && !r.isPaused && !r.isOff) {
          tips.push(`<b>${r.name}</b> runs on copilot (unlimited tokens, no metering). Token burn unknown — consider switching to claude CLI for visibility.`);
        }
      }

      // Model-aware tips from governor assignments
      const OPUS_COST = 15;
      const SONNET_COST = 3;
      for (const r of rows) {
        const agentData = agents.find(a => a.name === r.name);
        if (!agentData) continue;
        const gb = agentData.govBackend;
        const gm = agentData.govModel || '';
        if (gb === 'copilot' || gb === 'goose') {
          if (r.weeklyBurn > 0) {
            tips.push(`<b>${r.name}</b> assigned to ${gb} (free) — ${fmtTokens(r.weeklyBurn)}/wk savings vs metered CLI.`);
          }
        } else if (gm.includes('opus') && r.weeklyBurn > 0) {
          const sonnetBurn = Math.round(r.weeklyBurn * SONNET_COST / OPUS_COST);
          tips.push(`<b>${r.name}</b> on Opus (${OPUS_COST}x) — switching to Sonnet would save ~${fmtTokens(r.weeklyBurn - sonnetBurn)}/wk in cost-adjusted tokens.`);
        }
      }

      const budget = window._lastBudget || {};
      if (budget.PROJECTED_PCT) {
        const SAFE_THRESHOLD = 50;
        const WARN_THRESHOLD = 85;
        if (budget.PROJECTED_PCT > WARN_THRESHOLD) {
          tips.push(`Budget projected at <b>${budget.PROJECTED_PCT}%</b> — governor is auto-downgrading models to stay within budget.`);
        } else if (budget.PROJECTED_PCT < SAFE_THRESHOLD) {
          const inactive = rows.filter(r => r.isPaused || r.isOff);
          if (inactive.length > 0) {
            tips.push(`Budget at ${budget.PROJECTED_PCT}% — headroom to enable inactive agents: ${inactive.map(r => `<b>${r.name}</b>`).join(', ')}.`);
          }
        }
      }

      const barRows = rows.map(r => {
        if (r.isPaused || r.isOff) {
          const note = r.isOff ? 'off in current mode' : r.isOnDemand ? 'on demand' : 'paused';
          return `<div class="advisor-bar-row advisor-row-inactive">
            <span class="advisor-agent">${r.name}</span>
            <span class="advisor-inactive-note">${note} — projects 0</span>
            <span class="advisor-pct">—</span>
            <span class="advisor-burn">0/wk</span>
          </div>`;
        }
        const pct = totalWeeklyBurn > 0 ? Math.round((r.weeklyBurn / totalWeeklyBurn) * 100) : 0;
        const barColor = _getAgentColor(r.name);
        return `<div class="advisor-bar-row">
          <span class="advisor-agent">${r.name}</span>
          <div class="advisor-bar-track"><div class="advisor-bar-fill" style="width:${Math.max(pct, 2)}%;background:${barColor}"></div></div>
          <span class="advisor-pct">${pct + '%'}</span>
          <span class="advisor-burn">${fmtTokens(r.weeklyBurn)}/wk</span>
        </div>`;
      }).join('');

      const tipsHtml = tips.length > 0 ? `<div class="advisor-tips">${tips.map(t => `<div class="advisor-tip">💡 ${t}</div>`).join('')}</div>` : '';

      // On-demand/paused/off agents project 0 by cadence — correctly — but
      // they still burn real tokens when kicked manually. Surface that
      // actual window burn so the projection's blind spot is visible.
      const excludedBurn = rows
        .filter(r => r.isPaused || r.isOff)
        .reduce((sum, r) => {
          const d = byAgent[r.name] || {};
          return sum + (d.input || 0) + (d.output || 0) + (d.cacheRead || 0);
        }, 0);
      const DEFAULT_LOOKBACK_HOURS = 24;
      const lookbackHours = (window._lastStatus?.tokens?.lookbackHours) || DEFAULT_LOOKBACK_HOURS;
      const excludedHtml = excludedBurn > 0
        ? `<div class="advisor-excluded-burn">on-demand/manual burn last ${lookbackHours}h: <b>${fmtTokens(excludedBurn)}</b> tokens (excluded from projection)</div>`
        : '';

      return `<div class="advisor-section">
        <div class="advisor-title">Cadence Advisor <span class="var-meta">weekly projection at current cadence</span></div>
        <div class="advisor-total">Projected: <b>${fmtTokens(totalWeeklyBurn)}</b> tokens/week</div>
        <div class="advisor-bars">${barRows}</div>
        ${excludedHtml}
        ${tipsHtml}
      </div>`;
    }

// Intensity gauge: compares recent vs trailing token rate
// Returns SVG string for a half-circle speedometer
function computeHourlyBurnRates() {
  const MIN_POINTS = 6;
  if (historyData.length < MIN_POINTS) return null;

  // Collect points where tokenTotal actually changed (skip stale repeats)
  const changePoints = [];
  let lastVal = -1;
  for (const s of historyData) {
    const v = s.tokenTotal || 0;
    if (v !== lastVal) {
      changePoints.push({ t: s.t, v });
      lastVal = v;
    }
  }
  if (changePoints.length < 3) return null;

  // Compute rate between consecutive change points
  const rawRates = [];
  for (let i = 1; i < changePoints.length; i++) {
    const dt = (changePoints[i].t - changePoints[i - 1].t) / 1000;
    if (dt <= 0) continue;
    const delta = changePoints[i].v - changePoints[i - 1].v;
    if (delta <= 0) continue;
    rawRates.push({ t: changePoints[i].t, rate: (delta / dt) * 3600 });
  }
  if (rawRates.length < 3) return null;

  // Cap outliers at p95 to remove collector-reset spikes
  const sorted = rawRates.map(r => r.rate).sort((a, b) => a - b);
  const P95_IDX = Math.floor(sorted.length * 0.95);
  const cap = sorted[Math.min(P95_IDX, sorted.length - 1)] * 1.5;
  for (const r of rawRates) { if (r.rate > cap) r.rate = cap; }

  // Bucket into 5-minute windows
  const BUCKET_MS = 300000;
  const rates = [];
  let i = 0;
  while (i < rawRates.length) {
    const bucketEnd = rawRates[i].t + BUCKET_MS;
    let sum = 0, count = 0;
    const bt = rawRates[i].t;
    while (i < rawRates.length && rawRates[i].t < bucketEnd) {
      sum += rawRates[i].rate;
      count++;
      i++;
    }
    if (count > 0) rates.push({ t: bt, rate: sum / count });
  }
  return rates.length >= 3 ? rates : null;
}

function intensityGauge() {
  const rates = computeHourlyBurnRates();
  if (!rates) return '';

  const rateVals = rates.map(r => r.rate);
  const now = rateVals[rateVals.length - 1];
  const peak = Math.max(...rateVals);
  const low = Math.min(...rateVals);
  const avg = rateVals.reduce((a, b) => a + b, 0) / rateVals.length;

  let color;
  if (peak === 0) color = 'var(--muted)';
  else if (now / peak > 0.8) color = 'var(--red)';
  else if (now / peak > 0.5) color = 'var(--yellow)';
  else if (now / peak > 0.2) color = 'var(--green)';
  else color = 'var(--blue)';

  let label;
  if (peak === 0) label = 'idle';
  else if (now / peak > 0.8) label = 'surging';
  else if (now / peak > 0.5) label = 'ramping';
  else if (now / peak > 0.2) label = 'steady';
  else label = 'cooling';

  const W = 240, H = 50, PAD = 2;
  const range = peak - low || 1;
  const pts = rateVals.map((v, i) => {
    const x = PAD + (i / (rateVals.length - 1)) * (W - PAD * 2);
    const y = PAD + (1 - (v - low) / range) * (H - PAD * 2);
    return [x.toFixed(1), y.toFixed(1)];
  });

  const yAvg = (PAD + (1 - (avg - low) / range) * (H - PAD * 2)).toFixed(1);
  const yPeak = (PAD + (1 - (peak - low) / range) * (H - PAD * 2)).toFixed(1);
  const yLow = (PAD + (1 - (low - low) / range) * (H - PAD * 2)).toFixed(1);
  const lastPt = pts[pts.length - 1];

  const polyline = pts.map(p => p.join(',')).join(' ');

  const tStart = formatDashboardTime(rates[0].t, {hour:'numeric',minute:'2-digit',hour12:true,timeZoneName:'short'}).toLowerCase();
  const tEnd = formatDashboardTime(rates[rates.length - 1].t, {hour:'numeric',minute:'2-digit',hour12:true,timeZoneName:'short'}).toLowerCase();

  const dotLeftPct = (parseFloat(lastPt[0]) / W * 100).toFixed(1);
  const dotTopPct = (parseFloat(lastPt[1]) / H * 100).toFixed(1);

  return `<div class="burn-chart-wrap">
    <div class="burn-chart-title">burn rate (tok/hr)</div>
    <div style="position:relative">
      <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" style="width:100%;height:${H}px;display:block">
        <line x1="${PAD}" y1="${yPeak}" x2="${W - PAD}" y2="${yPeak}" stroke="var(--red)" stroke-width="0.5" stroke-dasharray="3,3" opacity="0.5" vector-effect="non-scaling-stroke"/>
        <line x1="${PAD}" y1="${yAvg}" x2="${W - PAD}" y2="${yAvg}" stroke="var(--yellow)" stroke-width="0.5" stroke-dasharray="3,3" opacity="0.5" vector-effect="non-scaling-stroke"/>
        <line x1="${PAD}" y1="${yLow}" x2="${W - PAD}" y2="${yLow}" stroke="var(--blue)" stroke-width="0.5" stroke-dasharray="3,3" opacity="0.5" vector-effect="non-scaling-stroke"/>
        <polyline points="${polyline}" fill="none" stroke="${color}" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" vector-effect="non-scaling-stroke"/>
      </svg>
      <div style="position:absolute;left:${dotLeftPct}%;top:${dotTopPct}%;width:6px;height:6px;border-radius:50%;background:${color};border:1px solid var(--bg);transform:translate(-50%,-50%)"></div>
    </div>
    <div style="display:flex;justify-content:space-between;font-size:var(--fs-2xs);color:var(--muted);margin-top:1px;font-family:var(--font-mono)">
      <span>${tStart}</span><span>${tEnd}</span>
    </div>
    <div class="burn-context">
      <div class="bc-stat"><span class="bc-val text-info">${fmtTokens(low)}</span><span class="bc-label">low/hr</span></div>
      <div class="bc-stat"><span class="bc-val text-warn">${fmtTokens(avg)}</span><span class="bc-label">avg/hr</span></div>
      <div class="bc-stat"><span class="bc-val text-danger">${fmtTokens(peak)}</span><span class="bc-label">peak/hr</span></div>
      <div class="bc-stat"><span class="bc-val" style="color:${color}">${fmtTokens(now)}</span><span class="bc-label">current/hr</span></div>
    </div>
    <div style="color:${color};font-size:11px;font-family:var(--font-mono);margin-top:var(--sp-1)">${label}</div>
  </div>`;
}

function burnAreaChart() {
  const rates = computeHourlyBurnRates();
  if (!rates || rates.length < 4) return '';

  const rateVals = rates.map(r => r.rate);
  const BUCKET_COUNT = 8;
  const bucketSize = Math.max(1, Math.floor(rateVals.length / BUCKET_COUNT));
  const buckets = [];
  for (let i = 0; i < rateVals.length; i += bucketSize) {
    const slice = rateVals.slice(i, i + bucketSize).sort((a, b) => a - b);
    if (slice.length === 0) continue;
    const p25Idx = Math.floor(slice.length * 0.25);
    const p75Idx = Math.min(Math.floor(slice.length * 0.75), slice.length - 1);
    const median = slice[Math.floor(slice.length * 0.5)];
    buckets.push({ p25: slice[p25Idx], p75: slice[p75Idx], median, t: rates[Math.min(i, rates.length - 1)].t });
  }

  if (buckets.length < 2) return '';

  const allVals = rateVals;
  const peak = Math.max(...allVals);
  const low = Math.min(...allVals);
  const range = peak - low || 1;

  const W = 400, H = 60, PAD = 2;
  const xStep = (W - PAD * 2) / (buckets.length - 1);

  const bandTop = buckets.map((b, i) => {
    const x = PAD + i * xStep;
    const y = PAD + (1 - (b.p75 - low) / range) * (H - PAD * 2);
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  });
  const bandBot = buckets.map((b, i) => {
    const x = PAD + i * xStep;
    const y = PAD + (1 - (b.p25 - low) / range) * (H - PAD * 2);
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  }).reverse();

  const medianPts = buckets.map((b, i) => {
    const x = PAD + i * xStep;
    const y = PAD + (1 - (b.median - low) / range) * (H - PAD * 2);
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  });

  const nowPts = rateVals.map((v, i) => {
    const x = PAD + (i / (rateVals.length - 1)) * (W - PAD * 2);
    const y = PAD + (1 - (v - low) / range) * (H - PAD * 2);
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  });
  const lastNow = nowPts[nowPts.length - 1];
  const nowVal = rateVals[rateVals.length - 1];
  const aboveBand = nowVal > buckets[buckets.length - 1].p75;
  const belowBand = nowVal < buckets[buckets.length - 1].p25;
  const dotColor = aboveBand ? 'var(--red)' : belowBand ? 'var(--blue)' : 'var(--green)';

  const areaDotLeftPct = (parseFloat(lastNow.split(',')[0]) / W * 100).toFixed(1);
  const areaDotTopPct = (parseFloat(lastNow.split(',')[1]) / H * 100).toFixed(1);

  return `<div class="burn-area-wrap">
    <div class="burn-area-title" style="text-align:left">burn rate distribution · <span style="color:rgba(57,210,192,0.6)">p25–p75 band</span> · <span style="color:var(--cyan)">actual</span></div>
    <div style="position:relative">
      <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" style="width:100%;height:${H}px;display:block">
        <polygon points="${bandTop.join(' ')} ${bandBot.join(' ')}" fill="rgba(57,210,192,0.12)" stroke="none"/>
        <polyline points="${medianPts.join(' ')}" fill="none" stroke="rgba(57,210,192,0.3)" stroke-width="1" stroke-dasharray="2,2" vector-effect="non-scaling-stroke"/>
        <polyline points="${nowPts.join(' ')}" fill="none" stroke="var(--cyan)" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" vector-effect="non-scaling-stroke"/>
      </svg>
      <div style="position:absolute;left:${areaDotLeftPct}%;top:${areaDotTopPct}%;width:6px;height:6px;border-radius:50%;background:${dotColor};border:1px solid var(--bg);transform:translate(-50%,-50%)"></div>
    </div>
  </div>`;
}


    function renderTokens(tokens) {
      const el = document.getElementById('token-panel');

      if (!tokens || !tokens.totals || tokens.totals.sessions === 0) {
        el.innerHTML = sectionCardShell({ id: 'token-panel', title: '🪙 Tokens', gripLabel: 'Tokens', panelClass: 'token-panel', headerClass: 'token-title', badgeHtml: '<span class="lookback">24h window · 0 sessions</span>', summaryHtml: '0 tok/hr · 0 sessions (24h)', summaryTitle: 'Current burn and session count' }, '<div class="text-muted">No token usage yet.</div>');
        applySectionCollapse('token-panel');
        return;
      }

      const sessionsEl = el.querySelector('.token-sessions');
      const storedSessionsOpen = localStorage.getItem('hive-sessions-open');
      /* Priority: localStorage (explicitly persisted by toggle handler) > DOM element > default open */
      const sessionsOpen = storedSessionsOpen !== null ? storedSessionsOpen === '1' : (sessionsEl ? sessionsEl.open : true);

      const t = tokens.totals;
      const totalTokens = t.input + t.output + t.cacheRead;
      const models = Object.entries(tokens.byModel || {}).sort((a, b) =>
        (b[1].input + b[1].output + b[1].cacheRead) - (a[1].input + a[1].output + a[1].cacheRead)
      );
      const sessions = tokens.sessions || [];

      const modelChips = models.map(([name, m]) => {
        const mTotal = m.input + m.output + m.cacheRead;
        const mSpark = sparkSvg(historyData.map(s => s.tokenModels?.[name] ?? 0), 'var(--purple)', getHistoryTimes(), `token-model:${name}`);
        return `<div class="token-model-chip">
          <span class="mname">${esc(name)}</span>
          <span class="mcount">${fmtTokens(mTotal)} tokens · ${m.messages} msgs</span>
          ${mSpark}
        </div>`;
      }).join('');

      const knownAgents = new Set((window._lastStatus?.agents || []).map(a => a.name));
      const agentSessions = sessions.filter(s => knownAgents.has(s.agent));
      const grouped = {};
      for (const s of agentSessions) {
        if (!grouped[s.agent]) grouped[s.agent] = [];
        grouped[s.agent].push(s);
      }
      const sortedAgentNames = (window._lastStatus?.agents || [])
        .sort((a, b) => (a.sortOrder || 100) - (b.sortOrder || 100))
        .map(a => a.name);
      const sortedGroups = sortedAgentNames.filter(a => grouped[a]).map(a => [a, grouped[a]]);
      const sessionRows = sortedGroups.map(([agent, grp]) => {
        const agentCls = `a-${agent}`;
        const rows = grp.map(s => {
          const age = s.lastActive ? formatDashboardTime(s.lastActive, {hour:'numeric',minute:'2-digit',hour12:true,timeZoneName:'short'}) : '';
          const act = s.activity || [];
          const MINI_W = 60, MINI_H = 14;
          let miniSpark = '';
          if (act.length >= 2) {
            const mx = Math.max(...act, 1);
            const mn = Math.min(...act);
            const clr = _getAgentColor(s.agent);
            const title = `session activity per interval — range ${fmtSparkVal(mn)}–${fmtSparkVal(mx)}, oldest → newest`;
            miniSpark = renderSparkline(null, act, { width: MINI_W, height: MINI_H, color: clr, min: mn, max: mx, strokeWidth: 1.2, dot: false, wrapperClass: 'sparkline--tight', lineAttrs: ' opacity="0.7"', title, ariaLabel: title, label: 'session activity', id: `session:${s.id}:activity` });
          }
          return `<div class="token-session-row" style="padding-left:var(--sp-5)">
            <span class="sid">${esc(s.id)}</span>
            <span class="smodel">${esc(s.model)}</span>
            <span class="stokens" ${s.estimated ? 'title="estimated from avg tokens/msg"' : ''}>${s.total > 0 ? (s.estimated ? '~' : '') + fmtTokens(s.total) : '—'}</span>
            <span class="smsgs">${s.messages} msgs</span>${miniSpark}
            <span class="sproj">${age}</span>
          </div>`;
        }).join('');
        const totalMsgs = grp.reduce((a, s) => a + s.messages, 0);
        const totalTokens = grp.reduce((a, s) => a + s.total, 0);
        const hasEstimates = grp.some(s => s.estimated && s.total > 0);
        return `<div class="mb-3">
          <div style="display:flex;align-items:center;gap:var(--sp-4);margin-bottom:var(--sp-1)">
            <span class="sagent ${agentCls}" style="font-weight:700">${agent}</span>
            <span style="color:var(--muted);font-size:0.65rem">${grp.length} session${grp.length > 1 ? 's' : ''} · ${totalMsgs} msgs${totalTokens > 0 ? ' · ' + (hasEstimates ? '~' : '') + fmtTokens(totalTokens) : ''}</span>
          </div>
          ${rows}
        </div>`;
      }).join('');

      const advisorHtml = buildCadenceAdvisor(tokens.byAgent || {});

      const totalSpark = sparkline('tokenTotal');
      const ba = tokens.byAgent || {};
      const agentNames = Object.keys(ba).sort((a, b) => {
        const aOrder = _getAgentSortOrder(a);
        const bOrder = _getAgentSortOrder(b);
        return aOrder - bOrder;
      });
      const agentSparkRows = agentNames.map(name => {
        const aData = ba[name];
        const aTotal = aData ? (aData.input || 0) + (aData.output || 0) + (aData.cacheRead || 0) : 0;
        const clr = _getAgentColor(name);
        const aSpark = sparkSvg(historyData.map(s => s.tokens?.[name] ?? 0), clr, getHistoryTimes(), `token-agent:${name}`);
        return `<div class="token-stat"><div class="spark-row"><span class="tval">${fmtTokens(aTotal)}</span>${aSpark}</div><div class="tlabel">${name}</div></div>`;
      }).join('');

      const tokenRates = computeHourlyBurnRates();
      const currentTokHr = tokenRates && tokenRates.length ? tokenRates[tokenRates.length - 1].rate : Math.round(totalTokens / Math.max(1, tokens.lookbackHours || 24));
      const tokenSummary = `${fmtTokens(currentTokHr)} tok/hr · ${t.sessions} sessions (${tokens.lookbackHours || 24}h)`;
      const tokenBody = `        <div class="sec-headline" data-collapsed-keep style="display:flex;gap:var(--sp-6);margin-bottom:var(--sp-8);padding-bottom:var(--sp-6);border-bottom:1px solid var(--border)">
          <div style="flex:1;min-width:0">${intensityGauge()}</div>
          <div style="flex:1;min-width:0">${burnAreaChart()}</div>
        </div>
        <div class="token-grid">
          <div class="token-stat"><div class="spark-row"><span class="tval">${fmtTokens(totalTokens)}</span>${totalSpark}</div><div class="tlabel">total tokens</div></div>
          <div class="token-stat"><div class="spark-row"><span class="tval">${fmtTokens(t.input)}</span>${sparkline('tokenInput')}</div><div class="tlabel">input</div></div>
          <div class="token-stat"><div class="spark-row"><span class="tval">${fmtTokens(t.output)}</span>${sparkline('tokenOutput')}</div><div class="tlabel">output</div></div>
          <div class="token-stat"><div class="spark-row"><span class="tval">${fmtTokens(t.cacheRead)}</span>${sparkline('tokenCacheRead')}</div><div class="tlabel">cache read</div></div>
          <div class="token-stat"><div class="spark-row"><span class="tval">${fmtTokens(t.cacheCreate)}</span>${sparkline('tokenCacheCreate')}</div><div class="tlabel">cache create</div></div>
          <div class="token-stat"><div class="spark-row"><span class="tval">${t.messages}</span>${sparkline('tokenMessages')}</div><div class="tlabel">messages</div></div>
        </div>
        ${agentSparkRows ? `<div class="token-grid mt-2">${agentSparkRows}</div>` : ''}
        ${advisorHtml}
        <div class="token-models">${modelChips}</div>
        ${agentSessions.length > 0 ? `<details class="token-sessions"${sessionsOpen ? ' open' : ''}><summary>Active Sessions (${agentSessions.length})</summary>${sessionRows}</details>` : ''}`;
      el.innerHTML = sectionCardShell({ id: 'token-panel', title: '🪙 Tokens', gripLabel: 'Tokens', panelClass: 'token-panel', headerClass: 'token-title', badgeHtml: `<span class="lookback">${tokens.lookbackHours || 24}h window · ${t.sessions} sessions</span>`, summaryHtml: tokenSummary, summaryTitle: 'Current burn and session count' }, tokenBody);
      applySectionCollapse('token-panel');
      const detailsEl = el.querySelector('.token-sessions');
      if (detailsEl) {
        /* Explicitly set .open property — HTML attribute alone can be unreliable after innerHTML */
        detailsEl.open = sessionsOpen;
        detailsEl.addEventListener('toggle', () => { localStorage.setItem('hive-sessions-open', detailsEl.open ? '1' : '0'); });
      }
    }

