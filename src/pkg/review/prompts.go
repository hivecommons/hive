package review

import (
	"fmt"
	"sort"
	"strings"
)

type PullRequest struct {
	Repo        string
	Number      int
	Title       string
	Author      string
	HeadSHA     string
	URL         string
	Lane        string
	AuthorAgent string
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
	// Revise tells the reviewer it is re-examining a PR it has already
	// reviewed, and that its existing review must be corrected in place
	// rather than joined by a second one. Editing notifies nobody; posting
	// again notifies every subscriber, which is too high a price for the hive
	// correcting its own mistake.
	// Perspectives is the set this hive reviews with, and supplies the focus
	// text for each. The zero value means the built-in defaults.
	Perspectives PerspectiveSet
	Revise       bool
}

// BuildPerspectivePromptWith is BuildPerspectivePromptOpts with the full set
// of publish-side options.
func BuildPerspectivePromptWith(p Perspective, pr PullRequest, opts PromptOptions) string {
	focus := opts.Perspectives.Focus(p)
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
		b.WriteString(buildPublishInstruction(pr, opts.AcknowledgeNoFindings, opts.Revise))
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
	b.WriteString("THE DIFF ALONE IS NOT ENOUGH. Open the files it touches, and the callers of what it changes:\n")
	if pr.HeadSHA != "" {
		fmt.Fprintf(&b, "  gh api repos/%s/contents/<path>?ref=%s --jq .content | base64 -d\n", pr.Repo, pr.HeadSHA)
	} else {
		fmt.Fprintf(&b, "  gh api repos/%s/contents/<path> --jq .content | base64 -d\n", pr.Repo)
	}
	fmt.Fprintf(&b, "  gh search code --repo %s '<changed symbol>'   # who calls it\n", pr.Repo)
	b.WriteString("This was measured, not assumed: a reviewer reading the diff plus the surrounding tree found 67% of known defects at 1.4 false positives per PR, against 17% at 3.6 for the diff alone. Reading the tree is four times more effective AND quieter. A guard, early return, or caller you cannot see is the usual reason a real defect reads as fine.\n")
	b.WriteString("Read widely; report narrowly. Only defects this diff introduces or exposes are in scope — pre-existing problems it does not touch stay out, however tempting. Reading the surrounding code tells you whether the change is safe; it is not an invitation to review the file.\n")
	if pr.HeadSHA != "" {
		fmt.Fprintf(&b, "Every citation must be code you actually read at head %s — in the diff or in the files around it. If the PR has moved on since, review the current head and say which revision you read.\n", pr.HeadSHA)
	}
	b.WriteString("If you cannot read the diff — fetch failed, or it is too large — return verdict requires_human and say so. Never infer the contents of a diff you did not read: an invented file:line is worse than no review at all.\n\n")
	return b.String()
}

