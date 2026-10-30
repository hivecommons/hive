package review

import (
	"fmt"
	"strings"
)

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
//
// Layout (hivecommons/hive#12126): the header, then every instruction that is
// the same for all perspectives — scope contract, how to read the PR, the
// review-bot thread obligations, grounding, the output schema and the publish
// contract — emitted exactly ONCE, then one short section per perspective
// carrying only what differs: its focus and, for plan_match, the plan wave.
// The per-perspective kick repeats the shared block for every perspective it
// is sent for (~4–6 KiB each); here it is paid for once per session.
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
	if pr.BodyDigest != "" {
		fmt.Fprintf(&b, "Review body digest: %s (an opaque id: copy it verbatim into body_digest or leave the field out)\n", pr.BodyDigest)
	}
	if pr.Author != "" {
		fmt.Fprintf(&b, "Author: @%s\n", strings.TrimPrefix(pr.Author, "@"))
	}
	writeAuthorModelLine(&b, pr)
	fmt.Fprintf(&b, "\nJudge this PR from ALL %d perspectives, in one pass: %s. Read the PR once; apply every perspective to what you read.\n", len(perspectives), joinPerspectives(perspectives))
	b.WriteString("These are distinct questions, not one question asked several ways. A change can be correct and still leak a secret, or secure and still not do what its issue asked. Ask each one separately and answer it on its own evidence.\n")
	b.WriteString("Report each finding under exactly one perspective — whichever it most belongs to. Do not restate one finding under several to look thorough; that is the padding failure mode, multiplied.\n\n")
	writeIndependentReviewSection(&b, opts)

	b.WriteString(combinedSharedHeader)
	b.WriteString(scopeContractSection(pr))
	b.WriteString(buildReadInstruction(pr))
	b.WriteString(deltaSection(pr, opts.Delta))
	b.WriteString(buildReviewBotFindingsInstruction(pr, opts.ReviewBotLogins, opts.ReviewBotMinPriority))
	b.WriteString(groundingSectionWithBudget(pr, combinedGroundingBudget(len(perspectives)), fmt.Sprintf("this whole review, across all %d perspectives", len(perspectives))))
	writeReviewWritingGuide(&b, opts)
	b.WriteString("\nReturn exactly one JSON ARRAY containing one object per perspective — all ")
	fmt.Fprintf(&b, "%d of them, even the ones that found nothing.\n", len(perspectives))
	b.WriteString("Each object: the standard outputschema AgentReport fields plus perspective, verdict, confidence, repo, number, head_sha, and body_digest (copy head_sha and body_digest verbatim from above, or leave them out; never compute or summarize them).\n")
	b.WriteString("Required AgentReport fields: lane, kind, findings, prs_opened, beads_filed, summary. Set kind to \"review\" and lane to \"review-swarm\". Use [] for empty arrays.\n")
	fmt.Fprintf(&b, "Every object must carry the same repo (%s) and number (%d). Allowed perspective values: %s.\n", pr.Repo, pr.Number, joinPerspectives(perspectives))
	b.WriteString(modelStampInstruction(pr, opts))
	b.WriteString(findingSchemaInstruction)
	b.WriteString("Give each perspective its OWN verdict. Use approve only when THAT perspective finds no in-scope blocker. Use changes_requested for agent-fixable in-scope issues. Use requires_human for ambiguous/high-risk in-scope judgment. Use reject for fundamentally unsuitable or harmful PRs. Out-of-scope findings alone must not lower any verdict.\n")
	b.WriteString("For Hive-authored PRs, resolve sequencing, naming, config-shape, scope, and land-now-vs-later questions yourself: prefer approve with an advisory note, or changes_requested only for a concrete in-scope fix. Use requires_human only for risk a human must own: security, data loss, irreversible or public-facing behavior, policy/governance, licensing, spend, or when you could not read the diff.\n")
	b.WriteString(permissionsTighteningGuidance)
	b.WriteString("A Hive-authored incomplete fix with concrete remediation is changes_requested, not requires_human. If there are two engineering remedies (for example, add producer code OR reword a changelog), choose the best route yourself and state it as the requested change; do not escalate the choice to a human unless it falls into one of the risk categories above. Intent-alignment cleanup such as trimming a PR summary or changelog fragment that duplicates already-landed work is also changes_requested when the remediation is concrete.\n")
	b.WriteString("A perspective you could not meaningfully assess is requires_human, not approve. Approving a perspective you did not actually consider is the one failure mode that makes this whole review worthless, because it is indistinguishable from having considered it.\n")
	if head := strings.TrimSpace(opts.HeadRecordOnly); head != "" {
		// A combined review head (hivecommons/hive#15058): every perspective
		// in one pass, recorded as one array, posted nowhere.
		b.WriteString(buildHeadRecordOnlyInstruction(pr, head))
		fmt.Fprintf(&b, "The verdict file carries all %d verdicts as ONE JSON array, including the clean ones, recorded with a single --record-verdict call. A perspective missing from the array is dispatched to you again.\n", len(perspectives))
		b.WriteString(buildHeadFanOutInstruction(pr, perspectives, opts.ReviewBackend, opts.ReviewModel))
	} else if opts.PostComments {
		b.WriteString(buildCombinedPublishInstruction(pr, perspectives, opts))
	}
	b.WriteString(buildProposeOnlyInstruction(pr, opts))

	b.WriteString(combinedPerspectiveSections(pr, perspectives, opts))
	return b.String()
}

