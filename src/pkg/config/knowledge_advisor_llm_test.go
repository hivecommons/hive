package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func advisorLLMCfg(a AdvisorLLMConfig) *Config {
	return &Config{Knowledge: KnowledgeConfig{Enabled: true, Advisor: true, AdvisorLLM: a}}
}

func TestKnowledgeAdvisorLLM_Validate(t *testing.T) {
	tests := []struct {
		name     string
		cfg      AdvisorLLMConfig
		wantErr  string
		wantWarn bool
	}{
		{"empty block ok", AdvisorLLMConfig{}, "", false},
		{"rank with model ok", AdvisorLLMConfig{Mode: "rank", Model: "m"}, "", false},
		{"bad mode", AdvisorLLMConfig{Mode: "loud"}, "knowledge.advisor_llm.mode", false},
		{"shadow without model warns", AdvisorLLMConfig{Mode: "shadow"}, "", true},
		{"rank without model warns", AdvisorLLMConfig{Mode: "rank"}, "", true},
		{"off without model silent", AdvisorLLMConfig{Mode: "off"}, "", false},
		{"hourly ceiling exceeded", AdvisorLLMConfig{MaxCallsPerAgentHour: 7}, "max_calls_per_agent_hour", false},
		{"daily ceiling exceeded", AdvisorLLMConfig{MaxCallsPerDay: 301}, "max_calls_per_day", false},
		{"ceilings at limit ok", AdvisorLLMConfig{MaxCallsPerAgentHour: 6, MaxCallsPerDay: 300}, "", false},
		{"ceilings raised with override", AdvisorLLMConfig{MaxCallsPerAgentHour: 60, MaxCallsPerDay: 3000, BudgetOverride: true}, "", false},
		{"holdout negative", AdvisorLLMConfig{HoldoutPercent: -1}, "holdout_percent", false},
		{"holdout over 100", AdvisorLLMConfig{HoldoutPercent: 101}, "holdout_percent", false},
		{"holdout 100 ok", AdvisorLLMConfig{HoldoutPercent: 100}, "", false},
		{"bad on_error", AdvisorLLMConfig{OnError: "panic"}, "knowledge.advisor_llm.on_error", false},
		{"on_error matcher ok", AdvisorLLMConfig{OnError: "matcher"}, "", false},
		{"candidates negative", AdvisorLLMConfig{Candidates: -1}, "candidates", false},
		{"pane_lines negative", AdvisorLLMConfig{PaneLines: -1}, "pane_lines", false},
		{"timeout_s negative", AdvisorLLMConfig{TimeoutS: -1}, "timeout_s", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warns, err := advisorLLMCfg(tt.cfg).validateKnowledgeAdvisorLLM()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want it to name %q", err, tt.wantErr)
			}
			if tt.wantWarn != (len(warns) > 0) {
				t.Fatalf("warnings = %v, wantWarn %v", warns, tt.wantWarn)
			}
		})
	}
}

func TestKnowledgeAdvisorLLM_ShadowWithoutModelResolvesOff(t *testing.T) {
	c := advisorLLMCfg(AdvisorLLMConfig{Mode: "shadow", Endpoint: "http://gw.local", APIKeyEnv: "ADV_LLM_TEST_KEY"})
	t.Setenv("ADV_LLM_TEST_KEY", "k")
	r, ok := c.KnowledgeAdvisorLLM()
	if !ok || r.Mode != AdvisorLLMModeOff {
		t.Fatalf("got %+v ok=%v, want ok with mode off", r, ok)
	}
}

func TestKnowledgeAdvisorLLM_DefaultsAndParse(t *testing.T) {
	var k KnowledgeConfig
	src := "advisor: true\nadvisor_llm:\n  mode: rank\n  model: small\n  endpoint: http://gw.local/\n  api_key_env: ADV_LLM_TEST_KEY\n"
	if err := yaml.Unmarshal([]byte(src), &k); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADV_LLM_TEST_KEY", "k")
	r, ok := (&Config{Knowledge: k}).KnowledgeAdvisorLLM()
	if !ok {
		t.Fatal("ok = false")
	}
	want := ResolvedAdvisorLLM{Mode: "rank", Model: "small", Endpoint: "http://gw.local", APIKey: "k",
		Candidates: 5, PaneLines: 80, TimeoutS: 15, MaxCallsPerAgentHour: 6, MaxCallsPerDay: 300, OnError: "silence"}
	if r != want {
		t.Fatalf("got %+v, want %+v", r, want)
	}
}

func TestKnowledgeAdvisorLLM_RequiresAdvisor(t *testing.T) {
	t.Setenv("ADV_LLM_TEST_KEY", "k")
	for _, mode := range []string{"off", "shadow", "rank"} {
		c := advisorLLMCfg(AdvisorLLMConfig{Mode: mode, Model: "m", Endpoint: "http://gw.local", APIKeyEnv: "ADV_LLM_TEST_KEY"})
		c.Knowledge.Advisor = false
		if _, ok := c.KnowledgeAdvisorLLM(); ok {
			t.Errorf("mode %q: ok = true with knowledge.advisor false", mode)
		}
	}
	var nilCfg *Config
	if _, ok := nilCfg.KnowledgeAdvisorLLM(); ok {
		t.Error("nil config ok = true")
	}
}

func TestKnowledgeAdvisorLLM_ResolutionPrecedence(t *testing.T) {
	t.Setenv(LiteLLMEndpointEnv, "")
	t.Setenv(DefaultLiteLLMAPIKeyEnv, "")
	t.Setenv("ADV_LLM_TEST_KEY", "explicit-key")
	t.Setenv("ADV_LLM_GOV_KEY", "gov-key")

	gov := func(c *Config) {
		c.Governor.LiteLLM = LiteLLMConfig{Endpoint: "http://gov.local", APIKeyEnv: "ADV_LLM_GOV_KEY", DefaultModel: "gov-model"}
	}
	t.Run("explicit wins", func(t *testing.T) {
		c := advisorLLMCfg(AdvisorLLMConfig{Mode: "rank", Model: "own", Endpoint: "http://own.local", APIKeyEnv: "ADV_LLM_TEST_KEY"})
		gov(c)
		r, ok := c.KnowledgeAdvisorLLM()
		if !ok || r.Endpoint != "http://own.local" || r.APIKey != "explicit-key" || r.Model != "own" {
			t.Fatalf("got %+v ok=%v", r, ok)
		}
	})
	t.Run("absent falls back to governor.litellm", func(t *testing.T) {
		c := advisorLLMCfg(AdvisorLLMConfig{Mode: "rank"})
		gov(c)
		r, ok := c.KnowledgeAdvisorLLM()
		if !ok || r.Endpoint != "http://gov.local" || r.APIKey != "gov-key" || r.Model != "gov-model" || r.Mode != "rank" {
			t.Fatalf("got %+v ok=%v", r, ok)
		}
	})
	t.Run("both absent", func(t *testing.T) {
		c := advisorLLMCfg(AdvisorLLMConfig{Mode: "rank", Model: "own"})
		if _, ok := c.KnowledgeAdvisorLLM(); ok {
			t.Fatal("ok = true with no endpoint or key anywhere")
		}
	})
}
