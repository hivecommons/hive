package config

import (
	"os"
	"sort"
	"strings"
)

// Meeting attendee catalog (hivecommons/hive#18396): the order the settings
// tab lists the persona presets in, the TTS voices the self-hosted Kokoro
// provides, the voice types, and which persona fields an operator has
// overridden.

// MeetingsPersonaPresetOrder is the order the settings tab lists the
// presets in: the default first, then English, then the other languages.
var MeetingsPersonaPresetOrder = []string{
	"betty", "barnaby", "beatrice", "bertie",
	"melisa", "melie", "melina", "bia", "madhu", "mitsu", "mimi",
}

// OrderedMeetingsPersonaPresets returns the presets in
// MeetingsPersonaPresetOrder, then any preset the order does not name (by
// ID), so a new preset is never hidden.
func OrderedMeetingsPersonaPresets() []MeetingsPersonaPreset {
	out := make([]MeetingsPersonaPreset, 0, len(MeetingsPersonaPresets))
	seen := make(map[string]bool, len(MeetingsPersonaPresets))
	for _, id := range MeetingsPersonaPresetOrder {
		if p, ok := MeetingsPersonaPresets[id]; ok && !seen[id] {
			out = append(out, p)
			seen[id] = true
		}
	}
	var rest []string
	for id := range MeetingsPersonaPresets {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	for _, id := range rest {
		out = append(out, MeetingsPersonaPresets[id])
	}
	return out
}

// MeetingsTTSVoice is one voice the meetings TTS service offers.
type MeetingsTTSVoice struct {
	ID string `json:"id"`
	// Language is the BCP-47 tag the voice speaks.
	Language  string `json:"language"`
	Accent    string `json:"accent"`
	VoiceType string `json:"voice_type"`
}

// kokoroLanguages maps a Kokoro voice id's first letter to the language it
// speaks and the accent shown for it. Kokoro ids are <language><gender>_<name>:
// a American English, b British English, e Spanish, f French, h Hindi,
// i Italian, j Japanese, p Brazilian Portuguese, z Mandarin.
var kokoroLanguages = map[byte][2]string{
	'a': {"en-US", "American"},
	'b': {"en-GB", "British"},
	'e': {"es-ES", "Spanish (Spain)"},
	'f': {"fr-FR", "French"},
	'h': {"hi-IN", "Indian"},
	'i': {"it-IT", "Italian"},
	'j': {"ja-JP", "Standard Japanese"},
	'p': {"pt-BR", "Brazilian"},
	'z': {"zh-CN", "Standard Mandarin"},
}

// kokoroVoiceIDs are the Kokoro-82M v1.0 voices Kokoro-FastAPI v0.10.0
// ships (api/src/voices/v1_0 at that tag, which GET /v1/audio/voices
// lists): the standard voices, without the legacy v0 voices and the
// FastAPI-only blends.
var kokoroVoiceIDs = []string{
	"af_alloy", "af_aoede", "af_bella", "af_heart", "af_jessica", "af_kore", "af_nicole", "af_nova", "af_river", "af_sarah", "af_sky",
	"am_adam", "am_echo", "am_eric", "am_fenrir", "am_liam", "am_michael", "am_onyx", "am_puck", "am_santa",
	"bf_alice", "bf_emma", "bf_isabella", "bf_lily",
	"bm_daniel", "bm_fable", "bm_george", "bm_lewis",
	"ef_dora", "em_alex", "em_santa",
	"ff_siwis",
	"hf_alpha", "hf_beta", "hm_omega", "hm_psi",
	"if_sara", "im_nicola",
	"jf_alpha", "jf_gongitsune", "jf_nezumi", "jf_tebukuro", "jm_kumo",
	"pf_dora", "pm_alex", "pm_santa",
	"zf_xiaobei", "zf_xiaoni", "zf_xiaoxiao", "zf_xiaoyi", "zm_yunjian", "zm_yunxi", "zm_yunxia", "zm_yunyang",
}

// KokoroVoice describes a Kokoro voice id; ok is false when the id does
// not follow Kokoro's <language><gender>_<name> shape.
func KokoroVoice(id string) (MeetingsTTSVoice, bool) {
	if len(id) < len("af_x") || id[2] != '_' {
		return MeetingsTTSVoice{}, false
	}
	lang, ok := kokoroLanguages[id[0]]
	if !ok {
		return MeetingsTTSVoice{}, false
	}
	var vt string
	switch id[1] {
	case 'f':
		vt = MeetingsPersonaVoiceFemale
	case 'm':
		vt = MeetingsPersonaVoiceMale
	default:
		return MeetingsTTSVoice{}, false
	}
	return MeetingsTTSVoice{ID: id, Language: lang[0], Accent: lang[1], VoiceType: vt}, true
}

// KokoroVoices returns the Kokoro voices in catalog order.
func KokoroVoices() []MeetingsTTSVoice {
	out := make([]MeetingsTTSVoice, 0, len(kokoroVoiceIDs))
	for _, id := range kokoroVoiceIDs {
		if v, ok := KokoroVoice(id); ok {
			out = append(out, v)
		}
	}
	return out
}

// MeetingsVoiceTypeOption is one voice type the settings tab offers.
type MeetingsVoiceTypeOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Available is false for a type no shipped voice provides yet.
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
}

