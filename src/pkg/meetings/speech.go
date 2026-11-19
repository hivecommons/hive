package meetings

import (
	"regexp"
	"strings"
	"unicode"
)

// Text for speech (Meet test): the intro "Hi, I'm Betty Bee from the hive
// (she/her)." was read aloud as "she slash her". Everything hive says out
// loud goes through SpeechText first; chat keeps the text as written.

var (
	// speechPronouns is a pronoun parenthetical such as "(she/her)".
	speechPronouns = regexp.MustCompile(`(?i)\s*\((?:she|he|they|xe|ze|it|any)(?:\s*/\s*[a-z]+){1,2}\)`)
	// speechMDLink is a Markdown link: its text is said, the target not.
	speechMDLink = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	// speechURL is a bare link.
	speechURL = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+`)
	// speechRef is an issue or PR reference: "#123" or "org/repo#123".
	speechRef = regexp.MustCompile(`(?:\b[\w.-]+/[\w.-]+)?#(\d+)\b`)
	// speechMDLine is a Markdown heading, bullet or quote mark at a line
	// start.
	speechMDLine = regexp.MustCompile(`(?m)^\s*(?:#{1,6}\s+|[-*+]\s+|>\s*|\d+\.\s+)`)
	// speechEmphasis is Markdown emphasis and code marks.
	speechEmphasis = regexp.MustCompile("(\\*\\*|__|\\*|`+|~~)")
	// speechWordSlash is a slash between two words ("issues/PRs").
	speechWordSlash = regexp.MustCompile(`(\pL)\s*/\s*(\pL)`)
	// speechNumberSlash is a slash between numbers ("40/12").
	speechNumberSlash = regexp.MustCompile(`(\d)\s*/\s*(\d)`)
	// speechSpaces collapses whitespace.
	speechSpaces = regexp.MustCompile(`\s+`)
)

// speechLinkWord stands in for a link read aloud.
const speechLinkWord = "a link"

// SpeechText is text as it should be spoken: pronoun parentheticals
// dropped, Markdown marks removed (a link says its text), bare links said
// as "a link", "#123" as "number 123", slashes read as "or" between words
// and "and" between numbers, and emoji dropped.
func SpeechText(text string) string {
	s := speechPronouns.ReplaceAllString(text, "")
	s = speechMDLink.ReplaceAllString(s, "$1")
	s = speechURL.ReplaceAllString(s, speechLinkWord)
	s = speechRef.ReplaceAllString(s, "number $1")
	s = speechMDLine.ReplaceAllString(s, "")
	s = speechEmphasis.ReplaceAllString(s, "")
	s = speechNumberSlash.ReplaceAllString(s, "$1 and $2")
	s = speechWordSlash.ReplaceAllString(s, "$1 or $2")
	s = strings.ReplaceAll(s, "/", " ")
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.So, r), unicode.Is(unicode.Sk, r) && r > unicode.MaxASCII,
			r == '‍', r >= '︀' && r <= '️', r >= 0x1f3fb && r <= 0x1f3ff:
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(speechSpaces.ReplaceAllString(s, " "))
	// A dropped parenthetical or emoji can leave " ." or " ,".
	s = strings.NewReplacer(" .", ".", " ,", ",", " !", "!", " ?", "?").Replace(s)
	return s
}
