package config

import (
	"os"
	"regexp"
	"testing"
)

// legacyExpandEnvVars is the exact pre-resolver implementation, kept here to
// prove the new resolver-backed expandEnvVars is byte-identical for configs
// without a `variables:` block.
func legacyExpandEnvVars(s string) string {
	pat := regexp.MustCompile(`\$\{([^}]+)\}`)
	return pat.ReplaceAllStringFunc(s, func(match string) string {
		varName := match[2 : len(match)-1]
		if val, ok := os.LookupEnv(varName); ok {
			return val
		}
		return match
	})
}

// TestExpandEnvVars_ByteIdenticalToLegacy runs a battery of inputs through both
// the legacy and the new (resolver-delegating) expandEnvVars and asserts
// identical output. Guards the "no behavior change" contract of PR 1.
func TestExpandEnvVars_ByteIdenticalToLegacy(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "ghp_secret123")
	t.Setenv("SLACK_WEBHOOK_URL", "https://hooks.example/xyz")
	t.Setenv("EMPTY_VAR", "")
	os.Unsetenv("MISSING_VAR")

	inputs := []string{
		"",
		"plain text no vars",
		"github:\n  token: ${HIVE_GITHUB_TOKEN}\n",
		"webhook: ${SLACK_WEBHOOK_URL}",
		"${MISSING_VAR} stays literal",
		"empty=[${EMPTY_VAR}]",
		"a ${HIVE_GITHUB_TOKEN} b ${MISSING_VAR} c ${EMPTY_VAR} d",
		"nested-ish ${${HIVE_GITHUB_TOKEN}}",  // regex is non-recursive; must match legacy
		"kick uses ${ISSUE_LIST} and ${AGENT_NAME}", // template vars unset at config time -> literal
		"$notavar ${} ${ x }",                       // odd shapes
	}
	for _, in := range inputs {
		got := expandEnvVars(in)
		want := legacyExpandEnvVars(in)
		if got != want {
			t.Errorf("expandEnvVars(%q)\n  got  %q\n  want %q", in, got, want)
		}
	}
}

// TestExpandEnvVars_WithVariablesBlock verifies that a config that DOES declare
// a variables block gets operator substitutions, while everything else still
// matches env behavior.
func TestExpandEnvVars_WithVariablesBlock(t *testing.T) {
	os.Unsetenv("HIVE_REGION")
	raw := `
variables:
  defs:
    DEPLOY_ENV:
      type: static
      value: production
      scope: both
    REGION:
      type: env
      env: HIVE_REGION
      default: us-east-1
      scope: config
project:
  org: testorg
  name: ${DEPLOY_ENV}
  region_note: ${REGION}
  missing: ${STILL_MISSING}
`
	out := expandEnvVars(raw)
	if !contains(out, "name: production") {
		t.Errorf("static var not substituted:\n%s", out)
	}
	if !contains(out, "region_note: us-east-1") {
		t.Errorf("env default not substituted:\n%s", out)
	}
	if !contains(out, "missing: ${STILL_MISSING}") {
		t.Errorf("unknown var should stay literal:\n%s", out)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
