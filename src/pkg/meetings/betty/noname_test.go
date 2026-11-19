package betty

import (
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// A meeting with one speaker: nothing she says carries the speaker's name
// (owner ruling: "she does not need to respond with my name each time she
// responds"). Acks are exactly the pre-rendered ack lines, answers lose a
// name the model put in front, and a late answer opens with its topic.
func TestSingleSpeakerNeverHearsTheirName(t *testing.T) {
	const speaker = "Andy"
	r := newRig(t, testSettings(), nil)
	var late atomic.Bool
	r.llm = &fakeLLM{reply: func(_, prompt string) (string, error) {
		if strings.Contains(prompt, "Research summary") {
			return "The release ships Friday, as planned.", nil
		}
		if late.Load() {
			r.clk.add(attributeAfter + time.Second)
		}
		if strings.Contains(promptQuestion(prompt), "hear me") {
			return `{"answer":"Yes, Andy, I can hear you.","confidence":0.9}`, nil
		}
		return `{"answer":"Andy, it ships Friday.","confidence":0.9,"topic":"release date"}`, nil
	}}
	r.e.d.LLM = r.llm
	acks := phrasesFor(r.e.s.Persona.Language).acks
	var replies []Reply
	ask := func(text string, want string) {
		t.Helper()
		r.e.Observe(Line{Who: speaker, Text: text, Spoken: true})
		ack := r.reply()
		if ack.Trigger != TriggerAck || !slices.Contains(acks, ack.Text) {
			t.Fatalf("ack must be exactly one of the ack lines: %+v", ack)
		}
		ans := r.reply()
		if ans.Trigger != TriggerAddressed || ans.Text != want {
			t.Fatalf("answer to %q = %q, want %q", text, ans.Text, want)
		}
		replies = append(replies, ack, ans)
	}
	ask("Betty, when does the release ship?", "It ships Friday.")
	ask("Betty, can you hear me?", "Yes, I can hear you.")
	// A late answer says what it is about, still without the name.
	late.Store(true)
	ask("Betty, when is the deploy window?", "On release date: It ships Friday.")
	late.Store(false)
	// A follow-up after a deep dive.
	r.e.FollowUpIn("when does the release ship", "Friday.", true)
	replies = append(replies, r.reply())
	// The intro and a hello.
	r.e.Observe(Line{Who: speaker, Text: "Betty, who are you?"})
	replies = append(replies, r.reply())
	for _, rep := range replies {
		if strings.Contains(rep.Text, speaker) {
			t.Errorf("reply names the speaker: %+v", rep)
		}
	}
	// The prompts tell the model the same.
	if sys := SystemPrompt(r.e.s.Persona); !strings.Contains(sys, "never open a reply with their name") {
		t.Fatalf("system prompt:\n%s", sys)
	}
	if p := r.llm.last().prompt; strings.Contains(p, "Address people only by") {
		t.Fatalf("answer prompt still asks for names:\n%s", p)
	}
}

// Two people ask about the same thing: the topic cannot tell the answers
// apart, so each names who it is for.
func TestTwoSpeakersSameTopicAttributeByName(t *testing.T) {
	gates := map[string]chan struct{}{"Andy": make(chan struct{}), "Bo": make(chan struct{})}
	llm := &fakeLLM{reply: func(_, prompt string) (string, error) {
		for who, gate := range gates {
			if strings.Contains(promptQuestion(prompt), "Question from "+who) {
				<-gate
			}
		}
		return `{"answer":"It ships Friday.","confidence":0.9,"topic":"release date"}`, nil
	}}
	r := newRig(t, testSettings(), llm)
	r.e.Observe(Line{Who: "Andy Anderson", Text: "Betty, when does the release ship?"})
	r.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship again?"})
	if n := r.reply(); n.Trigger != TriggerQueue {
		t.Fatalf("queue note %+v", n)
	}
	close(gates["Bo"])
	if a := r.reply(); a.Text != "Bo (re: release date): It ships Friday." || !a.Attributed {
		t.Fatalf("first answer %+v", a)
	}
	// The other request has been answered and is gone; the transcript
	// still shows two people asked about it.
	close(gates["Andy"])
	if a := r.reply(); a.Text != "Andy Anderson (re: release date): It ships Friday." {
		t.Fatalf("second answer %+v", a)
	}
}

func TestStripAskerName(t *testing.T) {
	for _, c := range []struct{ who, text, want string }{
		{"Andy Anderson", "Andy, there are 12 open PRs.", "There are 12 open PRs."},
		{"Andy Anderson", "Andy Anderson: there are 12.", "There are 12."},
		{"Andy", "Yes, Andy, I can hear you.", "Yes, I can hear you."},
		{"Andy", "Here you go, Andy.", "Here you go."},
		{"Andy", "Bo, Andy and Cy own it.", "Bo, Andy and Cy own it."},
		{"Andy", "Andy owns the deploy pipeline.", "Andy owns the deploy pipeline."},
		{"", "Hello.", "Hello."},
	} {
		if got := stripAskerName(c.text, c.who); got != c.want {
			t.Errorf("stripAskerName(%q, %q) = %q, want %q", c.text, c.who, got, c.want)
		}
	}
}

// The fixed spoken lines are what the session pre-renders: the short
// intro first, the plain ack lines exactly as the meeting bot caches them,
// and every other fixed sentence, each short enough for the cache.
func TestFixedSpokenLines(t *testing.T) {
	p := config.MeetingsPersonaConfig{}.Resolve()
	lines := FixedSpokenLines(p)
	if len(lines) == 0 || lines[0] != p.SpokenIntro() {
		t.Fatalf("the spoken intro comes first: %q", lines)
	}
	for _, want := range []string{
		"Let me check on that.", "One moment.", "Good question, let me look.", "Let me see.",
		"Leaving now.", "Still looking into that; I'll report back as soon as I have it.",
		"I'll dig into that and report back.", "I've put the rest in chat.", "Did you mean the hive queue?",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("fixed spoken lines miss %q: %q", want, lines)
		}
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if seen[l] {
			t.Errorf("duplicate line %q", l)
		}
		seen[l] = true
		if n := len([]rune(l)); n > config.MeetingsSpokenLineMaxChars {
			t.Errorf("line is %d characters, the speech cache takes %d: %q", n, config.MeetingsSpokenLineMaxChars, l)
		}
	}
	es := FixedSpokenLines(config.MeetingsPersonaConfig{Preset: "melisa"}.Resolve())
	if !slices.Contains(es, "Un segundo.") || !slices.Contains(es, "El resto lo dejé en el chat.") {
		t.Fatalf("the lines follow the persona's language: %q", es)
	}
}

