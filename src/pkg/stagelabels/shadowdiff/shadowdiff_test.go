package shadowdiff

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestDiff(t *testing.T) {
	same := Decision{Stage: stagelabels.ReviewPending, Reason: "contributor-lane:no-verdict"}
	if got := Diff(same, same); len(got) != 0 {
		t.Fatalf("identical decisions: got %+v, want none", got)
	}
	got := Diff(same, Decision{Stage: stagelabels.Fixing, Reason: same.Reason})
	if len(got) != 1 || got[0] != (Mismatch{Kind: KindStage, Legacy: "050-review-pending", Engine: "070-fixing"}) {
		t.Fatalf("stage-only diff = %+v", got)
	}
	got = Diff(same, Decision{Stage: same.Stage, Reason: "check-pending"})
	if len(got) != 1 || got[0] != (Mismatch{Kind: KindReason, Legacy: same.Reason, Engine: "check-pending"}) {
		t.Fatalf("reason-only diff = %+v", got)
	}
	got = Diff(same, Decision{Stage: stagelabels.None, Reason: ""})
	if len(got) != 2 || got[0].Kind != KindStage || got[1].Kind != KindReason || got[0].Engine != "none" {
		t.Fatalf("both diff = %+v", got)
	}
}

func TestRecorderCountsAndLogsOncePerBucket(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	r := NewRecorder(WithLogger(logger), WithNow(func() time.Time { return at }))

	legacy := Decision{Stage: stagelabels.ReviewPending, Reason: "a"}
	engine := Decision{Stage: stagelabels.Reviewed, Reason: "a"}
	if d := r.Record("o/r#1", legacy, legacy); len(d) != 0 {
		t.Fatalf("agreeing record returned %+v", d)
	}
	for i := 0; i < 3; i++ {
		if d := r.Record("o/r#2", legacy, engine); len(d) != 1 {
			t.Fatalf("record %d returned %+v", i, d)
		}
	}
	r.Record("o/r#3", legacy, Decision{Stage: stagelabels.ReviewPending, Reason: "b"})

	s := r.Snapshot()
	if s.Items != 5 || s.Agreed != 1 || s.StageMismatches != 3 {
		t.Fatalf("snapshot counts = items %d agreed %d stage %d", s.Items, s.Agreed, s.StageMismatches)
	}
	want := []Counter{
		{Kind: KindReason, Legacy: "a", Engine: "b", Count: 1},
		{Kind: KindStage, Legacy: "050-review-pending", Engine: "060-reviewed", Count: 3},
	}
	if len(s.Counters) != len(want) {
		t.Fatalf("counters = %+v", s.Counters)
	}
	for i := range want {
		if s.Counters[i] != want[i] {
			t.Fatalf("counter %d = %+v, want %+v", i, s.Counters[i], want[i])
		}
	}
	if len(s.Examples) != 4 || s.Examples[0].Key != "o/r#2" || s.Examples[3].Key != "o/r#3" || !s.Examples[0].At.Equal(at) {
		t.Fatalf("examples = %+v", s.Examples)
	}
	if n := strings.Count(buf.String(), "legacy and engine disagree"); n != 2 {
		t.Fatalf("logged %d lines, want one per distinct bucket (2):\n%s", n, buf.String())
	}
}

func TestRecorderRingIsBoundedOldestFirst(t *testing.T) {
	r := NewRecorder(WithExampleLimit(3))
	for i := 1; i <= 5; i++ {
		r.Record(strings.Repeat("k", i), Decision{Stage: stagelabels.Actionable}, Decision{Stage: stagelabels.Claimed})
	}
	s := r.Snapshot()
	if len(s.Examples) != 3 {
		t.Fatalf("ring held %d examples, want 3", len(s.Examples))
	}
	for i, want := range []string{"kkk", "kkkk", "kkkkk"} {
		if s.Examples[i].Key != want {
			t.Fatalf("example %d key = %q, want %q (oldest first)", i, s.Examples[i].Key, want)
		}
	}
	if s.Counters[0].Count != 5 || s.StageMismatches != 5 {
		t.Fatalf("counts survive ring eviction: %+v", s)
	}

	r.Reset()
	if s := r.Snapshot(); s.Items != 0 || len(s.Counters) != 0 || len(s.Examples) != 0 {
		t.Fatalf("after reset = %+v", s)
	}
	// Logging resumes after a reset (the bucket is distinct again).
	var buf bytes.Buffer
	r2 := NewRecorder(WithLogger(slog.New(slog.NewTextHandler(&buf, nil))), WithExampleLimit(0))
	if r2.limit != DefaultExampleLimit {
		t.Fatalf("limit 0 should keep the default, got %d", r2.limit)
	}
	r2.Record("k", Decision{Stage: stagelabels.Actionable}, Decision{Stage: stagelabels.Claimed})
	r2.Reset()
	r2.Record("k", Decision{Stage: stagelabels.Actionable}, Decision{Stage: stagelabels.Claimed})
	if n := strings.Count(buf.String(), "disagree"); n != 2 {
		t.Fatalf("logged %d lines across a reset, want 2", n)
	}
}

func TestRecorderNilAndConcurrent(t *testing.T) {
	var nilRec *Recorder
	if d := nilRec.Record("k", Decision{}, Decision{Stage: stagelabels.Claimed}); len(d) != 1 {
		t.Fatalf("nil recorder still diffs: %+v", d)
	}
	if s := nilRec.Snapshot(); s.Items != 0 {
		t.Fatalf("nil snapshot = %+v", s)
	}
	nilRec.Reset()

	r := NewRecorder(WithExampleLimit(4), WithNow(nil))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				r.Record("k", Decision{Stage: stagelabels.Actionable}, Decision{Stage: stagelabels.Claimed})
			}
		}()
	}
	wg.Wait()
	s := r.Snapshot()
	if s.Items != 200 || s.StageMismatches != 200 || len(s.Examples) != 4 || s.Counters[0].Count != 200 {
		t.Fatalf("concurrent snapshot = items %d stage %d examples %d counters %+v", s.Items, s.StageMismatches, len(s.Examples), s.Counters)
	}
}
