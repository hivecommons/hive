package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func (s *Server) handleBackends(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	vllmModels := s.queryInferenceModels("vllm")
	llmdModels := s.queryInferenceModels("llm-d")
	litellmModels := s.queryInferenceModels("litellm")

	// CLI backends each have a DIFFERENT discovery source (see cli_models.go):
	// provider HTTP APIs, vendor CLI protocols/subcommands, or a deliberately
	// authoritative single option. Every probe is best-effort and falls back to
	// a current static list, so a dropdown is never empty.
	claudeCLI := s.queryCLIModelsForBoot("claude")
	copilotCLI := s.queryCLIModelsForBoot("copilot")
	geminiCLI := s.queryCLIModelsForBoot("gemini")
	gooseCLI := s.queryCLIModelsForBoot("goose")
	codexCLI := s.queryCLIModelsForBoot("codex")
	agyCLI := s.queryCLIModelsForBoot(agyBackendID)
	ompCLI := s.queryCLIModelsForBoot(ompBackendID)
	// bob has no discovery source and no usable --model flag: it selects its
	// own model. Served explicitly so the client never falls through to the
	// copilot catalog and offers models bob cannot honor (see bobStaticModels).
	bobCLI := s.queryCLIModelsForBoot(bobBackendID)

	// cliBackendEntry renders one CLI backend, attaching the discovery notice
	// when there is one. A notice means the fallback list is being served
	// because UPSTREAM REJECTED THIS ACCOUNT, not because a probe was absent
	// or briefly unreachable — the dropdown says so instead of presenting a
	// static catalog that looks like a working entitlement (#6500).
	cliBackendEntry := func(id, name string, r cliModelResult) map[string]interface{} {
		entry := map[string]interface{}{
			"id": id, "name": name, "models": r.models, "fallback": r.fallback,
			// degraded: a LIVE list that is not the CLI's catalog (the HTTP
			// probe after the installed SDK helper failed, #7384). Real ids,
			// wrong inventory — the client labels it and auto-heal must sit
			// it out exactly as it sits out a fallback.
			"degraded": r.degraded,
		}
		if len(r.reasoningEfforts) > 0 {
			entry["reasoning_efforts"] = r.reasoningEfforts
		}
		if r.notice != nil {
			entry["notice"] = r.notice
		}
		if r.failed() {
			// discovery: WHY the list is not authoritative, in the words of
			// the failure itself. This is the string an operator had to read
			// off a server log by hand to tell "Could not find a
			// @github/copilot platform package" from "Not authenticated"
			// (#7384); now the dropdown carries it.
			entry["discovery"] = map[string]interface{}{
				"ok":       false,
				"error":    r.discoveryErr,
				"fallback": r.fallback,
				"degraded": r.degraded,
			}
		}
		return entry
	}

	w.Header().Add("Server-Timing", fmt.Sprintf("dashboard_config_backends;dur=%.1f, dashboard_lock_wait;dur=0", float64(time.Since(start).Microseconds())/1000))
	jsonResponse(w, []map[string]interface{}{
		cliBackendEntry("claude", "Claude Code", claudeCLI),
		cliBackendEntry("copilot", "GitHub Copilot", copilotCLI),
		cliBackendEntry(bobBackendID, "bob (IBM bobshell)", bobCLI),
		cliBackendEntry("gemini", "Gemini", geminiCLI),
		cliBackendEntry("goose", "Goose", gooseCLI),
		cliBackendEntry("codex", "OpenAI Codex", codexCLI),
		cliBackendEntry(agyBackendID, "Google Antigravity (agy)", agyCLI),
		cliBackendEntry(ompBackendID, "Oh My Pi (omp)", ompCLI),
		{"id": "vllm", "name": "vLLM (self-hosted)", "models": vllmModels, "inference": true},
		{"id": "llm-d", "name": "llm-d (self-hosted)", "models": llmdModels, "inference": true},
		{"id": "litellm", "name": "LiteLLM (proxy)", "models": litellmModels, "inference": true},
	})
}

