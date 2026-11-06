package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/blocker"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestAdoptedPushRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    adoptedPushFacts
		want string // substring; "" = pushable
	}{
		{name: "same-repo branch", f: adoptedPushFacts{}},
		{name: "fork with maintainer edits, no workflow diff", f: adoptedPushFacts{Fork: true, MaintainerCanModify: true}},
		{name: "fork without maintainer edits", f: adoptedPushFacts{Fork: true}, want: "does not allow edits by maintainers"},
		{name: "fork changing workflows (testsuite#1037)", f: adoptedPushFacts{Fork: true, MaintainerCanModify: true, WorkflowPath: true}, want: ".github/workflows/"},
		{name: "same-repo PR changing workflows is pushable", f: adoptedPushFacts{WorkflowPath: true}},
		{name: "fork-rehome-refused recorded (utah#919)", f: adoptedPushFacts{Fork: true, MaintainerCanModify: true, RehomeRefused: true}, want: "fork-rehome-refused"},
		{name: "escalated on fork-rehome-refused", f: adoptedPushFacts{Escalation: "adoption-stalled:fork-rehome-refused"}, want: "fork-rehome-refused"},
		{name: "escalated on workflow path", f: adoptedPushFacts{Escalation: "adoption-stalled:contributor-lane:workflow-path"}, want: "workflow"},
		{name: "escalated on a check failure is still pushable", f: adoptedPushFacts{Escalation: "adoption-stalled:check-failure"}},
		{name: "capability blocker recorded", f: adoptedPushFacts{Capability: "App lacks workflows permission"}, want: "fixer reported it cannot push: App lacks workflows permission"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.f.refusal()
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("refusal = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRecordedAdoptedPush(t *testing.T) {
	store := stagelabels.NewStore("", time.Now)
	key := stagelabels.Key("o/r", 919)
	store.Observe(key, stagelabels.AdoptedByHive, "contributor-lane:adopted-by-hive")
	store.MarkForkRehomeRefused(key, "f0b5e2f")
	var f adoptedPushFacts
	recordedAdoptedPush(store, "o/r", 919, "f0b5e2f", &f)
	if !f.RehomeRefused || !f.Adopted {
		t.Fatalf("facts %+v, want rehome refused and adopted", f)
	}
	f = adoptedPushFacts{}
	recordedAdoptedPush(store, "o/r", 919, "newhead", &f)
	if f.RehomeRefused || f.Adopted {
		t.Fatalf("a new head re-arms: %+v", f)
	}

	l := blocker.NewLedger("")
	prev := blockerLedger
	blockerLedger = func() *blocker.Ledger { return l }
	t.Cleanup(func() { blockerLedger = prev })
	l.Record("o/r", 1037, blocker.Blocker{Kind: blocker.CapabilityWorkflowsPermission}, "push of e2e.yml rejected")
	f = adoptedPushFacts{}
	recordedAdoptedPush(nil, "o/r", 1037, "9c0ec3d", &f)
	if f.Capability == "" {
		t.Fatalf("capability blocker not read: %+v", f)
	}
}

// The ownership check reads a short repo name under the org, and applies
// only to a PR the hive took on at this head: on any other PR the author's
// own agent is the one fixing.
func TestAdoptedPushRefusalForOwnership(t *testing.T) {
	store := stagelabels.NewStore("", time.Now)
	prev := adoptedPushStore
	adoptedPushStore = func() *stagelabels.Store { return store }
	t.Cleanup(func() { adoptedPushStore = prev })
	adopted := stagelabels.Key("o/adopted", 1037)
	store.Observe(adopted, stagelabels.AdoptedByHive, "contributor-lane:adopted-by-hive")
	store.MarkAdoptionEscalated(adopted, "9c0ec3d", "adoption-stalled:contributor-lane:workflow-path")
	check := adoptedPushRefusalFor("o")

	if got := check(github.PullRequest{Repo: "adopted", Number: 1037, HeadSHA: "9c0ec3d", FromFork: true, MaintainerCanModify: true}); !strings.Contains(got, "workflows") {
		t.Fatalf("escalated adopted fork: %q, want the workflow-path reason", got)
	}
	if got := check(github.PullRequest{Repo: "o/other", Number: 5, HeadSHA: "abc", FromFork: true}); got != "" {
		t.Fatalf("a fork PR the hive never took on: %q, want no refusal (its author fixes it)", got)
	}
}
