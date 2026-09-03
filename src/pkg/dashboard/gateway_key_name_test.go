package dashboard

// Tests for the optional key NAME/label recorded alongside a model gateway's
// API key (#3606). The name generalizes the bob key's KeyName (#3596/#3598) to
// every gateway kind (litellm / openrouter / watsonx / vllm). It is
// safe-to-show metadata that lets managers tell WHICH key a gateway uses
// without ever exposing the value. These tests pin the round-trip, the
// backwards-compatible absent-name state, and that the label is never confused
// with (nor allowed to leak) the secret.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"gopkg.in/yaml.v3"
)

const gwKeyNameTestKey = "sk-key-name-test-value-1234567890"

// upsertGatewayForNameTest points the secret dir at a temp dir and PUTs the
// given JSON body through the upsert handler, returning the recorder.
func upsertGatewayForNameTest(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	dir := t.TempDir()
	orig := gatewaySecretsDir
	gatewaySecretsDir = dir
	t.Cleanup(func() { gatewaySecretsDir = orig })

	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/gateways", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorGatewaysUpsert(rec, req)
	return rec
}

// Setting a gateway key WITH a name records the label and returns it in the
// response and list, while the key value itself never appears.
func TestGatewayUpsert_SetWithNameRecordsLabel(t *testing.T) {
	srv := newFullServer(t)
	const wantName = "Team inference key"
	body := `{"name":"openrouter","kind":"openrouter","endpoint":"https://openrouter.ai/api/v1",` +
		`"api_key":"` + gwKeyNameTestKey + `","key_name":"` + wantName + `"}`
	rec := upsertGatewayForNameTest(t, srv, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), gwKeyNameTestKey) {
		t.Error("response body leaks the key value")
	}
	gws := srv.deps.Config.Governor.Gateways
	if len(gws) != 1 || gws[0].KeyName != wantName {
		t.Fatalf("stored KeyName = %q, want %q", func() string {
			if len(gws) == 1 {
				return gws[0].KeyName
			}
			return "<no gateway>"
		}(), wantName)
	}
	var resp struct {
		Gateway map[string]interface{} `json:"gateway"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Gateway["keyName"] != wantName {
		t.Errorf("response keyName = %v, want %q", resp.Gateway["keyName"], wantName)
	}
}

// A name is trimmed of surrounding whitespace (it is a display string) but
// interior spaces are preserved.
func TestGatewayUpsert_NameIsTrimmed(t *testing.T) {
	srv := newFullServer(t)
	body := `{"name":"openrouter","kind":"openrouter","endpoint":"https://openrouter.ai/api/v1",` +
		`"api_key":"` + gwKeyNameTestKey + `","key_name":"   spaced label   "}`
	rec := upsertGatewayForNameTest(t, srv, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := srv.deps.Config.Governor.Gateways[0].KeyName; got != "spaced label" {
		t.Errorf("KeyName = %q, want %q", got, "spaced label")
	}
}

// An oversized name is rejected and nothing is stored.
func TestGatewayUpsert_OversizedNameRejected(t *testing.T) {
	srv := newFullServer(t)
	body := `{"name":"openrouter","kind":"openrouter","endpoint":"https://openrouter.ai/api/v1",` +
		`"api_key":"` + gwKeyNameTestKey + `","key_name":"` + strings.Repeat("x", gatewayKeyNameMaxLen+1) + `"}`
	rec := upsertGatewayForNameTest(t, srv, body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(srv.deps.Config.Governor.Gateways) != 0 {
		t.Error("a gateway was stored for a request rejected on name length")
	}
}

// BACKWARDS COMPAT: setting a gateway key with NO key_name field leaves the
// name empty — never an error. This is how existing gateways (which never
// recorded a name) behave.
func TestGatewayUpsert_AbsentNameIsSafe(t *testing.T) {
	srv := newFullServer(t)
	body := `{"name":"openrouter","kind":"openrouter","endpoint":"https://openrouter.ai/api/v1",` +
		`"api_key":"` + gwKeyNameTestKey + `"}`
	rec := upsertGatewayForNameTest(t, srv, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := srv.deps.Config.Governor.Gateways[0].KeyName; got != "" {
		t.Errorf("KeyName = %q, want empty when no name is supplied", got)
	}
	var resp struct {
		Gateway map[string]interface{} `json:"gateway"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Gateway["keyName"] != "" {
		t.Errorf("response keyName = %v, want empty string", resp.Gateway["keyName"])
	}
}

