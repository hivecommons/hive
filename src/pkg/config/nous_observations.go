package config

import "fmt"

// Nous observation filing (hivecommons/hive#12341 child 3, #12654).
//
// The hive-state detectors in pkg/hivestate can file their findings upstream
// through the self-report relay. Filing is opt-in and throttled: nothing is
// filed unless nous.observations.enabled is true, at most max_per_week new
// observations are filed in any rolling 7-day window, and nothing is filed
// while a self-upgrade drain is open.

// DefaultNousObservationsMaxPerWeek is the weekly filing budget applied when
// nous.observations.max_per_week is absent.
const DefaultNousObservationsMaxPerWeek = 5

// NousObservationsConfig is `nous.observations`. The zero value is off.
type NousObservationsConfig struct {
	// Enabled turns observation detection and filing on. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MaxPerWeek caps new observations filed per rolling 7 days. Absent
	// (nil) applies DefaultNousObservationsMaxPerWeek; an explicit value
	// below 1 fails validation.
	MaxPerWeek *int `yaml:"max_per_week,omitempty" json:"max_per_week,omitempty"`
}

// ResolvedMaxPerWeek returns the effective weekly filing budget.
func (o NousObservationsConfig) ResolvedMaxPerWeek() int {
	if o.MaxPerWeek == nil {
		return DefaultNousObservationsMaxPerWeek
	}
	return *o.MaxPerWeek
}

// Validate rejects an explicit max_per_week below 1.
func (o NousObservationsConfig) Validate() error {
	if o.MaxPerWeek != nil && *o.MaxPerWeek < 1 {
		return fmt.Errorf("observations.max_per_week must be >= 1 (got %d)", *o.MaxPerWeek)
	}
	return nil
}
