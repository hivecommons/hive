package cistatus

import "testing"

func TestReasonIsPending(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"", false},
		{"check-pending", true},
		{"status-pending", true},
		{"required-check-pending: build", false},
		{"pending: 2 checks queued", true},
		{"required-check-missing: e2e", true},
		{"required-check-missing:e2e", true},
		{"ci-unverified", false},
		{"check-failing: lint", false},
		{"required-check-failing: build", false},
		{"status-failure", false},
		{"check-runs", false},
		{"pending:no-space", false},
		{"pending-review", false},
		{"check-PENDING", false},
	}
	for _, tc := range cases {
		if got := ReasonIsPending(tc.reason); got != tc.want {
			t.Errorf("ReasonIsPending(%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}
