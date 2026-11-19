package betty

import (
	"fmt"
	"regexp"
	"strings"
)

// Domain-aware correction after speech-to-text (live meeting test). The
// transcriber does not know the hive's words, so "how many issues are in
// the hive queue?" arrived as "how many issues are in the high school?",
// "Kubernetes" as "Cuba needs" and "KubeCon" as "cubicon", and she answered
// about a high school.
//
// The correction is an explicit table of misheard phrase to intended term,
// plus one small sound key for one-word spellings of "hive". It is applied
// only to a request addressed to her (after trigger matching), never to
// other people's lines and never to the stored transcript: the words as
// heard are kept and the corrected reading is carried beside them
// (Trigger.Normalised, Trigger.HeardRest). The table is English.

// Replacement is one correction applied to a request.
type Replacement struct {
	// Heard is the text as transcribed; Meant is what replaced it.
	Heard string `json:"heard"`
	Meant string `json:"meant"`
	// Unsure marks a correction made on the sound alone, with no other
	// product word in the request: she asks rather than assumes.
	Unsure bool `json:"unsure,omitempty"`
	// ask is the term named in the clarifying question.
	ask string
}

// Normalised is a request as heard and as most likely meant.
type Normalised struct {
	// Raw is the text as transcribed; Text is the corrected reading (equal
	// to Raw when nothing was corrected).
	Raw  string
	Text string
	// Applied lists the corrections, in the order they were made.
	Applied []Replacement
}

// Changed reports whether any correction was applied.
func (n Normalised) Changed() bool { return len(n.Applied) > 0 }

// Unsure reports whether a correction was applied with low confidence.
func (n Normalised) Unsure() bool { return n.Clarify() != "" }

// clarifyFormat is the short clarifying question for an unsure correction.
const clarifyFormat = "Did you mean %s?"

// Clarify is the short question she asks instead of answering when a
// correction is unsure ("Did you mean the hive queue?"), or "".
func (n Normalised) Clarify() string {
	for _, r := range n.Applied {
		if r.Unsure {
			return fmt.Sprintf(clarifyFormat, r.ask)
		}
	}
	return ""
}

// sttWhen says when a table row applies.
type sttWhen int

const (
	// sttAlways: the misheard phrase is corrected wherever it appears in a
	// request to her.
	sttAlways sttWhen = iota
	// sttWithCue: corrected only when the rest of the request names
	// something of the hive (issues, PRs, the queue, agents, the board).
	sttWithCue
	// sttCueOrAsk: corrected; without such a cue the correction is unsure
	// and she asks.
	sttCueOrAsk
)

// sttRule is one row of the table: a misheard phrase (word-boundary,
// case-insensitive) and the intended term.
type sttRule struct {
	re   *regexp.Regexp
	out  string
	when sttWhen
	// ask is the term named when she has to ask (sttCueOrAsk rows).
	ask string
}

// hiveQueueTerm is how the clarifying question names the hive queue.
const hiveQueueTerm = "the hive queue"

