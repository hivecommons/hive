package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestWriteSurfaceEnforcedLanes(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"scanner":   {},
		"scanner-2": {ReplicaOf: "scanner"},
		"reviewer":  {},
	}
	cases := []struct {
		name    string
		cfg     *config.Config
		want    []string
		wantLen int
	}{
		{
			name: "nothing enforced by default",
			cfg:  &config.Config{Agents: agents},
			want: nil,
		},
		{
			name: "a listed lane carries its replicas",
			cfg: &config.Config{Agents: agents, WriteSurface: config.WriteSurfaceConfig{
				Enforce: []string{"scanner"},
			}},
			want: []string{"scanner", "scanner-2"},
		},
		{
			name: "matching ignores case and surrounding space",
			cfg: &config.Config{Agents: agents, WriteSurface: config.WriteSurfaceConfig{
				Enforce: []string{" REVIEWER "},
			}},
			want: []string{"reviewer"},
		},
		{
			name: "a star is published as a star, not expanded",
			cfg: &config.Config{Agents: agents, WriteSurface: config.WriteSurfaceConfig{
				Enforce: []string{"scanner", "*"},
			}},
			want: []string{"*"},
		},
		{
			name: "a lane naming no configured agent enforces nothing",
			cfg: &config.Config{Agents: agents, WriteSurface: config.WriteSurfaceConfig{
				Enforce: []string{"typo-lane"},
			}},
			want: nil,
		},
		{
			name: "nil config",
			cfg:  nil,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := writeSurfaceEnforcedLanes(tc.cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("writeSurfaceEnforcedLanes() = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("writeSurfaceEnforcedLanes() = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// The resolved list is what the sandbox compares its own lane against, so a
// replica must be able to find its own name in the published file - it cannot
// re-derive "scanner-2 follows scanner" for itself.
func TestPublishedEnforceListNamesReplicasIndividually(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner":   {},
			"scanner-2": {ReplicaOf: "scanner"},
			"reviewer":  {},
		},
		WriteSurface: config.WriteSurfaceConfig{Enforce: []string{"scanner"}},
	}
	path := filepath.Join(t.TempDir(), "write-surface-enforce.json")
	if err := github.PublishWriteSurfaceEnforce(path, writeSurfaceEnforcedLanes(cfg)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var doc github.WriteSurfaceEnforceFile
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("published document is not JSON: %v (%s)", err, data)
	}
	found := map[string]bool{}
	for _, lane := range doc.Lanes {
		found[lane] = true
	}
	if !found["scanner"] || !found["scanner-2"] {
		t.Errorf("lanes = %q, want both scanner and its replica", doc.Lanes)
	}
	if found["reviewer"] {
		t.Errorf("lanes = %q, want an unlisted lane left alone", doc.Lanes)
	}
}
