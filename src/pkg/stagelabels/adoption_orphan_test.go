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
	store.RevertOrphanAdoption(key, "aaa", 90)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded := NewStore(path, func() time.Time { return now })
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	rec, ok := reloaded.Get(key)
	if !ok || rec.AdoptedBy != 0 || rec.AdoptOrphanPrevious != 90 || !rec.AdoptOrphanRetriedAt.Equal(now) || rec.AdoptOrphanRevertedHead != "aaa" || rec.AdoptMarkerReleasedBy != 90 {
		t.Fatalf("lost orphan retry guard: %+v", rec)
	}
}

// ClearAdoption must drop the marker-read cache: with nothing on record the
// public marker is the only evidence left and a lost-store orphan would
// otherwise never be re-read while the head and comment count hold still.
func TestClearAdoptionResetsMarkerReadCache(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore(filepath.Join(t.TempDir(), "stages.json"), func() time.Time { return now })
	key := Key("o/r", 7)
	store.Observe(key, AdoptedByHive, "adopted")
	store.MarkAdopted(key, "aaa", 90, now)
	store.NoteAdoptionMarkerRead(key, "aaa", 3)
	store.ClearAdoption(key)
	rec, _ := store.Get(key)
	if rec.AdoptMarkerChecked || rec.AdoptMarkerHead != "" || rec.AdoptMarkerComments != 0 {
		t.Fatalf("marker cache survived ClearAdoption: %+v", rec)
	}
	if rec.AdoptMarkerReleasedBy != 0 {
		t.Fatalf("plain ClearAdoption must not mark the marker released: %+v", rec)
	}
	store.NoteAdoptionMarkerReleased(key, 90)
	rec, _ = store.Get(key)
	if rec.AdoptMarkerReleasedBy != 90 {
		t.Fatalf("released superseder not recorded: %+v", rec)
	}
}
