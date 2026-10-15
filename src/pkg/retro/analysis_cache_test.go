package retro

import (
	"context"
	"fmt"
	"testing"
)

const validAnalysisReply = `{"root_cause_hypothesis":"late CI","process_improvement":"run checks earlier","generalizable":false,"generalizable_lesson":""}`

// TestAnalyzeSkipsModelOnlyOnEmptyFindingSet: an empty deterministic finding
// set is the ONE condition that bypasses the model; a non-empty set always
// reaches it on first sight (hivecommons/hive#12131).
func TestAnalyzeSkipsModelOnlyOnEmptyFindingSet(t *testing.T) {
	client := &fakeAnalysisClient{replies: []string{validAnalysisReply}}
	analyzer := newAnalyzerWithClient("m", client)

	got, err := analyzer.Analyze(context.Background(), RetroRecord{BeadID: "b"}, nil)
	if err != nil || got != (Analysis{}) {
		t.Fatalf("empty findings: got %#v, %v; want zero analysis, nil", got, err)
	}
	if client.calls != 0 {
		t.Fatalf("empty findings: model called %d times, want 0", client.calls)
	}
	if st := analyzer.CacheStats(); st.SkippedEmpty != 1 {
		t.Fatalf("SkippedEmpty = %d, want 1", st.SkippedEmpty)
	}

	// A single low-severity finding that "looks fine" still goes to the model.
	findings := []Finding{{Pattern: "slow-ci", Severity: "low", Detail: "1 ci failure"}}
	got, err = analyzer.Analyze(context.Background(), RetroRecord{BeadID: "b"}, findings)
	if err != nil {
		t.Fatalf("non-empty findings: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("non-empty findings: model called %d times, want 1", client.calls)
	}
	if got.RootCauseHypothesis != "late CI" {
		t.Fatalf("analysis = %#v", got)
	}
}

// TestAnalyzeCachesByFindingSetHash: identical finding sets (in any order)
// are served from the cache; a different set is a miss.
func TestAnalyzeCachesByFindingSetHash(t *testing.T) {
	client := &fakeAnalysisClient{replies: []string{validAnalysisReply}}
	analyzer := newAnalyzerWithClient("m", client)
	a := Finding{Pattern: "kick-storm", Severity: "medium", Detail: "3 kicks"}
	b := Finding{Pattern: "ci-churn", Severity: "high", Detail: "2 failures"}

	if _, err := analyzer.Analyze(context.Background(), RetroRecord{BeadID: "b1"}, []Finding{a, b}); err != nil {
		t.Fatal(err)
	}
	if _, err := analyzer.Analyze(context.Background(), RetroRecord{BeadID: "b2"}, []Finding{b, a}); err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 {
		t.Fatalf("model called %d times for identical finding sets, want 1", client.calls)
	}
	st := analyzer.CacheStats()
	if st.Hits != 1 || st.Misses != 1 || st.Size != 1 {
		t.Fatalf("stats = %+v, want hits=1 misses=1 size=1", st)
	}

	c := Finding{Pattern: "ci-churn", Severity: "high", Detail: "5 failures"}
	if _, err := analyzer.Analyze(context.Background(), RetroRecord{BeadID: "b3"}, []Finding{a, c}); err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Fatalf("model called %d times after a changed detail, want 2", client.calls)
	}
}

// TestAnalyzeDoesNotCacheErrors: a failed model call leaves no entry, so the
// next identical request retries the model.
func TestAnalyzeDoesNotCacheErrors(t *testing.T) {
	client := &fakeAnalysisClient{err: &testErr{"boom"}}
	analyzer := newAnalyzerWithClient("m", client)
	findings := []Finding{{Pattern: "p", Severity: "low"}}
	for i := 0; i < 2; i++ {
		if _, err := analyzer.Analyze(context.Background(), RetroRecord{}, findings); err == nil {
			t.Fatal("expected error")
		}
	}
	if client.calls != 2 {
		t.Fatalf("calls = %d, want 2 (errors are not cached)", client.calls)
	}
	if st := analyzer.CacheStats(); st.Size != 0 {
		t.Fatalf("size = %d, want 0", st.Size)
	}
}

func TestFindingSetHashStableAndModelScoped(t *testing.T) {
	a := Finding{Pattern: "x", Severity: "low", Detail: "d"}
	b := Finding{Pattern: "y", Severity: "high", Detail: "e"}
	if FindingSetHash("m", []Finding{a, b}) != FindingSetHash("m", []Finding{b, a}) {
		t.Fatal("hash must be order-independent")
	}
	if FindingSetHash("m", []Finding{a}) == FindingSetHash("other", []Finding{a}) {
		t.Fatal("hash must differ across models")
	}
	if FindingSetHash("m", nil) == FindingSetHash("m", []Finding{a}) {
		t.Fatal("empty and non-empty sets must differ")
	}
}

func TestAnalysisCacheEvictsLRU(t *testing.T) {
	c := newAnalysisCache(2)
	for i := 0; i < 3; i++ {
		c.put(fmt.Sprint("k", i), Analysis{RootCauseHypothesis: fmt.Sprint(i)})
	}
	if _, ok := c.get("k0"); ok {
		t.Fatal("k0 should have been evicted")
	}
	if _, ok := c.get("k2"); !ok {
		t.Fatal("k2 should be present")
	}
	if st := c.snapshot(); st.Size != 2 {
		t.Fatalf("size = %d, want 2", st.Size)
	}
}
