package scheduler

import (
	"context"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
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
	if f.IsZero() {
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
	return s.formatIssueListWithPolicyForAgent(issues, false)
}

func (s *Scheduler) formatIssueListWithPolicyForAgent(issues []github.Issue, refsOnly bool) (string, bool) {
	notice := s.issueFilterNotice()
	if len(issues) == 0 {
		return notice + "(none)", false
	}
	var b strings.Builder
	b.WriteString(notice)
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
	if refsOnly {
		for _, issue := range shown {
			_, verdict := s.enforceIssueTextVerdict(issue.Title)
			failClosed = failClosed || (s.ioscanFailClosed() && verdict.HasCriticalInjection())
			_, labelsFailClosed := s.enforceLabelsWithPolicy(issue.Labels)
			failClosed = failClosed || labelsFailClosed
			b.WriteString(fmt.Sprintf("  %s\n", issueDisplayRef(issue)))
		}
		return b.String(), failClosed
	}
	b.WriteString(issuePriorityNote)
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
	return s.formatPRListWithPolicyForAgent(actionable, "", false)
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
	s.resetClassifierBudget()
	sweepCtx, cancelSweep := classify.SweepContext(context.Background())
	classifiedIssues := classify.ClassifyAll(sweepCtx, actionable.Issues.Items)
	s.recordClassified(classifiedIssues)
	classifiedIssues = s.applyRunTriage(sweepCtx, classifiedIssues)
	cancelSweep()
	s.offerQuestions(classifiedIssues)

	var messages []KickMessage
	for _, targetKey := range agentsDue {
		agentName, repo := config.SplitCadenceTargetKey(targetKey)
		targetActionable := actionableForRepo(actionable, repo)
		targetIssues := classifiedIssues
		if repo != "" {
			targetIssues = filterIssuesByRepo(classifiedIssues, repo)
		}
		elideStuffed := s.dropStuffedContext(agentName)
		msg := s.buildAgentMessage(agentName, targetIssues, targetActionable, elideStuffed)
		if msg != "" {
			msg = s.finalizeKickMessage(agentName, repo, msg)
			if elideStuffed && s.logger != nil {
				s.logger.Info("task MCP stuffed context elided", "agent", agentName, "elided_bytes", len(msg))
			}
			messages = append(messages, KickMessage{
				Agent:     agentName,
				Repo:      repo,
				Message:   msg,
				IssueRefs: issueRefsForAgent(agentName, s.freeOfInflight(targetIssues), s.issueCap()),
			})
		}
	}
	return messages
}

func (s *Scheduler) finalizeKickMessage(agentName, repo, msg string) string {
	includeRepos := true
	if agentCfg, ok := s.cfg.Agents[agentName]; ok {
		includeRepos = agentCfg.ShouldIncludeRepos()
	} else if agentCfg, ok := s.cfg.Agents[s.cfg.BaseAgentName(agentName)]; ok {
		includeRepos = agentCfg.ShouldIncludeRepos()
	} else if s.cfg.BaseAgentName(agentName) == "outreach" {
		includeRepos = false
	}
	msg = s.addIndependentReviewSection(agentName, msg)
	if repo != "" {
		msg = fmt.Sprintf("REPO-SCOPED CADENCE TARGET: %s\n\n%s", repo, msg)
	}
	if includeRepos {
		// Built per agent, not once for the fleet: a repo-scoped agent
		// must be told its own AUTHORIZED REPOS, or the one section
		// every agent sees would name repos it cannot write to (#6204).
		msg += "\n" + s.buildReposSectionFor(agentName)
	}
	return s.addCanaryPreamble(agentName, msg)
}

func (s *Scheduler) selectReviewModel(agentCfg config.AgentConfig, pr github.PullRequest) (string, string, bool) {
	authorModel := github.NormalizeAttributionModel(pr.HiveModel)
	authorFamily := github.ModelFamily(authorModel)
	for _, entry := range agentCfg.ReviewModels.Pool {
		candidate := github.NormalizeAttributionModel(entry.Model)
		if agentCfg.ReviewModels.ExcludeAuthorModelEnabled() && candidate == authorModel {
			continue
		}
		if agentCfg.ReviewModels.ExcludeAuthorFamily && github.ModelFamily(candidate) == authorFamily {
			continue
		}
		return strings.TrimSpace(entry.Backend), strings.TrimSpace(entry.Model), false
	}
	switch agentCfg.ReviewModels.EffectiveFallback() {
	case config.ReviewModelsFallbackSkip:
		return "", "", true
	default:
		return strings.TrimSpace(agentCfg.Backend), strings.TrimSpace(agentCfg.Model), true
	}
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
		sweepCtx, cancelSweep := classify.SweepContext(context.Background())
		classified = classify.ClassifyAll(sweepCtx, actionable.Issues.Items)
		s.recordClassified(classified)
		classified = s.applyRunTriage(sweepCtx, classified)
		cancelSweep()
	}
	return s.addIndependentReviewSection(agentName, s.BuildAgentMessage(agentName, classified, actionable))
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

// prListOverflowLine is the explicit marker appended when a PR list was cut
// at the cap, so the agent knows the list is partial rather than complete —
// a silent slice would read as "these are all the PRs".
func prListOverflowLine(omitted, limit int) string {
	return fmt.Sprintf("  … and %d more open PRs not listed (cap %d per kick, shared evenly across repos; they return on later kicks as this list drains)\n", omitted, limit)
}

// BuildAgentMessage constructs a kick prompt for the named agent using the
// template resolution chain (config kick_template → convention → embedded → hardcoded).
func (s *Scheduler) BuildAgentMessage(agentName string, issues []github.Issue, actionable *github.ActionableResult) (message string) {
	return s.buildAgentMessage(agentName, issues, actionable, s.dropStuffedContext(agentName))
}

func (s *Scheduler) buildAgentMessage(agentName string, issues []github.Issue, actionable *github.ActionableResult, elideStuffed bool) (message string) {
	// Hold-gated PRs are deliberately absent from actionable.PRs: fetchPRs moves
	// them into actionable.Hold as soon as it sees the hold label. Wrap every
	// resolution path here so config templates, repo-sourced prompts, embedded
	// defaults, hardcoded fallbacks, scheduled kicks, and manual kicks all see
	// the same occupied-ground preflight. A template-only fix would leave stale
	// operator overrides vulnerable indefinitely (kubestellar/hive#4744).
	defer func() {
		message = s.addHeldPRCoordination(agentName, actionable, message)
		message = s.addGuideFlow(agentName, actionable, message)
		// Formal verification is an operator-enabled quality capability, not a
		// property of one particular prompt file. Inject its contract after
		// template resolution so local edits, remote prompts, replicas, scheduled
		// kicks, and manual kicks cannot accidentally omit it.
		message = s.addFormalQualityCapability(agentName, message)
		// Fix-before-new: an agent with red PRs of its own must see them —
		// with the CI evidence — ahead of any new work. Injected at the same
		// post-resolution seam as the held-PR preflight so no template path
		// can omit it (kubestellar/hive#4744): observed live on
		// kubestellar/console (2026-08-26), ten red split-PRs each had exactly
		// one commit — kicks kept spawning NEW PRs while the reaper's
		// re-engagements aged every red SHA to its cap unfixed.
		message = s.addRedPRFixFirst(agentName, message)
		// Same shape of problem, same seam (hivecommons/hive#7360): a PR this
		// agent opened is stuck behind unresolved external review-bot threads
		// and needs a push + in-thread replies before any new work.
		message = s.addReviewThreadFixFirst(agentName, message)
		// PR follow-up handoff (hivecommons/hive#9583, default off): the
		// reasoning behind the agent's own PRs with open follow-ups, for the
		// fresh session this kick starts.
		message = s.addPRFollowUpHandoff(agentName, message)
		// Non-GitHub work source: tell the agent how the tracker half of its
		// policy maps onto Linear (identity, auth, filing, PR linking, hold).
		// Same seam, same reason — a customized template cannot omit it.
		message = s.addWorkTrackerSection(message)
		// Items a live session already holds were dropped from the list
		// above; say so at the same seam so a customized template cannot
		// leave the agent wondering where its delegated issue went.
		message = s.addInflightNote(message, issues)
		// The workflow-push ceiling is a property of the agent's MODE, not of
		// its policy text, and no policy path can state it correctly for every
		// deployment — so it is stated here, at the same seam, for the one mode
		// that has it (#6681).
		message = s.addWorkflowPushCeiling(agentName, message)
		message = s.addTaskMCPPointer(message, elideStuffed)
		message = s.addQuestionAnswerContract(agentName, message, issues)
		message = addConcreteKickClosingInstruction(message)
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
				body, failClosed := s.substituteTemplateWithPolicy(res.Body, actionable, agentName, issues, elideStuffed)
				if failClosed {
					return ""
				}
				return fmt.Sprintf("[agent:%s]\n\n%s", agentName, body)
			}
		}
	}

	// 1. Config-driven: use kick_template field if set
	if agentCfg, ok := s.cfg.Agents[baseName]; ok && agentCfg.KickTemplate != "" {
		template, source, tried := s.resolveNamedTemplate(agentCfg.KickTemplate)
		if template != "" {
			s.logger.Info("using config kick_template", "agent", agentName, "template", agentCfg.KickTemplate, "source", source)
			body, failClosed := s.substituteTemplateWithPolicy(template, actionable, agentName, issues, elideStuffed)
			if failClosed {
				return ""
			}
			return fmt.Sprintf("[agent:%s]\n\n%s", agentName, body)
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
						body, failClosed := s.substituteTemplateWithPolicy(template, actionable, agentName, issues, elideStuffed)
						if failClosed {
							return ""
						}
						return fmt.Sprintf("[agent:%s]\n\n%s", agentName, body)
					}
				}
			}
		}
	}

	// 3. Convention: look for <agent>.md template file
	if template := s.loadPromptTemplate(baseName); template != "" {
		s.logger.Info("using prompt template for kick", "agent", agentName)
		body, failClosed := s.substituteTemplateWithPolicy(template, actionable, agentName, issues, elideStuffed)
		if failClosed {
			return ""
		}
		return fmt.Sprintf("[agent:%s]\n\n%s", agentName, body)
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
		return s.buildReviewerMessage(agentName, actionable)
	}
	switch baseName {
	case "scanner":
		return s.buildScannerMessage(issues, actionable, elideStuffed)
	case "ci-maintainer":
		return s.buildCIMaintainerMessage(actionable)
	case "supervisor":
		return s.buildSupervisorMessage(actionable)
	case "quality":
		return s.buildQualityMessage(issues, actionable, elideStuffed)
	case "architect":
		return s.buildArchitectMessage(issues, actionable, elideStuffed)
	case "outreach":
		return s.buildOutreachMessage(actionable)
	case "sec-check":
		return s.buildSecCheckMessage(actionable)
	default:
		return s.buildGenericMessage(agentName, issues, actionable, elideStuffed)
	}
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

