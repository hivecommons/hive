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

// BuildPerspectivePrompt builds the per-PR review kick without the publish
// step, preserving the original behavior for callers that have not opted in.
func BuildPerspectivePrompt(p Perspective, pr PullRequest) string {
	return BuildPerspectivePromptOpts(p, pr, false)
}

// BuildPerspectivePromptOpts builds the kick and, when postComments is set,
// appends the instruction to publish the verdict on the PR through the
// `hive-review` relay.
//
// Without that step a reviewer's product is a JSON aggregate consumed only by
// the merge gate. On a hive that does not auto-merge, nothing consumes it: the
// review is performed, serialized, and discarded. Publishing is what turns it
// into help for the human who actually has to decide.
//
// Deliberately comment-only. approve/request_changes set a review state that
// influences merge, and that authority is not this agent's to exercise — the
// relay gates those separately, and the whole point here is to inform a human
// rather than to gate them.
func BuildPerspectivePromptOpts(p Perspective, pr PullRequest, postComments bool) string {
	return BuildPerspectivePromptWith(p, pr, PromptOptions{PostComments: postComments})
}

// PromptOptions carries the publish-side switches. It is a struct rather than
// more boolean parameters because these are independent policy choices that
// only ever travel together.
type PromptOptions struct {
	// PostComments tells the reviewer to publish its verdict on the PR.
	PostComments bool
	// AcknowledgeNoFindings makes a clean review visible. By default a
	// reviewer with nothing useful to say stays silent, which keeps the PR
	// uncluttered but is indistinguishable from a reviewer that never ran —
	// so on a hive whose review coverage is the thing being demonstrated,
	// silence reads as absence. This turns "nothing to report" into one short
	// line of evidence, deliberately capped at that.
	AcknowledgeNoFindings bool
}

// BuildPerspectivePromptWith is BuildPerspectivePromptOpts with the full set
// of publish-side options.
func BuildPerspectivePromptWith(p Perspective, pr PullRequest, opts PromptOptions) string {
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
	b.WriteString(buildReadInstruction(pr))
	b.WriteString("Return exactly one JSON object: the standard outputschema AgentReport fields plus perspective, verdict, repo, number, and head_sha.\n")
	b.WriteString("Required AgentReport fields: lane, kind, findings, prs_opened, beads_filed, summary. Set kind to \"review\" and lane to \"review-swarm\". Use [] for empty arrays.\n")
	b.WriteString("Allowed verdicts: approve, changes_requested, requires_human, reject. Finding severities: info, low, medium, high, critical.\n")
	b.WriteString("Use approve only when this perspective finds no blocker. Use changes_requested for agent-fixable issues. Use requires_human for ambiguous/high-risk judgment. Use reject for fundamentally unsuitable or harmful PRs.\n")
	if opts.PostComments {
		b.WriteString(buildPublishInstruction(pr, opts.AcknowledgeNoFindings))
	}
	return b.String()
}

// buildReadInstruction is the read half of the kick. The prompt has always
// demanded file:line citations, but never said how to obtain the diff — so
// whether the reviewer actually read the change was left to chance, and an
// agent that skipped it could still produce confident, ungrounded prose. That
// is the single worst thing this hive can put in front of a maintainer: a
// review that costs them time to refute. Name the commands, pin them to the
// dispatched head, and make "I could not read it" an explicit, honest outcome
// rather than a reason to guess.
func buildReadInstruction(pr PullRequest) string {
	var b strings.Builder
	b.WriteString("READ THE PR BEFORE YOU JUDGE IT.\n")
	fmt.Fprintf(&b, "  gh pr view %d --repo %s --json title,body,author,files,baseRefName\n", pr.Number, pr.Repo)
	fmt.Fprintf(&b, "  gh pr diff %d --repo %s\n", pr.Number, pr.Repo)
	b.WriteString("The body states what the author INTENDED; the diff is what they actually did. You need both — most of the findings worth reporting live in the gap between them.\n")
	b.WriteString("Reading is read-only and unrestricted: use gh freely here.\n")
	b.WriteString("Judge the diff, not the surrounding code. Pre-existing problems this PR does not touch are out of scope; raising them reads as an obstacle, not a review.\n")
	if pr.HeadSHA != "" {
		fmt.Fprintf(&b, "Your citations must come from that diff at head %s. If the PR has moved on since, review the current head and say which revision you read.\n", pr.HeadSHA)
	}
	b.WriteString("If you cannot read the diff — fetch failed, or it is too large — return verdict requires_human and say so. Never infer the contents of a diff you did not read: an invented file:line is worse than no review at all.\n\n")
	return b.String()
}

