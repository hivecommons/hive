// Reviewer lane (#5480): an explicitly-configured agent role with the
// authority to adjudicate ESCALATED (needs-human) hive-authored PRs.
//
// Escalation is a one-way door to a human queue with no assigned worker: when
// a PR exhausts its fix budget it gets `needs-human` and every automated path
// stands down — by design. Nothing owned that queue (kubestellar/console,
// 2026-09-01: nine escalated PRs sat invisible for a week until an ad-hoc
// operator-authorized review fleet cleared all nine in ~1 hour, every fix
// mechanical from CI evidence). The reviewer lane is that missing worker:
// off by default (an operator must add an agent with `role: reviewer`),
// hard-gated to ACMM level >= reviewerLaneMinACMMLevel, and bounded to
// reviewerMaxPRsPerKick adjudications per kick.
//
// This is distinct from the pack-defined on-demand "reviewer" agent
// (reviewer-advisory.md), which is PR-triggered pre-merge verdict voting on
// HEALTHY PRs in ADVISORY mode. The lane here is cadence-kicked, targets only
// escalated red PRs, and requires push capability — an operator enables it by
// adding a cadence agent (any name) with `role: reviewer`.
package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	// RoleReviewer is the AgentConfig.Role value that routes an agent into
	// the escalated-PR adjudication lane. Config loading defaults Role to the
	// agent's name, so an agent literally named "reviewer" (with no template
	// shadowing it) also lands here.
	RoleReviewer = "reviewer"

	// reviewerLaneMinACMMLevel is the HARD gate: the reviewer work list and
	// adjudication contract render only when the hive runs at this ACMM level
	// or above. Below it the kick states the lane is dormant and the agent
	// stands down — an operator adding a reviewer-role agent to an L3 hive
	// must not silently acquire an agent that un-escalates human queues.
	reviewerLaneMinACMMLevel = 5

	// reviewerCloseACMMLevel is the level at or above which the reviewer may
	// actually CLOSE a PR after posting a recommend-close comment. Below it,
	// closing stays operator-only and the comment is the entire verdict.
	reviewerCloseACMMLevel = 6

	// reviewerMaxPRsPerKick caps how many escalated PRs one kick may
	// adjudicate, oldest first. The cap is enforced structurally: the work
	// list itself never contains more rows than this.
	reviewerMaxPRsPerKick = 3

	// ReviewerPassedLabel marks a PR the reviewer lane has already repaired or
	// de-escalated once. A PR that RE-escalates after carrying this label
	// belongs to a true human: the work-list builder excludes labeled rows, so
	// there is no reviewer<->escalation ping-pong. The machinery-generation
	// amnesty (#5471) plus one reviewer pass then human is the full ladder.
	// Canonical in pkg/escalation, where Sweep's reviewer-verdict
	// reconciliation resets the ledger for PRs carrying it (#5511, gap G1).
	ReviewerPassedLabel = escalation.ReviewerPassedLabel

	// ReviewerRecommendCloseLabel marks a PR on which the reviewer has already
	// delivered a RECOMMEND-CLOSE verdict. Below reviewerCloseACMMLevel the PR
	// stays open (closing is operator-only), so without this marker the row
	// remains fully adjudicable and the reviewer re-comments the same verdict
	// on every kick until a human acts (#5511, gap G4 — Spin model witness
	// w_onepass_acmm5). The work-list builder excludes labeled rows exactly
	// like ReviewerPassedLabel; the kick contract instructs the reviewer to
	// add the label alongside the recommend-close comment.
	ReviewerRecommendCloseLabel = "reviewer-recommend-close"

	// reviewerLaneTemplate is the shipped kick template for the reviewer LANE
	// (kubestellar/hive#5617 item 2). It is deliberately a different file from
	// reviewer-advisory.md, which belongs to the pack-defined on-demand reviewer
	// described in the file header — that agent votes on HEALTHY PRs pre-merge
	// and never touches the needs-human queue, so the two must never resolve to
	// each other's prompt.
	//
	// The lane reaches this template through buildReviewerMessage rather than the
	// ordinary kick_template chain, because an operator enables the lane by ROLE
	// on an agent that may be named anything and carries no kick_template. It
	// ships in pkg/policies/defaults, so loadNamedTemplate finds it after the
	// operator override and git-clone paths — an operator can still customise the
	// contract, and the safety gates below are the reason that is safe.
	reviewerLaneTemplate = "reviewer-lane.md"
)

