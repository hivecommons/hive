package config

// DedupeAdvisoryConfig tunes what the hive does with the dedupe agent's advisory
// checklists (hivecommons/hive#15754).
type DedupeAdvisoryConfig struct {
	// AdvisoryAutoclose lets the close-on-merge backfill tick each
	// `- [ ] #N` item of a hive-authored dedupe checklist once #N is closed
	// or merged, and close the checklist as completed once every item is
	// resolved. *bool so unset means on; set
	// `dedupe.advisory_autoclose: false` to leave checklists for a person.
	AdvisoryAutoclose *bool `yaml:"advisory_autoclose,omitempty" json:"advisory_autoclose,omitempty"`
}

// AdvisoryAutocloseEnabled reports whether the item pass runs; unset means on.
func (c DedupeAdvisoryConfig) AdvisoryAutocloseEnabled() bool {
	return c.AdvisoryAutoclose == nil || *c.AdvisoryAutoclose
}