// sttTable is the reviewable table of mishearings, most specific first.
var sttTable = []sttRule{
	// pull request, board pressure (also fixed for every line by
	// NormalizeDomain; listed so a request carries the correction).
	{re: regexp.MustCompile(`(?i)\b(?:full|pool|poll|bull)[\s-]+request(s?)\b`), out: "pull request${1}"},
	{re: regexp.MustCompile(`(?i)\b(?:bored|bord|bore)\s+press(?:ure|er|ures)\b`), out: "board pressure"},
	{re: regexp.MustCompile(`(?i)\bboard\s+press(?:er|ures)\b`), out: "board pressure"},
	// Kubernetes, KubeCon, CNCF.
	{re: regexp.MustCompile(`(?i)\b(?:cuba needs|cuban eddies|cube and eddies|cooper netties|cuber netties|kuber netties|kubernetties)\b`), out: "Kubernetes"},
	{re: regexp.MustCompile(`(?i)\b(?:cubicon|cubecon|cubacon|cube[\s-]con|kube[\s-]con|cubic[\s-]con|cuba[\s-]con)\b`), out: "KubeCon"},
	{re: regexp.MustCompile(`(?i)\b(?:c\s+n\s+c\s+f|see\s+n\s+c\s+f|c\s+and\s+c\s+f|cnc\s+f|cn\s+cf)\b`), out: "CNCF"},
	// RFC, PR.
	{re: regexp.MustCompile(`(?i)\b(?:r|are|our)\s+f\s+(?:c|see|sea)(s?)\b`), out: "RFC${1}"},
	{re: regexp.MustCompile(`(?i)\b(?:pee\s+are|p\s+r)(s?)\b`), out: "PR${1}"},
	// quorum.
	{re: regexp.MustCompile(`(?i)\b(?:quorem|kworum|korum|corum|quarum)\b`), out: "quorum"},
	// Spektacular (only where a name fits, never the adjective), Spek.
	{re: regexp.MustCompile(`(?i)\b(in|from|ask|open|run|use|using|via|through)\s+spectacular\b`), out: "${1} Spektacular"},
	{re: regexp.MustCompile(`(?i)\bspectacular(\s+(?:sessions?|live mode|specs?|plans?|epics?|repo))\b`), out: "Spektacular${1}"},
	{re: regexp.MustCompile(`(?i)\bspeck\b`), out: "Spek"},
	// inbox, Plaud (only in a noun position).
	{re: regexp.MustCompile(`(?i)\b(the|my|our|your|hive|hive's)\s+in[\s-]box\b`), out: "${1} inbox"},
	{re: regexp.MustCompile(`(?i)\b(the|my|our|from|on|a|with|in)\s+(?:plod|plowed|plawd|plaude|plowd)\b`), out: "${1} Plaud"},
	{re: regexp.MustCompile(`(?i)\b(?:plod|plowed|plawd|plaude|plowd)(\s+(?:recordings?|notes?|sync|device|pin|app|transcripts?|inbox))\b`), out: "Plaud${1}"},
	// hive queue.
	{re: regexp.MustCompile(`(?i)\bhigh\s+school\s+(?:queue|q|cue|que)\b`), out: "hive queue"},
	{re: regexp.MustCompile(`(?i)\b(?:high|hive|hi|height)\s+of\s+(?:queue|q|cue|que)\b`), out: "hive queue"},
	{re: regexp.MustCompile(`(?i)\bhive\s+(?:q|cue|que)\b`), out: "hive queue"},
	{re: regexp.MustCompile(`(?i)\b(the|our)\s+high\s+(?:queue|q|cue|que)\b`), out: "${1} hive queue"},
	// "in the high school" is "in the hive queue" in a request about the
	// hive's work; with no such word she asks.
	{re: regexp.MustCompile(`(?i)\bin\s+(the|our)\s+high\s+school\b`), out: "in ${1} hive queue", when: sttCueOrAsk, ask: hiveQueueTerm},
	{re: regexp.MustCompile(`(?i)\b(the|our)\s+high\s+school\b`), out: "${1} hive queue", when: sttWithCue},
	// hive's: "Hives competitors". Only before a noun the product has, so
	// the plural ("how many hives are connected") is left alone.
	{re: regexp.MustCompile(`(?i)\bhives\s+(competitors?|roadmap|docs|documentation|dashboard|agents|queue|backlog|issues|prs|pull requests|repos?|status|website|license|governance|maintainers|contributors|features|architecture|community|inbox|board)\b`), out: "hive's ${1}"},
	// "tell me about Hives?" closing a clause.
	{re: regexp.MustCompile(`(?i)\b(about)\s+hives(\s*(?:[?.!,;]|$))`), out: "${1} hive${2}"},
	// "the high" in a noun position: closing a clause or before a verb,
	// never before a noun ("the high priority issues").
	{re: regexp.MustCompile(`(?i)\b(the|our|this)\s+high(\s+(?:is|has|was|does|doing|working|right now|today|currently)\b|\s*(?:[?.!,;]|$))`), out: "${1} hive${2}", when: sttWithCue},
}

// productCues are words that say a request is about the hive's work.
var productCues = map[string]bool{
	"issue": true, "issues": true, "pr": true, "prs": true, "queue": true, "queued": true,
	"agent": true, "agents": true, "board": true, "backlog": true, "merge": true, "merged": true,
	"merges": true, "repo": true, "repos": true, "repository": true, "repositories": true,
	"hive": true, "hive's": true, "spoke": true, "spokes": true, "hub": true, "bead": true,
	"beads": true, "rfc": true, "rfcs": true,
}

// productCuePhrases are the multi-word cues.
var productCuePhrases = []string{"pull request", "pull requests", "work item", "work items"}

// hasProductCue reports whether text names something of the hive.
func hasProductCue(text string) bool {
	w := words(text)
	for _, x := range w {
		if productCues[strings.Trim(x, "'")] {
			return true
		}
	}
	joined := " " + strings.Join(w, " ") + " "
	for _, p := range productCuePhrases {
		if strings.Contains(joined, " "+p+" ") {
			return true
		}
	}
	return false
}

// hiveWord is the product's name; hiveSound is its sound key.
const (
	hiveWord  = "hive"
	hiveSound = "hive"
)

// hiveSoundFolds reduce a one-word spelling to its sound: "hyve", "hi've",
// "high've" and "hife" all read "hive". The first letter is kept, so
// "five" and "dive" never match.
var hiveSoundFolds = strings.NewReplacer("'", "", "igh", "i", "ie", "i", "y", "i", "f", "v")

