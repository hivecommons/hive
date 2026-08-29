package dashboard

import "testing"

// A fresh server has never posted a capped digest: both counts are zero, so a
// hive that never caps reports 0 overflow, exactly as the contract promises.
func TestAdvisoryCounts_ZeroBeforeAnyPost(t *testing.T) {
	s := newAdvisoryTestServer()
	findings, overflow := s.AdvisoryCounts()
	if findings != 0 || overflow != 0 {
		t.Fatalf("expected (0,0) before any post, got (%d,%d)", findings, overflow)
	}
}

// RecordAdvisoryOverflow stores the withheld count alongside the finding count
// recorded by RecordAdvisoryPost, and AdvisoryCounts returns both.
func TestRecordAdvisoryOverflow_ReportedAlongsideFindings(t *testing.T) {
	s := newAdvisoryTestServer()
	s.RecordAdvisoryPost(5)
	s.RecordAdvisoryOverflow(3)

	findings, overflow := s.AdvisoryCounts()
	if findings != 5 {
		t.Fatalf("expected 5 findings, got %d", findings)
	}
	if overflow != 3 {
		t.Fatalf("expected 3 overflow, got %d", overflow)
	}
}

// RecordAdvisoryOverflow is separate from RecordAdvisoryPost: overwriting the
// overflow must not disturb the recorded finding count, and an uncapped post
// can reset overflow back to 0.
func TestRecordAdvisoryOverflow_IndependentOfPostCount(t *testing.T) {
	s := newAdvisoryTestServer()
	s.RecordAdvisoryPost(9)
	s.RecordAdvisoryOverflow(4)
	s.RecordAdvisoryOverflow(0) // next digest was not capped

	findings, overflow := s.AdvisoryCounts()
	if findings != 9 {
		t.Fatalf("overflow update must not change findings: got %d, want 9", findings)
	}
	if overflow != 0 {
		t.Fatalf("expected overflow reset to 0, got %d", overflow)
	}
}
