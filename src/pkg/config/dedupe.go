package config

// DedupeLaneConfig tunes the dedupe lane's follow-through: what the hive does
// with the consolidation checklists the lane files, once it has filed them.
type DedupeLaneConfig struct {
	// AdvisoryAutoclose runs the advisory-checklist sweep: every open
	// hive-authored advisory checklist has its `- [ ] #N` items ticked as
	// the referenced issues and PRs close or merge, and is closed as
	// completed with a short note once every item is resolved
	// (hivecommons/hive#15754). Default true; the checklist then never
	// waits on a person.
	AdvisoryAutoclose *bool `yaml:"advisory_autoclose,omitempty" json:"advisory_autoclose,omitempty"`
}

// AdvisoryAutocloseEnabled reports whether the advisory-checklist sweep runs.
func (d DedupeLaneConfig) AdvisoryAutocloseEnabled() bool {
	if d.AdvisoryAutoclose == nil {
		return true
	}
	return *d.AdvisoryAutoclose
}