// agentRole resolves the effective role for an agent name: the configured
// AgentConfig.Role when set, else the base agent name (config loading applies
// the same default, but a directly-constructed Config in tests may not have).
func (s *Scheduler) agentRole(agentName string) string {
	baseName := s.cfg.BaseAgentName(agentName)
	agentCfg, ok := s.cfg.Agents[agentName]
	if !ok {
		agentCfg, ok = s.cfg.Agents[baseName]
	}
	if ok && agentCfg.Role != "" {
		return agentCfg.Role
	}
	return baseName
}

// buildReviewerMessage is the hardcoded-fallback kick for reviewer-role
// agents, mirroring buildQualityMessage's structure. It renders the escalated
// hive-authored PR work list (from ci-failing.json) and the adjudication
// contract — or a dormant notice when the hive's ACMM level is below the gate.
func (s *Scheduler) buildReviewerMessage(agentName string, actionable *github.ActionableResult) string {
	level := 0
	if s.cfg.ACMMLevel != nil {
		level = *s.cfg.ACMMLevel
	}

	// THE TWO GATES BELOW ARE EVALUATED IN GO, BEFORE ANY TEMPLATE IS READ, AND
	// THAT IS THE WHOLE REASON A TEMPLATE IS SAFE TO SHIP (#5617 item 2).
	//
	// loadNamedTemplate honours operator overrides on disk, so the contract text
	// is editable — which is the point of templating it. Neither of these
	// decisions is: an edited template must not be able to un-dormant the lane on
	// a low-trust hive, and must not be able to invent work when the queue is
	// empty. Keeping both in code means the worst an operator can do to this
	// template is change the WORDING of a kick that was already going to be sent.
	if level < reviewerLaneMinACMMLevel {
		return reviewerKickHeader(agentName) + fmt.Sprintf(
			"⛔ REVIEWER LANE DORMANT: this hive runs at ACMM level %d; the escalated-PR\n"+
				"adjudication lane requires level %d or above. The needs-human queue belongs\n"+
				"entirely to human operators at this level. Do NOT touch, comment on, relabel,\n"+
				"rebase, or close any PR. Stand down this kick.\n",
			level, reviewerLaneMinACMMLevel)
	}

	workList := s.buildReviewerWorkList()
	if workList == "" {
		return reviewerKickHeader(agentName) +
			"ESCALATED PRs AWAITING ADJUDICATION: (none)\n" +
			"Nothing to adjudicate — stand down this kick. Do NOT go hunting for other work.\n"
	}

	// The shipped template renders the same contract from
	// pkg/policies/defaults/reviewer-lane.md. It is proven byte-identical to the
	// fallback below by TestReviewerLaneTemplate_ByteIdenticalToBuilder, so this
	// is an extraction and not a rewrite. The fallback stays for the case the
	// template cannot be resolved at all — a stripped binary, or an operator
	// override that reads as empty — because a reviewer with no contract is far
	// worse than one with the compiled-in wording.
	if rendered := s.renderReviewerLaneTemplate(agentName, actionable, level, workList); rendered != "" {
		return rendered
	}
	return s.buildReviewerMessageHardcoded(agentName, level, workList)
}

// reviewerKickHeader is the two-line preamble every reviewer kick opens with,
// including the two stand-down cases that never reach a template.
func reviewerKickHeader(agentName string) string {
	return fmt.Sprintf("[agent:%s]\nREVIEWER — adjudicate escalated (needs-human) hive-authored PRs.\n\n", agentName)
}

