package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Meeting attendee persona (hivecommons/hive#18391, #18392): the name,
// voice and manners hive attends meetings with. The default persona is
// Betty ("Betty Bee from the hive"). A persona is a preset plus any
// explicitly set field: the effective persona is the preset's values
// overridden by whatever meetings.persona sets, and every field can also be
// overridden by an HIVE_MEETINGS_PERSONA_* environment variable (env, then
// hive.yaml, then the preset).

// Persona humor settings.
const (
	MeetingsPersonaHumorLight = "light"
	MeetingsPersonaHumorOff   = "off"
)

// Persona voice types. They describe the persona (intro, self-description,
// the settings UI); they do not change behaviour yet.
const (
	MeetingsPersonaVoiceFemale = "female"
	MeetingsPersonaVoiceMale   = "male"
	MeetingsPersonaVoiceBot    = "bot"
)

// Persona defaults.
const (
	// DefaultMeetingsPersonaPreset is the preset a persona starts from.
	DefaultMeetingsPersonaPreset = "betty"
	// DefaultMeetingsPersonaPer10m caps unprompted (question-driven) chat
	// answers per ten minutes.
	DefaultMeetingsPersonaPer10m = 2
	// DefaultMeetingsPersonaMinGapS is the minimum gap between two
	// unprompted answers, in seconds.
	DefaultMeetingsPersonaMinGapS = 90
	// DefaultMeetingsPersonaPerMeeting caps unprompted answers per meeting.
	DefaultMeetingsPersonaPerMeeting = 12
	// DefaultMeetingsPersonaAnswerWaitS is how long the persona waits for a
	// person to answer a room question before answering it herself.
	DefaultMeetingsPersonaAnswerWaitS = 8
	// DefaultMeetingsPersonaMinConfidence is the model confidence an
	// unprompted answer needs.
	DefaultMeetingsPersonaMinConfidence = 0.75
	// DefaultMeetingsPersonaQueueDepth bounds room questions waiting at once.
	DefaultMeetingsPersonaQueueDepth = 3
	// DefaultMeetingsPersonaStaleS drops a room question not answered within
	// this many seconds.
	DefaultMeetingsPersonaStaleS = 60

	// DefaultMeetingsConsentTextNoPersona is the consent notice when the
	// persona is turned off.
	DefaultMeetingsConsentTextNoPersona = "Hive has joined to take notes and help with research. It records this meeting's transcript. Type 'hive leave' to remove it."
	// defaultMeetingsLeavePhraseNoPersona is the leave phrase without a
	// persona.
	defaultMeetingsLeavePhraseNoPersona = "hive leave"

	// meetingsMaxIntroChars bounds the intro template (one chat post, with
	// room to spare for substitutions).
	meetingsMaxIntroChars = 1000
)

// MeetingsPersonaPreset is one built-in persona. The settings tab lists
// these (hivecommons/hive#18396); src/pkg/meetings/betty re-exports them.
type MeetingsPersonaPreset struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Aliases     []string `json:"aliases"`
	Pronouns    string   `json:"pronouns"`
	// Language is a BCP-47 tag; replies follow its language and regional
	// conventions.
	Language string `json:"language"`
	// Voice is a TTS voice id. Kokoro ids encode accent and gender:
	// af_* American female, am_* American male, bf_*/bm_* British.
	Voice     string `json:"voice"`
	VoiceType string `json:"voice_type"`
	Accent    string `json:"accent"`
	Humor     string `json:"humor"`
	// IntroTemplate is the intro with humor on; PlainIntroTemplate is used
	// when humor is off and no intro_template is set.
	IntroTemplate      string   `json:"intro_template"`
	PlainIntroTemplate string   `json:"plain_intro_template"`
	LeavePhrases       []string `json:"leave_phrases"`
	// SampleLine is the short line the settings tab's Play sample button
	// speaks, in the preset's language. Same placeholders as the intro.
	SampleLine string `json:"sample_line"`
}

