package scheduler

import (
	"fmt"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/github"
)

// Mode-dependent seam text (hivecommons/hive#20036).
//
// The work-source, in-progress claim and closing seams are added to every kick
// and stay unconditional (#12134). What they tell the agent to do must still
// be something its mode can do: the issue-request relay refuses issue,
// comment, claim, close and label requests below ISSUES_ONLY
// (crux/agent/manager_modes.go, CanCreateIssues), and the PR and push relays
// refuse below ISSUES_AND_PRS (CanPush). An ADVISORY agent told to "file
// issues/PRs" or to "run hive-open-issue claim" spends turns on refused calls
// and reports its work as blocked. So the seams stay, and only their wording
// follows the mode the relays check.

// supervisorRole is the role whose shipped policies (supervisor-nogithub.md,
// supervisor-advisory.md) forbid all GitHub use.
const supervisorRole = "supervisor"

// inProgressClaimRuleIssuesOnly is the claim seam for an ISSUES_ONLY agent.
// It may file and comment on issues but cannot push, so it never starts a fix
// and never sends the claim request that marks one as started.
const inProgressClaimRuleIssuesOnly = "Your mode can file and comment on issues but cannot push or open PRs, so you do not implement fixes and you do not claim issues: never mark an issue from your list as started. Triage, verify and report on it through the issue relay as your policy describes; the agent that implements the fix marks the start. Issues already labelled `hive/in-progress` or `claimed` are someone else's: leave them alone."

// inProgressClaimRuleAdvisory is the claim seam for an ADVISORY agent, which
// can make no GitHub write at all.
//
// It does not forbid comments: an ADVISORY agent with the `converse`
// capability may comment directly (crux/proxy/rules.go), and its policy says
// when to.
const inProgressClaimRuleAdvisory = "Your mode cannot file, claim, label or close issues, and you do not implement fixes: never mark an issue from your list as started. Record what you find in an advisory bead; an agent whose mode allows it acts on the issue."

// concreteKickClosingInstructionIssuesOnly / concreteKickClosingInstructionAdvisory
// are the closing seam for the two modes that cannot open PRs. Each keeps the
// "Begin now:" lead and the no-human line that addConcreteKickClosingInstruction
// keys on.
const (
	concreteKickClosingInstructionIssuesOnly = "Begin now: pick the authorized repo you covered least recently and do your role's work there this session, filing issues through the hive relays as documented above. Your mode cannot push or open PRs, so do not implement fixes. Do not ask for clarification: no human is attached to this session; if something is ambiguous, make the conservative choice and note it in your output."
	concreteKickClosingInstructionAdvisory   = "Begin now: pick the authorized repo you covered least recently and do your role's work there this session, recording each finding in an advisory bead. Your mode cannot file issues, claim them, apply labels or open PRs, so do not try. Do not ask for clarification: no human is attached to this session; if something is ambiguous, make the conservative choice and note it in your output."
	concreteKickClosingInstructionNoGitHub   = "Begin now: do your role's work this session from the bead store and the hive's own state, recording each finding in an advisory bead. You have no GitHub access: do not run GitHub commands or open issues, comments, labels or PRs. Do not ask for clarification: no human is attached to this session; if something is ambiguous, make the conservative choice and note it in your output."
)

// governorWorkSourceRuleNoGitHub is the work-source seam for an agent whose
// policy forbids every GitHub call (#20038). It names no GitHub command, so an
// agent cannot read it as a hint to use one.
const governorWorkSourceRuleNoGitHub = "This kick lists no PRs or issues, and you have no GitHub access: do not run `gh` or any other GitHub command to find or read work. Work from the bead store and the hive's own state as your policy describes."