// renderReviewerLaneTemplate renders the shipped reviewer-lane template, or ""
// when it cannot be resolved or renders empty so the caller falls back.
//
// The reviewer-specific values are passed in rather than recomputed inside the
// substitution: buildReviewerMessage has ALREADY decided, from this exact work
// list, that there is work to adjudicate. Re-reading ci-failing.json here would
// let a rewrite between the two reads render a full adjudication contract over
// an empty list.
func (s *Scheduler) renderReviewerLaneTemplate(agentName string, actionable *github.ActionableResult, level int, workList string) string {
	tmpl := s.loadNamedTemplate(reviewerLaneTemplate)
	if tmpl == "" {
		return ""
	}
	extra := map[string]func() string{
		"REVIEWER_WORK_LIST":             func() string { return workList },
		"REVIEWER_MAX_PRS":               func() string { return fmt.Sprintf("%d", reviewerMaxPRsPerKick) },
		"REVIEWER_PASSED_LABEL":          func() string { return ReviewerPassedLabel },
		"REVIEWER_RECOMMEND_CLOSE_LABEL": func() string { return ReviewerRecommendCloseLabel },
		"REVIEWER_CLOSE_AUTHORITY":       func() string { return reviewerCloseAuthority(level) },
	}
	body, failClosed := s.substituteTemplateWithVars(tmpl, actionable, agentName, nil, extra)
	if failClosed || strings.TrimSpace(body) == "" {
		return ""
	}
	return body
}

// reviewerCloseAuthority renders the close-authority clause for a hive's ACMM
// level. It is computed HERE, not expressed as template logic, for the same
// reason the dormancy gate is: whether this agent may close a human-queued PR is
// a trust decision, and an operator editing prompt wording must not be able to
// grant it. Returns no trailing newline; the template supplies the spacing.
func reviewerCloseAuthority(level int) string {
	if level >= reviewerCloseACMMLevel {
		return fmt.Sprintf("     At this hive's ACMM level (%d) you MAY then close the PR yourself\n", level) +
			"     (gh pr close <number>) after the audited review and bead are recorded."
	}
	return fmt.Sprintf("     ⛔ NEVER close the PR yourself: closing is operator-only below ACMM level %d.\n", reviewerCloseACMMLevel) +
		"     The audited recommendation is your entire verdict; leave needs-human in place."
}

