package review

import (
	"fmt"
	"strings"
)

// untrustedFence returns a backtick fence longer than any backtick run in
// text, so the text cannot close the block it is quoted in.
func untrustedFence(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
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
	b.WriteString("If the PR summary claims behavior the diff does not implement, say so with file:line — that gap is one of the most useful things you can report.\n")
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
	b.WriteString("Before raising a maintainer decision, check the linked issue/spec and the PR body. If the answer is stated there, do not ask: cite it (`per #<issue>: ...`) and judge against it. Raise a decision question only when the answer is genuinely absent AND the choice is a risk class (permissions/tokens/secrets/credentials; security-sensitive paths such as .github/workflows, OWNERS, auth, crypto; data loss, destructive migrations, irreversible actions; merge/trust/ACMM-level/role policy; external side effects such as publishing, releases, billing, outbound email or notifications to humans; licensing/legal). Mark such a question with `<!-- hive-decision-risk: high -->` so it always reaches a person; naming, sequencing, defaults, config shape, scope and changelog wording are the hive's own call on a Hive-authored PR.\n")
	b.WriteString("A workflow diff that only removes or narrows `permissions:` scopes is NOT a permissions/sensitive-path risk class: do not raise a decision question or mark it high-risk for that reason alone.\n")
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

// permissionsTighteningGuidance is the direction-aware carve-out for the
// "security" risk class (hivecommons/hive#13068). A reviewer that read
// "edits workflow permissions" as human-owned risk parked hivecommons/pluk#305,
// a pure contents: write -> read narrowing with green CI, on a maintainer.
// The relay proves the same property structurally (WorkflowPermissionsTighteningOnly)
// and suppresses the permissions classes; this text keeps the model's verdict
// consistent with that code path instead of fighting it.
// hiveSupersedingGuidance tells the reviewer how its verdict is routed on
// the hive's own superseding copy of another PR (owner rule: those
// auto-merge; a requires_human there becomes a fix request, not a park).
const hiveSupersedingGuidance = "When a Hive-authored PR supersedes another PR (its body says `Supersedes #N`), it auto-merges once CI and review are clean and no operator decides on it: a requires_human you raise there — security-sensitive or not — is routed to the fixer as a concrete fix request, so state the remediation, not the question. Only the sentinel or an emergent problem with the head parks such a PR.\n"

const permissionsTighteningGuidance = "Narrowing GitHub Actions permissions is not a security risk a human must own: a workflow diff that only removes `permissions:` scopes or lowers them (write to read or none, a block replaced by `permissions: {}`) with no new job, step, trigger such as pull_request_target or workflow_run, `uses:` change, `secrets.*`/token reference, or added `id-token: write` is a mechanical reduction — approve it with an advisory note. Any widened or newly granted scope, or a tightening mixed with any other change, stays requires_human.\n"

// findingSchemaInstruction spells out the Finding object field by field. The
// relay validates the whole report against outputschema and a finding missing
// "summary" fails it — and in a combined review that discards every
// perspective's verdict at once. The first live combined review did exactly
// that: two well-cited findings, no summary key, nothing recorded. Naming only
// the top-level fields and leaving the finding shape to be guessed is not a
// schema, it is a trap.
// VerdictSchemaExample is one complete verdict object exactly as the relay
// validates it. It is quoted wherever an agent is told to produce a verdict —
// the cadence reviewer's kick template, and the relay's refusal message when a
// verdict fails validation — so the schema an agent is held to is never
// described only by reference. The queue-mode policy used to say "your kick
// names the exact schema" while the kick never did, and the reviewer filled
// the gap with {"repo","pr","verdict","summary"}: every verdict it produced
// was rejected and every PR it judged was handed straight back to it.
// TestVerdictSchemaExampleValidates keeps this literal honest.
const VerdictSchemaExample = `{"lane":"review-swarm","kind":"review","perspective":"correctness","verdict":"requires_human","confidence":0.75,"repo":"owner/repo","number":123,"head_sha":"<Head SHA, copied verbatim>","body_digest":"<Review body digest, copied verbatim>","summary":"one paragraph: the judgement and why","findings":[{"title":"short finding title","severity":"high","summary":"mechanism and consequence","file":"path/to/file.go","line":41,"review_scope":"in-scope"}],"prs_opened":[],"beads_filed":[]}`

const reportedConfidenceInstruction = "Each perspective MUST include confidence, chosen independently of its verdict from these fixed bins:\n" +
	"0.95 = claim verified directly in the diff and tests.\n" +
	"0.85 = verified in the diff, tests not checked or absent.\n" +
	"0.75 = inferred from context, not directly verified.\n" +
	"0.65 = plausible but uncertain.\n" +
	"0.55 = mostly a guess / outside this perspective's expertise.\n"

const findingSchemaInstruction = reportedConfidenceInstruction + "Allowed verdicts: approve, changes_requested, requires_human, reject.\n" +
	"Each element of findings is an object with these keys: title (string, required), severity (one of info, low, medium, high, critical, required), summary (string, required — the mechanism and consequence; this is the field the collector reads, so never put the body under another name such as description or body), file (string, optional), line (integer, optional), review_scope (required for new reports; exactly \"in-scope\" or \"out-of-scope\"). A finding missing title, severity or summary, or carrying any other review_scope value, fails validation and the ENTIRE verdict — every perspective — is discarded unrecorded.\n"

func perspectiveNames(ps []Perspective) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p))
	}
	return out
}

// writeAuthorModelLine names the PR's authoring model in the prompt header,
// next to the login author, when the attribution trailer recorded one.
func writeAuthorModelLine(b *strings.Builder, pr PullRequest) {
	if m := strings.TrimSpace(pr.AuthorModel); m != "" {
		fmt.Fprintf(b, "Author model: %s\n", m)
	}
}

// writeIndependentReviewSection adds the shared independent-review
// instruction when the planner chose a model for this kick.
func writeIndependentReviewSection(b *strings.Builder, opts PromptOptions) {
	if section := PerspectiveIndependentReviewSection(opts.ReviewBackend, opts.ReviewModel); section != "" {
		b.WriteString(section)
		b.WriteString("\n\n")
	}
}

// modelStampInstruction tells the reviewer which author_model and
// review_model values to put in its JSON report, so the verdict artifact
// carries the pair without the reviewer having to infer either. The planner
// also stamps both at collect time from the dispatch state (ModelStamps), so
// a reviewer that omits them still produces a complete record; the
// instruction makes the posted comment and the artifact agree.
func modelStampInstruction(pr PullRequest, opts PromptOptions) string {
	author := strings.TrimSpace(pr.AuthorModel)
	model := strings.TrimSpace(opts.ReviewModel)
	if author == "" && model == "" {
		return ""
	}
	var parts []string
	if author != "" {
		parts = append(parts, fmt.Sprintf("author_model to %q", author))
	}
	if model != "" {
		parts = append(parts, fmt.Sprintf("review_model to %q (the model the sub-agent ran with)", model))
	}
	return "Set " + strings.Join(parts, " and ") + ".\n"
}
