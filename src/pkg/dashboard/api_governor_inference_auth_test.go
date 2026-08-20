package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"
)

// ---- vLLM / llm-d inference auth (#4217) ----

func TestInferenceAuth_Get(t *testing.T) {
	s := covApiServer(t)
	// Owner-only: unauthenticated GET is rejected.
	if rec := doGet(s, "/api/config/governor/inference-auth"); rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated inference-auth get: %d", rec.Code)
	}
	rec := doOwnerGet(s, "/api/config/governor/inference-auth")
	if rec.Code != http.StatusOK {
		t.Fatalf("inference-auth get: %d", rec.Code)
	}
	var out map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"vllm", "llmd"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("missing %q section: %s", k, rec.Body.String())
		}
	}
}

func TestInferenceAuth_PutAndRoundTrip(t *testing.T) {
	s := covApiServer(t)
	rec := doPut(s, "/api/config/governor/inference-auth", map[string]any{
		"vllm": map[string]any{
			"api_key_header": "Authorization",
			"api_key_env":    "VLLM_API_KEY",
			"endpoint":       "https://vllm.example.com/v1",
		},
		"llmd": map[string]any{
			"api_key_env": "LLMD_API_KEY",
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("inference-auth put: %d (%s)", rec.Code, rec.Body.String())
	}
	gov := s.deps.Config.Governor
	if gov.VLLM.APIKeyHeader != "Authorization" || gov.VLLM.APIKeyEnv != "VLLM_API_KEY" ||
		gov.VLLM.Endpoint != "https://vllm.example.com/v1" {
		t.Fatalf("vllm not applied: %+v", gov.VLLM)
	}
	if gov.LLMD.APIKeyEnv != "LLMD_API_KEY" {
		t.Fatalf("llmd not applied: %+v", gov.LLMD)
	}
	// Absent fields left untouched; explicit empty clears.
	rec = doPut(s, "/api/config/governor/inference-auth", map[string]any{
		"vllm": map[string]any{"api_key_env": ""},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("inference-auth clear: %d", rec.Code)
	}
	gov = s.deps.Config.Governor
	if gov.VLLM.APIKeyEnv != "" {
		t.Fatalf("api_key_env not cleared: %+v", gov.VLLM)
	}
	if gov.VLLM.APIKeyHeader != "Authorization" || gov.VLLM.Endpoint != "https://vllm.example.com/v1" {
		t.Fatalf("absent fields were wiped: %+v", gov.VLLM)
	}
}

func TestInferenceAuth_PutRejectsBad(t *testing.T) {
	s := covApiServer(t)
	if rec := doPutRaw(s, "/api/config/governor/inference-auth", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", rec.Code)
	}
	// Key VALUE in the env-name field → 400 guardrail.
	if rec := doPut(s, "/api/config/governor/inference-auth", map[string]any{
		"vllm": map[string]any{"api_key_env": "sk-abcdef1234567890abcdef1234567890"},
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("key-value in env name: %d (%s)", rec.Code, rec.Body.String())
	}
	// Out-of-root key file path → 400 confinement.
	if rec := doPut(s, "/api/config/governor/inference-auth", map[string]any{
		"llmd": map[string]any{"api_key_file": "/etc/passwd"},
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("out-of-root key file: %d (%s)", rec.Code, rec.Body.String())
	}
	// Invalid endpoint → 400.
	if rec := doPut(s, "/api/config/governor/inference-auth", map[string]any{
		"vllm": map[string]any{"endpoint": "not a url"},
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad endpoint: %d (%s)", rec.Code, rec.Body.String())
	}
}