// buildHeadFanOutInstruction is the fan-out half of a combined review head
// kick (hivecommons/hive#15058). The head reads the PR once and then judges
// each perspective in its own parallel sub-agent, so one head kick costs
// about as long as its slowest perspective rather than the sum of all of
// them, and each perspective is still asked on its own evidence. The head
// stays the only caller of hive-review: sub-agents return verdict objects and
// the head records them as the one array the relay expects. Backend-neutral
// on purpose: Claude Code and Codex both run sub-agents, under different
// names. A kick carrying a single perspective (a re-ask) has nothing to fan
// out, so it gets no section.
func buildHeadFanOutInstruction(pr PullRequest, perspectives []Perspective, backend, model string) string {
	if len(perspectives) < 2 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nFAN OUT: ONE SUB-AGENT PER PERSPECTIVE, IN PARALLEL.\n")
	fmt.Fprintf(&b, "You orchestrate this review. The %d perspectives are independent questions, so answer them in parallel:\n", len(perspectives))
	fmt.Fprintf(&b, "1. Fetch the PR metadata, the full diff of %s#%d and any linked issue or contract ONCE, yourself, as the read instructions above describe.\n", pr.Repo, pr.Number)
	fmt.Fprintf(&b, "2. Start one sub-agent per perspective, all %d at once, IN PARALLEL, not one after another (Claude Code: the Task tool; Codex and other backends: their sub-agent facility). Give each sub-agent the diff, the PR metadata, the linked issue or contract text, the shared instructions above (scope contract, grounding, finding schema) and ONLY its own section from PERSPECTIVE SECTIONS below. Each sub-agent returns exactly one verdict object for its perspective.\n", len(perspectives))
	writeFanOutModelPin(&b, backend, model)
	fmt.Fprintf(&b, "   Grounding budget: the shared %d tool calls cover this whole review, not each sub-agent. Tell each sub-agent it may make at most %d investigation tool calls for its own perspective, so the total across all %d stays within the cap.\n", combinedGroundingBudget(len(perspectives)), fanOutSubAgentBudget(len(perspectives)), len(perspectives))
	b.WriteString("3. Sub-agents must NOT post anything on the PR, must NOT call hive-review, must NOT read other hive reviews of this head SHA, and must NOT fetch the PR or its diff again: they work from what you passed them. They may read the surrounding code in the checkout to ground a finding.\n")
	b.WriteString("4. Collect every result, check each object names its own perspective and carries this PR's repo, number, head_sha and body_digest, write them as ONE JSON array to the verdict file, and record it ONCE with the --record-verdict command above. Only you call hive-review.\n")
	b.WriteString("If sub-agents are unavailable, or one fails or returns no usable verdict, review those perspectives yourself one by one in this session. The array contract is unchanged either way.\n")
	return b.String()
}

