// Inference and backend configuration: gateways and API-key resolution,
// secret-file path allow-listing, reviewer endpoint resolution, inference
// auth, Bob and LiteLLM backends, and the inference/CLI backend catalogs.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// GatewayConfig is one named, OpenAI-compatible model gateway. A hive may
// configure several at once (e.g. OpenRouter for public models plus a private
// LiteLLM proxy for internal ones); each agent picks one by naming it as its
// backend. Secrets are referenced by env-var NAME or file PATH only — never
// inlined — matching the LiteLLM/inference key-handling rule elsewhere.
type GatewayConfig struct {
	// Name is the unique gateway id an agent names as its backend (e.g.
	// "openrouter", "corp-litellm"). Must be non-empty and unique per hive.
	Name string `yaml:"name" json:"name"`
	// Kind drives preset defaults + labeling: openrouter | litellm | vllm |
	// llm-d | watsonx | custom. Purely descriptive at runtime (all are
	// OpenAI-compatible); the endpoint is what actually routes. The one
	// exception is "watsonx", which additionally needs an IAM-minted bearer
	// (not the raw key) and a project_id header — see ProjectID below and the
	// watsonx token minter (pkg/watsonx).
	Kind string `yaml:"kind" json:"kind,omitempty"`
	// Endpoint is the OpenAI-compatible base URL, e.g. https://openrouter.ai/api/v1.
	// For a watsonx gateway this is the model-gateway base
	// https://<region>.ml.cloud.ibm.com/ml/gateway — hive appends /v1/models and
	// /v1/chat/completions to reach watsonx's OpenAI-compatible surface
	// (.../ml/gateway/v1/models, .../ml/gateway/v1/chat/completions).
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// APIKeyEnv / APIKeyFile resolve this gateway's key (env var NAME and/or file
	// PATH — never the value). Empty is allowed for keyless endpoints (some vLLM).
	// For watsonx this resolves the IBM Cloud API key that is exchanged for a
	// short-lived IAM token; the raw key is never sent as the bearer.
	APIKeyEnv  string `yaml:"api_key_env" json:"api_key_env,omitempty"`
	APIKeyFile string `yaml:"api_key_file" json:"api_key_file,omitempty"`
	// DefaultModel is used when an agent routed through this gateway selects none.
	DefaultModel string `yaml:"default_model" json:"default_model,omitempty"`
	// CABundle is an optional PEM path for a private CA (never disables verify).
	CABundle string `yaml:"ca_bundle" json:"ca_bundle,omitempty"`
	// ProjectID is the watsonx project (or space) id an OpenAI client does not
	// send but watsonx requires for billing/limits. It is sent as the
	// X-IBM-Project-ID header on outbound requests to a watsonx gateway. Only
	// meaningful for Kind == watsonx; omitempty keeps existing gateways
	// byte-identical in hive.yaml. Not a secret (an identifier, not a
	// credential), so unlike the key it is stored inline.
	ProjectID string `yaml:"project_id,omitempty" json:"project_id,omitempty"`
	// Region is the watsonx region slug (e.g. us-south, eu-de, jp-tok) the UI
	// preset uses to build the endpoint template. Purely a convenience for the
	// preset; Endpoint is authoritative. Only meaningful for Kind == watsonx.
	Region string `yaml:"region,omitempty" json:"region,omitempty"`
	// KeyName is an optional, human-chosen LABEL for this gateway's configured
	// key ("Team inference key", "andy personal", …). It is safe-to-show
	// metadata, NOT a secret — it records WHICH key a gateway is set to use so
	// managers can tell keys apart without ever seeing the value. Mirrors the
	// bob key's KeyName (#3596/#3598), now generalized to every gateway kind
	// (litellm / openrouter / watsonx / vllm). omitempty keeps hive.yaml on
	// existing gateways (which never recorded a name) byte-identical on
	// round-trip; an absent name is a normal, backwards-compatible state the
	// dashboard renders as "(unnamed)" rather than an error.
	KeyName string `yaml:"key_name,omitempty" json:"key_name,omitempty"`
}

// gatewayKind values.
const (
	GatewayKindOpenRouter = "openrouter"
	GatewayKindGroq       = "groq"
	GatewayKindLiteLLM    = "litellm"
	GatewayKindVLLM       = "vllm"
	GatewayKindLLMD       = "llm-d"
	GatewayKindWatsonx    = "watsonx"
	GatewayKindCustom     = "custom"

	// legacyLiteLLMGatewayName is the name of the implicit gateway synthesized
	// from the legacy Governor.LiteLLM block when no gateways are configured. It
	// matches the historical "litellm" agent backend so existing agents route
	// unchanged.
	legacyLiteLLMGatewayName = "litellm"
)

