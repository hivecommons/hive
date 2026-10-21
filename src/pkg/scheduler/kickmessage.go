package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/decisionpolicy"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/promptsrc"
	"github.com/hivecommons/hive/pkg/upstreamwatch"
)

func (s *Scheduler) formatIssueList(issues []github.Issue) string {
	out, _ := s.formatIssueListWithPolicy(issues)
	return out
}

func hasIssueLabel(labels []string, want string) bool {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), want) {
			return true
		}
	}
	return false
}

// upstreamPortDiffURL is the "port this" handoff (hivecommons/hive#9969): for
// an issue the upstream watch filed — one carrying the configured
// upstream-port label AND the hidden `<!-- upstream-ref: owner/repo#123 -->`
// marker RenderIssue embeds — it returns the upstream pull request's `.diff`
// URL so the fixer that picks the issue up starts with the patch in its kick
// context instead of hunting for it.
//
// It is derived, never trusted: the URL is rebuilt from a validated
// owner/repo#number marker, so an issue body cannot inject a link of its own.
// Empty for every other issue, for a release marker (no diff exists), and when
// no repo configures the label.
func (s *Scheduler) upstreamPortDiffURL(issue github.Issue) string {
	if s == nil || s.cfg == nil || len(s.cfg.UpstreamWatch.Repos) == 0 || issue.Body == "" {
		return ""
	}
	if !hasAnyIssueLabel(issue.Labels, s.upstreamPortLabels()) {
		return ""
	}
	ref, ok := upstreamwatch.ParseMarkerRef(issue.Body)
	if !ok {
		return ""
	}
	return upstreamwatch.MarkerDiffURL(ref)
}

// upstreamPortLabels is the set of labels the upstream watch files issues
// with across the configured repos (per-repo `label`, defaulting to
// upstream/port). A label is matched against this whole set rather than the
// issue's own repo entry because a kick list can span repos.
func (s *Scheduler) upstreamPortLabels() []string {
	labels := make([]string, 0, len(s.cfg.UpstreamWatch.Repos))
	for _, rc := range s.cfg.UpstreamWatch.Repos {
		label := strings.TrimSpace(rc.Label)
		if label == "" {
			label = config.DefaultUpstreamWatchLabel
		}
		labels = append(labels, label)
	}
	return labels
}

func hasAnyIssueLabel(labels []string, want []string) bool {
	for _, w := range want {
		if hasIssueLabel(labels, w) {
			return true
		}
	}
	return false
}

