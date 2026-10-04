package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

const (
	holdGatedACMMMinLevel = 3
	holdGatedACMMMaxLevel = 5
)

// addHeldPRCoordination makes open, human-review-gated work visible before a
// PR-capable agent chooses its next target. These PRs cannot ride ${PR_LIST}:
// the hold gate intentionally removes them from the actionable PR population.
// The section is injected outside policy-template resolution so a customized
// or remotely sourced policy cannot accidentally omit the coordination fact.
func (s *Scheduler) addHeldPRCoordination(agentName string, actionable *github.ActionableResult, message string) string {
	if message == "" || !s.isHoldGatedPRAgent(agentName) {
		return message
	}
	claims, failClosed := s.formatHeldPRClaimsWithPolicy(actionable)
	if failClosed {
		if s.logger != nil {
			s.logger.Warn("ioscan fail-closed blocked held-PR coordination kick", "agent", agentName)
		}
		return ""
	}

	section := `## Open hold-gated PR coordination — mandatory preflight

The PRs below are open and awaiting human review. Their files, functions, and
tracking-issue clusters are occupied ground even when they have been waiting
for hours. Review latency is not abandonment.

If one of your own held PRs shows unaddressed CHANGES REQUESTED, address the
review on that same branch and reply on the PR. Never remove the hold label.

OPEN HOLD-GATED PRs:
` + claims + `

Before choosing work:
1. Compare your intended files, functions, and tracker cluster with every
   plausibly related PR above.
2. Inspect any plausible overlap with ` + "`gh pr view <number> --repo <repo> --json title,body,files`" + `
   and, when needed, ` + "`gh pr diff <number> --repo <repo>`" + `. The supplied list is the
   authoritative open-PR snapshot; do not run ` + "`gh pr list`" + ` to rebuild it.
3. If another PR already covers any intended ground, choose a disjoint cluster
   or stand down. If the snapshot flags a repo as having omitted PRs, stand
   down for that repo: unseen occupied ground cannot be proven disjoint. Repos
   the snapshot lists in full remain workable. Do not write a second
   implementation and do not remove hold.
4. Cut any new branch from a fresh ` + "`origin/<base>`" + `, not from a local worktree
   or another agent's branch. If a hold-gated PR's head moves while held, the
   hold guard treats that as unreviewed drift: auto-merge stays blocked, a
   warning comment names the new commits/authors, and the PR must receive fresh
   human review before the hold is lifted again.
5. Make each new PR title and body name the exact files/functions/cluster it
   claims so the next kick can make the same comparison.

`
	if newline := strings.IndexByte(message, '\n'); newline >= 0 {
		return message[:newline+1] + "\n" + section + message[newline+1:]
	}
	return section + message
}

func (s *Scheduler) isHoldGatedPRAgent(agentName string) bool {
	if s.cfg == nil || s.cfg.ACMMLevel == nil || *s.cfg.ACMMLevel < holdGatedACMMMinLevel || *s.cfg.ACMMLevel > holdGatedACMMMaxLevel {
		return false
	}
	mode := s.agentEffectiveMode(agentName)
	return mode == "ISSUES_AND_PRS" || mode == "ISSUES_PRS_MERGE"
}

