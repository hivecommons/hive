// Package reviewrules captures the rules and sources of truth that people
// state in review feedback on the hive's own agent PRs, so the hive applies
// them to future work and not only to the PR they were written on
// (hivecommons/hive#19046).
//
// The review-feedback sweep (pkg/reviewfeedback, cmd/hive) kicks the authoring
// agent to fix the PR in hand. A fix applies to one PR; a stated rule ("the
// project's maintainers file is the source of truth for ownership") applies to
// every later analysis. This package makes the second half deterministic:
//
//   - Classify decides, from explicit phrase signals, whether a comment states
//     a rule or is a one-off correction.
//   - Store persists captured rules with their provenance, deduplicated by
//     comment id and by near-identical text, as scoped, pinned entries of a
//     local knowledge vault the kick primer loads for the matching agent.
//   - ParseBlocks reads the structured block the kicked agent may end its
//     reply with to sharpen a rule's wording or scope.
//
// Comment text is data. Nothing in it is executed, fetched or followed: it is
// matched against fixed phrases and, when it states a rule, quoted.
package reviewrules

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxRuleRunes bounds one rule's text. A rule is a sentence or two a reviewer
// wrote, not a document.
const MaxRuleRunes = 600

// Kind is how a comment is classified.
type Kind string

const (
	// KindCorrection applies to the PR the comment was left on only.
	KindCorrection Kind = "correction"
	// KindRule states something meant to hold for future work: a rule, a
	// criterion or a source of truth.
	KindRule Kind = "rule"
)

// Names of the explicit signals Classify matches. They are part of the log
// line and the rule's provenance, so a reader can see why a comment counted.
const (
	SignalSourceOfTruth  = "source-of-truth"
	SignalAuthoritative  = "authoritative"
	SignalAlways         = "always"
	SignalNever          = "never"
	SignalGoingForward   = "going-forward"
	SignalCaptureRequest = "capture-request"
	SignalCriteria       = "assessment-criteria"
	SignalKnowledgeBase  = "knowledge-base"
	// SignalOperatorPhrase is the signal an owner-configured extra phrase
	// (review_feedback.rules.extra_phrases) reports.
	SignalOperatorPhrase = "operator-phrase"
)

type signal struct {
	name string
	re   *regexp.Regexp
}

// ruleSignals are the phrases that mark a comment as a rule. Matching is
// case-insensitive and on word boundaries; adding a phrase means adding a row
// here, a row to SignalInfo and a row to the table test.
var ruleSignals = []signal{
	{SignalSourceOfTruth, regexp.MustCompile(`(?i)\bsources?[- ]of[- ]truth\b`)},
	{SignalAuthoritative, regexp.MustCompile(`(?i)\bauthoritative\b`)},
	{SignalAlways, regexp.MustCompile(`(?i)\b(?:should|must|needs? to|has to|have to) always\b`)},
	{SignalNever, regexp.MustCompile(`(?i)\b(?:should|must) never\b`)},
	{SignalGoingForward, regexp.MustCompile(`(?i)\b(?:going forward|moving forward|from now on|in (?:the )?future|for (?:the )?future|next time|future (?:analys[ie]s|assessments?|reviews?|runs|prs|work))\b`)},
	{SignalCaptureRequest, regexp.MustCompile(`(?i)\b(?:capture|record|document|write down|remember)\s+(?:this|that|it)\b`)},
	{SignalCriteria, regexp.MustCompile(`(?i)\bassessment (?:criteria|rubric|knowledge|guidance)\b`)},
	{SignalKnowledgeBase, regexp.MustCompile(`(?i)\bknowledge[- ]?base\b`)},
}

// SignalInfo names a built-in signal and gives the examples the Settings card
// and the docs show for it.
type SignalInfo struct {
	Name     string `json:"name"`
	Examples string `json:"examples"`
}

// Signals lists the built-in signals in table order.
func Signals() []SignalInfo {
	return []SignalInfo{
		{SignalSourceOfTruth, `"source of truth", "sources of truth"`},
		{SignalAuthoritative, `"authoritative"`},
		{SignalAlways, `"should always", "must always", "needs to always"`},
		{SignalNever, `"should never", "must never"`},
		{SignalGoingForward, `"going forward", "moving forward", "from now on", "in future", "next time", "future assessments"`},
		{SignalCaptureRequest, `"capture this", "document this", "record it", "remember that"`},
		{SignalCriteria, `"assessment criteria", "assessment rubric", "assessment knowledge"`},
		{SignalKnowledgeBase, `"knowledge base", "knowledge-base"`},
	}
}

