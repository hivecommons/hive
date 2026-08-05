package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/watsonx"
)

// A watsonx gateway upsert missing the project_id is rejected (400) and leaves
// config untouched — the failure is a clear config error now, not an opaque
// upstream error at agent inference time.
func TestWatsonxUpsert_RejectsMissingProjectID(t *testing.T) {
	srv := newFullServer(t)
	body := `{"name":"watsonx","kind":"watsonx","endpoint":"https://us-south.ml.cloud.ibm.com/ml/gateway","api_key":"ibm-cloud-key-1234567890"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/gateways", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleGovernorGatewaysUpsert(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "project_id") {
		t.Errorf("error should mention project_id: %s", w.Body.String())
	}
	if len(srv.deps.Config.Governor.Gateways) != 0 {
		t.Errorf("config mutated on validation failure")
	}
}

// A watsonx gateway upsert missing an API key is rejected (400).
func TestWatsonxUpsert_RejectsMissingKey(t *testing.T) {
	srv := newFullServer(t)
	body := `{"name":"watsonx","kind":"watsonx","endpoint":"https://us-south.ml.cloud.ibm.com/ml/gateway","project_id":"proj-1"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/gateways", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleGovernorGatewaysUpsert(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "API key") {
		t.Errorf("error should mention the API key: %s", w.Body.String())
	}
	if len(srv.deps.Config.Governor.Gateways) != 0 {
		t.Errorf("config mutated on validation failure")
	}
}

// A watsonx gateway with both project_id and a key is accepted; the key is
// stored as a file ref (never inlined) and project_id/region persist inline.
func TestWatsonxUpsert_StoresGateway(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	orig := gatewaySecretsDir
	gatewaySecretsDir = dir
	t.Cleanup(func() { gatewaySecretsDir = orig })

	// The upsert runs a save-time probe that, for watsonx, mints an IAM token.
	// Point the shared minter at a fake IAM+models server so the test never
	// touches the network (and stays deterministic under -race).
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/identity/token") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "IAM-JWT", "expires_in": 3600})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []map[string]string{{"id": "ibm/granite-4-h-small"}}})
	}))
	defer fake.Close()
	origMinter := watsonx.DefaultMinter
	watsonx.DefaultMinter = watsonx.NewTokenMinterForTest(fake.URL+"/identity/token", nil)
	t.Cleanup(func() { watsonx.DefaultMinter = origMinter })

	const secret = "ibm-cloud-key-super-secret-1234567890"
	body := `{"name":"watsonx","kind":"watsonx","endpoint":"` + fake.URL + `/ml/gateway","project_id":"proj-42","region":"us-south","api_key":"` + secret + `","default_model":"ibm/granite-4-h-small"}`
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
	if gw.Kind != config.GatewayKindWatsonx || gw.ProjectID != "proj-42" || gw.Region != "us-south" {
		t.Fatalf("gateway = %+v", gw)
	}
	// The key VALUE must never be inlined into config.
	if strings.Contains(gw.APIKeyFile, "ibm-cloud-key") || gw.APIKeyEnv == secret {
		t.Fatalf("secret leaked into config: %+v", gw)
	}
	if !strings.HasPrefix(gw.APIKeyFile, dir) {
		t.Fatalf("api_key_file = %q, want a path under %s", gw.APIKeyFile, dir)
	}
	// The response must not echo the raw key.
	if strings.Contains(w.Body.String(), secret) {
		t.Errorf("upsert response echoed the secret value")
	}
}

// watsonx model discovery hits the gateway's OpenAI-compatible /v1/models with
// the minted IAM bearer AND the X-IBM-Project-ID header, and parses the returned
// list — verified against a fake gateway. The endpoint the operator configures
// (.../ml/gateway) plus hive's /v1/models suffix must reach the gateway's models
// path.
func TestWatsonxModelDiscovery(t *testing.T) {
	var gotAuth, gotProject, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotProject = r.Header.Get(watsonx.ProjectIDHeader)
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]string{
				{"id": "ibm/granite-4-h-small"},
				{"id": "ibm/granite-3-3-8b-instruct"},
			},
		})
	}))
	defer srv.Close()

	headers := map[string]string{watsonx.ProjectIDHeader: "proj-123"}
	models, err := fetchModelsWithHeaders(srv.URL, "IAM-JWT-token", headers)
	if err != nil {
		t.Fatalf("fetchModelsWithHeaders: %v", err)
	}
	if len(models) != 2 || models[0] != "ibm/granite-4-h-small" {
		t.Fatalf("models = %v, want the two granite ids", models)
	}
	if gotAuth != "Bearer IAM-JWT-token" {
		t.Fatalf("Authorization = %q, want the minted bearer", gotAuth)
	}
	if gotProject != "proj-123" {
		t.Fatalf("%s = %q, want the project id", watsonx.ProjectIDHeader, gotProject)
	}
	if !strings.HasSuffix(gotPath, "/v1/models") {
		t.Fatalf("path = %q, want a /v1/models suffix", gotPath)
	}
}

// The save-time probe against a watsonx gateway sends the project header and the
// bearer; when discovery fails the probe reports not-ok (never masquerading as
// success). Here we drive probeModelsWithHeaders directly (the IAM-minting
// wrapper is covered in the watsonx package).
func TestWatsonxProbeCountsModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(watsonx.ProjectIDHeader) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"missing project"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]string{{"id": "ibm/granite-4-h-small"}},
		})
	}))
	defer srv.Close()

	// With the project header the probe succeeds and counts the model.
	n, err := probeModelsWithHeaders(srv.URL, "bearer", map[string]string{watsonx.ProjectIDHeader: "p"})
	if err != nil {
		t.Fatalf("probe with project: %v", err)
	}
	if n != 1 {
		t.Fatalf("model_count = %d, want 1", n)
	}
	// Without it the gateway 400s and the probe returns an error, not a false
	// "0 models available" success.
	if _, err := probeModelsWithHeaders(srv.URL, "bearer", nil); err == nil {
		t.Fatal("probe without project should error, not report success")
	}
}

// gatewayProbeAuth passes non-watsonx kinds through unchanged (raw key as
// bearer, no extra headers) so existing gateways are unaffected by the watsonx
// wiring.
func TestGatewayProbeAuthNonWatsonxPassthrough(t *testing.T) {
	bearer, headers, err := gatewayProbeAuth(config.GatewayKindOpenRouter, "sk-raw-key", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bearer != "sk-raw-key" {
		t.Fatalf("bearer = %q, want the raw key unchanged for non-watsonx", bearer)
	}
	if len(headers) != 0 {
		t.Fatalf("headers = %v, want none for non-watsonx", headers)
	}
}

// The static Granite fallback list is non-empty and carries current granite ids,
// so a watsonx model dropdown is never empty when live discovery cannot run.
func TestGraniteFallbackNonEmpty(t *testing.T) {
	if len(watsonx.GraniteFallbackModels) == 0 {
		t.Fatal("Granite fallback list must not be empty")
	}
	foundGranite4 := false
	for _, m := range watsonx.GraniteFallbackModels {
		if strings.Contains(m, "granite-4") {
			foundGranite4 = true
		}
		if !strings.HasPrefix(m, "ibm/granite") {
			t.Fatalf("fallback id %q should be an ibm/granite foundation-model id", m)
		}
	}
	if !foundGranite4 {
		t.Fatal("fallback should include a granite-4 series id")
	}
}
