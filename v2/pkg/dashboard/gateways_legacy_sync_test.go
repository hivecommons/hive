package dashboard

// Regression tests for the post-rotation key split-brain on the DISCOVERY
// side: rotating the litellm key via the Model Gateways tab wrote only the
// gateway's key file, leaving the legacy litellm: section's file holding the
// revoked key. Model discovery (inferenceAPIKey) resolved the legacy file and
// 401'd — logged as "no models found from any endpoint" and surfaced as a
// sticky error toast — while the save-time probe used the submitted key and
// reported the models fine. Inference-time forwarding was already fixed to
// resolve through the gateway (ResolveLiteLLMInferenceKey); these tests pin
// discovery to the same contract and pin the upsert-side legacy-store sync.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// inferenceAPIKey must resolve the same key the inference/entitlement paths
// use — the explicit gateway's key file — never the stale legacy file.
func TestInferenceAPIKeyPrefersGatewayKey(t *testing.T) {
	s, _ := apiServer(t)
	dir := t.TempDir()
	gwKey := filepath.Join(dir, "gateway_litellm_api_key")
	if err := os.WriteFile(gwKey, []byte("sk-gateway-FRESH\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyKey := filepath.Join(dir, "litellm_api_key")
	if err := os.WriteFile(legacyKey, []byte("sk-legacy-STALE\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	gov := &s.deps.Config.Governor
	gov.LiteLLM.Endpoint = "https://litellm.example.com"
	gov.LiteLLM.APIKeyFile = legacyKey
	gov.Gateways = []config.GatewayConfig{{
		Name:       "litellm",
		Kind:       config.GatewayKindLiteLLM,
		Endpoint:   "https://litellm.example.com",
		APIKeyFile: gwKey,
	}}

	if got := s.inferenceAPIKey("litellm"); got != "sk-gateway-FRESH" {
		t.Fatalf("inferenceAPIKey = %q, want the gateway's fresh key (the stale legacy key is what caused the post-rotation 'no models found' sticky toast)", got)
	}
}

// A gateway entry that resolves no key of its own (endpoint-only, key kept in
// the classic litellm: block) must keep falling back to the legacy store —
// pinned by TestInferenceModelsNonWatsonxUsesRawKeyPath too; this is the
// direct-unit twin.
func TestInferenceAPIKeyKeylessGatewayFallsBackToLegacy(t *testing.T) {
	s, _ := apiServer(t)
	dir := t.TempDir()
	legacyKey := filepath.Join(dir, "litellm_api_key")
	if err := os.WriteFile(legacyKey, []byte("sk-legacy-ONLY\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	gov := &s.deps.Config.Governor
	gov.LiteLLM.Endpoint = "https://litellm.example.com"
	gov.LiteLLM.APIKeyFile = legacyKey
	gov.Gateways = []config.GatewayConfig{{
		Name:     "litellm",
		Kind:     config.GatewayKindLiteLLM,
		Endpoint: "https://litellm.example.com",
	}}

	if got := s.inferenceAPIKey("litellm"); got != "sk-legacy-ONLY" {
		t.Fatalf("inferenceAPIKey = %q, want the legacy fallback for a keyless gateway", got)
	}
}

// Upserting the litellm gateway with a new key must also sync the legacy
// litellm: key store, so pre-gateways resolution paths never see a revoked key.
func TestGatewaysUpsertSyncsLegacyLiteLLMKey(t *testing.T) {
	s, _ := apiServer(t)
	allowPrivateURLHostsForTest(t, "litellm.example")

	oldDir := gatewaySecretsDir
	gatewaySecretsDir = t.TempDir()
	defer func() { gatewaySecretsDir = oldDir }()

	oldLegacy := writableLiteLLMKeyFile
	writableLiteLLMKeyFile = filepath.Join(t.TempDir(), "litellm_api_key")
	defer func() { writableLiteLLMKeyFile = oldLegacy }()

	// The legacy section points at the same proxy the gateway names.
	s.deps.Config.Governor.LiteLLM.Endpoint = "https://litellm.example"

	rec := doPut(s, "/api/config/governor/gateways", map[string]interface{}{
		"name":     "litellm",
		"kind":     "litellm",
		"endpoint": "https://litellm.example",
		"api_key":  "sk-rotated-NEW",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert = %d: %s", rec.Code, rec.Body.String())
	}

	data, err := os.ReadFile(writableLiteLLMKeyFile)
	if err != nil {
		t.Fatalf("legacy key store not synced: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "sk-rotated-NEW" {
		t.Fatalf("legacy key store holds %q, want the rotated key", got)
	}
	if got := s.deps.Config.Governor.LiteLLM.APIKeyFile; got != writableLiteLLMKeyFile {
		t.Fatalf("legacy api_key_file = %q, want %q (must repoint at the synced store)", got, writableLiteLLMKeyFile)
	}
}

// When the legacy litellm: endpoint is EMPTY (the most common upgrade path
// from pre-gateway hives), legacyLiteLLMSectionMatches falls back to matching
// the gateway name "litellm" (case-insensitive). Upserting that gateway must
// still sync the legacy key store.
func TestGatewaysUpsertSyncsLegacyKeyByNameFallback(t *testing.T) {
	s, _ := apiServer(t)
	allowPrivateURLHostsForTest(t, "litellm.internal")

	oldDir := gatewaySecretsDir
	gatewaySecretsDir = t.TempDir()
	defer func() { gatewaySecretsDir = oldDir }()

	oldLegacy := writableLiteLLMKeyFile
	writableLiteLLMKeyFile = filepath.Join(t.TempDir(), "litellm_api_key")
	defer func() { writableLiteLLMKeyFile = oldLegacy }()

	// Legacy endpoint is empty — the default for hives that never configured
	// an explicit litellm: block and only ever set up a gateway via the UI.
	s.deps.Config.Governor.LiteLLM.Endpoint = ""

	rec := doPut(s, "/api/config/governor/gateways", map[string]interface{}{
		"name":     "litellm",
		"kind":     "litellm",
		"endpoint": "https://litellm.internal",
		"api_key":  "sk-name-fallback-KEY",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert = %d: %s", rec.Code, rec.Body.String())
	}

	data, err := os.ReadFile(writableLiteLLMKeyFile)
	if err != nil {
		t.Fatalf("legacy key store not synced via name fallback: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "sk-name-fallback-KEY" {
		t.Fatalf("legacy key store holds %q, want the rotated key via name-fallback path", got)
	}
	if got := s.deps.Config.Governor.LiteLLM.APIKeyFile; got != writableLiteLLMKeyFile {
		t.Fatalf("legacy api_key_file = %q, want %q", got, writableLiteLLMKeyFile)
	}
}

// When the legacy endpoint is empty and the gateway name is NOT "litellm",
// the name-fallback must NOT match — the legacy store stays untouched.
func TestGatewaysUpsertNoNameFallbackForNonLiteLLMName(t *testing.T) {
	s, _ := apiServer(t)
	allowPrivateURLHostsForTest(t, "openrouter.example")

	oldDir := gatewaySecretsDir
	gatewaySecretsDir = t.TempDir()
	defer func() { gatewaySecretsDir = oldDir }()

	oldLegacy := writableLiteLLMKeyFile
	writableLiteLLMKeyFile = filepath.Join(t.TempDir(), "litellm_api_key")
	defer func() { writableLiteLLMKeyFile = oldLegacy }()

	// Legacy endpoint empty, but gateway name is NOT "litellm".
	s.deps.Config.Governor.LiteLLM.Endpoint = ""

	rec := doPut(s, "/api/config/governor/gateways", map[string]interface{}{
		"name":     "openrouter",
		"kind":     "litellm",
		"endpoint": "https://openrouter.example/v1",
		"api_key":  "sk-openrouter-key",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(writableLiteLLMKeyFile); !os.IsNotExist(err) {
		t.Fatalf("legacy litellm key store written by a non-'litellm'-named gateway when legacy endpoint is empty (err=%v)", err)
	}
}

// A non-litellm gateway (different kind) must NOT touch the legacy store.
func TestGatewaysUpsertLeavesLegacyStoreAloneForOtherKinds(t *testing.T) {
	s, _ := apiServer(t)
	allowPrivateURLHostsForTest(t, "other.example")

	oldDir := gatewaySecretsDir
	gatewaySecretsDir = t.TempDir()
	defer func() { gatewaySecretsDir = oldDir }()

	oldLegacy := writableLiteLLMKeyFile
	writableLiteLLMKeyFile = filepath.Join(t.TempDir(), "litellm_api_key")
	defer func() { writableLiteLLMKeyFile = oldLegacy }()

	rec := doPut(s, "/api/config/governor/gateways", map[string]interface{}{
		"name":     "othergw",
		"kind":     "custom",
		"endpoint": "https://other.example/v1",
		"api_key":  "sk-custom-key",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(writableLiteLLMKeyFile); !os.IsNotExist(err) {
		t.Fatalf("legacy litellm key store written by a custom-kind gateway upsert (err=%v)", err)
	}
}
