package config

import "strings"

// The capability tiers standby borrows from RFC #6825. They are the same three
// names `governor.rotation.agents` already spells in hive.yaml — standby uses
// the vocabulary, not that mapping, because that one keys on agent names and
// this one keys on model configurations.
const (
	StandbyTierT1 = "T1"
	StandbyTierT2 = "T2"
	StandbyTierT3 = "T3"
	// StandbyTierUnknown is the ABSENCE of a tier, not a weak one. A
	// configuration Hive cannot map resolves to it, and it never qualifies
	// against any floor. It is deliberately NOT accepted as a floor value:
	// see validateStandbyFloor.
	StandbyTierUnknown = "unknown"
)

// StandbyDefaultFloor is what `min_model_capability` means when it is absent.
//
// The default is the STRONGEST floor on purpose. A hive that adopts the block
// without thinking about the floor gets the safe answer, and lowering it is a
// deliberate edit rather than a value that drifts down by omission.
const StandbyDefaultFloor = StandbyTierT1

// standbyDailyCapMax mirrors pkg/config's clamp on daily_cap_per_contributor
// so StandbyDailyCap can re-clamp a config that skipped the defaults pass.
const standbyDailyCapMax = 50

// StandbyConfig is the per-lane `standby:` block on an agent.
//
// It is a POINTER on AgentConfig so that "no standby block" stays
// distinguishable from "a standby block that is off". The distinction is what
// keeps an existing hive byte-for-byte unchanged through a load/save cycle:
// absent stays absent, and no default is materialised into a config that never
// asked for one.
type StandbyConfig struct {
	// Enabled offers this lane's queue to approved standby contributors when
	// it is paused for budget. Off is the zero value.
	Enabled bool `yaml:"enabled" json:"enabled,omitempty"`
	// AutoDispatch lets Hive hand this lane's paused queue to qualified standby
	// contributors without an owner pressing the manual dispatch control.
	// Default off: S8 is opt-in per lane and preserves S5's hold-gated path.
	AutoDispatch bool `yaml:"auto_dispatch,omitempty" json:"auto_dispatch,omitempty"`
	// MinModelCapability is the lane's FLOOR: the weakest capability tier a
	// donated configuration may have and still be offered this lane's work.
	// Absent means StandbyDefaultFloor (T1).
	//
	// This field is READ-ONLY on every surface. There is no dashboard control,
	// no PUT field and no chat command that writes it, and
	// TestStandbyFloorHasNoWriterOutsideConfig asserts exactly that. The
	// reason is in the design's first section: a rule about what a UI *says*
	// can be undone by a well-meant copy change; an absent endpoint cannot.
	// Lowering a floor is an edit to hive.yaml, on purpose.
	MinModelCapability string `yaml:"min_model_capability,omitempty" json:"min_model_capability,omitempty"`
	// DailyCapPerContributor bounds how many donated tasks one approved
	// contributor may be dispatched on this lane per rolling day.
	//
	// Absent means 0, and 0 means NOTHING IS DISPATCHED. That is what makes
	// this block safe to adopt before the dispatch path exists (S5): a hive
	// can enable standby, see the counts S4 renders, and dispatch nothing.
	DailyCapPerContributor int `yaml:"daily_cap_per_contributor,omitempty" json:"daily_cap_per_contributor,omitempty"`
}

// IsStandbyEnabled resolves the lane's standby switch. A nil block is off.
func (s *StandbyConfig) IsStandbyEnabled() bool {
	return s != nil && s.Enabled
}

// StandbyFloor resolves the lane's effective floor, defaulting an absent or
// blank value to StandbyDefaultFloor.
//
// Always resolve through this rather than reading MinModelCapability directly:
// a nil block, a config built in a test, and a config that never ran through
// the defaults pass all legitimately carry an empty string, and every one of
// them means T1.
func (s *StandbyConfig) StandbyFloor() string {
	if s == nil {
		return StandbyDefaultFloor
	}
	if tier := NormalizeStandbyTier(s.MinModelCapability); tier != "" {
		return tier
	}
	return StandbyDefaultFloor
}

// StandbyDailyCap resolves the lane's per-contributor daily cap, re-clamping
// defensively for callers that build an AgentConfig without running the
// defaults pass. A nil block, and any value at or below zero, is 0 — nothing
// is dispatched.
func (s *StandbyConfig) StandbyDailyCap() int {
	if s == nil || s.DailyCapPerContributor <= 0 {
		return 0
	}
	if s.DailyCapPerContributor > standbyDailyCapMax {
		return standbyDailyCapMax
	}
	return s.DailyCapPerContributor
}

// NormalizeStandbyTier trims and upper-cases a tier name so `t2` in hive.yaml
// resolves to T2. It normalizes spelling ONLY: an unrecognised name (including
// "unknown") is returned upper-cased so the caller can name it in an error.
func NormalizeStandbyTier(tier string) string {
	return strings.ToUpper(strings.TrimSpace(tier))
}
