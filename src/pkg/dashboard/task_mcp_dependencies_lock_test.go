package dashboard

import (
	"fmt"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/planning"
)

func dependenciesLockFixture(t *testing.T, filler int) (dashboardTaskMCPProvider, *beads.Store, taskMCPSnapshot, string, string) {
	t.Helper()
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The child is created before its epic on purpose: see the duplicate test.
	child, err := store.Create("child", beads.TypeTask, beads.PriorityMedium, "architect", "")
	if err != nil {
		t.Fatal(err)
	}
	firstFiller := ""
	for i := 0; i < filler; i++ {
		b, err := store.Create(fmt.Sprintf("bead %d", i), beads.TypeTask, beads.PriorityMedium, "architect", "")
		if err != nil {
			t.Fatal(err)
		}
		if firstFiller == "" {
			firstFiller = b.ID
		}
	}
	epic, err := store.Create("epic", beads.TypeEpic, beads.PriorityMedium, "architect", "gh-owner/repo#42")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Update(epic.ID, func(b *beads.Bead) { b.Metadata[planning.MetaPlanStatus] = planning.PlanStatusApproved })
	_ = store.Update(child.ID, func(b *beads.Bead) { b.Metadata[planning.MetaParentEpic] = epic.ID })

	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{}, BeadStores: map[string]*beads.Store{"architect": store}}
	snap := taskMCPSnapshot{assign: WSTaskAssign{TaskID: "task-1", Kind: "issue", Repo: "owner/repo", Number: 42, Key: "owner/repo#42"}}
	return dashboardTaskMCPProvider{server: s}, store, snap, child.ID, firstFiller
}

// dependencies looked up a planned epic's children from inside a
// beads.Store.ReadEach callback, with another ReadEach on the same store.
// ReadEach holds the store's read lock and sync.RWMutex is not reentrant: once
// a writer queued between the two read locks, the inner read waited for the
// writer and the writer for the outer read, and every later read or write of
// that bead store blocked until the hive restarted.
func TestTaskMCPDependenciesDoesNotDeadlockTheBeadStore(t *testing.T) {
	p, store, snap, _, touch := dependenciesLockFixture(t, 200)

	// Anything else in the hive that writes a bead: a claim, a status change,
	// the planner.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = store.Update(touch, func(b *beads.Bead) { b.Title = "touched" })
		}
	}()

	const calls = 300
	for i := 1; i <= calls; i++ {
		done := make(chan struct{})
		go func() {
			_ = p.dependencyData(snap)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("dependencies call %d never returned while another goroutine was writing beads: the bead store is deadlocked", i)
		}
	}
}

// A child created before its epic was listed twice in suggested_plan: once by
// the pass over all beads and once more by the nested lookup.
func TestTaskMCPDependenciesListsEachPlanChildOnce(t *testing.T) {
	p, _, snap, childID, _ := dependenciesLockFixture(t, 3)
	data := p.dependencyData(snap)
	if len(data.SuggestedPlan) != 1 || data.SuggestedPlan[0].ID != childID {
		t.Fatalf("suggested_plan = %#v, want the one child %s", data.SuggestedPlan, childID)
	}
}
