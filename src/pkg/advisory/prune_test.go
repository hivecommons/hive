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

// TestMarkStaleAdvisoryBeadsDoesNotResolveAbsentFindings pins #6262: a finding
// absent from the next agent output is not proven fixed. It stays open, is
// captioned as unverified in the digest, and does not move to Recently Resolved.
func TestMarkStaleAdvisoryBeadsDoesNotResolveAbsentFindings(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	window := 7 * 24 * time.Hour

	stale := newPrunableBead(t, store, "stale finding nobody reports anymore", time.Now().Add(-10*24*time.Hour))
	fresh := newPrunableBead(t, store, "fresh finding re-reported this morning", time.Now().Add(-1*time.Hour))
	legacy := newPrunableBead(t, store, "legacy finding filed before last_seen_at existed", time.Time{})

	marked := MarkStaleAdvisoryBeads(map[string]*beads.Store{"scanner": store}, window)

	if len(marked) != 1 || marked[0] != stale.Title {
		t.Fatalf("marked titles = %v, want exactly [%q]", marked, stale.Title)
	}
	if got := statusOf(t, store, stale.ID); got != beads.StatusOpen {
		t.Errorf("stale bead status = %q, want %q — silence is not a resolution", got, beads.StatusOpen)
	}
	if got := statusOf(t, store, fresh.ID); got != beads.StatusOpen {
		t.Errorf("fresh bead status = %q, want %q — a re-reported finding must survive", got, beads.StatusOpen)
	}
	if got := statusOf(t, store, legacy.ID); got != beads.StatusOpen {
		t.Errorf("nil-LastSeenAt bead status = %q, want %q — pre-Upsert beads must never be marked stale", got, beads.StatusOpen)
	}

	sb, _ := store.Get(stale.ID)
	if got := sb.Meta(staleUnverifiedMetadataKey); got != staleUnverifiedReason {
		t.Errorf("stale marker = %q, want %q", got, staleUnverifiedReason)
	}
	d := BuildDigestFromBeads(map[string]*beads.Store{"scanner": store}, "observe", DigestOptions{})
	if len(d.RecentlyResolved) != 0 {
		t.Fatalf("RecentlyResolved = %+v, want none for an absent-but-unproven finding", d.RecentlyResolved)
	}
	got, ok := d.ByAgent["scanner"]
	if !ok || len(got) != 3 {
		t.Fatalf("open findings = %+v, want stale/fresh/legacy still open", d.ByAgent)
	}
	var sawStale bool
	for _, f := range got {
		if f.Title == stale.Title {
			sawStale = f.StaleUnverified
		}
	}
	if !sawStale {
		t.Fatalf("stale finding was not marked unverified in digest: %+v", got)
	}
}

// TestMarkStaleAdvisoryBeadsSkipsNonAdvisoryTypes confirms the marker stays
// inside the digest's own bead types: an agent's internal task bead is work in
// progress, not a finding, and marking it would create noisy false warnings.
func TestMarkStaleAdvisoryBeadsSkipsNonAdvisoryTypes(t *testing.T) {
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

	if marked := MarkStaleAdvisoryBeads(map[string]*beads.Store{"scanner": store}, 24*time.Hour); len(marked) != 0 {
		t.Fatalf("marked = %v, want none — task beads are not advisory findings", marked)
	}
	if got := statusOf(t, store, task.ID); got != beads.StatusOpen {
		t.Errorf("task bead status = %q, want %q", got, beads.StatusOpen)
	}
}

func TestStaleAdvisoryMarkerClearedOnFreshReport(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	old := newPrunableBead(t, store, "issue #157 launcher log API exists; requester relay still missing", time.Now().Add(-10*24*time.Hour))
	if marked := MarkStaleAdvisoryBeads(map[string]*beads.Store{"scanner": store}, 7*24*time.Hour); len(marked) != 1 {
		t.Fatalf("marked = %v, want one", marked)
	}

	PersistAsBeads([]Finding{{
		Agent:    "scanner",
		Type:     "advisory",
		Severity: "high",
		Title:    old.Title,
		Detail:   "still open in hivecommons/hive#157",
	}}, map[string]*beads.Store{"scanner": store})

	got, err := store.Get(old.ID)
	if err != nil {
		t.Fatalf("reading bead: %v", err)
	}
	if marker := got.Meta(staleUnverifiedMetadataKey); marker != "" {
		t.Fatalf("stale marker after fresh report = %q, want cleared", marker)
	}
}