func (s *Server) handleInferenceModels(w http.ResponseWriter, r *http.Request) {
	backend := r.PathValue("backend")
	if backend == "" {
		jsonError(w, "backend required", http.StatusBadRequest)
		return
	}
	endpoints, ok := s.getInferenceEndpoints(backend)
	if !ok {
		jsonError(w, "unknown inference backend: "+backend, http.StatusNotFound)
		return
	}

	models, complete := s.fetchInferenceModelsForBackendDetailed(backend, endpoints)
	fallback := false
	if len(models) == 0 {
		s.logger.Warn("no models found from any endpoint", "backend", backend, "endpoints", len(endpoints))
		// Discovery is authoritative (a LiteLLM gateway entitlement-filters
		// /v1/models per API key); only when it fails do we fall back to
		// the common static aliases, flagged so the UI can mark them as
		// unverified against the configured endpoint/key.
		models = inferenceStaticModelAliases
		fallback = true
	}

	resp := map[string]interface{}{
		"backend":  backend,
		"models":   models,
		"fallback": fallback,
		// partial: some of the backend's endpoints answered and others did
		// not, so `models` is a FLOOR, not a census — every id in it really
		// was discovered (so the dropdown labels them normally, unlike the
		// static fallback), but a model's absence proves nothing. Auto-heal
		// must sit out this sample rather than switch an agent off a model
		// that only the unreachable endpoint serves (#4438).
		"partial": !fallback && !complete,
	}
	// Some LiteLLM gateways advertise the FULL catalog on /v1/models but scope
	// a key's team to a SUBSET; a non-entitled model 403s at inference. When
	// the proxy has learned the entitled set (key-info probe or a "team not
	// allowed" 403), narrow the returned list to it so the dropdown offers
	// only usable models. Not applied to the static fallback (unverified).
	if !fallback {
		if entitled, source, ok := s.entitledModelsFor(backend, endpoints); ok {
			usable := intersectEntitled(models, entitled)
			if len(usable) > 0 {
				resp["models"] = usable
				resp["entitled"] = true
				resp["entitledSource"] = source
			}
		}
	}
	jsonResponse(w, resp)
}

// entitledModelsFor returns the entitled model set the proxy has learned for a
// LiteLLM backend's endpoint, if any. Only litellm gateways entitlement-filter
// per key; vllm/llm-d return no entitlement signal.
func (s *Server) entitledModelsFor(backend string, endpoints []string) (models []string, source string, known bool) {
	if backend != "litellm" {
		return nil, "", false
	}
	fn := getEntitledModelsFn()
	if fn == nil {
		return nil, "", false
	}
	for _, ep := range endpoints {
		if m, src, ok := fn(ep); ok {
			return m, src, true
		}
	}
	return nil, "", false
}

// intersectEntitled returns the discovered models that are also in the entitled
// set, preserving discovery order. Matching tolerates a missing/extra provider
// prefix (stored "gpt-4o" vs entitled "Azure/gpt-4o") by comparing the id tail
// after the last slash when the full ids differ.
func intersectEntitled(discovered, entitled []string) []string {
	full := make(map[string]bool, len(entitled))
	tail := make(map[string]bool, len(entitled))
	for _, e := range entitled {
		full[e] = true
		tail[e[strings.LastIndex(e, "/")+1:]] = true
	}
	out := make([]string, 0, len(discovered))
	for _, d := range discovered {
		if full[d] || tail[d[strings.LastIndex(d, "/")+1:]] {
			out = append(out, d)
		}
	}
	return out
}