// issueFilterNotice renders the operator's project.issue_filter as prompt text,
// or "" when no filter is configured. The filter is ENFORCED upstream at
// enumeration (github.Client.fetchIssues) — filtered issues never reach any
// kick — so this notice is informational: it tells agents WHY the list may
// look smaller than the repo's open issues and not to go hunting for the rest.
// It is prepended to every issue list (${ISSUE_LIST} in kick templates and the
// hardcoded builders alike) so no agent is ever told to look at excluded
// issues.
func (s *Scheduler) issueFilterNotice() string {
	f := s.cfg.Project.IssueFilter
	// An empty allowlist means "no label filter"; other filter parts (reporter
	// trust, hard suppress) must not render a deny-all label block.
	if len(f.RequireLabels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("ISSUE FILTER (operator policy — already enforced; the issue list below reflects it):\n")
	b.WriteString(fmt.Sprintf("  Agents may ONLY work issues carrying at least one of these labels: %s\n",
		strings.Join(f.RequireLabels, ", ")))
	b.WriteString("  ⛔ Do NOT pick up, plan, or open PRs for issues outside this list, even if you find them by listing the repo yourself.\n")
	return b.String()
}

func (s *Scheduler) formatIssueListWithPolicy(issues []github.Issue) (string, bool) {
	notice := s.issueFilterNotice()
	if len(issues) == 0 {
		return notice + "(none)", false
	}
	var b strings.Builder
	b.WriteString(notice)
	b.WriteString(issuePriorityNote)
	failClosed := false
	// #9839: an issue with an open GitHub "blocked by" dependency is not
	// ready work. It is kept out of the list an agent picks from, and named
	// once at the end with its blockers so the deferral is visible and a
	// reader can check it; when the blocker closes it is simply back.
	ready, blocked := partitionBlockedIssues(issues)
	if len(ready) == 0 && len(blocked) > 0 {
		b.WriteString("(none ready)\n")
	}
	shown := fairShareByRepo(ready, s.issueCap(), func(issue github.Issue) string { return issue.Repo })
	for _, issue := range shown {
		// The issue title AND labels are untrusted external text about to be
		// injected into an agent kick, and labels additionally drive classification
		// routing (pkg/classify). Gate both through ioscan (F11): a blocked
		// title/label is redacted/annotated rather than injected raw, and the block
		// is recorded to the dashboard audit log. Disabled → strict no-op
		// passthrough. Default is now ON (fail-safe).
		title, verdict := s.enforceIssueTextVerdict(issue.Title)
		failClosed = failClosed || (s.ioscanFailClosed() && verdict.HasCriticalInjection())
		const maxTitleRunes = 60
		if runes := []rune(title); len(runes) > maxTitleRunes {
			title = string(runes[:maxTitleRunes])
		}
		labels, labelsFailClosed := s.enforceLabelsWithPolicy(issue.Labels)
		failClosed = failClosed || labelsFailClosed
		b.WriteString(fmt.Sprintf("  %dm %s %s [%s] %s\n",
			issue.AgeMinutes, issueDisplayRef(issue), issuePriorityMarker(issue),
			strings.Join(labels, ","), title))
		if diff := s.upstreamPortDiffURL(issue); diff != "" {
			b.WriteString(fmt.Sprintf("    ↳ upstream patch: %s — read it first, then port the change to this fork by hand; do not cherry-pick across repos\n", diff))
		}
		if issue.ClaimContext != nil && issue.ClaimContext.Decision == github.ClaimDecisionReopenedAfterMerge {
			prRef := fmt.Sprintf("%s#%d", issue.ClaimContext.PRRepo, issue.ClaimContext.PRNumber)
			b.WriteString(fmt.Sprintf("    ↳ reopened after %s merged", prRef))
			if issue.ClaimContext.PRURL != "" {
				b.WriteString(fmt.Sprintf(" (%s)", issue.ClaimContext.PRURL))
			}
			b.WriteString(": that fix did not resolve it — read the reporter's reopen comment, then implement what is still missing; do NOT re-verify, re-label likely-done, or close it\n")
		}
		if issue.ClaimContext != nil && issue.ClaimContext.MergedPR {
			reason := "weakly claimed"
			if issue.ClaimContext.Reference {
				reason = "referenced without a closing keyword"
			} else if issue.ClaimContext.ExternalAuthor {
				reason = "was claimed by an external author"
			}
			prRef := fmt.Sprintf("%s#%d", issue.ClaimContext.PRRepo, issue.ClaimContext.PRNumber)
			b.WriteString(fmt.Sprintf("    ↳ merged in %s", prRef))
			if issue.ClaimContext.PRURL != "" {
				b.WriteString(fmt.Sprintf(" (%s)", issue.ClaimContext.PRURL))
			}
			if hasIssueLabel(issue.Labels, github.VerifiedOpenLabel) {
				b.WriteString(fmt.Sprintf("; already verified (%s): remaining work is confirmed — do NOT re-verify, implement the rest", github.VerifiedOpenLabel))
			} else {
				b.WriteString(fmt.Sprintf("; verify once: close the issue if resolved, otherwise label it `%s` and implement the remainder (hive then stops asking); context: %s", github.VerifiedOpenLabel, reason))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString(formatBlockedIssuesNote(blocked))
	b.WriteString(s.formatFoldedTrackersNote(issues))
	return b.String(), failClosed
}

// partitionBlockedIssues splits the actionable issues into those ready to be
// offered and those with an unresolved "blocked by" dependency (#9839),
// preserving order within each.
func partitionBlockedIssues(issues []github.Issue) (ready, blocked []github.Issue) {
	for _, issue := range issues {
		if issue.IsBlocked() {
			blocked = append(blocked, issue)
		} else {
			ready = append(ready, issue)
		}
	}
	return ready, blocked
}

// maxBlockedIssuesNamed caps the blocked footer so a repo with a long
// dependency chain does not push the ready work out of the kick.
const maxBlockedIssuesNamed = 10

// formatBlockedIssuesNote renders the blocked issues as one footer line each,
// titles omitted on purpose — they are not an offer, only the reason an issue
// that exists on GitHub is missing from the list above.
func formatBlockedIssuesNote(blocked []github.Issue) string {
	if len(blocked) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\nBlocked by open dependencies (%d, not ready — do NOT start these; they return automatically when their blockers close):\n", len(blocked)))
	for i, issue := range blocked {
		if i >= maxBlockedIssuesNamed {
			b.WriteString(fmt.Sprintf("  … and %d more\n", len(blocked)-i))
			break
		}
		b.WriteString(fmt.Sprintf("  %s blocked by %s\n", issueDisplayRef(issue), strings.Join(issue.OpenBlockers(), ", ")))
	}
	return b.String()
}

func (s *Scheduler) formatPRList(actionable *github.ActionableResult) string {
	out, _ := s.formatPRListWithPolicy(actionable)
	return out
}

func (s *Scheduler) formatPRListWithPolicy(actionable *github.ActionableResult) (string, bool) {
	return s.formatPRListWithPolicyForAgent(actionable, "")
}

// buildAgentListAndRoles returns a comma-separated agent list and a formatted
// role table derived from the config, so templates stay correct when agents
// are added, removed, or renamed.
func (s *Scheduler) buildAgentListAndRoles() (list, roles string) {
	var names []string
	for name := range s.cfg.EnabledAgents() {
		names = append(names, name)
	}
	list = strings.Join(names, ", ")

	var b strings.Builder
	for name, agentCfg := range s.cfg.EnabledAgents() {
		displayName := agentCfg.DisplayName
		if displayName == "" {
			displayName = name
		}
		model := agentCfg.Model
		if model == "" {
			model = "default"
		}
		b.WriteString(fmt.Sprintf("  - %s (%s, %s)\n", displayName, name, model))
	}
	roles = b.String()
	return list, roles
}

type KickMessage struct {
	Agent     string
	Repo      string
	Message   string
	IssueRefs []string
}

func (s *Scheduler) BuildKickMessages(actionable *github.ActionableResult, agentsDue []string) []KickMessage {
	messages, _ := s.BuildKickMessagesWithSkips(actionable, agentsDue)
	return messages
}

// BuildKickMessagesWithSkips also explains due targets that produced no kick,
// so the governor does not log an intentionally idle lane as a delivery attempt.
func (s *Scheduler) BuildKickMessagesWithSkips(actionable *github.ActionableResult, agentsDue []string) ([]KickMessage, []string) {
	s.resetClassifierBudget()
	classifiedIssues := classify.ClassifyAll(actionable.Issues.Items)
	s.recordClassified(classifiedIssues)
	classifiedIssues = s.applyRunTriage(classifiedIssues)
	s.offerQuestions(classifiedIssues)

	var messages []KickMessage
	var skipped []string
	for _, targetKey := range agentsDue {
		agentName, repo := config.SplitCadenceTargetKey(targetKey)
		targetActionable := actionableForRepo(actionable, repo)
		targetIssues := classifiedIssues
		if repo != "" {
			targetIssues = filterIssuesByRepo(classifiedIssues, repo)
		}
		msg, idleReason := s.buildAgentMessageFor(agentName, repo, s.kickIncludesRepos(agentName), targetIssues, targetActionable)
		if idleReason != "" {
			skipped = append(skipped, targetKey+" ("+idleReason+")")
			// The reviewer lane is dormant or has nothing to adjudicate
			// (#11046): skip the kick rather than spend a model turn on a
			// stand-down. Nothing is dispatched, so no LastKick is recorded
			// and no stand-down outcome marks the lane blocked; the next
			// tick checks the gates again.
			if s.logger != nil {
				s.logger.Info("reviewer-lane kick skipped: lane idle", "agent", agentName, "target", targetKey, "reason", idleReason)
			}
			continue
		}
		if msg == "" {
			skipped = append(skipped, targetKey+" (kick message withheld)")
		} else {
			// The canary is the one thing that goes AHEAD of the static
			// prefix: it is random by construction and prepended to the kick
			// (ADR-0008, pkg/ioscan/canary.go), and both properties are what
			// make it an exfiltration detector. It is opt-in
			// (ioscan.canaries), so a hive without it starts the kick at the
			// cacheable prefix; a hive with it pays one short random line.
			msg = s.addCanaryPreamble(agentName, msg)
			s.logKickSize(agentName, repo, msg, targetIssues, targetActionable)
			messages = append(messages, KickMessage{
				Agent:     agentName,
				Repo:      repo,
				Message:   msg,
				IssueRefs: issueRefsForAgent(agentName, s.kickIssueRefs(targetIssues), s.issueCap()),
			})
		}
	}
	return messages, skipped
}

// selectReviewModel is the cadence-kick face of
// config.ReviewModelsConfig.Select; the review swarm calls the same function
// (review.PlanDispatch), so a pool, its exclusions and its fallback mean one
// thing however the review is dispatched. The third result is true when the
// configured fallback was used; a "skip" fallback returns an empty model.
func (s *Scheduler) selectReviewModel(agentCfg config.AgentConfig, pr github.PullRequest) (string, string, bool) {
	sel := agentCfg.ReviewModels.Select(pr.HiveModel, agentCfg.Backend, agentCfg.Model)
	if sel.Skip {
		return "", "", true
	}
	return sel.Backend, sel.Model, sel.Fallback
}

// BuildAgentMessageFromLastActionable builds a kick message for the named
// agent from the scheduler's cached actionable snapshot, classifying issues
// exactly like governor-driven kicks do. The dashboard's manual-kick path
// previously called BuildAgentMessage with a nil issue list, so every manual
// kick delivered an EMPTY work list — agents (whose policies forbid running
// gh issue list themselves) then correctly reported "nothing to do" no matter
// how deep the queue was.
func (s *Scheduler) BuildAgentMessageFromLastActionable(agentName string) string {
	s.resetClassifierBudget()
	actionable := s.GetLastActionable()
	var classified []github.Issue
	if actionable != nil {
		classified = classify.ClassifyAll(actionable.Issues.Items)
		s.recordClassified(classified)
		classified = s.applyRunTriage(classified)
	}
	return s.BuildAgentMessage(agentName, classified, actionable)
}

// buildReposSection is buildReposSectionFor with no agent: the hive-wide list.
func (s *Scheduler) buildReposSection() string { return s.buildReposSectionFor("") }

// maxIssuesPerKick / maxPRsPerKick are the DEFAULT list caps; the live values
// come from governor.kick_limits via issueCap/prCap (hivecommons/hive#7368).
// The PR cap is the one that was missing: a spoke with 302 open PRs delivered
// a 69.5 KiB kick — the PR list alone ~36 KiB, over half the prompt and 3x the
// budget documented in pkg/dashboard/prompt_history.go — and every extra
// kilobyte lengthens the terminal-typed delivery window that #7363 hangs on.
const (
	maxIssuesPerKick = config.DefaultMaxIssuesPerKick
	maxPRsPerKick    = config.DefaultMaxPRsPerKick
)

// issueCap is how many issues one kick list may carry (governor.kick_limits
// .max_issues, default maxIssuesPerKick). Nil-safe for bare test schedulers.
func (s *Scheduler) issueCap() int {
	if s == nil || s.cfg == nil {
		return maxIssuesPerKick
	}
	return s.cfg.Governor.KickLimits.IssuesPerKick()
}

// prCap is how many PRs one kick list may carry (governor.kick_limits.max_prs,
// default maxPRsPerKick). Applied to every PR list a kick renders: actionable
// PRs, stale drafts, merge-eligible, CI-failing.
func (s *Scheduler) prCap() int {
	if s == nil || s.cfg == nil {
		return maxPRsPerKick
	}
	return s.cfg.Governor.KickLimits.PRsPerKick()
}

// logKickSize emits one structured line per rendered kick with the prompt's
// byte size next to the list counts and caps that drove it (#12133). It is
// the only per-kick size telemetry: the audit line in pkg/agent carries
// message_len but not the counts, and prompt-history truncates at 32 KiB.
// Operators tune governor.kick_limits from this line, not from guesswork —
// see docs/operator-reference.md "Kick size and the list caps".
func (s *Scheduler) logKickSize(agentName, repo, msg string, issues []github.Issue, actionable *github.ActionableResult) {
	if s == nil || s.logger == nil {
		return
	}
	issueTotal, prTotal := len(issues), 0
	if actionable != nil {
		prTotal = len(s.prListItemsForAgent(actionable, s.cfg.BaseAgentName(agentName)))
	}
	s.logger.Info("kick rendered",
		"agent", agentName,
		"repo", repo,
		"kick_bytes", len(msg),
		"issues_total", issueTotal,
		"issues_listed", min(issueTotal, capOrAll(s.issueCap(), issueTotal)),
		"prs_total", prTotal,
		"prs_listed", min(prTotal, capOrAll(s.prCap(), prTotal)),
		"max_issues", s.issueCap(),
		"max_prs", s.prCap(),
	)
}

// capOrAll maps the kick_limits "0 = unlimited" convention onto a count.
func capOrAll(limit, total int) int {
	if limit <= 0 {
		return total
	}
	return limit
}

// prListOverflowLine is the explicit marker appended when a PR list was cut
// at the cap, so the agent knows the list is partial rather than complete —
// a silent slice would read as "these are all the PRs".
func prListOverflowLine(omitted, limit int) string {
	return fmt.Sprintf("  … and %d more open PRs not listed (cap %d per kick, shared evenly across repos; they return on later kicks as this list drains)\n", omitted, limit)
}

// BuildAgentMessage constructs a kick prompt for the named agent using the
// template resolution chain (config kick_template → convention → embedded → hardcoded).
func (s *Scheduler) BuildAgentMessage(agentName string, issues []github.Issue, actionable *github.ActionableResult) string {
	message, _ := s.buildAgentMessage(agentName, issues, actionable)
	return message
}

// buildAgentMessage is BuildAgentMessage plus the reviewer lane's idle reason
// (reviewerLaneGate). A non-empty idleReason marks the message as a reviewer
// stand-down: BuildKickMessages drops it, and a forced kick that does render
// it gets no closing instruction to go and work a repo (#11045).
func (s *Scheduler) buildAgentMessage(agentName string, issues []github.Issue, actionable *github.ActionableResult) (message, idleReason string) {
	return s.buildAgentMessageFor(agentName, "", false, issues, actionable)
}

// kickIncludesRepos reports whether the agent's kick carries the AUTHORIZED
// REPOS block (`include_repos`, default on; outreach defaults off).
func (s *Scheduler) kickIncludesRepos(agentName string) bool {
	if agentCfg, ok := s.cfg.Agents[agentName]; ok {
		return agentCfg.ShouldIncludeRepos()
	}
	if agentCfg, ok := s.cfg.Agents[s.cfg.BaseAgentName(agentName)]; ok {
		return agentCfg.ShouldIncludeRepos()
	}
	return s.cfg.BaseAgentName(agentName) != "outreach"
}

// buildAgentMessageFor resolves the kick body through the template chain and
// then hands it to assembleKick, which wraps EVERY resolution path — config
// templates, repo-sourced prompts, embedded defaults, hardcoded fallbacks,
// scheduled kicks, and manual kicks — in the same post-resolution seams. A
// template-only fix would leave stale operator overrides vulnerable
// indefinitely (kubestellar/hive#4744), so no seam may live in a policy file.
//
// repo is the cadence target of a repo-scoped kick ("" otherwise) and
// includeRepos whether the AUTHORIZED REPOS block is rendered; both are
// semi-static and are placed by assembleKick.
func (s *Scheduler) buildAgentMessageFor(agentName, repo string, includeRepos bool, issues []github.Issue, actionable *github.ActionableResult) (message, idleReason string) {
	defer func() {
		message = s.assembleKick(kickAssembly{
			agent:        agentName,
			repo:         repo,
			includeRepos: includeRepos,
			issues:       issues,
			actionable:   actionable,
			idleReason:   idleReason,
		}, message)
	}()

	baseName := s.cfg.BaseAgentName(agentName)
	// 0. GitHub-sourced prompt: if the agent declares a prompt_source, resolve it
	//    live at kick time (with allowlist gating + graceful fallback). A miss
	//    (unset, denied, unreachable with no cache) falls through to the inline
	//    template chain below, so a bad source never blanks or crashes a kick.
	if agentCfg, ok := s.cfg.Agents[baseName]; ok && agentCfg.PromptSource.IsSet() {
		if resolver := s.gitHubPromptResolver(); resolver != nil {
			src := promptsrc.Source{
				Owner: agentCfg.PromptSource.Owner,
				Repo:  agentCfg.PromptSource.Repo,
				Path:  agentCfg.PromptSource.Path,
				Ref:   agentCfg.PromptSource.Ref,
			}
			if res := resolver.Resolve(context.Background(), src); res.Ok && res.Body != "" {
				s.logger.Info("using GitHub-sourced kick prompt", "agent", agentName, "source", res.Source)
				body, failClosed := s.substituteTemplateWithPolicy(res.Body, actionable, agentName, issues)
				if failClosed {
					return "", ""
				}
				return fmt.Sprintf("[agent:%s]\n\n%s", agentName, body), ""
			}
		}
	}

	// 1. Config-driven: use kick_template field if set
	if agentCfg, ok := s.cfg.Agents[baseName]; ok && agentCfg.KickTemplate != "" {
		template, source, tried := s.resolveNamedTemplate(agentCfg.KickTemplate)
		if template != "" {
			s.logger.Info("using config kick_template", "agent", agentName, "template", agentCfg.KickTemplate, "source", source)
			body, failClosed := s.substituteTemplateWithPolicy(template, actionable, agentName, issues)
			if failClosed {
				return "", ""
			}
			return fmt.Sprintf("[agent:%s]\n\n%s", agentName, body), ""
		}
		// The configured template does not exist anywhere. Say so — with the
		// same weight the success path gets — naming what the kick falls back
		// to. Silence here is what let a dangling kick_template look like a
		// working one for the life of a spoke (hivecommons/hive#7390).
		s.logger.Warn("config kick_template not found; falling back",
			"agent", agentName, "template", agentCfg.KickTemplate,
			"fallback", s.describeTemplateFallback(baseName),
			"paths_tried", strings.Join(tried, ", "))
	}

	// 2. ACMM pack default: if acmm_level is set, use the pack's template for this agent
	if s.cfg.ACMMLevel != nil && *s.cfg.ACMMLevel > 0 {
		if pack, err := config.ACMMPackByLevel(*s.cfg.ACMMLevel); err == nil {
			for _, pa := range pack.Agents {
				if pa.Name == baseName && pa.KickTemplate != "" {
					if template := s.loadNamedTemplate(pa.KickTemplate); template != "" {
						s.logger.Info("using ACMM pack template", "agent", agentName, "level", *s.cfg.ACMMLevel, "template", pa.KickTemplate)
						body, failClosed := s.substituteTemplateWithPolicy(template, actionable, agentName, issues)
						if failClosed {
							return "", ""
						}
						return fmt.Sprintf("[agent:%s]\n\n%s", agentName, body), ""
					}
				}
			}
		}
	}

	// 3. Convention: look for <agent>.md template file
	if template := s.loadPromptTemplate(baseName); template != "" {
		s.logger.Info("using prompt template for kick", "agent", agentName)
		body, failClosed := s.substituteTemplateWithPolicy(template, actionable, agentName, issues)
		if failClosed {
			return "", ""
		}
		return fmt.Sprintf("[agent:%s]\n\n%s", agentName, body), ""
	}

	// 3. Legacy hardcoded fallback (removed in Phase 4 when all agents use templates)
	s.logger.Info("no prompt template found, using hardcoded kick", "agent", agentName)
	// Role-based routing (#5480): an operator-added agent with `role: reviewer`
	// gets the escalated-PR adjudication kick regardless of its name. Checked
	// before the name switch because the lane is enabled by ROLE — the agent
	// may be named anything. The pack-defined queue "reviewer" never reaches
	// this fallback because its kick_template resolves above, which is why the
	// L5/L6 packs ship the lane as a separate template-less "adjudicator"
	// agent (hivecommons/hive#9477). A kick_template on a reviewer-role
	// agent shadows the lane.
	if s.agentRole(agentName) == RoleReviewer {
		return s.buildReviewerLaneMessage(agentName, actionable)
	}
	switch baseName {
	case "scanner":
		return s.buildScannerMessage(issues, actionable), ""
	case "ci-maintainer":
		return s.buildCIMaintainerMessage(actionable), ""
	case "supervisor":
		return s.buildSupervisorMessage(actionable), ""
	case "quality":
		return s.buildQualityMessage(issues, actionable), ""
	case "architect":
		return s.buildArchitectMessage(issues, actionable), ""
	case "outreach":
		return s.buildOutreachMessage(actionable), ""
	case "sec-check":
		return s.buildSecCheckMessage(actionable), ""
	default:
		return s.buildGenericMessage(agentName, issues, actionable), ""
	}
}

const governorWorkSourceRule = "The PRs/issues for this kick are listed in this prompt by the governor. Do not run `gh pr list`, `gh issue list`, or `gh search` to discover work — these are blocked by the hive proxy for agents and only waste a turn. Use `gh pr view <n>` on supplied PR numbers and `gh issue view <n>` only on supplied GitHub issue numbers. Prefer the compact form `hive-gh pr view <n>` / `hive-gh issue view <n>` / `hive-gh pr checks <n>` (fixed --json projection that keeps labels, author, head SHA, merge state, review decision and check conclusions); add `--full` only when you need the complete record."

func addGovernorWorkSourceRule(message string) string {
	if message == "" || strings.Contains(message, governorWorkSourceRule) {
		return message
	}
	return insertAfterKickHeader(message, governorWorkSourceRule)
}

// inProgressClaimRule tells the agent to mark the issue it is about to
// implement BEFORE it starts. The `claim` request is the hive's start signal:
// it applies `hive/in-progress` (plus `hive/claimed-by-<agent>`) and posts a
// timestamped marker, so a maintainer, another hive scanning the same repo and
// the SLA metrics all learn the instant work began rather than when the PR
// appeared. Re-running it is a harmless no-op; a PR naming the issue clears it.
const inProgressClaimRule = "Before you start implementing a fix for an issue from your list — before the worktree, before the first edit — run `hive-open-issue claim --repo <owner/repo> <issue-number>` for that issue. It labels the issue `hive/in-progress` so the hive, the maintainers and other hives can see work has started and when. Claim one issue at a time, only the one you are about to work; do not claim the whole list. Issues already labelled `hive/in-progress` or `claimed` are someone else's: leave them alone. The claim is the ONLY start signal besides opening a PR: never claim an issue you are only triaging, verifying, gate-checking or reporting on, and never claim an epic or tracking issue (label `epic`) — comment your finding and move on. A comment that only reports starts nothing; a comment that begins work says so (open with \"Starting …\" / \"Implementing …\" or include `<!-- hive:start -->`)."

// addInProgressClaimRule appends inProgressClaimRule to a kick that lists at
// least one issue and does not already carry it.
func addInProgressClaimRule(message string, issues []github.Issue) string {
	if message == "" || len(issues) == 0 || strings.Contains(message, "hive-open-issue claim --repo") {
		return message
	}
	return strings.TrimRight(message, "\n") + "\n\n" + inProgressClaimRule + "\n"
}

const concreteKickClosingInstruction = "Begin now: pick the authorized repo you covered least recently and do your role's work there this session, filing issues/PRs through the hive relays as documented above. Do not ask for clarification — no human is attached to this session; if something is ambiguous, make the conservative choice and note it in your output."

func addConcreteKickClosingInstruction(message string) string {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return message
	}
	if strings.Contains(trimmed, "Do not ask for clarification — no human is attached") ||
		strings.Contains(trimmed, "Begin now:") {
		return message
	}
	return trimmed + "\n\n" + concreteKickClosingInstruction
}

func (s *Scheduler) reposSection() string {
	var b strings.Builder
	host := s.cfg.GitHub.ResolvedBaseURL()
	b.WriteString(fmt.Sprintf("## Project Repositories\n\nYour role covers these repositories, all on **%s** (this hive is single-host):\n", host))
	for _, repo := range s.cfg.Project.Repos {
		full := repo
		if !strings.Contains(repo, "/") {
			full = s.cfg.Project.Org + "/" + repo
		}
		b.WriteString(fmt.Sprintf("  %s/%s\n", strings.TrimRight(host, "/"), full))
	}
	b.WriteString(fmt.Sprintf("\nAll work should be scoped to these repos on %s.\n\n", host))
	return b.String()
}

const issuePriorityNote = "Issue priority: human-filed and priority-labelled issues are listed first; work them before hive-filed.\n"

func issuePriorityMarker(issue github.Issue) string {
	if issue.AuthorIsHuman {
		return "[human]"
	}
	if issue.HumanAcknowledged {
		if parent, ok := strings.CutPrefix(issue.AckSource, "parent "); ok {
			// "[hive-filed+parent-ack #9802]": the relay split this child
			// out of an approved parent (#9840); name it so the ranking is
			// auditable from the kick line alone.
			return "[hive-filed+parent-ack " + parent + "]"
		}
		return "[hive-filed+ack]"
	}
	return "[hive-filed]"
}

func (s *Scheduler) formatPRListWithPolicyForAgent(actionable *github.ActionableResult, agentName string) (string, bool) {
	prs := s.prListItemsForAgent(actionable, agentName)
	if len(prs) == 0 {
		return "(none)", false
	}
	var b strings.Builder
	failClosed := false
	limit := s.prCap()
	shown := fairShareByRepo(prs, limit, func(pr github.PullRequest) string { return pr.Repo })
	verdicts := s.loadReviewVerdicts()
	links := loadReviewLinks()
	for _, pr := range shown {
		// The PR title and author login are untrusted external text about to be
		// injected into an agent kick (F11). PR titles in particular drive
		// classification routing, and an attacker controls both the title and their
		// own fork/login. Gate both through ioscan: a blocked value is redacted
		// rather than injected raw. Disabled → strict no-op. Default is now ON.
		title, titleVerdict := s.enforceIssueTextVerdict(pr.Title)
		failClosed = failClosed || (s.ioscanFailClosed() && titleVerdict.HasCriticalInjection())
		const maxPRTitleRunes = 70
		if runes := []rune(title); len(runes) > maxPRTitleRunes {
			title = string(runes[:maxPRTitleRunes])
		}
		author, authorVerdict := s.enforceIssueTextVerdict(pr.Author)
		failClosed = failClosed || (s.ioscanFailClosed() && authorVerdict.HasCriticalInjection())
		annotationText, skip := s.prReviewAnnotation(pr, agentName)
		if skip {
			continue
		}
		annotationText = prKickAnnotation(pr, agentName) + reviewedAnnotation(verdicts, links, pr, s.cfg.Project.Org) + annotationText
		annotation, annotationVerdict := s.enforceIssueTextVerdict(annotationText)
		failClosed = failClosed || (s.ioscanFailClosed() && annotationVerdict.HasCriticalInjection())
		b.WriteString(fmt.Sprintf("  %s#%d by @%s%s %s %s\n", pr.Repo, pr.Number, author, forkAnnotation(pr), annotation, title))
	}
	if b.Len() == 0 {
		return "(none)", failClosed
	}
	if omitted := len(prs) - len(shown); omitted > 0 {
		b.WriteString(prListOverflowLine(omitted, limit))
	}
	return b.String(), failClosed
}

func (s *Scheduler) prListItemsForAgent(actionable *github.ActionableResult, agentName string) []github.PullRequest {
	if actionable == nil {
		return nil
	}
	if s == nil || s.cfg == nil || s.cfg.BaseAgentName(agentName) != "reviewer" {
		return actionable.PRs.Items
	}
	prs := make([]github.PullRequest, 0, len(actionable.PRs.Items)+len(actionable.PRs.Held))
	seen := map[string]bool{}
	add := func(items []github.PullRequest) {
		for _, pr := range items {
			fullRepo := config.QualifyRepo(s.cfg.Project.Org, pr.Repo)
			key := github.ReviewQueueKey(fullRepo, pr.Number)
			if fullRepo == "" || pr.Number <= 0 || seen[key] {
				continue
			}
			seen[key] = true
			prs = append(prs, pr)
		}
	}
	add(actionable.PRs.Items)
	add(actionable.PRs.Held)
	sort.SliceStable(prs, func(i, j int) bool {
		a, b := prs[i], prs[j]
		if a.ReviewRank > 0 && b.ReviewRank > 0 && a.ReviewRank != b.ReviewRank {
			return a.ReviewRank < b.ReviewRank
		}
		if a.ReviewRank > 0 && b.ReviewRank <= 0 {
			return true
		}
		if a.ReviewRank <= 0 && b.ReviewRank > 0 {
			return false
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.IsZero() {
				return false
			}
			if b.CreatedAt.IsZero() {
				return true
			}
			return a.CreatedAt.Before(b.CreatedAt)
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Number < b.Number
	})
	return prs
}

func (s *Scheduler) buildReposSectionFor(agentName string) string {
	var b strings.Builder
	host := s.cfg.GitHub.ResolvedBaseURL() // always a full URL; github.com or the GHE instance
	org := s.cfg.Project.Org
	scoped := s.cfg.AgentRepoScope(agentName) != nil
	active, paused := s.activeReposForAgent(agentName)
	agentRepos := s.cfg.ReposForAgent(agentName)
	b.WriteString(fmt.Sprintf("AUTHORIZED REPOS (all on %s — you may ONLY interact with these):\n", host))
	for _, repo := range active {
		full := config.QualifyRepo(org, repo)
		// Print the fully-qualified URL so the host is unambiguous in the prompt —
		// a github.ibm.com repo must never be mistaken for a github.com one.
		b.WriteString(fmt.Sprintf("  %s/%s\n", strings.TrimRight(host, "/"), full))
	}
	if len(active) == 0 {
		if scoped {
			b.WriteString("  (none — this agent is scoped to repos this hive does not watch, or every repo it serves is currently paused; tell the operator)\n")
		} else {
			b.WriteString("  (none — every repo this hive watches is currently paused)\n")
		}
	}
	if scoped {
		b.WriteString(fmt.Sprintf("🎯 THIS AGENT IS REPO-SCOPED: the hive manages %d repo(s); you are defined for the %d listed above and only those. Other repos in this hive belong to other agents — writes to them are refused deterministically by the proxy and by the hive-open-pr/hive-merge/hive-open-issue relays, so retrying cannot succeed. This is how the operator composed the roster, NOT scope loss and NOT an outage: do not work them, do not route around it, and do not file an issue about it.\n",
			len(s.cfg.Project.Repos), len(agentRepos)))
	}
	if len(paused) > 0 {
		pausedFull := make([]string, 0, len(paused))
		for _, repo := range paused {
			pausedFull = append(pausedFull, config.QualifyRepo(org, repo))
		}
		b.WriteString(fmt.Sprintf("⏸️ PAUSED BY THE OPERATOR (watched, but OUT OF SCOPE this session): %s\n", strings.Join(pausedFull, ", ")))
		b.WriteString("   The hive is deliberately quiet on those repos — a release freeze, an incident, a repo declared but not yet onboarded. Writes to them are refused deterministically by the proxy and by the hive-open-pr/hive-merge relays, so retrying cannot succeed. This is an operator decision, NOT an outage and NOT scope loss: do not work them, do not route around it, and do not file an issue about it.\n")
	}
	b.WriteString("⛔ NEVER access, search, list, file issues in, or open PRs on repos not listed above.\n")
	b.WriteString(fmt.Sprintf("⛔ Every repo above is on %s. This hive is single-host — never touch a repo on a different GitHub host.\n", host))
	// SCOPE vs PROVISIONING (#4464). This list is what `include_repos: true`
	// puts in a kick, and agents have read it as a promise that the repos are
	// on disk: a guide agent found its workspace directory empty, concluded
	// "no git worktree has been provisioned despite include_repos=true", and
	// filed it as an infrastructure blocker that then sat in the operator's
	// advisory digest. There is no such provisioning step — nothing in the
	// hive materialises a per-agent worktree from this list — so the kick has
	// to say so, in the same section that produces the impression. Getting a
	// checkout is an ordinary thing an agent does for itself, not a fault.
	b.WriteString(fmt.Sprintf("ℹ️ This list is an AUTHORIZATION SCOPE, not a checkout: it does not put any repo on disk, and nothing provisions a per-agent git worktree from it. If you need files rather than the GitHub API and have no checkout, clone one yourself: git clone %s/<org>/<repo> /tmp/<repo>. An absent checkout is a normal state to handle, NOT an infrastructure fault — do not file a finding about a missing worktree or unprovisioned repo workspace.\n", strings.TrimRight(host, "/")))
	// Repo-local gates and companion checkouts (hivecommons/hive#13059): a
	// target repo's AGENTS.md may say "offer a spec and wait" or "stop when a
	// registered sibling checkout is missing". Dispatched work is already
	// accepted, and a registered sibling path under the workspace root is a
	// repo this installation can clone — neither is a reason to park.
	b.WriteString("ℹ️ " + decisionpolicy.DispatchSatisfiesRepoGates + " " + decisionpolicy.MissingCompanionCheckoutRule + "\n")
	for _, repo := range active {
		full := config.QualifyRepo(org, repo)
		plan := config.PlanCompanionCheckouts("$HOME/workspace", full, s.cfg.CompanionReposFor(full))
		if instr := config.CompanionCheckoutInstruction(full, host, plan); instr != "" {
			b.WriteString("ℹ️ " + instr + " (clone the primary to $HOME/workspace/" + full + " so the registered paths resolve.)\n")
		}
	}
	// Multi-repo projects: the agent workdir is never a checkout of anything
	// but the PRIMARY repo, and the shipped templates' examples say --repo
	// "$HIVE_REPO" (primary). Without an explicit rotation instruction agents lock onto
	// the primary repo forever and the other project repos are never
	// touched (root-caused on a live 3-repo hive: sec-check scanned only
	// the primary across every session). The kick is the one place every
	// agent/template combination sees, so the instruction lives here.
	if len(active) > 1 {
		// Rotate over the repos this agent both serves and may act on, and never
		// name one it cannot write to as the fallback: telling an agent "all of
		// them are in scope, not just the primary" while pointing it at a repo
		// that is paused or outside its scope is a contradiction it will try to
		// resolve by writing there (#6203/#6204).
		primary := s.cfg.PrimaryRepoForAgent(agentName)
		if primary == "" || s.cfg.IsRepoPaused(primary) {
			primary = active[0]
		}
		b.WriteString(fmt.Sprintf(`🔁 MULTI-REPO COVERAGE — REQUIRED: this project has %d authorized repos; ALL of them are in scope, not just the primary (%s).
Your workdir is, at most, a checkout of the primary repo — never of the others. Each session, pick the authorized repo you have LEAST RECENTLY covered (check your beads and the [<your-role>] issues you previously filed in each repo) and work THAT repo this session:
  - If it is not your workdir repo, clone it first: git clone %s/<org>/<repo> /tmp/<repo> && cd /tmp/<repo>
  - Pass the chosen repo EXPLICITLY to every gh command: --repo "<org>/<repo>" (do not rely on $HIVE_REPO, which always names the primary repo).
  - $HIVE_REPOS lists every authorized repo, comma-separated.
⛔ Do NOT default to the primary repo every session — repos you never visit accumulate unseen problems.
`, len(active), config.QualifyRepo(org, primary), strings.TrimRight(host, "/")))
	}
	return b.String()
}
