package config

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Every built-in persona's spoken intro fits the speech cache's line limit,
// names the persona, and is shorter than the full intro, which is unchanged.
func TestSpokenIntroFitsForEveryPreset(t *testing.T) {
	for _, pre := range OrderedMeetingsPersonaPresets() {
		for _, humor := range []string{MeetingsPersonaHumorLight, MeetingsPersonaHumorOff} {
			r := MeetingsPersonaConfig{Preset: pre.ID, Humor: humor}.Resolve()
			spoken, full := r.SpokenIntro(), r.Intro()
			n := utf8.RuneCountInString(spoken)
			if n == 0 || n > MeetingsSpokenLineMaxChars {
				t.Errorf("%s (%s): spoken intro is %d characters, want 1 to %d: %q", pre.ID, humor, n, MeetingsSpokenLineMaxChars, spoken)
			}
			if !strings.Contains(spoken, pre.DisplayName) {
				t.Errorf("%s: spoken intro must name the persona: %q", pre.ID, spoken)
			}
			if n >= utf8.RuneCountInString(full) {
				t.Errorf("%s: spoken intro must be shorter than the full intro", pre.ID)
			}
			if strings.Contains(spoken, "{") || strings.Contains(spoken, "(") {
				t.Errorf("%s: unrendered placeholder or pronouns: %q", pre.ID, spoken)
			}
		}
	}
}

func TestSpokenIntroDefaultAndFallbacks(t *testing.T) {
	r := MeetingsPersonaConfig{}.Resolve()
	want := "Hi, I'm Betty Bee from the hive. I take notes and answer questions from the hive. The details are in chat."
	if got := r.SpokenIntro(); got != want {
		t.Fatalf("default spoken intro = %q", got)
	}
	if !strings.Contains(r.Intro(), "keep this meeting's transcript") {
		t.Fatalf("the full intro is unchanged: %q", r.Intro())
	}

	// An operator's own short intro is spoken as written.
	short := MeetingsPersonaConfig{IntroTemplate: "Bee here."}.Resolve()
	if got := short.SpokenIntro(); got != "Bee here." {
		t.Fatalf("custom short intro = %q", got)
	}
	// A long one is spoken as the built-in short intro; chat keeps theirs.
	long := MeetingsPersonaConfig{IntroTemplate: strings.Repeat("I do a great many things. ", 8)}.Resolve()
	if got := long.SpokenIntro(); got != want {
		t.Fatalf("custom long intro speaks the built-in short one: %q", got)
	}
	// A language with no short template speaks the full intro.
	de := MeetingsPersonaConfig{Language: "de-DE"}.Resolve()
	if got := de.SpokenIntro(); got != de.Intro() {
		t.Fatalf("no short template: %q", got)
	}
	// A very long display name falls back to the short name.
	named := MeetingsPersonaConfig{DisplayName: strings.Repeat("Bee ", 15) + "Betty"}.Resolve()
	if got := named.SpokenIntro(); utf8.RuneCountInString(got) > MeetingsSpokenLineMaxChars || !strings.Contains(got, "I'm Betty.") {
		t.Fatalf("long display name: %q", got)
	}
}
