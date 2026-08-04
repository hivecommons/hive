package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// This file adds runtime model discovery for the CLI backends (copilot,
// claude, gemini, goose), mirroring the /v1/models discovery already done
// for the inference backends (vllm/llm-d/litellm) in api.go.
//
// Each CLI has a DIFFERENT discovery source — there is NO single uniform
// endpoint like LiteLLM's /v1/models:
//
//   - copilot: GET <copilot-api>/models with the stored GitHub OAuth token
//     and a Copilot-Integration-Id header. The api host is per-account
//     (public vs enterprise), so it is itself discovered from
//     api.github.com/copilot_internal/user (endpoints.api). Verified live.
//   - gemini:  GET generativelanguage.googleapis.com/v1beta/models with the
//     configured Gemini API key. Live only when a key is configured.
//   - claude:  Anthropic publishes no per-subscription "list my models"
//     endpoint and the Claude CLI has no non-interactive `claude models`
//     command, so this is a maintained static list of CURRENT model ids
//     (kept in sync with CLAUDE_CLI_MODELS in static/index.html).
//   - goose:   provider-configured (Ollama/OpenAI/Anthropic/...). Without a
//     stable machine-readable "list models for my provider" command, this
//     returns a per-provider static list, or "default" when unconfigured.
//
// Every discovery is BEST-EFFORT: a failed or absent probe falls back to a
// current static list so a dropdown is never empty and never errors. Results
// are cached with a short TTL. Tokens are never logged.

