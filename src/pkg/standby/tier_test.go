package standby

import (
	"strings"
	"testing"
)

func TestNormalizeTierFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Tier
	}{
		{"T1", T1},
		{"t1", T1},
		{"  T2  ", T2},
		{"t3", T3},
		{"", TierUnknown},
		{"unknown", TierUnknown},
		{"UNKNOWN", TierUnknown},
		{"T0", TierUnknown},
		{"T4", TierUnknown},
		{"tier1", TierUnknown},
		{"T1 or better", TierUnknown},
	} {
		if got := NormalizeTier(tc.in); got != tc.want {
			t.Errorf("NormalizeTier(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseTierRejectsTheAbsenceOfATier(t *testing.T) {
	// "unknown" is not a weak floor, it is no floor. A lane floored at it
	// would be a lane anything clears, so it must be reported, not folded.
	for _, in := range []string{"", "unknown", "UNKNOWN", "none", "any"} {
		if got, ok := ParseTier(in); ok {
			t.Errorf("ParseTier(%q) = (%q, true), want a rejection", in, got)
		}
	}
	for _, in := range []string{"T1", "t2", " T3 "} {
		if _, ok := ParseTier(in); !ok {
			t.Errorf("ParseTier(%q) rejected a legal tier", in)
		}
	}
}

func TestTierStrengthOrdering(t *testing.T) {
	// T1 > T2 > T3 > unknown, and unknown is zero.
	if !(T1.strength() > T2.strength() && T2.strength() > T3.strength() && T3.strength() > TierUnknown.strength()) {
		t.Fatalf("strength ordering broken: T1=%d T2=%d T3=%d unknown=%d",
			T1.strength(), T2.strength(), T3.strength(), TierUnknown.strength())
	}
	if TierUnknown.strength() != 0 {
		t.Errorf("unknown strength = %d, want 0", TierUnknown.strength())
	}
	if T3.strength() < 1 {
		t.Errorf("T3 strength = %d, want at least 1 so unknown loses mechanically", T3.strength())
	}
}

func TestTierUnknownRendersAsUnknown(t *testing.T) {
	if got := TierUnknown.String(); got != "unknown" {
		t.Errorf("TierUnknown.String() = %q, want %q", got, "unknown")
	}
	if got := T2.String(); got != "T2" {
		t.Errorf("T2.String() = %q, want %q", got, "T2")
	}
}

func opus() Configuration {
	return Configuration{Backend: "claude", Model: "claude-opus-5", ReasoningEffort: "high"}
}

func TestZeroTierMapResolvesEverythingToUnknown(t *testing.T) {
	// Hive ships no tier mapping. The out-of-the-box answer on every hive is
	// "nobody assessed this", not "close enough".
	var m TierMap
	if got := m.Tier(opus()); got != TierUnknown {
		t.Errorf("zero TierMap resolved %v to %q, want unknown", opus(), got)
	}
	if m.Len() != 0 {
		t.Errorf("zero TierMap Len() = %d, want 0", m.Len())
	}
}

func TestNewTierMapResolvesAndFoldsSpelling(t *testing.T) {
	m, err := NewTierMap([]TierEntry{
		{Config: opus(), Tier: T1},
		{Config: Configuration{Backend: "codex", Model: "gpt-5.6-terra", ReasoningEffort: "high"}, Tier: T2},
	})
	if err != nil {
		t.Fatalf("NewTierMap: %v", err)
	}
	if m.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", m.Len())
	}
	if got := m.Tier(opus()); got != T1 {
		t.Errorf("Tier(opus) = %q, want T1", got)
	}
	// Casing and stray whitespace are spelling, not substance.
	folded := Configuration{Backend: " Claude ", Model: "Claude-Opus-5", ReasoningEffort: "HIGH"}
	if got := m.Tier(folded); got != T1 {
		t.Errorf("Tier(%v) = %q, want T1 — casing must not change the configuration", folded, got)
	}
	// A configuration nobody mapped stays unknown.
	if got := m.Tier(Configuration{Backend: "claude", Model: "claude-haiku-4-5", ReasoningEffort: "high"}); got != TierUnknown {
		t.Errorf("Tier(unmapped) = %q, want unknown", got)
	}
}

func TestNewTierMapRejectsMisconfiguration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []TierEntry
		want    string
	}{
		{
			name:    "tier is not T1/T2/T3",
			entries: []TierEntry{{Config: opus(), Tier: Tier("T9")}},
			want:    "must be one of",
		},
		{
			name:    "tier spelled unknown",
			entries: []TierEntry{{Config: opus(), Tier: Tier("unknown")}},
			want:    "must be one of",
		},
		{
			name:    "no tier at all",
			entries: []TierEntry{{Config: opus()}},
			want:    "must be one of",
		},
		{
			name:    "no model is a wildcard in disguise",
			entries: []TierEntry{{Config: Configuration{Backend: "claude", ReasoningEffort: "high"}, Tier: T1}},
			want:    "backend and model are required",
		},
		{
			name:    "no backend",
			entries: []TierEntry{{Config: Configuration{Model: "claude-opus-5", ReasoningEffort: "high"}, Tier: T1}},
			want:    "backend and model are required",
		},
		{
			name:    "two entries disagree about one configuration",
			entries: []TierEntry{{Config: opus(), Tier: T1}, {Config: opus(), Tier: T3}},
			want:    "duplicate configuration",
		},
		{
			name: "duplicate differing only in casing",
			entries: []TierEntry{
				{Config: opus(), Tier: T1},
				{Config: Configuration{Backend: "Claude", Model: "CLAUDE-OPUS-5", ReasoningEffort: "High"}, Tier: T3},
			},
			want: "duplicate configuration",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewTierMap(tc.entries)
			if err == nil {
				t.Fatalf("NewTierMap accepted %v", tc.entries)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			if m.Len() != 0 {
				t.Errorf("a rejected map must be empty, got Len() = %d", m.Len())
			}
		})
	}
}

func TestIncompleteConfigurationIsNeverLookedUp(t *testing.T) {
	m, err := NewTierMap([]TierEntry{{Config: opus(), Tier: T1}})
	if err != nil {
		t.Fatalf("NewTierMap: %v", err)
	}
	for _, c := range []Configuration{
		{},
		{Backend: "claude"},
		{Model: "claude-opus-5"},
		{Backend: "  ", Model: "claude-opus-5"},
	} {
		if c.Complete() {
			t.Errorf("Complete() = true for %v", c)
		}
		if got := m.Tier(c); got != TierUnknown {
			t.Errorf("Tier(%v) = %q, want unknown", c, got)
		}
	}
}

func TestConfigurationStringIsTheLedgerKeyForm(t *testing.T) {
	c := Configuration{Backend: "Claude", Model: "claude-opus-5", ReasoningEffort: "high"}
	if got, want := c.String(), "claude|claude-opus-5|high||"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