// MeetingsVoiceTypeOptions are the voice types. Kokoro has no robotic
// voice and a bot-style voice would need a new service, so bot-style is
// listed as coming later.
func MeetingsVoiceTypeOptions() []MeetingsVoiceTypeOption {
	return []MeetingsVoiceTypeOption{
		{ID: MeetingsPersonaVoiceFemale, Label: "Female", Available: true},
		{ID: MeetingsPersonaVoiceMale, Label: "Male", Available: true},
		{ID: MeetingsPersonaVoiceBot, Label: "Bot-style", Note: "coming later: the self-hosted Kokoro TTS has no bot-style voice"},
	}
}

// Persona override sources.
const (
	MeetingsPersonaSourceEnv    = "env"
	MeetingsPersonaSourceConfig = "config"
)

// Overrides reports which persona fields do not come from the preset (or,
// for the unprompted caps, the defaults), keyed by the meetings.persona key
// ("name", "unprompted.per_10m") with the source: "env" when an
// HIVE_MEETINGS_PERSONA_* variable sets it, else "config" when hive.yaml
// does. The preset itself is reported the same way.
func (p MeetingsPersonaConfig) Overrides() map[string]string {
	out := map[string]string{}
	set := func(key, env string, inConfig bool) {
		switch {
		case strings.TrimSpace(os.Getenv(env)) != "":
			out[key] = MeetingsPersonaSourceEnv
		case inConfig:
			out[key] = MeetingsPersonaSourceConfig
		}
	}
	set("enabled", MeetingsPersonaEnabledEnv, p.Enabled != nil)
	set("preset", MeetingsPersonaPresetEnv, strings.TrimSpace(p.Preset) != "")
	for _, f := range []struct {
		key, env, v string
	}{
		{"name", MeetingsPersonaNameEnv, p.Name},
		{"display_name", MeetingsPersonaDisplayNameEnv, p.DisplayName},
		{"pronouns", MeetingsPersonaPronounsEnv, p.Pronouns},
		{"language", MeetingsPersonaLanguageEnv, p.Language},
		{"voice", MeetingsPersonaVoiceEnv, p.Voice},
		{"voice_type", MeetingsPersonaVoiceTypeEnv, p.VoiceType},
		{"accent", MeetingsPersonaAccentEnv, p.Accent},
		{"intro_template", MeetingsPersonaIntroTemplateEnv, p.IntroTemplate},
		{"humor", MeetingsPersonaHumorEnv, p.Humor},
		{"searxng_url", MeetingsPersonaSearxngURLEnv, p.SearxngURL},
	} {
		set(f.key, f.env, strings.TrimSpace(f.v) != "")
	}
	set("aliases", MeetingsPersonaAliasesEnv, len(p.Aliases) > 0)
	set("leave_phrases", MeetingsPersonaLeavePhrasesEnv, len(p.LeavePhrases) > 0)
	u := p.Unprompted
	for _, f := range []struct {
		key, env string
		set      bool
	}{
		{"unprompted.per_10m", MeetingsPersonaPer10mEnv, u.Per10m > 0},
		{"unprompted.min_gap_s", MeetingsPersonaMinGapEnv, u.MinGapS > 0},
		{"unprompted.per_meeting", MeetingsPersonaPerMeetingEnv, u.PerMeeting > 0},
		{"unprompted.answer_wait_s", MeetingsPersonaAnswerWaitEnv, u.AnswerWaitS > 0},
		{"unprompted.min_confidence", MeetingsPersonaMinConfEnv, u.MinConfidence > 0},
		{"unprompted.queue_depth", MeetingsPersonaQueueDepthEnv, u.QueueDepth > 0},
		{"unprompted.stale_s", MeetingsPersonaStaleEnv, u.StaleS > 0},
	} {
		set(f.key, f.env, f.set)
	}
	return out
}

// SampleLine renders the effective persona's sample line: the preset's
// line for the persona's language (the persona's own preset when it
// speaks that language, else the first preset that does, else the default
// preset's line), with the intro placeholders filled in.
func (r ResolvedMeetingsPersona) SampleLine() string {
	line := ""
	if pre, ok := MeetingsPersonaPresets[r.Preset]; ok && strings.EqualFold(pre.Language, r.Language) {
		line = pre.SampleLine
	}
	if line == "" {
		for _, pre := range OrderedMeetingsPersonaPresets() {
			if strings.EqualFold(pre.Language, r.Language) {
				line = pre.SampleLine
				break
			}
		}
	}
	if line == "" {
		line = MeetingsPersonaPresets[DefaultMeetingsPersonaPreset].SampleLine
	}
	t := r
	t.IntroTemplate = line
	return t.Intro()
}
