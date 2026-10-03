package holdguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s := Load(filepath.Join(t.TempDir(), "hold-guard.json"))
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	clock := &now
	s.SetClock(func() time.Time { return *clock })
	return s, clock
}

func TestKey(t *testing.T) {
	if got := Key("acme/widgets", 7); got != "acme/widgets#7" {
		t.Fatalf("Key = %q, want acme/widgets#7", got)
	}
}

func TestLoadMissingAndCorruptFiles(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "nope.json"))
	if _, ok := s.Recorded("r", 1); ok {
		t.Fatal("missing ledger must load empty, not fail")
	}

	path := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s = Load(path)
	if _, ok := s.Recorded("r", 1); ok {
		t.Fatal("corrupt ledger must load empty, not fail")
	}
	// A corrupt-loaded store must still be writable.
	if !s.Snapshot("r", 1, "abc", nil) {
		t.Fatal("snapshot after corrupt load must succeed")
	}
}

func TestSnapshotFirstHeldWinsAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hold-guard.json")
	s := Load(path)

	commits := []Commit{
		{SHA: "c1", Author: "zoe", Title: "feat: one"},
		{SHA: "c2", Author: "alice", Title: "feat: two"},
		{SHA: "c3", Author: "alice", Title: "feat: three"},
	}
	if !s.Snapshot("acme/widgets", 7, "c3", commits) {
		t.Fatal("first snapshot must record")
	}
	// A second snapshot while still held must NOT replace the baseline: the
	// first-held tree is what the human gate saw.
	if s.Snapshot("acme/widgets", 7, "c9", []Commit{{SHA: "c9", Author: "mallory"}}) {
		t.Fatal("second snapshot must not overwrite the first-held baseline")
	}
	// Empty head has no tree to pin.
	if s.Snapshot("acme/widgets", 8, "", nil) {
		t.Fatal("empty head SHA must not snapshot")
	}

	rec, ok := s.Recorded("acme/widgets", 7)
	if !ok {
		t.Fatal("Recorded must find the snapshot")
	}
	if rec.HeadSHA != "c3" {
		t.Fatalf("HeadSHA = %q, want c3 (first snapshot wins)", rec.HeadSHA)
	}
	if len(rec.CommitSHAs) != 3 {
		t.Fatalf("CommitSHAs = %v, want 3 entries", rec.CommitSHAs)
	}
	// Authors: sorted, de-duplicated.
	if len(rec.Authors) != 2 || rec.Authors[0] != "alice" || rec.Authors[1] != "zoe" {
		t.Fatalf("Authors = %v, want [alice zoe]", rec.Authors)
	}

	// Reload from disk: the ledger must survive a restart.
	s2 := Load(path)
	rec2, ok := s2.Recorded("acme/widgets", 7)
	if !ok || rec2.HeadSHA != "c3" || len(rec2.Authors) != 2 {
		t.Fatalf("reloaded entry = %+v ok=%v, want persisted snapshot", rec2, ok)
	}
}

func TestSnapshotBoundsTrackedCommits(t *testing.T) {
	s, _ := newTestStore(t)
	commits := make([]Commit, maxTrackedCommits+50)
	for i := range commits {
		commits[i] = Commit{SHA: strings.Repeat("a", 3) + string(rune('0'+i%10)) + time.Duration(i).String(), Author: "bot"}
	}
	s.Snapshot("r", 1, "head", commits)
	rec, _ := s.Recorded("r", 1)
	if len(rec.CommitSHAs) != maxTrackedCommits {
		t.Fatalf("tracked commits = %d, want capped at %d", len(rec.CommitSHAs), maxTrackedCommits)
	}
}

func TestRecordedReturnsACopy(t *testing.T) {
	s, _ := newTestStore(t)
	s.Snapshot("r", 1, "head", []Commit{{SHA: "c1", Author: "alice"}})
	rec, _ := s.Recorded("r", 1)
	rec.Authors[0] = "mallory"
	rec.CommitSHAs[0] = "evil"
	again, _ := s.Recorded("r", 1)
	if again.Authors[0] != "alice" || again.CommitSHAs[0] != "c1" {
		t.Fatalf("Recorded must return a copy; store mutated to %+v", again)
	}
}

func TestTouchClearMarkCommented(t *testing.T) {
	s, clock := newTestStore(t)
	s.Snapshot("r", 1, "head", nil)

	before, _ := s.Recorded("r", 1)
	*clock = clock.Add(time.Hour)
	s.Touch("r", 1)
	after, _ := s.Recorded("r", 1)
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatal("Touch must refresh UpdatedAt")
	}
	// Touch/MarkCommented/Clear on unknown PRs are safe no-ops.
	s.Touch("r", 99)
	s.MarkCommented("r", 99)
	s.Clear("r", 99)

	s.MarkCommented("r", 1)
	rec, _ := s.Recorded("r", 1)
	if !rec.Commented {
		t.Fatal("MarkCommented must set Commented")
	}

	s.Clear("r", 1)
	if _, ok := s.Recorded("r", 1); ok {
		t.Fatal("Clear must remove the entry")
	}
}

