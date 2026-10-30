package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
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
		// Persists every tick while the held PR stays open; warn once per agent
		// like the main fail-closed kick path, then demote repeats to Debug.
		s.warnOnce("blocked-held-pr-kick:"+agentName, "ioscan fail-closed blocked held-PR coordination kick", "agent", agentName)
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
	return appendKickSection(message, section)
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
	return insertAfterKickHeader(message, section)
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

// addRedPRFixFirst appends a fix-before-new section listing the agent's OWN
// red-CI or conflicted PRs, with the failing checks and raw CI evidence, in
// the dynamic tail of the kick (see kick_layout.go). It reads ci-failing.json (written by writeMergeEligible each
// eval tick), which attributes each PR to the agent whose relay request opened
// it. Unattributed rows default to scanner — the fleet's primary PR creator.
// Escalated (needs-human) PRs are never listed: they belong to a human.
func (s *Scheduler) addRedPRFixFirst(agentName string, message string) string {
	if message == "" || !s.isPRCapableAgent(agentName) {
		return message
	}
	section := s.redPRFixSection(agentName)
	if section == "" {
		return message
	}
	return appendKickSection(message, section)
}

// redPRFixSection renders the agent's fix-before-new section from
// ci-failing.json merged with the webhook CI-failure signals attributed to
// it (github.CIFailureSignals), so a check that went red seconds ago is
// listed — with its name and log pointer — before the next eval tick
// rewrites the file. Empty when the agent has nothing to repair.
func (s *Scheduler) redPRFixSection(agentName string) string {
	base := s.cfg.BaseAgentName(agentName)
	data, err := os.ReadFile(ciFailingPath)
	if err != nil {
		data = nil
	}
	return formatRedPRFixDataWithSignals(data, base, github.CIFailureSignals().ForAgent(base))
}

// CIFailureKickMessage is the targeted kick a webhook CI-failure signal
// sends to the owning agent: the same fix-before-new section a regular kick
// would carry, under a one-line header naming the trigger. Empty when the
// agent is not PR-capable or has no red PR to repair (e.g. every failing
// check is red on the base branch too).
func (s *Scheduler) CIFailureKickMessage(agentName string, sig github.CIFailureSignal) string {
	if s == nil || !s.isPRCapableAgent(agentName) {
		return ""
	}
	section := s.redPRFixSection(agentName)
	if section == "" {
		return ""
	}
	header := fmt.Sprintf("CI failed on %s#%d (%s) — repair it now. Detection was out-of-band (webhook); do not watch or poll CI yourself.\n",
		sig.Repo, sig.Number, sig.Check)
	if context := sig.Integration; context != nil && context.CandidateID != "" {
		header = fmt.Sprintf("Integration CI failed on candidate %s, tested commit %s tree %s, base %s, workflow policy %s, producer %s, attempt %d. Ordered PR:head members: %s. Existing ownership routes %s#%d at exact head %s to you first; this is NOT attribution of blame. Inspect the failed job %s (%s), coordinate shared prerequisite repair once, and retain every normal CI/review/trust/hold/current-base gate. Do not copy green statuses, cancel shared jobs, poll CI, or modify other members without their owner.\n",
			context.CandidateID, context.TestedSHA, context.TestedTree, context.BaseSHA, context.Policy, context.Producer, context.Attempt,
			strings.Join(context.Members, ", "), sig.Repo, sig.Number, sig.HeadSHA, sig.Check, sig.DetailsURL)
		if context.RunID > 0 && context.SuiteID > 0 {
			header += fmt.Sprintf("Evidence source %s; workflow run %d, suite %d.\n", context.Source, context.RunID, context.SuiteID)
		} else {
			header += "Workflow run/suite binding unavailable; membership observation is not passing policy proof.\n"
		}
	}
	return header + section
}

// heldRedPRNote is appended to a held PR's entry in the fix-before-new
// block: the hold is a merge checkpoint, not a repair checkpoint
// (hivecommons/hive#7438), and the agent's own policy says never to touch a
// held item — this line is the explicit, narrow exception.
const heldRedPRNote = "held for human review — fix CI, do not remove the hold"

// heldRedPRExemptAgent is the one agent whose held PRs are NOT routed back
// for repair: the level-hold comment tells humans that cross-repo outreach
// PRs are held for their review, so a held outreach PR must not be edited
// after a human may have started reading it. A same-repo outreach PR carries
// no outreach hold (hivecommons/hive, operator decision 2026-10-16) and so is
// routed for repair like any other lane's red PR.
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
	return formatRedPRFixDataWithSignals(data, agent, nil)
}

