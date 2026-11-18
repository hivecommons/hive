package betty

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Attribution (hivecommons/hive#18590): Betty works on several requests at
// once and answers each as soon as it is ready, so the order answers arrive
// in is not the order they were asked in. When that could confuse the room
// (more than one request in flight, other lines said since, or a late
// answer) an answer opens with a short topic phrase: "On open PRs: ...".
// A single quick answer stays short, with no prefix.
//
// She does not say the asker's name (owner ruling after the live test:
// "she does not need to respond with my name each time she responds"). A
// name is used only when the topic cannot tell two answers apart: two
// people asked about the same thing, so the answer opens "Andy (re: open
// PRs): ..." in chat, "Andy, about open PRs: ..." spoken. Acknowledgements
// never carry a name: the meeting bot pre-renders the plain ack lines, and
// only the exact text hits its speech cache.
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
	// sameTopicWindow is how long before a request another person's request
	// on the same topic still makes the answer name who asked.
	sameTopicWindow = time.Minute
	// minVocativeName is the shortest name stripped from an answer's
	// opening ("Andy, ...").
	minVocativeName = 2
)

// TriggerQueue is the one short chat note that tells the room she has
// several requests in hand.
const TriggerQueue = "queue"

// attrPhrases are the attribution formats in one language: topic takes
// the topic and the answer (no name); chat and voice take the name, the
// topic and the answer; the "bare" forms have no topic; queue takes the
// count.
type attrPhrases struct {
	topic            string
	chat, chatBare   string
	voice, voiceBare string
	queue            string
}

