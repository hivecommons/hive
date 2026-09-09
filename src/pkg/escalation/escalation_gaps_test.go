package escalation

// Tests for the ledger branches the existing suite left uncovered — all of
// them loop-safety or ledger-loss behavior where a silent regression would
// either page humans repeatedly or nudge a dead PR forever:
//
//   - TryReEngage's machinery amnesty (escalation.go:522-528): a ledger
//     entry written by an older MachineryVersion must get a fresh
//     re-engagement budget and be pulled back OUT of the needs-human state.
//   - TryReEngage on a PR with no ledger entry (507-509): first contact
//     must create the entry and allow the engagement.
//   - Sweep's labeled-PR handling for an entry the ledger lost (208, 241):
//     the forge label alone must keep the PR escalated (never re-comment)
//     and keep accumulating evidence.
//   - appendSHA's maxTrackedSHAs bound (354): the per-PR attempt history
//     must not grow without limit.
//   - ObserveRed's pending / empty-SHA / excerpt handling (441, 455, 468)
//     and StaleRed's zero-clock guard (489).
//   - The in-memory store (saveLocked's path=="" early return, 551) and
//     ReEngagements on an untracked PR (546).
//   - plural's n!=1 branch via CommentBody's exhausted wording (607).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTryReEngage_CreatesEntryOnFirstContact: the re-engagement paths can see
// a red PR before Sweep/ObserveRed ever recorded it (a fresh ledger after a
// /data wipe). First contact must create the entry and allow the dispatch.
func TestTryReEngage_CreatesEntryOnFirstContact(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	if !s.TryReEngage("org/repo", 7, "sha1") {
		t.Fatal("first re-engagement on an untracked PR must be allowed")
	}
	if got := s.ReEngagements("org/repo", 7); got != 1 {
		t.Fatalf("ReEngagements = %d, want 1", got)
	}
}

// TestTryReEngage_MachineryAmnestyGrantsFreshBudget: an entry recorded under
// an older fix-dispatch generation carries exhausted attempts and an engaged
// needs-human state. TryReEngage must reset ALL of it — budget, escalation,
// label bookkeeping, distinct-SHA history — so the current machinery gets its
// own chance before a human is paged again.
func TestTryReEngage_MachineryAmnestyGrantsFreshBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "streaks.json")

	// Seed a ledger written by generation MachineryVersion-1: cap exhausted,
	// escalated, label confirmed.
	old := map[string]*Entry{
		Key("org/repo", 7): {
			RedSHAs:        []string{"a", "b", "c"},
			Escalated:      true,
			LabelApplied:   true,
			LabelAppliedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
			CurRedSHA:      "sha-stuck",
			ReEngagements:  MaxReEngagements,
			Machinery:      MachineryVersion - 1,
		},
	}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	s := Load(path)
	// Same head SHA as tracked, so the SHA-sync branch does NOT reset the
	// counter — only the amnesty can.
	if !s.TryReEngage("org/repo", 7, "sha-stuck") {
		t.Fatal("amnesty must grant a fresh budget: TryReEngage returned false")
	}
	if got := s.ReEngagements("org/repo", 7); got != 1 {
		t.Fatalf("ReEngagements after amnesty = %d, want 1 (fresh set)", got)
	}
	if got := s.Attempts("org/repo", 7); got != 0 {
		t.Fatalf("distinct-SHA history must be cleared by amnesty, got %d", got)
	}

	// The un-escalation must be durable: a reload sees the amnestied state.
	s2 := Load(path)
	r := s2.Sweep([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "sha-stuck", Red: true}}, 3)
	got := r[Key("org/repo", 7)]
	if got.Escalated {
		t.Fatalf("amnesty must clear Escalated so the PR is back in the automated lane: %+v", got)
	}
}

