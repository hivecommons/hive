package betty

import (
	"sort"
	"strings"
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

// Matcher finds addressed lines.
type Matcher struct {
	// names are the aliases as word lists, longest first.
	names [][]string
	who   []string
}

// NewMatcher builds a matcher for a persona's name and aliases.
func NewMatcher(name string, aliases []string) *Matcher {
	m := &Matcher{}
	seen := map[string]bool{}
	for _, a := range append([]string{name}, aliases...) {
		w := normalize(words(a))
		key := strings.Join(w, " ")
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		m.names = append(m.names, w)
	}
	sort.SliceStable(m.names, func(i, j int) bool { return len(m.names[i]) > len(m.names[j]) })
	if w := normalize(words(name)); len(w) > 0 {
		m.who = w
	}
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

// separatedAfter reports whether raw has punctuation right after the n-th
// word (the name), as in "Betty, ...".
func separatedAfter(raw string, n int) bool {
	low := strings.ToLower(raw)
	seen := 0
	inWord := false
	for i, r := range low {
		isWord := r == '\'' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127
		switch {
		case isWord && !inWord:
			inWord = true
		case !isWord && inWord:
			inWord = false
			seen++
			if seen == n {
				rest := strings.TrimLeft(low[i:], " ")
				return rest != "" && strings.ContainsAny(rest[:1], ",:;!-.?")
			}
		}
	}
	return false
}

// Match reads one line. spoken marks a speech segment (as opposed to chat).
func (m *Matcher) Match(raw string, spoken bool) Trigger {
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
	for _, name := range m.names {
		start := 0
		if greetings[w[0]] && hasPrefix(w[1:], name) {
			start = 1
		}
		if hasPrefix(w[start:], name) {
			after := w[start+len(name):]
			ok := start == 1 || len(after) == 0 || requestStarters[after[0]] || len(name) > 1 ||
				separatedAfter(raw, start+len(name))
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
	return t
}
