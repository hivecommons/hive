package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

// The adoption hold the activity monitor reads (hivecommons/hive#14524):
// noted per head, ignored once the head moves or the PR is adopted,
// cleared when adoption proceeds.
func TestStoreAdoptionBlocked(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), clock)
	key := Key("hivecommons/hive", 14524)

	// Unknown key or empty head/reason: no-op.
	st.NoteAdoptionBlocked(key, "aaa", AdoptBlockedAuthorMaintainer, now)
	if _, ok := st.Get(key); ok {
		t.Fatal("note created a record")
	}
	st.Observe(key, ReturnedToContributor, "check-failing:unit")
	st.NoteAdoptionBlocked(key, "", AdoptBlockedAuthorMaintainer, now)
	st.NoteAdoptionBlocked(key, "aaa", "", now)
	if r, _ := st.Get(key); r.AdoptBlockedHead != "" || r.AdoptBlockedBy != "" {
		t.Fatalf("empty note recorded: %+v", r)
	}

	st.NoteAdoptionBlocked(key, "aaa", AdoptBlockedAuthorMaintainer, now)
	r, _ := st.Get(key)
	if got := r.AdoptionBlocked("aaa"); got != AdoptBlockedAuthorMaintainer || !r.AdoptBlockedAt.Equal(now) {
		t.Fatalf("blocked: %q %+v", got, r)
	}
	if r.AdoptionBlocked("bbb") != "" || r.AdoptionBlocked("") != "" {
		t.Fatal("stale head reported as blocked")
	}
	// Re-noting the same hold keeps the first stamp.
	st.NoteAdoptionBlocked(key, "aaa", AdoptBlockedAuthorMaintainer, now.Add(time.Hour))
	if r, _ = st.Get(key); !r.AdoptBlockedAt.Equal(now) {
		t.Fatalf("re-note moved the stamp: %+v", r)
	}

	st.ClearAdoptionBlocked(key)
	if r, _ = st.Get(key); r.AdoptionBlocked("aaa") != "" || r.AdoptBlockedHead != "" || !r.AdoptBlockedAt.IsZero() {
		t.Fatalf("clear left state: %+v", r)
	}

	// Adoption clears the hold too, in place and by supersession.
	st.NoteAdoptionBlocked(key, "aaa", AdoptBlockedAuthorMaintainer, now)
	st.MarkAdopted(key, "aaa", 14977, now)
	if r, _ = st.Get(key); r.AdoptionBlocked("aaa") != "" || r.AdoptBlockedBy != "" {
		t.Fatalf("MarkAdopted kept the hold: %+v", r)
	}
	key2 := Key("hivecommons/hive", 2)
	st.Observe(key2, ReturnedToContributor, "check-failing:unit")
	st.NoteAdoptionBlocked(key2, "ccc", AdoptBlockedAuthorMaintainer, now)
	st.MarkAdoptedInPlace(key2, "ccc", "contributor-offline", now)
	if r, _ = st.Get(key2); r.AdoptionBlocked("ccc") != "" || r.AdoptBlockedBy != "" {
		t.Fatalf("MarkAdoptedInPlace kept the hold: %+v", r)
	}

	// Persisted across a reload.
	key3 := Key("hivecommons/hive", 3)
	st.Observe(key3, ReturnedToContributor, "check-failing:unit")
	st.NoteAdoptionBlocked(key3, "ddd", AdoptBlockedAuthorMaintainer, now)
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	again := NewStore(st.path, clock)
	if err := again.Load(); err != nil {
		t.Fatal(err)
	}
	if r, ok := again.Get(key3); !ok || r.AdoptionBlocked("ddd") != AdoptBlockedAuthorMaintainer {
		t.Fatalf("hold not persisted: %+v", r)
	}
}
