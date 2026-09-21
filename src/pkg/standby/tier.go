// Package standby holds the matching rules for RFC #7629 standby
// contributors: which approved contributor configurations clear a paused
// lane's model floor, and how much of their daily cap is left.
//
// Everything here is pure. No I/O, no ambient clock, no hub or config types —
// the caller reads state, calls in, and renders the answer. That is what makes
// the rules testable as a table and keeps the one property the design exists
// to protect (`src/docs/design/standby-contributors.md`) checkable in one
// place: lowering a lane's floor must be possible and deliberate, and no
// surface Hive renders may propose it.
//
// Three rules are load-bearing and are asserted by tests that fail when the
// code stops stating them:
//
//  1. An unmapped or incomplete configuration is `unknown`, and `unknown`
//     never clears any floor. It is rejected by an explicit guard with its own
//     Reason, not merely by losing a numeric comparison.
//  2. A floor that is not exactly T1, T2 or T3 fails closed. Config validation
//     rejects such a floor at load; this is the second line, for a caller that
//     assembled a LanePolicy some other way.
//  3. A rejection states its reason class and never the delta. No exported
//     value here can tell a caller "you would qualify at T3" — the strength
//     ordering is deliberately unexported so no surface can render a gap.
//
// This is step S4 of the design's phase map. Item-tier matching (S7) threads a
// second tier into the decision; the lane-level rules below are unchanged by
// it.
package standby

import (
	"fmt"
	"strings"
)

// Tier is the RFC #6825 capability vocabulary. TierUnknown is the zero value
// on purpose: a Tier nobody set is the one that never qualifies.
//
// The ordering is T1 > T2 > T3 > unknown — T1 is the strongest. A floor of T2
// therefore admits T1 and T2 and refuses T3.
type Tier string

const (
	// TierUnknown is the absence of a capability tier, not a weak one. It is
	// the zero value, it is what an unmapped configuration resolves to, and it
	// clears no floor. The string "unknown" normalizes to it.
	TierUnknown Tier = ""
	// T1 is the strongest tier.
	T1 Tier = "T1"
	// T2 sits below T1.
	T2 Tier = "T2"
	// T3 is the weakest legal tier.
	T3 Tier = "T3"
)

// String renders the tier for logs and wire fields. TierUnknown renders as
// "unknown" so a reader never sees an empty cell and guesses.
func (t Tier) String() string {
	if t == TierUnknown {
		return "unknown"
	}
	return string(t)
}

// Known reports whether t is one of the three legal tiers.
func (t Tier) Known() bool {
	switch t {
	case T1, T2, T3:
		return true
	default:
		return false
	}
}

// strength orders the tiers for the floor comparison: T1=3, T2=2, T3=1, and
// unknown=0 so it loses to every legal floor mechanically as well as by the
// explicit guard in Qualifies.
//
// It is deliberately unexported. Exporting it would hand every surface the
// arithmetic for "you are one tier short" — the lobbying material the design
// forbids. Callers get a bool and a Reason; nothing more is available to them.
func (t Tier) strength() int {
	switch t {
	case T1:
		return 3
	case T2:
		return 2
	case T3:
		return 1
	default:
		return 0
	}
}

// NormalizeTier is the total, fail-closed reading of a tier from an untrusted
// string: a legal tier in any casing becomes that tier, and everything else —
// "", "unknown", "T0", "tier one", a typo — becomes TierUnknown.
//
// Use it on values that arrived from a relay or a file. Use ParseTier where a
// malformed value should be reported rather than silently downgraded.
func NormalizeTier(s string) Tier {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "T1":
		return T1
	case "T2":
		return T2
	case "T3":
		return T3
	default:
		return TierUnknown
	}
}

// ParseTier reads a tier that the caller requires to be legal, reporting
// failure rather than downgrading. "unknown" and "" both fail here: they are
// the absence of a tier, and a lane floored at the absence of a tier is a lane
// anything clears.
func ParseTier(s string) (Tier, bool) {
	t := NormalizeTier(s)
	return t, t.Known()
}

