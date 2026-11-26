package reviewfeedback

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func strandedItems(ids ...string) []github.FeedbackItem {
	out := make([]github.FeedbackItem, 0, len(ids))
	for _, id := range ids {
		out = append(out, github.FeedbackItem{ID: id})
	}
	return out
}

// A kick recorded before the check existed (no head, no routed ids) is still
// recovered: its routed ids are the handled ids stamped with the kick time,
// the current head becomes the baseline and the backoff restarts.
func TestCheckStranded_LegacyRecordRecovered(t *testing.T) {
	s, _ := Open(t.TempDir())
	kick := time.Date(2026, 11, 18, 20, 38, 0, 0, time.UTC)
	items := strandedItems("inline:1", "inline:2")
	if err := s.RecordKick("o/r#1", items, kick); err != nil {
		t.Fatal(err)
	}
	s.PRs["o/r#1"].LastKickIDs = nil // as written by an older hive
	p := RetryPolicy{MaxRetries: 2, Backoff: time.Hour}
	now := kick.Add(20 * time.Hour)
	if c, _ := s.CheckStranded("o/r#1", "sha", time.Time{}, items, now, p); c.Verdict != StrandedWaiting {
		t.Fatalf("legacy first check = %+v, want waiting (baseline)", c)
	}
	c, _ := s.CheckStranded("o/r#1", "sha", time.Time{}, items, now.Add(61*time.Minute), p)
	if c.Verdict != StrandedRetry || len(c.IDs) != 2 {
		t.Fatalf("legacy retry = %+v", c)
	}
	if d := s.Plan("o/r#1", items, now.Add(62*time.Minute), Limits{}); d.Verdict != VerdictKick || len(d.Items) != 2 {
		t.Fatalf("retry not re-planned: %+v", d)
	}
}

// Only the routed comments still unanswered are re-routed; once none is,
// the kick is closed.
func TestCheckStranded_OnlyStillUnanswered(t *testing.T) {
	s, _ := Open(t.TempDir())
	t0 := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	_ = s.RecordKickHead("o/r#1", strandedItems("inline:1", "inline:2"), "sha", t0)
	p := RetryPolicy{MaxRetries: 2, Backoff: time.Hour}
	c, _ := s.CheckStranded("o/r#1", "sha", time.Time{}, strandedItems("inline:2", "inline:9"), t0.Add(2*time.Hour), p)
	if c.Verdict != StrandedRetry || len(c.IDs) != 1 || c.IDs[0] != "inline:2" {
		t.Fatalf("check = %+v, want retry of inline:2 only", c)
	}
	s2, _ := Open(t.TempDir())
	_ = s2.RecordKickHead("o/r#1", strandedItems("inline:1"), "sha", t0)
	if c, _ := s2.CheckStranded("o/r#1", "sha", time.Time{}, nil, t0.Add(2*time.Hour), p); c.Verdict != StrandedNone {
		t.Fatalf("answered kick = %+v", c)
	}
	if !s2.PRs["o/r#1"].StrandedClosed {
		t.Fatal("answered kick not closed")
	}
}

// A retry kick counts a retry; a kick of new feedback starts over.
func TestRecordKickHead_RetryCounting(t *testing.T) {
	s, _ := Open(t.TempDir())
	t0 := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	items := strandedItems("inline:1")
	_ = s.RecordKickHead("o/r#1", items, "sha", t0)
	p := RetryPolicy{MaxRetries: 2, Backoff: time.Hour}
	_, _ = s.CheckStranded("o/r#1", "sha", time.Time{}, items, t0.Add(2*time.Hour), p)
	_ = s.RecordKickHead("o/r#1", items, "sha", t0.Add(2*time.Hour))
	if got := s.PRs["o/r#1"].Retries; got != 1 {
		t.Fatalf("retries = %d, want 1", got)
	}
	if check, err := s.CheckStranded("o/r#1", "sha", time.Time{}, items, t0.Add(3*time.Hour), p); err != nil || check.Verdict != StrandedWaiting {
		t.Fatalf("check during doubled backoff = %+v, %v", check, err)
	}
	_ = s.RecordKickHead("o/r#1", strandedItems("inline:5"), "sha2", t0.Add(3*time.Hour))
	if got := s.PRs["o/r#1"].Retries; got != 0 {
		t.Fatalf("retries after a fresh kick = %d, want 0", got)
	}
}

