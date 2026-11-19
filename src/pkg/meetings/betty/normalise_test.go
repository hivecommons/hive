package betty

import (
	"strings"
	"testing"
)

// The live meeting's mishearings, said and heard, each corrected in a
// request addressed to her.
func TestNormaliseAddressedTable(t *testing.T) {
	for _, c := range []struct{ heard, want string }{
		{"how many issues are in the high school?", "how many issues are in the hive queue?"},
		{"how many issues were in the high of Q right now.", "how many issues were in the hive queue right now."},
		{"search the web for Hives competitors. Senators.", "search the web for hive's competitors. Senators."},
		{"what else can you tell me about Hives?", "what else can you tell me about hive?"},
		{"is Cuba needs supported?", "is Kubernetes supported?"},
		{"are we going to cubicon this year", "are we going to KubeCon this year"},
		{"what is full request 12 about", "what is pull request 12 about"},
		{"any pool requests waiting", "any pull requests waiting"},
		{"what's in the high school queue", "what's in the hive queue"},
		{"what is in the hyve queue", "what is in the hive queue"},
		{"what can hi've do", "what can hive do"},
		{"which agents are working in the high?", "which agents are working in the hive?"},
		{"is the hive cue long", "is the hive queue long"},
		{"what is the bored pressure", "what is the board pressure"},
		{"is hive a c n c f project", "is hive a CNCF project"},
		{"open the r f c for me", "open the RFC for me"},
		{"how many pee ares are open", "how many PRs are open"},
		{"did the korum bot join", "did the quorum bot join"},
		{"start a session in spectacular", "start a session in Spektacular"},
		{"what is in the in box", "what is in the inbox"},
		{"sync my plod recordings", "sync my Plaud recordings"},
	} {
		n := NormaliseAddressed(c.heard)
		if n.Text != c.want || n.Raw != c.heard || !n.Changed() || len(n.Applied) == 0 {
			t.Errorf("NormaliseAddressed(%q) = %q (applied %+v), want %q", c.heard, n.Text, n.Applied, c.want)
		}
		if n.Unsure() {
			t.Errorf("NormaliseAddressed(%q) must be sure: %+v", c.heard, n.Applied)
		}
	}
}

// Everyday words stay as they are, also in a request to her.
func TestNormaliseAddressedLeavesOrdinaryWords(t *testing.T) {
	for _, text := range []string{
		"which high school did Einstein attend?",
		"my kid's high school",
		"five issues",
		"the hub airport",
		"the high priority issues",
		"how many hives are connected to the hub",
		"that demo was spectacular",
		"what is the high for today",
		"how many open pull requests does hive have",
	} {
		if n := NormaliseAddressed(text); n.Changed() || n.Text != text {
			t.Errorf("NormaliseAddressed(%q) = %q (applied %+v), want it untouched", text, n.Text, n.Applied)
		}
	}
}

// "in the high school" with no other word about the hive is corrected with
// low confidence: she asks instead of assuming.
func TestNormaliseUnsureAsks(t *testing.T) {
	n := NormaliseAddressed("what's in the high school right now?")
	if n.Text != "what's in the hive queue right now?" || !n.Unsure() || n.Clarify() != "Did you mean the hive queue?" {
		t.Fatalf("unsure = %+v, clarify %q", n, n.Clarify())
	}
	if len(n.Applied) != 1 || n.Applied[0].Heard != "in the high school" || n.Applied[0].Meant != "in the hive queue" || !n.Applied[0].Unsure {
		t.Fatalf("applied = %+v", n.Applied)
	}
	if sure := NormaliseAddressed("how many issues are in the high school?"); sure.Unsure() || sure.Clarify() != "" {
		t.Fatalf("a request about issues is sure: %+v", sure)
	}
}

func TestSoundsLikeHive(t *testing.T) {
	for word, want := range map[string]bool{
		"hyve": true, "hyves": true, "hi've": true, "high've": true, "hife": true, "Hyve's": true,
		"hive": false, "hives": false, "five": false, "dive": false, "high": false, "have": false, "hi": false, "his": false,
	} {
		if got, _ := soundsLikeHive(word); got != want {
			t.Errorf("soundsLikeHive(%q) = %v, want %v", word, got, want)
		}
	}
	if _, possessive := soundsLikeHive("hyve's"); !possessive {
		t.Fatal("possessive kept")
	}
}

