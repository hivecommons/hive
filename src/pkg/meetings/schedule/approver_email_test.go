package schedule

import "testing"

func TestAllowlistApprover(t *testing.T) {
	a := AllowlistApprover{}
	cases := []struct {
		ev   Event
		want bool
	}{
		{Event{Source: SourceManual, InvitedByOwner: true}, false},
		{Event{Source: SourceManual}, true},
		{Event{Source: SourceEmail, Trusted: true}, false},
		{Event{Source: SourceEmail}, true},
		{Event{Source: SourceDiscord, Trusted: true}, true},
	}
	for i, c := range cases {
		if got := a.NeedsApproval(c.ev); got != c.want {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
}