// ResolvedGateways returns the effective gateway list: the explicitly-configured
// Gateways when present, otherwise a single implicit gateway synthesized from the
// legacy LiteLLM block (only if that block has an endpoint). This is what lets a
// hive with no `gateways:` and a classic `litellm:` block keep working, while a
// hive that lists gateways gets exactly those.
func (g GovernorConfig) ResolvedGateways() []GatewayConfig {
	if len(g.Gateways) > 0 {
		return g.Gateways
	}
	if g.LiteLLM.Endpoint == "" {
		return nil
	}
	return []GatewayConfig{{
		Name:         legacyLiteLLMGatewayName,
		Kind:         GatewayKindLiteLLM,
		Endpoint:     g.LiteLLM.Endpoint,
		APIKeyEnv:    g.LiteLLM.APIKeyEnv,
		APIKeyFile:   g.LiteLLM.APIKeyFile,
		DefaultModel: g.LiteLLM.DefaultModel,
		CABundle:     g.LiteLLM.CABundle,
	}}
}

// ResolveGateway looks up a gateway by name in the resolved list. An empty name
// returns the FIRST resolved gateway (the default), so an inference agent that
// names no specific gateway routes through the default one. Returns nil if no
// gateway matches (or none are configured).
func (g GovernorConfig) ResolveGateway(name string) *GatewayConfig {
	gws := g.ResolvedGateways()
	if len(gws) == 0 {
		return nil
	}
	if name == "" {
		gw := gws[0]
		return &gw
	}
	for i := range gws {
		if strings.EqualFold(gws[i].Name, name) {
			gw := gws[i]
			return &gw
		}
	}
	return nil
}

// ResolveLiteLLMInferenceKey resolves the API key an agent should present when
// its inference routes through the legacy "litellm" backend. It MUST agree with
// the key the entitlement/probe path validates (dashboard gateways.go/cost.go/
// openrouter.go, which use ResolveGateway(name).ResolveAPIKey()) — otherwise a
// key rotation performed via the Model Gateways tab updates only the gateway key
// file, entitlement passes, but inference keeps sending the stale legacy key and
// 401s.
//
// Resolution rule:
//   - When an EXPLICIT `gateways:` block is configured, resolve the key from the
//     gateway matching this backend (its own api_key_file — the file the Model
//     Gateways tab writes), exactly as entitlement does. One source, no drift.
//   - When NO explicit gateways are configured, fall back to the legacy
//     Governor.LiteLLM resolver, which consults the k8s Secret mount and PVC copy
//     in addition to api_key_file. The synthetic gateway from ResolvedGateways
//     lacks that multi-location fallback, so we must not use it here — preserving
//     today's behavior for classic single-`litellm:`-block hives.
//
// backend is the agent's backend name (typically "litellm"); it selects which
// explicit gateway to consult.
func (g GovernorConfig) ResolveLiteLLMInferenceKey(backend string) string {
	if len(g.Gateways) > 0 {
		if gw := g.ResolveGateway(backend); gw != nil {
			return gw.ResolveAPIKey()
		}
	}
	return g.LiteLLM.ResolveAPIKey()
}

// ResolveAPIKey returns this gateway's key value, preferring the env var when
// set and falling back to the file. Returns "" when neither yields a value
// (a keyless endpoint). Mirrors LiteLLMConfig key resolution.
// secretFileRoots are the ONLY directories an api_key_file may live under.
//
//   - /secrets      — the read-only Kubernetes Secret projection
//   - /data/secrets — the PVC-backed dir the dashboard writes UI-entered keys to
//
// Anything else is refused. See SecretFilePathAllowed.
//
// A package var, not a const slice, so tests can point it at a t.TempDir()
// (production never reassigns it). Use SetSecretFileRootsForTest.
var secretFileRoots = []string{"/secrets", WritableSecretsDir}

// SetSecretFileRootsForTest overrides the allowed secret-file roots and returns
// a function restoring the previous value. Intended for tests, which must write
// key files into a t.TempDir() rather than the real /secrets.
//
// This is not a security hole: it is compiled into the binary but never called
// outside tests, and anyone able to call it already has in-process code
// execution — at which point they can read the files directly.
func SetSecretFileRootsForTest(roots ...string) func() {
	prev := secretFileRoots
	secretFileRoots = roots
	return func() { secretFileRoots = prev }
}

// AllowSecretFileRoot registers an ADDITIONAL directory whose files may be read
// as a secret, returning a function that removes it again.
//
// This exists so a component that WRITES key files keeps its write location and
// this package's READ gate in lockstep. pkg/dashboard writes per-gateway keys to
// its own gatewaySecretsDir — a package var tests repoint at a temp dir — and
// without this the two seams disagree: the dashboard writes a key it can then
// never read back. Callers register the same directory they write to, so the
// gate stays a real confinement rather than something each test disables.
func AllowSecretFileRoot(dir string) func() {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return func() {}
	}
	prev := secretFileRoots
	secretFileRoots = append(append([]string(nil), secretFileRoots...), dir)
	return func() { secretFileRoots = prev }
}

