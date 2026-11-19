package betty

import (
	"strings"
	"testing"
)

func TestSoundKey(t *testing.T) {
	for word, want := range map[string]string{
		"betty": "edi", "eddie": "edi", "ready": "edi", "betty's": "edi", "beddy": "edi", "betsy": "edsi", "kelly": "eli",
	} {
		if got := soundKey(word); got != want {
			t.Errorf("soundKey(%q) = %q, want %q", word, got, want)
		}
	}
	if indelDistance("edsi", "edi") != 1 || indelDistance("eli", "edi") != 2 || indelDistance("", "ab") != 2 {
		t.Fatal("indelDistance")
	}
}

// The live Discord test: STT heard her name as Eddie and as "ready".
func TestMisheardWakeWord(t *testing.T) {
	m := NewMatcher("Betty", []string{"betty bee", "hive"})
	cases := []struct {
		text         string
		spoken       bool
		participants []string
		addr, intro  bool
		rest         string
	}{
		{"Eddie, please introduce yourself.", true, []string{"Andy"}, true, true, "please introduce yourself"},
		{"ready, please introduce yourself.", true, []string{"Andy"}, true, true, "please introduce yourself"},
		{"Betsy, can you check the board?", true, nil, true, false, "can you check the board"},
		{"Betty's, are you there?", true, nil, true, false, "are you there"},
		{"hey Eddie, what's open?", true, nil, true, false, "what's open"},
		// A real Eddie in the meeting is spoken to, not mistaken for her.
		{"Eddie, please introduce yourself.", true, []string{"Andy", "Eddie Vance"}, false, false, ""},
		// Chat is typed, never misheard.
		{"Eddie, please introduce yourself.", false, nil, false, false, ""},
		// Without a request to her it is ordinary speech.
		{"ready when you are", true, nil, false, false, ""},
		{"ready, can we start?", true, nil, false, false, ""},
		{"Eddie, the floor is yours", true, nil, false, false, ""},
		{"Kelly, please share your screen", true, nil, false, false, ""},
		{"daddy, please come here", true, nil, false, false, ""},
	}
	for _, c := range cases {
		got := m.MatchWith(c.text, c.spoken, c.participants)
		if got.Addressed != c.addr || got.Intro != c.intro || (c.addr && (got.Rest != c.rest || !got.Misheard)) {
			t.Errorf("MatchWith(%q, %v, %v) = %+v", c.text, c.spoken, c.participants, got)
		}
	}
	// An exact name never reports misheard.
	if got := m.MatchWith("Betty, introduce yourself", true, nil); !got.Addressed || got.Misheard {
		t.Fatalf("exact name = %+v", got)
	}
	// A strict everyday alias is never matched loosely.
	if got := m.MatchWith("five, please introduce yourself", true, nil); got.Addressed {
		t.Fatalf("strict alias matched loosely: %+v", got)
	}
}

func TestEngineMisheardIntro(t *testing.T) {
	r := newRig(t, testSettings(), &fakeLLM{})
	r.e.Observe(Line{Who: "Andy", Text: "Eddie, please introduce yourself.", Spoken: true})
	if rep := r.reply(); rep.Trigger != TriggerIntro {
		t.Fatalf("misheard intro must still be answered: %+v", rep)
	}
}

// Never invent a participant: the prompt names the people heard and says
// a sound-alike of her name is her.
func TestAnswerPromptNamesParticipants(t *testing.T) {
	p := answerPrompt(modeAddressed, "Andy", "Eddie, the floor?", nil, []recentLine{{Who: "Andy", Text: "Eddie, please introduce yourself."}, {Who: "Bo", Text: "hi"}, {Who: "Andy", Text: "x"}}, true)
	if !strings.Contains(p, "People in the meeting: Andy, Bo\n") || !strings.Contains(p, "never a new participant") {
		t.Fatal(p)
	}
}