// fetchInferenceModelsForBackendDetailed discovers a backend's models,
// honouring the AUTH SCHEME of the gateway that backend resolves to. The plain
// path sends the resolved key as a raw bearer, which is correct for
// litellm/vllm/llm-d — but a watsonx gateway needs an IAM-minted bearer plus
// the X-IBM-Project-ID header, so sending the raw IBM Cloud key returns 401 and
// the dropdown silently fell back to unrelated static aliases. Reuses the same
// gatewayProbeAuth + fetchModelsWithHeaders pair the Model Gateways tab's
// discover/test paths use, so the agent's model dropdown and the gateway form
// agree on what is available. Falls back to the legacy raw-key path whenever no
// gateway resolves for this name (env-configured vllm/llm-d endpoints).
//
// The second result reports whether EVERY endpoint answered, so a caller can
// tell a complete census from a partial one (#4438). It matters most here: this
// is the discovery the dashboard's model auto-heal reads, and auto-heal does
// not merely toast — it rewrites the agent's configured model and relaunches
// its session. A gateway that drops out of a multi-endpoint sweep must never be
// able to spend an agent's selection that way.
func (s *Server) fetchInferenceModelsForBackendDetailed(backend string, endpoints []string) ([]string, bool) {
	gw := s.resolveGatewayForBackend(backend)
	if gw == nil || !gatewayKindNeedsProbeAuth(gw.Kind) {
		return fetchModelsFromEndpointsDetailed(endpoints, s.inferenceAPIKey(backend))
	}
	bearer, headers, err := s.gatewayProbeAuth(gw.Kind, gw.ResolveAPIKey(), gw.ProjectID)
	if err != nil {
		s.logger.Warn("gateway auth for model discovery failed",
			"backend", backend, "kind", gw.Kind, "error", err)
		return nil, false
	}
	seen := make(map[string]bool)
	var all []string
	complete := true
	for _, ep := range endpoints {
		models, err := fetchModelsWithHeaders(ep, bearer, headers)
		if err != nil {
			s.logger.Warn("model discovery failed",
				"backend", backend, "kind", gw.Kind, "endpoint", ep, "error", err)
			complete = false
			continue
		}
		for _, m := range models {
			if !seen[m] {
				seen[m] = true
				all = append(all, m)
			}
		}
	}
	return all, complete
}

// gatewayKindNeedsProbeAuth reports whether a gateway kind requires the
// gatewayProbeAuth path (a minted bearer and/or extra headers) rather than the
// raw key. Only watsonx does today; keeping it a predicate means a future kind
// with its own auth scheme is a one-line change here rather than a new branch in
// every discovery caller.
func gatewayKindNeedsProbeAuth(kind string) bool {
	return kind == config.GatewayKindWatsonx
}

// resolveGatewayForBackend finds the configured gateway an agent backend routes
// through. Agent backends name a gateway directly (registerGatewayEndpoints
// keys the endpoint registry by gateway NAME), and the built-in gateway methods
// share their name with their kind, so match on either.
func (s *Server) resolveGatewayForBackend(backend string) *config.GatewayConfig {
	if s.deps == nil || s.deps.Config == nil {
		return nil
	}
	for _, gw := range s.deps.Config.Governor.ResolvedGateways() {
		if strings.EqualFold(gw.Name, backend) || strings.EqualFold(gw.Kind, backend) {
			out := gw
			return &out
		}
	}
	return nil
}

