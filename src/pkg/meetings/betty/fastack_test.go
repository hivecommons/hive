package betty

import (
	"testing"
	"time"
)

func answerLLM() *fakeLLM {
	return &fakeLLM{reply: func(string, string) (string, error) {
		return `{"answer":"It ships Friday.","confidence":0.9}`, nil
	}}
}

// A partial transcript that already names her with a request gets the
// spoken ack at once; the final line then gets the answer and no second ack.
func TestPartialTranscriptAcksEarlyOnce(t *testing.T) {
	r := newRig(t, testSettings(), answerLLM())
	start := time.Now()
	r.e.ObservePartial(Line{Who: "Bo", Text: "Betty, when does the", Spoken: true})
	ack := r.reply()
	if ack.Kind != KindVoice || ack.Trigger != TriggerAck || time.Since(start) > time.Second {
		t.Fatalf("early ack %+v", ack)
	}
	// The same utterance growing does not ack again.
	r.e.ObservePartial(Line{Who: "Bo", Text: "Betty, when does the release", Spoken: true})
	r.none(200 * time.Millisecond)
	r.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?", Spoken: true})
	ans := r.reply()
	if ans.Trigger != TriggerAddressed || ans.Kind != KindVoice {
		t.Fatalf("answer after an early ack %+v", ans)
	}
	r.none(200 * time.Millisecond)
	// The next request gets its own ack again.
	r.e.Observe(Line{Who: "Bo", Text: "Betty, and the deploy window?", Spoken: true})
	if a := r.reply(); a.Trigger != TriggerAck {
		t.Fatalf("next request ack %+v", a)
	}
}

func TestPartialTranscriptNoAckCases(t *testing.T) {
	r := newRig(t, testSettings(), answerLLM())
	for _, l := range []Line{
		{Who: "Bo", Text: "Betty", Spoken: true},                      // may still become a hello
		{Who: "Bo", Text: "Betty, when", Spoken: true},                // too little of a request yet
		{Who: "Bo", Text: "when does the release ship", Spoken: true}, // not addressed
		{Who: "Bo", Text: "Betty, when does the release ship"},        // chat is never partial
		{Who: "Bo", Text: "Betty, who are you", Spoken: true},         // an intro waits for the final
	} {
		r.e.ObservePartial(l)
	}
	r.none(300 * time.Millisecond)

	noModel := newRig(t, testSettings(), nil)
	noModel.e.ObservePartial(Line{Who: "Bo", Text: "Betty, when does the release ship", Spoken: true})
	noModel.none(300 * time.Millisecond)
}

// An early ack whose final line did not address her does not swallow the
// ack of the speaker's next request.
func TestEarlyAckClearedByUnaddressedFinal(t *testing.T) {
	r := newRig(t, testSettings(), answerLLM())
	r.e.ObservePartial(Line{Who: "Bo", Text: "Betty, when does the", Spoken: true})
	if a := r.reply(); a.Trigger != TriggerAck {
		t.Fatalf("early ack %+v", a)
	}
	r.e.Observe(Line{Who: "Bo", Text: "pretty much done with the release notes", Spoken: true})
	r.none(200 * time.Millisecond)
	r.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?", Spoken: true})
	if a := r.reply(); a.Trigger != TriggerAck {
		t.Fatalf("ack after a cleared early ack %+v", a)
	}
}
