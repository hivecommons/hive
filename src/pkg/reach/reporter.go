package reach

import "time"

// ComponentReach is one spoke-reported per-component execution record — the
// shape phase 2a (#3993) persists on the hub from the heartbeat's
// `component_reach` payload. Field names/JSON keys mirror the 2a contract:
// {component, commit, spans_total, spans_error, first_seen, last_seen}.
type ComponentReach struct {
	// Component is the hive.component span-name prefix (phase 1, #3975).
	Component string `json:"component"`
	// Commit is the short SHA of the binary that was RUNNING when the spans
	// were counted — the ldflags-injected value, never a tag or Deployment
	// digest (anchoring rule, #3973/#3816).
	Commit string `json:"commit"`
	// SpansTotal / SpansError are cumulative-since-boot counts for the
	// component on that commit.
	SpansTotal int64 `json:"spans_total"`
	SpansError int64 `json:"spans_error"`
	// FirstSeen / LastSeen bracket the component's observed executions.
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// ReachReporter is the narrow seam between this package (phase 2b, #3994)
// and the heartbeat-fed reach store phase 2a (#3993) builds. 2b is developed
// in parallel with 2a, so everything here reads through this one method:
// wiring the real store when 2a merges is a construction-site change only.
type ReachReporter interface {
	// LatestReach returns, per hive ID, the latest persisted component_reach
	// report. Implementations return a snapshot the caller may read freely;
	// hives that have never reported are simply absent.
	LatestReach() map[string][]ComponentReach
}

// HistoryReachReporter extends ReachReporter to expose historical reach reports
// across previous deploy windows (#3973, phase 2c).
type HistoryReachReporter interface {
	ReachReporter
	// HistoryReach returns, per hive ID, the historical component_reach reports
	// ordered chronologically.
	HistoryReach() map[string][][]ComponentReach
}

// StubReachReporter is the placeholder implementation used until phase 2a
// lands: it reports an empty fleet, so every PR legitimately shows zero
// reach rather than fabricated data. It also serves as the test double.
type StubReachReporter struct {
	// Reports, when non-nil, is returned verbatim (tests populate it).
	Reports map[string][]ComponentReach
	// History, when non-nil, is returned verbatim by HistoryReach.
	History map[string][][]ComponentReach
}

// LatestReach implements ReachReporter.
func (s *StubReachReporter) LatestReach() map[string][]ComponentReach {
	if s == nil || s.Reports == nil {
		return map[string][]ComponentReach{}
	}
	return s.Reports
}

// HistoryReach implements HistoryReachReporter.
func (s *StubReachReporter) HistoryReach() map[string][][]ComponentReach {
	if s == nil || s.History == nil {
		return map[string][][]ComponentReach{}
	}
	return s.History
}
