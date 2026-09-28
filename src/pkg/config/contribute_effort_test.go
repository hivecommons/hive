package config

import "testing"

func TestReasoningEffortRank(t *testing.T) {
	for i, v := range ReasoningEffortLadder {
		if got := ReasoningEffortRank(v); got != i {
			t.Errorf("rank(%q) = %d, want %d", v, got, i)
		}
	}
	if got := ReasoningEffortRank(" HIGH "); got != 3 {
		t.Errorf("rank should be case/space-insensitive, got %d", got)
	}
	for _, v := range []string{"", "turbo"} {
		if got := ReasoningEffortRank(v); got != -1 {
			t.Errorf("rank(%q) = %d, want -1", v, got)
		}
	}
}

func TestLadderCoversEveryBackendEffort(t *testing.T) {
	for backend, levels := range ReasoningEffortsByBackend {
		for _, l := range levels {
			if ReasoningEffortRank(l) < 0 {
				t.Errorf("backend %s effort %q is missing from ReasoningEffortLadder", backend, l)
			}
		}
	}
}

func TestNormalizeContributeMinReasoningEffort(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"", "", false},
		{"  ", "", false},
		{"Medium", "medium", false},
		{"max", "max", false},
		{"extreme", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeContributeMinReasoningEffort(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("Normalize(%q) = %q, %v; want %q, err=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestReasoningEffortMeetsFloor(t *testing.T) {
	cases := []struct {
		name                   string
		backend, effort, floor string
		wantOK, wantKnown      bool
	}{
		{"no floor", "codex", "", "", true, true},
		{"invalid floor imposes nothing", "codex", "", "bogus", true, true},
		{"empty effort unknown", "codex", "", "medium", false, false},
		{"unrecognised effort unknown", "claude", "turbo", "medium", false, false},
		{"effort invalid for backend unknown", "agy", "minimal", "low", false, false},
		{"codex high meets medium", "codex", "high", "medium", true, true},
		{"codex equal meets", "codex", "medium", "medium", true, true},
		{"codex minimal below low", "codex", "minimal", "low", false, true},
		{"claude max meets xhigh", "claude", "max", "xhigh", true, true},
		{"codex xhigh below max floor clamps to xhigh", "codex", "xhigh", "max", true, true},
		{"codex high below clamped max floor", "codex", "high", "max", false, true},
		{"agy high meets clamped max floor", "agy", "high", "max", true, true},
		{"agy medium below clamped floor", "agy", "medium", "xhigh", false, true},
		{"open-vocabulary backend uses ladder", "omp", "low", "medium", false, true},
		{"open-vocabulary backend meets", "omp", "HIGH", "medium", true, true},
		{"backend case-insensitive", "Codex", "high", "high", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, known := ReasoningEffortMeetsFloor(c.backend, c.effort, c.floor)
			if ok != c.wantOK || known != c.wantKnown {
				t.Errorf("MeetsFloor(%q,%q,%q) = (%v,%v), want (%v,%v)",
					c.backend, c.effort, c.floor, ok, known, c.wantOK, c.wantKnown)
			}
		})
	}
}

func TestApplyDefaultsNormalizesMinReasoningEffort(t *testing.T) {
	c := &Config{}
	c.Hub.ContributeMinReasoningEffort = " High "
	c.applyDefaults()
	if c.Hub.ContributeMinReasoningEffort != "high" {
		t.Errorf("floor = %q, want high", c.Hub.ContributeMinReasoningEffort)
	}

	c = &Config{}
	c.Hub.ContributeMinReasoningEffort = "ludicrous"
	c.applyDefaults()
	if c.Hub.ContributeMinReasoningEffort != "" {
		t.Errorf("invalid floor should be cleared, got %q", c.Hub.ContributeMinReasoningEffort)
	}
}
