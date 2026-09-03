package retro

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/timeline"
)

type fakeAttempts map[string]int

func (f fakeAttempts) Attempts(repo string, number int) int {
	return f[repo+"#"+itoa(number)]
}

func TestReconstruct(t *testing.T) {
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		metadata map[string]interface{}
		timeline []timeline.Event
		attempts fakeAttempts
		want     RetroRecord
	}{
		{
			name: "metadata and timeline assemble compact record",
			metadata: map[string]interface{}{
				"claimed_at":       base.Format(time.RFC3339),
				"pr_ref":           "hivecommons/hive#42",
				"pr_state":         "closed",
				"ci_failure_count": "2",
				"drift_pauses":     "1",
			},
			timeline: []timeline.Event{
				{IssueRef: "hivecommons/hive#2809", Kind: timeline.KindKicked, At: base.Add(time.Hour).UnixMilli()},
				{IssueRef: "hivecommons/hive#2809", Kind: timeline.KindKicked, At: base.Add(2 * time.Hour).UnixMilli()},
			},
			attempts: fakeAttempts{"hivecommons/hive#42": 3},
			want: RetroRecord{
				IssueRef:       "hivecommons/hive#2809",
				PRRef:          "hivecommons/hive#42",
				PRState:        "closed",
				KicksReceived:  2,
				CIFailureCount: 3,
				FixAttempts:    3,
				DriftPauses:    1,
				ClaimToClose:   9 * 24 * time.Hour,
			},
		},
		{
			name: "timeline merged event supplies pr state and ref",
			timeline: []timeline.Event{
				{IssueRef: "hivecommons/hive#2810", Kind: timeline.KindKicked, At: base.Add(time.Hour).UnixMilli()},
				{IssueRef: "hivecommons/hive#2810", Kind: timeline.KindMerged, At: base.Add(2 * time.Hour).UnixMilli(), Attrs: map[string]string{"pr_number": "77"}},
				{IssueRef: "hivecommons/hive#2810", Kind: timeline.KindBlocked, At: base.Add(3 * time.Hour).UnixMilli(), Attrs: map[string]string{"trigger": "trajectory-review", "reason": "drift pause"}},
			},
			want: RetroRecord{
				IssueRef:      "hivecommons/hive#2810",
				PRRef:         "hivecommons/hive#77",
				PRState:       "merged",
				KicksReceived: 1,
				DriftPauses:   1,
				ClaimToClose:  9*24*time.Hour - time.Hour,
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t, tt.name+"-source")
			b, err := store.Create("retro source", beads.TypeTask, beads.PriorityMedium, "scanner", tt.want.IssueRef)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Update(b.ID, func(bd *beads.Bead) {
				bd.Status = beads.StatusClosed
				bd.Metadata = tt.metadata
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.Update(b.ID, func(bd *beads.Bead) { bd.ClosedAt = nil }); err != nil {
				t.Fatal(err)
			}
			if err := store.SetMetadata(b.ID, "closed_at", base.Add(9*24*time.Hour).Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}
			gotBead, err := store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			tl := timeline.NewStoreWithCap(20)
			for _, e := range tt.timeline {
				tl.Record(e)
			}
			got := Reconstruct(gotBead, tl, tt.attempts)
			if got.IssueRef != tt.want.IssueRef || got.PRRef != tt.want.PRRef || got.PRState != tt.want.PRState {
				t.Fatalf("refs/state = (%q,%q,%q), want (%q,%q,%q)", got.IssueRef, got.PRRef, got.PRState, tt.want.IssueRef, tt.want.PRRef, tt.want.PRState)
			}
			if got.KicksReceived != tt.want.KicksReceived || got.CIFailureCount != tt.want.CIFailureCount || got.FixAttempts != tt.want.FixAttempts || got.DriftPauses != tt.want.DriftPauses {
				t.Fatalf("counts = kicks %d ci %d fix %d drift %d, want kicks %d ci %d fix %d drift %d", got.KicksReceived, got.CIFailureCount, got.FixAttempts, got.DriftPauses, tt.want.KicksReceived, tt.want.CIFailureCount, tt.want.FixAttempts, tt.want.DriftPauses)
			}
			if got.ClaimToClose != tt.want.ClaimToClose {
				t.Fatalf("claim-to-close = %s, want %s", got.ClaimToClose, tt.want.ClaimToClose)
			}
		})
	}
}

