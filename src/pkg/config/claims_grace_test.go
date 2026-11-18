package config

import (
	"testing"
	"time"
)

func TestEffectiveContributorGrace(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    int
		want time.Duration
	}{
		{"unset uses the default", 0, DefaultContributorGrace},
		{"explicit", 300, 5 * time.Minute},
		{"negative turns it off", -1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (ClaimsConfig{ContributorGraceS: tc.s}).EffectiveContributorGrace(); got != tc.want {
				t.Fatalf("EffectiveContributorGrace() = %v, want %v", got, tc.want)
			}
		})
	}
	if DefaultContributorGrace != 15*time.Minute {
		t.Fatalf("default grace = %v, want 15m", DefaultContributorGrace)
	}
}