const (
	// cliModelCacheTTL bounds how long a discovered (or fallback) CLI model
	// list is reused before re-probing. Mirrors the inference-model dropdown
	// cache TTL on the frontend (INFERENCE_MODEL_CACHE_TTL_MS = 30s) so the
	// server- and client-side caches age at the same rate.
	cliModelCacheTTL = 30 * time.Second

	// cliModelQueryTimeout limits each outbound discovery HTTP call so a slow
	// or hanging upstream cannot stall the /api/config/backends response.
	cliModelQueryTimeout = 5 * time.Second

	// cliModelDropAfterMisses is how many consecutive successful live
	// discovery probes a previously-seen model must be missing from before
	// it is dropped from the served list. The Copilot /models response is
	// nondeterministic mid-rollout — verified live: the same token returned
	// 31 vs 35 chat-capable models across calls seconds apart, with
	// rollout-gated ids (claude-opus-4.6, claude-fable-5, gemini-3.x,
	// kimi-k2.7-code) absent from individual samples. Serving one bad
	// sample as authoritative made the dashboard auto-heal switch every
	// agent off a model the account is in fact entitled to. Retention only
	// smooths LIVE results; static fallback lists bypass it entirely, and a
	// genuinely revoked model still drops after this many consecutive
	// misses (roughly cliModelDropAfterMisses × cliModelCacheTTL of stable
	// absence).
	cliModelDropAfterMisses = 3

	// --- Copilot ---

	// copilotUserEndpointURL returns per-account Copilot metadata, including
	// endpoints.api (which differs for enterprise vs public plans) — so the
	// models host is discovered, never hardcoded.
	copilotUserEndpointURL = "https://api.github.com/copilot_internal/user"

	// copilotDefaultAPIHost is the public Copilot API host, used only if the
	// per-account endpoint lookup fails to return one.
	copilotDefaultAPIHost = "https://api.githubcopilot.com"

	// copilotModelsPath is the models listing path on the Copilot API host.
	copilotModelsPath = "/models"

	// copilotIntegrationID identifies the calling client to the Copilot API.
	// "vscode-chat" is the widely-accepted chat integration id (verified live
	// against both public and enterprise hosts). Overridable via
	// HIVE_COPILOT_INTEGRATION_ID in case the accepted value changes.
	copilotIntegrationID = "vscode-chat"

	// copilotEditorVersion is sent as Editor-Version; the Copilot endpoints
	// require a plausible editor identifier.
	copilotEditorVersion = "vscode/1.99.0"

	// copilotSDKHelperPath is where the image installs the Node helper that
	// lists Copilot models through the official @github/copilot-sdk (source:
	// bin/copilot-models.mjs, COPYed by v2/Dockerfile). The SDK spawns the
	// pinned copilot CLI as a JSON-RPC server, so the probe rides the CLI's
	// own stored auth and TLS handling — auth configurations the raw-HTTP
	// probe below cannot reach (verified live against copilot CLI 1.0.59 in a
	// hive pod: 25 models via stored device-flow auth behind the egress
	// proxy). Absent outside the image, in which case the SDK probe is
	// skipped instantly.
	copilotSDKHelperPath = "/usr/local/bin/copilot-models.mjs"

	// copilotSDKNodeBinary runs the helper. The helper is plain-Node ESM; it
	// is invoked explicitly (not via shebang) so no exec bit is needed.
	copilotSDKNodeBinary = "node"

	// copilotSDKProbeTimeout bounds the helper exec. The helper enforces its
	// OWN hard deadline (INTERNAL_TIMEOUT_MS = 15s in copilot-models.mjs);
	// this sits slightly above it so the helper always gets to report its own
	// error on stderr instead of being killed mid-flight.
	copilotSDKProbeTimeout = 20 * time.Second

	// copilotSDKAgentHome is the shared agent HOME the copilot CLI's stored
	// device-flow auth lives under ($HOME/.copilot). Agents launch with
	// HOME=/data/home (see agent.Manager; on hosted spokes /home/dev symlinks
	// to it), so the helper is run with the same HOME when the directory
	// exists — otherwise the server process's own HOME is left in place.
	copilotSDKAgentHome = "/data/home"

	// copilotSDKStderrLogLimit caps how much helper stderr is folded into the
	// returned error (and thus a single log line) — enough to diagnose, never
	// a dump. Helper stderr carries only error text, never tokens.
	copilotSDKStderrLogLimit = 300

	// copilotPolicyStateEnabled is the SDK policy.state value marking a model
	// as usable by this account; models carrying any OTHER explicit state are
	// excluded. Models with no policy at all are kept (the catalog omits
	// policy for unrestricted models).
	copilotPolicyStateEnabled = "enabled"

	// copilotAutoModel is the Copilot auto-selection sentinel: `copilot --model
	// auto` lets the Copilot CLI pick/adjust the optimal model per task instead
	// of pinning a concrete id. It is a valid --model VALUE (confirmed against
	// copilot CLI v1.0.59), not a discoverable catalog id, so it is always
	// offered (copilotAlwaysIncludeModels) and exempt from model auto-heal.
	copilotAutoModel = "auto"

	// --- bob (IBM bobshell) ---

	// bobBackendID is the backend id for the IBM bobshell ("bob") CLI. Mirrors
	// the agent package's unexported bobBackend constant; keep the two in sync.
	bobBackendID = "bob"

	// bobAutoModel is the ONLY model option offered for bob, and it is a UI
	// placeholder rather than a flag value: bob selects its own model and has
	// no working --model flag. Passing one is actively harmful — a concrete id
	// (e.g. claude-sonnet-4.6) makes every prompt die with "Cannot read
	// properties of undefined (reading 'maxTokens')", verified live on a spoke,
	// while the same bob with no --model runs inference fine.
	//
	// Unlike copilotAutoModel, this value must NEVER reach a command line:
	// `bob --model auto` is UNTESTED and presumed unsafe. The launch path
	// (agent.bobLaunchCmd) omits --model entirely, so "auto" only ever exists
	// as the stored/displayed selection.
	bobAutoModel = "auto"

	// --- Gemini ---

	// geminiModelsURL lists models available to a Gemini API key.
	geminiModelsURL = "https://generativelanguage.googleapis.com/v1beta/models"

	// geminiModelsPageSize caps the models page (the API allows up to 1000).
	geminiModelsPageSize = 200

	// geminiGenerateMethod is the supportedGenerationMethods entry that marks
	// a model as usable for chat/content generation (vs embeddings, etc.).
	geminiGenerateMethod = "generateContent"
)

// --- Static fallback lists (kept CURRENT — July 2026) ---

// claudeStaticModels is the maintained fallback/authoritative list for the
// Claude CLI. Anthropic exposes no "list my models" API and the CLI has no
// non-interactive list command, so this list IS the source of truth. Keep it
// in sync with CLAUDE_CLI_MODELS in static/index.html. Both the canonical
// dated/versioned ids AND the bare aliases the CLI accepts are included.
var claudeStaticModels = []string{
	"claude-fable-5",
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-opus-4-6",
	"claude-sonnet-5",
	"claude-sonnet-4-6",
	"claude-sonnet-4-5",
	"claude-haiku-4-5",
	// Bare aliases the Claude CLI also accepts.
	"opus",
	"sonnet",
	"haiku",
}

