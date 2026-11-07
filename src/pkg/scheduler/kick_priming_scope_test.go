package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestKickContextFor(t *testing.T) {
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{
		Agents: map[string]config.AgentConfig{
			"developer":   {},
			"developer-2": {ReplicaOf: "developer"},
		},
	}}
	tests := []struct {
		name       string
		cfg        *config.Config
		agent      string
		issues     []github.Issue
		wantLane   string
		wantLabels string
	}{
		{
			name:       "lane and de-duplicated lower-cased labels",
			cfg:        cfg,
			agent:      "developer",
			issues:     []github.Issue{{Labels: []string{"Area/CI", "kind/bug"}}, {Labels: []string{"area/ci", " docs "}}},
			wantLane:   "developer",
			wantLabels: "area/ci,kind/bug,docs",
		},
		{
			name:     "replica resolves to its base lane",
			cfg:      cfg,
			agent:    "developer-2",
			issues:   nil,
			wantLane: "developer",
		},
		{
			name:     "nil config keeps the agent name",
			cfg:      nil,
			agent:    "reviewer",
			wantLane: "reviewer",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kc := kickContextFor(tc.cfg, tc.agent, tc.issues)
			if kc.Lane != tc.wantLane {
				t.Fatalf("Lane = %q, want %q", kc.Lane, tc.wantLane)
			}
			if got := strings.Join(kc.Labels, ","); got != tc.wantLabels {
				t.Fatalf("Labels = %q, want %q", got, tc.wantLabels)
			}
			if len(kc.Paths) != 0 {
				t.Fatalf("Paths must stay empty on the kick path, got %v", kc.Paths)
			}
		})
	}
}
