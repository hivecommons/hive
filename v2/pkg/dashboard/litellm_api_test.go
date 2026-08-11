package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	markOwnerRequest(req)
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
	markOwnerRequest(req)
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

// --- env-name guardrail: key values pasted into the env NAME field ---

func TestHandleGovernorLiteLLM_EnvNameGuardrail(t *testing.T) {
	cases := []struct {
		name   string
		keyEnv string
		wantOK bool
	}{
		{"sk-prefixed key value", "sk-3aF9xQ7bLm2v", false},
		{"long dashed key value", strings.Repeat("aB3x-", 12), false},
		{"conventional env name", "HIVE_LITELLM_API_KEY", true},
		{"custom env name", "MY_GATEWAY_KEY", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFullServer(t)
			body := `{"endpoint": "https://litellm.example.com", "apiKeyEnv": "` + tc.keyEnv + `"}`
			req := httptest.NewRequest("PUT", "/api/config/governor/litellm", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			markOwnerRequest(req)
			srv.handleGovernorLiteLLM(w, req)
			if tc.wantOK && w.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200 (body: %s)", w.Code, w.Body.String())
			}
			if !tc.wantOK {
				if w.Code != http.StatusBadRequest {
					t.Fatalf("code = %d, want 400 for key-like env name", w.Code)
				}
				if !strings.Contains(w.Body.String(), "looks like an API key") {
					t.Errorf("error should explain the field takes a NAME: %s", w.Body.String())
				}
				if srv.deps.Config.Governor.LiteLLM.APIKeyEnv != "" {
					t.Errorf("key-like value persisted into APIKeyEnv: %q", srv.deps.Config.Governor.LiteLLM.APIKeyEnv)
				}
			}
		})
	}
}

// --- apiKey value storage: PVC file, 0600, never echoed ---

func TestHandleGovernorLiteLLM_APIKeyValueStored(t *testing.T) {
	origKeyFile := writableLiteLLMKeyFile
	writableLiteLLMKeyFile = filepath.Join(t.TempDir(), "secrets", "litellm_api_key")
	t.Cleanup(func() { writableLiteLLMKeyFile = origKeyFile })

	const secret = "sk-verysecretkeyvalue123456"
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"Authentication Error: token not found"}}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"claude_opus_4.8"},{"id":"gemini_2.5_pro"}]}`)
	}))
	defer gateway.Close()

	srv := newFullServer(t)
	body := `{"endpoint": "` + gateway.URL + `", "apiKey": "` + secret + `"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/litellm", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorLiteLLM(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d (body: %s)", w.Code, w.Body.String())
	}
	// Key value written to the PVC file with owner-only perms.
	data, err := os.ReadFile(writableLiteLLMKeyFile)
	if err != nil {
		t.Fatalf("key file not written: %v", err)
	}
	if string(data) != secret {
		t.Errorf("key file content mismatch")
	}
	info, _ := os.Stat(writableLiteLLMKeyFile)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", info.Mode().Perm())
	}
	// hive.yaml records only the file path.
	if got := srv.deps.Config.Governor.LiteLLM.APIKeyFile; got != writableLiteLLMKeyFile {
		t.Errorf("APIKeyFile = %q, want %q", got, writableLiteLLMKeyFile)
	}
	// The key value never appears in the response.
	if strings.Contains(w.Body.String(), secret) {
		t.Fatal("response echoes the API key value")
	}
	// Save-time probe reports live model count.
	var resp struct {
		Probe struct {
			OK     bool   `json:"ok"`
			Models int    `json:"models"`
			Error  string `json:"error"`
		} `json:"probe"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Probe.OK || resp.Probe.Models != 2 {
		t.Errorf("probe = %+v, want ok with 2 models", resp.Probe)
	}
}

// --- save-time probe surfaces the gateway's real error, redacted ---

func TestHandleGovernorLiteLLM_ProbeSurfacesGatewayError(t *testing.T) {
	origKeyFile := writableLiteLLMKeyFile
	writableLiteLLMKeyFile = filepath.Join(t.TempDir(), "litellm_api_key")
	t.Cleanup(func() { writableLiteLLMKeyFile = origKeyFile })

	const secret = "sk-wrongkeyvalue9876543210"
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// Echo the key back like a chatty gateway might — it must be redacted.
		fmt.Fprintf(w, `{"error":{"message":"token not found: %s"}}`, secret)
	}))
	defer gateway.Close()

	srv := newFullServer(t)
	body := `{"endpoint": "` + gateway.URL + `", "apiKey": "` + secret + `"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/litellm", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorLiteLLM(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d (config save should succeed even when the probe fails)", w.Code)
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Fatal("probe error echoes the API key value")
	}
	var resp struct {
		Probe struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"probe"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Probe.OK {
		t.Fatal("probe.ok = true, want false for a 401 gateway")
	}
	if !strings.Contains(resp.Probe.Error, "token not found") {
		t.Errorf("probe error should carry the gateway message, got: %s", resp.Probe.Error)
	}
	if !strings.Contains(resp.Probe.Error, "rejected the configured key") {
		t.Errorf("probe error should say the key was rejected, got: %s", resp.Probe.Error)
	}
}

// --- Test Connection endpoint: live results only, never static fallback ---

