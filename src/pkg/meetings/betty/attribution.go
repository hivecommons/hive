package betty

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Attribution (hivecommons/hive#18590): Betty works on several requests at
// once and answers each as soon as it is ready, so the order answers arrive
// in is not the order they were asked in. When that could confuse the room
// (more than one request in flight, other lines said since, or a late
// answer) every answer opens with who asked and a short topic phrase:
// "Andy (re: open PRs): ..." in chat, "Andy, about open PRs: ..." spoken.
// A single quick answer stays short, with no prefix.
//
// Names are written plain, never as "@name": a platform mention would ping
// the person, and on some chats "@everyone" style names would ping the room.
const (
	// attributeAfter is how late an answer may be before it names who and
	// what even when it was the only request.
	attributeAfter = 8 * time.Second
	// maxTopicWords bounds the topic phrase quoted in an attribution.
	maxTopicWords = 5
	// maxTopicChars bounds the topic phrase's length.
	maxTopicChars = 40
	// maxNameChars bounds the asker's name in an attribution.
	maxNameChars = 32
	// queueNoteGap is the least time between two "I have N questions"
	// notes.
	queueNoteGap = time.Minute
	// minQueueNote is how many requests must be in flight before the note.
	minQueueNote = 2
)

// TriggerQueue is the one short chat note that tells the room she has
// several requests in hand.
const TriggerQueue = "queue"

// attrPhrases are the attribution formats in one language: chat and voice
// take the name, the topic and the answer; the "bare" forms have no topic;
// ack takes the name and the acknowledgement; queue takes the count.
type attrPhrases struct {
	chat, chatBare   string
	voice, voiceBare string
	ack              string
	queue            string
}

var attributions = map[string]attrPhrases{
	"en": {chat: "%s (re: %s): %s", chatBare: "%s: %s", voice: "%s, about %s: %s", voiceBare: "%s, %s", ack: "%s, %s",
		queue: "I have %d questions; answering each as it's ready."},
	"es": {chat: "%s (sobre %s): %s", chatBare: "%s: %s", voice: "%s, sobre %s: %s", voiceBare: "%s, %s", ack: "%s, %s",
		queue: "Tengo %d preguntas; respondo cada una en cuanto esté lista."},
	"fr": {chat: "%s (à propos de %s) : %s", chatBare: "%s : %s", voice: "%s, à propos de %s : %s", voiceBare: "%s, %s", ack: "%s, %s",
		queue: "J'ai %d questions ; je réponds à chacune dès qu'elle est prête."},
	"de": {chat: "%s (zu %s): %s", chatBare: "%s: %s", voice: "%s, zu %s: %s", voiceBare: "%s, %s", ack: "%s, %s",
		queue: "Ich habe %d Fragen; ich antworte auf jede, sobald sie fertig ist."},
	"pt": {chat: "%s (sobre %s): %s", chatBare: "%s: %s", voice: "%s, sobre %s: %s", voiceBare: "%s, %s", ack: "%s, %s",
		queue: "Tenho %d perguntas; respondo cada uma assim que estiver pronta."},
	"it": {chat: "%s (su %s): %s", chatBare: "%s: %s", voice: "%s, riguardo a %s: %s", voiceBare: "%s, %s", ack: "%s, %s",
		queue: "Ho %d domande; rispondo a ciascuna appena è pronta."},
}

func attrFor(lang string) attrPhrases {
	primary := strings.ToLower(strings.SplitN(strings.TrimSpace(lang), "-", 2)[0])
	if p, ok := attributions[primary]; ok {
		return p
	}
	return attributions["en"]
}

// displayName is the asker's name as written in an attribution: chat keeps
// the display name, a spoken one only the first name. Mention marks are
// dropped so the text never pings anyone.
func displayName(who string, spoken bool) string {
	who = strings.Join(strings.Fields(strings.TrimLeft(strings.TrimSpace(who), "@#<")), " ")
	who = strings.TrimRight(who, ">")
	if spoken {
		if f := strings.Fields(who); len(f) > 0 {
			who = f[0]
		}
	}
	return clip(who, maxNameChars)
}

// topicFillers open or close a request without saying what it is about.
var topicFillers = map[string]bool{
	"many": true, "much": true, "me": true, "us": true, "please": true, "tell": true, "give": true,
	"show": true, "list": true, "quick": true, "quickly": true, "a": true, "an": true, "the": true,
	"of": true, "in": true, "on": true, "to": true, "for": true, "is": true, "are": true, "there": true,
	"right": true, "now": true, "currently": true, "today": true, "again": true, "remind": true,
	"explain": true, "check": true, "look": true, "up": true, "find": true, "out": true, "it": true,
	"i": true, "we": true, "our": true, "my": true, "be": true, "was": true, "were": true,
}

// recase gives lower's words the casing they have in original ("prs" in
// "Betty, what PRs are open?" becomes "PRs"): a trigger's Rest is
// lowercased.
func recase(original, lower string) string {
	orig := map[string]string{}
	for _, w := range strings.FieldsFunc(original, topicSep) {
		w = strings.Trim(w, "'.")
		if k := strings.ToLower(w); k != "" {
			if _, ok := orig[k]; !ok {
				orig[k] = w
			}
		}
	}
	out := strings.Fields(lower)
	for i, w := range out {
		if o, ok := orig[strings.Trim(w, "'.")]; ok {
			out[i] = o
		}
	}
	return strings.Join(out, " ")
}

func topicSep(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(",;:!?\"()[]", r) }

// topicPhrase is a short phrase for what a request is about, from its
// words: leading and trailing question and filler words dropped, at most
// maxTopicWords words, in the casing the person used ("open PRs").
func topicPhrase(text string) string {
	raw := strings.FieldsFunc(text, topicSep)
	filler := func(w string) bool {
		l := strings.Trim(strings.ToLower(w), "'.")
		return l == "" || interrogatives[l] || topicFillers[l] || stopwords[l]
	}
	i, j := 0, len(raw)
	for i < j && filler(raw[i]) {
		i++
	}
	for j > i && filler(raw[j-1]) {
		j--
	}
	w := raw[i:j]
	if len(w) > maxTopicWords {
		w = w[:maxTopicWords]
		for len(w) > 1 && filler(w[len(w)-1]) {
			w = w[:len(w)-1]
		}
	}
	return cleanTopic(strings.Join(w, " "))
}

// cleanTopic bounds a topic phrase (the model's or the heuristic's) and
// strips what must not be quoted: mention marks, quotes, newlines and
// trailing punctuation.
func cleanTopic(t string) string {
	t = strings.Join(strings.Fields(strings.NewReplacer("@", "", "\"", "", "`", "").Replace(t)), " ")
	if f := strings.Fields(t); len(f) > maxTopicWords+1 {
		t = strings.Join(f[:maxTopicWords+1], " ")
	}
	t = strings.TrimRight(t, ".,;:!? ")
	if utf8.RuneCountInString(t) > maxTopicChars {
		t = strings.TrimSpace(string([]rune(t)[:maxTopicChars]))
	}
	return t
}

// attribute opens text with who asked and what about, in the persona's
// language.
func attribute(lang string, voice bool, who, topic, text string) string {
	p := attrFor(lang)
	name := displayName(who, voice)
	if name == "" {
		return text
	}
	var prefix string
	switch {
	case voice && topic != "":
		prefix = fmt.Sprintf(p.voice, name, topic, "")
	case voice:
		prefix = fmt.Sprintf(p.voiceBare, name, "")
	case topic != "":
		prefix = fmt.Sprintf(p.chat, name, topic, "")
	default:
		prefix = fmt.Sprintf(p.chatBare, name, "")
	}
	// The answer was clipped to its limit already; the prefix (at most a
	// name and a few words) rides on top rather than cutting the answer.
	return prefix + text
}

// namedAck is an acknowledgement that names who it is for: "Andy, one
// moment."
func namedAck(lang, who, ack string) string {
	name := displayName(who, true)
	if name == "" {
		return ack
	}
	return fmt.Sprintf(attrFor(lang).ack, name, lowerFirst(ack))
}

// queueNote is the "I have N questions" note.
func queueNote(lang string, n int) string {
	return fmt.Sprintf(attrFor(lang).queue, n)
}

// lowerFirst lowercases a fixed phrase's first letter after a name prefix
// ("Andy, one moment."), leaving "I" and acronyms alone.
func lowerFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 || !unicode.IsUpper(r) {
		return s
	}
	next, _ := utf8.DecodeRuneInString(s[size:])
	if r == 'I' && (next == ' ' || next == '\'' || next == '’') {
		return s
	}
	if unicode.IsUpper(next) || unicode.IsDigit(next) {
		return s
	}
	return string(unicode.ToLower(r)) + s[size:]
}

