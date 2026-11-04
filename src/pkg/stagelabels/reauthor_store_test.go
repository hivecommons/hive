package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

// The hive's one-per-issue re-author record (hivecommons/hive#16582):
// marked on the superseded PR's record, idempotent per head, found again
// by issue across the repo, absent for other repos, and persisted.
func TestStoreReauthored(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), clock)
	key := Key("hivecommons/hive", 90)

	// Unknown key, bad issue: no-op; nothing re-authored for the issue.
	st.MarkReauthored(key, 12, "aaa", 6, now)
	st.MarkReauthorNoted(key)
	if _, ok := st.Get(key); ok {
		t.Fatal("Mark* must not create a record")
	}
	if st.ReauthoredFor("hivecommons/hive", 12) != 0 || (*Store)(nil).ReauthoredFor("hivecommons/hive", 12) != 0 {
		t.Fatal("nothing re-authored yet")
	}

	st.Observe(key, Fixing, ReauthoringReason(12))
	st.MarkReauthored(key, 0, "aaa", 6, now)
	if r, _ := st.Get(key); r.Reauthored() {
		t.Fatal("issue 0 must not mark")
	}
	st.MarkReauthored(key, 12, "aaa", 6, now)
	r, _ := st.Get(key)
	if !r.Reauthored() || r.ReauthorIssue != 12 || r.ReauthorHead != "aaa" || r.ReauthorAttempts != 6 || !r.ReauthoredAt.Equal(now) || r.ReauthorNoted {
		t.Fatalf("record = %+v", r)
	}
	// Same issue+head again: unchanged (the first timestamp stands).
	st.MarkReauthored(key, 12, "aaa", 7, now.Add(time.Hour))
	if r, _ = st.Get(key); r.ReauthorAttempts != 6 || !r.ReauthoredAt.Equal(now) {
		t.Fatalf("second mark rewrote the record: %+v", r)
	}
	st.MarkReauthorNoted(key)
	st.MarkReauthorNoted(key)
	if r, _ = st.Get(key); !r.ReauthorNoted {
		t.Fatalf("noted = %+v", r)
	}

	// Lookup by issue: lowest PR in the repo wins; other repos/issues don't match.
	st.Observe(Key("hivecommons/hive", 95), Fixing, "x")
	st.MarkReauthored(Key("hivecommons/hive", 95), 12, "bbb", 6, now)
	st.Observe(Key("acme/docs", 3), Fixing, "x")
	st.MarkReauthored(Key("acme/docs", 3), 12, "ccc", 6, now)
	if got := st.ReauthoredFor("HiveCommons/Hive", 12); got != 90 {
		t.Fatalf("ReauthoredFor = %d, want 90", got)
	}
	if got := st.ReauthoredFor("hivecommons/hive", 13); got != 0 {
		t.Fatalf("ReauthoredFor other issue = %d", got)
	}
	if got := st.ReauthoredFor("acme/docs", 12); got != 3 {
		t.Fatalf("ReauthoredFor acme = %d", got)
	}

	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	again := NewStore(st.path, clock)
	if err := again.Load(); err != nil {
		t.Fatal(err)
	}
	if r, ok := again.Get(key); !ok || r.ReauthorIssue != 12 || !r.ReauthorNoted || again.ReauthoredFor("hivecommons/hive", 12) != 90 {
		t.Fatalf("reloaded = %+v", r)
	}
}
