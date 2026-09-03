package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/claude"
)

// This file adds runtime model discovery for the CLI backends (copilot,
// claude, gemini, goose, agy), mirroring the /v1/models discovery already done
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
//   - claude:  GET api.anthropic.com/v1/models. The endpoint accepts BOTH a
//     standard Anthropic API key (x-api-key, preferred when ANTHROPIC_API_KEY
//     is set) and the Claude Code subscription OAuth bearer token stored by
//     the CLI in ~/.claude/.credentials.json (verified live: HTTP 200 with a
//     subscription token). The OAuth token is short-lived (<1 day) and is
//     refreshed ONLY when the claude CLI itself runs, so the prober re-reads
//     the file on every probe, checks expiresAt BEFORE any HTTP call, and
//     NEVER attempts a token refresh itself — rotating the token out from
//     under the CLI would break its login (the hive inotify-watches
//     ~/.claude/). Expired/absent credentials skip straight to the static
//     fallback (kept in sync with CLAUDE_CLI_MODELS in static/index.html).
//   - goose:   provider-configured (Ollama/OpenAI/Anthropic/...). goose
//     >= 1.37 ships an ACP (Agent Client Protocol) agent server (`goose acp`,
//     JSON-RPC over stdio): initialize + session/new returns result.models
//     {currentModelId, availableModels}, sourced from goose's provider
//     inventory, which live-fetches the CONFIGURED provider's own models
//     endpoint. goose resolves ALL provider credentials itself (keyring /
//     secrets.yaml / env) — hive never reads goose secrets. Unconfigured or
//     pre-1.37 goose omits the models key from the session/new result (the
//     clean fallback signal); that, a missing binary, or any error falls
//     back to a curated per-provider static list.
//   - codex:   `codex app-server` (stdio JSON-RPC) answers model/list with
//     the catalog BAKED INTO the installed CLI binary — per-CLI-version, not
//     per-account (the per-account remote_models fetch was removed upstream),
//     and fully unauthenticated. See cli_models_codex.go.
//   - agy:     `agy models` prints the Antigravity catalog available to the
//     signed-in Google account. The probe uses the agents' shared HOME when it
//     exists so the CLI, not hive, owns credential resolution. See
//     cli_models_agy.go.
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
	// bin/copilot-models.mjs, COPYed by src/Dockerfile). The SDK spawns the
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

	// --- Goose (ACP) ---

	// gooseACPBinary and gooseACPSubcommand spawn goose's ACP agent server
	// (`goose acp`, goose >= 1.37): JSON-RPC 2.0, newline-delimited, on stdio.
	gooseACPBinary     = "goose"
	gooseACPSubcommand = "acp"

	// gooseACPProbeTimeout bounds the whole probe (spawn + initialize +
	// session/new + session/close). Measured 0.39s locally against a live
	// Ollama including process spawn; the headroom is for slow remote
	// providers, whose models endpoint goose fetches inline.
	gooseACPProbeTimeout = 10 * time.Second

	// gooseACPProtocolVersion is the ACP protocol version sent in initialize.
	// Verified against goose 1.37.0, which answers protocolVersion 1.
	gooseACPProtocolVersion = 1

	// gooseACPShutdownGrace is how long the probe waits for goose to exit on
	// its own after stdin closes (letting it process the best-effort
	// session/close) before killing it. The process is ALWAYS reaped either
	// way; this only bounds how long a lingering goose can hold the probe.
	gooseACPShutdownGrace = 2 * time.Second

	// gooseACPClientName / gooseACPClientVersion identify this probe in the
	// ACP initialize handshake's clientInfo.
	gooseACPClientName    = "hive-model-probe"
	gooseACPClientVersion = "1.0"

	// gooseACPAgentHome is the shared agent HOME (see copilotSDKAgentHome —
	// same directory, same rationale): goose reads its provider config from
	// ~/.config/goose/config.yaml, so the probe must run under the HOME the
	// agents use to see which provider is configured. Unlike codex, goose
	// needs no isolated HOME. Side-effect: each probe writes a session (plus
	// inventory rows) into goose's sessions.db under this HOME, which is why
	// the probe sends session/close when done (see gooseACPExchange).
	gooseACPAgentHome = "/data/home"

	// gooseACPMaxLineBytes caps a single ACP stdout line. session/new results
	// carry the full model inventory, which can exceed bufio.Scanner's 64 KiB
	// default.
	gooseACPMaxLineBytes = 1 << 20

	// JSON-RPC request ids for the fixed probe sequence.
	gooseACPInitializeID   = 1
	gooseACPSessionNewID   = 2
	gooseACPSessionCloseID = 3

	// --- Claude (Anthropic) ---

	// anthropicModelsURL lists the models available to the presented credential
	// (API key or Claude Code subscription OAuth token — both accepted,
	// verified live).
	anthropicModelsURL = "https://api.anthropic.com/v1/models"

	// anthropicModelsPageLimit is the ?limit= page size for /v1/models. Set to
	// the API maximum so a single call covers the whole catalog (currently
	// ~a dozen ids) without pagination.
	anthropicModelsPageLimit = 1000

	// anthropicAPIVersion is the required anthropic-version header value.
	anthropicAPIVersion = "2023-06-01"

	// anthropicOAuthBeta is the anthropic-beta value sent alongside a Claude
	// Code subscription OAuth bearer token. Currently optional for /v1/models
	// (verified live: 200 without it) but sent anyway for forward
	// compatibility, mirroring what the Claude CLI sends.
	anthropicOAuthBeta = "oauth-2025-04-20"

	// claudeTokenExpirySkew is the safety margin subtracted from the stored
	// token's expiresAt before use: a token within this window of expiring is
	// treated as already expired so we never fire an HTTP call that a few
	// seconds of clock skew or transit time would turn into a 401.
	claudeTokenExpirySkew = 2 * time.Minute
)

