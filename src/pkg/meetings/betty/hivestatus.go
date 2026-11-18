package betty

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// Hive status questions (hivecommons/hive#18590 finding 9). "Betty, how
// many open issues are there?" is about the hive itself: it is answered from
// the hive's live dashboard state, in-process, and never sent to the
// research agents, which only see the public web.

// HiveStatusSource is the Snippet.Source of the hive's live state.
const HiveStatusSource = "hive live status"

// maxHiveStatusRunes bounds the live status snippet; it is longer than an
// ordinary snippet because it carries every count at once.
const maxHiveStatusRunes = 2000

// maxHiveItemRunes bounds the snippet for one issue or PR looked up by
// number (a summary of its description, state, review, CI and what blocks
// it), so the spoken answer stays under about 12 seconds.
const maxHiveItemRunes = 900

// HiveStatusRetriever returns the hive's live state for a question about
// the hive's own work: open issues and PRs, actionable, held and blocked
// counts, agents, merges and what is in progress.
type HiveStatusRetriever struct {
	// Brief is the dashboard's status summary (dashboard.Server.HiveStatusBrief).
	Brief func(ctx context.Context) (string, error)
	// Item describes one open issue or PR by number
	// (dashboard.Server.HiveItemBrief); optional.
	Item func(ctx context.Context, number int) (string, bool)
}

// Name implements Retriever.
func (HiveStatusRetriever) Name() string { return "hive-status" }

// Retrieve implements Retriever.
func (h HiveStatusRetriever) Retrieve(ctx context.Context, query string, _ []string, _ int) []Snippet {
	if h.Brief == nil || !IsHiveStatusQuestion(query) {
		return nil
	}
	var out []Snippet
	if n := itemNumber(NormalizeDomain(query)); n > 0 && h.Item != nil {
		if text, ok := h.Item(ctx, n); ok {
			out = append(out, Snippet{Source: HiveStatusSource, Title: "Open item " + strconv.Itoa(n), Text: clip(text, maxHiveItemRunes)})
		} else {
			out = append(out, Snippet{Source: HiveStatusSource, Title: "Open item " + strconv.Itoa(n),
				Text: "No open issue or PR numbered " + strconv.Itoa(n) + " in the hive's repos right now (it may be closed or merged, or in a repo the hive does not track)."})
		}
	}
	text, err := h.Brief(ctx)
	if err == nil && strings.TrimSpace(text) != "" {
		out = append(out, Snippet{Source: HiveStatusSource, Title: "The hive right now", Text: clip(text, maxHiveStatusRunes)})
	}
	return out
}

// itemRef finds an issue or PR number in a question: "pull request 18689",
// "PR #18689", "issue 12", "#18689".
var itemRef = regexp.MustCompile(`(?i)(?:\bpull\s+requests?|\bprs?|\bissues?|\bnumber)\s*#?\s*(\d{1,7})\b|#(\d{1,7})\b`)

// itemNumber is the issue or PR number a question names, or 0.
func itemNumber(text string) int {
	m := itemRef.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	for _, g := range m[1:] {
		if n, err := strconv.Atoi(g); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// sttDomainFixes are speech-to-text mishearings of the hive's vocabulary
// seen in live tests ("full requests", "pool request"), normalised before a
// line is matched or answered.
var sttDomainFixes = []struct {
	re  *regexp.Regexp
	out string
}{
	{regexp.MustCompile(`(?i)\b(?:full|pool|poll|bull|pull)[\s-]+requests\b`), "pull requests"},
	{regexp.MustCompile(`(?i)\b(?:full|pool|poll|bull)[\s-]+request\b`), "pull request"},
	{regexp.MustCompile(`(?i)\b(?:bored|bord|board)\s+press(?:ure|er|ures)\b`), "board pressure"},
	// STT writes "pull request 18,689": an item number has no separators.
	{regexp.MustCompile(`(?i)(\b(?:pull\s+requests?|prs?|issues?|number)\s*#?\s*)(\d{1,3}),(\d{3})\b`), "${1}${2}${3}"},
}

// NormalizeDomain fixes known mishearings of the hive's vocabulary.
func NormalizeDomain(text string) string {
	for _, f := range sttDomainFixes {
		text = f.re.ReplaceAllString(text, f.out)
	}
	return text
}

// hiveStateWords name what the hive's live state counts. hiveStrongWords
// are unambiguous on their own; the rest need the hive (or "the repo")
// named in the question.
var (
	hiveStrongWords = []string{
		"open issues", "open prs", "open pull requests", "actionable", "recent merges", "recently merged",
		"board pressure", "bp", "pull request", "pull requests", "prs", "how many issues", "total issues",
		"which agents", "agents running", "agents are running", "agents working", "agents are working",
	}
	hiveStateWords = []string{
		"issue", "issues", "pr", "prs", "pull request", "pull requests", "agent", "agents", "merge", "merges",
		"merged", "status", "held", "on hold", "blocked", "in progress", "backlog", "working on", "queue",
		"doing", "busy", "happening", "going on", "right now", "pressure", "board", "coverage", "health",
		"stale", "metrics", "numbers", "dashboard",
	}
	hiveNames = []string{"hive", "the repo", "this repo", "our repo"}
)

// attentionPhrases ask what is waiting on the speaker; attentionSubjects
// say it is about work items. Both are needed, so "pay attention to the
// demo" and "any issues with the audio" are not hive questions.
var (
	attentionPhrases = []string{
		"need my attention", "needs my attention", "needing my attention", "need attention", "needs attention",
		"waiting on me", "waiting for me", "awaiting me", "awaiting my review", "need me", "needs me",
		"need my review", "needs my review", "need my decision", "needs my decision", "need a decision", "needs a decision",
		"needs a human", "need a human", "needs human", "for me to review", "for me to look at",
	}
	attentionSubjects = []string{
		"issue", "issues", "pr", "prs", "pull request", "pull requests", "item", "items", "anything", "something",
		"things", "work", "review", "reviews", "decision", "decisions",
	}
)

// IsHiveStatusQuestion reports whether text asks about the hive's own
// state.
func IsHiveStatusQuestion(text string) bool {
	if itemNumber(NormalizeDomain(text)) > 0 && itemRefWord.MatchString(text) {
		return true
	}
	t := " " + strings.Join(strings.FieldsFunc(strings.ToLower(NormalizeDomain(text)), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}), " ") + " "
	has := func(words []string) bool {
		for _, w := range words {
			if strings.Contains(t, " "+w+" ") {
				return true
			}
		}
		return false
	}
	if has(hiveStrongWords) {
		return true
	}
	// "are there any issues that need my attention": about the hive's own
	// work waiting on the asker, never a web search or a research kick.
	if has(attentionPhrases) && has(attentionSubjects) {
		return true
	}
	return has(hiveNames) && has(hiveStateWords)
}

// itemRefWord is an explicit issue or PR word (a bare "#12" alone is not
// enough to call a line a hive question).
var itemRefWord = regexp.MustCompile(`(?i)\b(?:pull\s+requests?|prs?|issues?)\b`)

// hasHiveStatus reports whether the snippets carry the live state.
func hasHiveStatus(snips []Snippet) bool {
	for _, s := range snips {
		if s.Source == HiveStatusSource {
			return true
		}
	}
	return false
}
