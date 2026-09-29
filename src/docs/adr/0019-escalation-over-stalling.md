# ADR-0019: Escalate to direction, spec, signal, or meta-issue instead of stalling

Status: Accepted

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

1. **`needs-direction`** — Use when the next step is a maintainer decision,
   authority grant, risk acceptance, or product/voice choice. Add the
   `needs-direction` label and post exactly one comment to the maintainer with a
   short question and concrete options `A`, `B`, and `C`.
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

Verified RFC case studies map as follows:

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
