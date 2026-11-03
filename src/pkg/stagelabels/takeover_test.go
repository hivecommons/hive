package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

// An operator's Adopt now / Reroute (hivecommons/hive#15038) is recorded on
// an existing record only; a forced adoption drops a pending reroute, a
// pickup fallback stamps when the window lapsed, and a moved head ends the
// reroute.
func TestStoreTakeover(t *testing.T) {
	now := time.Date(2026, 10, 28, 20, 0, 0, 0, time.UTC)
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	key := Key("hivecommons/hive", 15038)

	// Unknown key or empty login: no-op, no record created.
	st.NoteAdoptionForced(key, "op", now)
	st.NoteReroute(key, "aaa", "bee2", "op", "address review", now)
	st.ClearAdoptionForced(key)
	st.ClearReroute(key, true, now)
	if _, ok := st.Get(key); ok {
		t.Fatal("takeover notes created a record")
	}
	st.Observe(key, ReturnedToContributor, "waiting on @danathar")
	st.NoteAdoptionForced(key, "", now)
	st.NoteReroute(key, "aaa", "", "op", "r", now)
	r, _ := st.Get(key)
	if by, ok := r.AdoptionForced(); ok || by != "" || r.Rerouted("aaa") != "" {
		t.Fatalf("empty login must not record: %+v", r)
	}
	// Clearing what is not there is a no-op.
	st.ClearAdoptionForced(key)
	st.ClearReroute(key, true, now)
	if r, _ = st.Get(key); !r.RerouteFallbackAt.IsZero() {
		t.Fatal("ClearReroute on a non-rerouted record stamped a fallback")
	}

	// Reroute: recorded for the head, read back per head.
	st.NoteReroute(key, "aaa", "bee2", "op", "address review", now)
	r, _ = st.Get(key)
	if r.Rerouted("aaa") != "bee2" || r.Rerouted("") != "bee2" || r.RerouteBy != "op" || r.RerouteReason != "address review" || !r.RerouteAt.Equal(now) {
		t.Fatalf("reroute: %+v", r)
	}
	if r.Rerouted("bbb") != "" {
		t.Fatal("a moved head must end the reroute")
	}

	// Forced adoption drops the pending reroute.
	st.NoteAdoptionForced(key, "op", now.Add(time.Minute))
	r, _ = st.Get(key)
	by, ok := r.AdoptionForced()
	if !ok || by != "op" || !r.AdoptForcedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("forced: %+v", r)
	}
	if r.Rerouted("aaa") != "" || r.RerouteTo != "" || r.RerouteHead != "" {
		t.Fatalf("forced adoption must drop the reroute: %+v", r)
	}
	st.ClearAdoptionForced(key)
	if r, _ = st.Get(key); r.AdoptForcedBy != "" || !r.AdoptForcedAt.IsZero() {
		t.Fatalf("clear forced: %+v", r)
	}

	// Reroute again; a fallback clear stamps RerouteFallbackAt, a plain
	// clear (picked up) does not.
	st.NoteReroute(key, "aaa", "bee3", "op", "", now)
	st.ClearReroute(key, false, now.Add(2*time.Minute))
	r, _ = st.Get(key)
	if r.RerouteTo != "" || !r.RerouteFallbackAt.IsZero() {
		t.Fatalf("picked-up clear: %+v", r)
	}
	st.NoteReroute(key, "aaa", "bee3", "op", "", now)
	st.ClearReroute(key, true, now.Add(31*time.Minute))
	r, _ = st.Get(key)
	if r.RerouteTo != "" || !r.RerouteFallbackAt.Equal(now.Add(31*time.Minute)) {
		t.Fatalf("fallback clear: %+v", r)
	}
	// A fresh reroute forgets the old fallback stamp.
	st.NoteReroute(key, "bbb", "bee2", "op", "", now)
	if r, _ = st.Get(key); !r.RerouteFallbackAt.IsZero() || r.Rerouted("bbb") != "bee2" {
		t.Fatalf("re-reroute: %+v", r)
	}

	// Persisted and reloaded.
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st2 := NewStore(st.path, func() time.Time { return now })
	if err := st2.Load(); err != nil {
		t.Fatal(err)
	}
	if r, _ = st2.Get(key); r.Rerouted("bbb") != "bee2" || r.RerouteBy != "op" {
		t.Fatalf("reloaded: %+v", r)
	}
}
