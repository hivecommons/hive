package betty

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIsHiveStatusQuestion(t *testing.T) {
	for text, want := range map[string]bool{
		"Betty, what can you tell me about Hive right now? How many open issues are there?": true,
		"Betty, what PRs are open in the hive repo?":                                        true,
		"how many actionable items do we have":                                              true,
		"which agents is the hive running?":                                                 true,
		"what did the hive merge recently, anything recently merged?":                       true,
		"what's blocked in the repo?":                                                       true,
		"what is the board pressure of hive's hive?":                                        true,
		"Betty, what's the BP right now?":                                                   true,
		"what does the hive dashboard say about coverage?":                                  true,
		"Betty, how does the cert rotation work?":                                           false,
		"are we blocked on the design review?":                                              false,
		"who owns the deploy pipeline?":                                                     false,
		"Betty, what is the capital of France?":                                             false,
	} {
		if got := IsHiveStatusQuestion(text); got != want {
			t.Errorf("IsHiveStatusQuestion(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestHiveStatusRetriever(t *testing.T) {
	brief := func(context.Context) (string, error) { return "Open now: 40 issues and 12 PRs.", nil }
	r := HiveStatusRetriever{Brief: brief}
	if r.Name() != "hive-status" {
		t.Fatal(r.Name())
	}
	got := r.Retrieve(context.Background(), "how many open issues are there?", nil, retrieveLimit)
	if len(got) != 1 || got[0].Source != HiveStatusSource || got[0].Text != "Open now: 40 issues and 12 PRs." {
		t.Fatalf("hits %+v", got)
	}
	if hits := r.Retrieve(context.Background(), "how does the cert rotation work?", nil, retrieveLimit); hits != nil {
		t.Fatalf("an unrelated question gets nothing: %+v", hits)
	}
	failing := HiveStatusRetriever{Brief: func(context.Context) (string, error) { return "", errors.New("initializing") }}
	if hits := failing.Retrieve(context.Background(), "open issues?", nil, retrieveLimit); hits != nil {
		t.Fatal("a failing brief gets nothing")
	}
	if (HiveStatusRetriever{}).Retrieve(context.Background(), "open issues?", nil, retrieveLimit) != nil {
		t.Fatal("no brief, nothing")
	}
}

// TestHiveStatusAnsweredInProcess pins hivecommons/hive#18590 finding 9: an
// addressed question about the hive is answered from the live status, and
// even a model that asks for research does not start a deep dive.
func TestHiveStatusAnsweredInProcess(t *testing.T) {
	llm := &fakeLLM{reply: func(_, prompt string) (string, error) {
		return `{"answer":"There are 40 open issues and 12 open PRs.","confidence":0.4,"needs_research":true}`, nil
	}}
	brief := func(context.Context) (string, error) { return "Open now: 40 issues and 12 PRs.", nil }
	r := newRigWith(t, testSettings(), llm, func(d *Deps) {
		d.CanResearch = true
		d.Retrievers = []Retriever{HiveStatusRetriever{Brief: brief}}
	})
	start := time.Now()
	r.e.Observe(Line{Who: "Andy", Text: "Betty, what can you tell me about Hive right now? How many open issues are there?"})
	a := r.reply()
	if a.Research != "" || a.Text != "There are 40 open issues and 12 open PRs." {
		t.Fatalf("answered from live data, no deep dive: %+v", a)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
	p := llm.last().prompt
	if !strings.Contains(p, "Open now: 40 issues and 12 PRs.") || !strings.Contains(p, `Context marked "hive live status" is the hive's own state`) {
		t.Fatalf("prompt %s", p)
	}
}
