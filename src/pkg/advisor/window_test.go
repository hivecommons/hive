package advisor

import (
	"math"
	"testing"
	"time"
)

func mustTime(t *testing.T, raw string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return ts
}

func TestStoreListWindow(t *testing.T) {
	s := NewStore("")
	s.Append(Record{Agent: "a", Timestamp: "2026-01-01T00:00:00Z", Text: "jan"})
	s.Append(Record{Agent: "a", Timestamp: "2026-03-01T00:00:00Z", Text: "mar"})
	s.Append(Record{Agent: "b", Timestamp: "2026-03-02T00:00:00Z", Text: "mar-b"})
	s.Append(Record{Agent: "a", Timestamp: "2026-06-01T00:00:00Z", Text: "jun"})
	s.Append(Record{Agent: "a", Timestamp: "not-a-time"})

	since := mustTime(t, "2026-02-01T00:00:00Z")
	until := mustTime(t, "2026-04-01T00:00:00Z")

	got := s.ListWindow("a", since, until, 0)
	if len(got) != 1 || got[0].Text != "mar" {
		t.Fatalf("agent+window: %+v", got)
	}
	got = s.ListWindow("", since, until, 0)
	if len(got) != 2 || got[0].Text != "mar-b" {
		t.Fatalf("fleet window must be newest first: %+v", got)
	}
	got = s.ListWindow("a", time.Time{}, until, 0)
	if len(got) != 2 {
		t.Fatalf("open since: want 2, got %+v", got)
	}
	if got := s.ListWindow("", time.Time{}, time.Time{}, 0); len(got) != 5 {
		t.Fatalf("open window keeps every record, including unparsable stamps: %d", len(got))
	}
	if got := s.ListWindow("", since, time.Time{}, 1); len(got) != 1 || got[0].Text != "jun" {
		t.Fatalf("limit must keep the newest: %+v", got)
	}
}

func TestStoreSpendByAgent(t *testing.T) {
	s := NewStore("")
	s.Append(Record{Agent: "b", Timestamp: "2026-03-01T00:00:00Z", InputTokens: 100, OutputTokens: 10, CostUSD: 0.25})
	s.Append(Record{Agent: "a", Timestamp: "2026-03-01T01:00:00Z", InputTokens: 50, OutputTokens: 5, CostUSD: 0.10})
	s.Append(Record{Agent: "a", Timestamp: "2026-03-01T02:00:00Z", InputTokens: 70, OutputTokens: 7, CostUSD: 0.15})
	s.Append(Record{Agent: "a", Timestamp: "2026-03-01T03:00:00Z", Skipped: SkipBudgetExhausted})
	s.Append(Record{Agent: "a", Timestamp: "2026-01-01T00:00:00Z", CostUSD: 9})

	since := mustTime(t, "2026-02-01T00:00:00Z")
	got := s.SpendByAgent(since, time.Time{})
	if len(got) != 2 || got[0].Agent != "a" || got[1].Agent != "b" {
		t.Fatalf("want agents sorted a,b: %+v", got)
	}
	a := got[0]
	if a.Reviews != 3 || a.Skipped != 1 || a.InputTokens != 120 || a.OutputTokens != 12 {
		t.Errorf("agent a aggregate: %+v", a)
	}
	if math.Abs(a.CostUSD-0.25) > 1e-9 {
		t.Errorf("agent a cost = %v, want the sum of its in-window record costs (0.25)", a.CostUSD)
	}

	// The spend must equal the sum over the listing for the same window.
	var sum float64
	for _, rec := range s.ListWindow("a", since, time.Time{}, 0) {
		sum += rec.CostUSD
	}
	if math.Abs(sum-a.CostUSD) > 1e-9 {
		t.Errorf("spend %v disagrees with listing sum %v", a.CostUSD, sum)
	}

	all := s.SpendByAgent(time.Time{}, time.Time{})
	if math.Abs(all[0].CostUSD-9.25) > 1e-9 {
		t.Errorf("open window must include every retained record: %+v", all[0])
	}
	if got := NewStore("").SpendByAgent(time.Time{}, time.Time{}); got == nil || len(got) != 0 {
		t.Errorf("empty store must return an empty, non-nil slice: %#v", got)
	}
}
