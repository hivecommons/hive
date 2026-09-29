package collect

import (
	"testing"
	"time"
)

// The typed Repo field (#9587) wins over the detail pair, and entries written
// before it existed still attribute through the detail.
func TestAuditEntryRepo_TypedFieldWinsLegacyFallsBack(t *testing.T) {
	cases := []struct {
		name   string
		entry  AuditEntry
		want   string
		wantOK bool
	}{
		{"typed only", AuditEntry{Repo: "z/typed", Detail: "number=1"}, "z/typed", true},
		{"typed wins over detail", AuditEntry{Repo: "z/typed", Detail: "repo=z/legacy, number=1"}, "z/typed", true},
		{"legacy detail", AuditEntry{Detail: "repo=z/legacy, number=1"}, "z/legacy", true},
		{"neither", AuditEntry{Detail: "number=1"}, "", false},
	}
	for _, tc := range cases {
		got, ok := AuditEntryRepo(tc.entry)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("%s: AuditEntryRepo = (%q, %v), want (%q, %v)", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}

// A typed entry with no repo= pair in its detail is still counted per repo:
// the reader does not depend on the legacy pair once the field is present.
func TestActivityCollector_CountsTypedRepoEntries(t *testing.T) {
	now := time.Now().UTC()
	stub := &stubAuditReader{entries: []AuditEntry{
		{Timestamp: rfc3339(now.Add(-time.Hour)), Action: "agent_comment_created", Detail: "number=4", Agent: "quality", Repo: "z/typed", Target: 4},
		{Timestamp: rfc3339(now.Add(-2 * time.Hour)), Action: "agent_comment_created", Detail: "repo=z/typed, number=5", Agent: "quality"},
	}}
	ac := NewActivityCollector(stub, "", nil)
	ac.nowFn = func() time.Time { return now }
	ac.collect()
	snap, ok := ac.Snapshot()
	if !ok {
		t.Fatal("snapshot not ready after collect")
	}
	if len(snap.Repos) != 1 || snap.Repos[0].Repo != "z/typed" || snap.Repos[0].Comments.Count != 2 {
		t.Fatalf("repos = %+v, want one z/typed repo with 2 comments", snap.Repos)
	}
}
