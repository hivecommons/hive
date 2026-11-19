package config

import (
	"strings"
	"unicode/utf8"
)

// Spoken intro. The full intro (Intro) is about 16 seconds of speech and
// slow to synthesize, so the persona speaks a one or two sentence version
// and posts the full text in chat. The spoken intro comes from the built-in
// templates below only: there is no user-editable spoken template, and the
// settings API does not expose one.

// MeetingsSpokenLineMaxChars is the longest line the meeting bot's speech
// cache pre-renders. Every built-in persona's spoken intro fits in it.
const MeetingsSpokenLineMaxChars = 120

// meetingsSpokenIntroEnglish is the English spoken intro. Same
// placeholders as the intro template.
const meetingsSpokenIntroEnglish = "Hi, I'm {display_name}. I take notes and answer questions from the hive. The details are in chat."

// meetingsSpokenIntroTemplates are the spoken intros by primary language
// subtag. A language with no entry speaks the full intro.
var meetingsSpokenIntroTemplates = map[string]string{
	"en": meetingsSpokenIntroEnglish,
	"es": "Hola, soy {display_name}. Tomo notas y respondo preguntas de la colmena. Más detalles en el chat.",
	"fr": "Bonjour, je suis {display_name}. Je prends des notes et réponds aux questions. Détails dans le chat.",
	"it": "Ciao, sono {display_name}. Prendo appunti e rispondo alle domande dell'alveare. I dettagli sono in chat.",
	"pt": "Oi, eu sou a {display_name}. Faço anotações e respondo perguntas da colmeia. Os detalhes estão no chat.",
	"hi": "नमस्ते, मैं {display_name} हूँ। मैं नोट्स लेती हूँ और सवालों के जवाब देती हूँ। बाकी जानकारी चैट में है।",
	"ja": "こんにちは、{display_name}です。メモを取り、ハイブの情報をもとに質問にお答えします。詳しくはチャットをご覧ください。",
	"zh": "大家好，我是{display_name}。我负责记笔记，并根据蜂巢的知识回答问题。详细说明在聊天里。",
}

// meetingsPrimaryLanguage is a BCP-47 tag's primary subtag ("en-GB" is
// "en").
func meetingsPrimaryLanguage(tag string) string {
	return strings.ToLower(strings.SplitN(strings.TrimSpace(tag), "-", 2)[0])
}

// hasBuiltInIntro reports whether the persona's intro template is one of
// the built-in ones (not an operator's own text).
func (r ResolvedMeetingsPersona) hasBuiltInIntro() bool {
	for _, pre := range MeetingsPersonaPresets {
		if r.IntroTemplate == pre.IntroTemplate || r.IntroTemplate == pre.PlainIntroTemplate {
			return true
		}
	}
	return false
}

// SpokenIntro is the intro as spoken: one or two sentences, at most
// MeetingsSpokenLineMaxChars for every built-in persona. The full Intro
// goes to chat. An operator's own intro template that already fits the
// limit is spoken as written; a language with no short template speaks the
// full intro.
func (r ResolvedMeetingsPersona) SpokenIntro() string {
	full := r.Intro()
	if !r.hasBuiltInIntro() && utf8.RuneCountInString(full) <= MeetingsSpokenLineMaxChars {
		return full
	}
	tmpl, ok := meetingsSpokenIntroTemplates[meetingsPrimaryLanguage(r.Language)]
	if !ok {
		return full
	}
	t := r
	t.IntroTemplate = tmpl
	short := t.Intro()
	if utf8.RuneCountInString(short) > MeetingsSpokenLineMaxChars && r.Name != "" {
		// A long custom display name: the short name keeps the line short.
		t.DisplayName = r.Name
		short = t.Intro()
	}
	if strings.TrimSpace(short) == "" {
		return full
	}
	return short
}
