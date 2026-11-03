package main

import (
	"testing"
)

func TestSharedAgentPoolEnabledEnv(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"on", true},
	} {
		t.Setenv("HIVE_SHARED_AGENT_POOL_ENABLED", tc.val)
		if got := sharedAgentPoolEnabled(); got != tc.want {
			t.Errorf("sharedAgentPoolEnabled() for %q = %v, want %v", tc.val, got, tc.want)
		}
	}
}
