package automerge

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

func readinessFixture(t *testing.T, durable bool) (*preparationAPI, *Engine, string) {
	t.Helper()
	api, srv := newPreparationAPI(t)
	dir := ""
	if durable {
		dir = filepath.Join(t.TempDir(), "preparation")
	}

	c := preparationEngine(srv.URL, NewBasePreparationStore(dir))
	c.requiredChecksForRepo = func(string) (map[string]bool, bool) { return map[string]bool{"build": true}, true }
	if ev, _, _, err := preparationGate(c, 1, "main", "head1"); err != nil || !ev.BranchUpdated {
		t.Fatalf("initial refresh: %+v %v", ev, err)
	}
	api.prs[1].Head.SHA = gh.Ptr("refreshed1")
	api.pending["refreshed1"] = true
	api.behind["refreshed1"] = 1
	api.baseSHA = "base2"
	return api, c, srv.URL
}

func TestPreparationCompletedOwnerYieldsAndSurvivesRestart(t *testing.T) {
	api, c, url := readinessFixture(t, true)
	if ev, reason, _, err := preparationGate(c, 2, "main", "head2"); err != nil || !ev.BranchUpdated {
		t.Fatalf("ready independent successor: %+v %q %v", ev, reason, err)
	}
	st, err := c.preparationStore().ownerSnapshot(preparationKey("acme", "widget", "main"))
	if err != nil || st.Owner.Number != 2 || len(st.Waiters) != 1 || st.Waiters[0].Number != 1 || !st.Waiters[0].Yielded {
		t.Fatalf("yield lost priority: %+v %v", st, err)
	}
	restarted := preparationEngine(url, NewBasePreparationStore(c.preparationStore().dir))
	restarted.requiredChecksForRepo = c.requiredChecksForRepo
	for i := 0; i < 3; i++ {
		if _, reason, _, err := preparationGate(restarted, 1, "main", "refreshed1"); err != nil || reason != preparationQueued(2) {
			t.Fatalf("unresolved second refresh: %q %v", reason, err)
		}
		restarted.observeBasePreparation(stageLaneSelf, "acme", "widget", api.prs[1], "required-check-pending:build", false, nil)
	}
	if api.updates[1] != 1 || api.updates[2] != 1 {
		t.Fatalf("pending-head refresh stampede: %v", api.updates)
	}
	st, _ = restarted.preparationStore().ownerSnapshot(preparationKey("acme", "widget", "main"))
	if len(st.Waiters) != 1 || !st.Waiters[0].Yielded {
		t.Fatalf("pending observation erased former owner: %+v", st)
	}
	api.prs[2].Head.SHA = gh.Ptr("refreshed2")
	api.pending["refreshed2"] = true
	api.pending["refreshed1"] = false
	if _, reason, _, err := preparationGate(restarted, 3, "main", "head3"); err != nil || reason != preparationQueued(1) {
		t.Fatalf("ready former owner was overtaken: %q %v", reason, err)
	}
	if ev, reason, _, err := preparationGate(restarted, 1, "main", "refreshed1"); err != nil || !ev.BranchUpdated {
		t.Fatalf("ready former owner could not regain priority: %+v %q %v", ev, reason, err)
	}
}

