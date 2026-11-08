package retro

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

func TestAdvisorDecisionStoreLaneEndToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knowledge-advisor-decisions.jsonl")
	w := NewAdvisorDecisionStore(path)
	w.Record(AdvisorDecisionRecord{Agent: "dev", Kick: "dev#1", Refs: []string{"o/r#1"}, Mode: "rank", Outcome: "held-out", Slug: "s1"})
	w.Record(AdvisorDecisionRecord{Agent: "dev", Kick: "dev#2", Refs: []string{"o/r#1"}, Mode: "rank", Outcome: "held-out"})
	w.Record(AdvisorDecisionRecord{Agent: "dev", Kick: "dev#3", Refs: []string{"o/r#2", "o/r#9"}, Mode: "rank", Outcome: "held-out"})
	w.Record(AdvisorDecisionRecord{Agent: "dev", Kick: "dev#4", Refs: []string{"O/R#2"}, Mode: "rank", Outcome: "delivered", Slug: "s2"})
	w.Record(AdvisorDecisionRecord{Agent: "", Kick: "dev#5", Refs: []string{"o/r#3"}, Mode: "rank", Outcome: "delivered"})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// A kill mid-write leaves a truncated final line; the reload skips it.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"ts":"x","agent":"dev","kick":"dev#6","refs":["o/r#3"`)
	_ = f.Close()

	store := NewAdvisorDecisionStore(path)
	t.Cleanup(func() { _ = store.Close() })

	source := newStore(t, "decisions-source")
	retroStore := newStore(t, "decisions-retro")
	for _, b := range []struct {
		issue, pr string
		fix       int
	}{{"o/r#1", "o/r#11", 4}, {"o/r#2", "o/r#12", 2}, {"o/r#3", "o/r#13", 1}} {
		bd, err := source.Create("work "+b.issue, beads.TypeTask, beads.PriorityMedium, "scanner", b.issue)
		if err != nil {
			t.Fatal(err)
		}
		fix := b.fix
		if err := source.Update(bd.ID, func(x *beads.Bead) {
			x.Status = beads.StatusClosed
			x.Metadata = map[string]interface{}{"pr_ref": b.pr, "pr_state": "merged", "fix_attempts": itoa(fix), "kicks_received": "2"}
		}); err != nil {
			t.Fatal(err)
		}
	}
	lane := NewLane(map[string]*beads.Store{"scanner": source, Actor: retroStore}, retroStore, nil, nil, Config{ScanIntervalS: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	lane.SetAdvisorDecisions(store)

	arms := map[string]string{}
	for _, b := range source.AllClosed() {
		r := Reconstruct(b, nil, nil)
		ApplyAdvisorDecisions(&r, store)
		arms[r.IssueRef] = r.AdvisorArm
	}
	if want := map[string]string{"o/r#1": "holdout", "o/r#2": "mixed", "o/r#3": "none"}; !reflect.DeepEqual(arms, want) {
		t.Fatalf("arms = %v, want %v", arms, want)
	}
	got := lane.AdvisorArmSummary()
	want := []ArmSummary{
		{Arm: "none", Beads: 1, MeanFixAttempts: 1, MeanCIFailureCount: 1, MeanKicksReceived: 2},
		{Arm: "holdout", Beads: 1, MeanFixAttempts: 4, MeanCIFailureCount: 4, MeanKicksReceived: 2},
		{Arm: "mixed", Beads: 1, MeanFixAttempts: 2, MeanCIFailureCount: 2, MeanKicksReceived: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}

func TestAdvisorDecisionStoreRecordFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.jsonl")
	s := NewAdvisorDecisionStore(path)
	s.Record(AdvisorDecisionRecord{Agent: "dev", Kick: "dev#1", Refs: []string{"hive#7"}, Mode: "shadow", Outcome: "delivered", Slug: "a"})
	s.Record(AdvisorDecisionRecord{Agent: "dev", Kick: "dev#2", Refs: []string{"o/r#8"}, Mode: "rank", Outcome: "null"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2:\n%s", len(lines), data)
	}
	for _, want := range []string{`"arm":"shadow"`, `"arm":"deliver"`, `"ts":"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("store missing %s:\n%s", want, data)
		}
	}
	// An owner-less ref ("repo#N") still joins to the bead's qualified ref.
	if got := s.AdvisorDecisions("hivecommons/hive#7"); len(got) != 1 || got[0].Mode != "shadow" {
		t.Errorf("short ref decisions = %+v", got)
	}
	if got := s.AdvisorDecisions(""); got != nil {
		t.Errorf("empty ref decisions = %+v", got)
	}
	var nilStore *AdvisorDecisionStore
	nilStore.Record(AdvisorDecisionRecord{Agent: "dev", Kick: "dev#1"})
	if nilStore.AdvisorDecisions("o/r#8") != nil {
		t.Error("nil store returned decisions")
	}
}
