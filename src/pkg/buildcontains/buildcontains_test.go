package buildcontains

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// fakeSource serves a fixed history: commits maps "base...head" to a compare
// status; a pair that is absent is "not found".
type fakeSource struct {
	compares map[string]string
	failing  map[string]bool
	prs      map[int]PullRequest
	prErr    error
	calls    atomic.Int32
}

func (f *fakeSource) Compare(_ context.Context, base, head string) (string, bool, error) {
	f.calls.Add(1)
	k := base + "..." + head
	if f.failing[k] {
		return "", false, errors.New("boom")
	}
	st, ok := f.compares[k]
	return st, ok, nil
}

func (f *fakeSource) PullRequest(_ context.Context, n int) (PullRequest, bool, error) {
	if f.prErr != nil {
		return PullRequest{}, false, f.prErr
	}
	pr, ok := f.prs[n]
	return pr, ok, nil
}

const (
	mergeSHA = "aaaaaaa1111111"
	runSHA   = "bbbbbbb2222222"
)

func TestParseQuery(t *testing.T) {
	cases := []struct {
		in   string
		kind QueryKind
		pr   int
		sha  string
		ok   bool
	}{
		{"123", KindPR, 123, "", true},
		{"#123", KindPR, 123, "", true},
		{"PR 123", KindPR, 123, "", true},
		{"pr#123", KindPR, 123, "", true},
		{"abcdef1", KindCommit, 0, "abcdef1", true},
		{"ABCDEF1234", KindCommit, 0, "abcdef1234", true},
		{"1234567", KindCommit, 0, "1234567", true},
		{"abc12", "", 0, "", false},
		{"", "", 0, "", false},
		{"0", "", 0, "", false},
		{"not-a-sha", "", 0, "", false},
		{"unknown", "", 0, "", false},
	}
	for _, c := range cases {
		q, err := ParseQuery(c.in)
		if (err == nil) != c.ok {
			t.Fatalf("ParseQuery(%q) err=%v want ok=%v", c.in, err, c.ok)
		}
		if !c.ok {
			continue
		}
		if q.Kind != c.kind || q.PR != c.pr || q.Commit != c.sha {
			t.Fatalf("ParseQuery(%q) = %+v", c.in, q)
		}
	}
}

func TestCommitContainment(t *testing.T) {
	src := &fakeSource{compares: map[string]string{
		mergeSHA + "..." + runSHA: "ahead",
	}}
	tgt := Resolve(context.Background(), src, Query{Kind: KindCommit, Commit: mergeSHA})
	if got := tgt.Check(context.Background(), src, runSHA); got.Contains != Yes {
		t.Fatalf("ancestor must be yes, got %+v", got)
	}
}

func TestStatusMapping(t *testing.T) {
	for status, want := range map[string]Verdict{
		"identical": Yes,
		"ahead":     Yes,
		"behind":    No,
		"diverged":  No,
		"weird":     Unknown,
	} {
		src := &fakeSource{compares: map[string]string{mergeSHA + "..." + runSHA: status}}
		tgt := Target{Commit: mergeSHA}
		if got := tgt.Check(context.Background(), src, runSHA); got.Contains != want {
			t.Fatalf("status %q: got %q want %q", status, got.Contains, want)
		}
	}
}

// An unresolvable commit must be unknown, never yes and never no.
func TestUnresolvableCommitIsUnknown(t *testing.T) {
	src := &fakeSource{compares: map[string]string{}}
	tgt := Target{Commit: mergeSHA}
	got := tgt.Check(context.Background(), src, runSHA)
	if got.Contains != Unknown || got.Reason == "" {
		t.Fatalf("missing commit must be unknown with a reason, got %+v", got)
	}
	for _, running := range []string{"", "unknown", "dev", "abc", "  "} {
		got := tgt.Check(context.Background(), src, running)
		if got.Contains != Unknown {
			t.Fatalf("running %q must be unknown, got %+v", running, got)
		}
	}
}