func TestReArmReplacesSnapshotAndResetsCommented(t *testing.T) {
	s, _ := newTestStore(t)
	s.Snapshot("r", 1, "old", []Commit{{SHA: "c1", Author: "alice"}})
	s.MarkCommented("r", 1)

	s.ReArm("r", 1, "", nil) // empty head: no-op
	rec, _ := s.Recorded("r", 1)
	if rec.HeadSHA != "old" {
		t.Fatal("ReArm with empty head must not change the snapshot")
	}

	s.ReArm("r", 1, "new", []Commit{{SHA: "c1", Author: "alice"}, {SHA: "c2", Author: "mallory"}})
	rec, _ = s.Recorded("r", 1)
	if rec.HeadSHA != "new" || rec.Commented {
		t.Fatalf("ReArm entry = %+v, want head=new and Commented reset", rec)
	}
	if len(rec.CommitSHAs) != 2 || len(rec.Authors) != 2 {
		t.Fatalf("ReArm sets = %v / %v, want refreshed commit and author sets", rec.CommitSHAs, rec.Authors)
	}

	// ReArm on an untracked PR creates the entry (label re-applied after a
	// restart lost the in-memory path to it).
	s.ReArm("r", 2, "h2", nil)
	if _, ok := s.Recorded("r", 2); !ok {
		t.Fatal("ReArm must create a missing entry")
	}
}

func TestAdvanceHygieneMovesBaselineAndClearsConflictNoted(t *testing.T) {
	s, _ := newTestStore(t)

	// No snapshot exists yet: AdvanceHygiene must not plant one — advancing a
	// baseline that was never pinned would invent a wrong one (#10437).
	s.AdvanceHygiene("r", 1, "new", []Commit{{SHA: "c1", Author: "alice"}})
	if _, ok := s.Recorded("r", 1); ok {
		t.Fatal("AdvanceHygiene must not create an entry for an untracked PR")
	}

	s.Snapshot("r", 1, "old", []Commit{{SHA: "c1", Author: "alice"}})
	s.MarkConflictNoted("r", 1)

	s.AdvanceHygiene("r", 1, "", nil) // empty head: no-op
	rec, _ := s.Recorded("r", 1)
	if rec.HeadSHA != "old" || !rec.ConflictNoted {
		t.Fatal("AdvanceHygiene with empty head must not change the snapshot")
	}

	s.AdvanceHygiene("r", 1, "new", []Commit{{SHA: "c1", Author: "alice"}, {SHA: "c2", Author: "alice"}})
	rec, _ = s.Recorded("r", 1)
	if rec.HeadSHA != "new" || rec.ConflictNoted {
		t.Fatalf("AdvanceHygiene entry = %+v, want head=new and ConflictNoted cleared", rec)
	}
	if len(rec.CommitSHAs) != 2 {
		t.Fatalf("AdvanceHygiene commit set = %v, want the refreshed list", rec.CommitSHAs)
	}
}

func TestConflictCommentBodyNamesTheConflict(t *testing.T) {
	body := ConflictCommentBody()
	if !strings.Contains(body, "conflict") {
		t.Fatalf("ConflictCommentBody = %q, want it to name the conflict", body)
	}
	if !strings.Contains(body, ReHoldLabel) {
		t.Fatalf("ConflictCommentBody = %q, want it to mention the %q label", body, ReHoldLabel)
	}
}

func TestPruneAgesOutStaleEntriesOnly(t *testing.T) {
	s, clock := newTestStore(t)
	s.Snapshot("r", 1, "h1", nil)
	s.Snapshot("r", 2, "h2", nil)

	*clock = clock.Add(Retention + time.Hour)
	s.Touch("r", 2) // still observed held — must survive
	s.Prune(0)      // 0 falls back to Retention

	if _, ok := s.Recorded("r", 1); ok {
		t.Fatal("stale entry must be pruned")
	}
	if _, ok := s.Recorded("r", 2); !ok {
		t.Fatal("touched entry must survive pruning")
	}
}

func TestDiffCleanLift(t *testing.T) {
	rec := Entry{HeadSHA: "same", CommitSHAs: []string{"c1"}, Authors: []string{"alice"}}
	d := Diff(rec, "same", []Commit{{SHA: "c1", Author: "alice"}})
	if d.Moved {
		t.Fatalf("Diff = %+v, want unmoved for identical heads", d)
	}
	if len(d.NewCommits) != 0 || len(d.NewAuthors) != 0 {
		t.Fatalf("clean lift must report no new commits/authors, got %+v", d)
	}
}

func TestDiffFailsClosedOnUnknownHeads(t *testing.T) {
	if d := Diff(Entry{HeadSHA: ""}, "cur", nil); !d.Moved {
		t.Fatal("empty recorded head must read as moved")
	}
	if d := Diff(Entry{HeadSHA: "rec"}, "", nil); !d.Moved {
		t.Fatal("empty current head must read as moved")
	}
}

