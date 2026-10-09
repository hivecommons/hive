package config

import "time"

const DefaultCloseOnMergeBackfillInterval = time.Hour

type IssuesConfig struct {
	CloseOnMerge                 *bool         `yaml:"close_on_merge,omitempty" json:"close_on_merge,omitempty"`
	CloseOnMergeBackfillInterval time.Duration `yaml:"close_on_merge_backfill_interval,omitempty" json:"close_on_merge_backfill_interval,omitempty"`
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
