package scheduler

import (
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

// Role guidance for a contributor who takes on an agent role.
//
// A role-delegated contributor task (and donated standby work) used to carry
// the resident agent's whole manual kick: its work list, the merge-eligible
// and CI-failing lists, the merge-sweep and sub-agent dispatch steps, the
// App-token instructions, the red-main notice and "Begin now: pick the
// authorized repo …". A contributor's model followed that instead of its one
// assigned issue. Building that kick also ran a full classification sweep
// over the cached queue on every assignment.
//
// BuildAgentGuidanceForTask renders the same templates through the same
// chain, but keeps only how the role works: the policy text, house style, lane
// rules, the writing guide, and through ${KNOWLEDGE} the assigned task's
// "# Relevant Knowledge" and its repository's own instructions (AGENTS.md and
// skills).

// roleGuidance marks a guidance render (substituteTemplate) and carries the
// one task it is rendered for.
type roleGuidance struct {
	task github.Issue
}

// instructionsRepo is the repository whose AGENTS.md and skills the render
// injects: the assigned task's, or primary when the task names none (or for a
// resident kick, where g is nil).
func (g *roleGuidance) instructionsRepo(primary string) string {
	if g == nil {
		return primary
	}
	if repo := strings.TrimSpace(g.task.Repo); repo != "" {
		return repo
	}
	return primary
}

// residentWorkSections are the H2 sections of a kick template that run the
// RESIDENT agent's session — what to do first, how to sweep merges, dispatch
// sub-agents, repair the CI-failing list, pick the next repo — rather than
// describe how the role works. A contributor's session is one assigned task,
// so a guidance render leaves them out. A heading matches when it is the name
// itself or the name followed by a qualifier (" — …", " (…)", ": …").
var residentWorkSections = []string{
	"Priority Order",
	"Workflow",
	"Merge Sweep",
	"Dispatching Fixes",
	"CI Repair",
	"File-Overlap Detection",
	"Escalated PRs",
	"Multi-Repo Coverage",
}

// workListVars are the template variables that carry the resident agent's
// work. A line holding only one of them is dropped from a guidance render,
// together with the label line directly above it ("ACTIONABLE ISSUES:",
// "MERGE-ELIGIBLE PRs (…):" and so on).
var workListVars = []string{"ISSUE_LIST", "PR_LIST", "MERGE_ELIGIBLE", "CI_FAILING"}

// guidanceBlankedVars render empty in a guidance render wherever a template
// still places them: the work lists, the session-claim note, the AUTHORIZED
// REPOS block (which repo to work next is not the contributor's choice) and
// ${GH_AUTH}, which is the resident agent's App-token and hive-open-pr
// contract — a contributor works with their own credentials, and their task
// prompt says how to push and open the PR.
var guidanceBlankedVars = append(append([]string{}, workListVars...), "IN_FLIGHT", "AUTHORIZED_REPOS", "GH_AUTH")

var lonePlaceholder = regexp.MustCompile(`^\$\{([A-Z_][A-Z0-9_]*)\}$`)

// BuildAgentGuidance is BuildAgentGuidanceForTask with no task known: no
// Relevant Knowledge, and the primary repository's AGENTS.md.
func (s *Scheduler) BuildAgentGuidance(agentName string) string {
	return s.BuildAgentGuidanceForTask(agentName, github.Issue{})
}

// BuildAgentGuidanceForTask renders the named role's GUIDANCE for a
// contributor working one assigned task in that role: the resolved policy
// template (same chain and same ${VAR} engine as the resident kick) without
// the resident agent's session sections or work lists, plus the static
// house-style, work-source, review, formal-quality and work-tracker seams.
//
// task is the assigned issue. Its title and labels prime the "# Relevant
// Knowledge" section the way a resident kick's issues do, and its Repo picks
// the repository whose AGENTS.md and skills are injected — the repo the
// contributor will change, not the role's primary repo. A task with no Repo
// falls back to the primary repo; one with no title or labels primes nothing.
//
// It is not a kick. It reads no queue and makes no classification, triage or
// kick-summary call, and priming records no KickSummary or primed slugs for
// the resident agent, so it is safe to build on every contributor assignment.
// Left out on purpose: the work lists, the merge-eligible and CI-failing
// lists, the resident session sections (residentWorkSections), ${GH_AUTH},
// AUTHORIZED REPOS, the workflow-push ceiling and task-MCP pointer (both
// describe the resident agent's own token and CLI), and every dynamic tail
// seam — held-PR coordination, RED MAIN, fix-before-new, follow-up handoffs,
// in-flight and maintainer notes, the claim rule and the closing "Begin now".
//
// Policy prose that names the resident workflow inline, outside those
// sections, stays: the caller's framing tells the contributor that the
// assigned task is the whole job.
func (s *Scheduler) BuildAgentGuidanceForTask(agentName string, task github.Issue) string {
	agentName = strings.TrimSpace(agentName)
	if s == nil || s.cfg == nil || agentName == "" {
		return ""
	}
	guidance := &roleGuidance{task: task}
	render := func(template string) (string, bool) {
		return s.substituteTemplate(guidanceTemplate(template), &github.ActionableResult{}, agentName, nil, nil, false, guidance)
	}
	message, ok := s.resolveTemplatedKick(agentName, render)
	if !ok {
		// The hardcoded kicks are work lists with a few steps around them;
		// the repository's own instructions are all there is to keep.
		message = renderTemplatedKick(agentName, "${KNOWLEDGE}", render)
	}
	if message == "" {
		return ""
	}
	message = applyStaticSeams(message, []func(string) string{
		func(m string) string { return s.addHouseStyle(agentName, m) },
		addGovernorWorkSourceRule,
		func(m string) string { return s.addIndependentReviewSection(agentName, m) },
		func(m string) string { return s.addFormalQualityCapability(agentName, m) },
		s.addWorkTrackerSection,
	})
	return collapseBlankLines(strings.TrimRight(message, "\n")) + "\n"
}

// guidanceTemplate removes the resident agent's session sections and work
// lists from a kick template before it is rendered. Headings inside fenced
// code blocks (issue-body examples) are not sections. A line holding only a
// non-work placeholder — ${KNOWLEDGE} above all — survives even inside a
// dropped section, so the repository's instructions are never lost with it.
func guidanceTemplate(template string) string {
	lines := strings.Split(template, "\n")
	out := make([]string, 0, len(lines))
	inFence, dropping := false, false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		} else if !inFence {
			switch {
			case strings.HasPrefix(line, "## "):
				dropping = isResidentWorkSection(strings.TrimSpace(strings.TrimPrefix(line, "## ")))
			case strings.HasPrefix(line, "# "):
				dropping = false
			}
		}
		name := ""
		if m := lonePlaceholder.FindStringSubmatch(trimmed); m != nil {
			name = m[1]
		}
		if name != "" && isWorkListVar(name) {
			if n := len(out); n > 0 && strings.HasSuffix(strings.TrimSpace(out[n-1]), ":") {
				out = out[:n-1]
			}
			continue
		}
		if dropping && name == "" {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func isResidentWorkSection(heading string) bool {
	for _, name := range residentWorkSections {
		if strings.EqualFold(heading, name) {
			return true
		}
		if len(heading) > len(name) && strings.EqualFold(heading[:len(name)], name) {
			switch rest := heading[len(name):]; {
			case strings.HasPrefix(rest, " —"), strings.HasPrefix(rest, " -"), strings.HasPrefix(rest, " ("), strings.HasPrefix(rest, ":"):
				return true
			}
		}
	}
	return false
}

func isWorkListVar(name string) bool {
	for _, v := range workListVars {
		if name == v {
			return true
		}
	}
	return false
}

var blankLineRun = regexp.MustCompile(`\n{3,}`)

// collapseBlankLines folds the blank-line runs dropped sections leave behind.
func collapseBlankLines(s string) string {
	return blankLineRun.ReplaceAllString(s, "\n\n")
}