// copilotStaticModels is the fallback offered when the Copilot models probe
// cannot run (no token) or fails. Copilot's real list is entitlement-filtered
// per plan and changes often, so live discovery is strongly preferred; this
// is only a floor so the dropdown is never empty.
var copilotStaticModels = []string{
	"gpt-5.4",
	"gpt-5.2",
	"gpt-4.1",
	"gpt-4o",
	"claude-opus-4.6",
	"claude-sonnet-4.6",
	"claude-sonnet-4.5",
	"claude-haiku-4.5",
	"gemini-2.5-pro",
	"gemini-flash-3.5",
	"o3",
	"o4-mini",
}

// copilotAlwaysIncludeModels are ids that must ALWAYS be offered in the
// copilot dropdown, whether the served list came from live discovery or the
// static fallback. The Copilot /models endpoint omits rollout-gated ids
// nondeterministically (see cliModelDropAfterMisses), and an id that never
// appears in a live sample can neither be selected nor survive the
// dashboard's model auto-heal. Ids listed here are appended to every served
// copilot list so agents can be pinned to them safely.
var copilotAlwaysIncludeModels = []string{
	"mai-code-1-flash-picker",
	// Auto-select sentinel — never returned by the /models catalog, but a valid
	// launch value, so it must always be offered and must survive auto-heal.
	copilotAutoModel,
}

// bobStaticModels is bob's complete model list: exactly one entry, the auto
// sentinel. bob has no model catalog to discover and no usable --model flag
// (see bobAutoModel), so offering anything else would invite a selection that
// either does nothing or breaks inference.
var bobStaticModels = []string{bobAutoModel}

// codexStaticModels is the maintained list for the OpenAI Codex CLI. Codex only
// accepts OpenAI models — Claude ids are rejected with a ChatGPT account
// ("model is not supported when using Codex with a ChatGPT account"), so codex
// must never fall through to the copilot list. Order mirrors `codex /model`
// (sol is the default). Keep CURRENT with the Codex CLI's model set.
var codexStaticModels = []string{
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.4-mini",
	"gpt-5.3-codex-spark",
}

// geminiStaticModels is the fallback when no Gemini API key is configured (or
// discovery fails). Keep CURRENT.
var geminiStaticModels = []string{
	"gemini-3-pro-preview",
	"gemini-3-flash-preview",
	"gemini-2.5-pro",
	"gemini-2.5-flash",
}

// gooseProviderStaticModels maps a configured goose provider to a sensible
// static model list, used when goose can't be probed for its provider's live
// model set. "default" (last resort) means goose is unconfigured.
var gooseProviderStaticModels = map[string][]string{
	"ollama":     {"llama3.3", "qwen2.5", "deepseek-r1", "default"},
	"openai":     {"gpt-5.4", "gpt-4.1", "gpt-4o", "o3", "o4-mini"},
	"anthropic":  {"claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5"},
	"google":     {"gemini-3-pro-preview", "gemini-2.5-pro", "gemini-2.5-flash"},
	"databricks": {"databricks-claude-opus", "databricks-meta-llama"},
}

// cliModelResult is a cached, best-effort model list for one CLI backend.
type cliModelResult struct {
	models   []string
	fallback bool // true when the list is the static fallback, not live discovery
}

type cliModelCacheEntry struct {
	result cliModelResult
	at     time.Time
}

// cliModelCache is a short-TTL per-backend cache guarding the discovery probes.
// It also carries the live-list retention state (see stabilize).
type cliModelCache struct {
	mu      sync.Mutex
	entries map[string]cliModelCacheEntry
	// retained tracks, per backend, every model id seen in a successful live
	// probe together with its consecutive-miss count (0 = present in the
	// latest probe). Used by stabilize() to smooth nondeterministic upstream
	// responses. Insertion order is kept in retainedOrder so the served list
	// stays stable for the frontend's list-change detection.
	retained      map[string]map[string]int
	retainedOrder map[string][]string
}