// buildReviewerMessageHardcoded is the compiled-in contract the template was
// extracted from, kept as the resolution fallback. Any edit here must be mirrored
// in pkg/policies/defaults/reviewer-lane.md — the parity test fails otherwise,
// which is exactly the drift guard that makes keeping both safe.
func (s *Scheduler) buildReviewerMessageHardcoded(agentName string, level int, workList string) string {
	var b strings.Builder
	b.WriteString(reviewerKickHeader(agentName))
	b.WriteString(s.ghAuthInstructions())
	b.WriteString(fmt.Sprintf("ESCALATED PRs AWAITING ADJUDICATION (max %d per kick, oldest first):\n", reviewerMaxPRsPerKick))
	b.WriteString(workList)

	b.WriteString("\nADJUDICATION CONTRACT — for EACH PR above, deliver EXACTLY ONE verdict:\n")
	b.WriteString("  1. REPAIR — the change is still wanted and can be made lossless:\n")
	b.WriteString("     a. Verify still-wanted: the problem it solves still exists on the base branch\n")
	b.WriteString("        and no merged PR supersedes it.\n")
	b.WriteString("     b. Verify lossless vs main: compare the PR's diff and its TEST COUNT against\n")
	b.WriteString("        the base branch (diff/test-count parity). A branch that drops code or tests\n")
	b.WriteString("        present on main is lossy — do NOT repair it; use RECOMMEND-CLOSE instead.\n")
	b.WriteString("     c. Fix on the SAME branch, working from the CI evidence above:\n")
	b.WriteString("        gh pr checkout <number> → fix → commit -s → git push\n")
	b.WriteString("        Do NOT open a replacement PR.\n")
	b.WriteString(fmt.Sprintf("     d. After completing the mandatory audit below, return it to the automated lane and mark your pass:\n"+
		"        gh pr edit <number> --remove-label needs-human --add-label %s\n", ReviewerPassedLabel))
	b.WriteString("  2. DE-ESCALATE — the failure was environmental (base-branch regression since\n")
	b.WriteString("     fixed, infra flake): rebase the branch on its base, push, complete the\n")
	b.WriteString("     mandatory audit below, then\n")
	b.WriteString(fmt.Sprintf("        gh pr edit <number> --remove-label needs-human --add-label %s\n", ReviewerPassedLabel))
	b.WriteString("  3. RECOMMEND-CLOSE — duplicate, superseded, or irreparably lossy: complete the\n")
	b.WriteString("     mandatory audit below using its recommend-close review body, then mark the\n")
	b.WriteString(fmt.Sprintf("     verdict delivered so it is never repeated:\n"+
		"        gh pr edit <number> --add-label %s\n", ReviewerRecommendCloseLabel))
	if level >= reviewerCloseACMMLevel {
		b.WriteString(fmt.Sprintf("     At this hive's ACMM level (%d) you MAY then close the PR yourself\n", level))
		b.WriteString("     (gh pr close <number>) after the audited review and bead are recorded.\n")
	} else {
		b.WriteString(fmt.Sprintf("     ⛔ NEVER close the PR yourself: closing is operator-only below ACMM level %d.\n", reviewerCloseACMMLevel))
		b.WriteString("     The audited recommendation is your entire verdict; leave needs-human in place.\n")
	}

	b.WriteString("\nMANDATORY AUDIT — complete BOTH records for EVERY verdict before relabeling or closing:\n")
	b.WriteString("  1. Submit a comment review through Hive's relay so the adjudication is attributed\n")
	b.WriteString("     as `agent_pr_reviewed` (direct `gh pr comment` / `gh pr review` is not sufficient):\n")
	b.WriteString("        REPAIR / DE-ESCALATE:\n")
	b.WriteString("          hive-review <number> --repo <owner/repo> --comment --body \"Reviewer adjudication: <REPAIR|DE-ESCALATE> — evidence: <why>; changes: <what>; tests: <commands/results>; remaining risk: <risk or none>\"\n")
	b.WriteString("        RECOMMEND-CLOSE (this is the one close-recommend record; do not post a second comment):\n")
	b.WriteString("          hive-review <number> --repo <owner/repo> --comment --body \"[reviewer] recommend close: <duplicate/superseded/lossy rationale>; evidence: <why>; tests: <checks>; remaining risk: <risk or none>\"\n")
	b.WriteString("     `hive-review` is asynchronous: poll the `.result.json` path it prints and do\n")
	b.WriteString("     not proceed until that file reports `\"ok\": true`. A queued request is not yet\n")
	b.WriteString("     an audit record; an error result must leave the PR in `needs-human`.\n")
	b.WriteString("  2. Create the matching advisory bead so the outcome reaches the advisory digest:\n")
	b.WriteString(fmt.Sprintf("        bd create --title \"Reviewer adjudication: <owner/repo>#<number> — <outcome>\" --type advisory --priority 2 --actor %s --external-ref \"gh-<owner/repo>#<number>\"\n", agentName))
	b.WriteString("     If either record fails, leave `needs-human` in place, do not add a reviewer verdict\n")
	b.WriteString("     label, and do not close; report the audit failure for a later retry.\n")

	b.WriteString("\nINVARIANTS:\n")
	b.WriteString(fmt.Sprintf("  ⛔ Adjudicate AT MOST %d PRs this kick, oldest first — the list above is\n", reviewerMaxPRsPerKick))
	b.WriteString("     already capped and ordered; work it top to bottom.\n")
	b.WriteString("  ⛔ NEVER touch a human-authored PR. The work list contains ONLY hive-authored\n")
	b.WriteString("     PRs; if you nonetheless encounter a PR not authored by this hive's bot\n")
	b.WriteString("     identity, skip it — it is not yours to adjudicate.\n")
	b.WriteString(fmt.Sprintf("  ⛔ NEVER adjudicate a PR whose ledger shows a prior reviewer pass (the\n"+
		"     `%s` label). Such PRs are excluded from your list; a PR that\n"+
		"     re-escalates after your pass belongs to a true human. That is why you add\n"+
		"     the label on every repair/de-escalate.\n", ReviewerPassedLabel))
	b.WriteString("  ⛔ NEVER run gh pr list / gh search — the work list above is your ONLY source.\n")

	return b.String()
}

// buildReviewerWorkList renders the escalated-PR work list from
// ci-failing.json ("" when the file is missing or holds no adjudicable rows).
func (s *Scheduler) buildReviewerWorkList() string {
	data, err := os.ReadFile(ciFailingPath)
	if err != nil {
		return ""
	}
	return formatReviewerWorkList(data)
}

