package betty

import (
	"sort"
	"strings"
	"unicode"
)

// Trigger is what a line asks of the persona.
type Trigger struct {
	// Addressed: a configured name opens or closes the line.
	Addressed bool
	// Rest is the line without the name and greeting (lowercase words).
	Rest string
	// Intro: the line asks who the persona is.
	Intro bool
	// VoiceWanted: the request was spoken, or asks for a spoken answer.
	// Recorded for spoken replies (phase B); replies are chat for now.
	VoiceWanted bool
	// Misheard: a spoken line addressed her through a sound-alike of her
	// name ("Eddie, please introduce yourself"; wakeword.go).
	Misheard bool
	// The domain-aware correction of a request (normalise.go), set only
	// when speech-to-text mishearings of the product's words were fixed.
	// Rest is then the corrected request; HeardRest is Rest as heard;
	// Normalised is the whole line as most likely meant (the stored line
	// keeps the words as heard); Fixes lists the corrections ("high school
	// -> hive queue; ..."); Clarify is the short question she asks instead
	// of answering when a correction is unsure.
	HeardRest  string
	Normalised string
	Fixes      string
	Clarify    string
}

// sttVariants maps common speech-to-text spellings to the word they stand
// for, so "Bettie" or "Betty Bea" still address Betty.
var sttVariants = map[string]string{
	"bettie": "betty", "betti": "betty", "beddy": "betty", "beddie": "betty", "bety": "betty",
	"bea": "bee",
}

// greetings may come before a name that opens a line ("hey betty").
var greetings = map[string]bool{"hey": true, "hi": true, "hello": true, "ok": true, "okay": true, "yo": true, "so": true}

// requestStarters may follow a name that opens a line without a comma
// ("betty what is ..."), which tells a request from a sentence that merely
// starts with the name ("hive is slow today").
var requestStarters = map[string]bool{
	"what": true, "whats": true, "what's": true, "how": true, "why": true, "who": true, "who's": true,
	"when": true, "where": true, "which": true, "can": true, "could": true, "would": true, "will": true,
	"please": true, "tell": true, "give": true, "show": true, "find": true, "explain": true,
	"summarize": true, "summarise": true, "introduce": true, "remind": true, "check": true, "look": true,
	"any": true, "do": true, "does": true, "did": true, "say": true, "thoughts": true, "help": true,
}

// trailingBlock are words that, right before a name closing a line, make it
// a mention rather than an address ("push it to the hive").
var trailingBlock = map[string]bool{
	"the": true, "a": true, "an": true, "to": true, "in": true, "on": true, "of": true, "our": true,
	"my": true, "your": true, "this": true, "that": true, "from": true, "with": true, "for": true,
	"about": true, "into": true, "at": true, "by": true, "and": true, "or": true,
}

// introPhrases ask the persona to introduce itself when addressed.
var introPhrases = []string{"introduce yourself", "who are you", "who're you", "what are you", "what do you do", "tell us about yourself"}

// botPhrases ask who the bot is without addressing it.
var botPhrases = []string{"who's the bot", "who is the bot", "whos the bot", "what's the bot", "what is the bot", "who is that bot", "who's that bot"}

// voicePhrases ask for a spoken answer.
var voicePhrases = []string{"say it", "out loud", "aloud", "speak up", "tell us out loud"}

// strictGreetings may open a line before a strict alias ("hey hive").
var strictGreetings = map[string]bool{"hey": true, "ok": true, "okay": true}

// Matcher finds addressed lines.
type Matcher struct {
	// names are the aliases as word lists, longest first.
	names [][]string
	// strict marks aliases that are everyday words (an alias sharing no
	// word with the persona's name, such as "hive"): they address her only
	// as "hive, ..." or "hive: ...", after "hey" or "ok", or closing a
	// question as "..., hive?".
	strict map[string]bool
	who    []string
	// soundKeys are her name's sound keys for misheard wake words.
	soundKeys []string
}

// NewMatcher builds a matcher for a persona's name and aliases.
func NewMatcher(name string, aliases []string) *Matcher {
	m := &Matcher{strict: map[string]bool{}}
	seen := map[string]bool{}
	nameWords := map[string]bool{}
	for _, w := range normalize(words(name)) {
		nameWords[w] = true
	}
	for _, a := range append([]string{name}, aliases...) {
		w := normalize(words(a))
		key := strings.Join(w, " ")
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		m.names = append(m.names, w)
		shared := false
		for _, x := range w {
			shared = shared || nameWords[x]
		}
		if !shared {
			m.strict[key] = true
		}
	}
	sort.SliceStable(m.names, func(i, j int) bool { return len(m.names[i]) > len(m.names[j]) })
	if w := normalize(words(name)); len(w) > 0 {
		m.who = w
	}
	m.soundKeys = fuzzyKeys(m.names, m.strict)
	return m
}

func normalize(w []string) []string {
	out := make([]string, len(w))
	for i, x := range w {
		x = strings.Trim(x, "'")
		if v, ok := sttVariants[x]; ok {
			x = v
		}
		out[i] = x
	}
	return out
}

func hasPrefix(w, p []string) bool {
	if len(w) < len(p) {
		return false
	}
	for i := range p {
		if w[i] != p[i] {
			return false
		}
	}
	return true
}