// Configuration is the WHOLE thing that is matched, never the model alone.
// These are the five fields a relay already reports on auth_response and
// re-reports on standby_declare, spelled the same way.
//
// AdvisorModel and AdvisorEffort are empty when the CLI runs no advisor. Empty
// is a legitimate value of the tuple, not a wildcard: a tier-map entry written
// without advisor fields matches only a configuration that reports none.
type Configuration struct {
	Backend         string
	Model           string
	ReasoningEffort string
	AdvisorModel    string
	AdvisorEffort   string
}

// canonical folds case and surrounding whitespace so that a relay reporting
// "Claude" matches an owner who wrote "claude". It never merges two tuples
// that differ in substance — only in spelling — so it cannot admit a
// configuration the owner did not map. Two map entries that differ only in
// casing collide, and NewTierMap reports that as the duplicate it is.
func (c Configuration) canonical() Configuration {
	fold := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	return Configuration{
		Backend:         fold(c.Backend),
		Model:           fold(c.Model),
		ReasoningEffort: fold(c.ReasoningEffort),
		AdvisorModel:    fold(c.AdvisorModel),
		AdvisorEffort:   fold(c.AdvisorEffort),
	}
}

// Complete reports whether the configuration names at least a backend and a
// model. An incomplete configuration is never looked up: an entry with no
// model would be a wildcard over every model on that backend, which is the
// opposite of matching a configuration.
func (c Configuration) Complete() bool {
	k := c.canonical()
	return k.Backend != "" && k.Model != ""
}

// String renders the configuration as the ledger's key form —
// backend|model|effort|advisor|advisor_effort — for logs and outcome rows.
func (c Configuration) String() string {
	k := c.canonical()
	return strings.Join([]string{k.Backend, k.Model, k.ReasoningEffort, k.AdvisorModel, k.AdvisorEffort}, "|")
}

// TierEntry is one owner-authored row of hub.standby_model_tiers: a whole
// configuration and the capability tier the owner assessed it at.
type TierEntry struct {
	Config Configuration
	Tier   Tier
}

// TierMap resolves a whole configuration to a tier. Hive ships no defaults, so
// the zero TierMap resolves everything to TierUnknown — which is why the
// out-of-the-box outcome on every hive is "0 qualify" until an owner writes
// the mapping deliberately.
//
// The zero value is usable.
type TierMap struct {
	entries map[Configuration]Tier
}

// NewTierMap builds a TierMap from owner-authored entries, reporting the
// misconfigurations that would otherwise be silent: a tier that is not
// T1/T2/T3, an entry with no backend or no model (a wildcard in disguise, and
// one that could never match), and two entries disagreeing about one
// configuration. Duplicates are an error rather than last-one-wins, because
// last-one-wins on a capability floor is a coin toss over how strict the lane
// is.
func NewTierMap(entries []TierEntry) (TierMap, error) {
	m := TierMap{entries: make(map[Configuration]Tier, len(entries))}
	for i, e := range entries {
		tier, ok := ParseTier(string(e.Tier))
		if !ok {
			return TierMap{}, fmt.Errorf("standby_model_tiers[%d]: tier %q must be one of T1, T2, T3", i, e.Tier)
		}
		if !e.Config.Complete() {
			return TierMap{}, fmt.Errorf("standby_model_tiers[%d]: backend and model are required", i)
		}
		key := e.Config.canonical()
		if prev, dup := m.entries[key]; dup {
			return TierMap{}, fmt.Errorf("standby_model_tiers[%d]: duplicate configuration %s already mapped to %s", i, key, prev)
		}
		m.entries[key] = tier
	}
	return m, nil
}

// Tier resolves a configuration. An incomplete configuration, or one with no
// entry, is TierUnknown — the whole point of the mapping shipping empty.
func (m TierMap) Tier(c Configuration) Tier {
	if len(m.entries) == 0 || !c.Complete() {
		return TierUnknown
	}
	return m.entries[c.canonical()]
}

// Len reports how many configurations the owner has mapped.
func (m TierMap) Len() int { return len(m.entries) }
