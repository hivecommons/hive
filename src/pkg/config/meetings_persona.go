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
}

// MeetingsPersonaPresets are the built-in personas, keyed by ID.
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
		IntroTemplate:      "Hi, I'm {display_name} ({pronouns}). I take notes, keep this meeting's transcript, and answer questions from what the hive knows: its knowledge base and work items. Say my name to ask me something. Say \"{leave}\" and I'll buzz off.",
		PlainIntroTemplate: "Hi, I'm {display_name} ({pronouns}). I take notes, keep this meeting's transcript, and answer questions from the hive's knowledge base and work items. Say my name to ask me something, or say \"{leave}\" to remove me.",
		LeavePhrases:       []string{"betty leave", "hive leave"},
	},
}

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
	return strings.NewReplacer(
		"{name}", r.Name, "{display_name}", r.DisplayName, "{pronouns}", r.Pronouns,
		"{leave}", leave, "{accent}", r.Accent, "{voice_type}", r.VoiceType, "{language}", r.Language,
	).Replace(r.IntroTemplate)
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
	return nil
}