// An absent (nil) key_name on an edit keeps the existing name, so rotating a
// key without re-typing its label does not silently wipe it. An explicit empty
// string clears it.
func TestGatewayUpsert_EditKeepsThenClearsName(t *testing.T) {
	srv := newFullServer(t)
	// Seed a gateway that already has a name.
	seed := `{"name":"openrouter","kind":"openrouter","endpoint":"https://openrouter.ai/api/v1",` +
		`"api_key":"` + gwKeyNameTestKey + `","key_name":"existing label"}`
	if rec := upsertGatewayForNameTest(t, srv, seed); rec.Code != http.StatusOK {
		t.Fatalf("seed status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// Edit sending no key_name field: the label must survive.
	edit := `{"name":"openrouter","kind":"openrouter","endpoint":"https://openrouter.ai/api/v1"}`
	if rec := upsertGatewayForNameTest(t, srv, edit); rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := srv.deps.Config.Governor.Gateways[0].KeyName; got != "existing label" {
		t.Errorf("name after nameless edit = %q, want it preserved", got)
	}

	// Now send an explicit empty string: the label is cleared.
	clear := `{"name":"openrouter","kind":"openrouter","endpoint":"https://openrouter.ai/api/v1","key_name":""}`
	if rec := upsertGatewayForNameTest(t, srv, clear); rec.Code != http.StatusOK {
		t.Fatalf("clear-name status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := srv.deps.Config.Governor.Gateways[0].KeyName; got != "" {
		t.Errorf("name after explicit empty = %q, want cleared", got)
	}
}

// The list endpoint surfaces the recorded name (safe metadata) and never the
// key value.
func TestGatewaysList_ReturnsKeyName(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Governor.Gateways = []config.GatewayConfig{{
		Name:     "openrouter",
		Kind:     "openrouter",
		Endpoint: "https://openrouter.ai/api/v1",
		KeyName:  "prod key",
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/config/governor/gateways", nil)
	rec := httptest.NewRecorder()
	srv.handleGovernorGatewaysList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Gateways []map[string]interface{} `json:"gateways"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Gateways) != 1 || resp.Gateways[0]["keyName"] != "prod key" {
		t.Errorf("list keyName = %v, want %q", resp.Gateways, "prod key")
	}
}

// BACKWARDS COMPAT at the config layer: a GatewayConfig with no key_name
// round-trips through YAML byte-identically (the omitempty tag), so existing
// gateways that never recorded a name are not rewritten with a spurious field.
func TestGatewayConfig_KeyNameOmitemptyRoundTrip(t *testing.T) {
	// No name set: the field must NOT appear in the marshaled YAML.
	noName := config.GatewayConfig{Name: "openrouter", Endpoint: "https://openrouter.ai/api/v1"}
	out, err := yaml.Marshal(noName)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "key_name") {
		t.Errorf("empty KeyName leaked a key_name field into YAML:\n%s", out)
	}

	// Name set: it round-trips.
	withName := config.GatewayConfig{Name: "openrouter", Endpoint: "https://openrouter.ai/api/v1", KeyName: "Team key"}
	out, err = yaml.Marshal(withName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "key_name: Team key") {
		t.Errorf("KeyName did not marshal as key_name:\n%s", out)
	}
	var back config.GatewayConfig
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back.KeyName != "Team key" {
		t.Errorf("KeyName after round-trip = %q, want %q", back.KeyName, "Team key")
	}
}
