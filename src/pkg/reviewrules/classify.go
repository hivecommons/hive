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
)

type signal struct {
	name string
	re   *regexp.Regexp
}

// ruleSignals are the phrases that mark a comment as a rule. Matching is
// case-insensitive and on word boundaries; adding a phrase means adding a row
// here and a row to the table test.
var ruleSignals = []signal{
	{SignalSourceOfTruth, regexp.MustCompile(`(?i)\bsources?[- ]of[- ]truth\b`)},
	{SignalAuthoritative, regexp.MustCompile(`(?i)\bauthoritative\b`)},
	{SignalAlways, regexp.MustCompile(`(?i)\b(?:should|must|needs? to|has to|have to) always\b`)},
	{SignalNever, regexp.MustCompile(`(?i)\b(?:should|must) never\b`)},
	{SignalGoingForward, regexp.MustCompile(`(?i)\b(?:going forward|from now on|in (?:the )?future|next time|for future (?:analys[ie]s|assessments|runs|prs|work))\b`)},
	{SignalCaptureRequest, regexp.MustCompile(`(?i)\b(?:capture|record|document|write down|remember)\s+(?:this|that|it)\b`)},
	{SignalCriteria, regexp.MustCompile(`(?i)\bassessment (?:criteria|rubric|knowledge|guidance)\b`)},
	{SignalKnowledgeBase, regexp.MustCompile(`(?i)\bknowledge[- ]?base\b`)},
}

// foreignHomeHint matches wording that puts the rule's proper home in the
// reviewed project's own repository (its docs or criteria files), which the
// hive does not write to.
var foreignHomeHint = regexp.MustCompile(`(?i)\b(?:(?:in|to|into)\s+(?:your|this|the)\s+(?:repo|repository|project)\b|your\s+(?:docs|documentation|criteria|readme|contributing))`)

// metaClause matches a clause that asks for the capture rather than stating the
// rule ("capture this in the knowledge base"). It is dropped from the rule text
// so the stored rule is the rule.
var metaClause = regexp.MustCompile(`(?i)^\s*(?:please\s+|and\s+)?(?:capture|record|document|add|write down|remember)\s+(?:this|that|it)\b`)

var sentenceSplit = regexp.MustCompile(`[.!?]+\s+|\n+`)

// Classification is Classify's verdict.
type Classification struct {
	Kind Kind
	// Signals are the names of the signals that matched, in table order.
	Signals []string
}

// Classify says whether body states a rule for future work. It is a pure
// function of the text: every signal is an explicit phrase from ruleSignals.
func Classify(body string) Classification {
	var hit []string
	for _, s := range ruleSignals {
		if s.re.MatchString(body) {
			hit = append(hit, s.name)
		}
	}
	if len(hit) == 0 {
		return Classification{Kind: KindCorrection}
	}
	return Classification{Kind: KindRule, Signals: hit}
}

// ForeignHomeHint reports whether body asks for the rule to live in the
// reviewed project's own repository.
func ForeignHomeHint(body string) bool { return foreignHomeHint.MatchString(body) }

// ExtractRuleText returns the rule a comment states: the sentences that carry a
// signal, without the clauses that only ask for the capture, normalised and
// bounded. A comment with no signal yields its whole body, bounded.
func ExtractRuleText(body string) string {
	var kept []string
	for _, sent := range sentenceSplit.Split(body, -1) {
		if strings.TrimSpace(sent) == "" {
			continue
		}
		if Classify(sent).Kind == KindRule {
			kept = append(kept, sent)
		}
	}
	if len(kept) == 0 {
		kept = []string{body}
	}
	var clauses []string
	for _, sent := range kept {
		for _, c := range strings.Split(sent, ";") {
			if !metaClause.MatchString(c) {
				clauses = append(clauses, c)
			}
		}
	}
	if len(clauses) == 0 {
		clauses = kept
	}
	text := NormalizeText(strings.Join(clauses, "; "))
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
