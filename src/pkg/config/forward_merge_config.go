package config

import (
	"strings"
	"time"
)

const DefaultAdoptRebaseAttemptTimeout = 15 * time.Minute

func (c StageAdoptionConfig) EffectiveRebaseAttemptTimeout() time.Duration {
	return parseDurationOr(c.RebaseAttemptTimeout, DefaultAdoptRebaseAttemptTimeout)
}

const (
	RFCFormatLine = "line"
	RFCFormatCard = "card"
)

func (d DiscordConfig) RFCFormatValid() bool {
	switch strings.ToLower(strings.TrimSpace(d.RFCFormat)) {
	case "", RFCFormatLine, RFCFormatCard:
		return true
	}
	return false
}

func (d DiscordConfig) RFCFormatMode() string {
	if strings.EqualFold(strings.TrimSpace(d.RFCFormat), RFCFormatCard) {
		return RFCFormatCard
	}
	return RFCFormatLine
}

const (
	defaultStageTrackerMergedHold = 8 * time.Second
	minStageTrackerMergedHold     = 3 * time.Second
)

func (c *Config) StageTrackerMergedHold() time.Duration {
	if c == nil || c.Dashboard.StageTracker.MergedHoldS == 0 {
		return defaultStageTrackerMergedHold
	}
	d := time.Duration(c.Dashboard.StageTracker.MergedHoldS) * time.Second
	if d < minStageTrackerMergedHold {
		return minStageTrackerMergedHold
	}
	return d
}

func (n NativeMergeQueueAutoMergeConfig) BaseSet() map[string]bool {
	if len(n.Bases) == 0 {
		return nil
	}
	set := make(map[string]bool, len(n.Bases))
	for _, scope := range n.Bases {
		repo, base, ok := strings.Cut(strings.TrimSpace(scope), "#")
		if ok && repo != "" && base != "" {
			set[strings.ToLower(repo)+"#"+base] = true
		}
	}
	return set
}

type HiveSupersedingAutoMergeConfig struct {
	AutoMerge *bool `yaml:"auto_merge,omitempty" json:"auto_merge,omitempty"`
}

func (h HiveSupersedingAutoMergeConfig) AutoMergeEnabled() bool {
	return h.AutoMerge == nil || *h.AutoMerge
}
