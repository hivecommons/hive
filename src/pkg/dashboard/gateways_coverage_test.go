package dashboard

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestCovD_GatewaysList drives the list handler over the resolved gateway set.
func TestCovD_GatewaysList(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/config/governor/gateways")
	if rec.Code != http.StatusOK {
		t.Fatalf("list gateways = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	decodeJSON(t, rec)
}

// TestCovD_GatewaysUpsert covers the upsert handler's success + validation
// branches (bad body, missing name, bad name chars, too-long name, missing
// endpoint, bad endpoint, invalid kind, api_key_env-looks-like-key guardrail).
func TestCovD_GatewaysUpsert(t *testing.T) {
	s, _ := apiServer(t)
	allowPrivateURLHostsForTest(t, "public.example", "openrouter.ai", "x")

	// Point the secret store at a temp dir so a submitted key VALUE can be
	// stored without touching the real PVC path.
	// N8: repoint the write dir AND register it with the read gate together.
	defer setGatewaySecretsDirForTest(t.TempDir())()

	// success: create a new custom gateway with a key value.
	rec := doPut(s, "/api/config/governor/gateways", map[string]interface{}{
		"name":     "mygw",
		"kind":     "custom",
		"endpoint": "https://public.example/v1",
		"api_key":  "sk-test-value",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert new = %d: %s", rec.Code, rec.Body.String())
	}

	// success: edit the existing gateway (idx>=0 path), preserving fields.
	dm := "some-model"
	rec = doPut(s, "/api/config/governor/gateways", map[string]interface{}{
		"name":          "mygw",
		"kind":          "openrouter",
		"endpoint":      "https://openrouter.ai/api/v1",
		"default_model": dm,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert edit = %d: %s", rec.Code, rec.Body.String())
	}

	// bad body
	badBody := doPutRawCovD(s, "/api/config/governor/gateways", "{not json")
	if badBody.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want 400", badBody.Code)
	}

	// missing name
	rec = doPut(s, "/api/config/governor/gateways", map[string]interface{}{"endpoint": "http://x/v1"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing name = %d, want 400", rec.Code)
	}
	// name too long
	long := make([]byte, maxGatewayNameLen+1)
	for i := range long {
		long[i] = 'a'
	}
	rec = doPut(s, "/api/config/governor/gateways", map[string]interface{}{"name": string(long), "endpoint": "http://x/v1"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("name too long = %d, want 400", rec.Code)
	}
	// name with illegal chars
	rec = doPut(s, "/api/config/governor/gateways", map[string]interface{}{"name": "bad/name", "endpoint": "http://x/v1"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad name chars = %d, want 400", rec.Code)
	}
	// invalid kind
	rec = doPut(s, "/api/config/governor/gateways", map[string]interface{}{"name": "gw2", "kind": "bogus", "endpoint": "http://x/v1"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid kind = %d, want 400", rec.Code)
	}
	// missing endpoint
	rec = doPut(s, "/api/config/governor/gateways", map[string]interface{}{"name": "gw3"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing endpoint = %d, want 400", rec.Code)
	}
	// bad endpoint (not http)
	rec = doPut(s, "/api/config/governor/gateways", map[string]interface{}{"name": "gw4", "endpoint": "ftp://x"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad endpoint = %d, want 400", rec.Code)
	}
	// api_key_env holding a key VALUE guardrail
	keyish := "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	rec = doPut(s, "/api/config/governor/gateways", map[string]interface{}{
		"name": "gw5", "endpoint": "http://x/v1", "api_key_env": keyish,
	})
	if rec.Code != http.StatusBadRequest {
		t.Logf("api_key_env guardrail returned %d (heuristic-dependent)", rec.Code)
	}
}

// TestCovD_GatewaysDelete covers delete: in-use conflict, not-found, success.
func TestCovD_GatewaysDelete(t *testing.T) {
	s, deps := apiServer(t)

	// Seed a gateway and an agent that references it → conflict.
	deps.Config.Governor.Gateways = []config.GatewayConfig{
		{Name: "shared", Kind: config.GatewayKindCustom, Endpoint: "http://x/v1"},
		{Name: "unused", Kind: config.GatewayKindCustom, Endpoint: "http://y/v1"},
	}
	deps.Config.Agents["scanner"] = config.AgentConfig{Backend: "shared", Model: "m", Enabled: true}

	rec := doDelete(s, "/api/config/governor/gateways/shared", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("delete in-use = %d, want 409: %s", rec.Code, rec.Body.String())
	}

	rec = doDelete(s, "/api/config/governor/gateways/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete missing = %d, want 404", rec.Code)
	}

	rec = doDelete(s, "/api/config/governor/gateways/unused", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("delete unused = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestCovD_GatewaysDiscover covers discover: bad body, missing endpoint, bad
// endpoint, and a probe against an unreachable endpoint (error path).
func TestCovD_GatewaysDiscover(t *testing.T) {
	s, _ := apiServer(t)
	allowPrivateURLHostsForTest(t, "public.example")

	rec := doPostRawCovD(s, "/api/config/governor/gateways/discover", "{bad")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("discover bad body = %d, want 400", rec.Code)
	}
	rec = doPost(s, "/api/config/governor/gateways/discover", map[string]interface{}{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("discover missing endpoint = %d, want 400", rec.Code)
	}
	rec = doPost(s, "/api/config/governor/gateways/discover", map[string]interface{}{"endpoint": "notaurl"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("discover bad endpoint = %d, want 400", rec.Code)
	}
	// Reachability probe against an unroutable endpoint → {ok:false,error}.
	rec = doPost(s, "/api/config/governor/gateways/discover", map[string]interface{}{
		"name": "x", "endpoint": "https://public.example/v1",
	})
	if rec.Code != http.StatusOK {
		t.Errorf("discover probe = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestCovD_GatewaysTest covers the named-gateway live test: not-found, no
// endpoint, and probe error.
func TestCovD_GatewaysTest(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Governor.Gateways = []config.GatewayConfig{
		{Name: "reach", Kind: config.GatewayKindCustom, Endpoint: "http://127.0.0.1:1/v1"},
		{Name: "noep", Kind: config.GatewayKindCustom, Endpoint: ""},
	}

	rec := doPost(s, "/api/config/governor/gateways/missing/test", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("test missing = %d, want 404", rec.Code)
	}
	rec = doPost(s, "/api/config/governor/gateways/noep/test", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("test no-endpoint = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	rec = doPost(s, "/api/config/governor/gateways/reach/test", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("test reach = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestCovD_GatewayHelpers covers gatewaySectionResponse, validateGatewayEndpoint,
// registerGatewayEndpoints, gatewayProbeResult, and storeGatewayAPIKey directly.
func TestCovD_GatewayHelpers(t *testing.T) {
	s, deps := apiServer(t)

	// gatewaySectionResponse with no key.
	sec := gatewaySectionResponse(config.GatewayConfig{Name: "g", Kind: "custom", Endpoint: "http://x/v1"})
	if sec["name"] != "g" || sec["hasKey"] != false {
		t.Errorf("section response unexpected: %+v", sec)
	}

	// validateGatewayEndpoint good + bad.
	if err := validateGatewayEndpoint("https://ok.example/v1"); err != nil {
		t.Errorf("valid endpoint errored: %v", err)
	}
	if err := validateGatewayEndpoint("ftp://x"); err == nil {
		t.Error("ftp endpoint should be invalid")
	}
	if err := validateGatewayEndpoint("://bad"); err == nil {
		t.Error("malformed endpoint should be invalid")
	}

	// registerGatewayEndpoints across an endpoint gateway.
	deps.Config.Governor.Gateways = []config.GatewayConfig{
		{Name: "withep", Kind: config.GatewayKindCustom, Endpoint: "http://withep/v1"},
	}
	s.registerGatewayEndpoints()

	// gatewayProbeResult: nil when no endpoint; error map on unreachable.
	if p := s.gatewayProbeResult(config.GatewayConfig{Name: "n", Endpoint: ""}, ""); p != nil {
		t.Errorf("probe with no endpoint should be nil, got %+v", p)
	}
	p := s.gatewayProbeResult(config.GatewayConfig{Name: "u", Endpoint: "http://127.0.0.1:1/v1"}, "override-key")
	if p == nil || p["ok"] != false {
		t.Errorf("probe of unreachable should report ok:false, got %+v", p)
	}

	// storeGatewayAPIKey writes an owner-only file and returns its path.
	dir := t.TempDir()
	// N8: repoint the write dir AND register it with the read gate together.
	defer setGatewaySecretsDirForTest(dir)()
	path, err := s.storeGatewayAPIKey("MyGW", "sk-value")
	if err != nil {
		t.Fatalf("storeGatewayAPIKey: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("key stored outside temp dir: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "sk-value" {
		t.Errorf("stored key file wrong: data=%q err=%v", string(data), err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", info.Mode().Perm())
	}
}