func hasSuffix(w, s []string) bool {
	return len(w) >= len(s) && hasPrefix(w[len(w)-len(s):], s)
}

// spans are the byte ranges of raw's words, as words() splits them.
func spans(raw string) [][2]int {
	var out [][2]int
	start := -1
	for i, r := range raw {
		isWord := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\''
		switch {
		case isWord && start < 0:
			start = i
		case !isWord && start >= 0:
			out = append(out, [2]int{start, i})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, [2]int{start, len(raw)})
	}
	return out
}

// separatedAfter reports whether raw has one of marks right after the
// n-th word (the name), as in "Betty, ...".
func separatedAfter(raw string, n int, marks string) bool {
	sp := spans(raw)
	if n < 1 || n > len(sp) {
		return false
	}
	rest := strings.TrimLeft(raw[sp[n-1][1]:], " ")
	return rest != "" && strings.ContainsAny(rest[:1], marks)
}

// commaBefore reports whether the word at index i (counted from the
// start) follows a comma, as in "what now, hive?".
func commaBefore(raw string, i int) bool {
	sp := spans(raw)
	if i < 1 || i >= len(sp) {
		return false
	}
	return strings.HasSuffix(strings.TrimRight(raw[:sp[i][0]], " "), ",")
}

// looseMarks may follow a name that opens a line; strictMarks may follow
// a strict alias.
const (
	looseMarks  = ",:;!-.?"
	strictMarks = ",:"
)

// matchStrict applies the strict alias rules and returns the rest of the
// line when the alias addresses her.
func (m *Matcher) matchStrict(raw string, w, name []string, question bool) (string, bool) {
	start := 0
	if strictGreetings[w[0]] && hasPrefix(w[1:], name) {
		start = 1
	}
	if hasPrefix(w[start:], name) {
		after := w[start+len(name):]
		switch {
		case start == 1:
			return strings.Join(after, " "), true
		case len(after) == 0:
			// The alias alone counts only as a question: "Hive?".
			return "", question
		case separatedAfter(raw, len(name), strictMarks):
			return strings.Join(after, " "), true
		}
	}
	if question && len(w) > len(name) && hasSuffix(w, name) && commaBefore(raw, len(w)-len(name)) {
		return strings.Join(w[:len(w)-len(name)], " "), true
	}
	return "", false
}

// Match reads one line. spoken marks a speech segment (as opposed to chat).
func (m *Matcher) Match(raw string, spoken bool) Trigger {
	return m.MatchWith(raw, spoken, nil)
}

// MatchWith is Match knowing who is in the meeting: a spoken line opening
// with a sound-alike of her name and a request addresses her, unless the
// word is one of participants' names.
func (m *Matcher) MatchWith(raw string, spoken bool, participants []string) Trigger {
	w := normalize(words(raw))
	var t Trigger
	if len(w) == 0 {
		return t
	}
	low := strings.Join(w, " ")
	for _, p := range botPhrases {
		if strings.Contains(low, strings.Join(normalize(words(p)), " ")) {
			t.Intro = true
		}
	}
	question := strings.HasSuffix(strings.TrimSpace(raw), "?")
	for _, name := range m.names {
		if m.strict[strings.Join(name, " ")] {
			if rest, ok := m.matchStrict(raw, w, name, question); ok {
				t.Addressed, t.Rest = true, rest
				break
			}
			continue
		}
		start := 0
		if greetings[w[0]] && hasPrefix(w[1:], name) {
			start = 1
		}
		if hasPrefix(w[start:], name) {
			after := w[start+len(name):]
			ok := start == 1 || len(after) == 0 || requestStarters[after[0]] || len(name) > 1 ||
				separatedAfter(raw, start+len(name), looseMarks)
			if ok {
				t.Addressed, t.Rest = true, strings.Join(after, " ")
				break
			}
		}
		if len(w) > len(name) && hasSuffix(w, name) && !trailingBlock[w[len(w)-len(name)-1]] {
			t.Addressed, t.Rest = true, strings.Join(w[:len(w)-len(name)], " ")
			break
		}
		if len(w) == len(name) && hasPrefix(w, name) {
			t.Addressed = true
			break
		}
	}
	if !t.Addressed && spoken {
		if rest, ok := m.matchFuzzy(w, participants); ok {
			t.Addressed, t.Rest, t.Misheard = true, rest, true
		}
	}
	if t.Addressed {
		for _, p := range introPhrases {
			if strings.Contains(" "+t.Rest+" ", " "+p+" ") {
				t.Intro = true
			}
		}
		if len(m.who) > 0 && strings.Contains(low, "who is "+strings.Join(m.who, " ")) {
			t.Intro = true
		}
	} else if len(m.who) > 0 && (strings.Contains(low, "who is "+strings.Join(m.who, " ")) || strings.Contains(low, "who's "+strings.Join(m.who, " "))) {
		t.Intro = true
	}
	if t.Addressed && spoken {
		t.VoiceWanted = true
	}
	if t.Addressed {
		for _, p := range voicePhrases {
			if strings.Contains(" "+low+" ", " "+p+" ") {
				t.VoiceWanted = true
			}
		}
	}
	normaliseTrigger(&t, raw)
	return t
}
