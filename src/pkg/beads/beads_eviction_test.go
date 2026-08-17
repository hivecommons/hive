package beads

import (
	"fmt"
	"testing"
	"time"
)

func TestEvictOldClosedTriggersEviction(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	// Inject beads directly into the map to avoid 5000+ disk writes
	const extra = 10
	total := maxBeadCount + extra
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("bead-%05d", i)
		status := StatusOpen
		if i%2 == 0 {
			status = StatusClosed
		}
		store.beads[id] = &Bead{
			ID:        id,
			Title:     fmt.Sprintf("test bead %d", i),
			Type:      TypeTask,
			Status:    status,
			Priority:  PriorityLow,
			Actor:     "scanner",
			CreatedAt: flexTime{time.Now().UTC().Add(-time.Duration(total-i) * time.Minute)},
			UpdatedAt: flexTime{time.Now().UTC().Add(-time.Duration(total-i) * time.Minute)},
		}
	}

	beforeCount := len(store.beads)
	store.evictOldClosed()
	afterCount := len(store.beads)
	store.mu.Unlock()

	if afterCount >= beforeCount {
		t.Errorf("eviction should reduce count: before=%d after=%d", beforeCount, afterCount)
	}
	if afterCount > maxBeadCount {
		t.Errorf("after eviction should be <= %d, got %d", maxBeadCount, afterCount)
	}
}

func TestEvictOldClosedNoClosedBeads(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	const extra = 5
	total := maxBeadCount + extra
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("open-%05d", i)
		store.beads[id] = &Bead{
			ID:        id,
			Title:     fmt.Sprintf("open bead %d", i),
			Type:      TypeTask,
			Status:    StatusOpen,
			Priority:  PriorityLow,
			Actor:     "scanner",
			CreatedAt: flexTime{time.Now().UTC()},
			UpdatedAt: flexTime{time.Now().UTC()},
		}
	}

	beforeCount := len(store.beads)
	store.evictOldClosed()
	afterCount := len(store.beads)
	store.mu.Unlock()

	if afterCount != beforeCount {
		t.Errorf("no closed beads to evict: before=%d after=%d", beforeCount, afterCount)
	}
}

func TestEvictOldClosedMixedStatuses(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	const extra = 20
	total := maxBeadCount + extra
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("mix-%05d", i)
		status := StatusOpen
		switch i % 4 {
		case 0:
			status = StatusClosed
		case 1:
			status = StatusDone
		}
		store.beads[id] = &Bead{
			ID:        id,
			Title:     fmt.Sprintf("mixed bead %d", i),
			Type:      TypeAdvisory,
			Status:    status,
			Priority:  PriorityMedium,
			Actor:     "quality",
			CreatedAt: flexTime{time.Now().UTC().Add(-time.Duration(total-i) * time.Second)},
			UpdatedAt: flexTime{time.Now().UTC().Add(-time.Duration(total-i) * time.Second)},
		}
	}

	store.evictOldClosed()
	afterCount := len(store.beads)
	store.mu.Unlock()

	if afterCount > maxBeadCount {
		t.Errorf("after eviction should be <= %d, got %d", maxBeadCount, afterCount)
	}
}
