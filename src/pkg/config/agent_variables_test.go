package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/resolve"
)

func agentVarsConfig() *Config {
	return &Config{
		Variables: VariablesConfig{
			Security: VarSecurityConfig{AllowExec: true},
			Defs: map[string]VarDef{
				"DEPLOY_ENV": {Type: "static", Value: "production"},
				"REGION":     {Type: "static", Value: "us-east-1"},
			},
		},
		Agents: map[string]AgentConfig{
			"scanner": {Variables: map[string]VarDef{
				"DEPLOY_ENV": {Type: "static", Value: "staging"},
				"LANE_GOAL":  {Type: "static", Value: "find bugs"},

				// Per-agent script defs are never honoured, even when the
				// seed enables exec for hive-level variables.
				"PWNED": {Type: "script", Command: []string{"echo", "pwned"}},
			}},
			"quality": {},
		},
	}
}

func TestResolveRegistryForAgent_MergesOverHiveLevel(t *testing.T) {
	cfg := agentVarsConfig()
	tests := []struct {
		name  string
		agent string
		in    string
		want  string
	}{
		{"agent wins on collision", "scanner", "${DEPLOY_ENV}", "staging"},
		{"inherits hive-level var", "scanner", "${REGION}", "us-east-1"},
		{"agent-only var", "scanner", "${LANE_GOAL}", "find bugs"},
		{"per-agent script ignored", "scanner", "${PWNED}", "${PWNED}"},
		{"agent without vars gets hive-level", "quality", "${DEPLOY_ENV}", "production"},
		{"other agent does not see scanner vars", "quality", "${LANE_GOAL}", "${LANE_GOAL}"},
		{"unknown agent gets hive-level", "ghost", "${DEPLOY_ENV}", "production"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := cfg.ResolveRegistryForAgent(tc.agent, nil)
			if got := reg.Expand(context.TODO(), tc.in, resolve.ScopeTemplate, nil); got != tc.want {
				t.Errorf("Expand(%q) for %s = %q, want %q", tc.in, tc.agent, got, tc.want)
			}
		})
	}
}

func TestEffectiveVariableDefs_DoesNotMutateHiveLevel(t *testing.T) {
	cfg := agentVarsConfig()
	defs := cfg.EffectiveVariableDefs("scanner")
	if defs["DEPLOY_ENV"].Value != "staging" {
		t.Fatalf("merged DEPLOY_ENV = %q, want staging", defs["DEPLOY_ENV"].Value)
	}
	if _, ok := defs["PWNED"]; ok {
		t.Error("per-agent script def must be dropped from the merged set")
	}
	if got := cfg.Variables.Defs["DEPLOY_ENV"].Value; got != "production" {
		t.Errorf("hive-level DEPLOY_ENV mutated to %q", got)
	}
	if _, ok := cfg.Variables.Defs["LANE_GOAL"]; ok {
		t.Error("agent var leaked into hive-level defs")
	}
}

func TestValidateAgentVariables(t *testing.T) {
	tests := []struct {
		name    string
		vars    map[string]VarDef
		wantErr string
	}{
		{"nil", nil, ""},
		{"static and env", map[string]VarDef{"A": {Type: "static", Value: "x"}, "B_2": {Type: "env", Env: "HOME"}}, ""},
		{"inferred type", map[string]VarDef{"A": {Value: "x"}}, ""},
		{"both scope", map[string]VarDef{"A": {Type: "static", Value: "x", Scope: "both"}}, ""},
		{"bad name", map[string]VarDef{"1BAD": {Type: "static", Value: "x"}}, "invalid name"},
		{"dash in name", map[string]VarDef{"A-B": {Type: "static", Value: "x"}}, "invalid name"},
		{"script rejected", map[string]VarDef{"A": {Type: "script", Command: []string{"echo"}}}, "not allowed per agent"},
		{"http rejected", map[string]VarDef{"A": {Type: "http", URL: "https://x"}}, "not allowed per agent"},
		{"config scope rejected", map[string]VarDef{"A": {Type: "static", Value: "x", Scope: "config"}}, "scope"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentVariables("scanner", tc.vars)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoad_AgentVariables(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "hive.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_x
variables:
  defs:
    DEPLOY_ENV:
      type: static
      value: production
agents:
  scanner:
    backend: copilot
    variables:
`
	t.Run("static parsed and merged", func(t *testing.T) {
		cfg, err := Load(write(t, base+"      DEPLOY_ENV:\n        type: static\n        value: staging\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		reg := cfg.ResolveRegistryForAgent("scanner", nil)
		if got := reg.Expand(context.TODO(), "${DEPLOY_ENV}", resolve.ScopeTemplate, nil); got != "staging" {
			t.Errorf("agent DEPLOY_ENV = %q, want staging", got)
		}
	})
	t.Run("script rejected", func(t *testing.T) {
		_, err := Load(write(t, base+"      X:\n        type: script\n        command: [echo, hi]\n"))
		if err == nil || !strings.Contains(err.Error(), "not allowed per agent") {
			t.Fatalf("Load error = %v, want per-agent type rejection", err)
		}
	})
}