func TestPreparationCannotYieldOnUnsafeOrMissingEvidence(t *testing.T) {
	for _, blocker := range []string{"unresolved", "repair", "legacy-attempt", "dependency", "missing-dependency", "unreadable-dependency", "late-dependency", "shared-claim", "reference", "stack", "pagination", "incomplete-count", "rename", "head-moved", "base-moved", "candidate-ci", "candidate-review"} {
		t.Run(blocker, func(t *testing.T) {
			api, c, _ := readinessFixture(t, false)
			switch blocker {
			case "unresolved":
				api.prs[1].Head.SHA = gh.Ptr("head1")
			case "repair", "legacy-attempt":
				_ = c.preparationStore().change(preparationKey("acme", "widget", "main"), func(st *basePreparation) error {
					if blocker == "repair" {
						st.Owner.Repair = true
					} else {
						attempt := st.Attempts[1]
						attempt.AuthoritativeBase = false
						st.Attempts[1] = attempt
					}

					return nil
				})
			case "dependency":
				api.dependencyBlocked = true
			case "missing-dependency":
				api.dependencyMissing = true
			case "unreadable-dependency":
				api.dependencyError = true
			case "late-dependency":
				api.onFiles = func(a *preparationAPI, _ int) { a.dependencyBlocked = true }
			case "shared-claim":
				api.prs[2].Body = api.prs[1].Body
			case "reference":
				api.prs[2].Body = gh.Ptr("Closes #102\nDepends on #1")
			case "stack":
				api.prs[2].Head.SHA = gh.Ptr("refreshed1")
			case "pagination":
				api.truncated = true
			case "incomplete-count":
				api.prs[2].ChangedFiles = gh.Ptr(2)
			case "rename":
				api.files[2][0].PreviousFilename = gh.Ptr("file1.go")
			case "head-moved":
				api.onFiles = func(a *preparationAPI, n int) {
					if n == 2 {
						a.prs[2].Head.SHA = gh.Ptr("unexpected")
					}
				}

			case "base-moved":
				api.onFiles = func(a *preparationAPI, _ int) { a.baseSHA = "base2" }
			case "candidate-ci":
				api.pending["head2"] = true
			case "candidate-review":
				c.reviewGate = &ReviewGate{RequireApproval: func() bool { return true }, Verdict: func(string, int, string) ReviewGateVerdict { return ReviewGateVerdict{} }}
			}
			if ev, _, _, err := preparationGate(c, 2, "main", api.prs[2].GetHead().GetSHA()); err != nil || ev.BranchUpdated || api.updates[2] != 0 {
				t.Fatalf("unsafe %s advanced: %+v %v %v", blocker, ev, err, api.updates)
			}
		})
	}
}

func TestPreparationSweepAndRelayShareDestinationGuard(t *testing.T) {
	api, c, url := readinessFixture(t, false)
	_, release, err := hgithub.AcquireDestinationMerge(context.Background(), "", "acme/widget", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, _, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 2, true); !errors.Is(err, hgithub.ErrDestinationMergeBusy) {
		t.Fatalf("sweep escaped destination guard: %v", err)
	}
	client := hgithub.NewClientForTest(url, "acme", nil, c.logger)
	if _, err := client.MergePR(context.Background(), "acme/widget", 2, "squash", "head2"); !errors.Is(err, hgithub.ErrDestinationMergeBusy) {
		t.Fatalf("relay merge escaped same destination guard: %v", err)
	}
	if api.merges[2] != 0 || api.updates[2] != 0 {
		t.Fatalf("guard allowed mutation: merges=%v updates=%v", api.merges, api.updates)
	}
}

func TestPreparationConcurrentYieldAdmitsOnlyOneMutation(t *testing.T) {
	api, c, _ := readinessFixture(t, false)
	var wg sync.WaitGroup
	for _, number := range []int{2, 3} {
		wg.Add(1)
		go func(number int) {
			defer wg.Done()
			_, _, _, err := preparationGate(c, number, "main", api.prs[number].GetHead().GetSHA())
			if err != nil {
				t.Error(err)
			}
		}(number)
	}
	wg.Wait()
	if api.updates[1] != 1 || api.updates[2]+api.updates[3] != 1 {
		t.Fatalf("completed owner yielded to concurrent mutations: %v", api.updates)
	}
}

func TestPreparationReadyFirstWithoutActiveOwner(t *testing.T) {
	api, c, _ := readinessFixture(t, false)
	_ = c.preparationStore().change(preparationKey("acme", "widget", "main"), func(st *basePreparation) error {
		ticket := st.Owner
		ticket.Head = "refreshed1"
		st.Waiting = []preparationCandidate{ticket}
		st.Owner = preparationCandidate{}
		return nil
	})
	if ev, _, _, err := preparationGate(c, 2, "main", "head2"); err != nil || !ev.BranchUpdated {
		t.Fatalf("blocked oldest waiting entry: %+v %v", ev, err)
	}
	if api.updates[1] != 1 || api.updates[2] != 1 {
		t.Fatalf("unexpected refreshes: %v", api.updates)
	}
}

func TestPreparationClaimUsesLiveBaseNotPRBasePayload(t *testing.T) {
	api, c, _ := readinessFixture(t, false)
	api.baseSHA = "base2"
	api.prs[1].Base.SHA = gh.Ptr("stale-base")
	api.prs[1].Head.SHA = gh.Ptr("refreshed1")
	if err := c.claimBasePreparationUpdate(context.Background(), "acme", "widget", 1, "refreshed1"); err != nil {
		t.Fatal(err)
	}
	st, _ := c.preparationStore().ownerSnapshot(preparationKey("acme", "widget", "main"))
	if st.Attempt.Base != "base2" || !st.Attempt.AuthoritativeBase {
		t.Fatalf("stale PR base used as authoritative: %+v", st)
	}
}
