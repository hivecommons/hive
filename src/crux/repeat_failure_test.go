package crux

import "testing"

func TestRepeatFailureCheckStateMessage(t *testing.T) {
	cases := map[RepeatFailureCheckState]string{
		RepeatFailureCheckOn:         RepeatFailureCheckOnMessage,
		RepeatFailureCheckNotCovered: RepeatFailureCheckNotCoveredMessage,
		RepeatFailureCheckNoData:     RepeatFailureCheckNoDataMessage,
		"":                           "",
		"bogus":                      "",
	}
	for state, want := range cases {
		if got := state.Message(); got != want {
			t.Errorf("RepeatFailureCheckState(%q).Message() = %q, want %q", state, got, want)
		}
	}
	if (RepeatFailureStatus{}).Check.Message() != "" {
		t.Error("zero RepeatFailureStatus must render nothing")
	}
}
