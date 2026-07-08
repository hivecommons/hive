package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- handleGovernorLiteLLM ---

func TestHandleGovernorLiteLLM_Update(t *testing.T) {
	srv := newFullServer(t)
	body := `{
		"endpoint": "https://litellm.example.com",
		"apiKeyEnv": "MY_LITELLM_KEY",
		"defaultModel": "gpt-4o",
		"caBundle": "/secrets/litellm-ca.pem",
		"localProxy": false
	}`
	req := httptest.NewRequest("PUT", "/api/config/governor/litellm", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleGovernorLiteLLM(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	lc := srv.deps.Config.Governor.LiteLLM
	if lc.Endpoint != "https://litellm.example.com" {
		t.Errorf("Endpoint = %q", lc.Endpoint)
	}
	if lc.APIKeyEnv != "MY_LITELLM_KEY" {
		t.Errorf("APIKeyEnv = %q", lc.APIKeyEnv)
	}
	if lc.DefaultModel != "gpt-4o" {
		t.Errorf("DefaultModel = %q", lc.DefaultModel)
	}
	// The endpoint must now be registered for model discovery.
	endpoints, ok := srv.getInferenceEndpoints("litellm")
	if !ok || len(endpoints) != 1 || endpoints[0] != "https://litellm.example.com" {
		t.Errorf("registered endpoints = %v, ok = %v", endpoints, ok)
	}
}

func TestHandleGovernorLiteLLM_InvalidEndpointRejected(t *testing.T) {
	srv := newFullServer(t)
	body := `{"endpoint": "not-a-url"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/litellm", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleGovernorLiteLLM(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
	if srv.deps.Config.Governor.LiteLLM.Endpoint != "" {
		t.Errorf("config endpoint mutated on validation failure: %q", srv.deps.Config.Governor.LiteLLM.Endpoint)
	}
}

// --- handleGovernorConfigGet: litellm section never leaks a key value ---

func TestHandleGovernorConfigGet_LiteLLMSection(t *testing.T) {
	srv := newFullServer(t)
	t.Setenv("HIVE_LITELLM_API_KEY", "sk-super-secret")
	srv.deps.Config.Governor.LiteLLM.Endpoint = "https://litellm.example.com"

	req := httptest.NewRequest("GET", "/api/config/governor", nil)
	w := httptest.NewRecorder()
	srv.handleGovernorConfigGet(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "sk-super-secret") {
		t.Fatal("response body contains the raw API key")
	}
	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	section, ok := result["litellm"].(map[string]any)
	if !ok {
		t.Fatalf("no litellm section in response: %v", result)
	}
	if section["endpoint"] != "https://litellm.example.com" {
		t.Errorf("endpoint = %v", section["endpoint"])
	}
	if section["hasKey"] != true {
		t.Errorf("hasKey = %v, want true (key resolves from env)", section["hasKey"])
	}
}
