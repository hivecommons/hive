package advisory

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// TestPruneStaleAdvisoryBeadsAliasMarksWithoutClosing pins the legacy alias.
// PruneStaleAdvisoryBeads used to CLOSE stale beads; after #6262 it must only
// delegate to MarkStaleAdvisoryBeads. An old caller reaching the alias must get
// the marking behaviour — stale bead stays open, gains the unverified marker —
// and never the historical close. If someone "restores" the close inside the
// alias, this test is the tripwire.
func TestPruneStaleAdvisoryBeadsAliasMarksWithoutClosing(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	window := 7 * 24 * time.Hour
	stale := newPrunableBead(t, store, "stale finding via legacy alias", time.Now().Add(-10*24*time.Hour))
	fresh := newPrunableBead(t, store, "fresh finding via legacy alias", time.Now().Add(-1*time.Hour))

	marked := PruneStaleAdvisoryBeads(map[string]*beads.Store{"scanner": store}, window)

	if len(marked) != 1 || marked[0] != stale.Title {
		t.Fatalf("marked titles = %v, want exactly [%q]", marked, stale.Title)
	}
	if got := statusOf(t, store, stale.ID); got != beads.StatusOpen {
		t.Errorf("stale bead status = %q, want %q — the alias must not close beads", got, beads.StatusOpen)
	}
	if got := statusOf(t, store, fresh.ID); got != beads.StatusOpen {
		t.Errorf("fresh bead status = %q, want %q", got, beads.StatusOpen)
	}
	sb, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("re-reading stale bead: %v", err)
	}
	if got := sb.Meta(staleUnverifiedMetadataKey); got != staleUnverifiedReason {
		t.Errorf("stale marker = %q, want %q", got, staleUnverifiedReason)
	}

	// Zero/negative window is a no-op through the alias too.
	if got := PruneStaleAdvisoryBeads(map[string]*beads.Store{"scanner": store}, 0); got != nil {
		t.Errorf("alias with window 0 marked %v, want nil", got)
	}
}