// buildPublishInstruction is the publish half of the kick: how to say what you
// found, and — more importantly — when to say nothing.
func buildPublishInstruction(pr PullRequest, acknowledgeNoFindings bool, revise bool) string {
	var b strings.Builder
	b.WriteString("\nPUBLISH YOUR VERDICT.\n")
	b.WriteString("You produce TWO artifacts and both must be delivered: the comment a human reads, and the JSON verdict the hive routes on.\n")
	if revise {
		b.WriteString("YOU HAVE REVIEWED THIS PR BEFORE. You are re-examining it because the reviewer was at fault, not the PR — an earlier review of yours was produced without reading the surrounding code, so its conclusion is not trustworthy.\n")
		b.WriteString("Correct your existing review in place. Do NOT add a second one:\n")
		fmt.Fprintf(&b, "  hive-review %d --repo %s --comment --body-file <comment> --verdict-file <verdict> --revise\n", pr.Number, pr.Repo)
		b.WriteString("--revise edits the review already on the PR, which notifies nobody. Posting again notifies every subscriber to say the hive changed its mind, and that cost lands on people who did nothing wrong.\n")
		b.WriteString("Write the comment you should have written the first time, not a diff against it. A maintainer rereading it must see one coherent review, with no reference to a previous version they may never have read.\n")
		b.WriteString("Reaching the same conclusion is a perfectly good outcome — say so plainly and the revision is skipped as unchanged. Do not manufacture a finding to justify the second look.\n")
	} else {
		b.WriteString("Write the JSON to a file, then post the comment and hand over the verdict in the same call:\n")
		fmt.Fprintf(&b, "  hive-review %d --repo %s --comment --body-file <comment> --verdict-file <verdict>\n", pr.Number, pr.Repo)
	}
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

// BuildCombinedPrompt is the kick for reviewing a PR from every perspective in
// one session, and it exists to break a false trade-off.
//
// One perspective per kick meant the only way to get security, scope, style and
// docs coverage was five kicks, and five kicks meant five review comments on
// one PR. That cost is what max_perspectives_per_pr was introduced to cap
// (#7562), and capping it at 1 is why nothing on this fleet has ever been
// reviewed for anything but correctness. Both settings were right given the
// choice available; the choice was the problem.
//
// One session covering all five produces one comment, so breadth stops costing
// the maintainer anything. It is also cheaper on the reviewer: reading the diff
// and the surrounding tree is the expensive part of a review and it is done
// once here instead of five times.
//
// The verdicts stay separate. Downstream routing is per-perspective -- the
// aggregate blocks if ANY perspective withholds approval -- so collapsing five
// judgments into one would quietly weaken the merge gate. One comment, five
// verdicts.
func BuildCombinedPrompt(pr PullRequest, perspectives []Perspective, opts PromptOptions) string {
	if len(perspectives) == 0 {
		perspectives = opts.Perspectives.List()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[review-perspective:%s]\n", strings.Join(perspectiveNames(perspectives), ","))
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
	fmt.Fprintf(&b, "\nJudge this PR from ALL %d perspectives below, in one pass. Read the PR once; apply every perspective to what you read.\n", len(perspectives))
	for _, p := range perspectives {
		fmt.Fprintf(&b, "  - %s — %s\n", p, opts.Perspectives.Focus(p))
	}
	b.WriteString("These are distinct questions, not one question asked several ways. A change can be correct and still leak a secret, or secure and still not do what its issue asked. Ask each one separately and answer it on its own evidence.\n")
	b.WriteString("Report each finding under exactly one perspective — whichever it most belongs to. Do not restate one finding under several to look thorough; that is the padding failure mode, multiplied.\n\n")
	b.WriteString(buildReadInstruction(pr))
	b.WriteString("Return exactly one JSON ARRAY containing one object per perspective — all ")
	fmt.Fprintf(&b, "%d of them, even the ones that found nothing.\n", len(perspectives))
	b.WriteString("Each object: the standard outputschema AgentReport fields plus perspective, verdict, repo, number, and head_sha.\n")
	b.WriteString("Required AgentReport fields: lane, kind, findings, prs_opened, beads_filed, summary. Set kind to \"review\" and lane to \"review-swarm\". Use [] for empty arrays.\n")
	fmt.Fprintf(&b, "Every object must carry the same repo (%s) and number (%d). Allowed perspective values: %s.\n", pr.Repo, pr.Number, joinPerspectives(perspectives))
	b.WriteString("Allowed verdicts: approve, changes_requested, requires_human, reject. Finding severities: info, low, medium, high, critical.\n")
	b.WriteString("Give each perspective its OWN verdict. Use approve only when THAT perspective finds no blocker. Use changes_requested for agent-fixable issues. Use requires_human for ambiguous/high-risk judgment. Use reject for fundamentally unsuitable or harmful PRs.\n")
	b.WriteString("A perspective you could not meaningfully assess is requires_human, not approve. Approving a perspective you did not actually consider is the one failure mode that makes this whole review worthless, because it is indistinguishable from having considered it.\n")
	if opts.PostComments {
		b.WriteString(buildCombinedPublishInstruction(pr, perspectives, opts))
	}
	return b.String()
}

// buildCombinedPublishInstruction is the publish half for a combined review.
// One comment carries every perspective, so the structure of that comment is
// what keeps five judgments legible to someone triaging a queue.
func buildCombinedPublishInstruction(pr PullRequest, perspectives []Perspective, opts PromptOptions) string {
	var b strings.Builder
	b.WriteString(buildPublishInstruction(pr, opts.AcknowledgeNoFindings, opts.Revise))
	b.WriteString("\nONE COMMENT, EVERY PERSPECTIVE.\n")
	fmt.Fprintf(&b, "You were kicked once for all %d perspectives and you post exactly ONE comment for all %d. Do not call hive-review once per perspective — that is the pile of comments this exists to avoid.\n", len(perspectives), len(perspectives))
	b.WriteString("Structure it so a maintainer can find the part they care about. Use a short `**<perspective>**` heading per perspective that HAS a finding, worst severity first.\n")
	b.WriteString("Perspectives that found nothing do NOT each get a line in the comment. Name them together on one closing line:\n")
	b.WriteString("  _No findings from: <comma-separated perspectives>._\n")
	b.WriteString("Five separate \"nothing to report\" paragraphs is the same noise as five separate comments, just collected into one place.\n")
	if opts.AcknowledgeNoFindings {
		b.WriteString("If NO perspective found anything, the whole comment is exactly one line:\n")
		// Named, not counted: "no findings from any of the 1 perspectives" is
		// what a single-perspective combined kick produced, and a count says
		// nothing a reader can check against the review anyway.
		fmt.Fprintf(&b, "  **Reviewed** — no findings from %s.\n", strings.Join(perspectiveNames(perspectives), ", "))
		b.WriteString("That line is the WHOLE comment. Do not list what you checked or append a summary of the diff.\n")
	}
	b.WriteString("The verdict file still carries all ")
	fmt.Fprintf(&b, "%d verdicts as a JSON array, including the clean ones — a perspective missing from the array reads downstream as never reviewed, and the PR comes back to you from scratch.\n", len(perspectives))
	return b.String()
}

func perspectiveNames(ps []Perspective) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p))
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
