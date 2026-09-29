package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func (s *Scheduler) buildScannerMessage(issues []github.Issue, actionable *github.ActionableResult) string {
	var b strings.Builder

	b.WriteString("[agent:scanner]\n")
	b.WriteString("YOUR WORK LIST (pre-filtered — hold/ADOPTERS/drafts excluded, classified):\n")
	b.WriteString(s.issueFilterNotice())

	scannerIssues := issues

	b.WriteString(fmt.Sprintf("ACTIONABLE ISSUES (%d, human/priority first):\n", len(scannerIssues)))
	if len(scannerIssues) > 0 {
		b.WriteString(issuePriorityNote)
	}
	shown := 0
	for _, issue := range scannerIssues {
		if shown >= s.issueCap() {
			break
		}
		tier := string(issue.ComplexityTier)
		if len(tier) > 0 {
			tier = tier[:1]
		}
		tracker := ""
		if issue.IsTracker {
			tracker = " [TRACKER]"
		}
		title := issue.Title
		const maxTitleRunes = 60
		if runes := []rune(title); len(runes) > maxTitleRunes {
			title = string(runes[:maxTitleRunes])
		}
		b.WriteString(fmt.Sprintf("  %dm %s %s [%s/%s] [%s] %s%s\n",
			issue.AgeMinutes, issueDisplayRef(issue), issuePriorityMarker(issue),
			tier, issue.ModelRec,
			strings.Join(issue.Labels, ","),
			title, tracker))
		shown++
	}

	b.WriteString(fmt.Sprintf("ACTIONABLE PRs (%d):\n", actionable.PRs.Count))
	prLimit := s.prCap()
	for i, pr := range actionable.PRs.Items {
		if i >= prLimit {
			b.WriteString(prListOverflowLine(len(actionable.PRs.Items)-i, prLimit))
			break
		}
		title := pr.Title
		const maxPRTitleRunes = 70
		if runes := []rune(title); len(runes) > maxPRTitleRunes {
			title = string(runes[:maxPRTitleRunes])
		}
		annotation, _ := s.enforceIssueTextVerdict(prKickAnnotation(pr, "scanner"))
		b.WriteString(fmt.Sprintf("  %s#%d by @%s%s %s %s\n", pr.Repo, pr.Number, pr.Author, forkAnnotation(pr), annotation, title))
	}

	if actionable.Issues.SLAViolations > 0 {
		b.WriteString(fmt.Sprintf("\n⚠️ %d SLA VIOLATIONS (>30 min)\n", actionable.Issues.SLAViolations))
	}

	// Surfaces exactly the work step 2 already asks for ("Close stale drafts
	// (>48h, needs-rebase + dco-no, or fix already merged)") — which had
	// nothing to act on before, since fetchPRs drops every draft before this
	// prompt is ever built. See kubestellar/hive#3963.
	if len(actionable.PRs.StaleDrafts) > 0 {
		b.WriteString(fmt.Sprintf("\nYOUR STALE DRAFT PRs (%d, >48h old — finish, mark ready, or close):\n", len(actionable.PRs.StaleDrafts)))
		for i, d := range actionable.PRs.StaleDrafts {
			if i >= prLimit {
				b.WriteString(prListOverflowLine(len(actionable.PRs.StaleDrafts)-i, prLimit))
				break
			}
			title := d.Title
			const maxDraftTitleRunes = 70
			if runes := []rune(title); len(runes) > maxDraftTitleRunes {
				title = string(runes[:maxDraftTitleRunes])
			}
			b.WriteString(fmt.Sprintf("  %s#%d %s\n", d.Repo, d.Number, title))
		}
	}

	if knowledgeSection := s.primeKnowledge(scannerIssues); knowledgeSection != "" {
		b.WriteString("\n")
		b.WriteString(knowledgeSection)
	}

	b.WriteString("\nWORKFLOW:\n")
	b.WriteString("  1. Check beads (`bd list --status open`) for context from previous cycles\n")
	b.WriteString("  2. Quick merges + cleanup (10 min cap) — merge PRs whose required checks are GREEN using a squash merge via your App token (MCP `merge_pull_request` with `merge_method: \"squash\"`, or `gh pr merge --squash`). Do NOT use `--admin` — never force-merge past pending or failing CI; wait for the required checks to pass. Ensure the PR body cites the issue it addresses: ask does merging this PR leave anything for that issue to track? If nothing, write `Closes #<issue>` — the default, auto-closes on merge. Use `Refs #<issue>` or `Part of #<issue>` (non-closing) only for an epic/multi-phase tracker or a deliberately partial fix, and say on the same line what remains and why; if the remainder needs a human, write `Refs #<issue> (needs-human: <reason>)`. Close stale drafts (>48h, needs-rebase + dco-no, or fix already merged). `@dependabot rebase` stale ones. Move on after 10 min.\n")
	b.WriteString("  3. Fix blockers — find the ONE fix that unblocks the most PRs/issues. Clone, fix, push, merge.\n")
	b.WriteString("  4. Crank quick fixes — launch background agents using the Agent tool (run_in_background: true) to fix remaining issues in parallel. One PR per issue, move fast.\n")

	return b.String()
}

