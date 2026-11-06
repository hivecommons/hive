package stagelabels

import (
	"testing"
	"time"
)

// The intent baseline is the contributor's head at in-place adoption; hive
// pushes move AdoptedHead but never the baseline. A record written before
// AdoptedIntentHead existed falls back to AdoptedHead.
func TestIntentBaseHead(t *testing.T) {
	now := time.Date(2026, 11, 3, 21, 0, 0, 0, time.UTC)
	s := NewStore("", func() time.Time { return now })
	key := Key("o/utah", 1091)
	s.Observe(key, AdoptedByHive, "adopted")
	s.MarkAdoptedInPlace(key, "7e9dfd7", "contributor-offline", now)
	s.MarkAdoptedHead(key, "a75a567")
	rec, _ := s.Get(key)
	if rec.AdoptedHead != "a75a567" || rec.IntentBaseHead() != "7e9dfd7" {
		t.Fatalf("record = %+v, baseline %q", rec, rec.IntentBaseHead())
	}
	if got := (Record{AdoptedHead: "old"}).IntentBaseHead(); got != "old" {
		t.Fatalf("pre-field record baseline = %q, want AdoptedHead", got)
	}
}
