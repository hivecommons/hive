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
	// VoiceBriefOverChars is the length past which a spoken answer is said
	// as its first sentence only, with the rest in chat: a CPU voice
	// synthesizes at about real time, so 200 characters (13 s of speech)
	// took 14 s to first audio and 25 s to say in a live Meet test, and a
	// longer one outran the 30 s say deadline.
	VoiceBriefOverChars = 120
	// VoiceBriefMaxChars bounds that first sentence.
	VoiceBriefMaxChars = 110
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
	// failed reports a deep dive that could not run or finish ("%s" is
	// the reason); still is the progress note while it runs long.
	failed string
	still  string
	// unsourced is said instead of a research figure that names no source;
	// empty falls back to English.
	unsourced string
}

// unsourcedText is the not-sure line for an unsourced research figure.
func (p phrases) unsourcedText() string {
	if p.unsourced != "" {
		return p.unsourced
	}
	return localized["en"].unsourced
}

// localized phrases by primary language subtag; English is the fallback.
var localized = map[string]phrases{
	"en": {
		acks:      []string{"Let me check on that.", "One moment.", "Good question, let me look.", "Let me see."},
		digIn:     "I'll dig into that and report back.",
		digged:    "I dug into %q: %s",
		failed:    "I couldn't look that up: %s.",
		still:     "Still looking into that; I'll report back as soon as I have it.",
		unsourced: "I'm not sure: what I found gave a number but no source for it, so I won't state it.",
	},
	"es": {
		acks:   []string{"Déjame revisarlo.", "Un segundo.", "Buena pregunta, lo miro.", "A ver."},
		digIn:  "Lo investigo y les cuento.",
		digged: "Investigué %q: %s",
		failed: "No pude investigarlo: %s.",
		still:  "Sigo investigándolo; les cuento en cuanto lo tenga.",
	},
	"fr": {
		acks:   []string{"Je vérifie.", "Un instant.", "Bonne question, je regarde.", "Voyons voir."},
		digIn:  "Je creuse la question et je reviens vers vous.",
		digged: "J'ai creusé %q : %s",
		failed: "Je n'ai pas pu faire la recherche : %s.",
		still:  "Je cherche encore ; je reviens vers vous dès que j'ai la réponse.",
	},
	"de": {
		acks:   []string{"Ich schaue kurz nach.", "Einen Moment.", "Gute Frage, ich sehe nach.", "Mal sehen."},
		digIn:  "Ich recherchiere das und melde mich.",
		digged: "Ich habe zu %q recherchiert: %s",
		failed: "Ich konnte das nicht nachschlagen: %s.",
		still:  "Ich recherchiere noch und melde mich, sobald ich es habe.",
	},
	"pt": {
		acks:   []string{"Deixa eu verificar.", "Só um instante.", "Boa pergunta, vou ver.", "Vamos ver."},
		digIn:  "Vou pesquisar e já volto com a resposta.",
		digged: "Pesquisei %q: %s",
		failed: "Não consegui pesquisar isso: %s.",
		still:  "Ainda estou pesquisando; aviso assim que tiver a resposta.",
	},
	"it": {
		acks:   []string{"Controllo subito.", "Un attimo.", "Bella domanda, verifico.", "Vediamo."},
		digIn:  "Approfondisco e vi faccio sapere.",
		digged: "Ho approfondito %q: %s",
		failed: "Non sono riuscita a cercarlo: %s.",
		still:  "Sto ancora cercando; vi faccio sapere appena ho la risposta.",
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

// BriefVoice is the short spoken form of a long spoken answer: its first
// sentence, when the answer is over VoiceBriefOverChars and has more
// sentences, the first fits VoiceBriefMaxChars and no sentence is cut. ok
// is false when the answer is said as it is.
func BriefVoice(text string) (string, bool) {
	text = strings.Join(strings.Fields(text), " ")
	if len([]rune(text)) <= VoiceBriefOverChars {
		return text, false
	}
	first := clipSentences(text, 1, VoiceBriefMaxChars+1)
	if first == text || strings.HasSuffix(first, "…") || len([]rune(first)) > VoiceBriefMaxChars {
		return text, false
	}
	return first, true
}
