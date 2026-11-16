package betty

import (
	"sort"
	"strings"
	"unicode"
)

// minQuestionWords is the shortest line that counts as a room question.
const minQuestionWords = 4

// interrogatives start a question even without a "?" (speech-to-text often
// drops it).
var interrogatives = map[string]bool{
	"what": true, "why": true, "how": true, "when": true, "where": true, "which": true,
	"who": true, "whom": true, "whose": true, "whats": true, "hows": true, "wheres": true, "whos": true,
	"is": true, "are": true, "can": true, "could": true, "would": true, "should": true,
	"do": true, "does": true, "did": true, "will": true, "has": true, "have": true, "anyone": true,
}

// stopwords are dropped from topic keys.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "but": true, "not": true, "you": true, "your": true,
	"all": true, "any": true, "can": true, "had": true, "her": true, "was": true, "one": true, "our": true,
	"out": true, "has": true, "have": true, "his": true, "how": true, "its": true, "may": true, "new": true,
	"now": true, "own": true, "see": true, "two": true, "who": true, "did": true, "get": true, "got": true,
	"let": true, "say": true, "she": true, "too": true, "use": true, "way": true, "what": true, "when": true,
	"where": true, "which": true, "why": true, "with": true, "this": true, "that": true, "there": true,
	"their": true, "they": true, "them": true, "then": true, "than": true, "these": true, "those": true,
	"from": true, "into": true, "about": true, "would": true, "could": true, "should": true, "will": true,
	"does": true, "doing": true, "done": true, "just": true, "like": true, "know": true, "think": true,
	"anyone": true, "someone": true, "here": true, "were": true, "been": true, "being": true, "some": true,
	"also": true, "only": true, "very": true, "really": true, "yeah": true, "okay": true, "sure": true,
	"right": true, "well": true, "thing": true, "things": true, "whats": true, "hows": true, "wheres": true,
	"whos": true, "it's": true, "don't": true, "isn't": true, "aren't": true, "i'm": true, "we're": true,
	"you're": true, "they're": true, "there's": true, "that's": true, "what's": true, "how's": true,
	"where's": true, "who's": true, "can't": true, "won't": true, "didn't": true, "doesn't": true,
	"tell": true, "please": true, "thanks": true, "thank": true, "maybe": true, "still": true,
}

// words lowercases text and splits it into words, keeping inner
// apostrophes.
func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
}

// IsQuestion reports whether a line is a room question: it ends in "?" or
// starts with an interrogative, and has at least four words.
func IsQuestion(text string) bool {
	w := words(text)
	if len(w) < minQuestionWords {
		return false
	}
	return strings.HasSuffix(strings.TrimSpace(text), "?") || interrogatives[strings.Trim(w[0], "'")]
}

// TopicKey is a line's sorted, de-duplicated content words: three letters
// or more, stopwords dropped, a trailing plural "s" folded.
func TopicKey(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range words(text) {
		w = strings.Trim(w, "'")
		if len([]rune(w)) < 3 || stopwords[w] {
			continue
		}
		// A crude plural fold so "ship" and "ships" share a topic.
		if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
			w = w[:len(w)-1]
		}
		if seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

func intersect(a, b []string) int {
	set := make(map[string]bool, len(a))
	for _, x := range a {
		set[x] = true
	}
	n := 0
	for _, x := range b {
		if set[x] {
			n++
		}
	}
	return n
}

// Jaccard is |a∩b| / |a∪b| of two topic keys.
func Jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	in := intersect(a, b)
	return float64(in) / float64(len(a)+len(b)-in)
}

// Overlap is the share of a's words that b also has.
func Overlap(a, b []string) float64 {
	if len(a) == 0 {
		return 0
	}
	return float64(intersect(a, b)) / float64(len(a))
}

// containment is |a∩b| / min(|a|, |b|).
func containment(a, b []string) float64 {
	m := min(len(a), len(b))
	if m == 0 {
		return 0
	}
	return float64(intersect(a, b)) / float64(m)
}
