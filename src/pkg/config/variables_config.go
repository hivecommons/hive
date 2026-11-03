package config

// IntentConfig controls intent-verification reporting and merge-gate
// enforcement. The zero value is report-only with built-in path patterns, so an
// absent intent: block preserves existing merge eligibility behavior.
type IntentConfig struct {
	Enforce bool `yaml:"enforce,omitempty" json:"enforce,omitempty"`
	// Shadow evaluates the intent gate as if enforce were on without acting
	// on it, recording shadow.intent.would_have_blocked on the evidence
	// bundle. It never blocks a merge and is ignored when Enforce is on.
	Shadow                bool     `yaml:"shadow,omitempty" json:"shadow,omitempty"`
	AlignmentModel        string   `yaml:"alignment_model,omitempty" json:"alignment_model,omitempty"`
	TestPathPatterns      []string `yaml:"test_path_patterns,omitempty" json:"test_path_patterns,omitempty"`
	DocsPathPatterns      []string `yaml:"docs_path_patterns,omitempty" json:"docs_path_patterns,omitempty"`
	GuardrailPathPatterns []string `yaml:"guardrail_path_patterns,omitempty" json:"guardrail_path_patterns,omitempty"`
	FeatureSignals        []string `yaml:"feature_signals,omitempty" json:"feature_signals,omitempty"`
}

// VariablesConfig declares operator-defined ${VAR} substitutions and the trust
// policy for resolvers that execute code or do network I/O. It drives the
// pluggable resolve engine (pkg/resolve) used by both config-load and per-kick
// template substitution. Absent (the default) means env-only substitution,
// byte-identical to hive's legacy behavior.
type VariablesConfig struct {
	// Security gates script/http resolvers. Honored ONLY from the trusted config
	// seed — the dashboard overlay's Security block is discarded on load so a
	// user-editable overlay can never enable code execution or network access.
	Security VarSecurityConfig `yaml:"security,omitempty"`
	// Defs maps a variable name (used as ${name}) to its definition.
	Defs map[string]VarDef `yaml:"defs,omitempty"`
}

// VarSecurityConfig is the resolver trust model. Defaults are deny.
type VarSecurityConfig struct {
	AllowExec     bool     `yaml:"allow_exec,omitempty"`
	AllowHTTP     bool     `yaml:"allow_http,omitempty"`
	HTTPAllowlist []string `yaml:"http_allowlist,omitempty"`
	ExecTimeoutS  int      `yaml:"exec_timeout_s,omitempty"`
	HTTPTimeoutS  int      `yaml:"http_timeout_s,omitempty"`

	// AllowGitHubPrompt gates the GitHub-repo prompt-source feature (agents may
	// source their kick prompt from a repo). Like AllowExec/AllowHTTP it is honored
	// ONLY from the trusted config seed — the dashboard overlay's Security block is
	// discarded on load, so a user-editable overlay can never enable this or widen
	// the allowlist. Default false (deny).
	AllowGitHubPrompt bool `yaml:"allow_github_prompt,omitempty"`
	// GitHubPromptAllowlist is the set of "owner/repo" slugs an agent's
	// prompt_source may read from. Required (non-empty) for the feature to work:
	// an empty allowlist denies all repos even when AllowGitHubPrompt is true. This
	// bounds the blast radius to repos the operator explicitly trusts, so the
	// feature cannot be used to read every repo the App happens to be installed on.
	GitHubPromptAllowlist []string `yaml:"github_prompt_allowlist,omitempty"`
}