// No head reported: the check cannot judge and leaves the record alone.
func TestCheckStranded_NoHeadWaits(t *testing.T) {
	s, _ := Open(t.TempDir())
	t0 := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	items := strandedItems("inline:1")
	_ = s.RecordKickHead("o/r#1", items, "sha", t0)
	if c, _ := s.CheckStranded("o/r#1", "", time.Time{}, items, t0.Add(9*time.Hour), RetryPolicy{MaxRetries: 2, Backoff: time.Hour}); c.Verdict != StrandedWaiting {
		t.Fatalf("check = %+v", c)
	}
}

func TestStrandedHandoff_PersistsWhileFreshFeedbackRoutes(t *testing.T) {
	const key = "o/r#1"
	t0 := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store := openStrandedTestStore(t, dir)
	policy := RetryPolicy{Backoff: time.Hour}
	items := strandedItems("inline:1", "inline:2")
	items[0].URL = "https://example.invalid/review/1"
	items[1].URL = "https://example.invalid/review/2"
	if err := store.RecordKickHead(key, items[:1], "head", t0); err != nil {
		t.Fatal(err)
	}
	exhaustedAt := t0.Add(2 * time.Hour)
	if check, err := store.CheckStranded(key, "head", time.Time{}, items, exhaustedAt, policy); err != nil || check.Verdict != StrandedExhausted {
		t.Fatalf("exhausted kick = %+v, %v", check, err)
	}
	fb := github.PRFeedback{HeadSHA: "head", Items: items}
	first, obsolete := readStrandedTestHandoff(t, store, key, fb, exhaustedAt, policy)
	if obsolete || !first.Ready || !strings.Contains(first.Body, items[0].URL) || strings.Contains(first.Body, items[1].URL) {
		t.Fatalf("first handoff = %+v, obsolete=%t", first, obsolete)
	}

	freshAt := t0.Add(3 * time.Hour)
	fresh := store.Plan(key, items, freshAt, Limits{})
	if fresh.Verdict != VerdictKick || len(fresh.Items) != 1 || fresh.Items[0].ID != items[1].ID {
		t.Fatalf("fresh routing plan = %+v", fresh)
	}
	if err := store.RecordKickHead(key, fresh.Items, "head", freshAt); err != nil {
		t.Fatal(err)
	}
	store = openStrandedTestStore(t, dir)
	later := t0.Add(5 * time.Hour)
	if check, err := store.CheckStranded(key, "head", time.Time{}, items, later, policy); err != nil || check.Verdict != StrandedExhausted {
		t.Fatalf("second exhausted kick = %+v, %v", check, err)
	}

	// A bounded read omitted the original item. It must not retire its note.
	fb.Items, fb.RuleCandidates = items[1:], items[1:]
	stillFirst, obsolete := readStrandedTestHandoff(t, store, key, fb, later, policy)
	if obsolete || stillFirst.Body != first.Body || !stillFirst.Ready {
		t.Fatalf("handoff after restart and partial read = %+v, obsolete=%t", stillFirst, obsolete)
	}

	// Once the original item is visible but answered, its note can be removed.
	fb.RuleCandidates = items
	resolved, obsolete := readStrandedTestHandoff(t, store, key, fb, later, policy)
	if !obsolete || resolved.Body != first.Body {
		t.Fatalf("resolved handoff = %+v, obsolete=%t", resolved, obsolete)
	}
	if err := store.DiscardStrandedHandoff(key, later); err != nil {
		t.Fatal(err)
	}
	second, obsolete := readStrandedTestHandoff(t, store, key, fb, later, policy)
	if obsolete || !second.Ready || !strings.Contains(second.Body, items[1].URL) || strings.Contains(second.Body, items[0].URL) {
		t.Fatalf("next handoff = %+v, obsolete=%t", second, obsolete)
	}
	if marked, err := store.MarkNeedsHuman(key, later); err != nil || !marked {
		t.Fatalf("mark handoff posted = %t, %v", marked, err)
	}
	store = openStrandedTestStore(t, dir)
	if note, _, err := store.NextStrandedHandoff(key, fb, later, policy); err != nil || note != nil || !store.NeedsHumanNoted(key) {
		t.Fatalf("posted handoff after restart = %+v, %v", note, err)
	}
}

