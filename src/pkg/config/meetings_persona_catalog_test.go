package config

import (
	"strings"
	"testing"
	"unicode"
)

// Every preset must be a persona Validate accepts, speak with a voice the
// self-hosted Kokoro ships in the preset's own language and voice type, and
// carry an intro, a plain intro and a sample line in that language.
func TestMeetingsPersonaPresetsAreKokoroBacked(t *testing.T) {
	voices := map[string]MeetingsTTSVoice{}
	for _, v := range KokoroVoices() {
		voices[v.ID] = v
	}
	if len(voices) != len(kokoroVoiceIDs) {
		t.Fatalf("catalog has %d valid voices of %d ids", len(voices), len(kokoroVoiceIDs))
	}
	languages := map[string]bool{}
	for id, p := range MeetingsPersonaPresets {
		if p.ID != id {
			t.Errorf("preset key %q has ID %q", id, p.ID)
		}
		v, ok := voices[p.Voice]
		if !ok {
			t.Errorf("%s: voice %q is not a Kokoro voice", id, p.Voice)
			continue
		}
		if v.Language != p.Language || v.VoiceType != p.VoiceType {
			t.Errorf("%s: voice %s speaks %s %s, preset says %s %s", id, p.Voice, v.Language, v.VoiceType, p.Language, p.VoiceType)
		}
		languages[p.Language] = true
		if err := (MeetingsPersonaConfig{Preset: id}).Validate(); err != nil {
			t.Errorf("%s: %v", id, err)
		}
		for name, tmpl := range map[string]string{"intro": p.IntroTemplate, "plain intro": p.PlainIntroTemplate} {
			if !strings.Contains(tmpl, "{display_name}") || !strings.Contains(tmpl, "{leave}") {
				t.Errorf("%s: %s %q must name the persona and the leave phrase", id, name, tmpl)
			}
			if n := len([]rune(tmpl)); n > meetingsMaxIntroChars {
				t.Errorf("%s: %s is %d characters", id, name, n)
			}
		}
		if !strings.Contains(p.SampleLine, "{display_name}") {
			t.Errorf("%s: sample line %q", id, p.SampleLine)
		}
		for _, l := range p.LeavePhrases {
			for _, r := range l {
				if r > unicode.MaxASCII {
					t.Errorf("%s: leave phrase %q must be ASCII (the leave matcher uses ASCII word boundaries)", id, l)
					break
				}
			}
		}
		if p.LeavePhrases[len(p.LeavePhrases)-1] != "hive leave" || p.Aliases[len(p.Aliases)-1] != "hive" {
			t.Errorf("%s: every preset keeps hive as an alias and hive leave as a leave phrase", id)
		}
		r := MeetingsPersonaConfig{Preset: id, Humor: MeetingsPersonaHumorOff}.Resolve()
		if r.IntroTemplate != p.PlainIntroTemplate {
			t.Errorf("%s: humor off must use the plain intro", id)
		}
	}
	for _, lang := range []string{"en-US", "en-GB", "es-ES", "fr-FR", "it-IT", "pt-BR", "hi-IN", "ja-JP", "zh-CN"} {
		if !languages[lang] {
			t.Errorf("no preset speaks %s", lang)
		}
	}
}

func TestMeetingsPersonaPresetOrder(t *testing.T) {
	ps := OrderedMeetingsPersonaPresets()
	if len(ps) != len(MeetingsPersonaPresets) || ps[0].ID != DefaultMeetingsPersonaPreset {
		t.Fatalf("ordered %d presets, first %q", len(ps), ps[0].ID)
	}
	seen := map[string]bool{}
	for _, id := range MeetingsPersonaPresetOrder {
		if seen[id] {
			t.Errorf("%s listed twice", id)
		}
		seen[id] = true
		if _, ok := MeetingsPersonaPresets[id]; !ok {
			t.Errorf("order names unknown preset %q", id)
		}
	}
	// A preset the order does not name is listed after the ordered ones.
	MeetingsPersonaPresets["zz-extra"] = MeetingsPersonaPreset{ID: "zz-extra"}
	MeetingsPersonaPresets["aa-extra"] = MeetingsPersonaPreset{ID: "aa-extra"}
	t.Cleanup(func() {
		delete(MeetingsPersonaPresets, "zz-extra")
		delete(MeetingsPersonaPresets, "aa-extra")
	})
	ps = OrderedMeetingsPersonaPresets()
	if n := len(ps); ps[n-2].ID != "aa-extra" || ps[n-1].ID != "zz-extra" {
		t.Fatalf("unordered presets not appended by ID: %q %q", ps[n-2].ID, ps[n-1].ID)
	}
}

