package hub

import "testing"

// TestNeedsPushUnknownIsNotMismatch pins the distinction the six push flags
// encode and that a naive "desired != observed" diff destroys.
//
// An empty report from a spoke means one of two things, and conflating them
// breaks the fleet in opposite directions:
//
//	observedKnown=true  — the spoke reported; "" is a real value
//	observedKnown=false — the spoke is too old to report this field at all
//
// Treating UNKNOWN as a mismatch re-sends on every beat forever, because no
// read-back can ever satisfy it — the spoke cannot report what it does not
// know how to report.
func TestNeedsPushUnknownIsNotMismatch(t *testing.T) {
	cases := []struct {
		name     string
		desired  string
		observed string
		known    bool
		want     bool
		why      string
	}{
		{"push when observed differs and is known", "https://ghe/api/v3", "https://api.github.com", true, true,
			"a real mismatch must be corrected"},
		{"no push when observed already matches", "https://ghe/api/v3", "https://ghe/api/v3", true, false,
			"converged; re-sending would never stop"},
		{"NO push when observed is UNKNOWN", "https://ghe/api/v3", "", false, false,
			"a spoke too old to report must not be pushed forever with no read-back to stop it"},
		{"push when observed is known-empty and differs", "https://ghe/api/v3", "", true, true,
			"the spoke reported it has none; that IS a mismatch"},
		{"no push when the hub wants nothing", "", "https://api.github.com", true, false,
			"no desired value is not an instruction to blank the spoke's"},
		{"no push when the hub wants nothing and observed is unknown", "", "", false, false,
			"nothing known on either side"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsPush(tc.desired, tc.observed, tc.known); got != tc.want {
				t.Fatalf("needsPush(%q,%q,%v) = %v, want %v — %s",
					tc.desired, tc.observed, tc.known, got, tc.want, tc.why)
			}
		})
	}
}
