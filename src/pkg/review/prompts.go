package review

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type PullRequest struct {
	Repo   string
	Number int
	Title  string
	Author string
	Labels []string
	// ChangedFiles are the PR's changed paths when the caller fetched them
	// (ChangedFilesKnown), for the bot-author model route's path exceptions.
	// Unknown means no downgrade.
	ChangedFiles      []string
	ChangedFilesKnown bool
	// BaseDiffEmpty is true when GitHub reports the PR's diff against its
	// base as zero files: there is nothing to review at any head, so a
	// prior verdict is re-stamped instead of asked again on every
	// merge-from-base (hivecommons/hive#15499).
	BaseDiffEmpty bool
	HeadSHA       string
	UpdatedAt     time.Time
	// BodyDigest fingerprints the PR title/body that the reviewer judged.
	// The body itself deliberately stays out of review.PullRequest (see
	// grounding tests); this opaque digest lets a body-only remediation make
	// an old non-approve verdict stale without leaking the rationale into the
	// prompt model.
	BodyDigest  string
	URL         string
	Lane        string
	AuthorAgent string
	// AuthorBase is AuthorAgent's base lane, resolved by the caller over the
	// full configured agent set. Empty falls back to resolving AuthorAgent
	// against DispatchOptions.Agents.
	AuthorBase string
	// HiveAttributed is true when the PR body carries the hive attribution
	// trailer: a PR an agent opened on a person's credentials, which GitHub
	// shows under that person's login. It is carried for consumers and the
	// audit trail; it is deliberately not a planner input. The fix gate
	// (fixPushAllowed) decides on the login author alone: under a person
	// login the squash is attributed to that person, so a hive-signed fix
	// would fail the sign-off gate (hivecommons/hive#13124).
	HiveAttributed bool
	// AuthorModel is the model that wrote the PR, from the `— hive:`
	// attribution trailer (github.PullRequest.HiveModel). It is what
	// review_models exclusions compare against, and it is stamped into the
	// perspective prompt and the verdict artifact as author_model so the
	// author/reviewer model pair is recorded even on a hive with no pool.
	// Empty when the PR carries no trailer (a person's PR).
	AuthorModel string
	// MergeBase is the commit the reviewer should ground its reading in. When
	// set, the prompt instructs the reviewer to read the repository at this
	// commit rather than reasoning from the diff alone — see groundingSection
	// for the measurement that makes this the load-bearing field.
	MergeBase string
	// RunKey and PlanRef are the values of the PR's `Hive-Run:` and
	// `Hive-Plan:` trailers (hivecommons/hive#8310), parsed by the caller.
	// They name the long-running run and plan this PR claims to implement.
	// Empty means the PR carries no trailer, and the plan_match perspective
	// reports itself not applicable. These are identifiers, not free text:
	// nothing else from the trailer block reaches the prompt.
	RunKey  string
	PlanRef string
	// PlanWave is the approved plan rendered for the reviewer (see
	// planning.PlanTree.WaveText): the epic and each planned item with its
	// status. It is the reference plan_match judges the diff against, and it
	// is written by the planner, not the PR author -- which is what keeps it
	// on the right side of the Arm A finding above.
	PlanWave string
	// ScopeContract names the linked issue or lease task the PR is expected to
	// satisfy. It is extracted before prompt construction so the reviewer gets
	// the contract without seeing the author's whole PR rationale.
	ScopeContract string
	// ScopeContractState and ScopeContractText are the workflow state and
	// capped text of a `linked Linear issue KEY-n` contract, read hive-side
	// through the Linear work source at dispatch (hivecommons/hive#14906) so
	// reviewers never hold a Linear token. The text is the issue tracker's,
	// rendered as untrusted data. Empty text for a Linear contract means the
	// fetch failed, and the prompt names the identifier instead.
	ScopeContractState string
	ScopeContractText  string
	// HumanChangeRequest is a trusted maintainer's instruction standing
	// against HeadSHA — a CHANGES_REQUESTED review or a `/hive decision`
	// comment (hivecommons/hive#13041). Set by the caller from the
	// HumanChangeRequestStore; the planner answers it with a fix kick and
	// reviews nothing on that head. It is the maintainer's text, not the
	// author's rationale, so it is allowed into the fix prompt verbatim.
	HumanChangeRequest *HumanChangeRequest
	// DeltaDiff is the compare between the head the hive last judged and
	// HeadSHA, fetched by the caller when review.delta_rereview is on and the
	// PR has a prior verdict at another head. nil means not fetched: the
	// reviewer still gets the delta instruction, but no trivial skip.
	DeltaDiff *DeltaDiff
}

