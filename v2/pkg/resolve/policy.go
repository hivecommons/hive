package resolve

// Scope declares which substitution system(s) an operator-defined variable
// serves. This split matters because config-load resolution (System A) runs
// pre-parse over the whole YAML with no runtime context, while per-kick
// resolution (System B) has rich runtime context. A script/http variable
// generally belongs to template scope; a static/env value can serve either.
type Scope string

const (
	// ScopeTemplate — resolved only during per-kick prompt templating. Default.
	ScopeTemplate Scope = "template"
	// ScopeConfig — resolved during config load (hive.yaml text expansion).
	ScopeConfig Scope = "config"
	// ScopeBoth — resolved in both systems.
	ScopeBoth Scope = "both"
)

// InScope reports whether a variable with the given declared scope participates
// in the requested system. An empty declared scope defaults to template.
func (s Scope) matches(want Scope) bool {
	switch s {
	case "", ScopeTemplate:
		return want == ScopeTemplate
	case ScopeConfig:
		return want == ScopeConfig
	case ScopeBoth:
		return true
	default:
		return false
	}
}

// VarSpec is one operator-declared variable from the config `variables.defs`
// block. Which fields are meaningful depends on Type; unrelated fields are
// ignored by each resolver's factory.
type VarSpec struct {
	// Name is the variable name (the map key in config), i.e. ${Name}.
	Name string
	// Type selects the resolver: "env" | "static" | "script" | "http". An empty
	// Type defaults to "static" when Value is set, else "env".
	Type string
	// Scope controls which substitution system this var serves.
	Scope Scope
	// Default is the fallback value used when the resolver would otherwise be
	// ErrUnresolved (e.g. env var unset). Empty means "no default" (leave literal).
	Default string
	// HasDefault distinguishes an explicit empty-string default from no default.
	HasDefault bool

	// Static.
	Value string
	// Env: the source environment variable name (defaults to Name if empty).
	Env string
	// Script: argv (NOT a shell string — no shell interpolation).
	Command []string
	// HTTP.
	URL     string
	Headers map[string]string
}

// Policy is the process-wide trust model for resolvers that execute code or do
// network I/O. It is built only from the cluster-trusted config seed, never
// from the dashboard overlay (the config package enforces that), so a
// compromised or user-edited overlay cannot enable script/http execution.
type Policy struct {
	// AllowExec gates script (exec) resolvers. Default false.
	AllowExec bool
	// AllowHTTP gates http resolvers. Default false.
	AllowHTTP bool
	// HTTPAllowlist is the set of hostnames an http resolver may contact.
	// Required (non-empty) for any http resolver to be built.
	HTTPAllowlist []string
	// ExecTimeoutS bounds a single script resolver's runtime. Default 5.
	ExecTimeoutS int
	// HTTPTimeoutS bounds a single http resolver's request. Default 5.
	HTTPTimeoutS int
}

// Default timeouts (seconds) applied when a Policy leaves them at zero.
const (
	DefaultExecTimeoutS = 5
	DefaultHTTPTimeoutS = 5
)

// withDefaults returns a copy of the policy with zero timeouts filled in.
func (p Policy) withDefaults() Policy {
	if p.ExecTimeoutS <= 0 {
		p.ExecTimeoutS = DefaultExecTimeoutS
	}
	if p.HTTPTimeoutS <= 0 {
		p.HTTPTimeoutS = DefaultHTTPTimeoutS
	}
	return p
}
