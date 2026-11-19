package betty

import (
	"testing"
	"time"
)

// Meet test: "Betty, please introduce yourself" said twice (the first
// answer was slow) got a second intro and the research ack "Let me check
// on that." over the first intro.
func TestRepeatedSpokenIntroIsDropped(t *testing.T) {
	r := newRig(t, testSettings(), answerLLM())
	r.e.Observe(Line{Who: "Andy", Text: "Betty, please introduce yourself.", Spoken: true})
	if rep := r.reply(); rep.Trigger != TriggerIntro {
		t.Fatalf("first intro %+v", rep)
	}
	r.clk.add(39 * time.Second)
	// The second ask, partial then final: no ack, no second intro.
	r.e.ObservePartial(Line{Who: "Andy", Text: "Betty, please introduce", Spoken: true})
	r.e.Observe(Line{Who: "Andy", Text: "Betty, please introduce yourself.", Spoken: true})
	r.none(300 * time.Millisecond)
	r.waitDrop("repeated_request")
	// Past the window it is a fresh request.
	r.clk.add(repeatWindow)
	r.e.Observe(Line{Who: "Andy", Text: "Betty, who are you?", Spoken: true})
	if rep := r.reply(); rep.Trigger != TriggerIntro {
		t.Fatalf("intro after the window %+v", rep)
	}
}

// An intro still being said never gets the research ack.
func TestPartialIntroGetsNoAck(t *testing.T) {
	r := newRig(t, testSettings(), answerLLM())
	for _, text := range []string{"Betty, please introduce", "Betty, can you introduce", "Betty, tell us about yourself"} {
		r.e.ObservePartial(Line{Who: "Bo", Text: text, Spoken: true})
	}
	r.none(300 * time.Millisecond)
	if !mayBecomeIntro("please introduce") || mayBecomeIntro("when does the release ship") {
		t.Fatal("mayBecomeIntro")
	}
}

func TestRepeatedRequestKeys(t *testing.T) {
	e := New(testSettings(), Deps{})
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if e.repeatedRequest("Bo", "what is the bp", now) {
		t.Fatal("first ask is not a repeat")
	}
	if !e.repeatedRequest("bo", "what is the bp", now.Add(10*time.Second)) {
		t.Fatal("same speaker, same request: a repeat")
	}
	if e.repeatedRequest("Ana", "what is the bp", now.Add(10*time.Second)) {
		t.Fatal("another speaker is not a repeat")
	}
	if !e.repeatPending("Bo", "what is", now.Add(20*time.Second)) || e.repeatPending("Bo", "how many", now.Add(20*time.Second)) {
		t.Fatal("repeatPending")
	}
}