func newCLIModelCache() *cliModelCache {
	return &cliModelCache{
		entries:       make(map[string]cliModelCacheEntry),
		retained:      make(map[string]map[string]int),
		retainedOrder: make(map[string][]string),
	}
}

// stabilize merges a fresh successful LIVE discovery result with recently-seen
// models for the backend: a model that was present in earlier live probes but
// is missing from this one is retained (appended after the fresh ids, in
// first-seen order) until it has been missing from cliModelDropAfterMisses
// consecutive probes. This shields consumers — most importantly the
// dashboard's model auto-heal — from upstream sampling noise while still
// converging on genuine entitlement changes. Never called for fallback lists.
func (c *cliModelCache) stabilize(backend string, fresh []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	misses := c.retained[backend]
	if misses == nil {
		misses = make(map[string]int)
		c.retained[backend] = misses
	}
	inFresh := make(map[string]bool, len(fresh))
	for _, m := range fresh {
		inFresh[m] = true
		if _, known := misses[m]; !known {
			c.retainedOrder[backend] = append(c.retainedOrder[backend], m)
		}
		misses[m] = 0
	}

	out := append([]string(nil), fresh...)
	keptOrder := c.retainedOrder[backend][:0]
	for _, m := range c.retainedOrder[backend] {
		if inFresh[m] {
			keptOrder = append(keptOrder, m)
			continue
		}
		misses[m]++
		if misses[m] >= cliModelDropAfterMisses {
			delete(misses, m) // genuinely gone — stop retaining
			continue
		}
		keptOrder = append(keptOrder, m)
		out = append(out, m) // retained: missing, but not yet confirmed gone
	}
	c.retainedOrder[backend] = keptOrder
	return out
}

// get returns a cached result if it is still within the TTL.
func (c *cliModelCache) get(backend string) (cliModelResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[backend]
	if !ok || time.Since(e.at) > cliModelCacheTTL {
		return cliModelResult{}, false
	}
	return e.result, true
}

func (c *cliModelCache) set(backend string, r cliModelResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[backend] = cliModelCacheEntry{result: r, at: time.Now()}
}

// queryCLIModels returns the model list for a CLI backend (copilot, claude,
// gemini, goose), running the backend's best-effort discovery probe (cached)
// and falling back to a current static list on any failure. It never errors
// and never returns an empty list.
// modelIDsForBackend returns the known model ids for a backend WITHOUT issuing
// a discovery probe: it uses only the already-populated cache. Validation runs
// on the request path, so a cold cache must not block the operator behind a
// network round-trip — an empty result means "cannot verify", and callers let
// the value through rather than rejecting something that may be valid.
//
// A fallback (static) list is also treated as unverifiable: it is a maintained
// guess, not the backend's authoritative inventory, and rejecting against it
// would block legitimately available models.
func (s *Server) modelIDsForBackend(backend string) []string {
	if s.cliModels == nil || backend == "" {
		return nil
	}
	r, ok := s.cliModels.get(backend)
	if !ok || r.fallback {
		return nil
	}
	return append([]string(nil), (r.models)...)
}

func (s *Server) queryCLIModels(backend string) cliModelResult {
	if s.cliModels != nil {
		if r, ok := s.cliModels.get(backend); ok {
			return r
		}
	}

	var r cliModelResult
	switch backend {
	case "copilot":
		r = s.discoverCopilotModels()
	case "gemini":
		r = s.discoverGeminiModels()
	case "goose":
		r = s.discoverGooseModels()
	case "claude":
		// No live source exists; the maintained static list is authoritative.
		r = cliModelResult{models: dedupeModels(claudeStaticModels), fallback: false}
	case "codex":
		// No probe wired yet; the maintained static list is authoritative and
		// OpenAI-only (Claude ids are rejected by Codex with a ChatGPT account).
		r = cliModelResult{models: dedupeModels(codexStaticModels), fallback: false}
	case bobBackendID:
		// bob picks its own model and exposes no catalog, so there is nothing
		// to discover. The single auto sentinel is authoritative (not a
		// fallback) — marking it fallback=true would label it "(common alias,
		// unverified)" in the dropdown and make it eligible for auto-heal.
		r = cliModelResult{models: dedupeModels(bobStaticModels), fallback: false}
	default:
		r = cliModelResult{models: nil, fallback: true}
	}

	if len(r.models) == 0 {
		r.models = dedupeModels(cliStaticFallback(backend))
		r.fallback = true
	} else if !r.fallback && s.cliModels != nil {
		// Smooth nondeterministic live results (see cliModelDropAfterMisses):
		// a model seen recently is kept through transient upstream omissions
		// so a single bad sample cannot trigger downstream auto-heal churn.
		r.models = s.cliModels.stabilize(backend, r.models)
	}
	// Applied after stabilization/fallback so the pinned ids survive BOTH
	// paths: a live sample that omits a rollout-gated id and the static
	// fallback list (see copilotAlwaysIncludeModels).
	if backend == "copilot" {
		r.models = dedupeModels(append(r.models, copilotAlwaysIncludeModels...))
	}
	if s.cliModels != nil {
		s.cliModels.set(backend, r)
	}
	return r
}