func (s *Scheduler) buildCIMaintainerMessage(actionable *github.ActionableResult) string {
	var b strings.Builder
	b.WriteString("[agent:ci-maintainer]\n")
	b.WriteString("Post-merge health check. Review CI status, GA4 errors, workflow health.\n")
	b.WriteString(fmt.Sprintf("Queue: %d issues, %d PRs, %d on hold\n",
		actionable.Issues.Count, actionable.PRs.Count, actionable.Hold.Total))
	return b.String()
}

func (s *Scheduler) buildSupervisorMessage(actionable *github.ActionableResult) string {
	now := time.Now().Local()
	var b strings.Builder
	b.WriteString("[agent:supervisor]\n")
	b.WriteString(fmt.Sprintf("MONITORING PASS %s\n\n", now.Format("1/2 3:04 PM MST")))

	b.WriteString(s.ghAuthInstructions())
	b.WriteString(s.reposSection())

	b.WriteString("ROLE: You are the SUPERVISOR. Your job is to MONITOR other agents, NOT to fix issues yourself.\n")
	b.WriteString("⛔ NEVER work on issues directly — that is scanner's job.\n")
	b.WriteString("⛔ NEVER open PRs or commit code — that is scanner's and architect's job.\n")
	b.WriteString("⛔ NEVER merge PRs — that is scanner's job.\n")
	b.WriteString("⛔ NEVER launch background fix agents — that is scanner's job.\n\n")

	b.WriteString("YOUR RESPONSIBILITIES:\n")
	b.WriteString("  1. Check all agent tmux panes — are they working or stuck at a prompt?\n")
	b.WriteString("  2. Check if agents are idle when they should be working (queue > 0 but agent idle)\n")
	b.WriteString("  3. Report agent health: running/stuck/crashed/idle/rate-limited\n")
	b.WriteString("  4. Flag stale agents that haven't produced output in > 1 cadence cycle\n")
	b.WriteString("  5. Summarize current state: what each agent is doing, what's stuck, what needs attention\n\n")

	b.WriteString(fmt.Sprintf("Queue: %d issues, %d PRs, %d on hold, %d SLA violations\n",
		actionable.Issues.Count, actionable.PRs.Count,
		actionable.Hold.Total, actionable.Issues.SLAViolations))

	b.WriteString("\nBeads: ~/supervisor-beads\n")
	return b.String()
}

var mergeEligiblePath = "/var/run/hive-metrics/merge-eligible.json"

var ciFailingPath = "/var/run/hive-metrics/ci-failing.json"

func formatMergeEligibleData(data []byte, limit int) string {
	return formatMergeEligibleDataFor(data, nil, limit)
}

// heldMarker annotates a red PR that is under hold. Held PRs entered this list
// with hivecommons/hive#7438 so they can be repaired; the marker is what keeps
// an agent from reading their presence as permission to merge or unhold them.
func heldMarker(held bool) string {
	if !held {
		return ""
	}
	return " [" + heldRedPRNote + "]"
}