// TestTryReEngage_AmnestyDoesNotFireForCurrentGeneration: the negative
// control — a current-generation entry at the cap stays capped.
func TestTryReEngage_AmnestyDoesNotFireForCurrentGeneration(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	for i := 0; i < MaxReEngagements; i++ {
		if !s.TryReEngage("org/repo", 7, "sha1") {
			t.Fatalf("engagement %d within the cap must be allowed", i+1)
		}
	}
	if s.TryReEngage("org/repo", 7, "sha1") {
		t.Fatal("engagement past MaxReEngagements on an unchanged SHA must be refused")
	}
}

// TestSweep_LabelAlonePinsEscalationAfterLedgerLoss: the forge label is the
// durable record; the ledger is a cache. A labeled PR with NO ledger entry (a
// wiped /data, a pruned entry) must be treated as already escalated — never
// NewlyEscala, so the escalation comment is never posted twice — while red
// SHAs and excerpts keep accumulating as evidence.
func TestSweep_LabelAlonePinsEscalationAfterLedgerLoss(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	o := Observation{
		Repo: "org/repo", Number: 7, HeadSHA: "sha1", Red: true,
		Labeled: true, Excerpt: "TestFoo failed: want 1, got 2",
	}
	r := s.Sweep([]Observation{o}, 3)
	got := r[Key("org/repo", 7)]
	if !got.Escalated || got.NewlyEscala {
		t.Fatalf("labeled PR must be escalated without re-firing: %+v", got)
	}
	if got.Attempts != 1 {
		t.Fatalf("red SHA must still be counted as evidence, got %d attempts", got.Attempts)
	}
	if ex := s.Excerpt("org/repo", 7); ex != "TestFoo failed: want 1, got 2" {
		t.Fatalf("excerpt must be recorded on the labeled path, got %q", ex)
	}

	// A second red SHA under the label: evidence accumulates, still no re-fire.
	o.HeadSHA = "sha2"
	o.Excerpt = "TestBar failed"
	r = s.Sweep([]Observation{o}, 3)
	got = r[Key("org/repo", 7)]
	if got.Attempts != 2 || got.NewlyEscala {
		t.Fatalf("labeled PR evidence must accumulate without re-firing: %+v", got)
	}
}

// TestSweep_ZeroThresholdFallsBackToDefault: callers passing an unset (0)
// threshold must get DefaultThreshold, not escalate-on-first-red.
func TestSweep_ZeroThresholdFallsBackToDefault(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	for i := 1; i < DefaultThreshold; i++ {
		r := s.Sweep([]Observation{obs("org/repo", 7, fmt.Sprintf("sha%d", i), true)}, 0)
		if got := r[Key("org/repo", 7)]; got.NewlyEscala {
			t.Fatalf("attempt %d must not escalate under the default threshold: %+v", i, got)
		}
	}
	r := s.Sweep([]Observation{obs("org/repo", 7, fmt.Sprintf("sha%d", DefaultThreshold), true)}, 0)
	if got := r[Key("org/repo", 7)]; !got.NewlyEscala {
		t.Fatalf("attempt %d must escalate at the default threshold: %+v", DefaultThreshold, got)
	}
}

// TestSweep_AttemptHistoryIsBounded: maxTrackedSHAs caps the per-PR history so
// the ledger cannot grow without bound on a PR that keeps pushing red commits.
func TestSweep_AttemptHistoryIsBounded(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	for i := 0; i < maxTrackedSHAs+5; i++ {
		s.Sweep([]Observation{obs("org/repo", 7, fmt.Sprintf("sha%03d", i), true)}, 1_000_000)
	}
	if got := s.Attempts("org/repo", 7); got != maxTrackedSHAs {
		t.Fatalf("attempt history must be capped at %d, got %d", maxTrackedSHAs, got)
	}
}

// TestObserveRed_PendingLeavesTheClockAlone: an inconclusive pass (checks
// running, or the check fetch failed) must not touch the staleness clock —
// clearing it would make a stuck PR look freshly red after one API error.
func TestObserveRed_PendingLeavesTheClockAlone(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return start })

	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "sha1", Red: true}})

	// Time passes the staleness horizon; the next pass is inconclusive.
	s.SetClock(func() time.Time { return start.Add(RedPRStaleAfter + time.Minute) })
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 7, Pending: true}})

	if !s.StaleRed("org/repo", 7, "sha1") {
		t.Fatal("a pending pass must not reset the staleness clock")
	}
}

