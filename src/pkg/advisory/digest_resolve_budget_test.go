package advisory

import (
	"fmt"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// A closed advisory bead whose persisted resolved_at already lies outside the
// recently-resolved window must not cost a live GitHub lookup: the linked
// reference pass can only move the timestamp earlier, so the verdict cannot
// change. Before this every closed bead in the store was re-resolved on every
// governor cycle.
func TestBuildDigest_SkipsLookupForLongResolvedBeads(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"guide": store}
	old := time.Now().Add(-recentlyResolvedWindow - 24*time.Hour)
	for i := 1; i <= 5; i++ {
		b, err := store.Create(fmt.Sprintf("stale finding %d (see #%d)", i, 100+i), beads.TypeAdvisory, beads.PriorityMedium, "guide", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.SetMetadata(b.ID, resolvedAtMetadataKey, formatResolvedAt(old)); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	opts := DigestOptions{
		Org:         "acme",
		PrimaryRepo: "widgets",
		ResolveRef: func(owner, repo string, number int) (RefState, bool) {
			calls++
			return RefState{Closed: true, ClosedAt: time.Now()}, true
		},
	}
	d := BuildDigestFromBeads(stores, "ACTIVE", opts)
	if calls != 0 {
		t.Fatalf("ResolveRef called %d times for beads resolved outside the window; want 0", calls)
	}
	if len(d.RecentlyResolved) != 0 {
		t.Fatalf("long-resolved beads surfaced as recently resolved: %+v", d.RecentlyResolved)
	}
}

// Beads that DO need resolving share one memoized, budgeted resolver per
// build: an issue cited by several beads is looked up once.
func TestBuildDigest_MemoizesLinkedRefLookups(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"guide": store}
	for i := 1; i <= 4; i++ {
		b, err := store.Create(fmt.Sprintf("finding %d tracked in #77", i), beads.TypeAdvisory, beads.PriorityMedium, "guide", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	opts := DigestOptions{
		Org:         "acme",
		PrimaryRepo: "widgets",
		ResolveRef: func(owner, repo string, number int) (RefState, bool) {
			calls++
			return RefState{Closed: true, ClosedAt: time.Now().Add(-time.Hour)}, true
		},
	}
	d := BuildDigestFromBeads(stores, "ACTIVE", opts)
	if calls != 1 {
		t.Fatalf("ResolveRef called %d times for one distinct ref; want 1", calls)
	}
	if len(d.RecentlyResolved) != 4 {
		t.Fatalf("recently resolved = %d, want 4", len(d.RecentlyResolved))
	}
}

// The two lookup passes share a memo cache but NOT a budget: a store with
// more distinct closed-bead refs than one pass may look up must not leave
// partitionSettledStale with an exhausted resolver, or settled stale findings
// would stay open purely because of pass order.
func TestBuildDigest_StalePassKeepsOwnBudget(t *testing.T) {
	store := staleFindingStore(t, staleDetail)
	for i := 1; i <= staleRefLookupBudget+5; i++ {
		b, err := store.Create(fmt.Sprintf("closed finding %d cites #%d", i, 1000+i), beads.TypeAdvisory, beads.PriorityMedium, "quality", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatal(err)
		}
	}
	closedAt := time.Now().Add(-24 * time.Hour)
	resolve := func(owner, repo string, number int) (RefState, bool) {
		return RefState{Closed: true, ClosedAt: closedAt}, true
	}
	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "advisory", DigestOptions{
		MaxFindings: 10,
		ResolveRef:  resolve,
		Snapshot:    &Snapshot{Owner: "Danathar", Repo: "atomic-image-builder", SHA: analyzedAt},
	})
	if got := len(digestFindings(d)); got != 0 {
		t.Fatalf("%d findings still open; the stale finding names closed work and must be retired even after the closed-bead pass spent its budget", got)
	}
}
