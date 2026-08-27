package config

import "testing"

func TestACMMPacksLoad(t *testing.T) {
	packs := ACMMPacks()
	if len(packs) != 6 {
		t.Fatalf("expected 6 packs (L1-L6), got %d", len(packs))
	}

	for i, p := range packs {
		if p.Level != i+1 {
			t.Errorf("pack %d: level = %d, want %d", i, p.Level, i+1)
		}
		if p.Name == "" {
			t.Errorf("pack %d: name is empty", i)
		}
		if p.Description == "" {
			t.Errorf("pack %d: description is empty", i)
		}
	}
}

func TestACMMPacksAgentCounts(t *testing.T) {
	packs := ACMMPacks()

	expected := map[int]int{
		1: 2, 2: 5, 3: 6, 4: 7, 5: 11, 6: 12,
	}
	for _, p := range packs {
		want, ok := expected[p.Level]
		if !ok {
			continue
		}
		if len(p.Agents) != want {
			t.Errorf("L%d (%s): expected %d agents, got %d", p.Level, p.Name, want, len(p.Agents))
		}
	}
}

func TestOperabilityAgentsExistOnlyAtL5AndL6(t *testing.T) {
	for level := 1; level <= 4; level++ {
		pack, err := ACMMPackByLevel(level)
		if err != nil {
			t.Fatalf("load L%d pack: %v", level, err)
		}
		for _, agent := range pack.Agents {
			if agent.Name == "telemetry" || agent.Name == "operations" {
				t.Errorf("L%d unexpectedly includes L5+ agent %q", level, agent.Name)
			}
		}
		for mode, cadences := range pack.Governor.Cadences {
			for _, agent := range []string{"telemetry", "operations"} {
				if _, exists := cadences[agent]; exists {
					t.Errorf("L%d %s unexpectedly defines cadence for L5+ agent %q", level, mode, agent)
				}
			}
		}
	}

	for level := 5; level <= 6; level++ {
		pack, err := ACMMPackByLevel(level)
		if err != nil {
			t.Fatalf("load L%d pack: %v", level, err)
		}
		present := map[string]bool{}
		for _, agent := range pack.Agents {
			present[agent.Name] = true
		}
		for _, mode := range []string{"surge", "busy", "quiet", "idle"} {
			for _, agent := range []string{"telemetry", "operations"} {
				if !present[agent] {
					t.Errorf("L%d is missing operability agent %q", level, agent)
				}
				cadence := NewIntervalCadence(pack.Governor.Cadences[mode][agent])
				if !cadence.IsPaused() {
					t.Errorf("L%d %s %s cadence = %q, want paused", level, mode, agent, cadence)
				}
			}
		}
	}
}

func TestOperabilityAgentDefaults(t *testing.T) {
	for _, name := range []string{"telemetry", "operations"} {
		agent := AgentConfig{}
		applyKnownAgentDefaults(name, &agent)
		if agent.Emoji == "" || agent.Color == "" || len(agent.Aliases) == 0 || len(agent.LaneKeywords) == 0 || len(agent.DetectKeywords) == 0 {
			t.Errorf("%s defaults incomplete: %#v", name, agent)
		}
		if agent.BeadRole != "worker" || agent.IncludeRepos == nil || !*agent.IncludeRepos {
			t.Errorf("%s defaults = bead_role %q, include_repos %v", name, agent.BeadRole, agent.IncludeRepos)
		}
	}
}

func TestACMMPacksAgentsHaveRequiredFields(t *testing.T) {
	packs := ACMMPacks()
	for _, p := range packs {
		for _, a := range p.Agents {
			if a.Name == "" {
				t.Errorf("L%d: agent missing name", p.Level)
			}
			if a.Emoji == "" {
				t.Errorf("L%d %s: missing emoji", p.Level, a.Name)
			}
			if a.Color == "" {
				t.Errorf("L%d %s: missing color", p.Level, a.Name)
			}
			if a.SortOrder == 0 {
				t.Errorf("L%d %s: sort_order is 0", p.Level, a.Name)
			}
			if a.Description == "" {
				t.Errorf("L%d %s: missing description", p.Level, a.Name)
			}
		}
	}
}

func TestACMMPackByLevel(t *testing.T) {
	p, err := ACMMPackByLevel(4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Name != "Security-Aware" {
		t.Errorf("L4 name = %q, want 'Security-Aware'", p.Name)
	}

	_, err = ACMMPackByLevel(99)
	if err == nil {
		t.Error("expected error for non-existent level 99")
	}
}

func TestACMMPacksAreSorted(t *testing.T) {
	packs := ACMMPacks()
	for i := 1; i < len(packs); i++ {
		if packs[i].Level <= packs[i-1].Level {
			t.Errorf("packs not sorted: L%d before L%d", packs[i-1].Level, packs[i].Level)
		}
	}
}
