package betty

import (
	"testing"
	"time"
)

// The ack watchdog: a quick answer comes with no ack at all; a slow one
// gets exactly one ack, then the answer.
func TestAckWatchdog(t *testing.T) {
	const delay = 100 * time.Millisecond
	s := testSettings()
	s.AckDelay = delay

	fast := newRig(t, s, answerLLM())
	fast.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?", Spoken: true})
	if rep := fast.reply(); rep.Trigger != TriggerAddressed {
		t.Fatalf("a fast answer needs no ack first: %+v", rep)
	}
	fast.none(3 * delay)

	release := make(chan struct{})
	slow := newRig(t, s, &fakeLLM{reply: func(string, string) (string, error) {
		<-release
		return `{"answer":"It ships Friday.","confidence":0.9}`, nil
	}})
	slow.e.ObservePartial(Line{Who: "Bo", Text: "Betty, when does the release", Spoken: true})
	slow.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?", Spoken: true})
	if rep := slow.reply(); rep.Trigger != TriggerAck || rep.Kind != KindVoice {
		t.Fatalf("a slow answer gets one ack: %+v", rep)
	}
	slow.none(3 * delay)
	close(release)
	if rep := slow.reply(); rep.Trigger != TriggerAddressed {
		t.Fatalf("answer after the ack %+v", rep)
	}
}