// ciFailureSignalEvidence is the evidence line a webhook signal contributes:
// the check name plus the log pointer, so the agent can open the run
// without listing checks itself.
func ciFailureSignalEvidence(sig github.CIFailureSignal) string {
	line := "webhook: " + sig.Check + " failed"
	if sig.Conclusion != "" && sig.Conclusion != "failure" {
		line += " (" + sig.Conclusion + ")"
	}
	if sig.DetailsURL != "" {
		line += " — log: " + sig.DetailsURL
	}
	return line
}

// formatRedPRFixDataWithSignals is formatRedPRFixData with the agent's
// webhook CI-failure signals folded in: a signal for a PR the file already
// lists adds its check and log pointer to that row; a signal for a PR the
// file does not know yet (the file is rewritten per eval tick) becomes its
// own row. A signal whose check the file reports red on the base branch is
// dropped like any other base-red failure.
func formatRedPRFixDataWithSignals(data []byte, agent string, signals []github.CIFailureSignal) string {
	type ciFailingRow struct {
		Number           int      `json:"number"`
		Repo             string   `json:"repo"`
		Title            string   `json:"title"`
		Agent            string   `json:"agent"`
		FailingChecks    []string `json:"failing_checks"`
		Excerpt          string   `json:"excerpt"`
		Escalated        bool     `json:"escalated"`
		FromFork         bool     `json:"from_fork"`
		ReachableAction  string   `json:"reachable_action"`
		Adopted          bool     `json:"adopted"`
		SentinelAlert    bool     `json:"sentinel_alert"`
		HeadRepo         string   `json:"head_repo"`
		Held             bool     `json:"held"`
		MergeableState   string   `json:"mergeable_state"`
		Conflict         bool     `json:"conflict"`
		ReroutedFrom     string   `json:"rerouted_from"`
		DeferredIncident int      `json:"deferred_incident"`
		BaseRedChecks    []string `json:"base_red_checks"`
		BaseRed          bool     `json:"base_red"`
		RebaseAttempt    int      `json:"rebase_attempt"`
		RebaseMax        int      `json:"rebase_attempts_max"`
		BaseRef          string   `json:"base_ref"`
		TrustedAuthor    bool     `json:"trusted_author"`
	}
	var payload struct {
		Items []ciFailingRow `json:"ci_failing"`
	}
	if len(data) > 0 && json.Unmarshal(data, &payload) != nil {
		return ""
	}
	if len(signals) > 0 {
		known := map[string]int{}
		for i, r := range payload.Items {
			known[fmt.Sprintf("%s#%d", strings.ToLower(r.Repo), r.Number)] = i
		}
		for _, sig := range signals {
			key := fmt.Sprintf("%s#%d", strings.ToLower(sig.Repo), sig.Number)
			if i, ok := known[key]; ok {
				r := &payload.Items[i]
				if slices.Contains(r.BaseRedChecks, sig.Check) {
					continue
				}
				if r.BaseRed {
					// The file says every failing check is base-red; a new
					// check the file has not seen yet changes that.
					r.BaseRed = false
				}
				if !slices.Contains(r.FailingChecks, sig.Check) {
					r.FailingChecks = append(r.FailingChecks, sig.Check)
				}
				r.Excerpt = strings.TrimSpace(r.Excerpt + "\n" + ciFailureSignalEvidence(sig))
				continue
			}
			row := ciFailingRow{
				Number:        sig.Number,
				Repo:          sig.Repo,
				Title:         sig.Title,
				Agent:         sig.Agent,
				FailingChecks: []string{sig.Check},
				Excerpt:       ciFailureSignalEvidence(sig),
			}
			if row.Title == "" {
				row.Title = "(title pending)"
			}
			payload.Items = append(payload.Items, row)
			known[key] = len(payload.Items) - 1
		}
	}
	var mine, deferred, baseRed []ciFailingRow
	forks := 0
	for _, pr := range payload.Items {
		if pr.Escalated {
			continue // needs-human: hands off for agents
		}
		if pr.FromFork && !pr.Adopted {
			// A fork PR can never be "yours" — unless the hive adopted it in
			// place (stage 074): then maintainer_can_modify lets the fixer
			// push to the contributor's branch and the row is listed.
			// Otherwise: the hive pushes only to the base
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
		if pr.BaseRed {
			// Every failing check is red on the base branch too: no fix
			// the agent pushes can turn it green, so it is named but never
			// listed for repair (base_branch_red.go).
			baseRed = append(baseRed, pr)
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
	if len(baseRed) > 0 {
		refs := make([]string, 0, len(baseRed))
		for _, pr := range baseRed {
			refs = append(refs, fmt.Sprintf("%s#%d (%s)", pr.Repo, pr.Number, strings.Join(pr.BaseRedChecks, ", ")))
		}
		b.WriteString(fmt.Sprintf("(%d of your red PR(s) are NOT listed: their only failing checks are red on the base branch too — not your PR's fault; do not push, retry or re-triage them; they return here if still red once the base recovers: %s)\n", len(baseRed), strings.Join(refs, ", ")))
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
		if pr.Adopted {
			b.WriteString(fmt.Sprintf("    ADOPTED contributor PR (stage 074, head repo %s): the author is offline; `gh pr checkout` then push to the PR's own branch — maintainer_can_modify grants access.\n", pr.HeadRepo))
			b.WriteString(adoptedPRStatusRule)
			if pr.SentinelAlert {
				b.WriteString("    " + adoptedSentinelNote + "\n")
			}
		}
		if pr.Conflict {
			state := strings.TrimSpace(pr.MergeableState)
			if state == "" {
				state = "dirty"
			}
			b.WriteString("    conflict: mergeable_state=" + state + " — merge the base branch or rebase the PR branch\n")
			b.WriteString("    " + rebaseResolveInstructions(pr.BaseRef, pr.RebaseAttempt, pr.RebaseMax, pr.TrustedAuthor) + "\n")
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
// Escalated (needs-human) PRs are never listed. Appended to the dynamic tail
// directly after the red-CI block (kick_layout.go) so the two stuck-PR lists
// sit together.
func (s *Scheduler) addReviewThreadFixFirst(agentName string, message string) string {
	if message == "" || !s.isPRCapableAgent(agentName) {
		return message
	}
	data, err := os.ReadFile(reviewThreadsPath)
	if err != nil {
		return message
	}
	section := formatReviewThreadFixDataScanned(data, s.cfg.BaseAgentName(agentName), s.reviewBotsResolveAfterFix(), s.reviewThreadEnforcer())
	if section == "" {
		return message
	}
	return appendKickSection(message, section)
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

// formatReviewThreadFixData is formatReviewThreadFixDataScanned with no
// ioscan enforcement, for formatting tests.
func formatReviewThreadFixData(data []byte, agent string, resolveAfterFix bool) string {
	return formatReviewThreadFixDataScanned(data, agent, resolveAfterFix, nil)
}

// formatReviewThreadFixDataScanned renders the review-thread fix-before-new
// section for one agent from raw review-threads.json bytes. Empty result means
// the agent has no open, non-escalated PR with an actionable bot thread.
//
// Each PR's title and thread bodies are external text about to be injected
// into a kick, so they go through enforce as ONE conversation (F11,
// hivecommons/hive#6685): a withheld finding is replaced by the redaction
// marker, and a PR whose thread trips the fail-closed policy is left out of
// the section entirely rather than injected in part.
func formatReviewThreadFixDataScanned(data []byte, agent string, resolveAfterFix bool, enforce threadEnforcer) string {
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
		scanned, failClosed := scanReviewThreadPR(pr, enforce)
		if failClosed {
			continue
		}
		mine = append(mine, scanned)
		threads += len(scanned.Threads)
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
		// direction of withholding resolution, never granting it. Nor is
		// step 1's push allowed there: a hive-signed commit on someone
		// else's PR fails the squash sign-off gate (hivecommons/hive#12897).
		if pr.HumanOpened {
			b.WriteString("    step 1: DO NOT check out, commit to, or push this branch — this PR is not yours (fix_human_prs), and a commit signed off by the hive fails its squash sign-off gate (hivecommons/hive#12897). Put the fix in your in-thread reply as a ```suggestion block (or a ```diff patch) for the author to apply.\n")
			if resolveAfterFix {
				b.WriteString("    step 3: DO NOT resolve — a person closes its threads (hivecommons/hive#9361).\n")
			}
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

// reviewThreadEnforcer binds enforceThread to the review surface.
func (s *Scheduler) reviewThreadEnforcer() threadEnforcer {
	return func(segments []string) ([]string, bool) { return s.enforceThread(segments, reviewSource) }
}

// scanReviewThreadPR runs one PR's title and thread bodies through enforce as
// an ordered conversation and returns a copy with withheld text replaced. The
// input is never mutated: the report is shared with other readers.
func scanReviewThreadPR(pr github.ReviewThreadPR, enforce threadEnforcer) (github.ReviewThreadPR, bool) {
	if enforce == nil {
		return pr, false
	}
	segments := make([]string, 0, len(pr.Threads)+1)
	segments = append(segments, pr.Title)
	for _, t := range pr.Threads {
		segments = append(segments, t.Body)
	}
	scanned, failClosed := enforce(segments)
	if failClosed {
		return pr, true
	}
	out := pr
	out.Title = scanned[0]
	out.Threads = make([]github.ReviewThread, len(pr.Threads))
	for i, t := range pr.Threads {
		t.Body = scanned[i+1]
		out.Threads[i] = t
	}
	return out, false
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
			title, verdict := s.enforceTextFrom(item.Title, prSource(heldPRs[fmt.Sprintf("%s#%d", item.Repo, item.Number)]))
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
	return review.QueueIndependentReviewSection()
}

func (s *Scheduler) addIndependentReviewSection(agentName, msg string) string {
	if msg == "" {
		return msg
	}
	if section := s.independentReviewSection(agentName); section != "" && !strings.Contains(msg, "## Independent review") {
		return insertAfterKickHeader(msg, section)
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

// adoptedSentinelNote follows an adopted-PR line whose PR still carries the
// sentinel alert: the alert is a merge checkpoint for a maintainer, not a
// repair checkpoint, so the fixer repairs the head and leaves the label.
const adoptedSentinelNote = "sentinel-alert: repair CI / rebase as usual, but do NOT remove the sentinel-alert label — a maintainer clears it before the merge"

// adoptedPRStatusRule follows every adopted-PR line in a fix kick: the
// stage label, not the check list, says what the hive still owes, and a
// pending `tide` or a missing lgtm is never the hive's blocker below stage
// 080 (hivecommons/hive#12858).
const adoptedPRStatusRule = "    ADOPTED BRANCH: when needs-rebase/conflicting, fetch the contributor's actual fork branch and the PR base, rebase onto the base, preserve original authors and Signed-off-by trailers (never --reset-author), and push back to that fork with --force-with-lease against the observed head. Stop if the contributor moved it; never overwrite their new work. Sign only your own new commits.\n    STATUS RULE: green CI is not done on a 074 PR. Read the stage comment/lane reason: an open review-swarm finding (reviewer-verdict:changes_requested) is YOUR blocker — fix it and let the swarm re-review. Never report \"only tide remains\" or \"needs maintainer lgtm/approve\" unless the stage label is already >= stage/080; below that, a pending tide check means the lane has not admitted the head yet, not that a person owes an approval.\n"

// rebaseResolveInstructions is the fixer's rebase-resolve task for a
// conflicting PR (hivecommons/hive#15103): rebase onto the base branch,
// resolve, force-with-lease to the PR's own branch, never open a new PR.
// attempt/max is the loop guard the fixer sees so it knows a second dirty
// head ends the hive's attempts.
func rebaseResolveInstructions(baseRef string, attempt, max int, trustedAuthor bool) string {
	base := strings.TrimSpace(baseRef)
	if base == "" {
		base = "the base branch"
	} else {
		base = "origin/" + base
	}
	if attempt < 1 {
		attempt = 1
	}
	if max < attempt {
		max = attempt
	}
	var b strings.Builder
	fmt.Fprintf(&b, "rebase-resolve (attempt %d/%d): `gh pr checkout`, `git rebase %s`, resolve every conflict keeping both sides' intent, re-run the build and tests, then `git push --force-with-lease` to the PR's OWN branch. Do not open a new PR; do not squash away the author's commits.", attempt, max, base)
	if trustedAuthor {
		b.WriteString(" This is a trusted author's PR the hive carries on their behalf — keep their authorship and sign-off intact.")
	}
	if attempt >= max {
		b.WriteString(" Last attempt: if the head is still conflicting after this push the hive stops and asks a person.")
	}
	return b.String()
}