// buildCIFailingListFor renders the CI-failing list narrowed to the repos the
// predicate accepts (#6204). A nil predicate keeps everything.
func (s *Scheduler) buildCIFailingList() string {
	return s.buildCIFailingListFor(nil)
}

// ghAuthInstructions tells the agent how to authenticate each tool class.
//
// The answer for every class is THE AGENT DOES NOTHING (#1861): git is served
// by the credential helper, and gh by the wrapper the image installs AS `+"`gh`"+`
// (src/Dockerfile: COPY bin/gh-wrapper.sh /usr/local/bin/gh), which reads
// HIVE_AGENT_TOKEN_CACHE and exports the agent's tier-scoped App token itself,
// per invocation. No token material has to reach the agent for either tool.
//
// This block used to instruct every agent to run
// `+"`export GH_TOKEN=$(cat .../agent-tokens/gh-token-<agent>.cache)`"+`, which was
// wrong three ways:
//
//   - It put a live App installation token into the agent's OWN reasoning and
//     transcript — the exact exposure #3842/#3889 removed from the native-install
//     prompt, differing only in blast radius (tier-scoped here, fleet-wide there).
//     A token in the transcript is one prompt injection from exfiltration, and
//     #1861's whole goal is that agents hold no token material.
//   - It was redundant. The wrapper had already injected the same token before
//     the agent's command ran.
//   - It could BREAK the session. bin/agent-launch.sh deliberately leaves
//     GH_TOKEN unset because "Copilot CLI uses GH_TOKEN for its own Copilot API
//     auth, which rejects GitHub App server-to-server tokens" — so a Copilot-
//     backed agent following this instruction could lose model auth. The final
//     bullet below already said the Copilot CLI owns that variable, contradicting
//     the instruction four lines above it.
//
// Keep this block free of any token path or GH_TOKEN assignment. Both hive
// tools authenticate the agent without its participation; anything that tells
// an agent to fetch, read, echo or export a credential is a regression, and
// TestGHAuthInstructions_NeverHandsTheAgentAToken pins that.
// The agent name is no longer a parameter: nothing in this block is per-agent
// any more, which is the point — there is no per-agent path for the agent to
// read, because the hive applies the per-agent token on its behalf.
func (s *Scheduler) ghAuthInstructions() string {
	return `## Project Authentication

- The GitHub App is the WRITE GATE. Every write to GitHub — opening or updating
  an issue or PR, commenting, and merging — goes through this hive's GitHub App
  (github.com or GitHub Enterprise, per the primary repo). If the App is not
  installed you have NO write credential: stay advisory (read, KB, beads) and do
  not attempt to write. Never substitute a personal user token to work around a
  missing App. Login/identity is a separate concern and is always github.com.
- Writes are authored by the App bot identity, not a personal account. Do not
  set git user.name/user.email to a human, and do not pass 'gh pr create' or
  'git commit' an explicit --author: let the App identity stand.
- To OPEN A PULL REQUEST, use ` + "`hive-open-pr`" + ` — the hive opens it with the
  App token so it is authored by the App bot ("<slug>[bot]"), never the login user:
    hive-open-pr --repo <org>/<repo> --head <your-branch> --title "<title>" \
      --body "<body citing the issue>" --issues <N>
  --body-file <path> is accepted in place of --body, exactly as gh accepts it.
  Pass --issues <N> naming the issue this PR is for whenever the work started
  from an issue: the hive verifies the body actually references that issue and
  refuses the request otherwise, and it always refuses an empty body.
  Cite the issue correctly: write ` + "`Closes #N`" + ` whenever this PR resolves
  issue N — that is the NORMAL case, and GitHub then closes the issue on
  merge. An issue the PR fixed but never closes stays open as standing noise
  for the maintainers. Write ` + "`Refs #N`" + ` (non-closing) ONLY when part of
  issue N is deliberately left open — an epic/multi-phase tracker, or a
  partial fix — and say on the SAME line what is left and why. If the
  remainder requires a human, write ` + "`Refs #N (needs-human: <reason>)`" + `. Never use Refs
  as the cautious default.
  Do NOT open PRs with the GitHub MCP (create_pull_request / create_pull_request_with_copilot)
  or raw 'gh pr create' — those author the PR as the Copilot login user. 'gh pr create'
  is auto-redirected to hive-open-pr, but prefer calling hive-open-pr directly.
  (Push your branch first; hive-open-pr requests the PR, the hive opens it within ~10s.)
- git push / git fetch: run them normally. A credential helper supplies the
  App-scoped push token automatically. Do NOT export GH_TOKEN for git and do
  NOT use HIVE_GITHUB_TOKEN (it is read-only; overriding breaks pushes).
- gh CLI: just run ` + "`gh`" + `. Authentication is already handled for you — the hive
  wraps every gh call and applies YOUR tier-scoped App token to it. You do not
  need, and must not set up, any credential of your own: do NOT export GH_TOKEN,
  and do NOT go looking for, read, or echo a token file — not one of your own,
  not another agent's, not a shared one. There is no token file you are meant
  to open. A token you put on a command line is a token in your transcript, and
  exporting GH_TOKEN can break the Copilot CLI's own auth, which owns that
  variable. If gh reports an auth problem, report it — do not go find a token.
- A missing GH_TOKEN at session start is therefore expected and is never a
  blocker — it is what "already handled for you" looks like from inside the
  session. All GitHub traffic flows through the hive proxy either way.

`
}

