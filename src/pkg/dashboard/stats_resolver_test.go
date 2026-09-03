package dashboard

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestResolveStatsSources_NativeSourcesUnchanged is the console-baseline guard:
// the dashboard-native stat sources (agentMetrics, health, tokens, status) must
// pass through resolveStatsSources completely untouched, so existing stats keep
// resolving to the exact same values on the client. Mirrors the real console
// hive stat shapes captured as a baseline.
func TestResolveStatsSources_NativeSourcesUnchanged(t *testing.T) {
	cfg := &config.Config{
		Variables: config.VariablesConfig{Defs: map[string]config.VarDef{
			"DEPLOY_ENV": {Type: "static", Value: "production"},
		}},
	}
	stats := []any{
		map[string]any{"key": "actionable", "label": "Actionable", "source": "status", "field": "actionableCount", "style": "spark"},
		map[string]any{"key": "coverage", "label": "Coverage", "source": "agentMetrics", "field": "coverage", "style": "pct-bar", "target": 91},
		map[string]any{"key": "brew", "label": "Brew", "source": "health", "field": "brew", "style": "dot"},
		map[string]any{"key": "stars", "label": "Stars", "source": "agentMetrics", "field": "stars", "style": "spark"},
	}
	// Deep copy for comparison.
	want := deepCopyStats(stats)

	got := resolveStatsSources(stats, cfg)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("native-source stats must pass through unchanged\n got  %v\n want %v", got, want)
	}
	// None of them should have gained a server-computed value.
	for _, s := range got {
		if _, ok := s.(map[string]any)["value"]; ok {
			t.Errorf("native-source stat unexpectedly got a value: %v", s)
		}
	}
}

// TestResolveStatsSources_ResolverBackedFilled proves a resolver-backed stat
// gets its value from the config's variable-resolution registry.
func TestResolveStatsSources_ResolverBackedFilled(t *testing.T) {
	cfg := &config.Config{
		Variables: config.VariablesConfig{Defs: map[string]config.VarDef{
			"RELEASE": {Type: "static", Value: "v1.2.3"},
		}},
	}
	stats := []any{
		map[string]any{"key": "rel", "label": "Release", "source": "resolver", "field": "RELEASE", "style": "text"},
		map[string]any{"key": "missing", "label": "Missing", "source": "resolver", "field": "NOPE", "style": "text"},
	}
	got := resolveStatsSources(stats, cfg)

	if v := got[0].(map[string]any)["value"]; v != "v1.2.3" {
		t.Errorf("resolver stat value: got %v want v1.2.3", v)
	}
	if _, ok := got[1].(map[string]any)["value"]; ok {
		t.Error("unresolvable resolver stat should get no value (render blank)")
	}
}

// TestResolveStatsSources_NilCfgSafe: no config -> passthrough, no panic.
func TestResolveStatsSources_NilCfgSafe(t *testing.T) {
	stats := []any{map[string]any{"source": "resolver", "field": "X"}}
	if got := resolveStatsSources(stats, nil); !reflect.DeepEqual(got, stats) {
		t.Error("nil cfg should pass stats through unchanged")
	}
}

func deepCopyStats(stats []any) []any {
	out := make([]any, len(stats))
	for i, s := range stats {
		m := s.(map[string]any)
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		out[i] = cp
	}
	return out
}