// KnownSignal reports whether name is a built-in signal.
func KnownSignal(name string) bool {
	for _, s := range ruleSignals {
		if s.name == name {
			return true
		}
	}
	return false
}

// Phrase limits for owner-configured extra phrases.
const (
	MinPhraseRunes = 3
	MaxPhraseRunes = 80
	MaxPhrases     = 20
)

// ValidatePhrase reports why an owner-configured extra phrase is unusable, or
// "" when it is fine. A phrase is plain text, matched case-insensitively on
// word boundaries; it is quoted into a pattern, never compiled as one.
func ValidatePhrase(p string) string {
	n := utf8.RuneCountInString(strings.TrimSpace(p))
	switch {
	case n < MinPhraseRunes:
		return "is shorter than 3 characters"
	case n > MaxPhraseRunes:
		return "is longer than 80 characters"
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "contains a control character"
		}
	}
	return ""
}

// phraseSignal builds the matcher for one extra phrase: the literal text,
// case-insensitive, with any run of spaces matching any whitespace, bounded by
// word boundaries where the phrase starts or ends with a word character.
func phraseSignal(p string) (signal, bool) {
	p = strings.Join(strings.Fields(p), " ")
	if ValidatePhrase(p) != "" {
		return signal{}, false
	}
	quoted := strings.ReplaceAll(regexp.QuoteMeta(p), " ", `\s+`)
	pre, post := "", ""
	if isWordRune(rune(p[0])) {
		pre = `\b`
	}
	if isWordRune(rune(p[len(p)-1])) {
		post = `\b`
	}
	re, err := regexp.Compile(`(?i)` + pre + quoted + post)
	if err != nil {
		return signal{}, false
	}
	return signal{SignalOperatorPhrase, re}, true
}