// MeetingsPersonaPresets are the built-in personas, keyed by ID. Every
// voice is one the self-hosted Kokoro TTS ships (KokoroVoices); the order
// the settings tab lists them in is MeetingsPersonaPresetOrder. Leave
// phrases stay in Latin script: the leave matcher uses ASCII word
// boundaries.
var MeetingsPersonaPresets = map[string]MeetingsPersonaPreset{
	"betty": {
		ID:                 "betty",
		Name:               "Betty",
		DisplayName:        "Betty Bee from the hive",
		Aliases:            []string{"betty", "betty bee", "hive"},
		Pronouns:           "she/her",
		Language:           "en-US",
		Voice:              "af_heart",
		VoiceType:          MeetingsPersonaVoiceFemale,
		Accent:             "American",
		Humor:              MeetingsPersonaHumorLight,
		IntroTemplate:      meetingsIntroEnglish,
		PlainIntroTemplate: meetingsPlainIntroEnglish,
		LeavePhrases:       []string{"betty leave", "hive leave"},
		SampleLine:         meetingsSampleEnglish,
	},
	"barnaby": {
		ID:                 "barnaby",
		Name:               "Barnaby",
		DisplayName:        "Barnaby Bee from the hive",
		Aliases:            []string{"barnaby", "barnaby bee", "hive"},
		Pronouns:           "he/him",
		Language:           "en-US",
		Voice:              "am_michael",
		VoiceType:          MeetingsPersonaVoiceMale,
		Accent:             "American",
		Humor:              MeetingsPersonaHumorLight,
		IntroTemplate:      meetingsIntroEnglish,
		PlainIntroTemplate: meetingsPlainIntroEnglish,
		LeavePhrases:       []string{"barnaby leave", "hive leave"},
		SampleLine:         meetingsSampleEnglish,
	},
	"beatrice": {
		ID:                 "beatrice",
		Name:               "Beatrice",
		DisplayName:        "Beatrice Bee from the hive",
		Aliases:            []string{"beatrice", "beatrice bee", "hive"},
		Pronouns:           "she/her",
		Language:           "en-GB",
		Voice:              "bf_emma",
		VoiceType:          MeetingsPersonaVoiceFemale,
		Accent:             "British",
		Humor:              MeetingsPersonaHumorLight,
		IntroTemplate:      meetingsIntroEnglish,
		PlainIntroTemplate: meetingsPlainIntroEnglish,
		LeavePhrases:       []string{"beatrice leave", "hive leave"},
		SampleLine:         meetingsSampleEnglish,
	},
	"bertie": {
		ID:                 "bertie",
		Name:               "Bertie",
		DisplayName:        "Bertie Bee from the hive",
		Aliases:            []string{"bertie", "bertie bee", "hive"},
		Pronouns:           "he/him",
		Language:           "en-GB",
		Voice:              "bm_george",
		VoiceType:          MeetingsPersonaVoiceMale,
		Accent:             "British",
		Humor:              MeetingsPersonaHumorLight,
		IntroTemplate:      meetingsIntroEnglish,
		PlainIntroTemplate: meetingsPlainIntroEnglish,
		LeavePhrases:       []string{"bertie leave", "hive leave"},
		SampleLine:         meetingsSampleEnglish,
	},
	"melisa": {
		ID:                 "melisa",
		Name:               "Melisa",
		DisplayName:        "Melisa, la abeja de la colmena",
		Aliases:            []string{"melisa", "abeja", "hive"},
		Pronouns:           "ella",
		Language:           "es-ES",
		Voice:              "ef_dora",
		VoiceType:          MeetingsPersonaVoiceFemale,
		Accent:             "Spanish (Spain)",
		Humor:              MeetingsPersonaHumorLight,
		IntroTemplate:      "Hola, soy {display_name}. Tomo notas, guardo la transcripción de esta reunión y respondo preguntas con lo que sabe la colmena: su base de conocimiento y sus tareas. Di mi nombre para preguntarme algo. Di \"{leave}\" y saldré volando.",
		PlainIntroTemplate: "Hola, soy {display_name}. Tomo notas, guardo la transcripción de esta reunión y respondo preguntas con la base de conocimiento y las tareas de la colmena. Di mi nombre para preguntarme algo, o di \"{leave}\" para que me vaya.",
		LeavePhrases:       []string{"melisa vete", "hive leave"},
		SampleLine:         "Hola, soy {display_name}. Pregúntame lo que quieras sobre el trabajo de la colmena.",
	},
	"melie": {
		ID:                 "melie",
		Name:               "Mélie",
		DisplayName:        "Mélie, l'abeille de la ruche",
		Aliases:            []string{"mélie", "melie", "abeille", "hive"},
		Pronouns:           "elle",
		Language:           "fr-FR",
		Voice:              "ff_siwis",
		VoiceType:          MeetingsPersonaVoiceFemale,
		Accent:             "French",
		Humor:              MeetingsPersonaHumorLight,
		IntroTemplate:      "Bonjour, je suis {display_name}. Je prends des notes, je garde la transcription de cette réunion et je réponds aux questions avec ce que sait la ruche : sa base de connaissances et ses tâches. Dites mon nom pour me poser une question. Dites « {leave} » et je m'envole.",
		PlainIntroTemplate: "Bonjour, je suis {display_name}. Je prends des notes, je garde la transcription de cette réunion et je réponds aux questions à partir de la base de connaissances et des tâches de la ruche. Dites mon nom pour me poser une question, ou dites « {leave} » pour me retirer.",
		LeavePhrases:       []string{"melie pars", "hive leave"},
		SampleLine:         "Bonjour, je suis {display_name}. Posez-moi vos questions sur le travail de la ruche.",
	},
	"melina": {
		ID:                 "melina",
		Name:               "Melina",
		DisplayName:        "Melina, l'ape dell'alveare",
		Aliases:            []string{"melina", "ape", "hive"},
		Pronouns:           "lei",
		Language:           "it-IT",
		Voice:              "if_sara",
		VoiceType:          MeetingsPersonaVoiceFemale,
		Accent:             "Italian",
		Humor:              MeetingsPersonaHumorLight,
		IntroTemplate:      "Ciao, sono {display_name}. Prendo appunti, conservo la trascrizione di questa riunione e rispondo alle domande con ciò che sa l'alveare: la sua base di conoscenza e le sue attività. Di' il mio nome per farmi una domanda. Di' \"{leave}\" e volo via.",
		PlainIntroTemplate: "Ciao, sono {display_name}. Prendo appunti, conservo la trascrizione di questa riunione e rispondo alle domande con la base di conoscenza e le attività dell'alveare. Di' il mio nome per farmi una domanda, oppure di' \"{leave}\" per farmi uscire.",
		LeavePhrases:       []string{"melina esci", "hive leave"},
		SampleLine:         "Ciao, sono {display_name}. Chiedimi pure qualcosa sul lavoro dell'alveare.",
	},
	"bia": {
		ID:                 "bia",
		Name:               "Bia",
		DisplayName:        "Bia, a abelha da colmeia",
		Aliases:            []string{"bia", "abelha", "hive"},
		Pronouns:           "ela",
		Language:           "pt-BR",
		Voice:              "pf_dora",
		VoiceType:          MeetingsPersonaVoiceFemale,
		Accent:             "Brazilian",
		Humor:              MeetingsPersonaHumorLight,
		IntroTemplate:      "Oi, eu sou a {display_name}. Faço anotações, guardo a transcrição desta reunião e respondo perguntas com o que a colmeia sabe: a base de conhecimento e as tarefas dela. Diga meu nome para me perguntar algo. Diga \"{leave}\" e eu saio voando.",
		PlainIntroTemplate: "Oi, eu sou a {display_name}. Faço anotações, guardo a transcrição desta reunião e respondo perguntas com a base de conhecimento e as tarefas da colmeia. Diga meu nome para me perguntar algo, ou diga \"{leave}\" para me remover.",
		LeavePhrases:       []string{"bia sai", "hive leave"},
		SampleLine:         "Oi, eu sou a {display_name}. Pode me perguntar qualquer coisa sobre o trabalho da colmeia.",
	},
	"madhu": {
		ID:                 "madhu",
		Name:               "मधु",
		DisplayName:        "मधु, छत्ते की मधुमक्खी",
		Aliases:            []string{"मधु", "madhu", "hive"},
		Pronouns:           "वह",
		Language:           "hi-IN",
		Voice:              "hf_alpha",
		VoiceType:          MeetingsPersonaVoiceFemale,
		Accent:             "Indian",
		Humor:              MeetingsPersonaHumorLight,
		IntroTemplate:      "नमस्ते, मैं {display_name} हूँ। मैं नोट्स लेती हूँ, इस मीटिंग का ट्रांसक्रिप्ट रखती हूँ, और छत्ते की जानकारी, यानी उसके नॉलेज बेस और काम की चीज़ों से सवालों के जवाब देती हूँ। कुछ पूछना हो तो मेरा नाम लीजिए। \"{leave}\" कहिए और मैं उड़ जाऊँगी।",
		PlainIntroTemplate: "नमस्ते, मैं {display_name} हूँ। मैं नोट्स लेती हूँ, इस मीटिंग का ट्रांसक्रिप्ट रखती हूँ, और छत्ते के नॉलेज बेस और काम की चीज़ों से सवालों के जवाब देती हूँ। कुछ पूछना हो तो मेरा नाम लीजिए, या मुझे हटाने के लिए \"{leave}\" कहिए।",
		LeavePhrases:       []string{"madhu leave", "hive leave"},
		SampleLine:         "नमस्ते, मैं {display_name} हूँ। छत्ते के काम के बारे में मुझसे कुछ भी पूछिए।",
	},
	"mitsu": {
		ID:                 "mitsu",
		Name:               "ミツ",
		DisplayName:        "ミツバチのミツ",
		Aliases:            []string{"ミツ", "ミツさん", "mitsu", "hive"},
		Pronouns:           "彼女",
		Language:           "ja-JP",
		Voice:              "jf_alpha",
		VoiceType:          MeetingsPersonaVoiceFemale,
		Accent:             "Standard Japanese",
		Humor:              MeetingsPersonaHumorLight,
		IntroTemplate:      "こんにちは、{display_name}です。メモを取り、この会議の文字起こしを記録し、ハイブのナレッジベースと作業項目をもとに質問にお答えします。質問があるときは私の名前を呼んでください。「{leave}」と言えば、ぶーんと飛んで帰ります。",
		PlainIntroTemplate: "こんにちは、{display_name}です。メモを取り、この会議の文字起こしを記録し、ハイブのナレッジベースと作業項目をもとに質問にお答えします。質問があるときは私の名前を呼んでください。退出させるには「{leave}」と言ってください。",
		LeavePhrases:       []string{"mitsu leave", "hive leave"},
		SampleLine:         "こんにちは、{display_name}です。ハイブの作業について何でも聞いてください。",
	},
	"mimi": {
		ID:                 "mimi",
		Name:               "蜜蜜",
		DisplayName:        "蜂巢的小蜜蜂蜜蜜",
		Aliases:            []string{"蜜蜜", "mimi", "hive"},
		Pronouns:           "她",
		Language:           "zh-CN",
		Voice:              "zf_xiaobei",
		VoiceType:          MeetingsPersonaVoiceFemale,
		Accent:             "Standard Mandarin",
		Humor:              MeetingsPersonaHumorLight,
		IntroTemplate:      "大家好，我是{display_name}。我负责记笔记、保存这次会议的文字记录，并根据蜂巢的知识库和工作项回答问题。想问我问题，就叫我的名字。说“{leave}”，我就嗡嗡飞走啦。",
		PlainIntroTemplate: "大家好，我是{display_name}。我负责记笔记、保存这次会议的文字记录，并根据蜂巢的知识库和工作项回答问题。想问我问题，就叫我的名字；要让我离开，请说“{leave}”。",
		LeavePhrases:       []string{"mimi leave", "hive leave"},
		SampleLine:         "大家好，我是{display_name}。关于蜂巢的工作，尽管问我。",
	},
}

