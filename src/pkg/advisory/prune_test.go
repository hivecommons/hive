package advisory

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// newPrunableBead creates an open advisory bead and stamps its LastSeenAt to
// lastSeen, mimicking what beads.Store.Upsert records when an agent re-files a
// finding. A zero lastSeen leaves LastSeenAt nil (a pre-Upsert bead).
func newPrunableBead(t *testing.T, store *beads.Store, title string, lastSeen time.Time) *beads.Bead {
	t.Helper()
	b, err := store.Create(title, beads.TypeAdvisory, beads.PriorityHigh, "scanner", "")
	if err != nil {
		t.Fatalf("creating bead %q: %v", title, err)
	}
	if !lastSeen.IsZero() {
		if err := store.SetLastSeenAt(b.ID, lastSeen); err != nil {
			t.Fatalf("stamping bead %q: %v", title, err)
		}
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("re-reading bead %q: %v", title, err)
	}
	return got
}

func statusOf(t *testing.T, store *beads.Store, id string) beads.Status {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("reading bead %s: %v", id, err)
	}
	return b.Status
}

// TestPruneStaleAdvisoryBeads is the core of the "beads never close" fix: a
// finding no agent has re-reported inside the window is retired, a freshly
// re-reported one is left alone, and a bead filed before LastSeenAt existed is
// never touched (its silence carries no information).
func TestPruneStaleAdvisoryBeads(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	window := 7 * 24 * time.Hour

	stale := newPrunableBead(t, store, "stale finding nobody reports anymore", time.Now().Add(-10*24*time.Hour))
	fresh := newPrunableBead(t, store, "fresh finding re-reported this morning", time.Now().Add(-1*time.Hour))
	legacy := newPrunableBead(t, store, "legacy finding filed before last_seen_at existed", time.Time{})

	closed := PruneStaleAdvisoryBeads(map[string]*beads.Store{"scanner": store}, window)

	if len(closed) != 1 || closed[0] != stale.Title {
		t.Fatalf("closed titles = %v, want exactly [%q]", closed, stale.Title)
	}
	if got := statusOf(t, store, stale.ID); got != beads.StatusClosed {
		t.Errorf("stale bead status = %q, want %q", got, beads.StatusClosed)
	}
	if got := statusOf(t, store, fresh.ID); got != beads.StatusOpen {
		t.Errorf("fresh bead status = %q, want %q — a re-reported finding must survive", got, beads.StatusOpen)
	}
	if got := statusOf(t, store, legacy.ID); got != beads.StatusOpen {
		t.Errorf("nil-LastSeenAt bead status = %q, want %q — pre-Upsert beads must never be pruned", got, beads.StatusOpen)
	}

	sb, _ := store.Get(stale.ID)
	if got := sb.Meta(closeReasonMetadataKey); got != staleCloseReason {
		t.Errorf("close_reason = %q, want %q", got, staleCloseReason)
	}
}

// TestPruneStaleAdvisoryBeadsSkipsNonAdvisoryTypes confirms the prune stays
// inside the digest's own bead types: an agent's internal task bead is work in
// progress, not a finding, and closing it would silently delete queued work.
func TestPruneStaleAdvisoryBeadsSkipsNonAdvisoryTypes(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	task, err := store.Create("a long-running internal task", beads.TypeTask, beads.PriorityMedium, "scanner", "")
	if err != nil {
		t.Fatalf("creating task bead: %v", err)
	}
	if err := store.SetLastSeenAt(task.ID, time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("stamping task bead: %v", err)
	}

	if closed := PruneStaleAdvisoryBeads(map[string]*beads.Store{"scanner": store}, 24*time.Hour); len(closed) != 0 {
		t.Fatalf("closed = %v, want none — task beads are not advisory findings", closed)
	}
	if got := statusOf(t, store, task.ID); got != beads.StatusOpen {
		t.Errorf("task bead status = %q, want %q", got, beads.StatusOpen)
	}
}