// The correction is made only on a line addressed to her; other people's
// lines are never rewritten, and an addressed line with no product context
// is left alone.
func TestTriggerCarriesNormalisedOnlyWhenAddressed(t *testing.T) {
	m := NewMatcher("Betty", []string{"betty", "betty bee", "hive"})
	for _, text := range []string{
		"my kid's high school is closed in the high school holidays",
		"we closed five issues in the high school",
		"the hub airport was packed",
		"I heard Cuba needs more pool requests at cubicon",
	} {
		if got := m.Match(text, true); got.Addressed || got.Normalised != "" || got.HeardRest != "" || got.Fixes != "" || got.Clarify != "" {
			t.Errorf("unaddressed %q must stay untouched: %+v", text, got)
		}
	}
	plain := m.Match("Betty, which high school did Einstein attend?", true)
	if !plain.Addressed || plain.Normalised != "" || plain.Clarify != "" || plain.Rest != "which high school did einstein attend" {
		t.Fatalf("no product context: %+v", plain)
	}
	got := m.Match("Betty, how many issues are in the high school?", true)
	if got.Rest != "how many issues are in the hive queue" || got.HeardRest != "how many issues are in the high school" ||
		got.Normalised != "Betty, how many issues are in the hive queue?" || got.Clarify != "" ||
		got.Fixes != "in the high school -> in the hive queue" {
		t.Fatalf("addressed = %+v", got)
	}
	// The corrected request is what the hive status match sees.
	if !IsHiveStatusQuestion(got.Rest) {
		t.Fatalf("the corrected request is a hive status question: %q", got.Rest)
	}
	kube := m.Match("Betty, does hive run on Cuba needs?", true)
	if kube.Rest != "does hive run on kubernetes" || kube.Normalised != "Betty, does hive run on Kubernetes?" {
		t.Fatalf("kubernetes = %+v", kube)
	}
	unsure := m.Match("Betty, what's in the high school right now?", true)
	if unsure.Clarify != "Did you mean the hive queue?" {
		t.Fatalf("unsure = %+v", unsure)
	}
	// An intro is never corrected.
	if intro := m.Match("Betty, who are you in the high school?", true); intro.Normalised != "" || intro.Clarify != "" {
		t.Fatalf("intro = %+v", intro)
	}
}

// The prompt shows both readings, and the transcript keeps the words as
// heard.
func TestAnswerPromptShowsHeardAndMeant(t *testing.T) {
	r := newRig(t, testSettings(), &fakeLLM{})
	r.e.Observe(Line{Who: "Andy", Text: "Betty, how many issues are in the high school?"})
	r.reply()
	p := r.llm.last().prompt
	for _, want := range []string{
		"Question from Andy: Betty, how many issues are in the high school?\n",
		"Heard: Betty, how many issues are in the high school? Most likely meant: Betty, how many issues are in the hive queue?\n",
		"Andy: Betty, how many issues are in the high school?\n",
		"answer the reading most likely meant",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	if at, open := strings.Index(p, "Heard: Betty"), strings.Index(p, dataOpen); at < open || at > strings.Index(p, dataClose) {
		t.Fatalf("the heard line is data:\n%s", p)
	}
	// A room question (not addressed) is never corrected.
	q := answerPrompt(modeQuestion, "Bo", "how many issues are in the high school?", nil, nil, false)
	if strings.Contains(q, "Most likely meant") {
		t.Fatalf("a room question must not be corrected:\n%s", q)
	}
	// An unsure correction tells the model to ask.
	u := answerPrompt(modeAddressed, "Bo", "Betty, what's in the high school right now?", nil, nil, true)
	if !strings.Contains(u, "Reply with only this short question") || !strings.Contains(u, "Did you mean the hive queue?") {
		t.Fatalf("unsure prompt:\n%s", u)
	}
}

// An unsure correction gets the fixed clarifying question, whatever the
// model answered, and never a deep dive.
func TestUnsureCorrectionAsksInsteadOfAnswering(t *testing.T) {
	llm := &fakeLLM{reply: func(string, string) (string, error) {
		return `{"answer":"The high school has 900 students.","confidence":0.2,"needs_research":true}`, nil
	}}
	r := newRig(t, testSettings(), llm)
	r.e.d.CanResearch = true
	r.e.Observe(Line{Who: "Andy", Text: "Betty, what's in the high school right now?"})
	rep := r.reply()
	if rep.Text != "Did you mean the hive queue?" || rep.Research != "" || rep.NeedsResearch || rep.Kind != KindChat || rep.Trigger != TriggerAddressed {
		t.Fatalf("clarifying reply = %+v", rep)
	}
	if rep.Question != "Betty, what's in the high school right now?" {
		t.Fatalf("the reply keeps the line as heard: %q", rep.Question)
	}
}