func (s *Scheduler) buildGenericMessage(agentName string, issues []github.Issue, actionable *github.ActionableResult) string {
	baseName := s.cfg.BaseAgentName(agentName)
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[agent:%s]\n", agentName))
	b.WriteString(s.issueFilterNotice())

	agentIssues := filterByLane(issues, baseName)
	if len(agentIssues) > 0 {
		b.WriteString(fmt.Sprintf("Work items (%d):\n", len(agentIssues)))
		b.WriteString(issuePriorityNote)
		for _, issue := range agentIssues {
			b.WriteString(fmt.Sprintf("  %s %s %s\n", issueDisplayRef(issue), issuePriorityMarker(issue), issue.Title))
		}
	}

	if knowledgeSection := s.primeKnowledge(agentIssues); knowledgeSection != "" {
		b.WriteString("\n")
		b.WriteString(knowledgeSection)
	}

	return b.String()
}

const defaultCoverageTargetPct = 91.0

func (s *Scheduler) buildQualityMessage(issues []github.Issue, actionable *github.ActionableResult) string {
	var b strings.Builder

	b.WriteString("[agent:quality]\n")
	b.WriteString("TEST STRATEGIST — build test coverage from current level toward target.\n\n")

	b.WriteString(fmt.Sprintf("COVERAGE TARGET: %.0f%%\n", defaultCoverageTargetPct))

	qualityIssues := filterByLane(issues, "quality")
	if len(qualityIssues) > 0 {
		b.WriteString(fmt.Sprintf("\nTEST-RELATED ISSUES (%d):\n", len(qualityIssues)))
		b.WriteString(issuePriorityNote)
		shown := 0
		for _, issue := range qualityIssues {
			if shown >= s.issueCap() {
				break
			}
			title := issue.Title
			const maxTitleRunes = 60
			if runes := []rune(title); len(runes) > maxTitleRunes {
				title = string(runes[:maxTitleRunes])
			}
			b.WriteString(fmt.Sprintf("  %s %s [%s] %s\n",
				issueDisplayRef(issue), issuePriorityMarker(issue),
				strings.Join(issue.Labels, ","),
				title))
			shown++
		}
	}

	b.WriteString("\nMATURITY-ADAPTIVE INSTRUCTIONS:\n")
	b.WriteString("  If project has NO tests or CI (Level 1-2, mode=suggest):\n")
	b.WriteString("    - Propose test scaffolding. Create stub files with TODO bodies.\n")
	b.WriteString("    - Suggest which test framework to adopt. Open draft PRs.\n")
	b.WriteString("    - Create shared test utilities (factories, fixtures, helpers).\n")
	b.WriteString("  If project has CI but coverage is below target (Level 3, mode=gate):\n")
	b.WriteString("    - Identify the highest-impact untested code paths.\n")
	b.WriteString("    - Create test PRs that raise coverage above the CI threshold.\n")
	b.WriteString("    - Focus on integration tests for critical paths.\n")
	b.WriteString("  If project has full CI + TDD markers (Level 4, mode=tdd):\n")
	b.WriteString("    - Identify modules without red-green discipline.\n")
	b.WriteString("    - Create regression tests for recent bug fixes missing them.\n")
	b.WriteString("    - Enforce test-first for new features.\n")

	if knowledgeSection := s.primeKnowledge(qualityIssues); knowledgeSection != "" {
		b.WriteString("\n")
		b.WriteString(knowledgeSection)
	}

	b.WriteString("\nWORKFLOW:\n")
	b.WriteString("  1. Analyze coverage reports and identify untested modules.\n")
	b.WriteString("  2. Prioritize: regression-prone code > new features > utilities.\n")
	b.WriteString("  3. Create test PRs in batches (max 3 concurrent).\n")
	b.WriteString("  4. Each PR must include: test file, required mocks/factories, coverage delta estimate.\n")
	b.WriteString("  5. Write test_scaffold and pattern facts to the knowledge wiki for future agents.\n")
	b.WriteString("⛔ NEVER run gh issue list, gh pr list, gh search issues — the work list above is your ONLY source.\n")

	return b.String()
}

