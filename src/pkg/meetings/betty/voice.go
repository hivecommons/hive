package betty

import (
	"regexp"
	"strings"
)

// Voice reply limits (hivecommons/hive#18393).
const (
	// VoiceMaxSentences and VoiceMaxChars bound a spoken answer.
	VoiceMaxSentences = 2
	VoiceMaxChars     = 200
)

// Reply triggers added in phase B.
const (
	// TriggerAck is the short spoken acknowledgement sent before a spoken
	// answer.
	TriggerAck = "ack"
	// TriggerFollowUp is the one post that reports a deep dive's result.
	TriggerFollowUp = "follow_up"
)

// phrases holds the persona's fixed lines in one language.
type phrases struct {
	acks   []string
	digIn  string
	digged string
}

// localized phrases by primary language subtag; English is the fallback.
var localized = map[string]phrases{
	"en": {
		acks:   []string{"Let me check on that.", "One moment.", "Good question, let me look.", "Let me see."},
		digIn:  "I'll dig into that and report back.",
		digged: "I dug into %q: %s",
	},
	"es": {
		acks:   []string{"Déjame revisarlo.", "Un segundo.", "Buena pregunta, lo miro.", "A ver."},
		digIn:  "Lo investigo y les cuento.",
		digged: "Investigué %q: %s",
	},
	"fr": {
		acks:   []string{"Je vérifie.", "Un instant.", "Bonne question, je regarde.", "Voyons voir."},
		digIn:  "Je creuse la question et je reviens vers vous.",
		digged: "J'ai creusé %q : %s",
	},
	"de": {
		acks:   []string{"Ich schaue kurz nach.", "Einen Moment.", "Gute Frage, ich sehe nach.", "Mal sehen."},
		digIn:  "Ich recherchiere das und melde mich.",
		digged: "Ich habe zu %q recherchiert: %s",
	},
	"pt": {
		acks:   []string{"Deixa eu verificar.", "Só um instante.", "Boa pergunta, vou ver.", "Vamos ver."},
		digIn:  "Vou pesquisar e já volto com a resposta.",
		digged: "Pesquisei %q: %s",
	},
	"it": {
		acks:   []string{"Controllo subito.", "Un attimo.", "Bella domanda, verifico.", "Vediamo."},
		digIn:  "Approfondisco e vi faccio sapere.",
		digged: "Ho approfondito %q: %s",
	},
}

// phrasesFor picks a BCP-47 tag's phrases ("en-GB" uses "en").
func phrasesFor(lang string) phrases {
	primary := strings.ToLower(strings.SplitN(strings.TrimSpace(lang), "-", 2)[0])
	if p, ok := localized[primary]; ok {
		return p
	}
	return localized["en"]
}

var (
	// mdLink is a Markdown link; its text is kept.
	mdLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// bareURL is a URL or a www. host.
	bareURL = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s()<>]*[^\s()<>.,!?;:'"]`)
	// mdMarks are Markdown emphasis, code, heading and list marks.
	mdMarks = regexp.MustCompile("[*_`#>~]+")
	// listMark is a list bullet at a line start.
	listMark = regexp.MustCompile(`(?m)^\s*(?:[-+]|\d+[.)])\s+`)
	// emptyParens is what is left of "(https://...)".
	emptyParens = regexp.MustCompile(`\(\s*\)`)
	// spaceBeforePunct is what removing a link can leave: "see ."
	spaceBeforePunct = regexp.MustCompile(`\s+([.,!?;:])`)
)

// speakable removes what a voice must not read out: links and Markdown.
func speakable(text string) string {
	text = mdLink.ReplaceAllString(text, "$1")
	text = bareURL.ReplaceAllString(text, "")
	text = listMark.ReplaceAllString(text, "")
	text = mdMarks.ReplaceAllString(text, "")
	text = emptyParens.ReplaceAllString(text, "")
	text = strings.Join(strings.Fields(text), " ")
	return spaceBeforePunct.ReplaceAllString(text, "$1")
}

// ClipVoice bounds a spoken answer to two sentences and 200 characters,
// with no URLs or Markdown.
func ClipVoice(text string) string {
	return clipSentences(speakable(text), VoiceMaxSentences, VoiceMaxChars)
}

// clipSentences keeps the first n sentences, then at most max characters.
func clipSentences(text string, n, maxChars int) string {
	text = strings.Join(strings.Fields(text), " ")
	seen, cut := 0, -1
	for i, r := range text {
		if r == '.' || r == '!' || r == '?' {
			if i+1 == len(text) || text[i+1] == ' ' {
				seen++
				if seen == n {
					cut = i + 1
					break
				}
			}
		}
	}
	if cut > 0 {
		text = text[:cut]
	}
	return clip(text, maxChars)
}
