package config

import (
	"testing"
	"time"
)

func TestContributorFirstGraceOrDefault(t *testing.T) {
	cases := []struct {
		name  string
		grace time.Duration
		want  time.Duration
	}{
		{name: "absent yields the default", grace: 0, want: DefaultContributorFirstGrace},
		{name: "positive is returned as-is", grace: 3 * time.Minute, want: 3 * time.Minute},
		{name: "negative (hand-built, never loaded) yields the default", grace: -time.Minute, want: DefaultContributorFirstGrace},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (AgentConfig{ContributorFirstGrace: tc.grace}).ContributorFirstGraceOrDefault(); got != tc.want {
				t.Fatalf("ContributorFirstGraceOrDefault() = %v, want %v", got, tc.want)
			}
		})
	}
	if DefaultContributorFirstGrace != 10*time.Minute {
		t.Fatalf("DefaultContributorFirstGrace = %v, want 10m (documented default)", DefaultContributorFirstGrace)
	}
}