// cliStaticFallback returns the current static model list for a CLI backend.
func cliStaticFallback(backend string) []string {
	switch backend {
	case "copilot":
		return copilotStaticModels
	case "gemini":
		return geminiStaticModels
	case "claude":
		return claudeStaticModels
	case "codex":
		return codexStaticModels
	case bobBackendID:
		return bobStaticModels
	case "goose":
		return []string{"default"}
	default:
		return nil
	}
}

// --- Copilot discovery ---

// discoverCopilotModels lists the models available to this hive's Copilot
// auth. Hardened probe order:
//
//  1. the official-SDK helper (copilotSDKHelperPath) — the SDK drives the
//     installed copilot CLI, so it rides the CLI's own auth resolution and
//     TLS handling and works in configurations the raw-HTTP probe cannot
//     reach (e.g. stored device-flow auth with no token surfaced here);
//  2. the raw HTTP GET <copilot-api>/models probe with the stored token,
//     where the api host is itself discovered per-account (public vs
//     enterprise);
//  3. any failure returns fallback=true with an empty list so the caller
//     substitutes the static list.
//
// Successful results from either live probe feed the same cache/retention
// machinery (see stabilize) via queryCLIModels.
func (s *Server) discoverCopilotModels() cliModelResult {
	token := s.copilotToken()

	if models, err := s.probeCopilotModelsSDK(token); err != nil {
		s.logger.Info("copilot SDK model discovery unavailable, falling back to HTTP probe", "err", err.Error())
	} else if len(models) == 0 {
		s.logger.Info("copilot SDK model discovery returned no models, falling back to HTTP probe")
	} else {
		return cliModelResult{models: dedupeModels(models), fallback: false}
	}

	if token == "" {
		return cliModelResult{fallback: true}
	}
	host := s.copilotAPIHost(token)
	integrationID := copilotIntegrationID
	if v := os.Getenv("HIVE_COPILOT_INTEGRATION_ID"); v != "" {
		integrationID = v
	}

	models, err := fetchCopilotModels(host+copilotModelsPath, token, integrationID)
	if err != nil || len(models) == 0 {
		if err != nil {
			// Do NOT log the token or full URL query; just the failure.
			s.logger.Warn("copilot HTTP model discovery failed, serving static fallback", "err", err.Error())
		}
		return cliModelResult{fallback: true}
	}
	return cliModelResult{models: dedupeModels(models), fallback: false}
}

// runCopilotSDKHelper executes the SDK helper and returns its stdout. A
// package-level seam (mirroring healthAgentStatuses) so unit tests can
// substitute helper output without running node or the real CLI.
var runCopilotSDKHelper = execCopilotSDKHelper

// probeCopilotModelsSDK runs the SDK helper under its own timeout and parses
// the resulting model ids. token may be empty — the CLI then resolves stored
// device-flow auth on its own, which is exactly the hardening this path adds.
func (s *Server) probeCopilotModelsSDK(token string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), copilotSDKProbeTimeout)
	defer cancel()
	out, err := runCopilotSDKHelper(ctx, token)
	if err != nil {
		return nil, err
	}
	return parseCopilotSDKModels(out)
}