// fanOutSubAgentBudget splits the combined grounding budget evenly across the
// per-perspective sub-agents of a fan-out (hivecommons/hive#15435), so the
// shared "whole review" cap is not read as each sub-agent's own.
func fanOutSubAgentBudget(n int) int {
	if n < 1 {
		n = 1
	}
	return max(1, combinedGroundingBudget(n)/n)
}

// writeFanOutModelPin keeps the head's model guarantee across the fan-out.
// A head IS its model (review_heads: each head's report is stamped with the
// model that head ran), and its kick already carries the independent-review
// instruction naming one sub-agent pinned to that model. The fan-out replaces
// that one sub-agent with one per perspective, so each of them must be pinned
// to the same model; a backend whose sub-agent facility defaults to another
// (often cheaper) model would otherwise produce verdicts under the head's name
// that the head's model never made.
func writeFanOutModelPin(b *strings.Builder, backend, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		b.WriteString("   Run every sub-agent on the model you are running on now; never let one default to a different or cheaper model.\n")
		return
	}
	target := model
	if bk := strings.TrimSpace(backend); bk != "" {
		target = bk + "/" + model
	}
	fmt.Fprintf(b, "   Launch EVERY sub-agent with `model` set to `%s`, this head's model. These per-perspective sub-agents ARE the independent review described above: launch them instead of that single sub-agent, not in addition to it. A sub-agent on any other model must not contribute a verdict; if your sub-agent facility cannot pin `%s`, review the perspectives yourself one by one. Every verdict object records review_model %q.\n", target, model, model)
}

// combinedSharedHeader opens the once-per-session block of a combined kick so
// the reviewer knows the instructions that follow are not repeated per
// perspective and must be carried into each section below.
const combinedSharedHeader = "SHARED INSTRUCTIONS — stated once, binding for every perspective below.\n" +
	"Everything from here to the per-perspective sections at the end applies to each perspective exactly as if it had been repeated under it: the scope contract, how to read the PR, the review-bot thread obligations, grounding, the output schema and the publish contract.\n\n"

// combinedPerspectiveSections is the per-perspective tail of a combined kick:
// one short section each, carrying only what the shared block cannot — the
// focus text and any perspective-specific material (plan_match's plan wave).
// They come AFTER the shared block so a reader meets the contract once and
// then the questions, rather than the same contract five times.
func combinedPerspectiveSections(pr PullRequest, perspectives []Perspective, opts PromptOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nPERSPECTIVE SECTIONS — %d, one verdict object each.\n", len(perspectives))
	for _, p := range perspectives {
		fmt.Fprintf(&b, "\n[perspective:%s]\n", p)
		fmt.Fprintf(&b, "%s — %s. Focus ONLY on that; do not duplicate other perspectives unless the issue is severe.\n", p, opts.Perspectives.Focus(p))
		fmt.Fprintf(&b, "Verdict object: perspective \"%s\", judged on this perspective's own in-scope evidence under the shared instructions above.\n", p)
		if p == PerspectivePlanMatch {
			b.WriteString(planMatchSection(pr))
		}
	}
	fmt.Fprintf(&b, "\nDeliver all %d verdict objects in the one JSON array the shared instructions describe.\n", len(perspectives))
	return b.String()
}

func writeReviewWritingGuide(b *strings.Builder, opts PromptOptions) {
	guide := strings.TrimSpace(opts.WritingGuideSection)
	if guide == "" {
		return
	}
	b.WriteString(guide)
	if !strings.HasSuffix(guide, "\n\n") {
		if strings.HasSuffix(guide, "\n") {
			b.WriteString("\n")
		} else {
			b.WriteString("\n\n")
		}
	}
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