// --- Static fallback lists (kept CURRENT — July 2026) ---

// claudeStaticModels is the fallback offered when the Claude models probe
// cannot run (no API key and no fresh OAuth token in the CLI credentials
// file) or fails. Live discovery via api.anthropic.com/v1/models is strongly
// preferred; this is only a floor so the dropdown is never empty. Refreshed
// 2026-08-03 from a live /v1/models response (11 ids, in API order). Keep it
// in sync with CLAUDE_CLI_MODELS in static/index.html. Both the canonical
// ids AND the bare aliases the CLI accepts are included.
var claudeStaticModels = []string{
	"claude-opus-5",
	"claude-sonnet-5",
	"claude-fable-5",
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-sonnet-4-6",
	"claude-opus-4-6",
	"claude-opus-4-5-20251101",
	"claude-haiku-4-5-20251001",
	"claude-sonnet-4-5-20250929",
	"claude-opus-4-1-20250805",
	// Bare aliases the Claude CLI also accepts (see claudeAlwaysIncludeModels).
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
	// The -5 family is DASHED in copilot CLI nomenclature; the 4.x family is
	// DOTTED. Keep in sync with agent.copilotCLIAcceptedModels (#4262).
	"claude-opus-5",
	"claude-sonnet-5",
	"claude-fable-5",
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

// claudeAlwaysIncludeModels are ids that must ALWAYS be offered in the claude
// dropdown, whether the served list came from live discovery or the static
// fallback (mirrors copilotAlwaysIncludeModels). The Anthropic /v1/models
// endpoint never returns the bare aliases, but they are valid `claude --model`
// values, so they are appended to every served claude list — surviving both
// paths and the dashboard's model auto-heal.
var claudeAlwaysIncludeModels = []string{
	"opus",
	"sonnet",
	"haiku",
}

// bobStaticModels is bob's complete model list: exactly one entry, the auto
// sentinel. bob has no model catalog to discover and no usable --model flag
// (see bobAutoModel), so offering anything else would invite a selection that
// either does nothing or breaks inference.
var bobStaticModels = []string{bobAutoModel}

// codexStaticModels is the fallback for the OpenAI Codex CLI, served when the
// `codex app-server` model/list probe cannot run (binary absent — the case on
// hosted spokes) or fails (see cli_models_codex.go). Codex only accepts OpenAI
// models — Claude ids are rejected with a ChatGPT account ("model is not
// supported when using Codex with a ChatGPT account"), so codex must never
// fall through to the copilot list. The catalog is baked into the CLI binary
// (per-CLI-version, not per-account); this snapshot matches codex 0.146.0:
// visible ids in picker order (sol is the default), then the hidden ids —
// hidden entries are still valid --model values (see orderCodexServedModels).
// Keep in sync with CODEX_CLI_MODELS in static/index.html.
var codexStaticModels = []string{
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.2",
	// Hidden in the 0.146.0 picker but still valid --model values.
	"gpt-5.4",
	"gpt-5.4-mini",
	"codex-auto-review",
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
		r = s.discoverClaudeModels()
	case "codex":
		r = s.discoverCodexModels()
	case agyBackendID:
		r = s.discoverAgyModels()
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
	// fallback list (see copilotAlwaysIncludeModels / claudeAlwaysIncludeModels).
	if backend == "copilot" {
		r.models = dedupeModels(append(r.models, copilotAlwaysIncludeModels...))
	}
	if backend == "claude" {
		r.models = dedupeModels(append(r.models, claudeAlwaysIncludeModels...))
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
	case agyBackendID:
		return agyStaticModels
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
		return cliModelResult{models: dedupeModels(canonicalizeCopilotModelIDs(models)), fallback: false}
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
	return cliModelResult{models: dedupeModels(canonicalizeCopilotModelIDs(models)), fallback: false}
}

// canonicalizeCopilotModelIDs maps every discovered Copilot catalog id to the
// nomenclature the copilot CLI's --model flag accepts (#4262): the catalog
// periodically returns ids whose version separator drifts between "." and "-"
// relative to the CLI (e.g. claude-fable.5 vs the CLI-accepted claude-fable-5).
// Normalizing at discovery time keeps the dropdown, the stored selections it
// produces, AND the stabilize/auto-heal retention keys in one canonical
// spelling, so a sample that flips separators can never look like a new model
// appearing plus the old one vanishing (false "model revoked" churn). Unknown
// ids pass through verbatim (see agent.CanonicalizeCopilotModel).
func canonicalizeCopilotModelIDs(in []string) []string {
	out := make([]string, len(in))
	for i, id := range in {
		out[i] = agent.CanonicalizeCopilotModel(id)
	}
	return out
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
	defer closeHTTPBody(resp.Body)
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
	defer closeHTTPBody(resp.Body)
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
	defer closeHTTPBody(resp.Body)
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

// --- Claude (Anthropic) discovery ---

// claudePodCredentialsPath is a var (not a const) solely so tests can point it
// at a temp file (mirroring sharedClaudeCredentialPath in pkg/agent). The
// production value — the hosted-pod home — is never mutated at runtime.
var claudePodCredentialsPath = claude.CredentialsPath

// anthropicModelsEndpoint redirects the models call away from the real
// Anthropic endpoint. TEST SEAM ONLY: it is never changed in production, where
// every probe goes to anthropicModelsURL. It exists so discoverClaudeModels'
// response handling (success, 401, malformed JSON) can be exercised against an
// httptest server without a live API round trip.
var anthropicModelsEndpoint = anthropicModelsURL

// discoverClaudeModels lists the models available to the local Claude
// credential by calling the Anthropic /v1/models endpoint. Credential
// precedence: ANTHROPIC_API_KEY (x-api-key header), else the Claude Code
// subscription OAuth access token from the CLI's credentials file
// (Authorization: Bearer — accepted by the endpoint, verified live).
// Best-effort: no credential, an expired token, or any HTTP/parse failure
// returns fallback=true with an empty list so the caller substitutes the
// static list. The stored token is NEVER refreshed here (see file header).
func (s *Server) discoverClaudeModels() cliModelResult {
	candidates := s.agentClaudeCredentialPaths()
	header, value, ok := claudeAPICredential(candidates)
	if !ok {
		// No API key and no fresh OAuth token. Skip the HTTP call entirely —
		// on copilot-only spokes the credentials file can sit expired for
		// weeks, and refreshing it is forbidden (it would rotate the token
		// out from under the claude CLI's own login).
		//
		// #4699: this return used to be SILENT while the credential-found-
		// but-call-failed path below logged, so from outside the process the
		// two causes of an all-"unverified" dropdown were indistinguishable —
		// and the silent one is the likelier. Naming the paths that were
		// actually stat'ed is the whole diagnostic: it distinguishes "no
		// credential anywhere" from "looked in the wrong home". Paths only —
		// never the token.
		// Guarded: unlike the failure path below, this branch is the COMMON
		// one, so a manager-less embedding with no logger must not start
		// panicking on it.
		if s.logger != nil {
			s.logger.Info("claude model discovery: no usable credential, using the static alias list",
				"paths_tried", strings.Join(claudeCredentialCandidatePaths(candidates), ", "),
				"anthropic_api_key_set", os.Getenv("ANTHROPIC_API_KEY") != "")
		}
		return cliModelResult{fallback: true}
	}
	models, err := fetchClaudeModels(anthropicModelsEndpoint, header, value)
	if err != nil || len(models) == 0 {
		if err != nil {
			// Do NOT log the credential or response body; just the failure.
			s.logger.Warn("claude model discovery failed", "err", err.Error())
		}
		return cliModelResult{fallback: true}
	}
	return cliModelResult{models: dedupeModels(models), fallback: false}
}

// claudeAPICredential resolves the credential for the Anthropic models call as
// an (header name, header value) pair. ANTHROPIC_API_KEY wins when set; else
// the freshest non-expired OAuth access token from the Claude CLI credentials
// file is used as a bearer. ok=false means no usable credential. Secret —
// never log the value.
// agentPaths are per-UID agent credential locations resolved by the agent
// manager; nil is valid and reduces this to the pre-#4699 shared-path probe.
func claudeAPICredential(agentPaths []string) (header, value string, ok bool) {
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		return "x-api-key", k, true
	}
	for _, path := range claudeCredentialCandidatePaths(agentPaths) {
		if tok := readFreshClaudeToken(path); tok != "" {
			return "Authorization", "Bearer " + tok, true
		}
	}
	return "", "", false
}

// agentClaudeCredentialPaths asks the agent manager where the fleet's claude
// credentials actually live, so model discovery resolves them exactly the way
// the auth probe does (#4699). Returns nil when there is no manager — tests and
// any manager-less embedding then keep the previous shared-path-only behavior.
func (s *Server) agentClaudeCredentialPaths() []string {
	if s == nil || s.deps == nil || s.deps.AgentMgr == nil {
		return nil
	}
	return s.deps.AgentMgr.ClaudeCredentialCandidatePaths()
}

// claudeCredentialCandidatePaths returns the credentials-file locations to try.
//
// agentPaths come first (#4699). They are the PER-UID homes the agents' own
// CLIs write to, and on a per-UID fleet they are the only place a token exists:
// this probe previously stat'ed just the two shared locations below, so it
// reported "no credential" — an all-"unverified" model dropdown — on hives
// where pkg/agent's probe simultaneously reported the agent logged in, because
// only that one had been taught about per-UID homes.
//
// Then the hosted-pod shared home (the same /data/home the agent manager uses),
// then $HOME/.claude/.credentials.json for non-hosted spokes. Note that $HOME
// here is the DASHBOARD process's home, which is why it cannot stand in for an
// agent's.
//
// Computed per call — never cached — because the claude CLI rewrites the file
// whenever it runs and the probe must see the freshest token.
func claudeCredentialCandidatePaths(agentPaths []string) []string {
	paths := make([]string, 0, len(agentPaths)+2)
	for _, p := range agentPaths {
		if p != "" && !containsString(paths, p) {
			paths = append(paths, p)
		}
	}
	if !containsString(paths, claudePodCredentialsPath) {
		paths = append(paths, claudePodCredentialsPath)
	}
	if home := os.Getenv("HOME"); home != "" {
		if p := filepath.Join(home, ".claude", ".credentials.json"); !containsString(paths, p) {
			paths = append(paths, p)
		}
	}
	return paths
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// readFreshClaudeToken reads and parses one credentials file and returns its
// OAuth access token ONLY if it is not expired (with claudeTokenExpirySkew of
// margin). Absent file, malformed JSON, missing token, or an expired/expiring
// token all return "" — the caller then falls back rather than firing a
// doomed HTTP call. This deliberately does not reuse claude.ReadAccessToken,
// which applies no skew margin.
func readFreshClaudeToken(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var creds claude.Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return ""
	}
	o := creds.ClaudeAIOAuth
	if o == nil || o.AccessToken == "" {
		return ""
	}
	// expiresAt is a millisecond epoch; treat anything inside the skew window
	// as already expired.
	if o.ExpiresAt > 0 && time.UnixMilli(o.ExpiresAt).Before(time.Now().Add(claudeTokenExpirySkew)) {
		return ""
	}
	return o.AccessToken
}

// fetchClaudeModels performs the GET <endpoint>?limit=N call and returns the
// model ids. authHeader/authValue carry either the API key (x-api-key) or the
// OAuth bearer (Authorization) — see claudeAPICredential.
func fetchClaudeModels(modelsURL, authHeader, authValue string) ([]string, error) {
	client := &http.Client{Timeout: cliModelQueryTimeout}
	url := fmt.Sprintf("%s?limit=%d", modelsURL, anthropicModelsPageLimit)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set(authHeader, authValue)
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	if authHeader == "Authorization" {
		// OAuth (subscription-token) calls also send the oauth beta flag —
		// currently optional for /v1/models, sent for forward compatibility.
		req.Header.Set("anthropic-beta", anthropicOAuthBeta)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer closeHTTPBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	return parseClaudeModelsResponse(resp.Body)
}

// parseClaudeModelsResponse decodes the Anthropic /v1/models body and returns
// the model ids. The response shape is:
//
//	{"data":[{"id":"claude-opus-5","type":"model",...}, ...]}
func parseClaudeModelsResponse(r io.Reader) ([]string, error) {
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range body.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, m.ID)
	}
	return out, nil
}

// --- Goose discovery ---

// errGooseBinaryMissing means goose is not on PATH — today's NORMAL on hosted
// spokes (verified: goose is not installed in the hive pods), so it is an
// expected fallback signal, not a logged failure.
var errGooseBinaryMissing = errors.New("goose binary not on PATH")

// errGooseModelsAbsent means session/new answered WITHOUT a models key: goose
// is unconfigured (no provider) or predates 1.37. This is goose's clean
// "nothing to discover" signal, so it too falls back silently.
var errGooseModelsAbsent = errors.New("goose session/new returned no models (unconfigured or pre-1.37 goose)")

// gooseACPModels is the parsed result.models of an ACP session/new response.
type gooseACPModels struct {
	// CurrentModelID is goose's active model (goose also lists it first in
	// AvailableModels).
	CurrentModelID string
	// AvailableModels are the modelId values of result.models.availableModels.
	AvailableModels []string
}

// runGooseACPProbe is the probe seam (mirroring runCopilotSDKHelper) so unit
// tests can substitute canned inventories without a goose binary.
var runGooseACPProbe = execGooseACPProbe

// discoverGooseModels lists the models goose's CONFIGURED provider serves, by
// driving `goose acp` (goose >= 1.37): initialize + session/new returns
// result.models {currentModelId, availableModels}, backed by goose's
// provider-inventory subsystem, which live-fetches the provider's own models
// endpoint (verified end-to-end against a live Ollama: the exact /api/tags
// inventory, ~0.4s round trip including process spawn).
//
// Design invariants:
//
//   - hive NEVER reads goose's provider credentials — goose resolves them
//     itself (keyring / secrets.yaml / env). That is the whole advantage of
//     this probe over probing providers directly, and it must stay that way.
//   - GOOSE_PROVIDER / GOOSE_MODEL env (inherited by the spawned goose)
//     override the config's active provider/model; a GOOSE_MODEL pin is also
//     surfaced first in the served list.
//   - For providers goose cannot (or is not configured to) refresh — e.g.
//     ollama without OLLAMA_HOST — availableModels is goose's curated catalog
//     rather than live inventory, with no distinguishing flag. Acceptable:
//     still fresher than hive's static map. On a slow remote provider the
//     FIRST call may serve catalog data with the refreshed inventory arriving
//     on the next call; the stabilize() retention absorbs that churn.
//   - Missing binary, absent models key, error, or timeout → empty result →
//     the caller serves the curated per-provider static fallback below.
func (s *Server) discoverGooseModels() cliModelResult {
	ctx, cancel := context.WithTimeout(context.Background(), gooseACPProbeTimeout)
	defer cancel()
	inv, err := runGooseACPProbe(ctx)
	if err == nil && inv != nil && len(inv.AvailableModels) > 0 {
		// Pinned model first: an explicit GOOSE_MODEL env pin wins, else
		// goose's own current model (which goose already orders first).
		pinned := strings.TrimSpace(os.Getenv("GOOSE_MODEL"))
		if pinned == "" {
			pinned = inv.CurrentModelID
		}
		models := inv.AvailableModels
		if pinned != "" {
			models = append([]string{pinned}, models...)
		}
		return cliModelResult{models: dedupeModels(models), fallback: false}
	}
	if err != nil && !errors.Is(err, errGooseBinaryMissing) && !errors.Is(err, errGooseModelsAbsent) {
		// Only our own error text — never session payloads, never secrets.
		s.logger.Info("goose ACP model discovery unavailable, serving static fallback", "err", err.Error())
	}
	return gooseStaticProviderResult()
}

// gooseStaticProviderResult maps the configured provider (GOOSE_PROVIDER env)
// to the curated static list, honoring a GOOSE_MODEL pin first, or ["default"]
// when unconfigured. Always fallback=true: this is a curated list, not a live
// enumeration of the provider's catalog.
func gooseStaticProviderResult() cliModelResult {
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
	return cliModelResult{models: dedupeModels(models), fallback: true}
}

// execGooseACPProbe spawns `goose acp` and runs the fixed ACP exchange. The
// probe runs under the shared agent HOME (gooseACPAgentHome) when it exists so
// goose sees the agents' provider config; otherwise the server's own HOME is
// left in place (dev machines). The spawned process is always reaped.
func execGooseACPProbe(ctx context.Context) (*gooseACPModels, error) {
	bin, err := exec.LookPath(gooseACPBinary)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errGooseBinaryMissing, err.Error())
	}
	cmd := exec.CommandContext(ctx, bin, gooseACPSubcommand)
	// os/exec documents that for duplicate keys the LAST value wins, so
	// appending overrides the inherited HOME. GOOSE_PROVIDER / GOOSE_MODEL
	// pass through via the inherited environment.
	env := os.Environ()
	cwd := os.TempDir()
	if st, statErr := os.Stat(gooseACPAgentHome); statErr == nil && st.IsDir() {
		env = append(env, "HOME="+gooseACPAgentHome)
		cwd = gooseACPAgentHome
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("goose acp stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("goose acp stdout: %w", err)
	}
	// goose logs startup chatter on stderr; it is discarded, never parsed and
	// never logged (it could echo config details).
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("goose acp start: %w", err)
	}
	inv, exchErr := gooseACPExchange(ctx, stdin, stdout, cwd)
	// Closing stdin is the ACP server's shutdown signal (after it drains the
	// best-effort session/close). Give it a short grace to exit on its own,
	// then kill; either way Wait reaps the process. A nonzero exit after a
	// successful exchange is irrelevant.
	_ = stdin.Close()
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(gooseACPShutdownGrace):
		_ = cmd.Process.Kill()
		<-waitDone
	}
	return inv, exchErr
}

