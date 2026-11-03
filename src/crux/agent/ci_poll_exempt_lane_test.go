package agent

import (
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

func TestCIPollNudgeExemptAgentLaneVariants(t *testing.T) {
	for name, want := range map[string]bool{
		"ci-maintainer":      true,
		"ci-maintainer-2":    true,
		"ci-maintainer_full": true,
		"Quality-advisory":   true,
		"scanner":            false,
		"ci-maintainers":     false,
	} {
		a := &AgentProcess{Name: name, Config: config.AgentConfig{Role: name}}
		if got := ciPollNudgeExemptAgent(a); got != want {
			t.Errorf("%q: got %v want %v", name, got, want)
		}
	}
}
