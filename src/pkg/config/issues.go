package config

import "time"

const DefaultCloseOnMergeBackfillInterval = time.Hour

type IssuesConfig struct {
	CloseOnMerge                 *bool         `yaml:"close_on_merge,omitempty" json:"close_on_merge,omitempty"`
	CloseOnMergeBackfillInterval time.Duration `yaml:"close_on_merge_backfill_interval,omitempty" json:"close_on_merge_backfill_interval,omitempty"`
	// ReporterConfirmation restores the legacy human-filed-bug close gate for
	// every matching issue. Default false: issues close when their fix merges;
	// use the per-issue hive: needs-confirmation marker when only one issue
	// needs reporter verification.
	ReporterConfirmation *bool `yaml:"reporter_confirmation,omitempty" json:"reporter_confirmation,omitempty"`
}

func (i IssuesConfig) CloseOnMergeEnabled() bool {
	if i.CloseOnMerge == nil {
		return true
	}
	return *i.CloseOnMerge
}

func (i IssuesConfig) EffectiveCloseOnMergeBackfillInterval() time.Duration {
	if i.CloseOnMergeBackfillInterval <= 0 {
		return DefaultCloseOnMergeBackfillInterval
	}
	return i.CloseOnMergeBackfillInterval
}

func (i IssuesConfig) ReporterConfirmationEnabled() bool {
	return i.ReporterConfirmation != nil && *i.ReporterConfirmation
}