// inferenceAPIKey returns the bearer key used for a backend's model
// discovery requests. litellm requires auth on /v1/models. vllm/llm-d are
// usually unauthenticated, but the configured endpoint may in fact be a
// LiteLLM gateway, which entitlement-filters /v1/models and hides
// key-gated models from anonymous callers — so resolve a backend-specific
// key first (governor.vllm / governor.llm-d api_key_env|api_key_file, or
// the HIVE_VLLM_API_KEY / HIVE_LLMD_API_KEY defaults), then fall back to
// the litellm key. A plain vLLM/llm-d server without --api-key ignores the
// Authorization header, so sending a key is harmless there.
func (s *Server) inferenceAPIKey(backend string) string {
	if s.deps == nil || s.deps.Config == nil {
		return ""
	}
	gov := &s.deps.Config.Governor
	switch backend {
	case "vllm":
		if key := gov.VLLM.ResolveAPIKey(config.DefaultVLLMAPIKeyEnv); key != "" {
			return key
		}
	case "llm-d":
		if key := gov.LLMD.ResolveAPIKey(config.DefaultLLMDAPIKeyEnv); key != "" {
			return key
		}
	}
	// Same contract as inference-time forwarding: an explicit gateway for
	// this backend wins over the legacy litellm: key store (see
	// ResolveLiteLLMInferenceKey). A key rotated via the Model Gateways tab
	// lands only in the gateway's key file, so resolving the legacy file here
	// made discovery send the revoked key and log "no models found from any
	// endpoint" (surfaced as a sticky error toast) while the save-time probe
	// — which uses the submitted key — reported the models fine.
	//
	// A matching gateway that resolves NO key of its own still falls back to
	// the legacy store: keyless gateway entries (e.g. an endpoint-only entry
	// with the key kept in the classic litellm: block) predate per-gateway
	// keys and must keep discovering with the legacy key.
	if key := gov.ResolveLiteLLMInferenceKey(backend); key != "" {
		return key
	}
	return gov.LiteLLM.ResolveAPIKey()
}

// inferenceStaticModelAliases is the FALLBACK model list for the inference
// model dropdowns (vllm, llm-d, litellm), offered only when runtime
// /v1/models discovery against the backend's configured endpoint fails
// (and no HIVE_*_MODELS env override is set), so a dropdown is never
// empty. Discovery is authoritative: a LiteLLM gateway entitlement-filters
// /v1/models per API key, so when discovery succeeds we show ONLY the
// discovered set — appending these aliases on top would offer unlicensed
// models that just 403 at runtime. The entries use the LiteLLM gateway
// naming convention observed live: UNDERSCORES between words and a DOT in
// the version (claude_opus_4.8) — unlike the Claude CLI (all hyphens,
// claude-opus-4-8) and the Copilot/Gemini backends (hyphenated words with
// a dotted version, claude-opus-4.8 / gemini-2.5-pro). Model set derived
// from the CLAUDE_CLI_MODELS/COPILOT_CLI_MODELS lists in static/index.html
// and the gemini backend list in handleBackends; keep in sync when those
// change.
var inferenceStaticModelAliases = []string{
	"claude_opus_4.8",
	"claude_opus_4.7",
	"claude_opus_4.6",
	"claude_sonnet_4.6",
	"claude_haiku_4.5",
	"gemini_2.5_pro",
	"gemini_2.5_flash",
}

func (s *Server) queryInferenceModels(backend string) []string {
	models, _ := s.queryInferenceModelsDetailed(backend)
	return models
}

// queryInferenceModelsDetailed additionally reports whether the returned list
// is NON-AUTHORITATIVE — a stand-in for a census that did not fully succeed —
// rather than a complete live-discovered or operator-configured set. Callers
// surfacing model add/remove events must ignore diffs against such a list.
//
// Three shapes are non-authoritative, and all three produced the same false
// "Model removed" storm before they were flagged:
//   - the static aliases, when discovery failed outright (#4426);
//   - a PARTIAL sweep, when some endpoints answered and others did not;
//   - the HIVE_*_MODELS env list, when it is reached only BECAUSE a
//     registered endpoint failed to answer (#4438).
//
// The env list is authoritative in the one case where it is the configured
// source of models rather than a consolation prize: no endpoint registered at
// all, so nothing was ever probed.
func (s *Server) queryInferenceModelsDetailed(backend string) ([]string, bool) {
	endpoints, ok := s.getInferenceEndpoints(backend)
	probed := ok && len(endpoints) > 0
	if probed {
		models, complete := fetchModelsFromEndpointsDetailed(endpoints, s.inferenceAPIKey(backend))
		if len(models) > 0 {
			// Show a partial sweep — a short dropdown beats an empty one —
			// but never let it stand as a census of what exists.
			return models, !complete
		}
	}
	envVar := "HIVE_VLLM_MODELS"
	switch backend {
	case "llm-d":
		envVar = "HIVE_LLMD_MODELS"
	case "litellm":
		envVar = "HIVE_LITELLM_MODELS"
	}
	if val := os.Getenv(envVar); val != "" {
		return inferenceModelsFromEnv(envVar, ""), probed
	}
	// Discovery failed or the backend is unconfigured — fall back to the
	// common static aliases (unverified against any endpoint/key).
	return inferenceStaticModelAliases, true
}

