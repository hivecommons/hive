# ADR-0019: Escalate to direction, spec, signal, or meta-issue instead of stalling

Status: Accepted

## Current action boundary

| Kind of work | Decision park | How a person answers |
|---|---|---|
| Issue | `stage/045-needs-decision` | A human with repo write/maintain/admin permission uses `/hive approve` or `/hive decision <direction>`. The command flow clears decision parking, adds `approved-direction`, and lifts `needs-human` even when a human applied it; it never clears `hold`. Label-driven repositories use their configured acceptance labels instead. |
| Pull request | `stage/085-needs-decision` | A trusted merger reviews the exact current head. On Hive-authored PRs at 085 or `stage/070-fixing`, `/hive decision <instruction>` routes the full text to the author/fixer; `/hive decision approve <rationale>` records head-bound approval. Bare `/hive approve` is acknowledged only, not a release. Other authors use GitHub reviews/manual merging. |

These are decision signals, not universal merge holds. All other merge gates
still apply; use `hold` or GitHub branch rules when merging must stop. See
[Running at Level 6](../running-at-level-6.md#does-the-hive-reviewer-stop-a-merge)
for the enforcing review gate and trusted-human override.

The current commands are implemented by `SweepIssueUnparkCommands` and
`tryUnparkPullRequest` in [issue command handling](../../pkg/github/issue_unpark_command.go)
and [PR decision handling](../../pkg/github/pr_unpark_decision.go).
The amendments below are migration history, not alternate current instructions.

## Context

Hive already has several half-present escalation mechanisms: hold labels keep
agents away from work that needs a human, `needs-human` parks failed CI fix
loops, and ADR-0010 defines a circuit breaker for blind retry loops. The gap is
the decision vocabulary after an item stops converging. Agents often keep
reacting to one PR or one issue while humans do the higher-level work: ask for a
maintainer decision, request a missing spec, demand a better CI signal, or group
recurring failures into a tracker.

For this ADR, an item is **stalled** when it has carried any hold label for more
than 48 hours, or when Hive has made at least two failed attempts to move it
without convergence. Escalation is rate-limited to one escalation move per item
per 24 hours.

## Decision

Hive agents escalate stalled work instead of silently parking it. Each
escalation must choose one of four moves:

1. **Decision park** (originally `needs-direction`) — Use when the next step
   is a maintainer decision, authority grant, risk acceptance, or product/voice
   choice. Park an **issue at `stage/045-needs-decision`**, or a **PR at
   `stage/085-needs-decision`**, and post exactly one comment to the maintainer
   with a short question and concrete options `A`, `B`, and `C`; with
   `hive-open-issue comment … --needs-decision` the relay applies the park
   label as part of the comment, so no separate label step is needed.
2. **`needs-spec`** — Use when the issue cannot be implemented safely because
   the expected behavior, API contract, acceptance criteria, or compatibility
   rule is missing. File one issue labelled `kind/spec` and `needs-spec` that
   lists the open questions, link it from the stalled item, and stop changing
   implementation code until the spec is answered.
3. **`needs-signal`** — Use when the same CI check fails at least three times
   with no code-caused root cause in the PR diff, or when the available logs do
   not expose the actionable failure. File one issue labelled `ci`,
   `kind/test`, and `needs-signal` asking for the missing guard, excerpt,
   blocking check, runner contract, or pre-PR gate; link affected items.
4. **`meta-issue`** — Use when three or more open items share one root cause.
   File one tracker labelled `meta`, link the child issues/PRs, summarize the
   shared cause, and stop working the children individually except to point them
   at the tracker.

ADR-0010 still governs CI fix-loop counting and `needs-human` authority. This
ADR defines what the agent should do after recognizing that the item is no
longer a single reactive fix.

### Historical case studies

The verified RFC cases below retain the original `needs-direction` spelling.
For action today, use the kind-specific decision park above.

| Case | Escalation move |
|---|---|
| tuna-os/hive #75 (API-proxy log-permission PR held for maintainer ack) | `needs-direction`: ask whether to accept, reject, or request a safer alternative. |
| tuna-os/hive #62 and #61 (architectural refactors waiting for direction) | `needs-direction`: present refactor options and ask which shape to pursue. |
| tuna-os/hive #70 (strategist fork-delta refresh waiting for review) | `needs-direction`: ask whether to adopt, revise, or defer the planning artifact. |
| tuna-os/hive #45 (guide docs PR waiting for review) | `needs-direction`: ask whether to merge, narrow, or rewrite the docs. |
| tuna-os/hive #79 (copy needing human voice/decision) | `needs-direction`, or `needs-spec` if the missing voice reflects absent product copy rules. |
| tuna-os/hive #68 and #39 (Actions cannot open PRs) | `needs-direction`: request the permission decision; use `meta-issue` if the permission block spans three or more open items. |
| tuna-os/hive #38 (`verify PR contents` failing org-wide) | `needs-signal`: file the missing/verifier-image CI signal issue and link affected PRs. |
| tuna-os/hive #53 and #52 (liveness-adjacent launch-contract/umask fixes shaped by a human) | `needs-spec`: capture the launch contract and filesystem-permission expectations before more fixes. |
| Candidate pattern 1 (fix-loop on a static red head) | `needs-signal` when repeated failures show the check lacks actionable evidence; ADR-0010 still caps the retry loop. |
| Candidate pattern 2 (root-cause workaround sign-off) | `needs-direction`: ask the maintainer to accept, reject, or replace the risk/workaround. |
| Candidate pattern 3 (permission block across repos) | `meta-issue` once three items share the permission root cause; otherwise `needs-direction` on the affected item. |
| Candidate pattern 4 (CI-rescue classes) | `needs-signal`: file the missing blocking guard, excerpt, contract pin, or pre-PR test issue. |
| upstream #9472, #9473, #9474, #9475, #9477, #9479, #9480, #9471, #9476, #9478 | `meta-issue` for recurring fix-loop/lane-ownership classes, with `needs-signal` for CI evidence gaps and `needs-direction` for permission/fork authority choices. |
| upstream #9536 / #9481 / #9550 (CI rescue class caught by hand) | `needs-signal`: require the touched-package Go-test/docs-guard pre-PR signal and link the tracker. |

## Consequences

Agents stop treating stalled work as a local retry problem. Maintainers get one
structured decision request, spec request, signal request, or tracker instead of
many per-item nudges. The trade-off is more issues and labels, but each one
records the missing decision or signal that made automation unsafe.

## Amendment (2026-10, [#12683](https://github.com/hivecommons/hive/issues/12683))

**Historical intermediate state, superseded for issues by the next amendment.**

The `needs-direction` label this ADR introduced, and the `needs-decision`
label the issue relay later applied, are retired. Both parked an item on a
maintainer ruling with identical gates — hard-suppressed at enumeration, the
same `/hive approve` / `/hive decision <text>` un-park, the same derived
`stage/085-needs-decision` — so a parked issue wore the stage label plus one
or two park labels that said the same thing. `stage/085-needs-decision` is now
the single park signal and the move this ADR calls `needs-direction` is made
by applying it (through the issue relay's `label` command, or
`hive-open-issue --needs-decision` when filing). The historical mapping table
above keeps the original spelling. Open items still carrying a retired label
are migrated by the issue stage reconciler (085 added, then the retired label
removed) and the two label definitions are deleted from each repo once
nothing open carries them. `needs-human` is unchanged: it means a human-held
identity or consent is required, not that a ruling is owed.

## Amendment (2026-10-17, owner decision: issue park is `stage/045-needs-decision`)

**Stage migration history.** The stage split remains current; the statement
below about preserving `needs-human` reflects the old command behavior.
Current explicit maintainer commands lift it as described at the top.

Issues no longer park at `stage/085-needs-decision`; that stage is PR-only.
The issue-side park is `stage/045-needs-decision`, which sits between
`040-covered-by-pr` and the PR stages so an issue's stage never reads as
"past the merge gate". Everything in the amendment above holds with 045 in
place of 085 for issues: the relay's `--needs-decision` and `label` commands
apply 045, the retired spellings migrate to 045, and `needs-human` on an
issue now implies 045 (on a PR it still implies 085). `/hive approve` /
`/hive decision <text>` un-park to `approved-direction` as before and keep
`needs-human`. Open issues still wearing 085 are re-staged to 045 by the
issue stage reconciler and by a one-shot startup sweep; the 085 definition
is never deleted, PRs wear it.
