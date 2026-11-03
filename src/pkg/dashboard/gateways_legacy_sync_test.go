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

	"github.com/hivecommons/hive/pkg/config"
)

// inferenceAPIKey must resolve the same key the inference/entitlement paths
// use — the explicit gateway's key file — never the stale legacy file.
func TestInferenceAPIKeyPrefersGatewayKey(t *testing.T) {
	s, _ := apiServer(t)
	dir := t.TempDir()
	// v4's audit-N8 gate confines api_key_file reads to the managed secrets
	// dirs; admit the test dir so the temp key files resolve.
	defer config.SetSecretFileRootsForTest(dir)()
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
	// v4's audit-N8 gate confines api_key_file reads to the managed secrets
	// dirs; admit the test dir so the temp key file resolves.
	defer config.SetSecretFileRootsForTest(dir)()
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

// #9945: repointing the built-in "litellm" gateway at a NEW proxy must carry
// the legacy governor.litellm block with it. Leaving the legacy endpoint on
// the OLD proxy is the split that made the Gateways-tab Test pass while agent
// inference sent the new key to the old proxy and got 401.
func TestGatewaysUpsertRepointsLegacyLiteLLMEndpoint(t *testing.T) {
	s, _ := apiServer(t)
	allowPrivateURLHostsForTest(t, "old-gateway.example", "litellm.cb.example")

	oldDir := gatewaySecretsDir
	gatewaySecretsDir = t.TempDir()
	defer func() { gatewaySecretsDir = oldDir }()

	oldLegacy := writableLiteLLMKeyFile
	writableLiteLLMKeyFile = filepath.Join(t.TempDir(), "litellm_api_key")
	defer func() { writableLiteLLMKeyFile = oldLegacy }()

	s.deps.Config.Governor.LiteLLM.Endpoint = "https://old-gateway.example"
	s.deps.Config.Governor.LiteLLM.DefaultModel = "old-model"

	rec := doPut(s, "/api/config/governor/gateways", map[string]interface{}{
		"name":          "litellm",
		"kind":          "litellm",
		"endpoint":      "https://litellm.cb.example",
		"api_key":       "sk-cb-NEW",
		"default_model": "rits/zai-org/glm-5-3",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert = %d: %s", rec.Code, rec.Body.String())
	}

	lc := s.deps.Config.Governor.LiteLLM
	if lc.Endpoint != "https://litellm.cb.example" {
		t.Fatalf("legacy endpoint = %q, want it repointed to the gateway's endpoint", lc.Endpoint)
	}
	if lc.DefaultModel != "rits/zai-org/glm-5-3" {
		t.Fatalf("legacy default model = %q, want the gateway's default model", lc.DefaultModel)
	}
	data, err := os.ReadFile(writableLiteLLMKeyFile)
	if err != nil {
		t.Fatalf("legacy key store not synced: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "sk-cb-NEW" {
		t.Fatalf("legacy key store holds %q, want the new gateway's key", got)
	}
}

// A gateway that is not the built-in "litellm" backend's (a second LiteLLM
// proxy under its own name) must never move the legacy endpoint.
func TestGatewaysUpsertLeavesLegacyEndpointForOtherLiteLLMGateways(t *testing.T) {
	s, _ := apiServer(t)
	allowPrivateURLHostsForTest(t, "old-gateway.example", "second.example")

	oldDir := gatewaySecretsDir
	gatewaySecretsDir = t.TempDir()
	defer func() { gatewaySecretsDir = oldDir }()

	s.deps.Config.Governor.LiteLLM.Endpoint = "https://old-gateway.example"

	rec := doPut(s, "/api/config/governor/gateways", map[string]interface{}{
		"name":     "second-litellm",
		"kind":     "litellm",
		"endpoint": "https://second.example",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert = %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.LiteLLM.Endpoint; got != "https://old-gateway.example" {
		t.Fatalf("legacy endpoint = %q, want it untouched by a differently-named gateway", got)
	}
}

// The legacy block is only editable from the Model Gateways tab (the
// standalone LiteLLM tab is gone), so that tab must render its editor — the
// #9945 reporter was told to set "the legacy endpoint" and found nowhere to.
func TestModelGatewaysTabRendersLegacyLiteLLMEditor(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(jsFunc(t, html, "renderGovGateways"), "renderGovLegacyLiteLLM()") {
		t.Error("renderGovGateways must render the legacy LiteLLM section — the legacy endpoint must be discoverable in Settings → Model Gateways")
	}
	legacy := jsFunc(t, html, "renderGovLegacyLiteLLM")
	for _, want := range []string{"renderGovLiteLLM(", "Legacy LiteLLM endpoint", "governor.litellm"} {
		if !strings.Contains(legacy, want) {
			t.Errorf("renderGovLegacyLiteLLM missing %q", want)
		}
	}
	if strings.Contains(html, "Settings → LiteLLM") {
		t.Error(`index.html still points operators at "Settings → LiteLLM", a tab that no longer exists`)
	}
}