const inferenceModelQueryTimeout = 3 * time.Second

// fetchModelsFromEndpointsDetailed additionally reports whether EVERY endpoint
// answered. A PARTIAL sweep — one gateway of several timing out or answering
// 403 while its siblings reply — still returns a non-empty list, and a caller
// that diffs model sets must not read the survivors as "the unreachable
// endpoint's models were removed" (#4438: a partial sweep wallpapered the
// dashboard with false "Model removed from litellm: …" toasts, including for
// the very model the hive's agents were configured to use).
func fetchModelsFromEndpointsDetailed(endpoints []string, apiKey string) ([]string, bool) {
	seen := make(map[string]bool)
	var all []string
	complete := true
	for _, ep := range endpoints {
		models, err := fetchModelsFromEndpoint(ep, apiKey)
		if err != nil {
			complete = false
			continue
		}
		for _, m := range models {
			if !seen[m] {
				seen[m] = true
				all = append(all, m)
			}
		}
	}
	return all, complete
}

func fetchModelsFromEndpoint(baseURL, apiKey string) ([]string, error) {
	return fetchModelsWithHeaders(baseURL, apiKey, nil)
}

// fetchModelsWithHeaders is fetchModelsFromEndpoint with optional extra request
// headers (e.g. watsonx's X-IBM-Project-ID). apiKey, when non-empty, is sent as
// the Bearer; for watsonx the caller passes the minted IAM token as apiKey.
func fetchModelsWithHeaders(baseURL, apiKey string, extraHeaders map[string]string) ([]string, error) {
	modelsURL := strings.TrimRight(baseURL, "/") + "/v1/models"
	client := &http.Client{
		Timeout: inferenceModelQueryTimeout,
		// SECURITY (audit F13): Go's default redirect policy PRESERVES the
		// Authorization header across same-host hops and follows up to 10
		// redirects. An upstream that answers /v1/models with a 302 could
		// therefore walk the gateway key onward. Strip credentials on any hop
		// that changes host, so a redirect can never carry the key somewhere the
		// original request was not authorized to reach. probeModelsWithHeaders
		// already used this policy; this sibling was simply missed.
		CheckRedirect: noRedirectToPrivate,
	}
	req, err := http.NewRequest("GET", modelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range extraHeaders {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer closeHTTPBody(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, litellmModelsHTTPError(req, resp, apiKey)
	}

	models, err := parseModelsResponse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return models, nil
}

// parseModelsResponse decodes an OpenAI-style GET /v1/models body into the
// list of model IDs. It is the single source of truth shared by the
// model-discovery dropdown (fetchModelsFromEndpoint) and the Test Connection
// probe (probeLiteLLMModels), so both agree on what "N models" means.
//
// It is deliberately lenient: any JSON object with a "data" array whose items
// carry a non-empty "id" string is accepted. It does NOT require top-level
// object=="list" (gateways may omit or reorder it), imposes no field order,
// and tolerates extra/unknown per-item fields (object, created, owned_by, …).
// IDs are returned verbatim, so provider-prefixed / hyphenated ids like
// "Azure/gpt-5.1-codex-2025-11-13" or "claude-opus-4-7" survive intact.
func parseModelsResponse(r io.Reader) ([]string, error) {
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&result); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

func inferenceModelsFromEnv(envVar, defaultModel string) []string {
	val := os.Getenv(envVar)
	if val == "" {
		return []string{defaultModel}
	}
	models := strings.Split(val, ",")
	for i := range models {
		models[i] = strings.TrimSpace(models[i])
	}
	return models
}
