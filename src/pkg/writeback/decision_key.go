package writeback

import (
	"regexp"
	"strconv"
	"strings"
)

// Decision-question identity (hivecommons/hive#15499).
//
// The reviewer swarm asked the owner "should #15499 be closed as a no-op?"
// on six heads in a row — every merge-from-base drew a fresh review round
// and a fresh "Decision needed" comment, plus a fresh 085 entry comment.
// Content dedupe (normalizeSameItem) never caught them: a model rewrote
// the question each time, so no two bodies hashed alike. The identity of
// a decision question is not its wording. It is the PR the review named
// as already carrying the change (`<!-- hive-redundant-of: #N -->`) when
// there is one, else the question with everything a rewrite changes
// stripped: mentions, item references, digits, punctuation, case.

const (
	// NeedsDecisionMarker is the review-body marker of a decision-requested
	// verdict (the same spelling pkg/github's relay reads).
	NeedsDecisionMarker = "<!-- hive-verdict: needs-decision -->"
	// DecisionQuestionMarker opens the hidden question line.
	DecisionQuestionMarker = "<!-- hive-decision-question:"
	// RedundantOfMarker opens the hidden superseding-PR line.
	RedundantOfMarker = "<!-- hive-redundant-of:"

	decisionKeyRedundantPrefix = "redundant-of:"
	decisionKeyQuestionPrefix  = "question:"
	decisionKeyMaxWords        = 24
)

var reDecisionNonWord = regexp.MustCompile(`[^a-z ]+`)

// DecisionKey is the stable identity of a reviewer decision question:
// "redundant-of:N" when the review named the PR already carrying the
// change, else "question:<normalised words>" of the question text. Empty
// when neither is known.
func DecisionKey(redundantOf int, question string) string {
	if redundantOf > 0 {
		return decisionKeyRedundantPrefix + strconv.Itoa(redundantOf)
	}
	q := NormalizeDecisionQuestion(question)
	if q == "" {
		return ""
	}
	return decisionKeyQuestionPrefix + q
}

// NormalizeDecisionQuestion reduces a question to the words a rewrite
// keeps: lower-cased, mentions / item refs / digits / punctuation removed,
// stop words out, capped at decisionKeyMaxWords.
func NormalizeDecisionQuestion(question string) string {
	s := strings.ToLower(strings.TrimSpace(question))
	if s == "" {
		return ""
	}
	s = reMention.ReplaceAllString(s, " ")
	s = reItemRef.ReplaceAllString(s, " ")
	s = reDigits.ReplaceAllString(s, " ")
	s = reDecisionNonWord.ReplaceAllString(s, " ")
	words := make([]string, 0, decisionKeyMaxWords)
	for _, w := range strings.Fields(s) {
		if decisionStopWords[w] || len(w) < 3 {
			continue
		}
		words = append(words, w)
		if len(words) == decisionKeyMaxWords {
			break
		}
	}
	return strings.Join(words, " ")
}

// decisionStopWords are the words a rewrite swaps freely; they carry no
// identity.
var decisionStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "this": true, "that": true, "with": true,
	"should": true, "could": true, "would": true, "please": true, "decide": true,
	"whether": true, "about": true, "into": true, "from": true, "its": true,
	"are": true, "was": true, "has": true, "have": true, "been": true, "being": true,
	"not": true, "now": true, "you": true, "your": true, "our": true, "can": true,
	"will": true, "may": true, "one": true, "all": true, "any": true, "but": true,
	"than": true, "then": true, "also": true, "both": true, "either": true, "there": true,
}

// DecisionKeyFromBody reads a review or comment body: ok is true when it
// carries NeedsDecisionMarker, and key is DecisionKey of its markers.
func DecisionKeyFromBody(body string) (key string, ok bool) {
	hasMarker := false
	redundantOf := 0
	question := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		switch {
		case lower == NeedsDecisionMarker:
			hasMarker = true
		case strings.HasPrefix(lower, RedundantOfMarker) && strings.HasSuffix(lower, "-->"):
			redundantOf = parseItemNumber(trimmed[len(RedundantOfMarker):])
		case strings.HasPrefix(lower, DecisionQuestionMarker) && strings.HasSuffix(lower, "-->") && question == "":
			question = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(trimmed[len(DecisionQuestionMarker):]), "-->"))
		}
	}
	if !hasMarker {
		return "", false
	}
	return DecisionKey(redundantOf, question), true
}

func parseItemNumber(s string) int {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "-->"))
	s = strings.TrimPrefix(s, "#")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimPrefix(s, "#")
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
