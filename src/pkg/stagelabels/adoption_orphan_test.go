package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

func TestOrphanRetryGuardSurvivesReversionAndReload(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "stages.json")
	store := NewStore(path, func() time.Time { return now })
	key := Key("o/r", 7)
	store.Observe(key, AdoptedByHive, "adopted")
	store.MarkAdopted(key, "aaa", 90, now)
	store.NoteAdoptionOrphan(key, 90, now, true)
	store.RevertOrphanAdoption(key, "aaa")
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded := NewStore(path, func() time.Time { return now })
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	rec, ok := reloaded.Get(key)
	if !ok || rec.AdoptedBy != 0 || rec.AdoptOrphanPrevious != 90 || !rec.AdoptOrphanRetriedAt.Equal(now) || rec.AdoptOrphanRevertedHead != "aaa" {
		t.Fatalf("lost orphan retry guard: %+v", rec)
	}
}