func (s *Scheduler) buildArchitectMessage(issues []github.Issue, actionable *github.ActionableResult) string {
	var b strings.Builder
	b.WriteString("[agent:architect]\n")
	b.WriteString("Full architect pass — refactor/perf scan across all repos.\n\n")

	b.WriteString(s.ghAuthInstructions())

	architectIssues := filterByLane(issues, "architect")
	if len(architectIssues) > 0 {
		b.WriteString(fmt.Sprintf("ARCHITECTURE-RELATED ISSUES (%d):\n", len(architectIssues)))
		b.WriteString(issuePriorityNote)
		shown := 0
		for _, issue := range architectIssues {
			if shown >= s.issueCap() {
				break
			}
			title := issue.Title
			const maxTitleRunes = 60
			if runes := []rune(title); len(runes) > maxTitleRunes {
				title = string(runes[:maxTitleRunes])
			}
			b.WriteString(fmt.Sprintf("  %s %s [%s] %s\n",
				issueDisplayRef(issue), issuePriorityMarker(issue),
				strings.Join(issue.Labels, ","),
				title))
			shown++
		}
		b.WriteString("\n")
	}

	b.WriteString(fmt.Sprintf("Queue: %d issues, %d PRs, %d on hold\n\n",
		actionable.Issues.Count, actionable.PRs.Count, actionable.Hold.Total))

	b.WriteString("YOUR RESPONSIBILITIES:\n")
	b.WriteString("  1. Scan repos for refactoring opportunities (dead code, duplication, tech debt)\n")
	b.WriteString("  2. Identify performance bottlenecks and propose improvements\n")
	b.WriteString("  3. Review architecture decisions and flag inconsistencies\n")
	b.WriteString("  4. Create RFC-style issues for large changes that need discussion\n")
	b.WriteString("  5. Open PRs for small refactors that improve maintainability\n\n")

	b.WriteString("AUTONOMY RULES:\n")
	b.WriteString("  ✅ May do without approval: refactoring PRs, perf improvements, dead code removal\n")
	b.WriteString("  ❌ Needs human approval: API changes, dependency upgrades, schema migrations\n\n")

	if knowledgeSection := s.primeKnowledge(architectIssues); knowledgeSection != "" {
		b.WriteString(knowledgeSection)
		b.WriteString("\n")
	}

	b.WriteString("Beads: ~/architect-beads\n")

	return b.String()
}