func PullRequestBodyDigest(title, body string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(title) + "\x00" + strings.TrimSpace(body)))
	return hex.EncodeToString(sum[:])
}

// Grounding thresholds, named so the numbers never appear bare in the prompt.
//
// The tool-call budget mirrors the measured experiment: the grounded arm used a
// mean of 10.6 tool calls under a cap of 25, so 25 is a ceiling that did not
// bind in practice rather than a limit tuned to force brevity. It exists to
// bound worst-case cost, not to shape behavior.
const (
	// GroundingToolCallBudget caps repository investigation per perspective.
	GroundingToolCallBudget = 25
	// GroundingToolCallBudgetPerExtraPerspective is what each perspective
	// beyond the first adds to a combined review's budget. The expensive
	// reads — the diff, the touched files, their callers — happen once for
	// the whole session, so an extra perspective costs a few targeted
	// lookups, not another full pass (hivecommons/hive#12126).
	GroundingToolCallBudgetPerExtraPerspective = 5
)

// Review-bot thread fetch page sizes (hivecommons/hive#12126). The reviewer
// acts on the bot's finding (the first comment of each unresolved thread) and
// needs to see whether anyone has already answered it; it does not act on a
// twenty-reply back-and-forth, so fetching one is tokens spent on nothing.
const (
	// ReviewBotThreadPageSize is the reviewThreads(first:) page size. The
	// command paginates, so this bounds the page, not the total.
	ReviewBotThreadPageSize = 100
	// ReviewBotThreadCommentsFetched is the comments(first:) bound per
	// thread: the finding plus the first few replies.
	ReviewBotThreadCommentsFetched = 5
)

// combinedGroundingBudget is the investigation budget for one session that
// covers n perspectives.
func combinedGroundingBudget(n int) int {
	if n < 1 {
		n = 1
	}
	return GroundingToolCallBudget + (n-1)*GroundingToolCallBudgetPerExtraPerspective
}

// groundingSection instructs the reviewer to read the actual tree, and it is
// the single most important paragraph in this file.
//
// An experiment (2026-08-28) scored three review arms against six merged hive
// PRs with pre-registered ground truth, blind scoring, and counted false
// positives:
//
//	Arm A  diff + the author's own PR body     0/6 hits (0%)   3.3 FP/PR
//	Arm B  diff + linked issue, cleared        1/6 hits (17%)  3.6 FP/PR
//	Arm C  cleared + repo at merge-base        4/6 hits (67%)  1.4 FP/PR
//
// Two findings shape this text:
//
//  1. REPO ACCESS IS THE ACTIVE INGREDIENT. B→C moved hit rate 17%→67% while
//     CUTTING false positives 61%. Grounding both finds real defects and
//     suppresses invented ones — the diff-only arms filled the gap with
//     confidently-argued claims the code did not support. Without this section
//     the review swarm IS Arm B: ~3.6 false positives per PR, roughly nine
//     non-issues per real finding.
//
//  2. THE AUTHOR'S RATIONALE ACTIVELY HURT. Arm A, the only arm shown the PR
//     body, was the worst. This prompt therefore never includes the PR body,
//     and PullRequest deliberately has no Body field so it cannot. That
//     absence is pinned by TestPromptExcludesAuthorRationale.
//
// Caveat, recorded because it bounds how much authority a verdict should carry:
// n=6, one run per cell. Directional, not definitive.
//
// The masked-text line is not from the experiment; it is from
// hivecommons/hive#8067, where a reviewer read a scrubbed Authorization header
// in place of its format string and reported the missing `%s` as the
// defect. Reading the tree does not help when a scrubber has already rewritten
// what the tree says, so the reviewer has to recognize a mask on sight. The
// relay refuses to publish a review whose evidence is masked text
// (pkg/github.redactedQuoteRefusal); this line is what keeps the reviewer from
// writing one in the first place.
func groundingSection(pr PullRequest) string {
	return groundingSectionWithBudget(pr, GroundingToolCallBudget, "this perspective")
}

