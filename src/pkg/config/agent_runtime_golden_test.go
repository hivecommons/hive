package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestAgentRuntimeConfigGolden pins the parse of a representative hive.yaml
// that exercises every agent-runtime key (hivecommons/hive#13536). The
// golden JSON was captured BEFORE the keys were grouped into
// AgentRuntimeConfig, so a field-for-field match here proves the regrouping
// changed neither the YAML shape nor any default. Regenerate deliberately
// with HIVE_UPDATE_GOLDEN=1 after an intentional schema change.
func TestAgentRuntimeConfigGolden(t *testing.T) {
	yamlPath := filepath.Join("testdata", "agent_runtime_golden.yaml")
	goldenPath := filepath.Join("testdata", "agent_runtime_golden.json")

	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	cfg.applyDefaults()

	got := marshalIndentJSON(t, cfg)
	if os.Getenv("HIVE_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (set HIVE_UPDATE_GOLDEN=1 to create): %v", err)
	}
	assertJSONEqual(t, want, got)

	// Pre-refactor the fleet cap lived beside the agent map; it must still
	// round-trip through the agents.github_api_hourly_cap scalar.
	if cfg.AgentsGitHubAPIHourlyCap != 120 || !cfg.agentsGitHubAPIHourlyCapSet {
		t.Fatalf("agents.github_api_hourly_cap not decoded: %d set=%v", cfg.AgentsGitHubAPIHourlyCap, cfg.agentsGitHubAPIHourlyCapSet)
	}
	if got := cfg.AgentGitHubAPIHourlyCap("supervisor"); got != 60 {
		t.Fatalf("per-agent cap = %d, want 60", got)
	}

	// Save/Load symmetry: re-marshal and re-parse must land on the same
	// struct, and the on-disk shape must keep the runtime keys where they
	// were (top-level and under governor:), not under a new nesting key.
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]yaml.Node
	if err := yaml.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"agents", "backends", "providers", "tool_approval", "agent_sandbox", "turn", "governor"} {
		if _, ok := top[key]; !ok {
			t.Errorf("re-marshaled YAML lost top-level key %q", key)
		}
	}
	var gov map[string]yaml.Node
	govNode := top["governor"]
	if err := govNode.Decode(&gov); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"watchdog", "litellm", "vllm", "llm-d", "bob", "trajectory", "kick_limits", "gateways", "rotation", "provider_budget"} {
		if _, ok := gov[key]; !ok {
			t.Errorf("re-marshaled YAML lost governor.%s", key)
		}
	}
	for _, forbidden := range []string{"agent_runtime:", "AgentRuntimeConfig"} {
		if strings.Contains(string(out), forbidden) {
			t.Errorf("re-marshaled YAML grew a new nesting key %q:\n%s", forbidden, out)
		}
	}
	var again Config
	if err := yaml.Unmarshal(out, &again); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	again.applyDefaults()
	assertJSONEqual(t, got, marshalIndentJSON(t, again))
}

func marshalIndentJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func assertJSONEqual(t *testing.T, want, got []byte) {
	t.Helper()
	if bytes.Equal(want, got) {
		return
	}
	var w, g map[string]any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	var diffs []string
	diffJSON("", w, g, &diffs)
	if len(diffs) > 0 {
		t.Fatalf("parse differs from golden:\n  %s", strings.Join(diffs, "\n  "))
	}
}

func diffJSON(path string, want, got any, diffs *[]string) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			*diffs = append(*diffs, path+": want object, got "+typeName(got))
			return
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok {
				*diffs = append(*diffs, path+"."+k+": missing")
				continue
			}
			diffJSON(path+"."+k, wv, gv, diffs)
		}
		for k := range g {
			if _, ok := w[k]; !ok {
				*diffs = append(*diffs, path+"."+k+": unexpected")
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok && len(w) == 0 && emptyJSON(got) {
			return
		}
		if !ok || len(g) != len(w) {
			*diffs = append(*diffs, path+": list differs")
			return
		}
		for i := range w {
			diffJSON(path+"["+string(rune('0'+i))+"]", w[i], g[i], diffs)
		}
	default:
		if emptyJSON(want) && emptyJSON(got) {
			return
		}
		if !reflect.DeepEqual(want, got) {
			*diffs = append(*diffs, path+": want "+stringify(want)+", got "+stringify(got))
		}
	}
}

// emptyJSON treats null, [] and {} as the same value: yaml.v3 encodes a nil
// slice as [] so a Save/Load round trip legitimately turns null into empty.
func emptyJSON(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func typeName(v any) string { return reflect.TypeOf(v).String() }

func stringify(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