// TestObserveRed_RedWithoutHeadSHAIsIgnored: a red observation whose head SHA
// could not be resolved carries no clock identity and must be skipped, not
// recorded as an empty-SHA entry.
func TestObserveRed_RedWithoutHeadSHAIsIgnored(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "", Red: true}})

	if s.StaleRed("org/repo", 7, "") {
		t.Fatal("an empty head SHA must never read as stale")
	}
	if got := s.ReEngagements("org/repo", 7); got != 0 {
		t.Fatalf("no entry should have been created, ReEngagements = %d", got)
	}
}

// TestObserveRed_RecordsExcerptForEscalationEvidence: the excerpt observed on
// the red path must be stored so the eventual escalation comment carries the
// raw CI evidence even if the final pass fails to fetch annotations.
func TestObserveRed_RecordsExcerptForEscalationEvidence(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	s.ObserveRed([]Observation{{
		Repo: "org/repo", Number: 7, HeadSHA: "sha1", Red: true,
		Excerpt: "panic: runtime error",
	}})
	if got := s.Excerpt("org/repo", 7); got != "panic: runtime error" {
		t.Fatalf("ObserveRed must record the excerpt, got %q", got)
	}
}

// TestStaleRed_ZeroFirstRedAtIsNotStale: a ledger entry that tracks a red SHA
// but never started its clock (a truncated or older-generation ledger file)
// must fail safe to "not stale" — matching the SHA is not enough.
func TestStaleRed_ZeroFirstRedAtIsNotStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "streaks.json")
	seed := map[string]*Entry{
		Key("org/repo", 7): {CurRedSHA: "sha1", Machinery: MachineryVersion},
	}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	s := Load(path)
	s.SetClock(func() time.Time {
		return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).Add(RedPRStaleAfter + time.Hour)
	})
	if s.StaleRed("org/repo", 7, "sha1") {
		t.Fatal("a zero FirstRedAt must never read as stale, even on a matching red SHA")
	}
}

// TestInMemoryStoreNeverTouchesDisk: Load("") yields a usable in-memory store;
// every persisting operation must take saveLocked's empty-path early return
// rather than writing a ledger file into the working directory.
func TestInMemoryStoreNeverTouchesDisk(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	s := Load("")
	s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	s.ObserveRed([]Observation{{Repo: "org/repo", Number: 7, HeadSHA: "sha1", Red: true}})
	if !s.TryReEngage("org/repo", 7, "sha1") {
		t.Fatal("in-memory store must still track re-engagements")
	}
	if got := s.Attempts("org/repo", 7); got != 1 {
		t.Fatalf("in-memory store must still count attempts, got %d", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("in-memory store must not write files, found %v", entries)
	}
}

// TestReEngagements_UntrackedPRIsZero pins the no-entry read path.
func TestReEngagements_UntrackedPRIsZero(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))
	if got := s.ReEngagements("org/repo", 404); got != 0 {
		t.Fatalf("ReEngagements on an untracked PR = %d, want 0", got)
	}
}

// TestCommentBody_ExhaustedPluralizesDistinctHeads: the exhausted wording must
// pluralize "head" correctly for both one and several distinct red heads — a
// dangling "2 distinct red head seen" reads as truncation.
func TestCommentBody_ExhaustedPluralizesDistinctHeads(t *testing.T) {
	one := CommentBody(1, nil, "", true)
	if !strings.Contains(one, "1 distinct red head seen") || strings.Contains(one, "heads seen") {
		t.Fatalf("singular wording wrong:\n%s", one)
	}
	two := CommentBody(2, nil, "", true)
	if !strings.Contains(two, "2 distinct red heads seen") {
		t.Fatalf("plural wording wrong:\n%s", two)
	}
}