func TestKokoroVoice(t *testing.T) {
	if v, ok := KokoroVoice("bm_george"); !ok || v.Language != "en-GB" || v.Accent != "British" || v.VoiceType != MeetingsPersonaVoiceMale {
		t.Fatalf("bm_george = %+v %v", v, ok)
	}
	for _, bad := range []string{"", "af", "af_", "afx_y", "qf_x", "ax_y"} {
		if _, ok := KokoroVoice(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMeetingsVoiceTypeOptions(t *testing.T) {
	opts := MeetingsVoiceTypeOptions()
	got := map[string]bool{}
	for _, o := range opts {
		got[o.ID] = o.Available
	}
	if len(opts) != 3 || !got[MeetingsPersonaVoiceFemale] || !got[MeetingsPersonaVoiceMale] || got[MeetingsPersonaVoiceBot] {
		t.Fatalf("voice types %+v", opts)
	}
}

func TestMeetingsPersonaOverrides(t *testing.T) {
	if o := (MeetingsPersonaConfig{}).Overrides(); len(o) != 0 {
		t.Fatalf("zero config overrides %v", o)
	}
	on := true
	p := MeetingsPersonaConfig{
		Enabled: &on, Preset: "bertie", Name: "B", DisplayName: "D", Pronouns: "p", Language: "en-GB", Voice: "bm_lewis",
		VoiceType: "male", Accent: "a", IntroTemplate: "i {leave}", Humor: "off", SearxngURL: "http://s",
		Aliases: []string{"b"}, LeavePhrases: []string{"b leave"},
		Unprompted: MeetingsPersonaUnprompted{Per10m: 1, MinGapS: 1, PerMeeting: 1, AnswerWaitS: 1, MinConfidence: 0.5, QueueDepth: 1, StaleS: 1},
	}
	o := p.Overrides()
	for _, k := range []string{"enabled", "preset", "name", "display_name", "pronouns", "language", "voice", "voice_type", "accent",
		"intro_template", "humor", "searxng_url", "aliases", "leave_phrases", "unprompted.per_10m", "unprompted.min_gap_s",
		"unprompted.per_meeting", "unprompted.answer_wait_s", "unprompted.min_confidence", "unprompted.queue_depth", "unprompted.stale_s"} {
		if o[k] != MeetingsPersonaSourceConfig {
			t.Errorf("%s = %q, want config", k, o[k])
		}
	}
	t.Setenv(MeetingsPersonaNameEnv, "Envy")
	t.Setenv(MeetingsPersonaPer10mEnv, "4")
	o = (MeetingsPersonaConfig{}).Overrides()
	if o["name"] != MeetingsPersonaSourceEnv || o["unprompted.per_10m"] != MeetingsPersonaSourceEnv || len(o) != 2 {
		t.Fatalf("env overrides %v", o)
	}
}

func TestMeetingsPersonaSampleLine(t *testing.T) {
	if got := (MeetingsPersonaConfig{}).Resolve().SampleLine(); got != "Hi, I'm Betty Bee from the hive. Ask me anything about the hive's work and I'll keep it short and sweet." {
		t.Fatalf("default sample %q", got)
	}
	// The persona's own preset speaks the language.
	if got := (MeetingsPersonaConfig{Preset: "melie"}).Resolve().SampleLine(); !strings.HasPrefix(got, "Bonjour, je suis Mélie") {
		t.Fatalf("melie sample %q", got)
	}
	// Betty switched to Italian takes the Italian preset's line with her name.
	if got := (MeetingsPersonaConfig{Language: "it-IT"}).Resolve().SampleLine(); got != "Ciao, sono Betty Bee from the hive. Chiedimi pure qualcosa sul lavoro dell'alveare." {
		t.Fatalf("italian betty sample %q", got)
	}
	// A language no preset speaks falls back to the default preset's line.
	if got := (MeetingsPersonaConfig{Language: "de-DE", DisplayName: "Biene"}).Resolve().SampleLine(); !strings.HasPrefix(got, "Hi, I'm Biene.") {
		t.Fatalf("fallback sample %q", got)
	}
}
