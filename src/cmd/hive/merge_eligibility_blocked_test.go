package main

import "testing"

// The dashboard's merge verdict refuses a `blocked` PR like the lanes do, so
// a PR held on its Depends-on targets never reads merge-eligible. A hold on
// the same PR still reads as the hold.
func TestAutomergeBlockLabelReasonBlocked(t *testing.T) {
	for _, tc := range []struct {
		labels []string
		want   string
	}{
		{[]string{"blocked"}, "label:blocked"},
		{[]string{"kind/bug", "Blocked"}, "label:blocked"},
		{[]string{"blocked", "hold"}, "label:hold"},
		{[]string{"kind/bug"}, ""},
	} {
		for _, trusted := range []bool{true, false} {
			if got := automergeBlockLabelReason(tc.labels, trusted); got != tc.want {
				t.Fatalf("automergeBlockLabelReason(%v, %v) = %q, want %q", tc.labels, trusted, got, tc.want)
			}
		}
	}
}
