package beads

import (
	"sync"
	"testing"
	"time"
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

// TestBeadClone_DeepCopiesEveryBranch pins clone and cloneMetadataValue:
// each container shape in Metadata, the DependsOn slice and both time
// pointers must be duplicated so a mutation on the copy is invisible to
// the original, and scalars pass through unchanged.
func TestBeadClone_DeepCopiesEveryBranch(t *testing.T) {
	closed := flexTime{time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	seen := flexTime{time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)}
	orig := &Bead{
		ID:         "b1",
		DependsOn:  []string{"d1"},
		ClosedAt:   &closed,
		LastSeenAt: &seen,
		Metadata: map[string]interface{}{
			"nested": map[string]interface{}{"k": "v"},
			"list":   []interface{}{"a", map[string]interface{}{"x": 1}},
			"strs":   []string{"s1"},
			"strmap": map[string]string{"m": "n"},
			"scalar": 42,
		},
	}

	c := orig.clone()

	c.DependsOn[0] = "changed"
	c.Metadata["nested"].(map[string]interface{})["k"] = "changed"
	c.Metadata["list"].([]interface{})[0] = "changed"
	c.Metadata["list"].([]interface{})[1].(map[string]interface{})["x"] = 2
	c.Metadata["strs"].([]string)[0] = "changed"
	c.Metadata["strmap"].(map[string]string)["m"] = "changed"
	c.Metadata["scalar"] = 43
	c.ClosedAt.Time = c.ClosedAt.Add(time.Hour)
	c.LastSeenAt.Time = c.LastSeenAt.Add(time.Hour)

	if orig.DependsOn[0] != "d1" {
		t.Errorf("DependsOn shared with clone: %v", orig.DependsOn)
	}
	if got := orig.Metadata["nested"].(map[string]interface{})["k"]; got != "v" {
		t.Errorf("nested map shared with clone: %v", got)
	}
	list := orig.Metadata["list"].([]interface{})
	if list[0] != "a" || list[1].(map[string]interface{})["x"] != 1 {
		t.Errorf("[]interface{} shared with clone: %v", list)
	}
	if got := orig.Metadata["strs"].([]string)[0]; got != "s1" {
		t.Errorf("[]string shared with clone: %v", got)
	}
	if got := orig.Metadata["strmap"].(map[string]string)["m"]; got != "n" {
		t.Errorf("map[string]string shared with clone: %v", got)
	}
	if got := orig.Metadata["scalar"]; got != 42 {
		t.Errorf("scalar changed on original: %v", got)
	}
	if !orig.ClosedAt.Equal(closed.Time) {
		t.Errorf("ClosedAt shared with clone: %v", orig.ClosedAt)
	}
	if !orig.LastSeenAt.Equal(seen.Time) {
		t.Errorf("LastSeenAt shared with clone: %v", orig.LastSeenAt)
	}

	// Nil containers stay nil rather than becoming empty allocations.
	bare := (&Bead{ID: "b2"}).clone()
	if bare.Metadata != nil || bare.DependsOn != nil || bare.ClosedAt != nil || bare.LastSeenAt != nil {
		t.Errorf("clone of bare bead allocated containers: %+v", bare)
	}
}
