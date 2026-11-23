package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/hivecommons/hive/crux/keystore"
)

// Defaults and hard ceilings for knowledge.advisor_llm (hivecommons/hive#17054).
const (
	AdvisorLLMModeOff    = "off"
	AdvisorLLMModeShadow = "shadow"
	AdvisorLLMModeRank   = "rank"

	AdvisorLLMOnErrorSilence = "silence"
	AdvisorLLMOnErrorMatcher = "matcher"

	DefaultAdvisorLLMCandidates       = 5
	DefaultAdvisorLLMPaneLines        = 80
	DefaultAdvisorLLMTimeoutS         = 15
	AdvisorLLMMaxCallsPerAgentHourCap = 6
	AdvisorLLMMaxCallsPerDayCap       = 300

	// AdvisorLLMMaxErrorsPerAgentHourCap and AdvisorLLMMaxErrorsPerDayCap
	// bound the calls the endpoint refused (hivecommons/hive#17054 evidence
	// pass 2). A refused call spends no tokens, so it is handed back to the
	// call budget and charged to this separate error ledger instead; the
	// error ledger never exceeds these ceilings, even with budget_override,
	// so a broken endpoint is still asked at most this often.
	AdvisorLLMMaxErrorsPerAgentHourCap = AdvisorLLMMaxCallsPerAgentHourCap
	AdvisorLLMMaxErrorsPerDayCap       = AdvisorLLMMaxCallsPerDayCap
)

// AdvisorLLMConfig is the knowledge.advisor_llm block. Zero numeric values
// take the defaults above; negative values are load errors.
type AdvisorLLMConfig struct {
	Mode                 string `yaml:"mode,omitempty"`
	Model                string `yaml:"model,omitempty"`
	Endpoint             string `yaml:"endpoint,omitempty"`
	APIKeyEnv            string `yaml:"api_key_env,omitempty"`
	APIKeyFile           string `yaml:"api_key_file,omitempty"`
	Candidates           int    `yaml:"candidates,omitempty"`
	PaneLines            int    `yaml:"pane_lines,omitempty"`
	TimeoutS             int    `yaml:"timeout_s,omitempty"`
	MaxCallsPerAgentHour int    `yaml:"max_calls_per_agent_hour,omitempty"`
	MaxCallsPerDay       int    `yaml:"max_calls_per_day,omitempty"`
	BudgetOverride       bool   `yaml:"budget_override,omitempty"`
	HoldoutPercent       int    `yaml:"holdout_percent,omitempty"`
	OnError              string `yaml:"on_error,omitempty"`
}

// ResolvedAdvisorLLM is the effective advisor_llm setting with defaults and
// the governor.litellm fallbacks applied. The key value is read from a file or
// env var and is never stored in hive.yaml.
type ResolvedAdvisorLLM struct {
	Mode                 string
	Model                string
	Endpoint             string
	APIKey               string
	Candidates           int
	PaneLines            int
	TimeoutS             int
	MaxCallsPerAgentHour int
	MaxCallsPerDay       int
	HoldoutPercent       int
	OnError              string
}

func orIntDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// advisorLLMModel is the explicit model, else governor.litellm's default model.
func (c *Config) advisorLLMModel() string {
	if m := strings.TrimSpace(c.Knowledge.AdvisorLLM.Model); m != "" {
		return m
	}
	return strings.TrimSpace(c.Governor.LiteLLM.DefaultModel)
}

// validateKnowledgeAdvisorLLM returns load errors for malformed values and
// warnings for settings that are tolerated but resolve to off.
func (c *Config) validateKnowledgeAdvisorLLM() (warnings []string, err error) {
	a := c.Knowledge.AdvisorLLM
	switch a.Mode {
	case "", AdvisorLLMModeOff, AdvisorLLMModeShadow, AdvisorLLMModeRank:
	default:
		return nil, fmt.Errorf("knowledge.advisor_llm.mode: %q is not %q, %q or %q", a.Mode, AdvisorLLMModeOff, AdvisorLLMModeShadow, AdvisorLLMModeRank)
	}
	switch a.OnError {
	case "", AdvisorLLMOnErrorSilence, AdvisorLLMOnErrorMatcher:
	default:
		return nil, fmt.Errorf("knowledge.advisor_llm.on_error: %q is not %q or %q", a.OnError, AdvisorLLMOnErrorSilence, AdvisorLLMOnErrorMatcher)
	}
	for _, f := range []struct {
		key string
		v   int
	}{
		{"candidates", a.Candidates},
		{"pane_lines", a.PaneLines},
		{"timeout_s", a.TimeoutS},
		{"max_calls_per_agent_hour", a.MaxCallsPerAgentHour},
		{"max_calls_per_day", a.MaxCallsPerDay},
	} {
		if f.v < 0 {
			return nil, fmt.Errorf("knowledge.advisor_llm.%s: %d must not be negative (omit or 0 for the default)", f.key, f.v)
		}
	}
	if a.HoldoutPercent < 0 || a.HoldoutPercent > 100 {
		return nil, fmt.Errorf("knowledge.advisor_llm.holdout_percent: %d must be between 0 and 100", a.HoldoutPercent)
	}
	if !a.BudgetOverride {
		if a.MaxCallsPerAgentHour > AdvisorLLMMaxCallsPerAgentHourCap {
			return nil, fmt.Errorf("knowledge.advisor_llm.max_calls_per_agent_hour: %d exceeds the hard ceiling of %d; set budget_override: true to raise it", a.MaxCallsPerAgentHour, AdvisorLLMMaxCallsPerAgentHourCap)
		}
		if a.MaxCallsPerDay > AdvisorLLMMaxCallsPerDayCap {
			return nil, fmt.Errorf("knowledge.advisor_llm.max_calls_per_day: %d exceeds the hard ceiling of %d; set budget_override: true to raise it", a.MaxCallsPerDay, AdvisorLLMMaxCallsPerDayCap)
		}
	}
	if (a.Mode == AdvisorLLMModeShadow || a.Mode == AdvisorLLMModeRank) && c.advisorLLMModel() == "" {
		warnings = append(warnings, fmt.Sprintf("knowledge.advisor_llm.mode %q has no model (set knowledge.advisor_llm.model); resolving to %q", a.Mode, AdvisorLLMModeOff))
	}
	return warnings, nil
}

