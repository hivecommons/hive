package betty

import (
	"context"
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

// HiveStatusRetriever returns the hive's live state for a question about
// the hive's own work: open issues and PRs, actionable, held and blocked
// counts, agents, merges and what is in progress.
type HiveStatusRetriever struct {
	// Brief is the dashboard's status summary (dashboard.Server.HiveStatusBrief).
	Brief func(ctx context.Context) (string, error)
}

// Name implements Retriever.
func (HiveStatusRetriever) Name() string { return "hive-status" }

// Retrieve implements Retriever.
func (h HiveStatusRetriever) Retrieve(ctx context.Context, query string, _ []string, _ int) []Snippet {
	if h.Brief == nil || !IsHiveStatusQuestion(query) {
		return nil
	}
	text, err := h.Brief(ctx)
	if err != nil || strings.TrimSpace(text) == "" {
		return nil
	}
	return []Snippet{{Source: HiveStatusSource, Title: "The hive right now", Text: clip(text, maxHiveStatusRunes)}}
}

// hiveStateWords name what the hive's live state counts. hiveStrongWords
// are unambiguous on their own; the rest need the hive (or "the repo")
// named in the question.
var (
	hiveStrongWords = []string{
		"open issues", "open prs", "open pull requests", "actionable", "recent merges", "recently merged",
		"board pressure", "bp",
	}
	hiveStateWords = []string{
		"issue", "issues", "pr", "prs", "pull request", "pull requests", "agent", "agents", "merge", "merges",
		"merged", "status", "held", "on hold", "blocked", "in progress", "backlog", "working on", "queue",
		"doing", "busy", "happening", "going on", "right now", "pressure", "board", "coverage", "health",
		"stale", "metrics", "numbers", "dashboard",
	}
	hiveNames = []string{"hive", "the repo", "this repo", "our repo"}
)

// IsHiveStatusQuestion reports whether text asks about the hive's own
// state.
func IsHiveStatusQuestion(text string) bool {
	t := " " + strings.Join(strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
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
	return has(hiveNames) && has(hiveStateWords)
}

// hasHiveStatus reports whether the snippets carry the live state.
func hasHiveStatus(snips []Snippet) bool {
	for _, s := range snips {
		if s.Source == HiveStatusSource {
			return true
		}
	}
	return false
}
