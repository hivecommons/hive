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

	// L5 and L6 each carry one extra agent over L4's roster growth: the
	// on-demand `reviewer`, which is PR-triggered rather than cadence-kicked.
	// It is defined only at the levels where a PR can reach merge without a
	// mandatory human read (L5 gates on `hold`, L6 auto-merges on green).
	expected := map[int]int{
		1: 2, 2: 5, 3: 6, 4: 7, 5: 10, 6: 11,
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