// buildPublishInstruction is the publish half of the kick: how to say what you
// found, and — more importantly — when to say nothing.
func buildPublishInstruction(pr PullRequest, acknowledgeNoFindings bool) string {
	var b strings.Builder
	b.WriteString("\nPUBLISH YOUR VERDICT.\n")
	b.WriteString("You produce TWO artifacts and both must be delivered: the comment a human reads, and the JSON verdict the hive routes on.\n")
	b.WriteString("Write the JSON to a file, then post the comment and hand over the verdict in the same call:\n")
	fmt.Fprintf(&b, "  hive-review %d --repo %s --comment --body-file <comment> --verdict-file <verdict>\n", pr.Number, pr.Repo)
	b.WriteString("Printing the JSON to your terminal does not deliver it, and you cannot write it into the metrics dir yourself — the relay is the only path. Omit --verdict-file and your judgement is lost: nothing is routed, nothing is escalated, and this PR is dispatched to you again from scratch.\n")
	b.WriteString("Use hive-review, never `gh pr review` — it is submitted with the App token and recorded on the audit trail.\n")
	b.WriteString("Only --comment. Do NOT approve, request changes, merge, close, or label; a human decides those.\n")
	b.WriteString("Every claim in the comment must cite file:line you actually read. A finding you cannot point at is a false positive, and it now costs a contributor their time to refute.\n")
	b.WriteString("Say nothing rather than pad. Do NOT post a comment that is only nits, only praise, or a restatement of the diff.\n")
	if acknowledgeNoFindings {
		b.WriteString("If this perspective found nothing a human needs, still post exactly one short line so the review is on the record:\n")
		b.WriteString("  **Reviewed** — no findings from this perspective.\n")
		b.WriteString("That line is the WHOLE comment. Do not append praise, a summary of the diff, a list of what you checked, or nits you just talked yourself out of; padding it is what makes an acknowledgement into noise.\n")
	} else {
		b.WriteString("If this perspective found nothing a human needs, skip the comment entirely — but still record the verdict, or this PR is dispatched to you again from scratch:\n")
		fmt.Fprintf(&b, "  hive-review %d --repo %s --record-verdict --verdict-file <verdict>\n", pr.Number, pr.Repo)
	}
	b.WriteString("If the PR body claims behavior the diff does not implement, say so with file:line — that gap is one of the most useful things you can report.\n")
	b.WriteString("Be brief and specific. One comment, at most a few findings, worst first.\n")
	b.WriteString(buildRoutingInstruction(pr))
	return b.String()
}

// buildRoutingInstruction is what turns a verdict into a decision.
//
// A hive that cannot merge produces reviews whose only possible outcome is a
// human acting on them. But a correct, well-cited comment buried in a queue of
// hundreds is not actionable: nothing distinguishes "a human must decide this"
// from routine review noise. Routing is therefore not a nicety on top of the
// review — it is the step that makes the review reach anyone.
func buildRoutingInstruction(pr PullRequest) string {
	var b strings.Builder
	b.WriteString("\nROUTING — make a needed human decision findable.\n")
	b.WriteString("If your verdict is requires_human or reject, the FIRST line of the comment must be exactly:\n")
	b.WriteString("  **HUMAN DECISION NEEDED** — <one line naming the decision only a human can make>\n")
	b.WriteString("A maintainer triaging a long queue filters on that marker; without it a blocking finding reads as one more comment and is skipped.\n")
	if handle := mentionableAuthor(pr.Author); handle != "" {
		fmt.Fprintf(&b, "On that same line, mention @%s (the PR author) so the person who can act is notified.\n", handle)
	} else {
		b.WriteString("Do NOT @-mention the PR author: this PR was opened by an app or bot account, so a mention notifies nobody. The marker line is the routing.\n")
	}
	b.WriteString("Report the limits of your own review. If you could not judge part of this PR — missing context, an unfamiliar subsystem, an ambiguous requirement, a change you cannot test — say so plainly and use requires_human. Naming what you could not verify is more useful than a confident guess, and omitting it is how an unreviewed change gets waved through on the strength of an automated approval.\n")
	return b.String()
}

// mentionableAuthor returns the bare @-handle for a PR author when mentioning
// it would reach a person, and "" when it would not.
//
// On a hive fleet most PRs are agent-authored, so the author is an App
// ("app/<name>") or a bot ("<name>[bot]"). @-mentioning either notifies no one
// — it renders as a link and nothing else — while still looking to a reader
// like the review was routed somewhere. That false signal is worse than no
// mention at all, because it suggests a human is already on it.
func mentionableAuthor(author string) string {
	a := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(author), "@"))
	if a == "" || strings.HasSuffix(a, "[bot]") || strings.Contains(a, "/") {
		return ""
	}
	return a
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
