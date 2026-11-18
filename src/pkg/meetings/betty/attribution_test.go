package betty

import (
	"strings"
	"testing"
	"time"
)

// gatedLLM answers each request once its topic's gate is released, with a
// model topic label, so a test controls which answer is ready first.
func gatedLLM(gates map[string]chan struct{}, topics map[string]string) *fakeLLM {
	return &fakeLLM{reply: func(_, prompt string) (string, error) {
		for key, gate := range gates {
			if strings.Contains(prompt, "Question from") && strings.Contains(promptQuestion(prompt), key) {
				<-gate
				return `{"answer":"Answer about ` + key + `.","confidence":0.9,"sources":[1],"topic":"` + topics[key] + `"}`, nil
			}
		}
		return `{"answer":"Other.","confidence":0.9}`, nil
	}}
}

// releaseSource is the citation a chat answer from gatedLLM ends with: its
// source 1 is the rig's "Release" knowledge fact (cite.go).
const releaseSource = ` (Source: knowledge "Release")`

// promptQuestion is the "Question from" line of a prompt.
func promptQuestion(prompt string) string {
	i := strings.Index(prompt, "Question from")
	if i < 0 {
		return ""
	}
	rest := prompt[i:]
	if j := strings.IndexByte(rest, '\n'); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// Two requests in flight: a single queue note, and each answer, whichever
// is ready first, says what it is about. The topics differ, so no name.
func TestConcurrentRequestsAreAttributed(t *testing.T) {
	gates := map[string]chan struct{}{"release": make(chan struct{}), "deploy": make(chan struct{})}
	r := newRig(t, testSettings(), gatedLLM(gates, map[string]string{"release": "release date", "deploy": "deploy window"}))
	r.e.Observe(Line{Who: "Andy Anderson", Text: "Betty, when does the release ship?"})
	r.e.Observe(Line{Who: "Bo", Text: "Betty, what is the deploy window?"})
	if n := r.reply(); n.Trigger != TriggerQueue || n.Text != "I have 2 questions; answering each as it's ready." || n.Kind != KindChat {
		t.Fatalf("queue note %+v", n)
	}
	// The second request is ready first.
	close(gates["deploy"])
	if a := r.reply(); a.Text != "On deploy window: Answer about deploy."+releaseSource {
		t.Fatalf("first answer %q", a.Text)
	}
	close(gates["release"])
	if a := r.reply(); a.Text != "On release date: Answer about release."+releaseSource {
		t.Fatalf("second answer %q", a.Text)
	}
	// A third request within the minute gets no second note.
	r.e.Observe(Line{Who: "Cy", Text: "Betty, what is the plan?"})
	r.e.Observe(Line{Who: "Di", Text: "Betty, what is the status?"})
	for range 2 {
		if a := r.reply(); a.Trigger == TriggerQueue {
			t.Fatalf("a second queue note within a minute: %+v", a)
		}
	}
}

// One request answered at once stays short; one answered late says what
// it is about even alone, without the asker's name.
func TestSingleRequestPrefixOnlyWhenLate(t *testing.T) {
	r := newRig(t, testSettings(), nil)
	r.llm = &fakeLLM{reply: func(string, string) (string, error) {
		r.clk.add(attributeAfter + time.Second)
		return `{"answer":"It ships Friday.","confidence":0.9}`, nil
	}}
	r.e.d.LLM = r.llm
	r.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?"})
	if a := r.reply(); a.Text != "On release ship: It ships Friday." {
		t.Fatalf("late answer %q", a.Text)
	}
}

// Spoken requests: an ack is exactly one of the plain ack lines (the
// meeting bot pre-renders them) even with another request in flight, and
// spoken answers on different topics open with the topic, not a name.
func TestSpokenAcksAndAnswersCarryNoName(t *testing.T) {
	gates := map[string]chan struct{}{"release": make(chan struct{}), "deploy": make(chan struct{})}
	r := newRig(t, testSettings(), gatedLLM(gates, map[string]string{"release": "the release", "deploy": "the deploy"}))
	r.e.Observe(Line{Who: "Andy Anderson", Text: "Betty, when does the release ship?", Spoken: true})
	if a := r.reply(); a.Trigger != TriggerAck || a.Text != "Let me check on that." {
		t.Fatalf("first ack %+v", a)
	}
	r.e.Observe(Line{Who: "Bo Li", Text: "Betty, what is the deploy window?", Spoken: true})
	if a := r.reply(); a.Trigger != TriggerAck || a.Text != "One moment." {
		t.Fatalf("second ack %+v", a)
	}
	if n := r.reply(); n.Trigger != TriggerQueue {
		t.Fatalf("queue note %+v", n)
	}
	close(gates["release"])
	if a := r.reply(); a.Kind != KindVoice || a.Text != "On the release: Answer about release." {
		t.Fatalf("spoken answer %+v", a)
	}
	close(gates["deploy"])
	if a := r.reply(); a.Text != "On the deploy: Answer about deploy." {
		t.Fatalf("spoken answer %+v", a)
	}
}

// Another person speaking between the request and its answer is enough to
// say what the answer is about.
func TestAnswerAfterOtherLinesIsAttributed(t *testing.T) {
	gate := map[string]chan struct{}{"release": make(chan struct{})}
	r := newRig(t, testSettings(), gatedLLM(gate, map[string]string{"release": ""}))
	r.e.Observe(Line{Who: "Bo", Text: "Betty, what PRs touch the release?"})
	waitCalls(t, r.llm, 1)
	r.clk.add(time.Second)
	r.e.Observe(Line{Who: "Ana", Text: "meanwhile the build is green"})
	time.Sleep(50 * time.Millisecond)
	close(gate["release"])
	// No model topic: the request's own words, in the casing used.
	if a := r.reply(); a.Text != "On PRs touch the release: Answer about release."+releaseSource {
		t.Fatalf("answer %q", a.Text)
	}
}

func waitCalls(t *testing.T, f *fakeLLM, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.n() < n {
		if time.Now().After(deadline) {
			t.Fatalf("model called %d times, want %d", f.n(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTopicPhrase(t *testing.T) {
	for in, want := range map[string]string{
		"how many open issues are there?":              "open issues",
		"what PRs are open in the hive repo":           "PRs are open",
		"can you tell me the deploy window":            "deploy window",
		"what's blocking the release on Friday":        "blocking the release on Friday",
		"what?":                                        "",
		"give us a quick summary of the cert rotation": "summary of the cert rotation",
	} {
		if got := topicPhrase(in); got != want {
			t.Errorf("topicPhrase(%q) = %q, want %q", in, got, want)
		}
	}
	if got := recase("Betty, what PRs are open?", "what prs are open"); got != "what PRs are open" {
		t.Fatalf("recase = %q", got)
	}
	if got := cleanTopic(`"@everyone open PRs today and tomorrow and more."`); got != "everyone open PRs today and tomorrow" {
		t.Fatalf("cleanTopic = %q", got)
	}
}

func TestDisplayNameNeverMentions(t *testing.T) {
	for _, c := range []struct {
		who    string
		spoken bool
		want   string
	}{
		{"Andy Anderson", false, "Andy Anderson"},
		{"Andy Anderson", true, "Andy"},
		{"@Andy", false, "Andy"},
		{"<@1234>", false, "1234"},
		{"  ", false, ""},
	} {
		if got := displayName(c.who, c.spoken); got != c.want {
			t.Errorf("displayName(%q, %v) = %q, want %q", c.who, c.spoken, got, c.want)
		}
	}
	if got := attribute("en", false, "Andy", "", "Hello."); got != "Hello." {
		t.Fatalf("no topic, no prefix: %q", got)
	}
	if got := attribute("en", true, "Andy", "open PRs", "Twelve."); got != "On open PRs: Twelve." {
		t.Fatalf("topic first, no name: %q", got)
	}
	if got := attributeNamed("en", false, "", "x", "Hello."); got != "On x: Hello." {
		t.Fatalf("no name: %q", got)
	}
	if got := attributeNamed("de", true, "Andy A", "", "Ja."); got != "Andy, Ja." {
		t.Fatalf("voice bare: %q", got)
	}
	if got := attributeNamed("en", false, "<@Andy A>", "open PRs", "Twelve."); got != "Andy A (re: open PRs): Twelve." {
		t.Fatalf("named: %q", got)
	}
}

func TestParseAnswerTopic(t *testing.T) {
	a, err := ParseAnswer(`{"answer":"x","confidence":1,"topic":"  open PRs. "}`)
	if err != nil || a.Topic != "open PRs" {
		t.Fatalf("%+v %v", a, err)
	}
}

// A late answer must say which question it answers even when the request
// holds no topic word ("Betty, are you there?").
func TestReplyTopicFallsBackToTheRequest(t *testing.T) {
	m := NewMatcher("Betty", nil)
	l := Line{Who: "Andy", Text: "Betty, are you there?", Spoken: true}
	p := &pending{line: l, trig: m.Match(l.Text, true), addressed: true}
	if got := replyTopic(p, ""); got == "" || !strings.Contains(got, "there") {
		t.Fatalf("replyTopic = %q", got)
	}
	if got := replyTopic(p, "board pressure"); got != "board pressure" {
		t.Fatalf("model topic wins: %q", got)
	}
	e := New(testSettings(), Deps{})
	rep := Reply{Text: "Yes, I can hear you.", VoiceWanted: true}
	p.overlapped = true
	e.attributeReply(p, &rep, "", time.Now())
	// The asker and topic are recorded for a late delivery; her own text
	// carries no name.
	if rep.Asker != "Andy" || rep.Topic == "" || rep.Text != "Yes, I can hear you." {
		t.Fatalf("attributed reply = %+v", rep)
	}
}