// addWorkflowPushCeiling states the one limit an ISSUES_AND_PRS agent cannot
// discover from its policy: a branch whose diff touches .github/workflows/**
// is unpushable at this mode, whatever the policy says it "can PR".
//
// The mode maps to the `contributor` scoped-token tier, and that tier
// deliberately does not request the Workflows permission (pkg/agent/mode.go
// TokenTier, pkg/github/app.go ScopedToken — only trusted/merger ask for it,
// so the push broker's protected-path rejection of .github/workflows/ stays
// meaningful for sandboxed contributors). GitHub then rejects the ref update
// server-side: "refusing to allow a GitHub App to create or update workflow
// ... without `workflows` permission".
//
// Nothing told the agent. Observed live (#6681): a hold-gated sec-check agent
// found an unsafe pattern in a workflow file, wrote the exact replacement into
// an issue, and filed no PR — the right outcome, reached with no way to say
// why, and indistinguishable to the operator from an agent that simply chose
// not to fix it. ci-maintainer-holdgated.md meanwhile promised
// ".github/workflows/*.yml changes" it could never land.
//
// Injected at the same post-resolution seam as the held-PR preflight and for
// the same reason (kubestellar/hive#4744): a customized or remotely sourced
// policy must not be able to omit it.
func (s *Scheduler) addWorkflowPushCeiling(agentName, message string) string {
	if message == "" || s.agentEffectiveMode(agentName) != "ISSUES_AND_PRS" {
		return message
	}
	section := `## Workflow files are out of reach at this mode — preflight

This agent runs in ISSUES_AND_PRS (hold-gated) mode, whose GitHub App token is
minted at the ` + "`contributor`" + ` tier. That tier does not carry the Workflows
permission, so a push whose diff touches

    .github/workflows/**

is rejected by GitHub server-side ("refusing to allow a GitHub App to create or
update workflow ... without ` + "`workflows`" + ` permission"), no matter what the App
installation grants. When this agent runs sandboxed the push broker refuses the
same diff first ("protected paths changed"), along with the other paths it
protects.

This is a ceiling, not a bug to work around: do not rewrite the file, do not
retry, and never weaken the finding to fit what is pushable.

When a fix belongs in a workflow file:
1. Do not open a PR for it. Nothing you can push will contain the change.
2. File the issue with the exact replacement text, so applying it is mechanical.
3. Say plainly in the issue that the change needs a human or an
   ISSUES_PRS_MERGE agent to land, and why — otherwise "issue, no PR" reads as
   a judgement call you did not make.
4. If part of the fix lives outside ` + "`.github/workflows/`" + `, PR that part and say
   in both the issue and the PR which part is still waiting.

Everything else is pushable as normal, including composite actions under
` + "`.github/actions/`" + ` — GitHub's restriction covers the workflows directory only.

`
	if newline := strings.IndexByte(message, '\n'); newline >= 0 {
		return message[:newline+1] + "\n" + section + message[newline+1:]
	}
	return section + message
}

// agentEffectiveMode returns the agent's configured mode, preferring the
// tools-derived effective mode when one is set. Empty when the agent is not
// configured. This is the resolution isHoldGatedPRAgent and isPRCapableAgent
// both did inline; it is one place now so a new caller cannot get it subtly
// different.
func (s *Scheduler) agentEffectiveMode(agentName string) string {
	if s.cfg == nil {
		return ""
	}
	agentCfg, ok := s.cfg.Agents[agentName]
	if !ok {
		agentCfg, ok = s.cfg.Agents[s.cfg.BaseAgentName(agentName)]
	}
	if !ok {
		return ""
	}
	mode := agentCfg.Mode
	if agentCfg.Tools != nil {
		if effective := agentCfg.Tools.EffectiveMode(); effective != "" {
			mode = effective
		}
	}
	return mode
}

// isPRCapableAgent reports whether the agent's effective mode lets it push
// branches / open PRs at all. Advisory-only agents can never repair a red PR,
// so the fix-before-new section would be noise for them.
func (s *Scheduler) isPRCapableAgent(agentName string) bool {
	if s.cfg == nil {
		return false
	}
	mode := s.agentEffectiveMode(agentName)
	return mode == "ISSUES_AND_PRS" || mode == "ISSUES_PRS_MERGE"
}

// redPRFixMaxDetailed bounds how many red PRs get a full evidence entry in one
// kick; the rest are summarized so a pathological backlog cannot flood the
// prompt. redPRFixExcerptRunes bounds the per-PR CI evidence excerpt.
const (
	redPRFixMaxDetailed  = 5
	redPRFixExcerptRunes = 400
)