// requestTopic is the topic phrase of a pending request: the model's label
// when it gave one, else the request's own words.
func requestTopic(p *pending, model string) string {
	if t := cleanTopic(model); t != "" {
		return t
	}
	if p.thoughts {
		return ""
	}
	if p.addressed {
		return topicPhrase(recase(p.line.Text, p.trig.Rest))
	}
	return topicPhrase(p.line.Text)
}

// inFlight counts the requests she is working on, but not except: the
// addressed ones, and room questions whose model call is running. Only
// addressed counts the addressed ones alone (what the room sees as
// "questions to her").
func (e *Engine) inFlight(except *pending, onlyAddressed bool) int {
	n := 0
	for _, o := range e.pending {
		if o == except || o.answered {
			continue
		}
		if o.addressed || (!onlyAddressed && o.asking) {
			n++
		}
	}
	return n
}

// markOverlap flags p and every other request in flight as overlapped.
func (e *Engine) markOverlap(p *pending) {
	for _, o := range e.pending {
		if o == p || o.answered || !(o.addressed || o.asking) {
			continue
		}
		o.overlapped, p.overlapped = true, true
	}
}

// needsAttribution reports whether p's answer must name who and what:
// another request overlapped it, the answer is late, or something else was
// said since it was asked (by someone else, or another answer of hers).
func (e *Engine) needsAttribution(p *pending, now time.Time) bool {
	if p.overlapped || now.Sub(p.created) > attributeAfter || e.lastAnswerAt.After(p.created) {
		return true
	}
	if e.inFlight(p, false) > 0 {
		return true
	}
	for _, l := range e.recent {
		if l.at.After(p.created) && !strings.EqualFold(l.Who, p.line.Who) {
			return true
		}
	}
	return false
}

// attributed is text as said for p: with who and what in front when the
// order could be unclear, else as is.
func (e *Engine) attributed(p *pending, voice bool, modelTopic, text string, now time.Time) string {
	if strings.TrimSpace(text) == "" || !e.needsAttribution(p, now) {
		return text
	}
	return attribute(e.s.Persona.Language, voice, p.line.Who, requestTopic(p, modelTopic), text)
}

// ackFor is the next acknowledgement, naming who it is for when other
// requests are in flight.
func (e *Engine) ackFor(who string) string {
	a := e.ack()
	if e.inFlight(nil, true) > 0 {
		return namedAck(e.s.Persona.Language, who, a)
	}
	return a
}

// noteQueue posts one short chat note when several requests are in hand,
// at most once per queueNoteGap.
func (e *Engine) noteQueue(now time.Time) {
	n := e.inFlight(nil, true)
	if n < minQueueNote || (!e.queueNoteAt.IsZero() && now.Sub(e.queueNoteAt) < queueNoteGap) {
		return
	}
	if e.emit(Reply{Kind: KindChat, Text: queueNote(e.s.Persona.Language, n), Trigger: TriggerQueue}) {
		e.queueNoteAt = now
	}
}
