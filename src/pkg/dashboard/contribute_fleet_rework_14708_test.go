package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#14708: a contributor card said "4 PRs awaiting rework"
// when three of them had merged two days earlier still wearing 072. The
// stage records outlived the PRs, so the card must check the PR is open.

func setReworkStatus(srv *Server, repos ...FrontendRepo) {
	srv.statusMu.Lock()
	srv.status = &StatusPayload{Repos: repos}
	srv.statusMu.Unlock()
}

func noteRework(store *stagelabels.Store, repo string, n int, stage stagelabels.Stage, author string) {
	store.Observe(stagelabels.Key(repo, n), stage, "check-failing:build")
	store.NoteAuthor(stagelabels.Key(repo, n), author)
}

func pendingNumbers(fc FleetClanker) []int {
	var out []int
	for _, pr := range fc.PendingReworkPRs {
		out = append(out, pr.Number)
	}
	return out
}

func TestFleetReworkExcludesMergedAndClosedPRs(t *testing.T) {
	hub, srv, store := reworkHub(t)
	fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())

	noteRework(store, "org/r", 1, stagelabels.ReturnedToContributor, "alice") // open
	noteRework(store, "org/r", 2, stagelabels.ReturnedToContributor, "alice") // merged
	noteRework(store, "org/r", 3, stagelabels.AdoptedByHive, "alice")         // closed
	setReworkStatus(srv, FrontendRepo{Name: "r", Full: "org/r", OpenPrs: []any{
		FrontendPR{PullRequest: github.PullRequest{Repo: "org/r", Number: 1, Author: "alice"}},
	}})

	fc := snapshotFor(t, hub, "alice")
	if got := pendingNumbers(fc); fc.PendingRework != 1 || len(got) != 1 || got[0] != 1 {
		t.Fatalf("pending_rework=%d prs=%v, want only the open #1", fc.PendingRework, got)
	}
}

func TestFleetReworkKeepsOpenDraftAndAdoptedPRs(t *testing.T) {
	hub, srv, store := reworkHub(t)
	fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())

	noteRework(store, "org/r", 4, stagelabels.ReturnedToContributor, "alice")
	noteRework(store, "org/r", 5, stagelabels.AdoptedByHive, "alice")
	// A draft is not in OpenPrs; the enumeration carries it as a sidelined
	// row, which is still the canonical open set.
	setReworkStatus(srv, FrontendRepo{Name: "r", Full: "org/r",
		OpenPrs:          []any{FrontendPR{PullRequest: github.PullRequest{Repo: "org/r", Number: 5}}},
		NonActionablePrs: []any{github.RepoWorkPR{Repo: "org/r", Number: 4, Draft: true, Bucket: "draft"}},
	})

	fc := snapshotFor(t, hub, "alice")
	if got := pendingNumbers(fc); fc.PendingRework != 2 || len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("pending_rework=%d prs=%v, want open #4 (draft) and #5 (074)", fc.PendingRework, got)
	}
}

func TestFleetReworkIncompleteListingKeepsRecords(t *testing.T) {
	cases := map[string]FrontendRepo{
		"incomplete": {Name: "r", Full: "org/r", CountsIncomplete: true},
		"stale":      {Name: "r", Full: "org/r", CountsStale: true},
		"unlisted":   {Name: "r", Full: "org/r", UnlistedPRs: 1},
		"paused":     {Name: "r", Full: "org/r", Paused: true},
		// Pre-scan shape: built before the first eval cycle (no actionable
		// snapshot), so no counts or item lists are set at all.
		"never enumerated": {Name: "r", Full: "org/r", ownershipDataMissing: true},
		"other repo":       {Name: "q", Full: "org/q"},
	}
	for name, repo := range cases {
		t.Run(name, func(t *testing.T) {
			hub, srv, store := reworkHub(t)
			fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
			noteRework(store, "org/r", 1, stagelabels.ReturnedToContributor, "alice")
			setReworkStatus(srv, repo)

			fc := snapshotFor(t, hub, "alice")
			if got := pendingNumbers(fc); fc.PendingRework != 1 || len(got) != 1 || got[0] != 1 {
				t.Fatalf("pending_rework=%d prs=%v: a PR missing from a listing that cannot vouch for it must stay", fc.PendingRework, got)
			}
		})
	}
}

func TestFleetReworkNoStatusKeepsRecords(t *testing.T) {
	hub, _, store := reworkHub(t)
	fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	noteRework(store, "org/r", 1, stagelabels.ReturnedToContributor, "alice")

	fc := snapshotFor(t, hub, "alice")
	if fc.PendingRework != 1 {
		t.Fatalf("pending_rework=%d, want 1 before the first status snapshot", fc.PendingRework)
	}
}

func TestOpenPRIndexClosedIsCaseInsensitive(t *testing.T) {
	idx := openPRIndex{repos: map[string]bool{"org/r": true}, prs: map[string]bool{stagelabels.Key("org/r", 1): true}}
	if idx.closed("Org/R", 1) {
		t.Fatal("#1 is listed open")
	}
	if !idx.closed("Org/R", 2) {
		t.Fatal("#2 is absent from a complete listing")
	}
	if (openPRIndex{}).closed("org/r", 2) {
		t.Fatal("an empty index knows nothing")
	}
}
