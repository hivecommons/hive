package scheduler

import (
	"log/slog"
	"os"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestSubstituteTemplate_BuiltinsAndOperatorVars verifies the resolver-backed
// substituteTemplate: built-in ${VAR}s still render; an operator-defined
// template variable renders; a built-in wins over a same-named operator def;
// and an unknown ${VAR} is left literal (no env fallback in template scope).
func TestSubstituteTemplate_BuiltinsAndOperatorVars(t *testing.T) {
	// An env var whose name matches an unknown template ${VAR}: it must NOT be
	// substituted in template scope (only config scope falls back to env).
	t.Setenv("UNKNOWN_TEMPLATE_VAR", "leaked-from-env")

	dflt := "prod"
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "acme", Name: "widgets", Repos: []string{"widgets"}},
		Agents:  map[string]config.AgentConfig{"scanner": {Role: "scanner"}},
		Variables: config.VariablesConfig{
			Defs: map[string]config.VarDef{
				// Custom template variable.
				"DEPLOY_ENV": {Type: "static", Value: dflt, Scope: "template"},
				// Same name as a built-in — the built-in must win.
				"PROJECT_ORG": {Type: "static", Value: "SHOULD-NOT-WIN", Scope: "both"},
			},
		},
	}
	s := New(cfg, slog.Default())

	tmpl := "org=${PROJECT_ORG} deploy=${DEPLOY_ENV} agent=${AGENT_NAME} miss=${UNKNOWN_TEMPLATE_VAR}"
	out := s.substituteTemplate(tmpl, nil, "scanner", nil)

	if !contains(out, "org=acme") {
		t.Errorf("built-in PROJECT_ORG should win over operator def: %q", out)
	}
	if !contains(out, "deploy=prod") {
		t.Errorf("operator template var not substituted: %q", out)
	}
	if !contains(out, "agent=scanner") {
		t.Errorf("built-in AGENT_NAME not substituted: %q", out)
	}
	if !contains(out, "miss=${UNKNOWN_TEMPLATE_VAR}") {
		t.Errorf("unknown template var must stay literal (no env fallback): %q", out)
	}
	if contains(out, "leaked-from-env") {
		t.Errorf("template scope must NOT fall back to env: %q", out)
	}
}

// TestSubstituteTemplate_NoVariablesBlock is the byte-identical guard: with no
// `variables:` block, output matches building the same string by hand from the
// built-ins (an unknown var stays literal, exactly like the old replacer).
func TestSubstituteTemplate_NoVariablesBlock(t *testing.T) {
	os.Unsetenv("PROJECT_NAME") // ensure no accidental env influence
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "acme", Name: "widgets", Repos: []string{"widgets"}},
		Agents:  map[string]config.AgentConfig{"scanner": {Role: "scanner"}},
	}
	s := New(cfg, slog.Default())
	out := s.substituteTemplate("a=${PROJECT_ORG} b=${NOPE}", nil, "scanner", nil)
	if out != "a=acme b=${NOPE}" {
		t.Errorf("unexpected: %q", out)
	}
}