// kickWriteMode is the GitHub mode the relays check for this agent: the
// configured mode (tools-derived when set, as agentEffectiveMode resolves it),
// else the ACMM default table the agent manager falls back to
// (crux/agent/manager_modes.go agentMode). An unset ACMM level resolves as the
// default table's lowest tier, which is what the manager does too.
func (s *Scheduler) kickWriteMode(agentName string) crux.AgentMode {
	if configured := s.agentEffectiveMode(agentName); configured != "" {
		if parsed, ok := crux.ParseAgentMode(configured); ok {
			return parsed
		}
	}
	return crux.DefaultAgentMode(agentName, s.cfg.ACMMLevelOrZero())
}

// isNoGitHubAgent reports whether the agent is the ADVISORY supervisor, whose
// shipped policies forbid every GitHub call. Hive has no per-agent "no GitHub"
// setting, so the role selects it, guarded by the mode: a supervisor an
// operator promotes to a writing mode gets the ordinary text.
func (s *Scheduler) isNoGitHubAgent(agentName string) bool {
	if s.cfg == nil {
		return false
	}
	return s.agentRole(agentName) == supervisorRole && !s.kickWriteMode(agentName).CanCreateIssues()
}

// governorWorkSourceRuleFor returns the work-source seam text for the agent.
func (s *Scheduler) governorWorkSourceRuleFor(agentName string) string {
	if s.isNoGitHubAgent(agentName) {
		return governorWorkSourceRuleNoGitHub
	}
	return governorWorkSourceRule
}

// inProgressClaimRuleFor returns the claim seam text for the agent's mode.
func (s *Scheduler) inProgressClaimRuleFor(agentName string) string {
	mode := s.kickWriteMode(agentName)
	switch {
	case mode.CanPush():
		return inProgressClaimRule
	case mode.CanCreateIssues():
		return inProgressClaimRuleIssuesOnly
	default:
		return inProgressClaimRuleAdvisory
	}
}

// concreteKickClosingInstructionFor returns the closing seam text for the
// agent's mode.
func (s *Scheduler) concreteKickClosingInstructionFor(agentName string) string {
	if s.isNoGitHubAgent(agentName) {
		return concreteKickClosingInstructionNoGitHub
	}
	mode := s.kickWriteMode(agentName)
	switch {
	case mode.CanPush():
		return concreteKickClosingInstruction
	case mode.CanCreateIssues():
		return concreteKickClosingInstructionIssuesOnly
	default:
		return concreteKickClosingInstructionAdvisory
	}
}

// hiveHoldsAgentPRs reports whether this hive's ACMM level gates every agent
// PR on a maintainer: at L3-L5 the PR-request watcher labels each new agent PR
// with the ready-for-review gate label (cmd/hive shouldHoldAgentPR), and that
// label blocks the merge like `hold` until a person removes it (#20044). The
// red-team lane is held at every level.
func (s *Scheduler) hiveHoldsAgentPRs(agentName string) bool {
	if s.cfg == nil {
		return false
	}
	if s.cfg.IsRedTeamAgent(agentName) {
		return true
	}
	level := s.cfg.ACMMLevelOrZero()
	return level >= holdGatedACMMMinLevel && level <= holdGatedACMMMaxLevel
}

// mergeAfterGreenSentence says what happens to the agent's PR once its checks
// pass. At hold-gated levels green CI does not merge it; elsewhere the sweep
// does.
func (s *Scheduler) mergeAfterGreenSentence(agentName string) string {
	if s.hiveHoldsAgentPRs(agentName) {
		return fmt.Sprintf("at this level the hive labels every agent PR `%s`, so green CI does not merge it: it merges after a maintainer reviews it and removes that label", github.ReadyForReviewLabelName())
	}
	return redPRFixMergeAfterGreenDefault
}

// reviewerLaneMergeAfterGreen is the reviewer lane's version of the same
// clause, for a PR it repaired.
func (s *Scheduler) reviewerLaneMergeAfterGreen(agentName string) string {
	if s.hiveHoldsAgentPRs(agentName) {
		return fmt.Sprintf("at this level the PR carries `%s`, so green CI does not merge it: it merges after a maintainer reviews it and removes that label", github.ReadyForReviewLabelName())
	}
	return "the automerge sweep merges it the moment its checks are green"
}