// formatReviewerWorkList renders the reviewer work list from raw
// ci-failing.json bytes. Rows are ESCALATED hive-authored PRs only —
// writeMergeEligible builds the file exclusively from the hive's own PR
// enumeration with per-agent attribution, so every row is hive work; rows
// whose Labels carry ReviewerPassedLabel or ReviewerRecommendCloseLabel are
// excluded (one reviewer pass per PR, ever — whatever the verdict was).
// Output is capped at reviewerMaxPRsPerKick rows, oldest first by the PR's
// real forge creation time; rows written by a hub that recorded none fall back
// to the old (repo, number) proxy and sort last (#5617 item 4).
func formatReviewerWorkList(data []byte) string {
	type ciFailingRow struct {
		Number        int       `json:"number"`
		Repo          string    `json:"repo"`
		Title         string    `json:"title"`
		Agent         string    `json:"agent"`
		Labels        []string  `json:"labels"`
		FailingChecks []string  `json:"failing_checks"`
		Excerpt       string    `json:"excerpt"`
		Escalated     bool      `json:"escalated"`
		CreatedAt     time.Time `json:"created_at"`
	}
	var payload struct {
		Items []ciFailingRow `json:"ci_failing"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return ""
	}
	var rows []ciFailingRow
	for _, pr := range payload.Items {
		if !pr.Escalated {
			continue // still in the automated fix lane — not the reviewer's
		}
		adjudicated := false
		for _, l := range pr.Labels {
			// One reviewer pass per PR, ever — for EVERY verdict class:
			// reviewer-passed covers REPAIR/DE-ESCALATE; recommend-close
			// covers the below-close-authority verdict whose PR stays open
			// awaiting an operator (#5511, gap G4).
			if l == ReviewerPassedLabel || l == ReviewerRecommendCloseLabel {
				adjudicated = true
				break
			}
		}
		if adjudicated {
			continue // one reviewer pass per PR: the rest is a human's
		}
		rows = append(rows, pr)
	}
	if len(rows) == 0 {
		return ""
	}
	// Oldest first, by the PR's TRUE creation time (#5617 item 4). The old key
	// carried no age signal at all: PR numbers are monotonic only WITHIN a
	// repo, so ordering on (repo, number) sorted by repo NAME first. Against
	// the reviewerMaxPRsPerKick cap that is a starvation bug rather than a
	// cosmetic one — a month-old escalated PR in "zeta/service" sits behind
	// three newer ones in "alpha/console" on every kick, forever, and the
	// reviewer never reaches it. Rows whose creation time is unknown (a
	// ci-failing.json written by an older hub, or a forge that omitted the
	// field) sort LAST and keep the old proxy among themselves: an unproven
	// age must not jump ahead of a measured one.
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.CreatedAt.IsZero() != b.CreatedAt.IsZero() {
			return !a.CreatedAt.IsZero()
		}
		if !a.CreatedAt.IsZero() && !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Number < b.Number
	})

	var b strings.Builder
	for i, pr := range rows {
		if i >= reviewerMaxPRsPerKick {
			b.WriteString(fmt.Sprintf("  … %d more escalated PRs held for later kicks (cap: %d per kick)\n",
				len(rows)-i, reviewerMaxPRsPerKick))
			break
		}
		author := pr.Agent
		if author == "" {
			author = "scanner"
		}
		b.WriteString(fmt.Sprintf("  %s#%d — %s\n", pr.Repo, pr.Number, pr.Title))
		b.WriteString(fmt.Sprintf("    original author agent: %s\n", author))
		if !pr.CreatedAt.IsZero() {
			// The ordering key, shown so the reviewer can see for itself that
			// the list really is oldest-first — the INVARIANTS below forbid
			// running `gh pr list` to check.
			b.WriteString(fmt.Sprintf("    opened: %s\n", pr.CreatedAt.UTC().Format(time.RFC3339)))
		}
		b.WriteString(fmt.Sprintf("    checkout: gh pr checkout %d --repo %s\n", pr.Number, pr.Repo))
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
	return b.String()
}
