package betty

import (
	"testing"
	"time"
)

// tones drains the replies that arrive within d and counts the listening
// tone requests among them.
func (r *rig) tones(d time.Duration) (tones int, others []Reply) {
	r.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case rep := <-r.e.Replies():
			if rep.Trigger == TriggerTone {
				tones++
				if rep.Text != "" || rep.Kind != KindVoice || !rep.VoiceWanted {
					r.t.Fatalf("a tone request carries no text and is a voice reply: %+v", rep)
				}
			} else {
				others = append(others, rep)
			}
		case <-deadline:
			return tones, others
		}
	}
}

func toneSettings(on bool) Settings {
	s := testSettings()
	s.ListeningTone = on
	return s
}

// A spoken request that addresses her gets the tone first, then the
// acknowledgement and the answer as before.
func TestListeningToneOnAddressedSpokenRequest(t *testing.T) {
	r := newRig(t, toneSettings(true), answerLLM())
	r.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?", Spoken: true})
	first := r.reply()
	if first.Trigger != TriggerTone || first.Asker != "Bo" {
		t.Fatalf("the first reply is the tone for the asker: %+v", first)
	}
	tones, others := r.tones(time.Second)
	if tones != 0 {
		t.Fatalf("one tone per request, got %d more", tones)
	}
	var answered bool
	for _, o := range others {
		answered = answered || o.Trigger == TriggerAddressed
	}
	if !answered {
		t.Fatalf("the answer still comes: %+v", others)
	}
}

// Lines she does not answer by voice get no tone: not addressed to her,
// typed in chat, or the setting off.
func TestListeningToneNotOnUnaddressedChatOrOff(t *testing.T) {
	r := newRig(t, toneSettings(true), answerLLM())
	r.e.Observe(Line{Who: "Bo", Text: "when does the release ship?", Spoken: true})
	r.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?", Spoken: false})
	if n, _ := r.tones(500 * time.Millisecond); n != 0 {
		t.Fatalf("no tone for unaddressed or chat lines, got %d", n)
	}

	off := newRig(t, toneSettings(false), answerLLM())
	off.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?", Spoken: true})
	if n, _ := off.tones(500 * time.Millisecond); n != 0 {
		t.Fatalf("no tone with the setting off, got %d", n)
	}
}

// A request she drops (the meeting budget is spent) is not answered, so it
// gets no tone.
func TestListeningToneNotWhenBudgetSkipsRequest(t *testing.T) {
	r := newRig(t, toneSettings(true), answerLLM())
	r.e.exhausted.Store(true)
	r.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?", Spoken: true})
	if n, _ := r.tones(500 * time.Millisecond); n != 0 {
		t.Fatalf("no tone for a request she will not answer, got %d", n)
	}
}

// Two requests within toneMinGap chime once.
func TestListeningToneMinGap(t *testing.T) {
	r := newRig(t, toneSettings(true), answerLLM())
	r.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?", Spoken: true})
	r.e.Observe(Line{Who: "Bo", Text: "Betty, what is the deploy window?", Spoken: true})
	if n, _ := r.tones(time.Second); n != 1 {
		t.Fatalf("one chime for two quick requests, got %d", n)
	}
}