// SecretFilePathAllowed reports whether p is a path hive may read a secret from.
//
// SECURITY (audit N8, CWE-200/918): api_key_file is attacker-controllable — the
// gateway upsert stores whatever absolute path it is given, and the save-time
// probe then reads that file and ships its contents to the gateway endpoint as
// an `Authorization: Bearer` header. Without confinement that is an arbitrary
// file read wired directly to an arbitrary outbound request: /data/secrets/*.pem
// (the GitHub App key), /proc/self/environ, token caches.
//
// The check is a prefix test against secretFileRoots, applied AFTER resolving
// symlinks, so a symlink inside an allowed directory cannot point out of it. A
// path that does not exist yet is still validated lexically — the dashboard
// legitimately writes a key file before anything reads it.
func SecretFilePathAllowed(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	clean := filepath.Clean(p)
	// Resolve symlinks when the path (or its parent) exists, so a link planted
	// inside an allowed root cannot escape it. EvalSymlinks fails on a
	// not-yet-created file, in which case the lexical check below still applies.
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = resolved
	} else if resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(clean)); err == nil {
		clean = filepath.Join(resolvedDir, filepath.Base(clean))
	}
	for _, root := range secretFileRoots {
		rootClean := filepath.Clean(root)
		// Resolve the ROOT too. On macOS /var is a symlink to /private/var, so a
		// resolved path under a temp dir would never prefix-match an unresolved
		// root — the comparison has to be symlink-resolved on both sides or it
		// is inconsistent.
		if resolvedRoot, err := filepath.EvalSymlinks(rootClean); err == nil {
			rootClean = resolvedRoot
		}
		if clean == rootClean {
			continue // the directory itself is not a key file
		}
		// The separator suffix keeps "/data/secrets-evil" from matching the
		// "/data/secrets" root.
		if strings.HasPrefix(clean, rootClean+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (gw GatewayConfig) ResolveAPIKey() string {
	if gw.APIKeyEnv != "" {
		if v := os.Getenv(gw.APIKeyEnv); v != "" {
			return v
		}
	}
	if gw.APIKeyFile != "" {
		// SECURITY (audit N8): confine the read to the managed secrets dirs. This
		// gate lives HERE, not only in the HTTP handler, so every caller is
		// covered — including a path that reached hive.yaml some other way (a
		// hand-edited config, an older build, a restored backup).
		if !SecretFilePathAllowed(gw.APIKeyFile) {
			return ""
		}
		if b, err := os.ReadFile(gw.APIKeyFile); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// ResolveReviewer returns the reviewer endpoint, API key, and model, falling
// back to the governor LiteLLM block for any field the trajectory block leaves
// empty. This is what lets the reviewer target a LiteLLM gateway, a vLLM
// server, or an llm-d front interchangeably: it only needs an
// OpenAI-compatible /v1/chat/completions URL. The key value is resolved from
// a file or env var and is never stored in hive.yaml.
func (g *GovernorConfig) ResolveReviewer() (endpoint, apiKey, model string) {
	t := g.Trajectory
	endpoint = strings.TrimRight(strings.TrimSpace(t.Endpoint), "/")
	if endpoint == "" {
		endpoint = g.LiteLLM.ResolveEndpoint()
	}
	apiKey = resolveKeyFromFileThenEnv(t.APIKeyFile, t.APIKeyEnv)
	if apiKey == "" {
		apiKey = g.LiteLLM.ResolveAPIKey()
	}
	model = strings.TrimSpace(t.Model)
	if model == "" {
		model = g.LiteLLM.DefaultModel
	}
	return endpoint, apiKey, model
}

// ReviewerReady reports whether the reviewer has both an endpoint and a model
// resolved — i.e. whether enabling the lane will actually run reviews. The UI
// uses this to distinguish "on and running" from "on but not configured", so
// a safety control never silently no-ops while showing as enabled.
func (g *GovernorConfig) ReviewerReady() bool {
	endpoint, _, model := g.ResolveReviewer()
	return endpoint != "" && model != ""
}

// resolveKeyFromFileThenEnv reads a secret from a file path, then an env var
// NAME, returning "" if neither yields a value. Mirrors the LiteLLM/inference
// resolution order without pulling in defaults.
func resolveKeyFromFileThenEnv(file, env string) string {
	if file != "" {
		if data, err := os.ReadFile(file); err == nil {
			if k := strings.TrimSpace(string(data)); k != "" {
				return k
			}
		}
	}
	if env != "" {
		if k := os.Getenv(env); k != "" {
			return k
		}
	}
	return ""
}

// Discovery-auth defaults for the self-hosted inference backends. Like
// LiteLLM, hive.yaml stores only the env var NAME and/or key FILE PATH —
// never the key value itself (Config.Save() writes the expanded config
// back to disk, so a key value in YAML would be persisted in plaintext).
const (
	// DefaultVLLMAPIKeyEnv is the env var consulted for the vLLM model
	// discovery API key when governor.vllm.api_key_env is not set.
	DefaultVLLMAPIKeyEnv = "HIVE_VLLM_API_KEY"
	// DefaultLLMDAPIKeyEnv is the env var consulted for the llm-d model
	// discovery API key when governor.llm-d.api_key_env is not set.
	DefaultLLMDAPIKeyEnv = "HIVE_LLMD_API_KEY"
)

// InferenceAuthConfig holds optional /v1/models discovery auth for a
// self-hosted inference backend (vllm, llm-d). Plain vLLM/llm-d servers
// need no key, but the configured endpoint may actually be a LiteLLM
// gateway, which entitlement-filters /v1/models per API key and hides
// key-gated models from anonymous callers.
type InferenceAuthConfig struct {
	APIKeyHeader string `yaml:"api_key_header,omitempty" json:"api_key_header,omitempty"` // header NAME the key is sent in (default "Authorization")
	APIKeyEnv    string `yaml:"api_key_env" json:"api_key_env"`                           // env var NAME holding the key; never the key value
	APIKeyFile   string `yaml:"api_key_file" json:"api_key_file"`                         // path to a file holding the key
	Endpoint     string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`             // optional endpoint override for this backend
}

// ResolveAPIKey returns the backend's discovery API key using the
// resolution order: key file (api_key_file) → env var named by
// api_key_env → defaultEnv. Returns "" when no key is configured. The key
// value itself is never stored in hive.yaml.
func (c *InferenceAuthConfig) ResolveAPIKey(defaultEnv string) string {
	// SECURITY (audit N8): same confinement as GatewayConfig.ResolveAPIKey — an
	// api_key_file is only ever read from the managed secrets dirs.
	if c.APIKeyFile != "" && SecretFilePathAllowed(c.APIKeyFile) {
		if data, err := os.ReadFile(c.APIKeyFile); err == nil {
			if key := strings.TrimSpace(string(data)); key != "" {
				return key
			}
		}
	}
	if c.APIKeyEnv != "" {
		if key := os.Getenv(c.APIKeyEnv); key != "" {
			return key
		}
	}
	if defaultEnv != "" {
		return os.Getenv(defaultEnv)
	}
	return ""
}

// LiteLLM key/endpoint resolution defaults. hive.yaml stores only the env
// var NAME and/or key FILE PATH — never the key value itself. Config.Save()
// writes the expanded config back to disk, so a key value stored in YAML
// would be baked into the file in plaintext.
const (
	// DefaultLiteLLMAPIKeyEnv is the env var consulted for the LiteLLM API
	// key when api_key_env is not set in hive.yaml.
	DefaultLiteLLMAPIKeyEnv = "HIVE_LITELLM_API_KEY"
	// DefaultLiteLLMAPIKeyFile is the key file consulted when api_key_file
	// is not set. Matches the /secrets volume used for k8s Secret mounts.
	DefaultLiteLLMAPIKeyFile = "/secrets/litellm_api_key"
	// WritableSecretsDir is the PVC-backed directory where the dashboard
	// persists secret VALUES entered in the UI. Unlike /secrets (a
	// read-only Kubernetes Secret mount), /data is the hive's writable
	// persistent volume, so files written here survive pod restarts and
	// hosted users can set keys without cluster access.
	WritableSecretsDir = "/data/secrets"
	// WritableLiteLLMAPIKeyFile is where the dashboard stores an API key
	// value entered in the LiteLLM config UI. hive.yaml references it via
	// api_key_file; the key value itself never enters hive.yaml or logs.
	WritableLiteLLMAPIKeyFile = WritableSecretsDir + "/litellm_api_key"
	// LiteLLMEndpointEnv overrides governor.litellm.endpoint at runtime
	// (mirrors HIVE_VLLM_ENDPOINT / HIVE_LLMD_ENDPOINT).
	LiteLLMEndpointEnv = "HIVE_LITELLM_ENDPOINT"
)

// Bob (IBM bobshell) API-key resolution. bobshell cannot authenticate in a pod
// any other way: its default flow (W3ID SSO) opens a browser and polls a
// localhost callback port, which in a headless container cannot succeed and
// instead burns a 3-minute timeout per launch. IBM's documented remedy for
// non-interactive sessions is API-key auth. As with LiteLLM, hive.yaml stores
// only the env var NAME and/or key FILE PATH — never the key value, because
// Config.Save() round-trips the config back to disk.
const (
	// BobAPIKeyEnvVar is the env var name bobshell itself reads the key from.
	// Verified against the installed bundle (bobshell 1.0.6 bundle/bob.js):
	// `process.env.BOBSHELL_API_KEY`. This is the name injected INTO the
	// agent's environment, distinct from the hive-side variable below that
	// an operator sets on the hive pod.
	BobAPIKeyEnvVar = "BOBSHELL_API_KEY"

	// DefaultBobAPIKeyEnv is the hive-side env var consulted for the bob API
	// key when governor.bob.api_key_env is not set in hive.yaml.
	DefaultBobAPIKeyEnv = "HIVE_BOB_API_KEY"

	// DefaultBobAPIKeyFile is the key file consulted when api_key_file is not
	// set. Matches the read-only /secrets volume used for k8s Secret mounts.
	DefaultBobAPIKeyFile = "/secrets/bob_api_key"

	// WritableBobAPIKeyFile is the PVC-backed location a hosted operator can
	// write the key to without cluster access, mirroring
	// WritableLiteLLMAPIKeyFile. Referenced by path only; never by value.
	WritableBobAPIKeyFile = WritableSecretsDir + "/bob_api_key"

	// BobAuthTypeEnvVar is the env var bobshell reads to pick its auth type.
	// It is only a FALLBACK DEFAULT, not an override. From bundle/bob.js
	// (bobshell 1.0.6), the auth-dialog preselect is:
	//   let l=null,d=process.env.BOBSHELL_DEFAULT_AUTH_TYPE;
	//   d&&Object.values(fr).includes(d)&&(l=d);
	//   let c=a.findIndex(p=>e.merged.security?.auth?.selectedType
	//         ? p.value===e.merged.security.auth.selectedType
	//         : l ? p.value===l : ...)
	// i.e. a persisted security.auth.selectedType WINS over this env var.
	// An invalid value is a hard error ("Invalid value for
	// BOBSHELL_DEFAULT_AUTH_TYPE"), so it must be one of the `fr` enum values.
	BobAuthTypeEnvVar = "BOBSHELL_DEFAULT_AUTH_TYPE"

	// BobAuthTypeAPIKey selects API-key auth. It is bob's own enum constant
	// `USE_BOBSHELL="api-key"` from bundle/bob.js (the sibling value being
	// `W3ID_SSO="sso"`), so it is dictated by the vendor, not by us. Not a
	// secret — it is the literal string "api-key" and carries no credential.
	BobAuthTypeAPIKey = "api-key"

	// BobAuthMethodFlag is bobshell's `--auth-method` CLI flag. It EXISTS in
	// bobshell 1.0.6 — an earlier fix removed it after concluding from
	// `bob --help` that it did not. That conclusion was wrong: bundle/bob.js
	// registers it and then HIDES it from help output:
	//   t.option("auth-method",{type:"string",...,choices:[fr.W3ID_SSO,fr.USE_BOBSHELL]});
	//   let a=[...,"auth-method"]; a.forEach(c=>t.hide(c));
	// so it is functional but invisible to `--help | grep auth-method`.
	//
	// It is the STRONGEST control available, because it is the only input that
	// beats the persisted settings file. bob stores it as
	// globalThis.authMethodByCliArg, and the settings-normalization step reads:
	//   let n=globalThis.authMethodByCliArg||t.merged.security.auth.selectedType||r;
	// — the CLI arg is consulted FIRST, ahead of the persisted selectedType.
	// It also suppresses the write-back that would otherwise persist a
	// different value (`&&!globalThis.authMethodByCliArg`), so passing it
	// makes hive's choice authoritative without bob rewriting the shared file.
	BobAuthMethodFlag = "--auth-method"

	// BobApprovalModeFlag / BobApprovalModeYolo set bob's tool-approval policy.
	// Verified in bobshell 1.0.6 `bob --help`:
	//   --approval-mode  Set the approval mode: default (prompt for approval),
	//                    auto_edit (auto-approve edit tools), yolo
	//                    (auto-approve all tools)
	//                    [string] [choices: "default", "auto_edit", "yolo"]
	//
	// Without it bob defaults to "default" and the TUI shows
	// `Auto-approve: Off`, so an unattended agent blocks forever on the first
	// tool call — no human is attached to answer the prompt. With
	// `--approval-mode yolo` the TUI shows `Auto-approve: Full` and bob
	// executes shell/edit tools unattended (verified live on a spoke: bob
	// wrote /tmp/_tool.txt with no approval prompt).
	//
	// The named flag is preferred over the equivalent `-y`/`--yolo` boolean
	// because it states the mode at the call site.
	BobApprovalModeFlag = "--approval-mode"
	BobApprovalModeYolo = "yolo"

	// BobTrustFlag marks the agent's workspace as trusted. bobshell 1.0.6
	// otherwise renders "This folder is not trusted. Some features may be
	// disabled." and gates tool availability behind that state. Verified in
	// `bob --help`:
	//   --trust  specify trust level for the current workspace
	//
	// Passing the flag is preferred over seeding $HOME/.bob/trustedFolders.json
	// because the flag is per-launch and stateless: it needs no knowledge of
	// bob's on-disk trust schema, cannot drift when that schema changes, and
	// applies to whatever workdir the agent is launched in. The shared
	// /data/home is used by EVERY bob agent on a hive, so a seeded trust file
	// would also be a fleet-wide mutation of the kind that already caused the
	// selectedType incident (see BobAuthTypeEnvVar).
	BobTrustFlag = "--trust"

	// BobSettingsRelPath is the persisted settings file, relative to $HOME.
	// From bundle/bob.js: `as=".bob"` and
	// `getGlobalSettingsPath(){return fu.join(t.getGlobalGeminiDir(),"settings.json")}`
	// where getGlobalGeminiDir() is `path.join(os.homedir(), as)`.
	// On a hive this resolves to /data/home/.bob/settings.json — a SHARED file,
	// so one agent picking SSO at the prompt re-breaks every other bob agent.
	BobSettingsRelPath = ".bob/settings.json"

	// BobSettingsAuthKey / BobSettingsSelectedTypeKey / BobSettingsEnforcedTypeKey
	// are the nested JSON keys hive owns inside that file. Shape per bundle:
	//   e.merged.security?.auth?.selectedType   // which method is chosen
	//   e.merged.security?.auth?.enforcedType   // FILTERS the option list:
	//     e.merged.security?.auth?.enforcedType && (a=a.filter(p=>p.value===...))
	//   and validation: eEr() errors when enforcedType !== the active type.
	// Setting enforcedType to api-key leaves SSO unselectable, so a stray
	// interactive pick cannot re-break the fleet.
	BobSettingsSecurityKey     = "security"
	BobSettingsAuthKey         = "auth"
	BobSettingsSelectedTypeKey = "selectedType"
	BobSettingsEnforcedTypeKey = "enforcedType"

	// BobSettingsFileMode keeps the shared settings file group-writable: it
	// lives in /data/home/.bob (drwxrwx--- dev:node) and bob runs as agent UIDs
	// in group `node`, which must still be able to write sibling state. Making
	// it read-only is deliberately NOT done — bob calls setValue() on this file
	// during normal startup normalization, and an EACCES there is an unhandled
	// write path, so re-assertion on every launch is the safer self-heal.
	BobSettingsFileMode = 0o664

	// BobSettingsDirMode matches the observed /data/home/.bob (drwxrwx---).
	BobSettingsDirMode = 0o770

	// BobStateDirName is the per-workspace state directory bob creates inside
	// the directory it is launched in (in addition to the shared $HOME/.bob).
	// Its .bob-errors/ subdirectory is the logger target behind bob's
	// "Failed to initialize logger:" message, observed in production at
	// /data/agents/<name>/.bob/.bob-errors/errors-YYYY-MM-DD.log.
	BobStateDirName = ".bob"
)

// BobConfig configures the IBM bobshell ("bob") CLI backend. Only the
// key's LOCATION is stored — never the key itself.
type BobConfig struct {
	APIKeyEnv  string `yaml:"api_key_env" json:"api_key_env,omitempty"`   // env var NAME holding the key; default HIVE_BOB_API_KEY
	APIKeyFile string `yaml:"api_key_file" json:"api_key_file,omitempty"` // path to a file holding the key; default /secrets/bob_api_key
	// KeyName is an optional, human-chosen LABEL for the configured key
	// ("Team inference key", "andy personal", …). It is safe-to-show metadata,
	// NOT a secret — it records WHICH key a hive is set to use so managers can
	// tell keys apart without ever seeing the value (#3596/#3598). omitempty
	// keeps hive.yaml byte-identical on round-trip when unset; an absent name
	// is a normal, backwards-compatible state that the dashboard renders as
	// "(unnamed)" rather than an error.
	KeyName string `yaml:"key_name,omitempty" json:"key_name,omitempty"`
}

// ResolveAPIKey returns the bob API key, or "" when none is configured.
// Key FILES are consulted in priority order — the configured api_key_file,
// then the k8s Secret mount (DefaultBobAPIKeyFile), then the PVC file
// (WritableBobAPIKeyFile) — followed by the env var named by api_key_env and
// finally DefaultBobAPIKeyEnv. This mirrors LiteLLMConfig.ResolveAPIKey so a
// key stays working if hive.yaml is re-seeded and the api_key_file pointer is
// lost, or if the PVC copy is wiped but an admin Secret exists.
func (c *BobConfig) ResolveAPIKey() string {
	key, _ := c.resolveAPIKeyWithSource()
	return key
}

// ResolveAPIKeySource reports WHERE the key was found without exposing the
// value: "file:<path>", "env:<NAME>", or "" when unconfigured. Safe to log
// and safe to return from APIs.
func (c *BobConfig) ResolveAPIKeySource() string {
	_, source := c.resolveAPIKeyWithSource()
	return source
}

func (c *BobConfig) resolveAPIKeyWithSource() (string, string) {
	if c == nil {
		return "", ""
	}
	files := []string{c.APIKeyFile, DefaultBobAPIKeyFile, WritableBobAPIKeyFile}
	seen := map[string]bool{"": true}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		if data, err := os.ReadFile(f); err == nil {
			if key := strings.TrimSpace(string(data)); key != "" {
				return key, "file:" + f
			}
		}
	}
	// TrimSpace the env-var sources too, matching the file branch above. bob
	// does NO trimming of its own — bundle/bob.js reads
	// `process.env.BOBSHELL_API_KEY` verbatim into the Authorization header —
	// and hive delivers the value through `tmux set-environment`, which passes
	// it as a raw exec argument with no shell word-splitting to strip a stray
	// newline. So any trailing whitespace here reaches IBM's API inside the
	// header and 401s, which bob surfaces as a fallback to the SSO flow rather
	// than as an auth error. Cheap to normalize; impossible to debug from the
	// symptom.
	if c.APIKeyEnv != "" {
		if key := strings.TrimSpace(os.Getenv(c.APIKeyEnv)); key != "" {
			return key, "env:" + c.APIKeyEnv
		}
	}
	if key := strings.TrimSpace(os.Getenv(DefaultBobAPIKeyEnv)); key != "" {
		return key, "env:" + DefaultBobAPIKeyEnv
	}
	return "", ""
}

// LiteLLMConfig configures the litellm inference backend: an OpenAI-compatible
// LiteLLM proxy (remote or local) that agents reach through the hive's
// inference translator.
type LiteLLMConfig struct {
	Endpoint     string `yaml:"endpoint"`      // base URL, e.g. https://litellm.example.com
	APIKeyEnv    string `yaml:"api_key_env"`   // env var NAME holding the key; default HIVE_LITELLM_API_KEY
	APIKeyFile   string `yaml:"api_key_file"`  // path to a file holding the key; default /secrets/litellm_api_key
	DefaultModel string `yaml:"default_model"` // model used when an agent has none selected
	CABundle     string `yaml:"ca_bundle"`     // optional PEM path for a private CA (never disables verification)
	LocalProxy   bool   `yaml:"local_proxy"`   // run the bundled litellm binary as a local translator fallback
}

// ResolveAPIKey returns the LiteLLM API key. Key FILES are consulted in
// priority order — the configured api_key_file, then the k8s Secret mount
// (DefaultLiteLLMAPIKeyFile), then the dashboard-written PVC file
// (WritableLiteLLMAPIKeyFile) — followed by the env var named by
// api_key_env and finally DefaultLiteLLMAPIKeyEnv. Returns "" when no key
// is configured. The key value itself is never stored in hive.yaml.
//
// Consulting all three file locations means a key saved via the dashboard
// keeps working even if hive.yaml is reset (e.g. re-seeded from a
// ConfigMap) and the api_key_file pointer is lost, and an admin-managed
// Secret key keeps working if the PVC copy is wiped.
func (c *LiteLLMConfig) ResolveAPIKey() string {
	key, _ := c.resolveAPIKeyWithSource()
	return key
}

// ResolveAPIKeySource reports where ResolveAPIKey found the key without
// exposing the value: "file:<path>", "env:<NAME>", or "" when no key is
// configured. Safe to return from APIs (the dashboard shows it as the
// "Key detected" store).
func (c *LiteLLMConfig) ResolveAPIKeySource() string {
	_, source := c.resolveAPIKeyWithSource()
	return source
}

func (c *LiteLLMConfig) resolveAPIKeyWithSource() (string, string) {
	files := []string{c.APIKeyFile, DefaultLiteLLMAPIKeyFile, WritableLiteLLMAPIKeyFile}
	seen := map[string]bool{"": true}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		// SECURITY (audit N8): only the managed secrets dirs. The two defaults
		// appended above already satisfy this; the gate is for c.APIKeyFile,
		// which is operator/API-supplied.
		if !SecretFilePathAllowed(f) {
			continue
		}
		if data, err := os.ReadFile(f); err == nil {
			if key := strings.TrimSpace(string(data)); key != "" {
				return key, "file:" + f
			}
		}
	}
	if c.APIKeyEnv != "" {
		if key := os.Getenv(c.APIKeyEnv); key != "" {
			return key, "env:" + c.APIKeyEnv
		}
	}
	if key := os.Getenv(DefaultLiteLLMAPIKeyEnv); key != "" {
		return key, "env:" + DefaultLiteLLMAPIKeyEnv
	}
	return "", ""
}

// ResolveEndpoint returns the effective LiteLLM base URL: the
// HIVE_LITELLM_ENDPOINT env var when set, otherwise the YAML endpoint.
func (c *LiteLLMConfig) ResolveEndpoint() string {
	if ep := os.Getenv(LiteLLMEndpointEnv); ep != "" {
		return ep
	}
	return c.Endpoint
}

// Validate checks that the configured endpoint (when set) parses as an
// absolute http(s) URL.
func (c *LiteLLMConfig) Validate() error {
	if c.Endpoint == "" {
		return nil
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return fmt.Errorf("governor.litellm.endpoint %q is not a valid URL: %w", c.Endpoint, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("governor.litellm.endpoint %q must be an absolute http(s) URL", c.Endpoint)
	}
	return nil
}

// InferenceBackends is the canonical list of model-gateway / inference backend
// IDs. It lives in the config package (a leaf in the import graph) so the
// agent and proxy packages can share it without an import cycle
// (proxy → agent → config).
//
// These are NOT agentic CLIs. Every one of them names a model endpoint that
// hive fronts with its own OpenAI-compatible translator and drives with the
// claude CLI; auth is an API key (or, for watsonx, an IAM bearer minted from
// one) supplied by config, never an interactive login.
//
// "watsonx" is here because IBM watsonx.ai is a model gateway in exactly that
// sense. It was previously supported ONLY as a `gateways:` kind and as an
// onboarding UI option, while the agent launcher had no case for it — so
// `backend: watsonx` was accepted by config and then rejected hours later at
// kick time with "unknown backend: watsonx". Anything that enumerates gateway
// backends must read THIS list rather than repeating the members inline.
var InferenceBackends = []string{"vllm", "llm-d", "litellm", "watsonx"}

// IsInferenceBackend returns true if the backend name is a self-hosted
// inference backend rather than a CLI tool.
func IsInferenceBackend(backend string) bool {
	for _, b := range InferenceBackends {
		if b == backend {
			return true
		}
	}
	return false
}

// CLIBackends is the canonical list of agentic-CLI agent backends — backends
// that launch a real coding CLI binary rather than routing to a model gateway.
//
// This exists so the CONFIG VALIDATOR and the LAUNCHER dispatch on one list.
// They used to keep separate inline sets, which is the drift that let
// `backend: watsonx` be accepted at config-set time and then fail at kick time
// with "unknown backend: watsonx". Adding a backend in one place only is now a
// visible omission rather than a silent accept-then-fail.
//
// `agy` is the Antigravity CLI, Google's replacement for the Gemini CLI. Google
// consolidated its CLI tooling under Antigravity at I/O 2026 and Gemini CLI
// STOPPED SERVING personal and Google AI Pro accounts on 2026-06-18; only
// customers holding paid Gemini Code Assist licences can still invoke it. The
// `gemini` entry is therefore retained for those licence holders, but a hive
// whose Google access is a Gemini/AI Pro subscription can only reach Google
// through `agy`. It was already listed in config/backends.conf's
// KNOWN_BACKENDS, so omitting it here reproduced exactly the accept-in-one-
// place drift this list exists to prevent — except inverted: valid in the
// shell config, rejected by the hub.
//
// TestShellAndGoCLIBackendListsAgree (backend_list_parity_test.go) asserts
// this list against config/backends.conf's KNOWN_BACKENDS, with a closed,
// commented set of exceptions (cliBackendExceptions in that file) for the two
// names — litellm, gemini — that are known to belong on only one side. Adding
// a backend here without also updating the shell side (or, if it genuinely
// belongs on only one side, documenting why in cliBackendExceptions) fails
// that test.
var CLIBackends = []string{"claude", "copilot", "goose", "codex", "pi", "bob", "aider", "gemini", "agy", "opencode", "kilo", "muse", "omp"}

// IsCLIBackend returns true if the backend launches an agentic CLI binary.
func IsCLIBackend(backend string) bool {
	for _, b := range CLIBackends {
		if b == backend {
			return true
		}
	}
	return false
}

// SupportedBackends returns every backend name valid independent of this
// hive's configuration: the agentic CLIs plus the model-gateway backends. A
// configured gateway NAME is additionally valid as a backend, but that is
// config-dependent and so is checked separately by the validator.
func SupportedBackends() []string {
	out := make([]string, 0, len(CLIBackends)+len(InferenceBackends))
	out = append(out, CLIBackends...)
	out = append(out, InferenceBackends...)
	return out
}

// ValidateBackend reports whether backend is a usable agent backend for this
// config, returning a descriptive error naming the supported values when it is
// not. An empty backend is valid (it means "the hive default").
//
// This is the single gate the config write path uses so an unsupported backend
// is refused AT SET TIME with a clear message, instead of being persisted and
// surfacing later as an agent that silently never launches.
func (g GovernorConfig) ValidateBackend(backend string) error {
	if backend == "" {
		return nil
	}
	if IsCLIBackend(backend) || IsInferenceBackend(backend) {
		return nil
	}
	for _, gw := range g.ResolvedGateways() {
		if gw.Name != "" && strings.EqualFold(gw.Name, backend) {
			return nil
		}
	}
	var gatewayNames []string
	for _, gw := range g.ResolvedGateways() {
		if gw.Name != "" {
			gatewayNames = append(gatewayNames, gw.Name)
		}
	}
	msg := fmt.Sprintf("unsupported backend %q (supported: %s",
		backend, strings.Join(SupportedBackends(), ", "))
	if len(gatewayNames) > 0 {
		msg += fmt.Sprintf("; or a configured gateway name: %s", strings.Join(gatewayNames, ", "))
	} else {
		msg += "; or the name of a gateway configured under the Model Gateways tab"
	}
	return fmt.Errorf("%s)", msg)
}