func TestStrandedHandoff_DefersRefusedRetryAndWaitsForPersistence(t *testing.T) {
	const key = "o/r#1"
	t0 := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store := openStrandedTestStore(t, dir)
	items := strandedItems("inline:1", "inline:2")
	if err := store.RecordKickHead(key, items[:1], "head", t0); err != nil {
		t.Fatal(err)
	}
	policy := RetryPolicy{MaxRetries: 1, Backoff: time.Hour}
	if check, err := store.CheckStranded(key, "head", time.Time{}, items, t0.Add(2*time.Hour), policy); err != nil || check.Verdict != StrandedRetry {
		t.Fatalf("initial retry = %+v, %v", check, err)
	}
	store = openStrandedTestStore(t, dir) // the retry was refused, leaving an empty Handled map
	policy = RetryPolicy{Backoff: 5 * time.Hour}
	waitAt := t0.Add(3 * time.Hour)
	if check, err := store.CheckStranded(key, "", time.Time{}, items, waitAt, policy); err != nil || check.Verdict != StrandedExhausted {
		t.Fatalf("capped retry = %+v, %v", check, err)
	}
	fb := github.PRFeedback{Items: items}
	deferred, obsolete := readStrandedTestHandoff(t, store, key, fb, waitAt, policy)
	if obsolete || deferred.Ready {
		t.Fatalf("headless handoff = %+v, obsolete=%t", deferred, obsolete)
	}
	if fresh := store.Plan(key, items, waitAt, Limits{}); fresh.Verdict != VerdictKick || len(fresh.Items) != 1 || fresh.Items[0].ID != items[1].ID {
		t.Fatalf("routing plan while waiting = %+v", fresh)
	}
	fb.HeadSHA = "head"
	deferred, obsolete = readStrandedTestHandoff(t, store, key, fb, t0.Add(4*time.Hour), policy)
	if obsolete || deferred.Ready {
		t.Fatalf("handoff before longer backoff = %+v, obsolete=%t", deferred, obsolete)
	}

	postAt := t0.Add(6 * time.Hour)
	blocker := filepath.Join(dir, stateFile+".tmp")
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	if note, _, err := store.NextStrandedHandoff(key, fb, postAt, policy); err == nil || note != nil {
		t.Fatalf("handoff before persistence = %+v, %v", note, err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	ready, obsolete := readStrandedTestHandoff(t, store, key, fb, postAt, policy)
	if obsolete || !ready.Ready || ready.Body != deferred.Body {
		t.Fatalf("handoff after persistence recovered = %+v, obsolete=%t", ready, obsolete)
	}
	store = openStrandedTestStore(t, dir)
	fb.HeadSHA = "new-head"
	if _, obsolete := readStrandedTestHandoff(t, store, key, fb, postAt.Add(time.Hour), policy); !obsolete {
		t.Fatal("a push from the latest kick should retire its handoff")
	}
}

func openStrandedTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func readStrandedTestHandoff(t *testing.T, store *Store, key string, fb github.PRFeedback, now time.Time, policy RetryPolicy) (*StrandedHandoff, bool) {
	t.Helper()
	note, obsolete, err := store.NextStrandedHandoff(key, fb, now, policy)
	if err != nil || note == nil {
		t.Fatalf("expected a pending handoff, got %+v, %v", note, err)
	}
	return note, obsolete
}

func TestRetryPolicy_Normalized(t *testing.T) {
	p := RetryPolicy{MaxRetries: -3, Backoff: time.Second}.Normalized()
	if p.MaxRetries != 0 || p.Backoff != MinRetryBackoff {
		t.Fatalf("normalized = %+v", p)
	}
	if p := (RetryPolicy{MaxRetries: 99}).Normalized(); p.MaxRetries != MaxRetryMax || p.Backoff != DefaultRetryBackoff {
		t.Fatalf("normalized = %+v", p)
	}
	t.Setenv(RetryMaxEnvVar, "4")
	t.Setenv(RetryBackoffEnvVar, "30m")
	if p := RetryPolicyFromEnv(); p.MaxRetries != 4 || p.Backoff != 30*time.Minute {
		t.Fatalf("from env = %+v", p)
	}
}
