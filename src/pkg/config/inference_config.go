package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

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

type LoggingConfig struct {
	Dir        string `yaml:"dir"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxAgeDays int    `yaml:"max_age_days"`
	MaxBackups int    `yaml:"max_backups"`
	Compress   bool   `yaml:"compress"`
	Level      string `yaml:"level"`
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

	// BobV2APIKeyEnvVar is the renamed env var bobshell 2.x reads for the
	// default `provider:"harness"` path. Verified in dist/bob.js 2.0.4:
	// `t.apiKey??process.env.BOB_API_KEY`.
	BobV2APIKeyEnvVar = "BOB_API_KEY"

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
	//
	// bobshell 2.0.4 removed this flag entirely (the only "auth-method" string
	// left in dist/bob.js is ai-gateway-auth-method), so launch code must only
	// pass it to 1.x.
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
	BobAutoApproveFlag  = "--auto-approve"

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

	// BobV2SettingsRelPath is bobshell 2.x's global settings file. From
	// dist/bob.js (2.0.4): getGlobalBobDirectory() is
	// path.join(os.homedir(), ".bob"), getGlobalSettingsDirectory() appends
	// "settings", and the user config joins that directory with "settings.json".
	BobV2SettingsRelPath = ".bob/settings/settings.json"
	BobV2ProviderKey     = "provider"
	BobV2ProviderHarness = "harness"
	BobV2ShellKey        = "bobShell"
	BobV2AutoUpdateKey   = "autoUpdate"

	// BobLegacyGeneralKey and the disable keys cover bob/gemini-lineage 1.x
	// settings shapes. They are harmless for versions that ignore them, and
	// keep auto-update disabled on lagging images while v2 uses
	// bobShell.autoUpdate=false in BobV2SettingsRelPath.
	BobLegacyGeneralKey           = "general"
	BobLegacyDisableAutoUpdateKey = "disableAutoUpdate"
	BobLegacyDisableUpdateNagKey  = "disableUpdateNag"

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
	// SessionPrefix is an optional global prefix for bob --instance-id, used
	// to make Bob/Bobalytics sessions distinguishable without renaming Hive
	// agents. Empty is default-off and preserves the exact launch command.
	SessionPrefix string `yaml:"session_prefix,omitempty" json:"session_prefix,omitempty"`
}

// AgentBobConfig holds per-agent bob backend options.
type AgentBobConfig struct {
	// SessionLabel overrides governor.bob.session_prefix + agent name for Bob
	// --instance-id. It never changes the Hive agent identity.
	SessionLabel string `yaml:"session_label,omitempty" json:"session_label,omitempty"`
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