func isWordRune(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// Classifier matches comments against a set of signals: the built-in table
// minus any disabled signal, plus owner-configured extra phrases.
type Classifier struct {
	signals []signal
}

// NewClassifier builds a classifier. disabled names built-in signals to
// ignore; extra are plain phrases that also mark a rule. Invalid phrases are
// skipped (the Settings API refuses them before they get here).
func NewClassifier(extra []string, disabled map[string]bool) Classifier {
	var c Classifier
	for _, s := range ruleSignals {
		if !disabled[s.name] {
			c.signals = append(c.signals, s)
		}
	}
	for _, p := range extra {
		if s, ok := phraseSignal(p); ok {
			c.signals = append(c.signals, s)
		}
	}
	return c
}

// defaultClassifier is the built-in table with no extras.
var defaultClassifier = NewClassifier(nil, nil)

// foreignHomeHint matches wording that puts the rule's proper home in the
// reviewed project's own repository (its docs or criteria files), which the
// hive does not write to.
var foreignHomeHint = regexp.MustCompile(`(?i)\b(?:(?:in|to|into)\s+(?:your|this|the)\s+(?:repo|repository|project)\b|your\s+(?:docs|documentation|criteria|readme|contributing))`)

// metaClause matches a clause that asks for the capture rather than stating the
// rule ("capture this in the knowledge base"). It is dropped from the rule text
// so the stored rule is the rule.
var metaClause = regexp.MustCompile(`(?i)^\s*(?:please\s+|and\s+)?(?:capture|record|document|add|write down|remember)\s+(?:this|that|it)\b`)

var sentenceSplit = regexp.MustCompile(`[.!?]+\s+|\n+`)

// quoteLine matches a Markdown block-quote line: text the reviewer quoted,
// typically the hive's own earlier reply, which is not the reviewer's rule.
var quoteLine = regexp.MustCompile(`^\s{0,3}>`)

// fenceLine opens or closes a fenced code block.
var fenceLine = regexp.MustCompile("^\\s{0,3}(?:```|~~~)")

// anaphor matches a word that refers back to something said elsewhere, so a
// short rule sentence carrying one does not stand on its own.
var anaphor = regexp.MustCompile(`(?i)\b(?:this|that|these|those|it)\b`)

// minStandaloneWords is the length under which a rule sentence that refers
// back ("we should consider this in future") is recorded with the rest of the
// comment, so the rule keeps what "this" is.
const minStandaloneWords = 8

// minRuleRunes is the shortest rule text that can stand alone. A shorter one
// ("Going forward.") is meaningless without the comment around it, so the
// whole comment is kept instead.
const minRuleRunes = 20

// StripQuoted removes block-quoted lines and fenced code blocks from body:
// what the reviewer quoted is not what the reviewer stated.
func StripQuoted(body string) string {
	var kept []string
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		if fenceLine.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence || quoteLine.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

type sentence struct {
	text     string
	question bool
}

// sentences splits body into sentences, remembering which ones end in a
// question mark. A dot inside a token (".project/maintainers.yaml") does not
// split: a sentence ends at punctuation followed by whitespace, or a newline.
func sentences(body string) []sentence {
	var out []sentence
	start := 0
	add := func(text, delim string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		q := strings.Contains(delim, "?") || strings.HasSuffix(strings.TrimSpace(text), "?")
		out = append(out, sentence{text: text, question: q})
	}
	for _, loc := range sentenceSplit.FindAllStringIndex(body, -1) {
		add(body[start:loc[0]], body[loc[0]:loc[1]])
		start = loc[1]
	}
	add(body[start:], "")
	return out
}

// Classification is Classify's verdict.
type Classification struct {
	Kind Kind
	// Signals are the names of the signals that matched, in table order.
	Signals []string
}

// Classify says whether body states a rule for future work with the built-in
// signals. See Classifier.Classify.
func Classify(body string) Classification { return defaultClassifier.Classify(body) }

// Classify says whether body states a rule for future work. It is a pure
// function of the text: every signal is an explicit phrase. Quoted lines and
// code blocks are ignored, and so are questions: "is there a way to add this
// to the knowledge base?" asks about capture, it does not state a rule.
func (c Classifier) Classify(body string) Classification {
	hit := map[string]bool{}
	for _, sent := range sentences(StripQuoted(body)) {
		if sent.question {
			continue
		}
		for _, s := range c.signals {
			if s.re.MatchString(sent.text) {
				hit[s.name] = true
			}
		}
	}
	if len(hit) == 0 {
		return Classification{Kind: KindCorrection}
	}
	var names []string
	seen := map[string]bool{}
	for _, s := range c.signals {
		if hit[s.name] && !seen[s.name] {
			seen[s.name] = true
			names = append(names, s.name)
		}
	}
	return Classification{Kind: KindRule, Signals: names}
}

// ForeignHomeHint reports whether body asks for the rule to live in the
// reviewed project's own repository.
func ForeignHomeHint(body string) bool { return foreignHomeHint.MatchString(StripQuoted(body)) }

// ExtractRuleText returns the rule a comment states with the built-in signals.
// See Classifier.ExtractRuleText.
func ExtractRuleText(body string) string { return defaultClassifier.ExtractRuleText(body) }

// ExtractRuleText returns the rule a comment states: the sentences that carry a
// signal, without the clauses that only ask for the capture, normalised and
// bounded. Quoted text and questions are never part of it. A comment with no
// signal yields its whole (unquoted) body, bounded, and so does one whose rule
// sentence is short and refers back to something else in the comment.
func (c Classifier) ExtractRuleText(body string) string {
	own := StripQuoted(body)
	if strings.TrimSpace(own) == "" {
		own = body
	}
	var kept []string
	for _, sent := range sentences(own) {
		if !sent.question && c.Classify(sent.text).Kind == KindRule {
			kept = append(kept, sent.text)
		}
	}
	if len(kept) == 0 {
		kept = []string{own}
	}
	var clauses []string
	for _, sent := range kept {
		for _, cl := range strings.Split(sent, ";") {
			if !metaClause.MatchString(cl) {
				clauses = append(clauses, cl)
			}
		}
	}
	if len(clauses) == 0 {
		clauses = kept
	}
	text := NormalizeText(strings.Join(clauses, "; "))
	if utf8.RuneCountInString(text) < minRuleRunes ||
		(len(strings.Fields(text)) < minStandaloneWords && anaphor.MatchString(text)) {
		text = NormalizeText(own)
	}
	return truncateRunes(text, MaxRuleRunes)
}

// NormalizeText turns control characters and runs of whitespace into single
// spaces, removes HTML comment delimiters (a rule is quoted into comments and
// files, and must not be able to open or close one) and trims.
func NormalizeText(s string) string {
	s = strings.ReplaceAll(s, "<!--", "")
	s = strings.ReplaceAll(s, "-->", "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}

var mentionRe = regexp.MustCompile("(^|[^\\w`@])@([A-Za-z0-9][A-Za-z0-9_-]*)")

// DefuseMentions wraps raw @handles in backticks so text quoted into a GitHub
// comment never notifies anyone.
func DefuseMentions(s string) string {
	return mentionRe.ReplaceAllString(s, "$1`@$2`")
}