func (s *Scheduler) buildOutreachMessage(actionable *github.ActionableResult) string {
	now := time.Now().Local()
	var b strings.Builder
	b.WriteString("[agent:outreach]\n")
	b.WriteString(fmt.Sprintf("Full outreach pass. Time: %s\n\n", now.Format("1/2 3:04 PM MST")))

	b.WriteString(s.ghAuthInstructions())

	b.WriteString("YOUR RESPONSIBILITIES:\n")
	b.WriteString("  1. Open PRs on external repos to promote adoption (awesome-lists, adopters files, install guides)\n")
	b.WriteString("  2. Check blocked_orgs before opening new PRs — one PR per org at a time\n")
	b.WriteString("  3. Monitor open outreach PRs for review feedback and address comments\n")
	b.WriteString("  4. Track placement progress toward target\n\n")

	b.WriteString("RULES:\n")
	b.WriteString("  ⛔ NEVER re-query PR counts with gh search — use pre-computed metrics\n")
	b.WriteString("  ⛔ NEVER open a second PR on an org that already has an open outreach PR\n")
	b.WriteString("  ⛔ NEVER open PRs on repos without verifying a matching mission exists first\n")
	b.WriteString("  ✅ Check ADOPTERS.MD before proposing cold outreach to any org\n\n")

	b.WriteString("Beads: ~/outreach-beads\n")

	return b.String()
}

func (s *Scheduler) buildSecCheckMessage(actionable *github.ActionableResult) string {
	now := time.Now().Local()
	var b strings.Builder
	b.WriteString("[agent:sec-check]\n")
	b.WriteString(fmt.Sprintf("Security review pass. Time: %s\n\n", now.Format("1/2 3:04 PM MST")))

	b.WriteString(s.ghAuthInstructions())

	b.WriteString("YOUR RESPONSIBILITIES:\n")
	b.WriteString("  1. Scan repos for security vulnerabilities (OWASP top 10, dependency CVEs)\n")
	b.WriteString("  2. Review recent PRs for security implications\n")
	b.WriteString("  3. Check for exposed secrets, hardcoded credentials, insecure defaults\n")
	b.WriteString("  4. Verify security headers, CSP policies, and auth middleware\n")
	b.WriteString("  5. Open issues or PRs for any findings\n\n")

	b.WriteString(fmt.Sprintf("Queue: %d issues, %d PRs\n",
		actionable.Issues.Count, actionable.PRs.Count))

	return b.String()
}

// buildMergeEligibleListFor renders the merge-eligible section of a kick,
// narrowed to the repos the predicate accepts (#6204). A nil predicate keeps
// everything, which is what an unscoped agent gets.
func (s *Scheduler) buildMergeEligibleListFor(keep func(repo string) bool) string {
	data, err := os.ReadFile(mergeEligiblePath)
	if err != nil {
		return "(none)\n"
	}
	return formatMergeEligibleDataFor(data, keep, s.prCap())
}

// formatMergeEligibleDataFor narrows to the repos keep accepts (#6204, nil =
// all) and caps the rendered list at limit (governor.kick_limits.max_prs,
// hivecommons/hive#7368; 0 = uncapped).
func formatMergeEligibleDataFor(data []byte, keep func(repo string) bool, limit int) string {
	var payload struct {
		Items []struct {
			Number int    `json:"number"`
			Repo   string `json:"repo"`
			Title  string `json:"title"`
			Queued bool   `json:"queued"`
		} `json:"merge_eligible"`
	}
	if json.Unmarshal(data, &payload) != nil || len(payload.Items) == 0 {
		return "(none)\n"
	}
	var b strings.Builder
	shown := 0
	for i, pr := range payload.Items {
		if keep != nil && !keep(pr.Repo) {
			continue
		}
		if limit > 0 && shown >= limit {
			b.WriteString(prListOverflowLine(len(payload.Items)-i, limit))
			break
		}
		shown++
		queued := ""
		if pr.Queued {
			queued = " [queued for auto-merge]"
		}
		b.WriteString(fmt.Sprintf("  #%d %s%s — %s\n", pr.Number, pr.Repo, queued, pr.Title))
	}
	if b.Len() == 0 {
		return "(none)\n"
	}
	return b.String()
}