// execCopilotSDKHelper is the real helper runner: `node copilot-models.mjs`
// with the inherited environment plus the agent HOME (stored CLI auth) and,
// when known, the device-flow token. Skips instantly when the helper is not
// installed (dev machines, unit tests).
func execCopilotSDKHelper(ctx context.Context, token string) ([]byte, error) {
	if _, err := os.Stat(copilotSDKHelperPath); err != nil {
		return nil, fmt.Errorf("sdk helper not installed: %w", err)
	}
	cmd := exec.CommandContext(ctx, copilotSDKNodeBinary, copilotSDKHelperPath)
	// os/exec documents that for duplicate keys the LAST value wins, so
	// appending overrides the inherited values.
	env := os.Environ()
	if st, err := os.Stat(copilotSDKAgentHome); err == nil && st.IsDir() {
		env = append(env, "HOME="+copilotSDKAgentHome)
	}
	if token != "" {
		// The copilot CLI honors COPILOT_GITHUB_TOKEN; the device-flow token
		// may exist only in the agent manager's memory. Secret — never logged.
		env = append(env, "COPILOT_GITHUB_TOKEN="+token)
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > copilotSDKStderrLogLimit {
			msg = msg[:copilotSDKStderrLogLimit]
		}
		return nil, fmt.Errorf("sdk helper: %w (stderr: %s)", err, msg)
	}
	return stdout.Bytes(), nil
}

// copilotSDKModel mirrors one entry of the helper's stdout JSON. The helper
// also emits name/efforts/defaultEffort for future use; discovery only needs
// the id and the policy gate today.
type copilotSDKModel struct {
	ID          string `json:"id"`
	PolicyState string `json:"policyState"`
}

// parseCopilotSDKModels decodes the helper output ({"models":[...]}) and
// returns usable model ids: models carrying an explicit policy state other
// than "enabled" are excluded, as is the "auto" pseudo-entry — the dropdown
// pipeline expects real catalog ids and re-appends the auto sentinel itself
// via copilotAlwaysIncludeModels (matching the raw-HTTP probe, whose catalog
// never returns "auto").
func parseCopilotSDKModels(data []byte) ([]string, error) {
	var body struct {
		Models []copilotSDKModel `json:"models"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("parse sdk helper output: %w", err)
	}
	var out []string
	for _, m := range body.Models {
		if m.ID == "" || m.ID == copilotAutoModel {
			continue
		}
		if m.PolicyState != "" && m.PolicyState != copilotPolicyStateEnabled {
			continue
		}
		out = append(out, m.ID)
	}
	return out, nil
}

// copilotToken resolves the Copilot GitHub OAuth token from the agent manager
// (device-flow login) or the COPILOT_GITHUB_TOKEN env var. Secret — never log.
func (s *Server) copilotToken() string {
	if s.deps != nil && s.deps.AgentMgr != nil {
		if t := s.deps.AgentMgr.CopilotToken(); t != "" {
			return t
		}
	}
	return os.Getenv("COPILOT_GITHUB_TOKEN")
}

// copilotAPIHost resolves the per-account Copilot API host from
// copilot_internal/user (enterprise plans use api.enterprise.githubcopilot.com,
// not the public host). Falls back to the public host on any failure.
func (s *Server) copilotAPIHost(token string) string {
	client := &http.Client{Timeout: cliModelQueryTimeout}
	req, err := http.NewRequest(http.MethodGet, copilotUserEndpointURL, nil)
	if err != nil {
		return copilotDefaultAPIHost
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Editor-Version", copilotEditorVersion)
	resp, err := client.Do(req)
	if err != nil {
		return copilotDefaultAPIHost
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return copilotDefaultAPIHost
	}
	host, ok := parseCopilotAPIHost(resp.Body)
	if !ok || host == "" {
		return copilotDefaultAPIHost
	}
	return strings.TrimRight(host, "/")
}

// parseCopilotAPIHost extracts endpoints.api from a copilot_internal/user body.
func parseCopilotAPIHost(r io.Reader) (string, bool) {
	var body struct {
		Endpoints struct {
			API string `json:"api"`
		} `json:"endpoints"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return "", false
	}
	if body.Endpoints.API == "" {
		return "", false
	}
	return body.Endpoints.API, true
}