// soundsLikeHive reports whether one word is a misspelling of "hive" (not
// "hive" or the plural "hives" themselves) and whether it was possessive.
func soundsLikeHive(word string) (match, possessive bool) {
	w := strings.ToLower(word)
	if strings.HasSuffix(w, "'s") {
		w, possessive = strings.TrimSuffix(w, "'s"), true
	}
	if w == hiveWord || w == hiveWord+"s" || !strings.HasPrefix(w, "h") {
		return false, false
	}
	k := hiveSoundFolds.Replace(w)
	if k != hiveSound {
		k = strings.TrimSuffix(k, "s")
	}
	return k == hiveSound, possessive
}

// sttWord is one word, with inner apostrophes.
var sttWord = regexp.MustCompile(`[A-Za-z]+(?:'[A-Za-z]+)*`)

// applyRule replaces r's matches in text and records each real change.
func applyRule(text string, r sttRule, unsure bool, applied *[]Replacement) string {
	var b strings.Builder
	last := 0
	for _, m := range r.re.FindAllStringSubmatchIndex(text, -1) {
		heard := text[m[0]:m[1]]
		meant := string(r.re.ExpandString(nil, r.out, text, m))
		if strings.EqualFold(heard, meant) {
			continue
		}
		b.WriteString(text[last:m[0]])
		b.WriteString(meant)
		last = m[1]
		if applied != nil {
			*applied = append(*applied, Replacement{Heard: strings.TrimSpace(heard), Meant: strings.TrimSpace(meant), Unsure: unsure, ask: r.ask})
		}
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// fixHiveWords replaces one-word misspellings of "hive".
func fixHiveWords(text string, applied *[]Replacement) string {
	return sttWord.ReplaceAllStringFunc(text, func(w string) string {
		match, possessive := soundsLikeHive(w)
		if !match {
			return w
		}
		meant := hiveWord
		if possessive {
			meant += "'s"
		}
		if applied != nil {
			*applied = append(*applied, Replacement{Heard: w, Meant: meant})
		}
		return meant
	})
}

// NormaliseAddressed corrects known mishearings of the product vocabulary
// in a request addressed to the persona. The caller has matched the
// trigger already; it must not be called on other speakers' lines.
func NormaliseAddressed(text string) Normalised {
	return normalise(text, text)
}

// normalise corrects text. Whether a context-dependent row applies is
// decided on cue (the request without her name), so the same decisions are
// made for the request's words and for the whole line as transcribed.
func normalise(text, cue string) Normalised {
	n := Normalised{Raw: text, Text: text}
	for _, r := range sttTable {
		if !r.re.MatchString(n.Text) {
			continue
		}
		unsure := false
		if r.when != sttAlways {
			has := hasProductCue(r.re.ReplaceAllString(cue, " "))
			if !has && r.when == sttWithCue {
				continue
			}
			unsure = !has
		}
		n.Text = applyRule(n.Text, r, unsure, &n.Applied)
		cue = applyRule(cue, r, false, nil)
	}
	n.Text = fixHiveWords(n.Text, &n.Applied)
	return n
}

// normaliseTrigger carries the corrected reading on an addressed trigger:
// Rest becomes the corrected request (what retrieval, the hive status
// match and the topic use), HeardRest keeps the words as heard, and
// Normalised is the whole line as most likely meant, for the prompt.
func normaliseTrigger(t *Trigger, raw string) {
	if !t.Addressed || t.Intro || strings.TrimSpace(t.Rest) == "" {
		return
	}
	rest := normalise(t.Rest, t.Rest)
	if !rest.Changed() {
		return
	}
	t.HeardRest = t.Rest
	t.Rest = strings.Join(words(rest.Text), " ")
	t.Normalised = normalise(raw, t.HeardRest).Text
	t.Clarify = rest.Clarify()
	fixes := make([]string, 0, len(rest.Applied))
	for _, r := range rest.Applied {
		fixes = append(fixes, r.Heard+" -> "+r.Meant)
	}
	t.Fixes = strings.Join(fixes, "; ")
}

// heardAndMeant is the prompt line that shows both readings of a corrected
// request: "Heard: <raw>. Most likely meant: <normalised>."
func heardAndMeant(n Normalised) string {
	end := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" || strings.ContainsAny(s[len(s)-1:], ".?!") {
			return s
		}
		return s + "."
	}
	return "Heard: " + end(n.Raw) + " Most likely meant: " + end(n.Text)
}

// clarifyLines are the fixed clarifying questions the table can produce.
func clarifyLines() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range sttTable {
		if r.ask == "" || seen[r.ask] {
			continue
		}
		seen[r.ask] = true
		out = append(out, fmt.Sprintf(clarifyFormat, r.ask))
	}
	return out
}
