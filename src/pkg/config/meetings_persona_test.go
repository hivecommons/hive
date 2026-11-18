package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMeetingsPersonaDefaults(t *testing.T) {
	var p MeetingsPersonaConfig
	r := p.Resolve()
	if !r.Enabled || r.Preset != "betty" || r.Name != "Betty" || r.DisplayName != "Betty Bee from the hive" ||
		r.Pronouns != "she/her" || r.Language != "en-US" || r.Voice != "af_heart" || r.VoiceType != "female" ||
		r.Accent != "American" || r.Humor != "light" {
		t.Fatalf("defaults %+v", r)
	}
	if strings.Join(r.Aliases, ",") != "betty,betty bee,hive" || strings.Join(r.LeavePhrases, ",") != "betty leave,hive leave" {
		t.Fatalf("lists %+v %+v", r.Aliases, r.LeavePhrases)
	}
	u := r.Unprompted
	if u.Per10m != 2 || u.MinGapS != 90 || u.PerMeeting != 12 || u.AnswerWaitS != 8 || u.MinConfidence != 0.75 || u.QueueDepth != 3 || u.StaleS != 60 {
		t.Fatalf("caps %+v", u)
	}
	if r.ConsentText() != DefaultMeetingsConsentText {
		t.Fatalf("consent %q", r.ConsentText())
	}
	for _, want := range []string{"Betty", "transcript", "'betty leave'", "'hive leave'"} {
		if !strings.Contains(DefaultMeetingsConsentText, want) {
			t.Errorf("consent text missing %q", want)
		}
	}
	intro := r.Intro()
	for _, want := range []string{"Betty Bee from the hive", `"betty leave"`, "buzz"} {
		if !strings.Contains(intro, want) {
			t.Errorf("intro %q missing %q", intro, want)
		}
	}
	if strings.Contains(intro, "{") {
		t.Fatalf("unreplaced placeholder in %q", intro)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	m := MeetingsConfig{}
	if m.BotNameOrDefault() != "Betty Bee from the hive" || m.ConsentTextOrDefault() != DefaultMeetingsConsentText {
		t.Fatal("the persona sets the default bot name and consent text")
	}
}

func TestMeetingsPersonaDisabledAndOverrides(t *testing.T) {
	off := false
	p := MeetingsPersonaConfig{Enabled: &off}
	r := p.Resolve()
	if r.Enabled || r.Name != "hive" || strings.Join(r.LeavePhrases, ",") != "hive leave" || r.ConsentText() != DefaultMeetingsConsentTextNoPersona {
		t.Fatalf("disabled %+v", r)
	}
	if (MeetingsConfig{Persona: p}).BotNameOrDefault() != DefaultMeetingsBotName {
		t.Fatal("with the persona off the bot is hive (notes)")
	}
	if err := (MeetingsPersonaConfig{Enabled: &off, Humor: "loud"}).Validate(); err != nil {
		t.Fatal("a disabled persona is never rejected")
	}

	p = MeetingsPersonaConfig{
		Name: "Max", DisplayName: "Max from the hive", Aliases: []string{" Max ", "", "MAXIMUS"}, Pronouns: "he/him",
		Language: "en-GB", Voice: "bm_george", VoiceType: "Male", Accent: "British", Humor: "OFF",
		LeavePhrases: []string{"max leave"},
		Unprompted:   MeetingsPersonaUnprompted{Per10m: 1, MinGapS: 30, PerMeeting: 3, AnswerWaitS: 5, MinConfidence: 0.9, QueueDepth: 1, StaleS: 20},
	}
	r = p.Resolve()
	if r.Name != "Max" || strings.Join(r.Aliases, ",") != "max,maximus" || r.VoiceType != "male" || r.Humor != "off" || r.Unprompted.MinConfidence != 0.9 {
		t.Fatalf("overrides %+v", r)
	}
	if strings.Contains(r.Intro(), "buzz") || strings.Contains(r.Intro(), "he/him") || !strings.Contains(r.Intro(), "remove me") {
		t.Fatalf("humor off uses the plain intro: %q", r.Intro())
	}
	if !strings.Contains(r.ConsentText(), "'max leave'") || !strings.Contains(r.ConsentText(), "Max from the hive") {
		t.Fatal(r.ConsentText())
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.IntroTemplate = "I am {name}, {voice_type}, {accent}, {language}."
	if got := p.Resolve().Intro(); got != "I am Max, male, British, en-GB." {
		t.Fatal(got)
	}
}

func TestMeetingsPersonaEnv(t *testing.T) {
	env := map[string]string{
		MeetingsPersonaEnabledEnv: "true", MeetingsPersonaPresetEnv: "BETTY", MeetingsPersonaNameEnv: "Bee",
		MeetingsPersonaDisplayNameEnv: "Bee bot", MeetingsPersonaAliasesEnv: "bee, bumble",
		MeetingsPersonaPronounsEnv: "they/them", MeetingsPersonaLanguageEnv: "fr-CA", MeetingsPersonaVoiceEnv: "ff_siwis",
		MeetingsPersonaVoiceTypeEnv: "bot", MeetingsPersonaAccentEnv: "Quebec", MeetingsPersonaIntroTemplateEnv: "{name} here",
		MeetingsPersonaLeavePhrasesEnv: "bee leave", MeetingsPersonaHumorEnv: "off", MeetingsPersonaSearxngURLEnv: "http://searx.local/",
		MeetingsPersonaPer10mEnv: "4", MeetingsPersonaMinGapEnv: "10", MeetingsPersonaPerMeetingEnv: "5",
		MeetingsPersonaAnswerWaitEnv: "2", MeetingsPersonaMinConfEnv: "0.5", MeetingsPersonaQueueDepthEnv: "6",
		MeetingsPersonaStaleEnv: "7",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	p := MeetingsPersonaConfig{Name: "yaml", Aliases: []string{"yaml"}}
	r := p.Resolve()
	if r.Name != "Bee" || r.DisplayName != "Bee bot" || strings.Join(r.Aliases, ",") != "bee,bumble" || r.Pronouns != "they/them" ||
		r.Language != "fr-CA" || r.Voice != "ff_siwis" || r.VoiceType != "bot" || r.Accent != "Quebec" || r.Intro() != "Bee here" ||
		strings.Join(r.LeavePhrases, ",") != "bee leave" || r.Humor != "off" || r.SearxngURL != "http://searx.local" {
		t.Fatalf("env %+v", r)
	}
	u := r.Unprompted
	if u.Per10m != 4 || u.MinGapS != 10 || u.PerMeeting != 5 || u.AnswerWaitS != 2 || u.MinConfidence != 0.5 || u.QueueDepth != 6 || u.StaleS != 7 {
		t.Fatalf("env caps %+v", u)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MeetingsPersonaMinConfEnv, "nope")
	t.Setenv(MeetingsPersonaAliasesEnv, " , ")
	if r := (MeetingsPersonaConfig{Unprompted: MeetingsPersonaUnprompted{MinConfidence: 0.6}}).Resolve(); r.Unprompted.MinConfidence != 0.6 || strings.Join(r.Aliases, ",") != "betty,betty bee,hive" {
		t.Fatalf("invalid env falls back: %+v", r)
	}
	t.Setenv(MeetingsPersonaEnabledEnv, "off")
	if (MeetingsPersonaConfig{}).IsEnabled() {
		t.Fatal("env turns the persona off")
	}
}

func TestMeetingsPersonaValidate(t *testing.T) {
	long := strings.Repeat("x", meetingsMaxBotNameChars+1)
	cases := []struct {
		name string
		p    MeetingsPersonaConfig
		want string
	}{
		{"preset", MeetingsPersonaConfig{Preset: "nobody"}, "preset"},
		{"language", MeetingsPersonaConfig{Language: "english please"}, "language"},
		{"display", MeetingsPersonaConfig{DisplayName: long}, "display_name"},
		{"intro", MeetingsPersonaConfig{IntroTemplate: strings.Repeat("x", meetingsMaxIntroChars+1)}, "intro_template"},
		{"humor", MeetingsPersonaConfig{Humor: "loud"}, "humor"},
		{"voice type", MeetingsPersonaConfig{VoiceType: "robot"}, "voice_type"},
		{"confidence", MeetingsPersonaConfig{Unprompted: MeetingsPersonaUnprompted{MinConfidence: 1.5}}, "min_confidence"},
		{"negative", MeetingsPersonaConfig{Unprompted: MeetingsPersonaUnprompted{StaleS: -1}}, "stale_s"},
		{"searxng", MeetingsPersonaConfig{SearxngURL: "searx"}, "searxng_url"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.p.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate = %v, want %q", err, c.want)
			}
		})
	}
	m := MeetingsConfig{Enabled: true, Persona: MeetingsPersonaConfig{Humor: "loud"}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "humor") {
		t.Fatalf("meetings.Validate must check the persona: %v", err)
	}
}

func TestMeetingsPersonaYAML(t *testing.T) {
	src := `
enabled: false
preset: betty
name: B
display_name: D
aliases: [a, b]
pronouns: she/her
language: en-US
voice: af_heart
voice_type: female
accent: American
intro_template: hi
leave_phrases: [b leave]
humor: off
searxng_url: http://s
unprompted: {per_10m: 1, min_gap_s: 2, per_meeting: 3, answer_wait_s: 4, min_confidence: 0.5, queue_depth: 6, stale_s: 7}
`
	var p MeetingsPersonaConfig
	if err := yaml.Unmarshal([]byte(src), &p); err != nil {
		t.Fatal(err)
	}
	if p.Enabled == nil || *p.Enabled || p.Name != "B" || p.DisplayName != "D" || len(p.Aliases) != 2 || p.VoiceType != "female" ||
		p.Unprompted.MinConfidence != 0.5 || p.Unprompted.StaleS != 7 || p.SearxngURL != "http://s" || p.LeavePhrases[0] != "b leave" {
		t.Fatalf("yaml %+v", p)
	}
}

// Operator ruling after the live Meet test: the persona never states
// pronouns. No preset's intro, plain intro or sample line, with humor on
// or off, carries a pronoun parenthetical, and a custom template that
// spells one out has it dropped.
func TestPersonaNeverStatesPronouns(t *testing.T) {
	off := MeetingsPersonaHumorOff
	for id := range MeetingsPersonaPresets {
		for _, humor := range []string{"", off} {
			r := MeetingsPersonaConfig{Preset: id, Humor: humor}.Resolve()
			intro := r.Intro()
			for _, bad := range []string{"(" + r.Pronouns, "（" + r.Pronouns, "()", "（）", "{pronouns}"} {
				if strings.Contains(intro, bad) {
					t.Errorf("preset %s humor %q intro states %q: %s", id, humor, bad, intro)
				}
			}
		}
	}
	custom := MeetingsPersonaConfig{IntroTemplate: "Hi, I'm {display_name} (she/her), or ({pronouns})."}.Resolve()
	if got := custom.Intro(); strings.Contains(got, "she") || strings.Contains(got, "(") {
		t.Fatalf("custom intro %q", got)
	}
	if got := StripPronouns("I'm Ana (they/them)."); got != "I'm Ana." {
		t.Fatalf("StripPronouns = %q", got)
	}
}