var attributions = map[string]attrPhrases{
	"en": {topic: "On %s: %s", chat: "%s (re: %s): %s", chatBare: "%s: %s", voice: "%s, about %s: %s", voiceBare: "%s, %s",
		queue: "I have %d questions; answering each as it's ready."},
	"es": {topic: "Sobre %s: %s", chat: "%s (sobre %s): %s", chatBare: "%s: %s", voice: "%s, sobre %s: %s", voiceBare: "%s, %s",
		queue: "Tengo %d preguntas; respondo cada una en cuanto esté lista."},
	"fr": {topic: "À propos de %s : %s", chat: "%s (à propos de %s) : %s", chatBare: "%s : %s", voice: "%s, à propos de %s : %s", voiceBare: "%s, %s",
		queue: "J'ai %d questions ; je réponds à chacune dès qu'elle est prête."},
	"de": {topic: "Zu %s: %s", chat: "%s (zu %s): %s", chatBare: "%s: %s", voice: "%s, zu %s: %s", voiceBare: "%s, %s",
		queue: "Ich habe %d Fragen; ich antworte auf jede, sobald sie fertig ist."},
	"pt": {topic: "Sobre %s: %s", chat: "%s (sobre %s): %s", chatBare: "%s: %s", voice: "%s, sobre %s: %s", voiceBare: "%s, %s",
		queue: "Tenho %d perguntas; respondo cada uma assim que estiver pronta."},
	"it": {topic: "Su %s: %s", chat: "%s (su %s): %s", chatBare: "%s: %s", voice: "%s, riguardo a %s: %s", voiceBare: "%s, %s",
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

// attribute opens text with what it is about, in the persona's language:
// "On open PRs: ...". It never names who asked (who is accepted so callers
// keep one signature); with no topic the text is returned as is.
func attribute(lang string, _ bool, _ string, topic, text string) string {
	if topic == "" {
		return text
	}
	// The answer was clipped to its limit already; the prefix (a few
	// words) rides on top rather than cutting the answer.
	return fmt.Sprintf(attrFor(lang).topic, topic, "") + text
}

// attributeNamed opens text with who asked and what about, for the one
// case a name is needed: two people asked about the same topic.
func attributeNamed(lang string, voice bool, who, topic, text string) string {
	p := attrFor(lang)
	name := displayName(who, voice)
	if name == "" {
		return attribute(lang, voice, who, topic, text)
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
	return prefix + text
}

// queueNote is the "I have N questions" note.
func queueNote(lang string, n int) string {
	return fmt.Sprintf(attrFor(lang).queue, n)
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

// needsAttribution reports whether p's answer must say what it is about:
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

// attributed is text as said for p: with what it is about in front when
// the order could be unclear (and who asked only when someone else asked
// about the same topic), else as is.
func (e *Engine) attributed(p *pending, voice bool, modelTopic, text string, now time.Time) string {
	if strings.TrimSpace(text) == "" || !e.needsAttribution(p, now) {
		return text
	}
	lang, topic := e.s.Persona.Language, requestTopic(p, modelTopic)
	if e.sameTopicAsker(p) {
		return attributeNamed(lang, voice, p.line.Who, topic, text)
	}
	return attribute(lang, voice, p.line.Who, topic, text)
}

// attributeReply sets rep's text as said for p (attributed) and records
// who asked and what about, so the session can attribute it when it
// delivers it late. An answer that opens by addressing the asker by name
// loses the name, and an unsure correction of what was heard
// (normalise.go) is answered with its short clarifying question.
func (e *Engine) attributeReply(p *pending, rep *Reply, modelTopic string, now time.Time) {
	rep.Asker, rep.Topic = p.line.Who, replyTopic(p, modelTopic)
	if q := p.trig.Clarify; q != "" && p.addressed {
		// "Did you mean the hive queue?" rather than a long answer about
		// the wrong thing, and no deep dive on it.
		rep.Kind, rep.Text = replyKind(p.trig), q
		rep.Research, rep.NeedsResearch, rep.Sources = "", false, nil
		return
	}
	rep.Text = stripAskerName(rep.Text, p.line.Who)
	before := rep.Text
	rep.Text = e.attributed(p, rep.VoiceWanted, modelTopic, rep.Text, now)
	rep.Attributed = rep.Text != before
}

// sameSpeaker reports whether two speaker names are one person.
func sameSpeaker(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// sameTopicAsker reports whether someone other than p's asker has asked
// about p's topic: a request of theirs is in flight, or they asked within
// sameTopicWindow before p or any time since. Only then does an answer
// name who it is for.
func (e *Engine) sameTopicAsker(p *pending) bool {
	if len(p.topic) == 0 {
		return false
	}
	for _, o := range e.pending {
		if o != p && !sameSpeaker(o.line.Who, p.line.Who) && Jaccard(o.topic, p.topic) >= dedupeJaccard {
			return true
		}
	}
	cut := p.created.Add(-sameTopicWindow)
	for _, l := range e.recent {
		if l.at.Before(cut) || strings.TrimSpace(l.Who) == "" || sameSpeaker(l.Who, p.line.Who) {
			continue
		}
		var key []string
		if t := e.match.Match(l.Text, false); t.Addressed {
			key = TopicKey(t.Rest)
		} else if IsQuestion(l.Text) {
			key = TopicKey(l.Text)
		}
		if Jaccard(key, p.topic) >= dedupeJaccard {
			return true
		}
	}
	return false
}

// vocativeOpeners may come before the asker's name at the start of an
// answer ("Yes, Andy, I can hear you").
const vocativeOpeners = `yes|no|sure|ok|okay|hi|hello|hey|thanks|well|right|great|sorry|good question|great question`

// stripAskerName removes the asker's name where an answer addresses them
// with it: opening the answer ("Andy, there are 12."), right after an
// opening word ("Yes, Andy, I can hear you.") or closing it ("Here you
// go, Andy."). A name inside a sentence is left alone.
func stripAskerName(text, who string) string {
	out := strings.TrimSpace(text)
	for _, name := range []string{displayName(who, false), displayName(who, true)} {
		if utf8.RuneCountInString(name) < minVocativeName {
			continue
		}
		q := regexp.QuoteMeta(name)
		opening, err := regexp.Compile(`(?i)^((?:` + vocativeOpeners + `),\s+)?` + q + `\s*[,:]\s*`)
		if err != nil {
			continue
		}
		if m := opening.FindStringSubmatch(out); m != nil {
			rest := out[len(m[0]):]
			if m[1] == "" {
				rest = upperFirst(rest)
			}
			out = m[1] + rest
		}
		closing, err := regexp.Compile(`(?i),\s*` + q + `\s*([.!?])$`)
		if err != nil {
			continue
		}
		out = closing.ReplaceAllString(out, "$1")
	}
	if out == strings.TrimSpace(text) {
		return text
	}
	return out
}

// upperFirst capitalises the first letter of a sentence.
func upperFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 || !unicode.IsLower(r) {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// replyTopic is requestTopic, falling back to the first words of the
// request itself ("are you there") when they hold no topic word: an
// answer delivered late must still say which question it answers.
func replyTopic(p *pending, model string) string {
	if t := requestTopic(p, model); t != "" {
		return t
	}
	rest := strings.TrimSpace(p.trig.Rest)
	if !p.addressed || rest == "" {
		rest = p.line.Text
	}
	return cleanTopic(strings.Join(strings.Fields(recase(p.line.Text, rest)), " "))
}

// ackFor is the next acknowledgement. It never names who it is for (who
// is accepted so callers keep one signature): the spoken ack must be
// exactly one of the persona's ack lines, which the meeting bot has
// pre-rendered, so it plays at once.
func (e *Engine) ackFor(_ string) string {
	return e.ack()
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
