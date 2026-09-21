package dashboard

import (
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestQualifiedStandbyCountsPausedLane(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{"quality": {Standby: &config.StandbyConfig{MinModelCapability: "T2", DailyCapPerContributor: 1}}},
		Hub: config.HubConfig{
			StandbyContributors: []string{"alice"},
			StandbyModelTiers:   []config.StandbyModelTier{{Backend: "claude", Model: "opus", ReasoningEffort: "high", Tier: "T2"}},
		},
	}
	s := &Server{deps: &Dependencies{Config: cfg}, logger: slog.Default()}
	h := NewContributeWSHub(slog.Default(), s)
	h.mu.Lock()
	h.connections["c1"] = &ContributorConnection{
		profile:         &ContributorProfile{GitHubUsername: "alice"},
		cliBackend:      "claude",
		model:           "opus",
		reasoningEffort: "high",
		standby: map[string]StandbyConnectionState{"quality": {
			Lane:            "quality",
			CLIBackend:      "claude",
			Model:           "opus",
			ReasoningEffort: "high",
			DailyCap:        1,
			UpdatedAt:       time.Now(),
		}},
	}
	h.connections["c2"] = &ContributorConnection{
		profile:    &ContributorProfile{GitHubUsername: "mallory"},
		cliBackend: "claude",
		model:      "opus",
		standby:    map[string]StandbyConnectionState{"quality": {Lane: "quality", CLIBackend: "claude", Model: "opus"}},
	}
	h.mu.Unlock()
	counts := h.QualifiedStandbyCounts([]string{"quality"})
	if counts["quality"] != 1 {
		t.Fatalf("qualified quality count = %d, want 1", counts["quality"])
	}
}
