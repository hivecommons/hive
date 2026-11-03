package crux

// RepeatFailureCheckState is an agent's repeat-failure check state. Empty
// means the check is off, and the dashboard shows nothing.
type RepeatFailureCheckState string

// Repeat-failure check states.
const (
	RepeatFailureCheckOn         RepeatFailureCheckState = "on"
	RepeatFailureCheckNotCovered RepeatFailureCheckState = "not_covered"
	RepeatFailureCheckNoData     RepeatFailureCheckState = "no_data"
)

// Row texts for the check states.
const (
	RepeatFailureCheckOnMessage         = "repeat-failure check on"
	RepeatFailureCheckNotCoveredMessage = "repeat-failure check not available for this backend"
	RepeatFailureCheckNoDataMessage     = "repeat-failure check not receiving data"
)

// Message returns the row text for the state, "" when the check is off.
func (s RepeatFailureCheckState) Message() string {
	switch s {
	case RepeatFailureCheckOn:
		return RepeatFailureCheckOnMessage
	case RepeatFailureCheckNotCovered:
		return RepeatFailureCheckNotCoveredMessage
	case RepeatFailureCheckNoData:
		return RepeatFailureCheckNoDataMessage
	}
	return ""
}

// RepeatFailureStatus is what an agent's dashboard row shows for the
// repeat-failure check. The zero value (check off) shows nothing.
type RepeatFailureStatus struct {
	Check RepeatFailureCheckState
	// Note is "Repeating failure: `<command>` failed N times in M min (exit
	// S)", with " and K others" when several commands repeat. Built from
	// scrubbed, normalized commands only. Empty when nothing repeats.
	Note string
}
