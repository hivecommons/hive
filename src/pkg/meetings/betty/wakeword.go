package betty

import (
	"strings"
	"unicode/utf8"
)

// Tolerant wake words (live Discord test): speech-to-text heard "Betty,
// please introduce yourself" as "Eddie, please introduce yourself" and
// "ready, please introduce yourself". A spoken line that opens with a word
// that sounds like her name, followed by a request to her, addresses her,
// unless that word is the name of someone in the meeting.

// fuzzyMinKey is the shortest sound key compared loosely: shorter names
// ("Max" is "ax") would match too many everyday words.
const fuzzyMinKey = 3

// fuzzyMaxDistance is how far apart two sound keys may be, counting an
// inserted or dropped letter as 1 and a changed letter as 2: "Betsy"
// (edsi) still sounds like "Betty" (edi), "Kelly" (eli) does not.
const fuzzyMaxDistance = 1

// fuzzyStarters open a request to her after a misheard name. They are
// stricter than requestStarters: "ready when you are" or "ready, so"
// must not address her.
var fuzzyStarters = map[string]bool{
	"please": true, "introduce": true, "tell": true, "give": true, "show": true, "explain": true,
	"summarize": true, "summarise": true, "check": true, "what's": true, "whats": true, "who's": true,
}

// fuzzyYouStarters open a request only when "you" follows ("can you",
// "are you there"): "ready, can we start" is to the room.
var fuzzyYouStarters = map[string]bool{
	"can": true, "could": true, "would": true, "will": true, "are": true, "do": true, "what": true, "who": true,
}

// soundKey is a rough sound of a word: possessive dropped, y/ie/ey read
// as i, ea as e, t as d, doubled letters collapsed, and a leading
// consonant dropped (STT often loses or swaps the first sound).
func soundKey(w string) string {
	w = strings.ToLower(strings.Trim(w, "'"))
	w = strings.TrimSuffix(w, "'s")
	r := strings.NewReplacer("ie", "i", "ey", "i", "ea", "e", "y", "i", "t", "d")
	w = r.Replace(w)
	var b strings.Builder
	var last rune
	for _, c := range w {
		if c == last {
			continue
		}
		b.WriteRune(c)
		last = c
	}
	k := b.String()
	if c, size := utf8.DecodeRuneInString(k); size > 0 && !strings.ContainsRune("aeiou", c) {
		k = k[size:]
	}
	return k
}

// indelDistance is the edit distance with a substitution costing 2 (one
// drop plus one insert).
func indelDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			sub := 2
			if ar[i-1] == br[j-1] {
				sub = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+sub)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

// soundsLike reports whether word sounds close to one of keys.
func soundsLike(word string, keys []string) bool {
	k := soundKey(word)
	if utf8.RuneCountInString(k) < fuzzyMinKey {
		return false
	}
	for _, key := range keys {
		if indelDistance(k, key) <= fuzzyMaxDistance {
			return true
		}
	}
	return false
}

// fuzzyRequest reports whether after (the words following the misheard
// name) opens a request to her.
func fuzzyRequest(after []string) bool {
	if len(after) == 0 {
		return false
	}
	if fuzzyStarters[after[0]] {
		return true
	}
	if fuzzyYouStarters[after[0]] && len(after) > 1 && after[1] == "you" {
		return true
	}
	rest := " " + strings.Join(after, " ") + " "
	for _, p := range introPhrases {
		if strings.Contains(rest, " "+p+" ") {
			return true
		}
	}
	return false
}

// isParticipant reports whether word is (the first name of) someone in
// the meeting: a real "Eddie" is spoken to, not mistaken for her.
func isParticipant(word string, participants []string) bool {
	word = strings.TrimSuffix(strings.Trim(word, "'"), "'s")
	for _, p := range participants {
		w := words(p)
		if len(w) > 0 && (strings.EqualFold(w[0], word) || strings.EqualFold(strings.Join(w, ""), word)) {
			return true
		}
	}
	return false
}

// matchFuzzy is Match's last resort for a spoken line: a sound-alike of
// her name opening a request. It returns the request's words.
func (m *Matcher) matchFuzzy(w, participants []string) (string, bool) {
	if len(m.soundKeys) == 0 || len(w) < 2 {
		return "", false
	}
	start := 0
	if greetings[w[0]] {
		start = 1
	}
	if start >= len(w) {
		return "", false
	}
	word, after := w[start], w[start+1:]
	if !soundsLike(word, m.soundKeys) || isParticipant(word, participants) || !fuzzyRequest(after) {
		return "", false
	}
	return strings.Join(after, " "), true
}

// fuzzyKeys are the sound keys of the persona's own name words (never a
// strict alias such as "hive": an everyday word is matched exactly).
func fuzzyKeys(names [][]string, strict map[string]bool) []string {
	var keys []string
	seen := map[string]bool{}
	for _, n := range names {
		if strict[strings.Join(n, " ")] || len(n) == 0 {
			continue
		}
		k := soundKey(n[0])
		if utf8.RuneCountInString(k) < fuzzyMinKey || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	return keys
}