func TestCompareErrorIsUnknownNotNo(t *testing.T) {
	src := &fakeSource{failing: map[string]bool{mergeSHA + "..." + runSHA: true}}
	got := Target{Commit: mergeSHA}.Check(context.Background(), src, runSHA)
	if got.Contains != Unknown {
		t.Fatalf("transport failure must be unknown, got %+v", got)
	}
}

// A PR whose merge commit is not in the build's history is no.
func TestPRNotInHistoryIsNo(t *testing.T) {
	src := &fakeSource{
		compares: map[string]string{mergeSHA + "..." + runSHA: "behind"},
		prs:      map[int]PullRequest{7: {Merged: true, MergeCommit: mergeSHA}},
	}
	tgt := Resolve(context.Background(), src, Query{Kind: KindPR, PR: 7})
	if tgt.Commit != mergeSHA {
		t.Fatalf("PR must resolve to its merge commit, got %+v", tgt)
	}
	if got := tgt.Check(context.Background(), src, runSHA); got.Contains != No {
		t.Fatalf("got %+v", got)
	}
}

func TestPRResolution(t *testing.T) {
	src := &fakeSource{prs: map[int]PullRequest{
		1: {Merged: false},
		2: {Merged: true, MergeCommit: ""},
	}}
	cases := []struct {
		name string
		src  Source
		pr   int
		want Verdict
	}{
		{"unmerged", src, 1, No},
		{"merged without commit", src, 2, Unknown},
		{"missing", src, 99, Unknown},
		{"lookup error", &fakeSource{prErr: errors.New("boom")}, 1, Unknown},
	}
	for _, c := range cases {
		tgt := Resolve(context.Background(), c.src, Query{Kind: KindPR, PR: c.pr})
		got := tgt.Check(context.Background(), c.src, runSHA)
		if got.Contains != c.want {
			t.Fatalf("%s: got %+v want %q", c.name, got, c.want)
		}
	}
}

func TestCheckAllDedupesAndBounds(t *testing.T) {
	src := &fakeSource{compares: map[string]string{mergeSHA + "..." + runSHA: "ahead"}}
	tgt := Target{Commit: mergeSHA}
	out := tgt.CheckAll(context.Background(), src, []string{runSHA, runSHA, runSHA, "unknown"})
	if src.calls.Load() != 1 {
		t.Fatalf("one distinct commit must be one compare, got %d", src.calls.Load())
	}
	if out[runSHA].Contains != Yes || out["unknown"].Contains != Unknown {
		t.Fatalf("got %+v", out)
	}

	many := make([]string, 0, MaxCompares+5)
	for i := 0; i < MaxCompares+5; i++ {
		many = append(many, "c"+string(rune('a'+i%6))+"00000"+string(rune('0'+i/10))+string(rune('0'+i%10)))
	}
	out = tgt.CheckAll(context.Background(), &fakeSource{}, many)
	for _, r := range many {
		if out[r].Contains == Yes {
			t.Fatalf("an unresolved commit must never be yes: %q", r)
		}
	}
}

func TestCachingSourceCachesOnlyDefinitiveAnswers(t *testing.T) {
	inner := &fakeSource{compares: map[string]string{mergeSHA + "..." + runSHA: "ahead"}}
	c := NewCachingSource(inner)
	for i := 0; i < 3; i++ {
		if st, found, err := c.Compare(context.Background(), mergeSHA, runSHA); st != "ahead" || !found || err != nil {
			t.Fatalf("got %q %v %v", st, found, err)
		}
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("definitive answer must be cached, calls=%d", inner.calls.Load())
	}
	for i := 0; i < 2; i++ {
		if _, found, _ := c.Compare(context.Background(), "ccccccc", runSHA); found {
			t.Fatal("missing commit must stay not found")
		}
	}
	if inner.calls.Load() != 3 {
		t.Fatalf("not-found must not be cached, calls=%d", inner.calls.Load())
	}
}