// The English presets share their intro and sample templates.
const (
	meetingsIntroEnglish      = "Hi, I'm {display_name}. I take notes, keep this meeting's transcript, and answer questions from what the hive knows: its knowledge base and work items. Say my name to ask me something. Say \"{leave}\" and I'll buzz off."
	meetingsPlainIntroEnglish = "Hi, I'm {display_name}. I take notes, keep this meeting's transcript, and answer questions from the hive's knowledge base and work items. Say my name to ask me something, or say \"{leave}\" to remove me."
	meetingsSampleEnglish     = "Hi, I'm {display_name}. Ask me anything about the hive's work and I'll keep it short and sweet."
)

// Persona env overrides. List values are comma separated.
const (
	MeetingsPersonaEnabledEnv       = "HIVE_MEETINGS_PERSONA_ENABLED"
	MeetingsPersonaPresetEnv        = "HIVE_MEETINGS_PERSONA_PRESET"
	MeetingsPersonaNameEnv          = "HIVE_MEETINGS_PERSONA_NAME"
	MeetingsPersonaDisplayNameEnv   = "HIVE_MEETINGS_PERSONA_DISPLAY_NAME"
	MeetingsPersonaAliasesEnv       = "HIVE_MEETINGS_PERSONA_ALIASES"
	MeetingsPersonaPronounsEnv      = "HIVE_MEETINGS_PERSONA_PRONOUNS"
	MeetingsPersonaLanguageEnv      = "HIVE_MEETINGS_PERSONA_LANGUAGE"
	MeetingsPersonaVoiceEnv         = "HIVE_MEETINGS_PERSONA_VOICE"
	MeetingsPersonaVoiceTypeEnv     = "HIVE_MEETINGS_PERSONA_VOICE_TYPE"
	MeetingsPersonaAccentEnv        = "HIVE_MEETINGS_PERSONA_ACCENT"
	MeetingsPersonaIntroTemplateEnv = "HIVE_MEETINGS_PERSONA_INTRO_TEMPLATE"
	MeetingsPersonaLeavePhrasesEnv  = "HIVE_MEETINGS_PERSONA_LEAVE_PHRASES"
	MeetingsPersonaHumorEnv         = "HIVE_MEETINGS_PERSONA_HUMOR"
	MeetingsPersonaSearxngURLEnv    = "HIVE_MEETINGS_PERSONA_SEARXNG_URL"
	MeetingsPersonaPer10mEnv        = "HIVE_MEETINGS_PERSONA_UNPROMPTED_PER_10M"
	MeetingsPersonaMinGapEnv        = "HIVE_MEETINGS_PERSONA_UNPROMPTED_MIN_GAP_S"
	MeetingsPersonaPerMeetingEnv    = "HIVE_MEETINGS_PERSONA_UNPROMPTED_PER_MEETING"
	MeetingsPersonaAnswerWaitEnv    = "HIVE_MEETINGS_PERSONA_UNPROMPTED_ANSWER_WAIT_S"
	MeetingsPersonaMinConfEnv       = "HIVE_MEETINGS_PERSONA_UNPROMPTED_MIN_CONFIDENCE"
	MeetingsPersonaQueueDepthEnv    = "HIVE_MEETINGS_PERSONA_UNPROMPTED_QUEUE_DEPTH"
	MeetingsPersonaStaleEnv         = "HIVE_MEETINGS_PERSONA_UNPROMPTED_STALE_S"
)

