package automerge

import (
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestPreparationYieldedHeadChangeDoesNotHideTerminalState(t *testing.T) {
	for _, terminal := range []string{"closed", "retargeted", "held"} {
		t.Run(terminal, func(t *testing.T) {
			api, c, _ := readinessFixture(t, false)
			if ev, _, _, err := preparationGate(c, 2, "main", "head2"); err != nil || !ev.BranchUpdated {
				t.Fatalf("yield: %+v %v", ev, err)
			}
			api.prs[1].Head.SHA = gh.Ptr("changed-after-yield")
			switch terminal {
			case "closed":
				api.prs[1].State = gh.Ptr("closed")
			case "retargeted":
				api.prs[1].Base.Ref = gh.Ptr("other")
			case "held":
				api.prs[1].Labels = []*gh.Label{{Name: gh.Ptr("hold")}}
			}
			api.prs[2].Head.SHA = gh.Ptr("refreshed2")
			api.pending["refreshed2"] = true
			if ev, reason, _, err := preparationGate(c, 3, "main", "head3"); err != nil || !ev.BranchUpdated {
				t.Fatalf("terminal yielded ticket blocked successor: %+v %q %v", ev, reason, err)
			}
		})
	}
}
