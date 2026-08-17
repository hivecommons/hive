package review

import (
	"fmt"
	"sort"
	"strings"
)

type PullRequest struct {
	Repo    string
	Number  int
	Title   string
	Author  string
	HeadSHA string
	URL     string
	Lane    string
}

func BuildPerspectivePrompt(p Perspective, pr PullRequest) string {
	focus := map[Perspective]string{
		PerspectiveCorrectness:     "correctness, regressions, edge cases, data races, and test adequacy",
		PerspectiveSecurity:        "exploitable vulnerabilities, unsafe permissions, injection, secrets, and trust-boundary regressions",
		PerspectiveIntentAlignment: "whether the diff solves the linked issue without unrelated scope creep",
		PerspectiveStyle:           "maintainability, conventions, readability, and repository idioms",
		PerspectiveDocsCurrency:    "documentation, examples, generated docs, and operator-facing text that must change with behavior",
	}[p]
	if focus == "" {
		focus = "the named review perspective"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[review-perspective:%s]\n", p)
	fmt.Fprintf(&b, "Review PR %s#%d", pr.Repo, pr.Number)
	if pr.Title != "" {
		fmt.Fprintf(&b, " — %s", pr.Title)
	}
	b.WriteString(".\n")
	if pr.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", pr.URL)
	}
	if pr.HeadSHA != "" {
		fmt.Fprintf(&b, "Head SHA: %s\n", pr.HeadSHA)
	}
	if pr.Author != "" {
		fmt.Fprintf(&b, "Author: @%s\n", strings.TrimPrefix(pr.Author, "@"))
	}
	fmt.Fprintf(&b, "Focus ONLY on %s. Do not duplicate other perspectives unless the issue is severe.\n\n", focus)
	b.WriteString("Return exactly one JSON object: the standard outputschema AgentReport fields plus perspective, verdict, repo, number, and head_sha.\n")
	b.WriteString("Required AgentReport fields: lane, kind, findings, prs_opened, beads_filed, summary. Set kind to \"review\" and lane to \"review-swarm\". Use [] for empty arrays.\n")
	b.WriteString("Allowed verdicts: approve, changes_requested, requires_human, reject. Finding severities: info, low, medium, high, critical.\n")
	b.WriteString("Use approve only when this perspective finds no blocker. Use changes_requested for agent-fixable issues. Use requires_human for ambiguous/high-risk judgment. Use reject for fundamentally unsuitable or harmful PRs.\n")
	return b.String()
}

func BuildPerspectivePrompts(pr PullRequest, perspectives []Perspective) map[Perspective]string {
	if len(perspectives) == 0 {
		perspectives = DefaultPerspectives
	}
	out := make(map[Perspective]string, len(perspectives))
	for _, p := range perspectives {
		out[p] = BuildPerspectivePrompt(p, pr)
	}
	return out
}

func BuildSequentialPrompt(pr PullRequest, perspectives []Perspective) string {
	prompts := BuildPerspectivePrompts(pr, perspectives)
	keys := make([]string, 0, len(prompts))
	for p := range prompts {
		keys = append(keys, string(p))
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("Run the following review perspectives sequentially. In production the governor may fan these out to parallel review-capable agents; this prompt is the phase-1 sequential fallback.\n\n")
	for _, k := range keys {
		b.WriteString(prompts[Perspective(k)])
		b.WriteString("\n---\n")
	}
	return b.String()
}