// MeetingsPersonaConfig is meetings.persona. Empty fields take the preset's
// value.
type MeetingsPersonaConfig struct {
	// Enabled turns the persona on. Nil defaults true.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Actions are the drafts people may ask the persona for in a meeting
	// (issues, RFCs, Spek sessions, knowledge notes), each approved in the
	// dashboard (hivecommons/hive#18406).
	Actions MeetingsPersonaActions `yaml:"actions,omitempty" json:"actions,omitzero"`
	// Preset is the built-in persona to start from. Default "betty".
	Preset string `yaml:"preset,omitempty" json:"preset,omitempty"`
	// Name is what people call the persona ("Betty").
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// DisplayName is the meeting display name; it is the default bot name
	// (meetings.bot_name still wins). At most 64 characters.
	DisplayName string `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	// Aliases are the names that address the persona at the start or end of
	// a line.
	Aliases []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	// Pronouns are used only in the intro and self-description.
	Pronouns string `yaml:"pronouns,omitempty" json:"pronouns,omitempty"`
	// Language is a BCP-47 tag ("en-US"); replies use its language and
	// regional conventions.
	Language string `yaml:"language,omitempty" json:"language,omitempty"`
	// Voice is the TTS voice id for spoken replies.
	Voice string `yaml:"voice,omitempty" json:"voice,omitempty"`
	// VoiceType is female, male or bot (descriptive).
	VoiceType string `yaml:"voice_type,omitempty" json:"voice_type,omitempty"`
	// Accent is free text ("American", descriptive).
	Accent string `yaml:"accent,omitempty" json:"accent,omitempty"`
	// IntroTemplate is the intro. Placeholders: {name}, {display_name},
	// {pronouns}, {leave}, {accent}, {voice_type}, {language}.
	IntroTemplate string `yaml:"intro_template,omitempty" json:"intro_template,omitempty"`
	// LeavePhrases remove the persona from the meeting when said or typed.
	LeavePhrases []string `yaml:"leave_phrases,omitempty" json:"leave_phrases,omitempty"`
	// Humor is light or off.
	Humor string `yaml:"humor,omitempty" json:"humor,omitempty"`
	// Unprompted bounds question-driven answers nobody asked the persona for.
	Unprompted MeetingsPersonaUnprompted `yaml:"unprompted,omitempty" json:"unprompted,omitzero"`
	// SearxngURL is the SearXNG web search service Betty queries for
	// answers (JSON output), for example
	// http://searxng.meetings.svc.cluster.local:8080. Empty means no web search.
	SearxngURL string `yaml:"searxng_url,omitempty" json:"searxng_url,omitempty"`
}

// MeetingsPersonaUnprompted is meetings.persona.unprompted. Zero means the
// default for each field.
type MeetingsPersonaUnprompted struct {
	Per10m        int     `yaml:"per_10m,omitempty" json:"per_10m,omitempty"`
	MinGapS       int     `yaml:"min_gap_s,omitempty" json:"min_gap_s,omitempty"`
	PerMeeting    int     `yaml:"per_meeting,omitempty" json:"per_meeting,omitempty"`
	AnswerWaitS   int     `yaml:"answer_wait_s,omitempty" json:"answer_wait_s,omitempty"`
	MinConfidence float64 `yaml:"min_confidence,omitempty" json:"min_confidence,omitempty"`
	QueueDepth    int     `yaml:"queue_depth,omitempty" json:"queue_depth,omitempty"`
	StaleS        int     `yaml:"stale_s,omitempty" json:"stale_s,omitempty"`
}

// ResolvedMeetingsPersona is the effective persona.
type ResolvedMeetingsPersona struct {
	Enabled       bool
	Preset        string
	Name          string
	DisplayName   string
	Aliases       []string
	Pronouns      string
	Language      string
	Voice         string
	VoiceType     string
	Accent        string
	IntroTemplate string
	LeavePhrases  []string
	Humor         string
	SearxngURL    string
	Unprompted    MeetingsPersonaUnprompted
}

// meetingsEnvList returns the comma-separated env list when set, else the
// config list when non-empty, else def. Items are trimmed, lowercased and
// empty items dropped.
func meetingsEnvList(env string, value, def []string) []string {
	clean := func(in []string) []string {
		var out []string
		for _, s := range in {
			if s = strings.ToLower(strings.Join(strings.Fields(s), " ")); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if raw := strings.TrimSpace(os.Getenv(env)); raw != "" {
		if out := clean(strings.Split(raw, ",")); len(out) > 0 {
			return out
		}
	}
	if out := clean(value); len(out) > 0 {
		return out
	}
	return clean(def)
}

// meetingsEnvFloat returns a positive env value when set and valid, else a
// positive config value, else def.
func meetingsEnvFloat(env string, value, def float64) float64 {
	if raw := strings.TrimSpace(os.Getenv(env)); raw != "" {
		if f, err := strconv.ParseFloat(raw, 64); err == nil && f > 0 {
			return f
		}
	}
	if value > 0 {
		return value
	}
	return def
}

// presetOrDefault returns the effective preset ID and its values; an
// unknown ID falls back to the default preset (Validate reports it).
func (p MeetingsPersonaConfig) presetOrDefault() (string, MeetingsPersonaPreset) {
	id := strings.ToLower(meetingsEnvString(MeetingsPersonaPresetEnv, p.Preset, DefaultMeetingsPersonaPreset))
	if pre, ok := MeetingsPersonaPresets[id]; ok {
		return id, pre
	}
	return id, MeetingsPersonaPresets[DefaultMeetingsPersonaPreset]
}

// IsEnabled reports whether the persona is on: env, then
// meetings.persona.enabled, default true.
func (p MeetingsPersonaConfig) IsEnabled() bool {
	if v, ok := parseBoolEnv(MeetingsPersonaEnabledEnv); ok {
		return v
	}
	return p.Enabled == nil || *p.Enabled
}

// Resolve returns the effective persona: preset values overridden by
// meetings.persona fields, overridden by env. With the persona off, Name is
// "hive" and the leave phrase is "hive leave".
func (p MeetingsPersonaConfig) Resolve() ResolvedMeetingsPersona {
	id, pre := p.presetOrDefault()
	humor := strings.ToLower(meetingsEnvString(MeetingsPersonaHumorEnv, p.Humor, pre.Humor))
	intro := pre.IntroTemplate
	if humor == MeetingsPersonaHumorOff && pre.PlainIntroTemplate != "" {
		intro = pre.PlainIntroTemplate
	}
	u := p.Unprompted
	r := ResolvedMeetingsPersona{
		Enabled:       p.IsEnabled(),
		Preset:        id,
		Name:          meetingsEnvString(MeetingsPersonaNameEnv, p.Name, pre.Name),
		DisplayName:   meetingsEnvString(MeetingsPersonaDisplayNameEnv, p.DisplayName, pre.DisplayName),
		Aliases:       meetingsEnvList(MeetingsPersonaAliasesEnv, p.Aliases, pre.Aliases),
		Pronouns:      meetingsEnvString(MeetingsPersonaPronounsEnv, p.Pronouns, pre.Pronouns),
		Language:      meetingsEnvString(MeetingsPersonaLanguageEnv, p.Language, pre.Language),
		Voice:         meetingsEnvString(MeetingsPersonaVoiceEnv, p.Voice, pre.Voice),
		VoiceType:     strings.ToLower(meetingsEnvString(MeetingsPersonaVoiceTypeEnv, p.VoiceType, pre.VoiceType)),
		Accent:        meetingsEnvString(MeetingsPersonaAccentEnv, p.Accent, pre.Accent),
		IntroTemplate: meetingsEnvString(MeetingsPersonaIntroTemplateEnv, p.IntroTemplate, intro),
		LeavePhrases:  meetingsEnvList(MeetingsPersonaLeavePhrasesEnv, p.LeavePhrases, pre.LeavePhrases),
		Humor:         humor,
		SearxngURL:    strings.TrimRight(meetingsEnvString(MeetingsPersonaSearxngURLEnv, p.SearxngURL, ""), "/"),
		Unprompted: MeetingsPersonaUnprompted{
			Per10m:        meetingsEnvInt(MeetingsPersonaPer10mEnv, u.Per10m, DefaultMeetingsPersonaPer10m),
			MinGapS:       meetingsEnvInt(MeetingsPersonaMinGapEnv, u.MinGapS, DefaultMeetingsPersonaMinGapS),
			PerMeeting:    meetingsEnvInt(MeetingsPersonaPerMeetingEnv, u.PerMeeting, DefaultMeetingsPersonaPerMeeting),
			AnswerWaitS:   meetingsEnvInt(MeetingsPersonaAnswerWaitEnv, u.AnswerWaitS, DefaultMeetingsPersonaAnswerWaitS),
			MinConfidence: meetingsEnvFloat(MeetingsPersonaMinConfEnv, u.MinConfidence, DefaultMeetingsPersonaMinConfidence),
			QueueDepth:    meetingsEnvInt(MeetingsPersonaQueueDepthEnv, u.QueueDepth, DefaultMeetingsPersonaQueueDepth),
			StaleS:        meetingsEnvInt(MeetingsPersonaStaleEnv, u.StaleS, DefaultMeetingsPersonaStaleS),
		},
	}
	if !r.Enabled {
		r.Name = "hive"
		r.LeavePhrases = []string{defaultMeetingsLeavePhraseNoPersona}
	}
	return r
}

// Intro renders the intro template.
func (r ResolvedMeetingsPersona) Intro() string {
	leave := ""
	if len(r.LeavePhrases) > 0 {
		leave = r.LeavePhrases[0]
	}
	out := strings.NewReplacer(
		"{name}", r.Name, "{display_name}", r.DisplayName, "{pronouns}", "",
		"{leave}", leave, "{accent}", r.Accent, "{voice_type}", r.VoiceType, "{language}", r.Language,
	).Replace(r.IntroTemplate)
	return StripPronouns(out)
}

// The persona never states pronouns in a reply (operator ruling after the
// live Meet test, where "(she/her)" was also read aloud as "she slash
// her"). {pronouns} renders empty, and a pronoun parenthetical a custom
// template spells out is dropped.
var (
	pronounParen = regexp.MustCompile(`(?i)\s*[\(（]\s*(?:(?:she|he|they|xe|ze|it|any|ella|elle|lei|ela|il|er|sie)\s*(?:/\s*[\pL]+\s*){0,2})?[\)）]`)
	pronounSpace = regexp.MustCompile(`\s+([.,;:!?。，、])`)
)

// StripPronouns removes pronoun parentheticals ("(she/her)", "(they/them)")
// and the empty parentheses a blank {pronouns} leaves.
func StripPronouns(s string) string {
	s = pronounParen.ReplaceAllString(s, "")
	return pronounSpace.ReplaceAllString(s, "$1")
}

// ConsentText is the default consent notice for this persona: it names the
// persona, says it records the transcript and how to remove it. Pronoun
// free, so any persona reads correctly.
func (r ResolvedMeetingsPersona) ConsentText() string {
	if !r.Enabled {
		return DefaultMeetingsConsentTextNoPersona
	}
	quoted := make([]string, 0, len(r.LeavePhrases))
	for _, l := range r.LeavePhrases {
		quoted = append(quoted, "'"+l+"'")
	}
	how := strings.Join(quoted, " or ")
	return fmt.Sprintf("%s has joined to take notes and help with research. %s records this meeting's transcript. Type %s to remove %s.",
		r.DisplayName, r.Name, how, r.Name)
}

// bcp47Shape is a loose BCP-47 shape check (language, then subtags).
var bcp47Shape = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{1,8})*$`)

