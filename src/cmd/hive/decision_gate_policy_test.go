package main

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// decisionGateFor must never hand the automerge engine an open gate when the
// config is missing or the key is unset (hivecommons/hive#19741).
func TestDecisionGateForNilAndUnsetConfigUseSafeDefault(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"nil config": nil,
		"unset key":  {},
	} {
		t.Run(name, func(t *testing.T) {
			got := decisionGateFor(cfg, "hivecommons/hive")
			if !got.Enabled {
				t.Fatal("gate disabled by default, want on")
			}
			if !got.Sentinel {
				t.Fatal("sentinel off by default, want on")
			}
			if !reflect.DeepEqual(got.Paths, config.DefaultDecisionGatePaths) {
				t.Fatalf("paths = %v, want DefaultDecisionGatePaths", got.Paths)
			}
		})
	}
}

func TestDecisionGateForHonorsExplicitConfig(t *testing.T) {
	off := false
	cfg := &config.Config{}
	cfg.AutoMerge.DecisionGate = config.DecisionGateConfig{
		Enabled:  &off,
		Paths:    []string{"**/vault/**"},
		Sentinel: &off,
	}
	got := decisionGateFor(cfg, "hivecommons/hive")
	if got.Enabled || got.Sentinel {
		t.Fatalf("explicit false not honored: %+v", got)
	}
	if !reflect.DeepEqual(got.Paths, []string{"**/vault/**"}) {
		t.Fatalf("paths = %v, want the explicit list", got.Paths)
	}

	// The policy's Paths is a copy: mutating it must not reach the config.
	got.Paths[0] = "mutated"
	if cfg.AutoMerge.DecisionGate.Paths[0] != "**/vault/**" {
		t.Fatal("decisionGateFor returned the config's backing slice")
	}
}
