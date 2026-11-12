package dashboard

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// After a hub restart the status snapshot cannot vouch for any repo until
// its listing completes, so #14708's open-PR check kept every stage record.
// On projectbluefin (2026-11-09) a contributor card then listed seven
// utah-packages PRs as awaiting rework; six had been closed and one merged
// by a maintainer 9-13 days earlier, and no stage pass had seen them since.

// restartedReworkStore writes the records through a store whose clock is
// set per record, saves it, and loads it into a fresh store the way the hub
// does on start-up.
func restartedReworkStore(t *testing.T, write func(store *stagelabels.Store, at func(time.Time))) *stagelabels.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), stagelabels.StateFile)
	clock := time.Now()
	before := stagelabels.NewStore(path, func() time.Time { return clock })
	write(before, func(at time.Time) { clock = at })
	if err := before.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	after := stagelabels.NewStore(path, nil)
	if err := after.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	return after
}

func TestFleetReworkAfterRestartDropsLongUnseenRecords(t *testing.T) {
	now := time.Now()
	store := restartedReworkStore(t, func(s *stagelabels.Store, at func(time.Time)) {
		at(now.Add(-13 * 24 * time.Hour)) // closed by a maintainer, unseen since
		noteRework(s, "org/r", 702, stagelabels.AdoptedByHive, "alice")
		noteRework(s, "org/r", 769, stagelabels.ReturnedToContributor, "alice")
		at(now.Add(-9 * 24 * time.Hour)) // merged by a maintainer, unseen since
		noteRework(s, "org/r", 801, stagelabels.AdoptedByHive, "alice")
		at(now.Add(-5 * time.Minute)) // open, seen by the last pass before the restart
		noteRework(s, "org/r", 972, stagelabels.ReturnedToContributor, "alice")
	})
	cases := map[string]*FrontendRepo{
		"no status yet":      nil,
		"listing incomplete": {Name: "r", Full: "org/r", CountsIncomplete: true},
		"never enumerated":   {Name: "r", Full: "org/r", ownershipDataMissing: true},
	}
	for name, repo := range cases {
		t.Run(name, func(t *testing.T) {
			hub, srv, _ := reworkHub(t)
			hub.stageStore = store
			fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
			if repo != nil {
				setReworkStatus(srv, *repo)
			}

			fc := snapshotFor(t, hub, "alice")
			if got := pendingNumbers(fc); fc.PendingRework != 1 || len(got) != 1 || got[0] != 972 {
				t.Fatalf("pending_rework=%d prs=%v, want only #972 (seen by the last pass); #702/#769/#801 have not been listed for days", fc.PendingRework, got)
			}
		})
	}
}

func TestFleetReworkAfterRestartKeepsFreshlyReturnedPR(t *testing.T) {
	now := time.Now()
	store := restartedReworkStore(t, func(s *stagelabels.Store, at func(time.Time)) {
		// Returned to the contributor just before the restart; the last
		// pass saw it a minute before the hub went down.
		at(now.Add(-time.Minute))
		noteRework(s, "org/r", 5, stagelabels.ReturnedToContributor, "alice")
		// Seen just inside the bound: an outage shorter than a day keeps it.
		at(now.Add(-pendingReworkUnseenBound + time.Hour))
		noteRework(s, "org/r", 6, stagelabels.ReturnedToContributor, "alice")
	})
	hub, srv, _ := reworkHub(t)
	hub.stageStore = store
	fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	setReworkStatus(srv, FrontendRepo{Name: "r", Full: "org/r", CountsStale: true})

	fc := snapshotFor(t, hub, "alice")
	if got := pendingNumbers(fc); fc.PendingRework != 2 || len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Fatalf("pending_rework=%d prs=%v, want #5 and #6 (072): both seen within %s", fc.PendingRework, got, pendingReworkUnseenBound)
	}
	if fc.PendingReworkPRs[0].Stage != stagelabels.ReturnedToContributor.Label() {
		t.Fatalf("stage = %q, want 072", fc.PendingReworkPRs[0].Stage)
	}
}