func (s *Scheduler) buildCIFailingListFor(keep func(repo string) bool) string {
	data, err := os.ReadFile(ciFailingPath)
	if err != nil {
		return "(none)\n"
	}
	type ciFailingRow struct {
		Number        int      `json:"number"`
		Repo          string   `json:"repo"`
		Title         string   `json:"title"`
		Author        string   `json:"author"`
		HeadSHA       string   `json:"head_sha"`
		HeadRef       string   `json:"head_ref"`
		HeadRepo      string   `json:"head_repo"`
		FromFork      bool     `json:"from_fork"`
		Held          bool     `json:"held"`
		FailingChecks []string `json:"failing_checks"`
		Excerpt       string   `json:"excerpt"`
	}
	var payload struct {
		Items []ciFailingRow `json:"ci_failing"`
	}
	if json.Unmarshal(data, &payload) != nil || len(payload.Items) == 0 {
		return "(none)\n"
	}
	if keep != nil {
		kept := payload.Items[:0]
		for _, pr := range payload.Items {
			if keep(pr.Repo) {
				kept = append(kept, pr)
			}
		}
		payload.Items = kept
		if len(payload.Items) == 0 {
			return "(none)\n"
		}
	}
	// Held red PRs ride ci-failing.json since hivecommons/hive#7438 so their
	// AUTHOR can repair them, but they are not this shared queue's business:
	// every policy says never touch a held item, and only the owning agent's
	// fix-before-new block carries the narrow exception. Keep them out here
	// so a repair agent does not push to a PR a human is reviewing.
	held := 0
	kept := payload.Items[:0]
	for _, pr := range payload.Items {
		if pr.Held {
			held++
			continue
		}
		kept = append(kept, pr)
	}
	payload.Items = kept
	// Two queues, not one (hivecommons/hive#7386): a red PR whose head lives
	// in a fork is comment-only for every agent — the App token pushes to the
	// base repository and nowhere else. Rendering the two together as one
	// "repair queue" sent a scanner through 107 PRs of which 66 were forks;
	// it found out by pushing, and the push landed a stray branch on the base
	// repo under the fork's head-ref name. The split keeps the push queue
	// honest about its size and takes the discovery cost off the agent.
	var pushable, forks []ciFailingRow
	for _, pr := range payload.Items {
		if pr.FromFork {
			forks = append(forks, pr)
		} else {
			pushable = append(pushable, pr)
		}
	}
	var b strings.Builder
	limit := s.prCap()
	if len(pushable) == 0 {
		b.WriteString("  (none you can push to)\n")
	}
	if held > 0 {
		b.WriteString(fmt.Sprintf("  (%d held red PR(s) are not listed: a held PR is repaired only by the agent that opened it, via its own FIX-BEFORE-NEW block)\n", held))
	}
	for i, pr := range pushable {
		if i >= limit {
			b.WriteString(prListOverflowLine(len(pushable)-i, limit))
			break
		}
		b.WriteString(fmt.Sprintf("  #%d %s by @%s (sha:%s)%s — %s\n", pr.Number, pr.Repo, pr.Author, pr.HeadSHA, heldMarker(pr.Held), pr.Title))
	}
	if len(forks) > 0 {
		b.WriteString(fmt.Sprintf("FORK PRs (%d — review/comment only, do not push unless an explicit contributor-PR gate says this hive may):\n", len(forks)))
		b.WriteString("  Their head branch lives in the contributor's fork. Default action is to report the\n")
		b.WriteString("  failing/held required check and the exact fix in one PR comment; do NOT create a\n")
		b.WriteString("  same-named branch on the base repo. Only an owner-enabled contributor_prs gate may\n")
		b.WriteString("  allow DCO-safe unstick moves such as merge-commit base syncs or approved reruns.\n")
		for i, pr := range forks {
			if i >= limit {
				b.WriteString(prListOverflowLine(len(forks)-i, limit))
				break
			}
			head := pr.HeadRepo
			if head == "" {
				head = "(fork deleted)"
			}
			if pr.HeadRef != "" {
				head += ":" + pr.HeadRef
			}
			checks := ""
			if len(pr.FailingChecks) > 0 {
				checks = " — failing: " + strings.Join(pr.FailingChecks, ", ")
			}
			excerpt := strings.TrimSpace(pr.Excerpt)
			if excerpt != "" {
				checks += " — " + excerpt
			}
			b.WriteString(fmt.Sprintf("  #%d %s by @%s [fork: %s — comment/check-report only]%s — %s%s\n", pr.Number, pr.Repo, pr.Author, head, heldMarker(pr.Held), pr.Title, checks))
		}
	}
	return b.String()
}