func TestDiffNamesNewCommitsAndAuthors(t *testing.T) {
	rec := Entry{
		HeadSHA:    "c2",
		CommitSHAs: []string{"c1", "c2"},
		Authors:    []string{"alice"},
	}
	current := []Commit{
		{SHA: "c1", Author: "alice", Title: "feat: base"},
		{SHA: "c2", Author: "alice", Title: "feat: more"},
		{SHA: "f1", Author: "mallory", Title: "sneak: saas.go"},
		{SHA: "f2", Author: "mallory", Title: "sneak: wrapper"},
		{SHA: "f3", Author: "eve", Title: "sneak: manifests"},
	}
	d := Diff(rec, "f3", current)
	if !d.Moved {
		t.Fatal("moved head must read as drift")
	}
	if len(d.NewCommits) != 3 {
		t.Fatalf("NewCommits = %+v, want the 3 foreign commits", d.NewCommits)
	}
	if len(d.NewAuthors) != 2 || d.NewAuthors[0] != "eve" || d.NewAuthors[1] != "mallory" {
		t.Fatalf("NewAuthors = %v, want sorted [eve mallory]", d.NewAuthors)
	}
}

func TestDiffRebaseTreatsEverythingAsNew(t *testing.T) {
	rec := Entry{HeadSHA: "c2", CommitSHAs: []string{"c1", "c2"}, Authors: []string{"alice"}}
	// Force-push rebase: same author, all SHAs renamed. The reviewed tree no
	// longer exists, so every commit must be surfaced — but the author set
	// did not grow.
	d := Diff(rec, "r2", []Commit{{SHA: "r1", Author: "alice"}, {SHA: "r2", Author: "alice"}})
	if !d.Moved || len(d.NewCommits) != 2 {
		t.Fatalf("Diff = %+v, want all rebased commits new", d)
	}
	if len(d.NewAuthors) != 0 {
		t.Fatalf("NewAuthors = %v, want empty for a same-author rebase", d.NewAuthors)
	}
}

func TestDiffEmptySnapshotCommitListFailsTowardEvidence(t *testing.T) {
	rec := Entry{HeadSHA: "old"} // commit list was unavailable at hold time
	d := Diff(rec, "new", []Commit{{SHA: "x", Author: "bob"}})
	if len(d.NewCommits) != 1 || len(d.NewAuthors) != 1 {
		t.Fatalf("Diff = %+v, want every current commit treated as new", d)
	}
}

func TestCommentBodyNamesEvidenceWithoutMentions(t *testing.T) {
	d := Drift{
		Moved:           true,
		RecordedHeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CurrentHeadSHA:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		NewCommits: []Commit{
			{SHA: "f1f1f1f1f1f1f1f1", Author: "mallory", Title: "sneak: saas.go"},
		},
		NewAuthors: []string{"mallory"},
	}
	body := CommentBody(d)
	for _, want := range []string{
		"fresh review required",
		"`aaaaaaaaaaaa`", // short recorded SHA
		"`bbbbbbbbbbbb`", // short current SHA
		"`f1f1f1f1f1f1`",
		"`mallory`",
		"sneak: saas.go",
		"`" + ReHoldLabel + "`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}
	// The hard rule: evidence, never notification — no @-mentions, anywhere.
	if strings.Contains(body, "@") {
		t.Fatalf("comment must not contain @-mentions:\n%s", body)
	}
}

func TestCommentBodyCapsListedCommitsAndHandlesNoCommitList(t *testing.T) {
	var commits []Commit
	for i := 0; i < maxListedCommits+4; i++ {
		commits = append(commits, Commit{SHA: strings.Repeat("e", 8), Author: "bot", Title: "chore"})
	}
	body := CommentBody(Drift{Moved: true, RecordedHeadSHA: "a", CurrentHeadSHA: "b", NewCommits: commits})
	if !strings.Contains(body, "and 4 more") {
		t.Fatalf("comment must summarize commits past the cap:\n%s", body)
	}
	if got := strings.Count(body, "- `"); got != maxListedCommits {
		t.Fatalf("listed bullets = %d, want %d capped lines", got, maxListedCommits)
	}

	empty := CommentBody(Drift{Moved: true, RecordedHeadSHA: "a", CurrentHeadSHA: "b"})
	if !strings.Contains(empty, "treat the entire branch as unreviewed") {
		t.Fatalf("comment without a commit list must fail toward full re-review:\n%s", empty)
	}
	if strings.Contains(empty, "@") {
		t.Fatal("comment must not contain @-mentions")
	}
}

func TestEmptyPathStoreDoesNotWrite(t *testing.T) {
	s := &Store{entries: map[string]*Entry{}}
	s.Snapshot("r", 1, "h", nil) // must not panic on save with no path
	if _, ok := s.Recorded("r", 1); !ok {
		t.Fatal("path-less store must still track entries in memory")
	}
}
