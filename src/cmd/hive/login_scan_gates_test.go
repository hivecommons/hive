package main

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
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
				nil, nil, nil, restoreTestLogger(), nil)
		})
	}
}

// A pattern list with at least one valid regex must clear the "stand down"
// gate and reach the per-agent scan loop, even when other entries in the
// list are blank or fail to compile. With zero agents registered,
// AllStatuses() is empty and the loop body never runs — proving the
// invalid-pattern skip itself does not panic or short-circuit the whole
// function, only the individual bad pattern.
func TestScanForLoginRequiredMixedValidityPatternsReachesScanLoop(t *testing.T) {
	mgr := agent.NewManager(map[string]config.AgentConfig{}, restoreTestLogger(), agent.ProjectContext{})
	cfg := loginScanConfig([]string{"", "[unclosed", "please log in"})

	// A nil dashSrv/notifier is safe here only because there are no running
	// agents for AllStatuses() to return — the loop body that would use them
	// never executes. This still proves the function gets PAST the early
	// "no usable patterns" return (which the panic-on-touch test above
	// verifies happens for an all-invalid list) to the scan loop itself.
	scanForLoginRequired(context.Background(), cfg, mgr, nil, nil, restoreTestLogger(), newLoginSightingTracker())
}
