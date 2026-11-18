package meetings

import "testing"

func TestSpeechText(t *testing.T) {
	for in, want := range map[string]string{
		"Hi, I'm Betty Bee from the hive (she/her). I take notes.":         "Hi, I'm Betty Bee from the hive. I take notes.",
		"Board pressure is 40/12 open issues/PRs.":                         "Board pressure is 40 and 12 open issues or PRs.",
		"See #18677 and hivecommons/hive#18679.":                           "See number 18677 and number 18679.",
		"Docs at https://example.com/docs?x=1 and [the RFC](https://x.y).": "Docs at a link and the RFC.",
		"**Done**: run `make dashboard-gen` 🎉 now":                         "Done: run make dashboard-gen now",
		"- first\n- second":         "first second",
		"## Heading\nText":          "Heading Text",
		"They (they/them) left 👍🏽.": "They left.",
		"plain words stay":          "plain words stay",
	} {
		if got := SpeechText(in); got != want {
			t.Errorf("SpeechText(%q) = %q, want %q", in, got, want)
		}
	}
}

// What hive says goes through SpeechText; the chat copy is untouched.
func TestSayUsesSpeechText(t *testing.T) {
	q := &confirmingQuorum{fakeQuorum: newFakeQuorum()}
	m, mt := liveTeams(t, q, true)
	eventually(t, "consent", func() bool { got, _ := m.Get(mt.ID); return got.ConsentPostedAt != nil })
	if _, err := m.Say(t.Context(), mt.ID, "Hi, I'm Betty (she/her), see #12."); err != nil {
		t.Fatal(err)
	}
	if s := q.says(); len(s) != 1 || s[0] != "Hi, I'm Betty, see number 12." {
		t.Fatalf("says %q", s)
	}
}

// Scored live test: "Betty leave" was transcribed "Betty Lee." and she
// stayed. A line that is only the name and a sound-alike of "leave" asks
// her to leave; longer lines still need the phrase itself.
func TestLeaveMatcherMisheard(t *testing.T) {
	l := NewLeaveMatcher([]string{"betty leave", "hive leave"})
	for text, want := range map[string]bool{
		"Betty Lee.":             true,
		"betty, leaf":            true,
		"Betty please leave":     true,
		"Hive lead!":             true,
		"Betty leave":            true,
		"Betty, Lee is here":     false,
		"Lee, can you share":     false,
		"Betty leaves the room?": false,
		"we should leave":        false,
	} {
		if got := l.Match(text); got != want {
			t.Errorf("Match(%q) = %v, want %v", text, got, want)
		}
	}
}
