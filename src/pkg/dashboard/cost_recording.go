package dashboard

import (
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// costRecording tells the Cost card whether the token recorder is actually
// capturing the spend the hive is generating right now, so the UI can say
// "no priced usage recorded since <date>" instead of presenting a frozen
// headline and a cents-per-day run-rate as live figures (hivecommons/hive#13485).
//
// Two independent signals feed it. ConfiguredModels is every model an enabled
// agent is configured to use; any of those with no tokens in the live token
// summary is listed in UnrecordedModels. LastPricedGrowth is the most recent
// cost-history sample whose priced total rose over the previous sample — the
// last moment the recorder saw billable usage. Either signal alone marks the
// recording stale: configured models that never appear in the summary, or no
// priced growth for longer than costRecordingStaleAfter while agents exist.
type costRecording struct {
	ConfiguredModels []string `json:"configured_models"`
	RecordedModels   []string `json:"recorded_models"`
	UnrecordedModels []string `json:"unrecorded_models"`
	// LastPricedGrowth / LastTokenGrowth are unix-millisecond stamps of the
	// most recent history sample where the priced total / any model's token
	// counters rose; 0 when the history never showed growth.
	LastPricedGrowth int64 `json:"last_priced_growth,omitempty"`
	LastTokenGrowth  int64 `json:"last_token_growth,omitempty"`
	Stale            bool  `json:"stale"`
	// StaleSince is when live recording is believed to have stopped
	// (unix ms): the last priced growth, else the summary's window start.
	StaleSince int64 `json:"stale_since,omitempty"`
	// Reason is a one-line, operator-readable explanation of why Stale is set.
	Reason string `json:"reason,omitempty"`
	// Issue names the tracking issue the UI links to when Stale is set.
	Issue string `json:"issue,omitempty"`
}

// costRecordingStaleAfter is how long the priced total may stay flat before
// the recording is called stale. Governor cadence on even a quiet hive kicks
// agents many times a day, so a full two days without a single priced token
// is a recorder fault, not a quiet period.
const costRecordingStaleAfter = 48 * time.Hour

// costRecordingIssue is the root-cause tracking issue the banner links to:
// Copilot CLI sessions for Claude models live under per-agent HOMEs and go
// through /v1/messages, neither of which the recorder followed.
const costRecordingIssue = "hivecommons/hive#13480"

// normalizeModelName folds case and the dot/dash spelling difference between
// config ("claude-sonnet-4.6") and backend-reported names ("claude-sonnet-4-6")
// so a configured model is not reported unrecorded over punctuation.
func normalizeModelName(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), ".", "-")
}

// configuredAgentModels returns the distinct models of every enabled agent,
// lowercased and sorted. Agents without an explicit model are skipped: the
// backend default is not knowable here, and a false "unrecorded" would be
// worse than a missed one.
func configuredAgentModels(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, a := range cfg.Agents {
		if !a.Enabled {
			continue
		}
		m := normalizeModelName(a.Model)
		if m == "" {
			continue
		}
		seen[m] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// costHistoryGrowth scans the history for the last sample where the priced
// total rose and the last sample where any model's token counters rose. A
// drop (a lineage reset) is not growth; the sample after a reset is compared
// against the reset floor like any other.
func costHistoryGrowth(hist []CostHistoryEntry) (lastPriced, lastTokens int64) {
	const eps = 1e-9
	for i := 1; i < len(hist); i++ {
		prev, cur := hist[i-1], hist[i]
		if cur.USD > prev.USD+eps {
			lastPriced = cur.Timestamp
		}
		var prevTok, curTok int64
		for _, m := range prev.Models {
			prevTok += m.Input + m.Output
		}
		for _, m := range cur.Models {
			curTok += m.Input + m.Output
		}
		if len(cur.Models) > 0 && curTok > prevTok {
			lastTokens = cur.Timestamp
		}
	}
	return lastPriced, lastTokens
}

// buildCostRecording derives the recording block from configured agents, the
// live estimate and the cost history. now is injected for tests.
func buildCostRecording(cfg *config.Config, est costEstimated, hist []CostHistoryEntry, now time.Time) costRecording {
	rec := costRecording{
		ConfiguredModels: configuredAgentModels(cfg),
		RecordedModels:   []string{},
		UnrecordedModels: []string{},
	}
	if rec.ConfiguredModels == nil {
		rec.ConfiguredModels = []string{}
	}
	recorded := map[string]struct{}{}
	for _, m := range est.ByModel {
		if m.Input+m.Output+m.CacheRead+m.CacheCreate <= 0 {
			continue
		}
		recorded[normalizeModelName(m.Name)] = struct{}{}
	}
	for m := range recorded {
		rec.RecordedModels = append(rec.RecordedModels, m)
	}
	sort.Strings(rec.RecordedModels)
	for _, m := range rec.ConfiguredModels {
		if _, ok := recorded[m]; !ok {
			rec.UnrecordedModels = append(rec.UnrecordedModels, m)
		}
	}

	rec.LastPricedGrowth, rec.LastTokenGrowth = costHistoryGrowth(hist)

	var reasons []string
	if len(rec.UnrecordedModels) > 0 {
		reasons = append(reasons, "configured model(s) with no recorded tokens: "+strings.Join(rec.UnrecordedModels, ", "))
	}
	flatFor := time.Duration(0)
	if rec.LastPricedGrowth > 0 {
		flatFor = now.Sub(time.UnixMilli(rec.LastPricedGrowth))
	} else if len(hist) > 1 {
		flatFor = now.Sub(time.UnixMilli(hist[0].Timestamp))
	}
	if len(rec.ConfiguredModels) > 0 && len(hist) > 1 && flatFor > costRecordingStaleAfter {
		reasons = append(reasons, "priced total flat for "+flatFor.Truncate(time.Hour).String())
	}
	if len(reasons) == 0 {
		return rec
	}
	rec.Stale = true
	rec.Reason = strings.Join(reasons, "; ")
	rec.Issue = costRecordingIssue
	switch {
	case rec.LastPricedGrowth > 0:
		rec.StaleSince = rec.LastPricedGrowth
	case est.WindowStart > 0:
		rec.StaleSince = est.WindowStart
	case len(hist) > 0:
		rec.StaleSince = hist[0].Timestamp
	}
	return rec
}

func (s *Server) costRecording(est costEstimated) costRecording {
	var cfg *config.Config
	if s != nil && s.deps != nil {
		cfg = s.deps.Config
	}
	var hist []CostHistoryEntry
	if s != nil {
		hist = s.CostHistory()
	}
	return buildCostRecording(cfg, est, hist, time.Now())
}
