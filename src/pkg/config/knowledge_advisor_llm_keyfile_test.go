package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/keystore"
)

const advisorKeyfileTestKey = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func activateAdvisorTestKeystore(t *testing.T) {
	t.Helper()
	keystore.ResetForTest()
	t.Cleanup(keystore.ResetForTest)
	prov, err := keystore.NewEnvKeyProvider([]byte(advisorKeyfileTestKey))
	if err != nil {
		t.Fatal(err)
	}
	keystore.SetActive(prov)
}

// hivecommons/hive#17054 evidence pass 2: the durable Copilot token is sealed
// at rest once a keystore provider is active. The advisor read the file raw
// and sent the envelope as its bearer token, so every call was refused; the
// key file must resolve to the plaintext.
func TestKnowledgeAdvisorLLM_SealedKeyFileResolvesToPlaintext(t *testing.T) {
	activateAdvisorTestKeystore(t)
	t.Setenv(LiteLLMEndpointEnv, "")
	t.Setenv(DefaultLiteLLMAPIKeyEnv, "")
	path := filepath.Join(t.TempDir(), "copilot-user-token")
	const token = "plain-advisor-token"
	if err := keystore.WriteFile(context.Background(), path, []byte(token)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !keystore.IsEnvelope(raw) {
		t.Fatalf("test setup: file is not sealed: %q", raw)
	}
	c := advisorLLMCfg(AdvisorLLMConfig{Mode: AdvisorLLMModeRank, Model: "m", Endpoint: "http://own.local", APIKeyFile: path})
	r, ok := c.KnowledgeAdvisorLLM()
	if !ok {
		t.Fatal("ok = false with a sealed key file")
	}
	if r.APIKey != token {
		t.Fatalf("APIKey = %q, want the plaintext token (an envelope here is the #17054 bug)", r.APIKey)
	}
	if p := c.KnowledgeAdvisorLLMProblem(); p != "" {
		t.Fatalf("problem = %q, want none", p)
	}
}

func TestKnowledgeAdvisorLLM_UnopenableKeyFileIsReportedNotSent(t *testing.T) {
	activateAdvisorTestKeystore(t)
	t.Setenv(LiteLLMEndpointEnv, "")
	t.Setenv(DefaultLiteLLMAPIKeyEnv, "")
	path := filepath.Join(t.TempDir(), "copilot-user-token")
	if err := keystore.WriteFile(context.Background(), path, []byte("plain-advisor-token")); err != nil {
		t.Fatal(err)
	}
	// The hive restarts without the key that sealed the file.
	keystore.ResetForTest()
	keystore.SetActive(nil)
	c := advisorLLMCfg(AdvisorLLMConfig{Mode: AdvisorLLMModeRank, Model: "m", Endpoint: "http://own.local", APIKeyFile: path})
	if r, ok := c.KnowledgeAdvisorLLM(); ok {
		t.Fatalf("resolved %+v from a file this process cannot open", r)
	}
	p := c.KnowledgeAdvisorLLMProblem()
	if !strings.Contains(p, "knowledge.advisor_llm.api_key_file") || !strings.Contains(p, path) {
		t.Fatalf("problem = %q, want it to name the key and the path", p)
	}
	if strings.Contains(p, keystore.Prefix) {
		t.Fatalf("problem leaks envelope text: %q", p)
	}
}

func TestKnowledgeAdvisorLLMProblem(t *testing.T) {
	t.Setenv(LiteLLMEndpointEnv, "")
	t.Setenv(DefaultLiteLLMAPIKeyEnv, "")
	t.Setenv("ADV_LLM_PROBLEM_KEY", "k")
	t.Setenv("ADV_LLM_PROBLEM_SEALED", keystore.Prefix+"v1:env:abc:Zm9v")
	tests := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"advisor off", &Config{Knowledge: KnowledgeConfig{AdvisorLLM: AdvisorLLMConfig{Mode: AdvisorLLMModeRank}}}, ""},
		{"mode off", advisorLLMCfg(AdvisorLLMConfig{Mode: AdvisorLLMModeOff}), ""},
		{"no model", advisorLLMCfg(AdvisorLLMConfig{Mode: AdvisorLLMModeRank, Endpoint: "http://e", APIKeyEnv: "ADV_LLM_PROBLEM_KEY"}), "no model"},
		{"no endpoint", advisorLLMCfg(AdvisorLLMConfig{Mode: AdvisorLLMModeShadow, Model: "m", APIKeyEnv: "ADV_LLM_PROBLEM_KEY"}), "no endpoint"},
		{"no key", advisorLLMCfg(AdvisorLLMConfig{Mode: AdvisorLLMModeRank, Model: "m", Endpoint: "http://e"}), "no API key"},
		{"sealed env key", advisorLLMCfg(AdvisorLLMConfig{Mode: AdvisorLLMModeRank, Model: "m", Endpoint: "http://e", APIKeyEnv: "ADV_LLM_PROBLEM_SEALED"}), "sealed keystore envelope"},
		{"ok", advisorLLMCfg(AdvisorLLMConfig{Mode: AdvisorLLMModeRank, Model: "m", Endpoint: "http://e", APIKeyEnv: "ADV_LLM_PROBLEM_KEY"}), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.KnowledgeAdvisorLLMProblem()
			if tc.want == "" && got != "" {
				t.Fatalf("problem = %q, want none", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("problem = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}
