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

func TestOperabilityAgentsArePausedInEveryGovernorModeAtEligibleLevels(t *testing.T) {
	for level := 5; level <= 6; level++ {
		pack, err := ACMMPackByLevel(level)
		if err != nil {
			t.Fatalf("load L%d pack: %v", level, err)
		}
		for _, mode := range []string{"surge", "busy", "quiet", "idle"} {
			for _, agent := range []string{"telemetry", "operations"} {
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

func TestACMMPackAgentNames(t *testing.T) {
	p, err := ACMMPackByLevel(5)
	if err != nil {
		t.Fatal(err)
	}
	names := p.AgentNames()
	if len(names) != len(p.Agents) {
		t.Fatalf("AgentNames() returned %d names for %d agents", len(names), len(p.Agents))
	}
	for i, a := range p.Agents {
		if names[i] != a.Name {
			t.Errorf("AgentNames()[%d] = %q, want %q (pack order)", i, names[i], a.Name)
		}
	}
}

// ACMMPackManagedAgentNames is the set a pack apply may re-derive `mode` for
// (#7503): every roster member at every level, once each, sorted — and
// nothing that no pack lists.
func TestACMMPackManagedAgentNames(t *testing.T) {
	names := ACMMPackManagedAgentNames()
	seen := make(map[string]bool, len(names))
	for i, n := range names {
		if seen[n] {
			t.Errorf("duplicate name %q", n)
		}
		seen[n] = true
		if i > 0 && names[i-1] > n {
			t.Errorf("not sorted: %q before %q", names[i-1], n)
		}
	}
	for _, p := range ACMMPacks() {
		for _, a := range p.Agents {
			if !seen[a.Name] {
				t.Errorf("L%d agent %q missing from the managed set", p.Level, a.Name)
			}
		}
	}
	// The union must not grow beyond what the packs define: an agent listed
	// here is one whose operator-set mode a pack apply is allowed to discard.
	if seen["reviewer"] {
		t.Errorf("`reviewer` is in no pack yet appears in the managed set")
	}
}