// Validate checks the effective persona. A disabled persona is never
// rejected.
func (p MeetingsPersonaConfig) Validate() error {
	if !p.IsEnabled() {
		return nil
	}
	id := strings.ToLower(meetingsEnvString(MeetingsPersonaPresetEnv, p.Preset, DefaultMeetingsPersonaPreset))
	if _, ok := MeetingsPersonaPresets[id]; !ok {
		return fmt.Errorf("meetings.persona.preset %q is not a known preset", id)
	}
	r := p.Resolve()
	switch {
	case r.Name == "":
		return fmt.Errorf("meetings.persona.name must not be empty")
	case len(r.Aliases) == 0:
		return fmt.Errorf("meetings.persona.aliases must name at least one alias")
	case len(r.LeavePhrases) == 0:
		return fmt.Errorf("meetings.persona.leave_phrases must name at least one phrase")
	case r.Voice == "":
		return fmt.Errorf("meetings.persona.voice must not be empty")
	case r.Language == "" || !bcp47Shape.MatchString(r.Language):
		return fmt.Errorf("meetings.persona.language %q is not a BCP-47 tag (for example en-US)", r.Language)
	}
	if n := len([]rune(r.DisplayName)); n == 0 || n > meetingsMaxBotNameChars {
		return fmt.Errorf("meetings.persona.display_name is %d characters, it must be 1 to %d", n, meetingsMaxBotNameChars)
	}
	if n := len([]rune(r.IntroTemplate)); n == 0 || n > meetingsMaxIntroChars {
		return fmt.Errorf("meetings.persona.intro_template is %d characters, it must be 1 to %d", n, meetingsMaxIntroChars)
	}
	switch r.Humor {
	case MeetingsPersonaHumorLight, MeetingsPersonaHumorOff:
	default:
		return fmt.Errorf("meetings.persona.humor %q is not light or off", r.Humor)
	}
	switch r.VoiceType {
	case MeetingsPersonaVoiceFemale, MeetingsPersonaVoiceMale, MeetingsPersonaVoiceBot:
	default:
		return fmt.Errorf("meetings.persona.voice_type %q is not female, male or bot", r.VoiceType)
	}
	if c := r.Unprompted.MinConfidence; c > 1 {
		return fmt.Errorf("meetings.persona.unprompted.min_confidence %v must be between 0 and 1", c)
	}
	u := p.Unprompted
	for _, f := range []struct {
		key string
		v   float64
	}{
		{"per_10m", float64(u.Per10m)}, {"min_gap_s", float64(u.MinGapS)}, {"per_meeting", float64(u.PerMeeting)},
		{"answer_wait_s", float64(u.AnswerWaitS)}, {"min_confidence", u.MinConfidence},
		{"queue_depth", float64(u.QueueDepth)}, {"stale_s", float64(u.StaleS)},
	} {
		if f.v < 0 {
			return fmt.Errorf("meetings.persona.unprompted.%s must not be negative (got %v)", f.key, f.v)
		}
	}
	if r.SearxngURL != "" {
		if err := validateMeetingsURL("meetings.persona.searxng_url", r.SearxngURL); err != nil {
			return err
		}
	}
	return p.Actions.Validate()
}