func TestHandleGovernorLiteLLMTest_NeverCountsFallbackAliases(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"No api key passed in."}}`)
	}))
	defer gateway.Close()

	srv := newFullServer(t)
	srv.deps.Config.Governor.LiteLLM.Endpoint = gateway.URL

	req := httptest.NewRequest("POST", "/api/config/governor/litellm/test", nil)
	w := httptest.NewRecorder()
	srv.handleGovernorLiteLLMTest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var resp struct {
		Probe struct {
			OK     bool   `json:"ok"`
			Models int    `json:"models"`
			Error  string `json:"error"`
		} `json:"probe"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Probe.OK {
		t.Fatal("probe reported success against a 401 gateway (static fallback leak?)")
	}
	if !strings.Contains(resp.Probe.Error, "requires an API key") {
		t.Errorf("no-key 401 should say a key is required, got: %s", resp.Probe.Error)
	}
	if !strings.Contains(resp.Probe.Error, "No api key passed in") {
		t.Errorf("probe error should carry the gateway body, got: %s", resp.Probe.Error)
	}
}

func TestHandleGovernorLiteLLMTest_NoEndpoint(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/config/governor/litellm/test", nil)
	w := httptest.NewRecorder()
	srv.handleGovernorLiteLLMTest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 when no endpoint configured", w.Code)
	}
}

// --- file-path field guardrail ---

func TestHandleGovernorLiteLLM_KeyFileGuardrail(t *testing.T) {
	srv := newFullServer(t)
	body := `{"endpoint": "https://litellm.example.com", "apiKeyFile": "sk-3aF9xQ7bLm2v"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/litellm", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorLiteLLM(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 for key-like file path", w.Code)
	}
	if !strings.Contains(w.Body.String(), "file PATH") {
		t.Errorf("error should explain the field takes a path: %s", w.Body.String())
	}
	// A real absolute path is accepted.
	srv2 := newFullServer(t)
	body = `{"endpoint": "https://litellm.example.com", "apiKeyFile": "/secrets/litellm_api_key"}`
	req = httptest.NewRequest("PUT", "/api/config/governor/litellm", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	markOwnerRequest(req)
	srv2.handleGovernorLiteLLM(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 for absolute path (body: %s)", w.Code, w.Body.String())
	}
}

// --- GET masking: a key VALUE pasted into env/file fields never echoes ---

func TestHandleGovernorConfigGet_MasksKeyLikeEnvName(t *testing.T) {
	const pastedKey = "sk-8uOe6IBkSSxEU7lfEp1WMg"
	srv := newFullServer(t)
	srv.deps.Config.Governor.LiteLLM.Endpoint = "https://litellm.example.com"
	// Pre-guardrail leak scenario: the user pasted the actual key into
	// the env-name field and it was persisted to hive.yaml.
	srv.deps.Config.Governor.LiteLLM.APIKeyEnv = pastedKey

	req := httptest.NewRequest("GET", "/api/config/governor", nil)
	w := httptest.NewRecorder()
	srv.handleGovernorConfigGet(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), pastedKey) {
		t.Fatal("response echoes the pasted key value")
	}
	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	section := result["litellm"].(map[string]any)
	if section["apiKeyEnvLooksLikeKey"] != true {
		t.Error("apiKeyEnvLooksLikeKey should be true")
	}
	masked, _ := section["apiKeyEnv"].(string)
	if !strings.HasPrefix(masked, "••••") || !strings.HasSuffix(masked, "1WMg") {
		t.Errorf("apiKeyEnv should be a masked tail hint, got %q", masked)
	}
}

func TestHandleGovernorConfigGet_KeyHintMaskedTail(t *testing.T) {
	srv := newFullServer(t)
	t.Setenv("HIVE_LITELLM_API_KEY", "sk-resolvedsecretkeyvalue")
	srv.deps.Config.Governor.LiteLLM.Endpoint = "https://litellm.example.com"

	req := httptest.NewRequest("GET", "/api/config/governor", nil)
	w := httptest.NewRecorder()
	srv.handleGovernorConfigGet(w, req)

	if strings.Contains(w.Body.String(), "sk-resolvedsecretkeyvalue") {
		t.Fatal("response contains the resolved key value")
	}
	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	section := result["litellm"].(map[string]any)
	if section["hasKey"] != true {
		t.Fatalf("hasKey = %v", section["hasKey"])
	}
	if hint, _ := section["keyHint"].(string); hint != "••••alue" {
		t.Errorf("keyHint = %q, want masked last-4 tail", hint)
	}
}

// --- storing a real key scrubs a pasted key from api_key_env ---

func TestHandleGovernorLiteLLM_StoringKeyScrubsPastedEnvValue(t *testing.T) {
	origKeyFile := writableLiteLLMKeyFile
	writableLiteLLMKeyFile = filepath.Join(t.TempDir(), "litellm_api_key")
	t.Cleanup(func() { writableLiteLLMKeyFile = origKeyFile })

	srv := newFullServer(t)
	srv.deps.Config.Governor.LiteLLM.APIKeyEnv = "sk-previouslypastedkeyvalue"

	body := `{"endpoint": "https://litellm.example.com", "apiKey": "sk-thecorrectkey123"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/litellm", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorLiteLLM(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d (body: %s)", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.Governor.LiteLLM.APIKeyEnv; got != "" {
		t.Errorf("pasted key value still in APIKeyEnv: %q", got)
	}
}
