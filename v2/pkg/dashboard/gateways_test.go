package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// TestGatewaysList_LegacyBackCompat verifies a legacy-only hive (a classic
// litellm block, no explicit gateways) surfaces its synthesized "litellm"
// gateway through GET /gateways, so nothing is lost when the UI replaces the
// single-LiteLLM tab with the manager.
func TestGatewaysList_LegacyBackCompat(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Governor.LiteLLM = config.LiteLLMConfig{
		Endpoint:     "https://litellm.example.com",
		DefaultModel: "gpt-4o",
	}
	req := httptest.NewRequest("GET", "/api/config/governor/gateways", nil)
	w := httptest.NewRecorder()
	srv.handleGovernorGatewaysList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Gateways []map[string]interface{} `json:"gateways"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Gateways) != 1 {
		t.Fatalf("gateways = %d, want 1 (synthesized litellm)", len(resp.Gateways))
	}
	if resp.Gateways[0]["name"] != "litellm" {
		t.Errorf("synthesized gateway name = %v, want litellm", resp.Gateways[0]["name"])
	}
}

// TestGatewaysUpsert_StoresKeyAsFileRef verifies a typed key VALUE is written
// to a secret file and only the PATH lands in config — never the value.
func TestGatewaysUpsert_StoresKeyAsFileRef(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	orig := gatewaySecretsDir
	gatewaySecretsDir = dir
	t.Cleanup(func() { gatewaySecretsDir = orig })

	secret := "s" + "k-super-secret-value-1234567890"
	body := `{"name":"openrouter","kind":"openrouter","endpoint":"https://openrouter.ai/api/v1","api_key":"` + secret + `","default_model":"anthropic/claude-opus-4.8"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/gateways", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleGovernorGatewaysUpsert(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	gws := srv.deps.Config.Governor.Gateways
	if len(gws) != 1 {
		t.Fatalf("gateways = %d, want 1", len(gws))
	}
	gw := gws[0]
	if gw.Name != "openrouter" || gw.Kind != "openrouter" {
		t.Errorf("gateway = %+v", gw)
	}
	// The key VALUE must never be inlined into config.
	if gw.APIKeyEnv == secret || gw.APIKeyFile == secret || strings.Contains(gw.APIKeyFile, "sk-super") {
		t.Fatalf("secret leaked into config: %+v", gw)
	}
	if gw.APIKeyFile == "" || !strings.HasPrefix(gw.APIKeyFile, dir) {
		t.Fatalf("api_key_file = %q, want a path under %s", gw.APIKeyFile, dir)
	}
	// The file must hold the value.
	data, err := os.ReadFile(gw.APIKeyFile)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	if strings.TrimSpace(string(data)) != secret {
		t.Errorf("key file content mismatch")
	}
	// File perms owner-only.
	info, _ := os.Stat(gw.APIKeyFile)
	if info.Mode().Perm() != gatewaySecretFileMode {
		t.Errorf("key file mode = %o, want %o", info.Mode().Perm(), gatewaySecretFileMode)
	}
	// The GET response must not echo the value either.
	if strings.Contains(w.Body.String(), secret) {
		t.Errorf("upsert response echoed the secret value")
	}
}

// TestGatewaysUpsert_RejectsInvalidEndpoint ensures a bad endpoint is a 400 and
// leaves config untouched.
func TestGatewaysUpsert_RejectsInvalidEndpoint(t *testing.T) {
	srv := newFullServer(t)
	body := `{"name":"bad","endpoint":"not-a-url"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/gateways", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleGovernorGatewaysUpsert(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	if len(srv.deps.Config.Governor.Gateways) != 0 {
		t.Errorf("config mutated on validation failure")
	}
}

// TestGatewaysDelete_InUseConflict verifies deleting a gateway an agent
// references returns 409 and lists the agent.
func TestGatewaysDelete_InUseConflict(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Governor.Gateways = []config.GatewayConfig{
		{Name: "openrouter", Kind: "openrouter", Endpoint: "https://openrouter.ai/api/v1"},
	}
	// Point an agent at the gateway.
	srv.deps.Config.Agents["router-agent"] = config.AgentConfig{Backend: "openrouter", Model: "x"}

	req := httptest.NewRequest("DELETE", "/api/config/governor/gateways/openrouter", nil)
	req.SetPathValue("name", "openrouter")
	w := httptest.NewRecorder()
	srv.handleGovernorGatewaysDelete(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "router-agent") {
		t.Errorf("409 body should name the in-use agent: %s", w.Body.String())
	}
	// Gateway must still be present.
	if len(srv.deps.Config.Governor.Gateways) != 1 {
		t.Errorf("gateway was deleted despite being in use")
	}
}

// TestGatewaysDelete_Succeeds verifies an unreferenced gateway deletes cleanly.
func TestGatewaysDelete_Succeeds(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Governor.Gateways = []config.GatewayConfig{
		{Name: "openrouter", Kind: "openrouter", Endpoint: "https://openrouter.ai/api/v1"},
	}
	req := httptest.NewRequest("DELETE", "/api/config/governor/gateways/openrouter", nil)
	req.SetPathValue("name", "openrouter")
	w := httptest.NewRecorder()
	srv.handleGovernorGatewaysDelete(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if len(srv.deps.Config.Governor.Gateways) != 0 {
		t.Errorf("gateway not removed")
	}
}

// TestGatewaysUpsert_PreservesKeyOnEdit verifies re-saving a gateway without a
// new key keeps the previously stored key file reference.
func TestGatewaysUpsert_PreservesKeyOnEdit(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Governor.Gateways = []config.GatewayConfig{
		{Name: "corp", Kind: "litellm", Endpoint: "https://old.example.com", APIKeyFile: "/data/secrets/gateway_corp_api_key", DefaultModel: "gpt-4o"},
	}
	// Edit only the endpoint; omit api_key / api_key_file / ca_bundle.
	body := `{"name":"corp","kind":"litellm","endpoint":"https://new.example.com","default_model":"gpt-4o-mini"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/gateways", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleGovernorGatewaysUpsert(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	gw := srv.deps.Config.Governor.Gateways[0]
	if gw.Endpoint != "https://new.example.com" {
		t.Errorf("endpoint not updated: %q", gw.Endpoint)
	}
	if gw.APIKeyFile != "/data/secrets/gateway_corp_api_key" {
		t.Errorf("api_key_file not preserved on edit: %q", gw.APIKeyFile)
	}
	if gw.DefaultModel != "gpt-4o-mini" {
		t.Errorf("default_model not updated: %q", gw.DefaultModel)
	}
}
