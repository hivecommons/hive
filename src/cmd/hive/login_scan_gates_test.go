package main

import (
	"context"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

func loginScanConfig(patterns []string) *config.Config {
	return &config.Config{
		Governor: config.GovernorConfig{
			Sensing: config.SensingConfig{LoginPatterns: patterns},
		},
	}
}

// The login detector must stand down COMPLETELY when it has no usable
// patterns — no pane reads, no pauses, no notifications. Passing nil for the
// manager, notifier and dashboard makes the invariant self-enforcing: any
// scan activity past the gate would panic.
func TestScanForLoginRequiredStandsDownWithoutUsablePatterns(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
	}{
		{"no patterns configured", nil},
		{"empty pattern list", []string{}},
		{"only blank patterns", []string{"", "   "}},
		{"only invalid regexes", []string{"(", "[unclosed"}},
		{"blank and invalid mixed", []string{"", "("}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Must return without touching any of the nil collaborators.
			scanForLoginRequired(context.Background(), loginScanConfig(tc.patterns),
				nil, nil, nil, restoreTestLogger())
		})
	}
}
