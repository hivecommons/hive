package betty

import (
	"context"
	"testing"
	"time"
)

// Each answer carries its stage timings: retrieval, the model call and the
// time to the answer, its source and its budget.
func TestAnswerTimingStages(t *testing.T) {
	var r *rig
	hook := hookRetriever{fn: func() { r.clk.add(300 * time.Millisecond) }}
	llm := &fakeLLM{reply: func(string, string) (string, error) {
		r.clk.add(time.Second)
		return `{"answer":"It ships Friday.","confidence":0.9}`, nil
	}}
	r = newRig(t, testSettings(), llm, hook)
	r.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?"})
	tm := r.reply().Timing
	if tm == nil {
		t.Fatal("no timing")
	}
	if tm.RetrieveMS != 300 || tm.ModelMS != 1000 || tm.AnswerMS != 1300 || tm.Source != SourceRetrieval ||
		tm.BudgetMS != AnswerBudget.Milliseconds() || tm.OverBudget {
		t.Fatalf("timing %+v", tm)
	}
}

// A live-state answer has the tighter budget and is flagged when it
// misses it.
func TestHiveStatusAnswerBudget(t *testing.T) {
	var r *rig
	llm := &fakeLLM{reply: func(string, string) (string, error) {
		r.clk.add(HiveStatusBudget + time.Second)
		return `{"answer":"40 open issues.","confidence":0.9}`, nil
	}}
	r = newRig(t, testSettings(), llm, fakeRetriever{hits: []Snippet{{Source: HiveStatusSource, Title: "The hive right now", Text: "Open now: 40 issues."}}})
	r.e.Observe(Line{Who: "Bo", Text: "Betty, how many open issues are there?"})
	tm := r.reply().Timing
	if tm == nil || tm.Source != SourceHiveStatus || tm.BudgetMS != HiveStatusBudget.Milliseconds() || !tm.OverBudget {
		t.Fatalf("timing %+v", tm)
	}
	noModel := newRig(t, testSettings(), nil)
	noModel.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?"})
	if tm := noModel.reply().Timing; tm == nil || tm.Source != SourceFallback {
		t.Fatalf("fallback timing %+v", tm)
	}
}

// The hive's live state ends retrieval at once: a question about the hive
// does not wait for slower sources.
func TestGatherStopsAtHiveStatus(t *testing.T) {
	live := fakeRetriever{hits: []Snippet{{Source: HiveStatusSource, Text: "Open now: 40 issues."}}}
	slow := fakeRetriever{hits: []Snippet{{Source: "web", Text: "slow"}}, delay: time.Second}
	start := time.Now()
	got := Gather(context.Background(), []Retriever{slow, live}, "how many open issues", []string{"issue"}, 1500*time.Millisecond)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("gather waited %v for slower sources", d)
	}
	if len(got) != 1 || got[0].Source != HiveStatusSource {
		t.Fatalf("got %+v", got)
	}
	// Without the live state every source is waited for.
	if got := Gather(context.Background(), []Retriever{slow}, "q", []string{"q"}, 1500*time.Millisecond); len(got) != 1 {
		t.Fatalf("slow source dropped: %+v", got)
	}
}