func (s *Scheduler) dropStuffedContext(agentName string) bool {
	if !s.hasTaskMCPURL() {
		return false
	}
	if agentCfg, ok := s.cfg.Agents[agentName]; ok {
		return agentCfg.TaskMCP != nil && agentCfg.TaskMCP.DropStuffedContext
	}
	if agentCfg, ok := s.cfg.Agents[s.cfg.BaseAgentName(agentName)]; ok {
		return agentCfg.TaskMCP != nil && agentCfg.TaskMCP.DropStuffedContext
	}
	return false
}

func (s *Scheduler) addTaskMCPPointer(message string, elideStuffed bool) string {
	if message == "" || !s.hasTaskMCPURL() {
		return message
	}
	section := "## Task context MCP\n\nThe `hive-task` MCP server is connected for this launch. Call `context_bundle` first for the assigned task, related work, and CI summary instead of re-reading the issue/PR and CI from scratch.\n\n"
	if elideStuffed {
		section = "## Task context MCP\n\nThe `hive-task` MCP server is connected for this launch. Call `context_bundle` first for the assigned task, related work, and CI summary instead of re-reading the issue/PR and CI from scratch.\n\nStuffed work lists in this prompt were elided to `repo#N` refs only; `context_bundle` and `related_work` are the source of truth for titles, labels, age, annotations, and related context.\n\n"
	}
	if newline := strings.IndexByte(message, '\n'); newline >= 0 {
		return message[:newline+1] + "\n" + section + message[newline+1:]
	}
	return section + message
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

func (s *Scheduler) formatPRListWithPolicyForAgent(actionable *github.ActionableResult, agentName string, refsOnly ...bool) (string, bool) {
	if len(actionable.PRs.Items) == 0 {
		return "(none)", false
	}
	var b strings.Builder
	failClosed := false
	limit := s.prCap()
	shown := fairShareByRepo(actionable.PRs.Items, limit, func(pr github.PullRequest) string { return pr.Repo })
	verdicts := s.loadReviewVerdicts()
	links := loadReviewLinks()
	if len(refsOnly) > 0 && refsOnly[0] {
		for _, pr := range shown {
			_, titleVerdict := s.enforceIssueTextVerdict(pr.Title)
			failClosed = failClosed || (s.ioscanFailClosed() && titleVerdict.HasCriticalInjection())
			_, authorVerdict := s.enforceIssueTextVerdict(pr.Author)
			failClosed = failClosed || (s.ioscanFailClosed() && authorVerdict.HasCriticalInjection())
			annotationText, skip := s.prReviewAnnotation(pr, agentName)
			if skip {
				continue
			}
			annotationText = prKickAnnotation(pr, agentName) + reviewedAnnotation(verdicts, links, pr, s.cfg.Project.Org) + annotationText
			annotation, annotationVerdict := s.enforceIssueTextVerdict(annotationText)
			failClosed = failClosed || (s.ioscanFailClosed() && annotationVerdict.HasCriticalInjection())
			b.WriteString(fmt.Sprintf("  %s#%d%s %s\n", pr.Repo, pr.Number, forkAnnotation(pr), annotation))
		}
		if b.Len() == 0 {
			return "(none)", failClosed
		}
		return b.String(), failClosed
	}
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
	if omitted := len(actionable.PRs.Items) - len(shown); omitted > 0 {
		b.WriteString(prListOverflowLine(omitted, limit))
	}
	return b.String(), failClosed
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