// addRedPRFixFirst prepends a fix-before-new section listing the agent's OWN
// red-CI or conflicted PRs, with the failing checks and raw CI evidence, right
// below the kick header. It reads ci-failing.json (written by writeMergeEligible each
// eval tick), which attributes each PR to the agent whose relay request opened
// it. Unattributed rows default to scanner — the fleet's primary PR creator.
// Escalated (needs-human) PRs are never listed: they belong to a human.
func (s *Scheduler) addRedPRFixFirst(agentName string, message string) string {
	if message == "" || !s.isPRCapableAgent(agentName) {
		return message
	}
	data, err := os.ReadFile(ciFailingPath)
	if err != nil {
		return message
	}
	section := formatRedPRFixData(data, s.cfg.BaseAgentName(agentName))
	if section == "" {
		return message
	}
	// Insert directly after the "[agent:x]" header line when present, so the
	// section is the first thing the agent reads; otherwise prefix.
	if idx := strings.Index(message, "\n"); idx >= 0 && strings.HasPrefix(message, "[agent:") {
		return message[:idx+1] + section + message[idx+1:]
	}
	return section + message
}

// heldRedPRNote is appended to a held PR's entry in the fix-before-new
// block: the hold is a merge checkpoint, not a repair checkpoint
// (hivecommons/hive#7438), and the agent's own policy says never to touch a
// held item — this line is the explicit, narrow exception.
const heldRedPRNote = "held for human review — fix CI, do not remove the hold"

// heldRedPRExemptAgent is the one agent whose held PRs are NOT routed back
// for repair: the level-hold comment tells humans that outreach PRs are
// always held for their review, so an outreach PR must not be edited after a
// human may have started reading it.
const heldRedPRExemptAgent = "outreach"