// gooseACPRequest writes one newline-delimited JSON-RPC 2.0 request.
func gooseACPRequest(w io.Writer, id int, method string, params any) error {
	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return fmt.Errorf("marshal %s: %w", method, err)
	}
	if _, err := w.Write(append(msg, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", method, err)
	}
	return nil
}

// gooseACPAwait reads newline-delimited JSON-RPC messages until the response
// carrying the given id arrives, skipping notifications and agent-initiated
// requests (anything carrying a method). Honors ctx.
func gooseACPAwait(ctx context.Context, lines <-chan []byte, id int) (json.RawMessage, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("awaiting goose acp response %d: %w", id, ctx.Err())
		case line, ok := <-lines:
			if !ok {
				return nil, fmt.Errorf("goose acp stream ended awaiting response %d", id)
			}
			var msg struct {
				ID     *int            `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(line, &msg); err != nil {
				continue // non-JSON chatter — skip, like notifications
			}
			if msg.Method != "" || msg.ID == nil || *msg.ID != id {
				continue
			}
			if msg.Error != nil {
				return nil, fmt.Errorf("goose acp error %d on request %d: %s", msg.Error.Code, id, msg.Error.Message)
			}
			return msg.Result, nil
		}
	}
}

// gooseACPExchange speaks the fixed probe sequence over an already-connected
// stdio pair (factored off the process plumbing so tests can feed canned
// responses through pipes):
//
//	initialize (protocolVersion 1) → session/new (cwd, no MCP servers) →
//	parse result.models → best-effort session/close.
//
// session/close matters: every session/new persists a session (plus inventory
// rows) into goose's sessions.db under the probe HOME, and closing the session
// (the capability is advertised by goose 1.37) keeps repeated probes from
// accumulating rows. Close failures are ignored — the inventory is already in
// hand.
func gooseACPExchange(ctx context.Context, w io.Writer, r io.Reader, cwd string) (*gooseACPModels, error) {
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), gooseACPMaxLineBytes)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			select {
			case lines <- []byte(line):
			case <-ctx.Done():
				return
			}
		}
	}()

	initParams := map[string]any{
		"protocolVersion": gooseACPProtocolVersion,
		"clientCapabilities": map[string]any{
			// The probe serves no filesystem to the agent.
			"fs": map[string]bool{"readTextFile": false, "writeTextFile": false},
		},
		"clientInfo": map[string]string{"name": gooseACPClientName, "version": gooseACPClientVersion},
	}
	if err := gooseACPRequest(w, gooseACPInitializeID, "initialize", initParams); err != nil {
		return nil, err
	}
	if _, err := gooseACPAwait(ctx, lines, gooseACPInitializeID); err != nil {
		return nil, err
	}

	newParams := map[string]any{"cwd": cwd, "mcpServers": []any{}}
	if err := gooseACPRequest(w, gooseACPSessionNewID, "session/new", newParams); err != nil {
		return nil, err
	}
	result, err := gooseACPAwait(ctx, lines, gooseACPSessionNewID)
	if err != nil {
		return nil, err
	}

	var res struct {
		SessionID string `json:"sessionId"`
		Models    *struct {
			CurrentModelID  string `json:"currentModelId"`
			AvailableModels []struct {
				ModelID string `json:"modelId"`
			} `json:"availableModels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(result, &res); err != nil {
		return nil, fmt.Errorf("parse session/new result: %w", err)
	}

	// Best-effort session/close BEFORE returning, whether or not models were
	// present — session/new persisted a session either way. Fire and forget:
	// no await, and a write failure is ignored.
	if res.SessionID != "" {
		_ = gooseACPRequest(w, gooseACPSessionCloseID, "session/close", map[string]any{"sessionId": res.SessionID})
	}

	if res.Models == nil {
		return nil, errGooseModelsAbsent
	}
	inv := &gooseACPModels{CurrentModelID: res.Models.CurrentModelID}
	for _, m := range res.Models.AvailableModels {
		if m.ModelID == "" {
			continue
		}
		inv.AvailableModels = append(inv.AvailableModels, m.ModelID)
	}
	if len(inv.AvailableModels) == 0 {
		return nil, errGooseModelsAbsent
	}
	return inv, nil
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