func TestDetect(t *testing.T) {
	thresholds := Thresholds{MaxFixAttempts: 3, MaxKicks: 5, LongStall: 7 * 24 * time.Hour}
	cases := []struct {
		name string
		rec  RetroRecord
		want []string
	}{
		{"none below thresholds", RetroRecord{PRRef: "o/r#1", FixAttempts: 2, KicksReceived: 4, ClaimToClose: 7 * 24 * time.Hour}, nil},
		{"fix attempts at threshold", RetroRecord{BeadID: "b1", PRRef: "o/r#1", FixAttempts: 3}, []string{PatternExcessiveFixAttempts}},
		{"kicks at threshold", RetroRecord{BeadID: "b2", PRRef: "o/r#2", KicksReceived: 5}, []string{PatternExcessiveKicks}},
		{"long stall strictly greater", RetroRecord{BeadID: "b3", PRRef: "o/r#3", ClaimToClose: 7*24*time.Hour + time.Second}, []string{PatternLongStall}},
		{"drift pause", RetroRecord{BeadID: "b4", PRRef: "o/r#4", DriftPauses: 1}, []string{PatternDriftPause}},
		{"all patterns stable order", RetroRecord{BeadID: "b5", PRRef: "o/r#5", FixAttempts: 4, KicksReceived: 6, ClaimToClose: 8 * 24 * time.Hour, DriftPauses: 2}, []string{PatternExcessiveFixAttempts, PatternExcessiveKicks, PatternLongStall, PatternDriftPause}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			gotFindings := Detect(tt.rec, thresholds)
			got := make([]string, len(gotFindings))
			for i, f := range gotFindings {
				got[i] = f.Pattern
			}
			if len(got) == 0 {
				got = nil
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("patterns = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestReconstructDoneBeadUsesUpdatedAtAsCompletion(t *testing.T) {
	store := newStore(t, "done-updated-at")
	claimed := time.Now().UTC().Add(-8 * 24 * time.Hour)
	b, err := store.Create("done source", beads.TypeTask, beads.PriorityMedium, "scanner", "hivecommons/hive#2809")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(b.ID, func(bd *beads.Bead) {
		bd.Status = beads.StatusDone
		bd.Metadata = map[string]interface{}{
			"claimed_at": claimed.Format(time.RFC3339),
			"pr_ref":     "hivecommons/hive#42",
			"pr_state":   "merged",
		}
	}); err != nil {
		t.Fatal(err)
	}
	gotBead, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := Reconstruct(gotBead, nil, nil)
	if got.ClosedAt.IsZero() {
		t.Fatal("done bead should use UpdatedAt as completion time")
	}
	if got.ClaimToClose < 7*24*time.Hour {
		t.Fatalf("claim-to-close = %s, want at least 7d", got.ClaimToClose)
	}
}

func TestLaneFilesAdvisoryOnceAndMarksAnalyzed(t *testing.T) {
	source := newStore(t, "lane-source")
	retroStore := newStore(t, "lane-retro")
	b, err := source.Create("hard issue", beads.TypeTask, beads.PriorityMedium, "scanner", "hivecommons/hive#2809")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Update(b.ID, func(bd *beads.Bead) {
		bd.Status = beads.StatusClosed
		bd.Metadata = map[string]interface{}{
			"pr_ref":       "hivecommons/hive#42",
			"pr_state":     "merged",
			"fix_attempts": "3",
		}
	}); err != nil {
		t.Fatal(err)
	}
	lane := NewLane(map[string]*beads.Store{"scanner": source, Actor: retroStore}, retroStore, nil, nil, Config{ScanIntervalS: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if created := lane.Run(context.Background()); created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	if created := lane.Run(context.Background()); created != 0 {
		t.Fatalf("second run created = %d, want 0", created)
	}
	advisories := retroStore.List(beads.ListFilter{})
	if len(advisories) != 1 {
		t.Fatalf("advisories = %d, want 1", len(advisories))
	}
	if advisories[0].Actor != Actor || advisories[0].Type != beads.TypeAdvisory {
		t.Fatalf("advisory bead actor/type = %s/%s", advisories[0].Actor, advisories[0].Type)
	}
	got, err := source.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Meta(metadataAnalyzedAt) == "" {
		t.Fatalf("source bead missing %s", metadataAnalyzedAt)
	}
}

func TestBuildAnalysisPromptBounds(t *testing.T) {
	rec := RetroRecord{
		BeadID:         "b1",
		Title:          strings.Repeat("title ", 200),
		Type:           beads.TypeTask,
		Actor:          "scanner",
		IssueRef:       "o/r#1",
		PRRef:          "o/r#2",
		PRState:        "merged",
		KicksReceived:  6,
		CIFailureCount: 5,
		FixAttempts:    4,
		DriftPauses:    1,
		ClaimToClose:   9 * 24 * time.Hour,
	}
	findings := []Finding{{Pattern: PatternExcessiveFixAttempts, Severity: "medium", Detail: strings.Repeat("detail ", 2000)}}
	msgs := buildAnalysisPrompt(rec, findings)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if len([]rune(msgs[1].Content)) > maxPromptChars {
		t.Fatalf("prompt length = %d, want <= %d", len([]rune(msgs[1].Content)), maxPromptChars)
	}
	if strings.Contains(msgs[1].Content, strings.Repeat("title ", 50)) {
		t.Fatal("record title was not compacted")
	}
}

type fakeAnalysisClient struct {
	replies []string
	err     error
	calls   int
}

func (f *fakeAnalysisClient) Complete(context.Context, []chatMessage, int) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if f.calls > len(f.replies) {
		return f.replies[len(f.replies)-1], nil
	}
	return f.replies[f.calls-1], nil
}

func TestAnalysisRetriesInvalidJSON(t *testing.T) {
	client := &fakeAnalysisClient{replies: []string{
		`{"root_cause_hypothesis":"","process_improvement":"x","generalizable":false,"generalizable_lesson":""}`,
		`{"root_cause_hypothesis":"CI failures were discovered late.","process_improvement":"Add pre-submit checks before opening PRs.","generalizable":true,"generalizable_lesson":"Run the same validation locally before opening PRs so repeated CI-only failures are caught earlier."}`,
	}}
	analyzer := newAnalyzerWithClient("m", client)
	got, err := analyzer.Analyze(context.Background(), RetroRecord{BeadID: "b"}, []Finding{{Pattern: PatternExcessiveFixAttempts}})
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Fatalf("calls = %d, want 2", client.calls)
	}
	if !got.Generalizable || got.GeneralizableLesson == "" {
		t.Fatalf("unexpected analysis: %#v", got)
	}
}

func TestLaneFailOpenOnAnalysisError(t *testing.T) {
	source := newStore(t, "analysis-error-source")
	retroStore := newStore(t, "analysis-error-retro")
	b, err := source.Create("hard issue", beads.TypeTask, beads.PriorityMedium, "scanner", "hivecommons/hive#2809")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Update(b.ID, func(bd *beads.Bead) {
		bd.Status = beads.StatusClosed
		bd.Metadata = map[string]interface{}{"pr_ref": "hivecommons/hive#42", "pr_state": "merged", "fix_attempts": "3"}
	}); err != nil {
		t.Fatal(err)
	}
	lane := NewLane(map[string]*beads.Store{"scanner": source, Actor: retroStore}, retroStore, nil, nil, Config{ScanIntervalS: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	lane.SetAnalyzer(newAnalyzerWithClient("m", &fakeAnalysisClient{err: errors.New("model down")}))
	if created := lane.Run(context.Background()); created != 1 {
		t.Fatalf("created = %d, want deterministic advisory despite model error", created)
	}
	advisories := retroStore.List(beads.ListFilter{})
	if advisories[0].Meta(metadataAnalysisRootCause) != "" || strings.Contains(advisories[0].Notes, "Model-generated") {
		t.Fatalf("advisory should not be enriched on model error: %#v", advisories[0])
	}
}

type fakeKnowledgeSink struct {
	lessons []knowledge.RetroLesson
}

func (f *fakeKnowledgeSink) IngestRetroLesson(_ context.Context, lesson knowledge.RetroLesson) (string, bool, error) {
	f.lessons = append(f.lessons, lesson)
	return "retro-lesson-test", true, nil
}

func TestLaneEnrichesAdvisoryAndFeedsKnowledge(t *testing.T) {
	source := newStore(t, "analysis-source")
	retroStore := newStore(t, "analysis-retro")
	b, err := source.Create("hard issue", beads.TypeTask, beads.PriorityMedium, "scanner", "hivecommons/hive#2809")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Update(b.ID, func(bd *beads.Bead) {
		bd.Status = beads.StatusClosed
		bd.Metadata = map[string]interface{}{"pr_ref": "hivecommons/hive#42", "pr_state": "merged", "fix_attempts": "3"}
	}); err != nil {
		t.Fatal(err)
	}
	client := &fakeAnalysisClient{replies: []string{`{"root_cause_hypothesis":"Validation ran too late.","process_improvement":"Run targeted checks before opening a PR.","generalizable":true,"generalizable_lesson":"Run targeted validation before opening PRs when changes affect CI-sensitive code paths."}`}}
	sink := &fakeKnowledgeSink{}
	lane := NewLane(map[string]*beads.Store{"scanner": source, Actor: retroStore}, retroStore, nil, nil, Config{ScanIntervalS: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	lane.SetAnalyzer(newAnalyzerWithClient("m", client))
	lane.SetKnowledgeSink(sink)
	if created := lane.Run(context.Background()); created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	advisory := retroStore.List(beads.ListFilter{})[0]
	if advisory.Meta(metadataAnalysisRootCause) == "" || !strings.Contains(advisory.Notes, "Model-generated retro analysis") {
		t.Fatalf("advisory not enriched: %#v", advisory)
	}
	if len(sink.lessons) != 1 || sink.lessons[0].SourcePR != "hivecommons/hive#42" {
		t.Fatalf("lessons = %#v, want one sourced lesson", sink.lessons)
	}
}

func newStore(t *testing.T, name string) *beads.Store {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(wd, ".retro-test", sanitizeName(t.Name()+"-"+name))
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(wd, ".retro-test")) })
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func sanitizeName(s string) string {
	repl := strings.NewReplacer("/", "-", " ", "-", "#", "-")
	return repl.Replace(s)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