// KnowledgeAdvisorLLM returns the effective advisor_llm setting. ok is false
// when knowledge.advisor is off (whatever the mode), or when no endpoint or key
// resolves from advisor_llm or governor.litellm. Mode is "off" when shadow/rank
// has no model. Precedence per field is explicit → governor.litellm → unset,
// matching GovernorConfig.ResolveReviewer.
func (c *Config) KnowledgeAdvisorLLM() (ResolvedAdvisorLLM, bool) {
	if c == nil || !c.Knowledge.Advisor {
		return ResolvedAdvisorLLM{}, false
	}
	a := c.Knowledge.AdvisorLLM
	endpoint := strings.TrimRight(strings.TrimSpace(a.Endpoint), "/")
	if endpoint == "" {
		endpoint = c.Governor.LiteLLM.ResolveEndpoint()
	}
	apiKey := resolveKeyFromFileThenEnv(a.APIKeyFile, a.APIKeyEnv)
	if apiKey == "" {
		apiKey = c.Governor.LiteLLM.ResolveAPIKey()
	}
	if endpoint == "" || apiKey == "" {
		return ResolvedAdvisorLLM{}, false
	}
	r := ResolvedAdvisorLLM{
		Mode:                 strings.TrimSpace(a.Mode),
		Model:                c.advisorLLMModel(),
		Endpoint:             endpoint,
		APIKey:               apiKey,
		Candidates:           orIntDefault(a.Candidates, DefaultAdvisorLLMCandidates),
		PaneLines:            orIntDefault(a.PaneLines, DefaultAdvisorLLMPaneLines),
		TimeoutS:             orIntDefault(a.TimeoutS, DefaultAdvisorLLMTimeoutS),
		MaxCallsPerAgentHour: orIntDefault(a.MaxCallsPerAgentHour, AdvisorLLMMaxCallsPerAgentHourCap),
		MaxCallsPerDay:       orIntDefault(a.MaxCallsPerDay, AdvisorLLMMaxCallsPerDayCap),
		HoldoutPercent:       a.HoldoutPercent,
		OnError:              strings.TrimSpace(a.OnError),
	}
	if r.Mode == "" || r.Model == "" {
		r.Mode = AdvisorLLMModeOff
	}
	if r.OnError == "" {
		r.OnError = AdvisorLLMOnErrorSilence
	}
	return r, true
}

// KnowledgeAdvisorLLMProblem returns the operator-facing reason the ranking
// pass cannot make a model call although knowledge.advisor is on and
// advisor_llm.mode asks for shadow or rank, or "" when it can (or is not
// asked to). It names config keys and paths only, never a key value. Boot
// logs it at WARN and GET /api/knowledge/advisor/summary returns it, so a
// misconfiguration is visible instead of resolving silently to off.
func (c *Config) KnowledgeAdvisorLLMProblem() string {
	if c == nil || !c.Knowledge.Advisor {
		return ""
	}
	a := c.Knowledge.AdvisorLLM
	mode := strings.TrimSpace(a.Mode)
	if mode != AdvisorLLMModeShadow && mode != AdvisorLLMModeRank {
		return ""
	}
	if c.advisorLLMModel() == "" {
		return fmt.Sprintf("knowledge.advisor_llm.mode is %q but no model resolves: set knowledge.advisor_llm.model (or governor.litellm.default_model)", mode)
	}
	endpoint := strings.TrimRight(strings.TrimSpace(a.Endpoint), "/")
	if endpoint == "" {
		endpoint = c.Governor.LiteLLM.ResolveEndpoint()
	}
	if endpoint == "" {
		return fmt.Sprintf("knowledge.advisor_llm.mode is %q but no endpoint resolves: set knowledge.advisor_llm.endpoint (or governor.litellm.endpoint)", mode)
	}
	key, fileProblem := readKeyFile(a.APIKeyFile)
	if key == "" && a.APIKeyEnv != "" {
		key = os.Getenv(a.APIKeyEnv)
	}
	if key == "" {
		key = c.Governor.LiteLLM.ResolveAPIKey()
	}
	switch {
	case fileProblem != "":
		// Even when another source supplied a key, the configured file is
		// not the one in use: say so.
		return "knowledge.advisor_llm.api_key_file: " + fileProblem
	case key == "":
		return fmt.Sprintf("knowledge.advisor_llm.mode is %q but no API key resolves: set knowledge.advisor_llm.api_key_file or api_key_env (or a governor.litellm key)", mode)
	case keystore.IsEnvelopeString(key):
		return "knowledge.advisor_llm: the resolved API key is a sealed keystore envelope, not a token; check " + keystore.EnvSecretsKey + " on this hive"
	}
	return ""
}
