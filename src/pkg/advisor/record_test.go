package advisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreAppendAndList(t *testing.T) {
	s := NewStore("") // memory-only
	s.Append(Record{Agent: "a", Severity: SeverityAside})
	s.Append(Record{Agent: "b", Severity: SeverityConcern, Text: "hm"})
	s.Append(Record{Agent: "a", Severity: SeverityBlocker, Text: "stop"})

	all := s.List("", time.Time{}, 0)
	if len(all) != 3 {
		t.Fatalf("want 3 records, got %d", len(all))
	}
	// Newest first.
	if all[0].Severity != SeverityBlocker || all[2].Severity != SeverityAside {
		t.Errorf("not newest-first: %+v", all)
	}
	if all[0].Timestamp == "" {
		t.Error("append must stamp a timestamp")
	}

	forA := s.List("a", time.Time{}, 0)
	if len(forA) != 2 {
		t.Fatalf("agent filter: want 2, got %d", len(forA))
	}
	limited := s.List("", time.Time{}, 1)
	if len(limited) != 1 || limited[0].Severity != SeverityBlocker {
		t.Errorf("limit must keep the newest: %+v", limited)
	}
}

func TestStoreSinceFilter(t *testing.T) {
	s := NewStore("")
	s.Append(Record{Agent: "a", Timestamp: "2026-01-01T00:00:00Z"})
	s.Append(Record{Agent: "a", Timestamp: "2026-06-01T00:00:00Z"})
	s.Append(Record{Agent: "a", Timestamp: "not-a-time"})

	since, _ := time.Parse(time.RFC3339, "2026-03-01T00:00:00Z")
	got := s.List("a", since, 0)
	if len(got) != 1 || got[0].Timestamp != "2026-06-01T00:00:00Z" {
		t.Fatalf("since filter: %+v", got)
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "advisor-records.jsonl")

	s := NewStore(path)
	s.Append(Record{Agent: "a", Severity: SeverityConcern, Text: "careful"})
	s.Append(Record{Agent: "b", Skipped: SkipTimeout})

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("records file not written: %v", err)
	}

	reopened := NewStore(path)
	got := reopened.List("", time.Time{}, 0)
	if len(got) != 2 {
		t.Fatalf("reopen: want 2 records, got %d", len(got))
	}
	if got[1].Agent != "a" || got[1].Text != "careful" {
		t.Errorf("reopen lost fields: %+v", got[1])
	}
	if got[0].Skipped != SkipTimeout {
		t.Errorf("skip reason lost: %+v", got[0])
	}
}

func TestStoreMissingDirIsMemoryOnly(t *testing.T) {
	s := NewStore("/nonexistent-hive-dir/advisor.jsonl")
	s.Append(Record{Agent: "a"})
	if got := s.List("a", time.Time{}, 0); len(got) != 1 {
		t.Fatalf("memory-only store must still record: %d", len(got))
	}
}

func TestStoreRingCap(t *testing.T) {
	s := NewStore("")
	for i := 0; i < recordRingCap+10; i++ {
		s.Append(Record{Agent: "a"})
	}
	if got := len(s.List("", time.Time{}, 0)); got != recordRingCap {
		t.Fatalf("ring must cap at %d, got %d", recordRingCap, got)
	}
}
