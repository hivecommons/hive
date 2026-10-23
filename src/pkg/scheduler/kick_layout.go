package scheduler

import (
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

// Kick layout (hivecommons/hive#12125).
//
// Provider prompt caching (Anthropic, OpenAI) only reuses a BYTE-IDENTICAL
// prefix, so the typed kick is laid out static-first: everything that is the
// same for a given agent on every kick comes before the first byte that
// changes between kicks. The order below is fixed and is what
// TestKickLayoutStaticPrefixIsCacheable asserts.
//
//	1. [canary]                              random, opt-in, ALWAYS first (ADR-0008; added by BuildKickMessages)
//	2. [agent:<name>]                        static — the kick header
//	3. governor work-source rule             static
//	4. independent review section            static per agent config
//	5. workflow push ceiling                 static per agent mode
//	6. formal quality capability             static per role + config
//	7. work tracker section                  static per work-source config
//	7b. task context MCP pointer             static per agent task_mcp config
//	8. <resolved policy body>                static text, then ${ISSUE_LIST}/${PR_LIST}/… where the template puts them
//	9. REPO-SCOPED CADENCE TARGET            semi-static (repo-scoped kicks only)
//	10. AUTHORIZED REPOS                     semi-static (include_repos)
//	11. hold-gated PR coordination           dynamic (held PR snapshot)
//	12. red-PR fix-before-new                dynamic (ci-failing.json)
//	13. review-thread fix-before-new         dynamic (review-threads.json)
//	14. PR follow-up handoff                 dynamic (pr-followup/)
//	15. guide flow line                      dynamic (guide only)
//	16. in-flight note                       dynamic (session claims)
//	17. question answer contract             dynamic (scanner only)
//	18. in-progress claim rule               static, present whenever the kick lists issues
//	19. closing instruction                  static, always last
//
// What this does NOT change: `/clear` still precedes every kick
// (pkg/agent/manager_kick.go), the canary is neither stabilised nor moved
// out of first position, the full contract is still typed on every kick (no
// --append-system-prompt-file, no cached policy state), and every seam
// below is still an unconditional post-resolution injection — a customized
// or remotely sourced template cannot omit any of them. Only the ORDER
// moved: 3–7 used to be interleaved with 11–14 directly under the header,
// and 9 was prepended at offset 0.
//
// Why the data-bearing seams (11–14) sit AFTER the policy body rather than
// under the header where they used to be: the body's own lists end the
// cacheable prefix, so anything placed before the body must itself be
// static or it costs the cache every byte of policy behind it. The
// fix-before-new and hold-coordination blocks are still mandatory
// preflights — each says so in its own text — and they now sit at the tail,
// directly ahead of the closing instruction, where the agent reads them
// last before acting. A `${…}` the template places itself is left where
// the template put it; the seam only adds what the template did not.
//
// ${GH_AUTH} is NOT moved out of the shipped templates: ghAuthInstructions is
// a constant, so it is already part of the byte-identical prefix wherever a
// template puts it.

// kickAssembly is what assembleKick needs besides the resolved body.
type kickAssembly struct {
	agent        string
	repo         string
	includeRepos bool
	issues       []github.Issue
	actionable   *github.ActionableResult
	idleReason   string
	// elideStuffed is the task MCP drop_stuffed_context decision for this
	// kick; the MCP pointer seam words itself accordingly.
	elideStuffed bool
}

// assembleKick lays the post-resolution seams around a resolved kick body in
// the documented order. An empty body (fail-closed kick) stays empty, and a
// seam that fails closed (held-PR coordination under ioscan) empties it.
func (s *Scheduler) assembleKick(a kickAssembly, message string) string {
	if message == "" {
		return message
	}

	// Static prefix, directly under the header. Each seam splices in right
	// after the header line, so the LAST one applied ends up FIRST; the slice
	// is written in reading order and applied in reverse to keep it readable.
	staticSeams := []func(string) string{
		addGovernorWorkSourceRule,
		func(m string) string { return s.addIndependentReviewSection(a.agent, m) },
		func(m string) string { return s.addWorkflowPushCeiling(a.agent, m) },
		func(m string) string { return s.addFormalQualityCapability(a.agent, m) },
		s.addWorkTrackerSection,
		func(m string) string { return s.addTaskMCPPointer(m, a.elideStuffed) },
	}
	for i := len(staticSeams) - 1; i >= 0; i-- {
		message = staticSeams[i](message)
		if message == "" {
			return message
		}
	}

	// Semi-static: changes when the operator pauses/scopes repos, not per kick.
	if a.repo != "" {
		message = appendKickSection(message, "REPO-SCOPED CADENCE TARGET: "+a.repo)
	}
	if a.includeRepos {
		// Built per agent, not once for the fleet: a repo-scoped agent must be
		// told its own AUTHORIZED REPOS, or the one section every agent sees
		// would name repos it cannot write to (#6204).
		message = appendKickSection(message, s.buildReposSectionFor(a.agent))
	}

	// Dynamic tail, in reading order.
	message = s.addHeldPRCoordination(a.agent, a.actionable, message)
	message = s.addRedMainNotice(a.agent, a.actionable, message)
	message = s.addRedPRFixFirst(a.agent, message)
	message = s.addReviewThreadFixFirst(a.agent, message)
	message = s.addPRFollowUpHandoff(a.agent, message)
	message = s.addGuideFlow(a.agent, a.actionable, message)
	message = s.addInflightNote(message, a.issues)
	message = s.addQuestionAnswerContract(a.agent, message, a.issues)
	message = addInProgressClaimRule(message, a.issues)
	if a.idleReason == "" {
		message = addConcreteKickClosingInstruction(message)
	}
	return message
}

// insertAfterKickHeader splices a static section directly under the kick's
// first line (the `[agent:<name>]` tag), separated by blank lines. A message
// without a newline has no header to splice under and gets the section first.
func insertAfterKickHeader(message, section string) string {
	section = strings.Trim(section, "\n")
	if section == "" {
		return message
	}
	if newline := strings.IndexByte(message, '\n'); newline >= 0 {
		return message[:newline+1] + "\n" + section + "\n" + message[newline+1:]
	}
	return section + "\n\n" + message
}

// appendKickSection appends a section to the kick, separated by one blank
// line and ending in a single newline.
func appendKickSection(message, section string) string {
	section = strings.Trim(section, "\n")
	if section == "" {
		return message
	}
	return strings.TrimRight(message, "\n") + "\n\n" + section + "\n"
}
