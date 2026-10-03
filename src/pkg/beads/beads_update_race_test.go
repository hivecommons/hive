package beads

import (
	"sync"
	"testing"
)

// TestUpdate_DoesNotMutateBeadsHeldByReaders pins the copy-on-write contract
// of Update: a *Bead handed out by Get or List before an Update is a stable
// snapshot, so a reader walking its Metadata concurrently with SetMetadata is
// not a data race, and the store's own view carries the new value.
func TestUpdate_DoesNotMutateBeadsHeldByReaders(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	b, err := s.Create("shared", TypeTask, PriorityMedium, "scanner", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.SetMetadata(b.ID, "stage", "plan"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	held, err := s.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = held.Meta("stage")
			for range held.DependsOn {
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := s.SetMetadata(b.ID, "stage", "implement"); err != nil {
				t.Errorf("SetMetadata: %v", err)
				return
			}
			if err := s.AddDependency(b.ID, "other"); err != nil {
				t.Errorf("AddDependency: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	if got := held.Meta("stage"); got != "plan" {
		t.Fatalf("snapshot mutated under the reader: stage = %q, want %q", got, "plan")
	}
	if len(held.DependsOn) != 0 {
		t.Fatalf("snapshot DependsOn mutated under the reader: %v", held.DependsOn)
	}
	now, err := s.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := now.Meta("stage"); got != "implement" {
		t.Fatalf("store view stage = %q, want %q", got, "implement")
	}
	if len(now.DependsOn) != 1 || now.DependsOn[0] != "other" {
		t.Fatalf("store view DependsOn = %v, want [other]", now.DependsOn)
	}
}
