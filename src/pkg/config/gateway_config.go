package config

import (
	"crypto/sha256"
	"encoding/hex"
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

// APIKeySHA256 returns the lowercase SHA-256 hex digest of the exact key
// string Hive will present to an inference gateway. It returns empty for an
// empty key so callers can expose presence/hash without leaking the secret.
func APIKeySHA256(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
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
