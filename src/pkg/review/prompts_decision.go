package review

import (
	"fmt"
	"sort"
	"strings"
)

// BuildDecisionPrompt is the kick for the review-heads decision step
// (hivecommons/hive#13542): one session per PR, on the base reviewer, that
// reconciles every head's independent report for every perspective into one
// decided report per perspective and posts the PR's only comment.
//
// It carries the heads' reports in full — verdicts, every finding with its
// severity, scope and location, summaries and decision questions — because
// the decision is about evidence, not a vote. It names any head that never
// answered, so the comment can say what the decision rests on.
func BuildDecisionPrompt(pr PullRequest, perspectives []Perspective, agg Aggregate, opts PromptOptions) string {
	if len(perspectives) == 0 {
		perspectives = opts.Perspectives.List()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[review-decision:%s]\n", strings.Join(perspectiveNames(perspectives), ","))
	fmt.Fprintf(&b, "Decide the review of PR %s#%d", pr.Repo, pr.Number)
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
	heads := decisionHeadNames(agg)
	fmt.Fprintf(&b, "\nYOU ARE THE DECISION STEP. %d independent review heads (%s) each judged this PR from the perspectives %s without seeing each other's work. Their full reports are below. Reconcile them into ONE decided verdict per perspective and post the PR's ONLY review comment.\n", len(heads), strings.Join(heads, ", "), joinPerspectives(perspectives))
	b.WriteString("This is a judgement on evidence, not a vote. A finding one head raised and another missed is not outvoted: check it against the code. A finding every head raised can still be wrong: check it too.\n\n")

	b.WriteString(scopeContractSection(pr))
	b.WriteString(buildReadInstruction(pr))
	b.WriteString(groundingSectionWithBudget(pr, combinedGroundingBudget(len(perspectives)), fmt.Sprintf("this whole decision, across all %d perspectives", len(perspectives))))
	writeReviewWritingGuide(&b, opts)

	b.WriteString("\nHOW TO DECIDE, PER PERSPECTIVE.\n")
	b.WriteString("1. Read every head finding for the perspective and open the file:line it cites. De-duplicate: findings from different heads about the same defect are ONE finding; keep the clearest statement and credit every head that raised it.\n")
	b.WriteString("2. Give EVERY head finding a disposition:\n")
	fmt.Fprintf(&b, "   - %s — it is real and you verified it in the code;\n", DispositionConfirmed)
	fmt.Fprintf(&b, "   - %s — you checked and the code does not do what the finding says (say what it actually does);\n", DispositionNotReproducible)
	fmt.Fprintf(&b, "   - %s — real, but outside this PR's scope contract (it goes to the backlog, it does not block).\n", DispositionOutOfScope)
	b.WriteString("   Every disposition carries a one-sentence reason that cites what you read. Never drop a head finding silently.\n")
	b.WriteString("3. Set the FINAL severity of each confirmed finding, whatever severity the head gave it:\n")
	b.WriteString("   - blocking (high or critical, or medium when it must land before merge): correctness defects, security problems, data loss or corruption, a broken contract or public interface, a required test that is missing;\n")
	b.WriteString("   - backlog (low or info): documentation wording, style, naming, and refactors that do not change behaviour.\n")
	b.WriteString("   The hive routes on these severities: findings at or above review.severity.block_at block the merge and send a fix; findings below it are filed as follow-up backlog items. Calibrate accordingly.\n")
	b.WriteString("4. Decide the perspective's verdict from the confirmed findings only: approve when none blocks; changes_requested for agent-fixable in-scope issues; requires_human for ambiguous or high-risk judgement a person must own; reject for a fundamentally unsuitable change. When the heads disagreed on a verdict, resolve it on the evidence and say why.\n")
	b.WriteString("For Hive-authored PRs, resolve sequencing, naming, config-shape, scope, and land-now-vs-later questions yourself; use requires_human only for risk a human must own: security, data loss, irreversible or public-facing behavior, policy/governance, licensing, spend, or when you could not read the diff.\n")
	b.WriteString(permissionsTighteningGuidance)

	reportPerspectives := append([]Perspective(nil), perspectives...)
	seen := map[Perspective]bool{}
	for _, p := range reportPerspectives {
		seen[p] = true
	}
	var historical []string
	for _, r := range agg.HeadReports {
		if !seen[r.Perspective] {
			seen[r.Perspective] = true
			historical = append(historical, string(r.Perspective))
		}
	}
	sort.Strings(historical)
	for _, p := range historical {
		reportPerspectives = append(reportPerspectives, Perspective(p))
	}
	if len(historical) > 0 {
		b.WriteString("\nReports from removed perspectives are historical evidence. Reconcile their findings and verdicts into the most relevant configured perspective; do not return a separate verdict for a removed perspective.\n")
	}
	b.WriteString(decisionHeadReportsSection(reportPerspectives, agg))
	if len(agg.MissingHeads) > 0 {
		b.WriteString("\nHEADS THAT NEVER ANSWERED.\n")
		fmt.Fprintf(&b, "These head reviews expired without a verdict after the retry limit: %s. Decide on the heads that did answer, and say so in the comment: one line per missing head, e.g. `<head>: no verdict after N attempts; decided on <the heads that answered> only`.\n", strings.Join(agg.MissingHeads, ", "))
	}

	b.WriteString("\nRETURN exactly one JSON ARRAY with one object per perspective — all ")
	fmt.Fprintf(&b, "%d of them: %s.\n", len(perspectives), joinPerspectives(perspectives))
	b.WriteString("Each object: the standard outputschema AgentReport fields plus perspective, verdict, confidence, repo, number, head_sha, body_digest and dispositions (copy head_sha and body_digest verbatim from above, or leave them out; never compute or summarize them).\n")
	b.WriteString("Required AgentReport fields: lane, kind, findings, prs_opened, beads_filed, summary. Set kind to \"review\" and lane to \"review-swarm\". Use [] for empty arrays.\n")
	fmt.Fprintf(&b, "Every object must carry the same repo (%s) and number (%d).\n", pr.Repo, pr.Number)
	b.WriteString("findings: ONLY the confirmed and out-of-scope findings, each once, with its FINAL severity (out-of-scope ones with review_scope \"out-of-scope\"). Not-reproducible findings do not appear here.\n")
	b.WriteString("dispositions: one object per head finding for this perspective: {\"head\": \"<head>\", \"title\": \"<the head's finding title>\", \"file\": \"<path>\", \"line\": <n>, \"disposition\": \"confirmed|not_reproducible|out_of_scope\", \"reason\": \"<what you read>\", \"severity\": \"<final severity for a confirmed finding>\"}.\n")
	b.WriteString(findingSchemaInstruction)
	b.WriteString("A perspective you could not meaningfully decide is requires_human, not approve.\n")
	if opts.PostComments {
		b.WriteString(buildDecisionPublishInstruction(pr, perspectives, opts))
	} else {
		b.WriteString("\nRecord the decided verdicts without posting a comment:\n")
		fmt.Fprintf(&b, "  hive-review %d --repo %s --record-verdict --verdict-file <verdict>\n", pr.Number, pr.Repo)
	}
	b.WriteString(buildProposeOnlyInstruction(pr, opts))
	return b.String()
}

// decisionHeadNames are the heads whose reports the decision step reads.
func decisionHeadNames(agg Aggregate) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range agg.HeadReports {
		if r.Head != "" && !seen[r.Head] {
			seen[r.Head] = true
			out = append(out, r.Head)
		}
	}
	sort.Strings(out)
	return out
}