// formatRedPRFixData renders the fix-before-new section for one agent from
// raw ci-failing.json bytes. Empty result means the agent has no open,
// non-escalated red or conflicted PRs it can repair. A held red PR
// (hivecommons/hive#7438) is listed like any other — with heldRedPRNote —
// except for outreach's. A red PR already deferred to a still-open shared-CI
// incident (deferred_incident, hivecommons/hive#10528) has nothing to repair:
// it is named in one line without repair instructions, and when every red PR
// the agent owns is deferred there is no section — and no banner — at all.
func formatRedPRFixData(data []byte, agent string) string {
	type ciFailingRow struct {
		Number           int      `json:"number"`
		Repo             string   `json:"repo"`
		Title            string   `json:"title"`
		Agent            string   `json:"agent"`
		FailingChecks    []string `json:"failing_checks"`
		Excerpt          string   `json:"excerpt"`
		Escalated        bool     `json:"escalated"`
		FromFork         bool     `json:"from_fork"`
		HeadRepo         string   `json:"head_repo"`
		Held             bool     `json:"held"`
		MergeableState   string   `json:"mergeable_state"`
		Conflict         bool     `json:"conflict"`
		ReroutedFrom     string   `json:"rerouted_from"`
		DeferredIncident int      `json:"deferred_incident"`
	}
	var payload struct {
		Items []ciFailingRow `json:"ci_failing"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return ""
	}
	var mine, deferred []ciFailingRow
	forks := 0
	for _, pr := range payload.Items {
		if pr.Escalated {
			continue // needs-human: hands off for agents
		}
		if pr.FromFork {
			// A fork PR can never be "yours": the hive pushes only to the base
			// repository, so it did not open this PR and cannot repair it.
			// Unattributed rows default to scanner below, which is exactly how
			// 66 contributor PRs from forks became one scanner's FIX-BEFORE-NEW
			// gate on the projectbluefin spoke (hivecommons/hive#7386).
			forks++
			continue
		}
		owner := pr.Agent
		if owner == "" {
			owner = "scanner"
		}
		if owner != agent {
			continue
		}
		if pr.Held && owner == heldRedPRExemptAgent {
			continue // a human may already be reading it; leave it alone
		}
		if pr.DeferredIncident > 0 {
			deferred = append(deferred, pr)
			continue
		}
		mine = append(mine, pr)
	}
	if len(mine) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n## 🔴 FIX-BEFORE-NEW — your open PRs with failing CI or merge conflicts (%d)\n\n", len(mine)))
	if forks > 0 {
		b.WriteString(fmt.Sprintf("(%d red PR(s) from forks are NOT listed here: you cannot push to a fork — they appear under CI_FAILING as comment-only.)\n", forks))
	}
	if len(deferred) > 0 {
		refs := make([]string, 0, len(deferred))
		for _, pr := range deferred {
			refs = append(refs, fmt.Sprintf("%s#%d → incident #%d", pr.Repo, pr.Number, pr.DeferredIncident))
		}
		b.WriteString(fmt.Sprintf("(%d of your red PR(s) are NOT listed: already deferred to a still-open shared-CI incident — do not repair or re-triage them; they return here when the incident closes: %s)\n", len(deferred), strings.Join(refs, ", ")))
	}
	b.WriteString("These PRs are YOURS and they are red or conflicted. Repairing them comes BEFORE claiming\n")
	b.WriteString("new issues or opening ANY new PR. For each one:\n")
	b.WriteString("  gh pr checkout <number> → fix using the evidence below → commit -s → git push\n")
	b.WriteString("Push to the SAME branch. Do NOT open a replacement PR. Do NOT leave these\n")
	b.WriteString("for a later cycle — every kick will re-list them until they are green.\n")
	b.WriteString("After each push move on: do NOT watch, poll, or sleep on CI (no gh run watch/view loops) —\n")
	b.WriteString("the sweep merges green PRs; infrastructure failures get one comment and a DEFER.\n\n")
	for i, pr := range mine {
		if i >= redPRFixMaxDetailed {
			b.WriteString(fmt.Sprintf("  … and %d more (full list: %s)\n", len(mine)-i, ciFailingPath))
			break
		}
		b.WriteString(fmt.Sprintf("  #%d %s — %s\n", pr.Number, pr.Repo, pr.Title))
		if pr.Conflict {
			state := strings.TrimSpace(pr.MergeableState)
			if state == "" {
				state = "dirty"
			}
			b.WriteString("    conflict: mergeable_state=" + state + " — merge the base branch or rebase the PR branch\n")
			if from := strings.TrimSpace(pr.ReroutedFrom); from != "" {
				b.WriteString("    rerouted from paused/unavailable lane: " + from + "\n")
			}
		}
		if pr.Held {
			b.WriteString("    " + heldRedPRNote + "\n")
		}
		if len(pr.FailingChecks) > 0 {
			b.WriteString(fmt.Sprintf("    failing: %s\n", strings.Join(pr.FailingChecks, ", ")))
		}
		if excerpt := strings.TrimSpace(pr.Excerpt); excerpt != "" {
			if runes := []rune(excerpt); len(runes) > redPRFixExcerptRunes {
				excerpt = string(runes[:redPRFixExcerptRunes]) + "…"
			}
			b.WriteString("    evidence: " + strings.ReplaceAll(excerpt, "\n", "\n              ") + "\n")
		}
	}
	b.WriteString("\n")
	return b.String()
}

// reviewThreadsPath is the review-thread monitor's artifact
// (github.ReviewThreadsPath); a var here, like ciFailingPath, so tests can
// point the kick builder at a fixture.
var reviewThreadsPath = github.ReviewThreadsPath

// addReviewThreadFixFirst prepends a fix-before-new section listing the
// agent's OWN open PRs that carry unresolved external review-bot threads
// (hivecommons/hive#7360), with each thread's path, line, and an excerpt of
// the bot's finding. It reads review-threads.json (written by the governor's
// eval tick), which — exactly like ci-failing.json — attributes each PR to the
// agent whose relay request opened it; unattributed rows default to scanner.
// Escalated (needs-human) PRs are never listed. Inserted at the same
// below-the-header seam as the red-CI block (which runs first, so this block
// lands directly above it) so the two stuck-PR lists sit together at the top
// of the kick, ahead of the work list.
func (s *Scheduler) addReviewThreadFixFirst(agentName string, message string) string {
	if message == "" || !s.isPRCapableAgent(agentName) {
		return message
	}
	data, err := os.ReadFile(reviewThreadsPath)
	if err != nil {
		return message
	}
	section := formatReviewThreadFixData(data, s.cfg.BaseAgentName(agentName), s.reviewBotsResolveAfterFix())
	if section == "" {
		return message
	}
	if idx := strings.Index(message, "\n"); idx >= 0 && strings.HasPrefix(message, "[agent:") {
		return message[:idx+1] + section + message[idx+1:]
	}
	return section + message
}

// reviewBotsResolveAfterFix reads classification.review_bots.resolve_after_fix
// from the loaded config (default true). The project-file fallback is not
// consulted here: the kick text only decides whether to ALSO write a
// resolve_thread request, and the watcher's guard is what actually gates
// resolution, so a stale answer here costs at most one denied request.
func (s *Scheduler) reviewBotsResolveAfterFix() bool {
	if s.cfg == nil {
		return true
	}
	return s.cfg.Classification.ReviewBots.ResolveAfterFixEnabled()
}

// formatReviewThreadFixData renders the review-thread fix-before-new section
// for one agent from raw review-threads.json bytes. Empty result means the
// agent has no open, non-escalated PR with an actionable bot thread.
func formatReviewThreadFixData(data []byte, agent string, resolveAfterFix bool) string {
	var report github.ReviewThreadsReport
	if json.Unmarshal(data, &report) != nil || !report.Enabled {
		return ""
	}
	var mine []github.ReviewThreadPR
	threads := 0
	for _, pr := range report.PRs {
		if pr.Escalated || len(pr.Threads) == 0 {
			continue // needs-human, or nothing left to address on this PR
		}
		owner := pr.Agent
		if owner == "" {
			owner = "scanner"
		}
		if owner != agent {
			continue
		}
		mine = append(mine, pr)
		threads += len(pr.Threads)
	}
	if len(mine) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n## 💬 FIX-BEFORE-NEW — your open PRs with unresolved review-bot threads (%d PRs, %d threads)\n\n", len(mine), threads))
	b.WriteString("An external review bot left inline threads on PRs that are YOURS. Until every\n")
	b.WriteString("thread is resolved these PRs cannot merge. Addressing them comes BEFORE claiming\n")
	b.WriteString("new issues or opening ANY new PR. ONE pass per kick, for each PR:\n")
	b.WriteString("  1. gh pr checkout <number> (branch: head_ref below) → address each thread's\n")
	b.WriteString("     finding → git commit -s → git push to the SAME branch. No replacement PR.\n")
	b.WriteString("  2. For EACH thread, reply in-thread with ONE line saying what changed, or why\n")
	b.WriteString("     nothing needed to (the reply goes into the thread, not a new review):\n")
	b.WriteString("       hive-review <number> --repo <owner/repo> --comment --thread <thread_id> --body \"<one line>\"\n")
	if resolveAfterFix {
		b.WriteString("  3. Then resolve it:\n")
		b.WriteString("       hive-review <number> --repo <owner/repo> --resolve-thread <thread_id>\n")
		b.WriteString("     UNLESS the PR below says otherwise — read each PR's own step-3 line first.\n")
	} else {
		b.WriteString("  3. Do NOT resolve the thread — a human closes it on this hive (resolve_after_fix: false).\n")
	}
	b.WriteString("Never reply twice in the same thread: threads you already answered are not listed\n")
	b.WriteString("here; if one still appears, skip it. Only threads a review bot opened are listed\n")
	b.WriteString("and only those can be resolved this way — a human's thread is never yours to close.\n\n")
	shown := 0
	for _, pr := range mine {
		if shown >= redPRFixMaxDetailed {
			b.WriteString(fmt.Sprintf("  … and %d more PRs (full list: %s)\n", len(mine)-shown, reviewThreadsPath))
			break
		}
		shown++
		title := strings.TrimSpace(pr.Title)
		if title != "" {
			title = " — " + title
		}
		b.WriteString(fmt.Sprintf("  #%d %s%s\n", pr.Number, pr.Repo, title))
		b.WriteString(fmt.Sprintf("    head_ref: %s\n", pr.HeadRef))
		// resolveAfterFix (the global setting) already covers every PR when
		// pr.HumanOpened is false; only a human-opened PR (hivecommons/
		// hive#9361) can DISAGREE with the global setting, and only in the
		// direction of withholding resolution, never granting it.
		if pr.HumanOpened && resolveAfterFix {
			b.WriteString("    step 3: DO NOT resolve — this PR is not yours (fix_human_prs), a person closes its threads (hivecommons/hive#9361). Reply and push the fix only.\n")
		}
		for _, t := range pr.Threads {
			loc := t.Path
			if t.Line > 0 {
				loc = fmt.Sprintf("%s:%d", t.Path, t.Line)
			}
			b.WriteString(fmt.Sprintf("    thread %s (%s, by %s)\n", t.ThreadID, loc, t.Author))
			if excerpt := strings.TrimSpace(t.Body); excerpt != "" {
				if runes := []rune(excerpt); len(runes) > redPRFixExcerptRunes {
					excerpt = string(runes[:redPRFixExcerptRunes]) + "…"
				}
				b.WriteString("      finding: " + strings.ReplaceAll(excerpt, "\n", "\n               ") + "\n")
			}
		}
	}
	b.WriteString("\n")
	return b.String()
}

// maxHeldPRsPerRepoPerKick bounds the held-PR snapshot per repository instead
// of across the whole kick.
//
// Disjointness is only ever evaluated against PRs in the repo the agent is
// about to touch, so a repo-scoped cap preserves the fail-closed guarantee
// exactly — the agent still refuses to act wherever the snapshot is
// incomplete — while preventing one crowded repo from blanking the snapshot
// for every other repo in the sweep.
//
// The previous global cap reused maxIssuesPerKick, which coupled two unrelated
// limits and made the stand-down fire on render-cap overflow rather than on
// real contention: a spoke tracking 16 repos stood down across all of them
// because a single repo pushed the combined list one item past 100, and could
// then never open the PRs that would drain the backlog causing the overflow.
const maxHeldPRsPerRepoPerKick = 100

func (s *Scheduler) formatHeldPRClaimsWithPolicy(actionable *github.ActionableResult) (string, bool) {
	if actionable == nil {
		return "  (none)", false
	}
	var b strings.Builder
	shown := 0
	failClosed := false

	repoOrder := make([]string, 0, 8)
	byRepo := make(map[string][]github.HoldItem)
	heldPRs := make(map[string]github.PullRequest, len(actionable.PRs.Held))
	for _, pr := range actionable.PRs.Held {
		heldPRs[fmt.Sprintf("%s#%d", pr.Repo, pr.Number)] = pr
	}
	for _, item := range actionable.Hold.Items {
		if item.Type != "pr" {
			continue
		}
		if _, seen := byRepo[item.Repo]; !seen {
			repoOrder = append(repoOrder, item.Repo)
		}
		byRepo[item.Repo] = append(byRepo[item.Repo], item)
	}

	for _, repo := range repoOrder {
		items := byRepo[repo]
		limit := len(items)
		if limit > maxHeldPRsPerRepoPerKick {
			limit = maxHeldPRsPerRepoPerKick
		}
		for _, item := range items[:limit] {
			title, verdict := s.enforceIssueTextVerdict(item.Title)
			failClosed = failClosed || (s.ioscanFailClosed() && verdict.HasCriticalInjection())
			const maxHeldPRTitleRunes = 70
			if runes := []rune(title); len(runes) > maxHeldPRTitleRunes {
				title = string(runes[:maxHeldPRTitleRunes])
			}
			reviewState := heldPRReviewState(heldPRs[fmt.Sprintf("%s#%d", item.Repo, item.Number)])
			if reviewState != "" {
				reviewState = " — " + reviewState
			}
			b.WriteString(fmt.Sprintf("  %s#%d %s%s\n", item.Repo, item.Number, title, reviewState))
			shown++
		}
		if omitted := len(items) - limit; omitted > 0 {
			b.WriteString(fmt.Sprintf("  ... %d additional open held PRs omitted in %s; STAND DOWN for %s this kick\n", omitted, repo, repo))
		}

	}

	if shown == 0 {
		return "  (none)", failClosed
	}
	return strings.TrimSuffix(b.String(), "\n"), failClosed
}

func heldPRReviewState(pr github.PullRequest) string {
	if pr.Number <= 0 || pr.Protection == nil {
		return "no review state available"
	}
	by := strings.TrimSpace(pr.Protection.LatestHumanReviewBy)
	if by != "" {
		by = " by @" + by
	}
	switch pr.Protection.LatestHumanReviewState {
	case github.ReviewDecisionChangesRequested:
		status := "unaddressed"
		if github.HumanReviewAddressed(pr) {
			status = "addressed"
		}
		return "CHANGES REQUESTED" + by + " (" + status + ")"
	case github.ReviewDecisionApproved:
		return "approved" + by
	case github.ReviewDecisionNone:
		if len(pr.RequestedReviewers) > 0 || len(pr.RequestedTeams) > 0 {
			return "review requested"
		}
		return "no human review yet"
	default:
		return strings.ToLower(strings.ReplaceAll(string(pr.Protection.LatestHumanReviewState), "_", " ")) + by
	}
}

func (s *Scheduler) independentReviewSection(agentName string) string {
	baseName := s.cfg.BaseAgentName(agentName)
	agentCfg, ok := s.cfg.Agents[baseName]
	if !ok || !agentCfg.ReviewModels.Configured() {
		return ""
	}
	return "## Independent review\n\nFor each PR whose list line includes `review_with=<backend>/<model>`, launch a sub-agent (Agent tool) with `model` set to the PR's `review_with` model value. Give the sub-agent the PR URL plus the review checklist, and have it probe author-model failure modes: invented APIs, tests that assert the implementation rather than the requirement, and swallowed errors. Record the sub-agent's verdict and model in the verdict artifact as `review_model`. Never review a PR yourself with the author's model."
}

func (s *Scheduler) addIndependentReviewSection(agentName, msg string) string {
	if msg == "" {
		return msg
	}
	if section := s.independentReviewSection(agentName); section != "" && !strings.Contains(msg, "## Independent review") {
		return section + "\n\n" + msg
	}
	return msg
}

// reviewPerspectivesSection renders ${REVIEW_PERSPECTIVES}: this hive's
// configured review perspectives with what each one looks for, as a bullet
// list. The cadence reviewer's template requires one verdict object per
// perspective named here; without the list in the kick the agent could only
// guess at the set (and guessed one — a single `correctness` object on a
// clean PR, which the confidence score then capped as "1 of 5 perspectives
// reported"). Falls back to the built-in set when the configured one does not
// validate, the same fallback the relay applies.
func (s *Scheduler) reviewPerspectivesSection() string {
	set, err := review.NewPerspectiveSet(s.cfg.Review.Perspectives, s.cfg.Review.PerspectivePrompts)
	if err != nil {
		set = review.PerspectiveSet{}
	}
	if s.cfg.Review.PlanMatch.Enabled {
		set = set.WithPlanMatch()
	}
	var b strings.Builder
	for _, p := range set.List() {
		fmt.Fprintf(&b, "- `%s` — %s\n", p, set.Focus(p))
	}
	return strings.TrimRight(b.String(), "\n")
}
