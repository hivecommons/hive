package intent

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
)

func alignedReply(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"status\":\"aligned\",\"confidence\":0.9}"}}]}`)
}

func countingReviewer(t *testing.T) (*AlignmentReviewer, *int32) {
	t.Helper()
	var calls int32
	r, _ := newTestReviewer(t, func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&calls, 1)
		alignedReply(w, req)
	})
	return r, &calls
}

// TestReviewCachesByHeadSHAAndIntentRevision: the same (head SHA, intent
// revision) is served from the cache; a new head or an edited intent/PR text
// is a miss (hivecommons/hive#12131).
func TestReviewCachesByHeadSHAAndIntentRevision(t *testing.T) {
	r, calls := countingReviewer(t)
	ac := AlignmentContext{HeadSHA: "abc123", AuthorizedIntent: "fix the thing", PRTitle: "fix", PRBody: "Fixes #1", DiffSummary: "a.go +1 -1\n"}

	for i := 0; i < 3; i++ {
		v, err := r.Review(context.Background(), ac)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != AlignmentStatusAligned {
			t.Fatalf("verdict = %+v", v)
		}
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("model called %d times for identical (head, intent), want 1", got)
	}
	if st := r.CacheStats(); st.Hits != 2 || st.Misses != 1 || st.Size != 1 {
		t.Fatalf("stats = %+v", st)
	}

	newHead := ac
	newHead.HeadSHA = "def456"
	if _, err := r.Review(context.Background(), newHead); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("model called %d times after new head SHA, want 2", got)
	}

	editedIntent := ac
	editedIntent.AuthorizedIntent = "fix the thing and also the other thing"
	if _, err := r.Review(context.Background(), editedIntent); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("model called %d times after intent edit, want 3", got)
	}

	editedBody := ac
	editedBody.PRBody = "Fixes #2"
	if _, err := r.Review(context.Background(), editedBody); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(calls); got != 4 {
		t.Fatalf("model called %d times after PR body edit, want 4", got)
	}
}

// TestReviewWithoutHeadSHAKeysOnDiff: when the caller has no head SHA the
// diff summary stands in, so a changed file set is still a miss.
func TestReviewWithoutHeadSHAKeysOnDiff(t *testing.T) {
	r, calls := countingReviewer(t)
	ac := AlignmentContext{AuthorizedIntent: "intent", PRTitle: "t", DiffSummary: "a.go +1 -1\n"}
	for i := 0; i < 2; i++ {
		if _, err := r.Review(context.Background(), ac); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	ac.DiffSummary = "b.go +1 -1\n"
	if _, err := r.Review(context.Background(), ac); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("calls = %d after diff change, want 2", got)
	}
}

// TestReviewNeverShortCircuitsOnDeterministicVerdict is the negative case
// from hivecommons/hive#12131: a deterministic verdict — clean OR ambiguous
// — must never stand in for the model. Review has no knowledge of the
// deterministic result and calls the model for every unseen input.
func TestReviewNeverShortCircuitsOnDeterministicVerdict(t *testing.T) {
	r, calls := countingReviewer(t)

	// Deterministic checks find nothing: no scope tokens in the intent, so
	// scopeFinding is nil and the verdict is "aligned". The model must still
	// be asked — "deterministic looked OK" is not a reason to skip.
	clean := AlignmentContext{HeadSHA: "h1", AuthorizedIntent: "do it", PRTitle: "do it", Files: []DiffFileSummary{{Filename: "x/y.go"}}, DiffSummary: "x/y.go +1 -0\n"}
	if det := EvaluateAlignment(clean, Tier1, Config{}); det.Status != AlignmentStatusAligned || len(det.DeterministicFindings) != 0 {
		t.Fatalf("precondition: deterministic verdict = %+v, want aligned with no findings", det)
	}
	if _, err := r.Review(context.Background(), clean); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("clean deterministic verdict: model called %d times, want 1", got)
	}

	// Deterministic checks are ambiguous: the intent references pkg/a but the
	// diff touches pkg/b, which the heuristics flag as scope drift. Policy
	// still expects semantic judgment, so the model is called.
	ambiguous := AlignmentContext{HeadSHA: "h2", AuthorizedIntent: "refactor pkg/alpha/handler.go", PRTitle: "refactor", Files: []DiffFileSummary{{Filename: "pkg/beta/other.go"}}, DiffSummary: "pkg/beta/other.go +5 -5\n"}
	if det := EvaluateAlignment(ambiguous, Tier1, Config{}); len(det.DeterministicFindings) == 0 {
		t.Fatalf("precondition: expected a deterministic scope finding, got %+v", det)
	}
	if _, err := r.Review(context.Background(), ambiguous); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("ambiguous deterministic verdict: model called %d times total, want 2", got)
	}
}

// TestReviewDoesNotCacheErrors: a reviewer failure leaves no entry.
func TestReviewDoesNotCacheErrors(t *testing.T) {
	var calls int32
	r, _ := newTestReviewer(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	})
	ac := AlignmentContext{HeadSHA: "h", AuthorizedIntent: "i", DiffSummary: "d"}
	for i := 0; i < 2; i++ {
		if _, err := r.Review(context.Background(), ac); err == nil {
			t.Fatal("expected error")
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
	if st := r.CacheStats(); st.Size != 0 {
		t.Fatalf("size = %d, want 0", st.Size)
	}
}

func TestBuildAlignmentContextCarriesHeadSHA(t *testing.T) {
	ac := BuildAlignmentContext(PR{Title: "t", HeadSHA: " abc "}, nil, nil, nil)
	if ac.HeadSHA != "abc" {
		t.Fatalf("HeadSHA = %q, want abc", ac.HeadSHA)
	}
}

func TestAlignmentCacheEvictsLRU(t *testing.T) {
	c := newAlignmentCache(2)
	for i := 0; i < 3; i++ {
		c.put(fmt.Sprint("k", i), ModelAlignmentVerdict{Status: AlignmentStatusAligned})
	}
	if _, ok := c.get("k0"); ok {
		t.Fatal("k0 should have been evicted")
	}
	if _, ok := c.get("k2"); !ok {
		t.Fatal("k2 should be present")
	}
}

func TestNilReviewerCacheStats(t *testing.T) {
	var r *AlignmentReviewer
	if st := r.CacheStats(); st != (AlignmentCacheStats{}) {
		t.Fatalf("stats = %+v, want zero", st)
	}
}
