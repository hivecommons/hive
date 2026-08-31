package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// ---------------------------------------------------------------------------
// LiteLLMConfig.ResolveAPIKey — resolution order (file → named env → default env)
// ---------------------------------------------------------------------------

func TestLiteLLMResolveAPIKey_FromFile(t *testing.T) {
	dir := t.TempDir()
	// N8: api_key_file reads are confined to the managed secrets dirs; declare
	// this test's temp dir as an allowed root.
	defer SetSecretFileRootsForTest(dir)()
	keyPath := filepath.Join(dir, "litellm_api_key")
	if err := os.WriteFile(keyPath, []byte("sk-file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_LITELLM_TEST_KEY", "sk-env-key")
	t.Setenv(DefaultLiteLLMAPIKeyEnv, "sk-default-env-key")

	c := &LiteLLMConfig{APIKeyFile: keyPath, APIKeyEnv: "HIVE_LITELLM_TEST_KEY"}
	if got := c.ResolveAPIKey(); got != "sk-file-key" {
		t.Errorf("ResolveAPIKey() = %q, want key file to win", got)
	}
}

func TestLiteLLMResolveAPIKey_FromNamedEnv(t *testing.T) {
	t.Setenv("HIVE_LITELLM_TEST_KEY", "sk-env-key")
	t.Setenv(DefaultLiteLLMAPIKeyEnv, "sk-default-env-key")

	c := &LiteLLMConfig{APIKeyFile: filepath.Join(t.TempDir(), "missing"), APIKeyEnv: "HIVE_LITELLM_TEST_KEY"}
	if got := c.ResolveAPIKey(); got != "sk-env-key" {
		t.Errorf("ResolveAPIKey() = %q, want named env var to win over default", got)
	}
}

func TestLiteLLMResolveAPIKey_FromDefaultEnv(t *testing.T) {
	t.Setenv(DefaultLiteLLMAPIKeyEnv, "sk-default-env-key")

	c := &LiteLLMConfig{APIKeyFile: filepath.Join(t.TempDir(), "missing")}
	if got := c.ResolveAPIKey(); got != "sk-default-env-key" {
		t.Errorf("ResolveAPIKey() = %q, want default env fallback", got)
	}
}

func TestLiteLLMResolveAPIKey_Unconfigured(t *testing.T) {
	t.Setenv(DefaultLiteLLMAPIKeyEnv, "")

	c := &LiteLLMConfig{APIKeyFile: filepath.Join(t.TempDir(), "missing")}
	if got := c.ResolveAPIKey(); got != "" {
		t.Errorf("ResolveAPIKey() = %q, want empty when nothing configured", got)
	}
}

// ---------------------------------------------------------------------------
// LiteLLMConfig.ResolveEndpoint — env override
// ---------------------------------------------------------------------------

func TestLiteLLMResolveEndpoint(t *testing.T) {
	c := &LiteLLMConfig{Endpoint: "https://yaml.example.com"}

	t.Setenv(LiteLLMEndpointEnv, "")
	if got := c.ResolveEndpoint(); got != "https://yaml.example.com" {
		t.Errorf("ResolveEndpoint() = %q, want yaml value", got)
	}

	t.Setenv(LiteLLMEndpointEnv, "https://env.example.com")
	if got := c.ResolveEndpoint(); got != "https://env.example.com" {
		t.Errorf("ResolveEndpoint() = %q, want env override", got)
	}
}

// ---------------------------------------------------------------------------
// LiteLLMConfig.Validate — endpoint URL validation
// ---------------------------------------------------------------------------

func TestLiteLLMValidate(t *testing.T) {
	tests := []struct {
		endpoint string
		wantErr  bool
	}{
		{"", false},
		{"https://litellm.example.com", false},
		{"http://127.0.0.1:4000", false},
		{"litellm.example.com", true},       // no scheme
		{"ftp://litellm.example.com", true}, // wrong scheme
		{"https://", true},                  // no host
	}
	for _, tt := range tests {
		c := &LiteLLMConfig{Endpoint: tt.endpoint}
		err := c.Validate()
		if (err != nil) != tt.wantErr {
			t.Errorf("Validate(%q) error = %v, wantErr %v", tt.endpoint, err, tt.wantErr)
		}
	}
}

func TestValidate_LiteLLMEndpointRejected(t *testing.T) {
	yaml := minimalValidYAML("my-org", "ghp_tok") + `
governor:
  litellm:
    endpoint: not-a-url
`
	path := writeTempConfig(t, yaml)
	if _, err := Load(path); err == nil {
		t.Fatal("Load() expected error for invalid litellm endpoint, got nil")
	}
}

// ---------------------------------------------------------------------------
// config.IsInferenceBackend — canonical list
// ---------------------------------------------------------------------------

// The want-set is written out literally rather than ranged over from
// InferenceBackends. Ranging over the very slice IsInferenceBackend linearly
// scans is tautological: every element is a member by construction, so the
// scan always hits on that element and NO edit to the list can fail the test.
// Verified (#5388) by replacing the list with
// []string{"vllm", "totally-bogus-backend"} — dropping litellm and watsonx and
// inventing a backend that does not exist — and watching the loop still pass.
// An independent literal is what makes a wrong list a red test.
//
// Adding a real inference backend means adding it in BOTH places, which is the
// point: the second edit is a deliberate confirmation, not a formality.
func TestIsInferenceBackend_Config(t *testing.T) {
	want := []string{"vllm", "llm-d", "litellm", "watsonx"}

	for _, b := range want {
		if !IsInferenceBackend(b) {
			t.Errorf("IsInferenceBackend(%q) = false, want true", b)
		}
	}

	// The converse: nothing may quietly JOIN the canonical list either. Without
	// this, adding a backend to InferenceBackends alone still passes.
	if len(InferenceBackends) != len(want) {
		t.Errorf("InferenceBackends = %v (%d entries), want %v (%d). A backend was added or removed without updating this test's want-set.",
			InferenceBackends, len(InferenceBackends), want, len(want))
	}
	for _, b := range InferenceBackends {
		if !slices.Contains(want, b) {
			t.Errorf("InferenceBackends contains %q, which this test does not expect. Add it to want above if it is genuinely an inference backend.", b)
		}
	}

	if IsInferenceBackend("claude") {
		t.Error("IsInferenceBackend(claude) = true, want false")
	}
}