// Asked to search the web, she never says she cannot search from this
// answer: she answers from the results, says the search found nothing and
// offers to dig deeper, or says plainly that web search is down.
func TestAnswerPromptWebSearch(t *testing.T) {
	none := answerPrompt(modeAddressed, "Andy", "Betty, search the web for bee facts", nil, nil, true)
	for _, want := range []string{"answer from them and name the source", "Never say you cannot search the web", "returned nothing just now", "set needs_research to true"} {
		if !strings.Contains(none, want) {
			t.Errorf("prompt missing %q:\n%s", want, none)
		}
	}
	if strings.Contains(none, "web search is down right now") {
		t.Fatalf("no outage note without the unavailable source:\n%s", none)
	}
	down := answerPrompt(modeAddressed, "Andy", "Betty, search the web for bee facts",
		[]Snippet{{Source: WebUnavailableSource, Title: "Web search", Text: "The web search is unavailable right now."}}, nil, true)
	if !strings.Contains(down, "\""+WebUnavailableSource+"\"") || !strings.Contains(down, "web search is down right now") {
		t.Fatalf("outage prompt:\n%s", down)
	}
	if room := answerPrompt(modeQuestion, "Andy", "what do bees eat in winter?", nil, nil, false); strings.Contains(room, "search the web") {
		t.Fatalf("a room question gets no web search note:\n%s", room)
	}
}
