package beads

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Regression: one malformed timestamp used to poison an ENTIRE agent ledger.
// flexTime.UnmarshalJSON returned an error for any format it didn't know, that
// error surfaced through the store-wide json.Unmarshal of beads.json, and
// NewStore failed — the store was dropped from beadStores, the hive ran on a
// partial ledger for the life of the process, and the hub raised "N bead
// store(s) failed to load at startup". Observed live on torch-spyre: a single
// hand-written `"created_at": "2026-07-23 16:04 EDT"` in one supervisor bead
// kept the whole store unloadable for a month.
//
// The fix is two-layered and both layers are pinned here: the common
// space-separated human formats now PARSE, and anything still unknown
// fail-softs to the zero time instead of erroring — so no future creative
// timestamp can take out a ledger again.
func TestFlexTimeAcceptsHumanFormats(t *testing.T) {
	cases := []struct {
		in       string
		wantYear int
	}{
		// The exact live torch-spyre value. Go resolves the bare "EDT"
		// abbreviation to offset 0 when it is not the local zone — hours-level
		// imprecision is accepted; losing the store is not.
		{`"2026-07-23 16:04 EDT"`, 2026},
		{`"2026-07-23 16:04:05 EDT"`, 2026},
		{`"2026-07-23 16:04:05"`, 2026},
		{`"2026-07-23 16:04"`, 2026},
	}
	for _, c := range cases {
		var ft flexTime
		if err := json.Unmarshal([]byte(c.in), &ft); err != nil {
			t.Fatalf("flexTime rejected %s: %v", c.in, err)
		}
		if ft.Year() != c.wantYear {
			t.Fatalf("flexTime parsed %s to %v (want year %d)", c.in, ft.Time, c.wantYear)
		}
	}
}

func TestFlexTimeFailSoftOnGarbage(t *testing.T) {
	var ft flexTime
	if err := json.Unmarshal([]byte(`"three days after the equinox"`), &ft); err != nil {
		t.Fatalf("an unparseable timestamp must fail-soft, not error: %v", err)
	}
	if !ft.IsZero() {
		t.Fatalf("an unparseable timestamp must land on the zero time, got %v", ft.Time)
	}
	// Non-string JSON is still a hard error: that is file corruption, not a
	// creative timestamp, and the caller should see it.
	if err := json.Unmarshal([]byte(`42`), &ft); err == nil {
		t.Fatal("a non-string timestamp value must still be a hard error")
	}
}

// The store-level consequence: a beads.json carrying the exact live torch-spyre
// bead must LOAD, keeping every other bead readable.
func TestStoreLoadsDespiteMalformedBeadTimestamp(t *testing.T) {
	dir := t.TempDir()
	raw := `[
	  {
	    "id": "sup-sweep-bad",
	    "title": "sweep with hand-written timestamp",
	    "type": "task",
	    "status": "open",
	    "priority": 2,
	    "actor": "supervisor",
	    "created_at": "2026-07-23 16:04 EDT",
	    "updated_at": "2026-07-23 16:04 EDT"
	  },
	  {
	    "id": "sup-good",
	    "title": "healthy bead",
	    "type": "task",
	    "status": "open",
	    "priority": 2,
	    "actor": "supervisor",
	    "created_at": "2026-07-23T16:04:00Z",
	    "updated_at": "2026-07-23T16:04:00Z"
	  }
	]`
	if err := os.WriteFile(filepath.Join(dir, "beads.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("store failed to load over one malformed timestamp — the exact regression: %v", err)
	}
	all := store.List(ListFilter{})
	if len(all) != 2 {
		t.Fatalf("want both beads readable, got %d", len(all))
	}
	good, err := store.Get("sup-good")
	if err != nil || good.CreatedAt.IsZero() {
		t.Fatalf("healthy bead must keep its timestamp: %v / %v", err, good)
	}
	bad, err := store.Get("sup-sweep-bad")
	if err != nil {
		t.Fatalf("malformed-timestamp bead must still be readable: %v", err)
	}
	if bad.CreatedAt.Year() != 2026 {
		t.Fatalf("the EDT shape now parses; want year 2026, got %v", bad.CreatedAt.Time)
	}
	_ = time.Now() // keep time import for future edits
}
