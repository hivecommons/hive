package betty

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Citations (hivecommons/hive#18590): a chat answer says where its facts
// came from, from the context entries the model reported using: the link
// of a web hit, a work item's id, or the name of a knowledge fact, code
// map or the hive's live status. Spoken answers carry none (no links
// aloud).
const (
	// maxCitations bounds the sources named after one answer.
	maxCitations = 2
	// maxCitationChars bounds one named source.
	maxCitationChars = 80
)

// citeFormat is the citation suffix by primary language subtag.
var citeFormat = map[string]string{
	"en": "Source: ", "es": "Fuente: ", "fr": "Source : ", "de": "Quelle: ", "pt": "Fonte: ", "it": "Fonte: ",
}

// trailingURL is the link a web snippet's text ends with.
var trailingURL = regexp.MustCompile(`\((https?://[^\s()]+)\)\s*$`)

// citation names one snippet as a source.
func citation(s Snippet) string {
	switch {
	case strings.HasPrefix(s.Source, webSource+" "):
		if m := trailingURL.FindStringSubmatch(s.Text); m != nil {
			return m[1]
		}
		return strings.TrimPrefix(s.Source, webSource+" ")
	case strings.HasPrefix(s.Source, "work item "):
		return strings.TrimPrefix(s.Source, "work item ")
	case s.Source == HiveStatusSource:
		return "hive live status"
	case s.Title != "":
		return s.Source + " \"" + s.Title + "\""
	}
	return s.Source
}

// citeSources appends the sources an answer used to a chat answer, unless
// the answer already names them. Sources are the model's context numbers
// (1-based); unknown ones are skipped.
func citeSources(lang, text string, used []string, snips []Snippet) string {
	if strings.TrimSpace(text) == "" || len(used) == 0 {
		return text
	}
	var names []string
	seen := map[string]bool{}
	for _, u := range used {
		i, err := strconv.Atoi(strings.Trim(strings.TrimSpace(u), `"[]`))
		if err != nil || i < 1 || i > len(snips) {
			continue
		}
		c := strings.TrimSpace(citation(snips[i-1]))
		if c == "" || seen[c] || strings.Contains(text, c) {
			continue
		}
		seen[c] = true
		if utf8.RuneCountInString(c) > maxCitationChars {
			c = string([]rune(c)[:maxCitationChars-1]) + "…"
		}
		names = append(names, c)
		if len(names) == maxCitations {
			break
		}
	}
	if len(names) == 0 {
		return text
	}
	prefix := citeFormat[primaryLang(lang)]
	if prefix == "" {
		prefix = citeFormat["en"]
	}
	return strings.TrimSpace(text) + " (" + prefix + strings.Join(names, "; ") + ")"
}

func primaryLang(lang string) string {
	return strings.ToLower(strings.SplitN(strings.TrimSpace(lang), "-", 2)[0])
}

// unsurePhrases already say she is not sure, in the persona languages.
var unsurePhrases = []string{
	"not sure", "don't know", "do not know", "no sé", "no estoy segur", "pas sûr", "je ne sais pas",
	"nicht sicher", "weiß ich nicht", "não sei", "não tenho certeza", "non sono sicur", "non lo so",
}

// unsurePrefix opens a low-confidence answer, by language.
var unsurePrefix = map[string]string{
	"en": "I'm not sure, but ", "es": "No estoy segura, pero ", "fr": "Je ne suis pas sûre, mais ",
	"de": "Ich bin nicht sicher, aber ", "pt": "Não tenho certeza, mas ", "it": "Non ne sono sicura, ma ",
}

// hedge marks a low-confidence answer as unsure, unless it already says
// so, rather than stating a guess as fact.
func hedge(lang, text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return text
	}
	low := strings.ToLower(t)
	for _, p := range unsurePhrases {
		if strings.Contains(low, p) {
			return text
		}
	}
	prefix := unsurePrefix[primaryLang(lang)]
	if prefix == "" {
		prefix = unsurePrefix["en"]
	}
	return prefix + lowerInitial(t)
}

// lowerInitial lowercases the first letter unless it starts "I " or an
// acronym.
func lowerInitial(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 || r < 'A' || r > 'Z' {
		return s
	}
	next, _ := utf8.DecodeRuneInString(s[n:])
	if (r == 'I' && (next == ' ' || next == '\'')) || (next >= 'A' && next <= 'Z') || (next >= '0' && next <= '9') {
		return s
	}
	return string(r+('a'-'A')) + s[n:]
}
