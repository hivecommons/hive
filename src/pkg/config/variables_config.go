package config

// IntentConfig controls intent-verification reporting and merge-gate
// enforcement. The zero value is report-only with built-in path patterns, so an
// absent intent: block preserves existing merge eligibility behavior.
type IntentConfig struct {
	Enforce               bool     `yaml:"enforce,omitempty" json:"enforce,omitempty"`
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

// PromptSourceConfig describes a GitHub repo location to pull an agent's kick
// prompt from. Owner/Repo/Path are required; Ref (branch/tag/SHA) is optional
// and defaults to the repo's default branch.
type PromptSourceConfig struct {
	// Type selects the source kind. Only "github" is currently supported; an
	// empty type defaults to "github" when a repo is set.
	Type  string `yaml:"type,omitempty" json:"type,omitempty"`
	Owner string `yaml:"owner,omitempty" json:"owner,omitempty"`
	Repo  string `yaml:"repo,omitempty" json:"repo,omitempty"`
	Path  string `yaml:"path,omitempty" json:"path,omitempty"`
	Ref   string `yaml:"ref,omitempty" json:"ref,omitempty"`
}

// Slug returns the "owner/repo" identifier used for allowlist matching, or ""
// when owner or repo is unset.
func (p *PromptSourceConfig) Slug() string {
	if p == nil || p.Owner == "" || p.Repo == "" {
		return ""
	}
	return p.Owner + "/" + p.Repo
}

// IsSet reports whether this prompt source is fully specified (owner, repo, and
// path all present). A partially-filled source is treated as unset so a kick
// falls back to the inline template rather than erroring.
func (p *PromptSourceConfig) IsSet() bool {
	return p != nil && p.Owner != "" && p.Repo != "" && p.Path != ""
}

// DefinitionSourceConfig describes a GitHub repo location to pull a WHOLE agent
// definition (the portable AgentDefinition YAML) from, keeping the agent "live"
// so edits on the repo propagate on the next reload/kick. It is the whole-agent
// analogue of PromptSourceConfig. Owner/Repo/Path are required; Ref is optional
// and defaults to the repo's default branch.
//
// Security: re-applying a live definition NEVER changes security-sensitive or
// seed-only fields (the resolver trust policy, token scopes, etc.). Only the
// operator-safe presentation/behavior fields are merged — see pkg/defsrc for the
// exact allowed-field boundary. The fetch is gated to the same seed-only repo
// allowlist as prompt_source (VarSecurityConfig.GitHubPromptAllowlist), so a
// user-writable dashboard overlay can neither widen the allowlist nor point an
// agent at an arbitrary repo the App happens to be installed on.
type DefinitionSourceConfig struct {
	// Type selects the source kind. Only "github" is currently supported; an
	// empty type defaults to "github" when a repo is set.
	Type  string `yaml:"type,omitempty" json:"type,omitempty"`
	Owner string `yaml:"owner,omitempty" json:"owner,omitempty"`
	Repo  string `yaml:"repo,omitempty" json:"repo,omitempty"`
	Path  string `yaml:"path,omitempty" json:"path,omitempty"`
	Ref   string `yaml:"ref,omitempty" json:"ref,omitempty"`
	// URL is the human-facing source URL the operator pasted in the import UI
	// (e.g. the github.com blob URL). Informational only — Owner/Repo/Path/Ref
	// are authoritative for fetching. Kept so the UI can round-trip it.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
}

// Slug returns the "owner/repo" identifier used for allowlist matching, or ""
// when owner or repo is unset.
func (d *DefinitionSourceConfig) Slug() string {
	if d == nil || d.Owner == "" || d.Repo == "" {
		return ""
	}
	return d.Owner + "/" + d.Repo
}

// IsSet reports whether this definition source is fully specified (owner, repo,
// and path all present). A partially-filled source is treated as unset so an
// agent keeps its baked definition rather than erroring.
func (d *DefinitionSourceConfig) IsSet() bool {
	return d != nil && d.Owner != "" && d.Repo != "" && d.Path != ""
}

// VarDef is one operator-declared variable. `default` uses a pointer so an
// explicit empty-string default is distinguishable from "no default".
type VarDef struct {
	Type    string            `yaml:"type,omitempty"`  // env|static|script|http
	Scope   string            `yaml:"scope,omitempty"` // config|template|both (default template)
	Default *string           `yaml:"default,omitempty"`
	Value   string            `yaml:"value,omitempty"`   // static
	Env     string            `yaml:"env,omitempty"`     // env source var name
	Command []string          `yaml:"command,omitempty"` // script argv
	URL     string            `yaml:"url,omitempty"`     // http
	Headers map[string]string `yaml:"headers,omitempty"` // http
}