func TestFleetReworkVouchedListingDecidesOverRecordAge(t *testing.T) {
	// #14708 behaviour stands where the listing is complete: a PR it lists
	// open is owed rework however long ago a stage pass last touched it.
	now := time.Now()
	store := restartedReworkStore(t, func(s *stagelabels.Store, at func(time.Time)) {
		at(now.Add(-3 * 24 * time.Hour))
		noteRework(s, "org/r", 1, stagelabels.ReturnedToContributor, "alice") // listed open
		noteRework(s, "org/r", 2, stagelabels.ReturnedToContributor, "alice") // not listed
		at(now)
		noteRework(s, "org/r", 3, stagelabels.ReturnedToContributor, "alice") // fresh, not listed
	})
	hub, srv, _ := reworkHub(t)
	hub.stageStore = store
	fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	setReworkStatus(srv, FrontendRepo{Name: "r", Full: "org/r", OpenPrs: []any{
		FrontendPR{PullRequest: github.PullRequest{Repo: "org/r", Number: 1, Author: "alice"}},
	}})

	fc := snapshotFor(t, hub, "alice")
	if got := pendingNumbers(fc); fc.PendingRework != 1 || len(got) != 1 || got[0] != 1 {
		t.Fatalf("pending_rework=%d prs=%v, want only #1: a complete listing decides", fc.PendingRework, got)
	}
}

func TestFleetReworkSkipsRecordsTheStoreMarksFinished(t *testing.T) {
	hub, _, store := reworkHub(t)
	fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	now := time.Now()

	// Superseded: the hive's copy merged and the original was closed
	// (or told, when the close was refused). The stage tracker drops it.
	noteRework(store, "org/r", 1, stagelabels.AdoptedByHive, "alice")
	store.MarkSuperseded(stagelabels.Key("org/r", 1), 11, false, now)
	// Close-first supersession: the original closed as soon as the copy
	// existed, while the copy is still in flight.
	noteRework(store, "org/r", 2, stagelabels.AdoptedByHive, "alice")
	store.MarkSupersededClosedFirst(stagelabels.Key("org/r", 2), 12, false, now)
	// A lane reported the PR closed or merged: the stage pass forgets it.
	noteRework(store, "org/r", 3, stagelabels.ReturnedToContributor, "alice")
	store.Forget(stagelabels.Key("org/r", 3))
	// A reopen for a failed copy clears the supersession and the next stage
	// pass returns the PR to its author: owed again.
	noteRework(store, "org/r", 4, stagelabels.AdoptedByHive, "alice")
	store.MarkSuperseded(stagelabels.Key("org/r", 4), 14, false, now)
	store.MarkAdoptionReopened(stagelabels.Key("org/r", 4), 14, now)
	noteRework(store, "org/r", 4, stagelabels.ReturnedToContributor, "alice")

	for _, ai := range store.ByAuthor("alice", stagelabels.ReturnedToContributor) {
		if _, n := splitStageKey(ai.Key); n == 3 {
			t.Fatalf("ByAuthor returned a forgotten record: %+v", ai)
		}
	}
	fc := snapshotFor(t, hub, "alice")
	if got := pendingNumbers(fc); fc.PendingRework != 1 || len(got) != 1 || got[0] != 4 {
		t.Fatalf("pending_rework=%d prs=%v, want only the reopened #4", fc.PendingRework, got)
	}
}

func TestOwedReworkBound(t *testing.T) {
	now := time.Now()
	var unvouched openPRIndex
	rec := func(seen time.Time) stagelabels.Record {
		return stagelabels.Record{Stage: stagelabels.ReturnedToContributor, LastSeen: seen}
	}
	if !owedRework(unvouched, "org/r", 1, rec(now.Add(-pendingReworkUnseenBound)), now) {
		t.Fatal("a record seen exactly one bound ago is still owed")
	}
	if owedRework(unvouched, "org/r", 1, rec(now.Add(-pendingReworkUnseenBound-time.Minute)), now) {
		t.Fatal("a record unseen past the bound is not owed")
	}
	if !owedRework(unvouched, "org/r", 1, rec(time.Time{}), now) {
		t.Fatal("a record with no LastSeen is not evidence of a close")
	}
	if pendingReworkUnseenBound != stagelabels.ReturnedToContributor.Dwell() || pendingReworkUnseenBound <= stagelabels.LastSeenPersistEvery {
		t.Fatalf("bound %s: want 072's dwell, well above the LastSeen persist interval", pendingReworkUnseenBound)
	}
}
