package config

// Defaults for the merge-hotspot advisor (GovernorConfig.Hotspot*).
const (
	DefaultHotspotWindowDays  = 7
	DefaultHotspotMinPRs      = 6
	DefaultHotspotMinOverlaps = 3
	DefaultHotspotMinLines    = 800
	DefaultHotspotRefileDays  = 30
	DefaultHotspotMaxOpen     = 2
	DefaultHotspotMaxPerPass  = 1
)

// HotspotIssuesEnabled resolves GovernorConfig.HotspotEnabled (default on).
func (g GovernorConfig) HotspotIssuesEnabled() bool {
	return g.HotspotEnabled == nil || *g.HotspotEnabled
}

func hotspotOrDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// HotspotWindowDaysOrDefault resolves governor.hotspot_window_days.
func (g GovernorConfig) HotspotWindowDaysOrDefault() int {
	return hotspotOrDefault(g.HotspotWindowDays, DefaultHotspotWindowDays)
}

// HotspotMinPRsOrDefault resolves governor.hotspot_min_prs.
func (g GovernorConfig) HotspotMinPRsOrDefault() int {
	return hotspotOrDefault(g.HotspotMinPRs, DefaultHotspotMinPRs)
}

// HotspotMinOverlapsOrDefault resolves governor.hotspot_min_overlaps.
func (g GovernorConfig) HotspotMinOverlapsOrDefault() int {
	return hotspotOrDefault(g.HotspotMinOverlaps, DefaultHotspotMinOverlaps)
}

// HotspotMinLinesOrDefault resolves governor.hotspot_min_lines.
func (g GovernorConfig) HotspotMinLinesOrDefault() int {
	return hotspotOrDefault(g.HotspotMinLines, DefaultHotspotMinLines)
}

// HotspotRefileDaysOrDefault resolves governor.hotspot_refile_days.
func (g GovernorConfig) HotspotRefileDaysOrDefault() int {
	return hotspotOrDefault(g.HotspotRefileDays, DefaultHotspotRefileDays)
}

// HotspotMaxOpenOrDefault resolves governor.hotspot_max_open.
func (g GovernorConfig) HotspotMaxOpenOrDefault() int {
	return hotspotOrDefault(g.HotspotMaxOpen, DefaultHotspotMaxOpen)
}

// HotspotMaxPerPassOrDefault resolves governor.hotspot_max_per_pass.
func (g GovernorConfig) HotspotMaxPerPassOrDefault() int {
	return hotspotOrDefault(g.HotspotMaxPerPass, DefaultHotspotMaxPerPass)
}