// fetchCopilotModels performs the GET <host>/models call and returns the
// chat-capable model ids.
func fetchCopilotModels(modelsURL, token, integrationID string) ([]string, error) {
	client := &http.Client{Timeout: cliModelQueryTimeout}
	req, err := http.NewRequest(http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Copilot-Integration-Id", integrationID)
	req.Header.Set("Editor-Version", copilotEditorVersion)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	return parseCopilotModelsResponse(resp.Body)
}

// parseCopilotModelsResponse decodes the Copilot /models body and returns the
// ids of chat-capable models. The response shape is:
//
//	{"data":[{"id":"gpt-4o","capabilities":{"type":"chat"},"model_picker_enabled":bool}, ...]}
//
// It filters to capabilities.type == "chat" (skipping embeddings, etc.). When
// no item carries a capabilities.type at all (older/edge responses), it falls
// back to returning every non-empty id rather than an empty list.
func parseCopilotModelsResponse(r io.Reader) ([]string, error) {
	var body struct {
		Data []struct {
			ID           string `json:"id"`
			Capabilities struct {
				Type string `json:"type"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, err
	}
	var chat, any []string
	sawType := false
	for _, m := range body.Data {
		if m.ID == "" {
			continue
		}
		any = append(any, m.ID)
		if m.Capabilities.Type != "" {
			sawType = true
			if m.Capabilities.Type == "chat" {
				chat = append(chat, m.ID)
			}
		}
	}
	if sawType {
		return chat, nil
	}
	return any, nil
}

// --- Gemini discovery ---

// discoverGeminiModels lists content-generation models for the configured
// Gemini API key. Best-effort: no key or a failed call → fallback=true.
func (s *Server) discoverGeminiModels() cliModelResult {
	key := geminiAPIKey()
	if key == "" {
		return cliModelResult{fallback: true}
	}
	models, err := fetchGeminiModels(key)
	if err != nil || len(models) == 0 {
		if err != nil {
			s.logger.Warn("gemini model discovery failed", "err", err.Error())
		}
		return cliModelResult{fallback: true}
	}
	return cliModelResult{models: dedupeModels(models), fallback: false}
}

// geminiAPIKey resolves the Gemini API key from the standard env vars. Secret.
func geminiAPIKey() string {
	for _, v := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_API_KEY"} {
		if k := os.Getenv(v); k != "" {
			return k
		}
	}
	return ""
}

// fetchGeminiModels calls the v1beta/models list endpoint and returns the ids
// (with the "models/" prefix stripped) of models that support generateContent.
func fetchGeminiModels(apiKey string) ([]string, error) {
	client := &http.Client{Timeout: cliModelQueryTimeout}
	url := fmt.Sprintf("%s?pageSize=%d", geminiModelsURL, geminiModelsPageSize)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Pass the key via header (not the query string) so it never lands in a
	// URL that might be logged upstream.
	req.Header.Set("x-goog-api-key", apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	return parseGeminiModelsResponse(resp.Body)
}

// parseGeminiModelsResponse decodes the Gemini models list, keeping only
// models that support generateContent and stripping the "models/" name prefix.
func parseGeminiModelsResponse(r io.Reader) ([]string, error) {
	var body struct {
		Models []struct {
			Name                       string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range body.Models {
		if m.Name == "" {
			continue
		}
		if !contains(m.SupportedGenerationMethods, geminiGenerateMethod) {
			continue
		}
		out = append(out, strings.TrimPrefix(m.Name, "models/"))
	}
	return out, nil
}

// --- Goose discovery ---

// discoverGooseModels returns models for goose's configured provider. Goose has
// no stable machine-readable "list models for my configured provider" command
// across providers, so this maps the configured provider (via GOOSE_PROVIDER /
// GOOSE_MODEL env, honoring the pinned model) to a static per-provider list,
// or "default" when unconfigured. Marked fallback=true since it is not a live
// provider query.
func (s *Server) discoverGooseModels() cliModelResult {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("GOOSE_PROVIDER")))
	var models []string
	if list, ok := gooseProviderStaticModels[provider]; ok {
		models = append(models, list...)
	}
	// If a specific model is pinned via GOOSE_MODEL, surface it first.
	if m := strings.TrimSpace(os.Getenv("GOOSE_MODEL")); m != "" {
		models = append([]string{m}, models...)
	}
	if len(models) == 0 {
		models = []string{"default"}
	}
	// Always fallback=true: this is a curated per-provider list, not a live
	// enumeration of the provider's catalog.
	return cliModelResult{models: dedupeModels(models), fallback: true}
}

// --- helpers ---

// dedupeModels removes duplicate ids while preserving first-seen order.
func dedupeModels(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, m := range in {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
