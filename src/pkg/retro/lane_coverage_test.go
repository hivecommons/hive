package retro

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/timeline"
)

// NewLane defaulting branches: non-positive scan interval falls back to
// DefaultScanIntervalS and a nil logger falls back to slog.Default.
func TestNewLaneDefaultsIntervalAndLogger(t *testing.T) {
	lane := NewLane(nil, nil, nil, nil, Config{ScanIntervalS: 0}, nil)
	if want := time.Duration(DefaultScanIntervalS) * time.Second; lane.interval != want {
		t.Fatalf("interval = %v, want %v", lane.interval, want)
	}
	if lane.logger == nil {
		t.Fatal("logger not defaulted")
	}

	lane = NewLane(nil, nil, nil, nil, Config{ScanIntervalS: -5}, nil)
	if want := time.Duration(DefaultScanIntervalS) * time.Second; lane.interval != want {
		t.Fatalf("negative interval = %v, want %v", lane.interval, want)
	}
}

// NewLane analyzer branches: a configured model with no endpoint hits the
// NewAnalyzer error path (analysis disabled, lane still usable); model plus
// endpoint constructs a live analyzer.
func TestNewLaneAnalyzerConstruction(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	lane := NewLane(nil, nil, nil, nil, Config{ScanIntervalS: 1, AnalysisModel: "m"}, logger)
	if lane.analyzer != nil {
		t.Fatalf("analyzer = %#v, want nil when endpoint missing", lane.analyzer)
	}

	lane = NewLane(nil, nil, nil, nil, Config{ScanIntervalS: 1, AnalysisModel: "m", AnalysisEndpoint: "http://localhost:9/v1/"}, logger)
	if lane.analyzer == nil {
		t.Fatal("analyzer = nil, want configured analyzer")
	}

	lane = NewLane(nil, nil, nil, nil, Config{ScanIntervalS: 1}, logger)
	if lane.analyzer != nil {
		t.Fatalf("analyzer = %#v, want nil when no model configured", lane.analyzer)
	}
}

type recordingSink struct {
	calls   int
	created bool
	err     error
}

func (r *recordingSink) IngestRetroLesson(_ context.Context, _ knowledge.RetroLesson) (string, bool, error) {
	r.calls++
	return "slug", r.created, r.err
}

// ingestLesson guard branches: nil analysis, non-generalizable analysis, and a
// nil sink must all skip ingestion without panicking.
func TestIngestLessonGuards(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := RetroRecord{BeadID: "b-1"}

	sink := &recordingSink{}
	lane := NewLane(nil, nil, nil, nil, Config{ScanIntervalS: 1}, logger)
	lane.SetKnowledgeSink(sink)

	lane.ingestLesson(context.Background(), rec, nil)
	lane.ingestLesson(context.Background(), rec, &Analysis{Generalizable: false, GeneralizableLesson: "x"})
	if sink.calls != 0 {
		t.Fatalf("sink called %d times, want 0", sink.calls)
	}

	noSink := NewLane(nil, nil, nil, nil, Config{ScanIntervalS: 1}, logger)
	noSink.ingestLesson(context.Background(), rec, &Analysis{Generalizable: true, GeneralizableLesson: "x"})
}

// ingestLesson outcome branches: sink error is swallowed with a warning, and
// created=false (deduplicated lesson) completes without the info log path.
func TestIngestLessonSinkOutcomes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := RetroRecord{BeadID: "b-1", PRRef: "hivecommons/hive#42"}
	analysis := &Analysis{Generalizable: true, GeneralizableLesson: "lesson"}

	failing := &recordingSink{err: errors.New("vault offline")}
	lane := NewLane(nil, nil, nil, nil, Config{ScanIntervalS: 1}, logger)
	lane.SetKnowledgeSink(failing)
	lane.ingestLesson(context.Background(), rec, analysis)
	if failing.calls != 1 {
		t.Fatalf("failing sink calls = %d, want 1", failing.calls)
	}

	dedup := &recordingSink{created: false}
	lane.SetKnowledgeSink(dedup)
	lane.ingestLesson(context.Background(), rec, analysis)
	if dedup.calls != 1 {
		t.Fatalf("dedup sink calls = %d, want 1", dedup.calls)
	}

	created := &recordingSink{created: true}
	lane.SetKnowledgeSink(created)
	lane.ingestLesson(context.Background(), rec, analysis)
	if created.calls != 1 {
		t.Fatalf("created sink calls = %d, want 1", created.calls)
	}
}

func TestHasAssociatedClosedPR(t *testing.T) {
	cases := []struct {
		name  string
		prRef string
		state string
		want  bool
	}{
		{"no pr ref", "", "merged", false},
		{"merged", "o/r#1", "merged", true},
		{"closed", "o/r#1", "closed", true},
		{"close", "o/r#1", "close", true},
		{"done", "o/r#1", "done", true},
		{"uppercase merged", "o/r#1", "MERGED", true},
		{"open", "o/r#1", "open", false},
		{"empty state", "o/r#1", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := RetroRecord{PRRef: tc.prRef, PRState: tc.state}
			if got := r.HasAssociatedClosedPR(); got != tc.want {
				t.Fatalf("HasAssociatedClosedPR() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsDriftPauseStage(t *testing.T) {
	if isDriftPauseStage(timeline.KindKicked, nil) {
		t.Fatal("nil stage should not be a drift pause")
	}
	match := &timeline.Stage{Agent: "trajectory", Attrs: map[string]string{"action": "drift-pause"}}
	if !isDriftPauseStage(timeline.KindKicked, match) {
		t.Fatal("trajectory drift stage should match")
	}
	noMatch := &timeline.Stage{Agent: "scanner", Attrs: map[string]string{"action": "kick"}}
	if isDriftPauseStage(timeline.KindKicked, noMatch) {
		t.Fatal("unrelated stage should not match")
	}
	pauseOnly := &timeline.Stage{Agent: "scanner", Attrs: map[string]string{"reason": "pause requested"}}
	if isDriftPauseStage(timeline.KindKicked, pauseOnly) {
		t.Fatal("pause without trajectory should not match")
	}
}
