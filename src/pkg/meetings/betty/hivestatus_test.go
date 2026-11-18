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
		"are there any issues that need my attention":                                       true,
		"Betty, are there any issues that need my attention option":                         true,
		"is anything waiting on me?":                                                        true,
		"which PRs need my review":                                                          true,
		"pay attention to the demo":                                                         false,
		"any issues with the audio":                                                         false,
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
	r.e.Observe(Line{Who: "Andy", Text: "Betty, how many open issues does Hive have right now?"})
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

// Scored live test: board pressure, PR counts, running agents and a PR
// lookup all went to research and failed. Questions about the hive never
// start a deep dive, a misheard "full requests" still counts, and a PR
// lookup is answered from the live state.
func TestHiveQuestionsNeverResearch(t *testing.T) {
	for _, q := range []string{
		"Betty, what is the board pressure right now?",
		"Betty, how many full requests are open right now?",
		"Betty, which agents are running right now?",
		"Betty, what is pool request 18689 about?",
	} {
		if !IsHiveStatusQuestion(NormalizeDomain(q)) {
			t.Errorf("not a hive question: %q", q)
		}
	}
	if got := NormalizeDomain("how many full requests and one pool request, bored pressure?"); got != "how many pull requests and one pull request, board pressure?" {
		t.Fatalf("NormalizeDomain = %q", got)
	}
	if itemNumber(NormalizeDomain("what is pull request 18,689 about")) != 18689 || itemNumber("what is pull request 18689 about") != 18689 || itemNumber("PR #12") != 12 || itemNumber("the 3 of us") != 0 {
		t.Fatal("itemNumber")
	}
	llm := &fakeLLM{reply: func(_, prompt string) (string, error) {
		return `{"answer":"Not sure.","confidence":0.2,"needs_research":true}`, nil
	}}
	item := func(_ context.Context, n int) (string, bool) {
		return "PR hivecommons/hive#18689 \"fix: x\" by andy.", n == 18689
	}
	brief := func(context.Context) (string, error) { return "Open now: 35 issues and 12 PRs across 13 repos.", nil }
	r := newRigWith(t, testSettings(), llm, func(d *Deps) {
		d.CanResearch = true
		d.Retrievers = []Retriever{HiveStatusRetriever{Brief: brief, Item: item}}
	})
	r.e.Observe(Line{Who: "Andy", Text: "Betty, what is pull request 18689 about?"})
	if a := r.reply(); a.Research != "" {
		t.Fatalf("a hive lookup must not dig in: %+v", a)
	}
	if p := llm.last().prompt; !strings.Contains(p, "hivecommons/hive#18689") {
		t.Fatalf("lookup not in the context: %s", p)
	}
}

// Two questions in one segment each get their own answer.
func TestTwoQuestionsInOneLine(t *testing.T) {
	if got := splitQuestions("Betty, how many open issues are there? How many pull requests are open?"); len(got) != 2 {
		t.Fatalf("split = %q", got)
	}
	if got := splitQuestions("Betty, really? ok"); len(got) != 1 {
		t.Fatalf("short fragments stay whole: %q", got)
	}
	llm := &fakeLLM{reply: func(_, prompt string) (string, error) {
		if strings.Contains(strings.ToLower(prompt), "question from andy: betty, how many pull requests") {
			return `{"answer":"12 PRs are open.","confidence":0.9,"topic":"open PRs"}`, nil
		}
		return `{"answer":"35 issues are open.","confidence":0.9,"topic":"open issues"}`, nil
	}}
	r := newRigWith(t, testSettings(), llm, func(d *Deps) {
		d.Retrievers = []Retriever{HiveStatusRetriever{Brief: func(context.Context) (string, error) { return "Open now: 35 issues and 12 PRs.", nil }}}
	})
	r.e.Observe(Line{Who: "Andy", Text: "Betty, how many open issues are there? How many pull requests are open?"})
	var answers []string
	for len(answers) < 2 {
		if rep := r.reply(); rep.Trigger != TriggerQueue {
			answers = append(answers, rep.Text)
		}
	}
	got := strings.Join(answers, " | ")
	if !strings.Contains(got, "35 issues") || !strings.Contains(got, "12 PRs") {
		t.Fatalf("both questions answered: %s", got)
	}
}

// "are there any issues that need my attention" was sent to web search and
// then to a research kick (live Meet test); it is answered from the hive's
// own state and is kept off both.
func TestAttentionQuestionStaysOnHiveStatus(t *testing.T) {
	q := "Betty, are there any issues that need my attention option"
	h := HiveStatusRetriever{Brief: func(context.Context) (string, error) { return "Awaiting a decision: 2.", nil }}
	got := h.Retrieve(context.Background(), q, nil, retrieveLimit)
	if len(got) != 1 || got[0].Source != HiveStatusSource {
		t.Fatalf("got %+v", got)
	}
	if _, blocked := NewWebGuard("Betty", nil, nil).check(q); blocked != webBlockedStatus {
		t.Fatalf("web guard blocked = %q, want %q", blocked, webBlockedStatus)
	}
	if !AnsweredLocally(got) && !hasHiveStatus(got) {
		t.Fatal("hive status must settle the answer")
	}
	if !strings.Contains(answerPrompt(modeAddressed, "Bo", q, got, nil, false), "nothing needs attention right now") {
		t.Fatal("prompt lacks the nothing-needs-attention instruction")
	}
}
