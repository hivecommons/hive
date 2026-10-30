package automerge

import (
	"testing"
	"time"
)

func TestDurationOr(t *testing.T) {
	for _, tc := range []struct{ d, def, want time.Duration }{
		{0, time.Hour, time.Hour},
		{-time.Minute, time.Hour, time.Hour},
		{time.Minute, time.Hour, time.Minute},
	} {
		if got := DurationOr(tc.d, tc.def); got != tc.want {
			t.Errorf("DurationOr(%v, %v) = %v, want %v", tc.d, tc.def, got, tc.want)
		}
	}
}