// decisionHeadReportsSection renders every head's report, in full, grouped by
// perspective.
func decisionHeadReportsSection(perspectives []Perspective, agg Aggregate) string {
	var b strings.Builder
	b.WriteString("\nTHE HEADS' REPORTS (independent; each head saw none of the others).\n")
	for _, p := range perspectives {
		fmt.Fprintf(&b, "\n[perspective:%s]\n", p)
		reported := false
		for _, r := range agg.HeadReports {
			if r.Perspective != p {
				continue
			}
			reported = true
			model := r.ReviewModel
			if model == "" {
				model = "model not recorded"
			}
			verdict := string(r.Verdict)
			if r.NotApplicable {
				verdict += " (not applicable)"
			}
			fmt.Fprintf(&b, "- head %s (%s): verdict %s\n", r.Head, model, verdict)
			if r.Summary != "" {
				fmt.Fprintf(&b, "  summary: %s\n", r.Summary)
			}
			if r.DecisionQuestion != "" {
				fmt.Fprintf(&b, "  decision question: %s\n", r.DecisionQuestion)
			}
			if len(r.Findings) == 0 {
				b.WriteString("  findings: none\n")
			}
			for i, f := range r.Findings {
				loc := strings.TrimSpace(f.File)
				if loc != "" && f.Line > 0 {
					loc = fmt.Sprintf("%s:%d", loc, f.Line)
				}
				if loc == "" {
					loc = "no location"
				}
				scope := strings.TrimSpace(f.ReviewScope)
				if scope == "" {
					scope = "in-scope"
				}
				fmt.Fprintf(&b, "  finding %d: [%s, %s] %s — %s (%s)\n", i+1, f.Severity, scope, f.Title, f.Summary, loc)
			}
		}
		if !reported {
			b.WriteString("- no head reported this perspective\n")
		}
	}
	return b.String()
}

// buildDecisionPublishInstruction is the publish half of the decision kick:
// one comment for the whole PR, findings tagged by the heads that raised them.
func buildDecisionPublishInstruction(pr PullRequest, perspectives []Perspective, opts PromptOptions) string {
	var b strings.Builder
	b.WriteString(buildPublishInstruction(pr, opts.AcknowledgeNoFindings, opts.Revise))
	b.WriteString("\nONE COMMENT FOR THE WHOLE PR.\n")
	b.WriteString("The heads posted nothing; your comment is the PR's only review. Call hive-review exactly once, with the comment and the JSON array of all decided verdicts.\n")
	b.WriteString("Use a short `**<perspective>**` heading per perspective that has a confirmed finding, worst severity first. Tag each finding with the head(s) that raised it, e.g. `[sonnet, sol]`.\n")
	b.WriteString("Name the perspectives with no confirmed finding together on one line:\n")
	b.WriteString("  _No findings from: <comma-separated perspectives>._\n")
	b.WriteString("End with a `### Head findings` section listing EVERY head finding with its disposition and one-line reason (confirmed / not reproducible / out of scope), so a reader can see what each head raised and why it stands or falls. Then one line per head that never answered, if any.\n")
	fmt.Fprintf(&b, "The verdict file carries all %d decided verdicts as one JSON array, including the clean ones.\n", len(perspectives))
	return b.String()
}