// groundingSectionWithBudget is groundingSection with the tool-call budget and
// the scope it applies to spelled out, so a combined review can state one
// budget for the whole session instead of repeating a per-perspective one
// (hivecommons/hive#12126).
func groundingSectionWithBudget(pr PullRequest, budget int, scope string) string {
	var b strings.Builder
	b.WriteString("\nGROUNDING — read the code, do not infer it.\n")
	if pr.MergeBase != "" {
		fmt.Fprintf(&b, "A checkout of this repository at merge-base %s is available to you. ", pr.MergeBase)
	} else {
		b.WriteString("A checkout of this repository is available to you. ")
	}
	b.WriteString("Open the files the diff touches and the files that CALL them.\n")
	b.WriteString("- Every finding MUST cite concrete file:line evidence you actually read. A finding you cannot cite is the signature of a false positive — omit it.\n")
	b.WriteString("- Before asserting a defect, check the surrounding code for the guard, early return, or caller that would already prevent it.\n")
	b.WriteString("- Prefer one verified finding over three plausible ones. Measured: reviewers that read the tree found 4x more real defects AND made 61% fewer false claims.\n")
	b.WriteString("- If you cannot verify a concern, either state it as an explicit open question or leave it out. Do not assert it as a defect.\n")
	b.WriteString("- MASKED TEXT IS NOT THE CODE. If a line you are reading has a credential-shaped literal replaced by [REDACTED], <redacted>, or a run of asterisks, a secret scrubber put it there — the file does not say that. Re-read the line from the repository before you quote it, and never report the mask itself as a defect.\n")
	fmt.Fprintf(&b, "- Budget: up to %d investigation tool calls for %s.\n", budget, scope)
	return b.String()
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
	// ProposeFixesOnly tells the reviewer that no agent will push to this
	// PR's branch — it was opened by someone outside the hive, and a
	// hive-signed commit on it would fail the squash sign-off gate whatever
	// review.fix_human_prs says (hivecommons/hive#12897) — so every fix it wants has to travel in
	// the review comment itself, as a suggestion or a patch the author can
	// apply. Without this a changes_requested verdict on such a PR would
	// name a problem and then leave the author to guess at the fix the
	// reviewer already had in mind.
	ProposeFixesOnly bool
	// WritingGuideSection is config.ProjectConfig.WritingGuideSection().
	WritingGuideSection string
	// ReviewBotLogins is config.EffectiveReviewBots().Logins: the external
	// review bots (Codex, Copilot, …) whose unresolved inline threads the
	// reviewer must read and answer (hivecommons/hive#9360). Empty means the
	// hive has not named any, and the prompt says nothing about bot threads.
	ReviewBotLogins      []string
	ReviewBotMinPriority string
	// ReviewBackend/ReviewModel are the model this review must run with, as
	// chosen by the planner from agents.<name>.review_models (and
	// review.perspective_models). Empty means no pool is configured and the
	// prompt says nothing about models; set, the prompt carries the same
	// "Independent review" instruction the cadence kick overlay uses and
	// tells the reviewer to record the model as review_model.
	ReviewBackend string
	ReviewModel   string
	// HeadRecordOnly is set to the head key when this kick is one review
	// head's independent pass (agents.<name>.review_heads,
	// hivecommons/hive#13542). The head records its verdict without
	// posting anything on the PR and must not read other reviews of this
	// head; PostComments and AcknowledgeNoFindings are ignored.
	HeadRecordOnly string
	// Delta is set when this kick is a delta re-review (review.delta_rereview):
	// the PR already has a verdict at an earlier head, and the reviewer is
	// to read the diff between that head and this one, re-check the earlier
	// findings, and leave the rest of the PR as judged. nil is a full review.
	Delta *DeltaContext
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
	if pr.BodyDigest != "" {
		fmt.Fprintf(&b, "Review body digest: %s (an opaque id: copy it verbatim into body_digest or leave the field out)\n", pr.BodyDigest)
	}
	if pr.Author != "" {
		fmt.Fprintf(&b, "Author: @%s\n", strings.TrimPrefix(pr.Author, "@"))
	}
	writeAuthorModelLine(&b, pr)
	fmt.Fprintf(&b, "Focus ONLY on %s. Do not duplicate other perspectives unless the issue is severe.\n\n", focus)
	writeIndependentReviewSection(&b, opts)
	b.WriteString(scopeContractSection(pr))
	b.WriteString(buildReadInstruction(pr))
	b.WriteString(deltaSection(pr, opts.Delta))
	b.WriteString(buildReviewBotFindingsInstruction(pr, opts.ReviewBotLogins, opts.ReviewBotMinPriority))
	b.WriteString(groundingSection(pr))
	if p == PerspectivePlanMatch {
		b.WriteString(planMatchSection(pr))
	}
	writeReviewWritingGuide(&b, opts)
	b.WriteString("\nReturn exactly one JSON object: the standard outputschema AgentReport fields plus perspective, verdict, confidence, repo, number, head_sha, and body_digest (copy head_sha and body_digest verbatim from above, or leave them out; never compute or summarize them).\n")
	b.WriteString("Required AgentReport fields: lane, kind, findings, prs_opened, beads_filed, summary. Set kind to \"review\" and lane to \"review-swarm\". Use [] for empty arrays.\n")
	b.WriteString(modelStampInstruction(pr, opts))
	b.WriteString(findingSchemaInstruction)
	b.WriteString("Use approve only when this perspective finds no IN-SCOPE blocker. Use changes_requested for agent-fixable in-scope issues. Use requires_human for ambiguous/high-risk in-scope judgment. Use reject for fundamentally unsuitable or harmful PRs. Out-of-scope findings alone must not lower the verdict.\n")
	b.WriteString("For Hive-authored PRs, resolve sequencing, naming, config-shape, scope, and land-now-vs-later questions yourself: prefer approve with an advisory note, or changes_requested only for a concrete in-scope fix. Use requires_human only for risk a human must own: security, data loss, irreversible or public-facing behavior, policy/governance, licensing, spend, or when you could not read the diff.\n")
	b.WriteString(permissionsTighteningGuidance)
	b.WriteString("A Hive-authored incomplete fix with concrete remediation is changes_requested, not requires_human. If there are two engineering remedies (for example, add producer code OR reword a changelog), choose the best route yourself and state it as the requested change; do not escalate the choice to a human unless it falls into one of the risk categories above. Intent-alignment cleanup such as trimming a PR summary or changelog fragment that duplicates already-landed work is also changes_requested when the remediation is concrete.\n")
	if head := strings.TrimSpace(opts.HeadRecordOnly); head != "" {
		b.WriteString(buildHeadRecordOnlyInstruction(pr, head))
	} else if opts.PostComments {
		b.WriteString(buildPublishInstruction(pr, opts.AcknowledgeNoFindings, opts.Revise))
	}
	b.WriteString(buildProposeOnlyInstruction(pr, opts))
	return b.String()
}
