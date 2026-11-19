package reviewfeedback

import (
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
