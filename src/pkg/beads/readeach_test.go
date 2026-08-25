package beads

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newReadEachStore builds a store with four beads whose CreatedAt timestamps
// are pinned to known, strictly increasing instants so ordering assertions
// cannot flake on clock resolution. Returned IDs are in creation order.
func newReadEachStore(t *testing.T) (*Store, []string) {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "beads"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	specs := []struct {
		title, actor, ref string
		status            Status
	}{
		{"first", "alice", "repo/a.go", StatusOpen},
		{"second", "bob", "repo/b.go", StatusOpen},
		{"third", "alice", "repo/a.go", StatusClosed},
		{"fourth", "carol", "", StatusInProgress},
	}

	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ids := make([]string, 0, len(specs))
	for i, spec := range specs {
		b, err := s.Create(spec.title, TypeTask, PriorityMedium, spec.actor, spec.ref)
		if err != nil {
			t.Fatalf("Create %q: %v", spec.title, err)
		}
		created := base.Add(time.Duration(i) * time.Minute)
		status := spec.status
		if err := s.Update(b.ID, func(b *Bead) {
			b.CreatedAt = flexTime{created}
			b.Status = status
		}); err != nil {
			t.Fatalf("Update %q: %v", spec.title, err)
		}
		ids = append(ids, b.ID)
	}
	return s, ids
}

// collectTitles runs ReadEach with the given filter and copies out each bead's
// title, obeying the documented rule that fn must not retain the *Bead.
func collectTitles(s *Store, filter ListFilter) []string {
	var titles []string
	s.ReadEach(filter, func(b *Bead) {
		titles = append(titles, b.Title)
	})
	return titles
}

func assertTitles(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d: got %v, want %v", i, got, want)
		}
	}
}

// TestReadEach_NilFnIsNoOp verifies the guard: a nil callback must return
// without panicking and without touching the lock.
func TestReadEach_NilFnIsNoOp(t *testing.T) {
	s, _ := newReadEachStore(t)
	s.ReadEach(ListFilter{}, nil) // must not panic
}

// TestReadEach_NoFilterVisitsAllInCreationOrder verifies that an empty filter
// visits every bead exactly once, ordered by CreatedAt — the same contract
// List documents.
func TestReadEach_NoFilterVisitsAllInCreationOrder(t *testing.T) {
	s, _ := newReadEachStore(t)
	assertTitles(t, collectTitles(s, ListFilter{}),
		[]string{"first", "second", "third", "fourth"})
}

// TestReadEach_FiltersMatchListSemantics verifies each ListFilter field
// narrows the visited set the same way List does.
func TestReadEach_FiltersMatchListSemantics(t *testing.T) {
	s, _ := newReadEachStore(t)

	open := StatusOpen
	assertTitles(t, collectTitles(s, ListFilter{Status: &open}),
		[]string{"first", "second"})

	alice := "alice"
	assertTitles(t, collectTitles(s, ListFilter{Actor: &alice}),
		[]string{"first", "third"})

	ref := "repo/a.go"
	closed := StatusClosed
	assertTitles(t, collectTitles(s, ListFilter{ExternalRef: &ref, Status: &closed}),
		[]string{"third"})

	nobody := "nobody"
	assertTitles(t, collectTitles(s, ListFilter{Actor: &nobody}), nil)
}

// TestReadEach_AgreesWithList pins ReadEach to List for the same filter, so
// the two projections of the ledger cannot silently drift apart.
func TestReadEach_AgreesWithList(t *testing.T) {
	s, _ := newReadEachStore(t)
	open := StatusOpen
	filter := ListFilter{Status: &open}

	listed := s.List(filter)
	want := make([]string, 0, len(listed))
	for _, b := range listed {
		want = append(want, b.Title)
	}
	assertTitles(t, collectTitles(s, filter), want)
}

// TestReadEach_ConcurrentUpdateIsRaceFree exercises the reason ReadEach exists
// (kubestellar/hive#3845): reading bead fields concurrently with in-place
// Update mutations. Run under -race (the CI default for this repo), a
// regression that moves the field reads back outside the lock fails this test.
func TestReadEach_ConcurrentUpdateIsRaceFree(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "beads"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	const n = 8
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		b, err := s.Create(fmt.Sprintf("bead-%d", i), TypeTask, PriorityMedium, "alice", "")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, b.ID)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	go func() { // writer: mutates Status and DependsOn in place, as Update callers do
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			id := ids[i%n]
			_ = s.Update(id, func(b *Bead) {
				if b.Status == StatusOpen {
					b.Status = StatusInProgress
				} else {
					b.Status = StatusOpen
				}
				b.DependsOn = append(b.DependsOn[:0], ids[(i+1)%n])
			})
		}
	}()

	// Reader on the test goroutine: projects every field the admission gate
	// reads. Its fixed iteration count bounds the test.
	for i := 0; i < 200; i++ {
		s.ReadEach(ListFilter{}, func(b *Bead) {
			_ = b.Status
			_ = b.UpdatedAt
			for _, dep := range b.DependsOn {
				_ = dep
			}
		})
	}

	close(stop)
	wg.Wait()
}
