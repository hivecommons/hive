package betty

import (
	"testing"
	"time"
)

// Every reply path carries a timing, not only model answers (live Discord
// test: a slow stock intro left no record): the intro, a hello and an
// acknowledgement are heard and ready at once, from a fixed line.
func TestEveryReplyCarriesATiming(t *testing.T) {
	r := newRig(t, testSettings(), &fakeLLM{})
	heard := r.clk.Now()
	r.e.Observe(Line{Who: "Ana", Text: "Betty, introduce yourself", Spoken: true})
	intro := r.reply()
	if intro.Trigger != TriggerIntro || intro.Timing == nil || !intro.Timing.HeardAt.Equal(heard) || intro.Timing.Source != SourceFixed ||
		intro.Timing.AnswerMS != 0 || intro.Timing.OverBudget {
		t.Fatalf("intro timing %+v", intro.Timing)
	}
	r.e.Observe(Line{Who: "Bo", Text: "hey betty"})
	if hello := r.reply(); hello.Timing == nil || hello.Timing.Source != SourceFixed {
		t.Fatalf("hello timing %+v", hello.Timing)
	}
	r.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?", Spoken: true})
	if ack := r.reply(); ack.Trigger != TriggerAck || ack.Timing == nil || ack.Timing.Source != SourceFixed || ack.Timing.HeardAt.IsZero() {
		t.Fatalf("ack timing %+v", ack)
	}
	if ans := r.reply(); ans.Trigger != TriggerAddressed || ans.Timing == nil || ans.Timing.Source != SourceRetrieval {
		t.Fatalf("answer timing %+v", ans.Timing)
	}
	r.e.FollowUpIn("how does cert rotation work", "cert-manager renews it nightly.", false)
	if fu := r.reply(); fu.Trigger != TriggerFollowUp || fu.Timing == nil || fu.Timing.Source != SourceResearch {
		t.Fatalf("follow-up timing %+v", fu.Timing)
	}
}

// Asked by voice the intro carries a short spoken form beside the full
// text (the session says the short one and posts the full one once);
// asked in chat it is the full text only.
func TestIntroSpokenFormOnlyByVoice(t *testing.T) {
	r := newRig(t, testSettings(), &fakeLLM{})
	r.e.Observe(Line{Who: "Ana", Text: "Betty, introduce yourself", Spoken: true})
	voice := r.reply()
	if voice.Kind != KindVoice || voice.Spoken != r.e.s.Persona.SpokenIntro() || voice.Text != r.e.s.Persona.Intro() || voice.Spoken == voice.Text {
		t.Fatalf("spoken intro %+v", voice)
	}
	r.e.Observe(Line{Who: "Bo", Text: "who's the bot?"})
	if chat := r.reply(); chat.Kind != KindChat || chat.Spoken != "" || chat.Text != r.e.s.Persona.Intro() {
		t.Fatalf("chat intro %+v", chat)
	}
}

// A web hit is named as the answer's source, apart from the hive's own
// knowledge.
func TestTimingSourceTellsWebFromKnowledge(t *testing.T) {
	web := newRig(t, testSettings(), &fakeLLM{}, fakeRetriever{hits: []Snippet{{Source: webSource + " example.com", Title: "Release", Text: "The release ships Friday."}}})
	web.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?"})
	if tm := web.reply().Timing; tm == nil || tm.Source != SourceWeb {
		t.Fatalf("web source %+v", tm)
	}
	if !hasWeb([]Snippet{{Source: webSource}}) || hasWeb([]Snippet{{Source: "knowledge"}, {Source: "webinar notes"}}) {
		t.Fatal("hasWeb")
	}
}

// The ack watchdog is source-aware (live Meet test: an answer ready 2.6 s
// after the request was held back about 5 s by "let me check on that"): a
// question about the hive's own state, expected to be fast, gets no ack
// until fastAckDelayFactor times the delay; any other question gets one
// at the delay.
func TestAckDelayIsLongerForFastSources(t *testing.T) {
	const delay = 200 * time.Millisecond
	s := testSettings()
	s.AckDelay = delay
	release := make(chan struct{})
	llm := &fakeLLM{reply: func(string, string) (string, error) {
		<-release
		return `{"answer":"40 open issues.","confidence":0.9}`, nil
	}}
	live := fakeRetriever{hits: []Snippet{{Source: HiveStatusSource, Title: "The hive right now", Text: "Open now: 40 issues."}}}
	fast := newRig(t, s, llm, live)
	fast.e.Observe(Line{Who: "Bo", Text: "Betty, how many open issues are there?", Spoken: true})
	// Past the plain delay, short of the widened one: still no ack.
	fast.none(delay + delay/2)
	close(release)
	if rep := fast.reply(); rep.Trigger != TriggerAddressed {
		t.Fatalf("a hive-status answer inside the widened delay needs no ack: %+v", rep)
	}
	fast.none(delay)

	held := make(chan struct{})
	defer close(held)
	slow := newRig(t, s, &fakeLLM{reply: func(string, string) (string, error) {
		<-held
		return `{"answer":"It ships Friday.","confidence":0.9}`, nil
	}})
	slow.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?", Spoken: true})
	if rep := slow.reply(); rep.Trigger != TriggerAck {
		t.Fatalf("a slow answer to any other question is acknowledged: %+v", rep)
	}
}

// The default delay keeps a normal 1 to 2.6 s answer free of an ack.
func TestDefaultAckDelayIsPastANormalAnswer(t *testing.T) {
	const normalAnswer = 2600 * time.Millisecond
	if DefaultAckDelay <= normalAnswer {
		t.Fatalf("DefaultAckDelay %v would acknowledge a %v answer", DefaultAckDelay, normalAnswer)
	}
	if DefaultAckDelay*fastAckDelayFactor <= HiveStatusBudget {
		t.Fatalf("a hive-status answer inside its %v budget would be acknowledged", HiveStatusBudget)
	}
}
