package client

import "testing"

// KickResult.Queued is the seam callers use to distinguish "this call queued
// the prompt" from "an in-flight delivery absorbed it" (#5325 async kick).
// It was previously uncovered, so a typo against the wrong wire constant
// ("in-flight", or the lifecycle "resumed" mistake documented on Paused)
// would not have failed any test.
func TestKickResultQueued(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"queued", true},
		{"in-flight", false}, // deduplicated into an existing delivery
		{"", false},
		{"Queued", false}, // wire values are lowercase; case must not be folded
	}
	for _, tc := range cases {
		r := KickResult{Status: tc.status, Agent: "scout"}
		if got := r.Queued(); got != tc.want {
			t.Errorf("KickResult{Status: %q}.Queued() = %v, want %v", tc.status, got, tc.want)
		}
	}
}
