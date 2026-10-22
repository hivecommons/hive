package classify

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/issueshape"
)

// Clarity score weights (hivecommons/hive#13577). They add up to 100; a
// contradiction marker subtracts ClarityContradictionPenalty. Acceptance
// criteria weigh most: their absence is what sends a merged fix back as a
// reopen.
const (
	ClarityGoalPoints           = 25
	ClarityCriteriaPoints       = 35
	ClarityScopePoints          = 20
	ClarityLengthPoints         = 20
	ClarityContradictionPenalty = 30

	// clarityDetailedBodyChars earns the full length points; a body at or
	// above the caller's minimum earns half.
	clarityDetailedBodyChars = 300
)

// Clarity signal names, as they appear in ClarityAssessment.Signals.
const (
	ClaritySignalGoal          = "goal"
	ClaritySignalCriteria      = "acceptance-criteria"
	ClaritySignalScope         = "scope"
	ClaritySignalDetailed      = "detailed-body"
	ClaritySignalShort         = "short-body"
	ClaritySignalContradiction = "contradiction"
)

var (
	clarityGoalHeadingRE = regexp.MustCompile(`(?im)^\s*(#{1,6}\s*|\*\*)\s*(goal|problem|summary|motivation|description|background|context|proposal|user story|what|why)\b`)
	clarityGoalWordsRE   = regexp.MustCompile(`(?i)\b(should|must|expected|want(s|ed)? to|needs? to|so that|in order to|goal|propos(e|al|ed)|instead of|fails?|failing|broken|crash(es|ed)?|error)\b`)

	clarityCriteriaHeadingRE = regexp.MustCompile(`(?im)^\s*(#{1,6}\s*|\*\*)?\s*(acceptance criteria|definition of done|done when|success criteria|expected (behaviou?r|result|outcome))\b`)
	clarityChecklistRE       = regexp.MustCompile(`(?m)^\s*[-*]\s+\[[ xX]\]\s+\S`)

	clarityCodeSpanRE   = regexp.MustCompile("`[^`\\n]+`")
	clarityPathRE       = regexp.MustCompile(`\b[\w.-]+/[\w.-]+(/[\w.-]+)*\b|\b[\w-]+\.(go|ts|js|py|md|ya?ml|json|sh|html|css)\b`)
	clarityScopeWordsRE = regexp.MustCompile(`(?i)\b(scope|in scope|out of scope|affects?|affected|component|module|package|endpoint|page|command|function|api|tab|settings?)\b`)

	clarityContradictionRE = regexp.MustCompile(`(?i)(\bTB[DC]\b|\?\?\?|\bnot sure (if|whether|what|how)\b|\bor maybe\b|\bcontradict)`)
)

// ClarityAssessment is the clarity gate's deterministic verdict on one issue
// body: a 0-100 score, the signals that earned it and one concise question
// per gap, ready to post to the author.
type ClarityAssessment struct {
	Score     int
	Signals   []string
	Questions []string
}

// ScoreClarity scores how clearly body states a goal, acceptance criteria
// and scope (hivecommons/hive#13577). It is a heuristic over the markdown,
// with no model call, so the same body always gets the same score and
// questions. minBodyChars is the body length below which the issue is
// considered short (runs.triage.min_body_chars).
func ScoreClarity(body string, minBodyChars int) ClarityAssessment {
	body = strings.TrimSpace(body)
	var a ClarityAssessment
	if clarityGoalHeadingRE.MatchString(body) || clarityGoalWordsRE.MatchString(body) {
		a.Score += ClarityGoalPoints
		a.Signals = append(a.Signals, ClaritySignalGoal)
	} else {
		a.Questions = append(a.Questions, "What problem should this solve, and what should happen once it is fixed? One or two sentences on the goal is enough.")
	}
	if clarityCriteriaHeadingRE.MatchString(body) || clarityChecklistRE.MatchString(body) {
		a.Score += ClarityCriteriaPoints
		a.Signals = append(a.Signals, ClaritySignalCriteria)
	} else {
		a.Questions = append(a.Questions, "How will we know it is done? Please add an **Acceptance criteria** list (`- [ ] …`) describing the observable result.")
	}
	if clarityCodeSpanRE.MatchString(body) || clarityPathRE.MatchString(body) || clarityScopeWordsRE.MatchString(body) {
		a.Score += ClarityScopePoints
		a.Signals = append(a.Signals, ClaritySignalScope)
	} else {
		a.Questions = append(a.Questions, "What is in scope (which command, page, file or component), and is anything explicitly out of scope?")
	}
	switch n := len(body); {
	case n >= clarityDetailedBodyChars:
		a.Score += ClarityLengthPoints
		a.Signals = append(a.Signals, ClaritySignalDetailed)
	case n >= minBodyChars:
		a.Score += ClarityLengthPoints / 2
	default:
		a.Signals = append(a.Signals, ClaritySignalShort)
		a.Questions = append(a.Questions, "Could you add a little more detail: steps to reproduce, an example, or a link to where this shows up?")
	}
	if marker := clarityContradiction(body); marker != "" {
		a.Score -= ClarityContradictionPenalty
		a.Signals = append(a.Signals, ClaritySignalContradiction)
		a.Questions = append(a.Questions, fmt.Sprintf("The issue still leaves a choice open (%q). Which option should be implemented?", marker))
	}
	if a.Score < 0 {
		a.Score = 0
	}
	return a
}

// clarityContradiction returns the first open-choice marker in body: an
// unchosen option list, an unsubstituted template placeholder, a TBD or an
// "or maybe" — or "".
func clarityContradiction(body string) string {
	if token, ok := issueshape.UnchosenOptionList(body); ok {
		return strings.TrimSpace(token)
	}
	if token, ok := issueshape.UnsubstitutedTemplatePlaceholder(body); ok {
		return strings.TrimSpace(token)
	}
	return clarityContradictionRE.FindString(body)
}
