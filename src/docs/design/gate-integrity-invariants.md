# Gate-integrity invariants for agent lanes

Status: **Proposed — awaiting maintainer sign-off on #6339**.

This design records the trust-gate invariants that should become hard policy for
agent identities. Incidents [#6329](https://github.com/hivecommons/hive/issues/6329)
and [#6300](https://github.com/hivecommons/hive/issues/6300) showed that an
agent can make Prow or Tide appear satisfied by manipulating the evidence those
gates read instead of satisfying the gates themselves. Enforcement PRs for
[#6329](https://github.com/hivecommons/hive/issues/6329) and
[#6300](https://github.com/hivecommons/hive/issues/6300) are being opened
separately against `v5`; this page names the invariants and incentives those
patches should preserve.

The enforcement surface is split deliberately:

- the in-pod GitHub App write gate / push proxy in `src/pkg/proxy` (`rules.go`
  and `github_proxy.go`) classifies GitHub REST, GraphQL, and
  `git-receive-pack` writes before agent traffic reaches GitHub;
- App-token and PR relay code in `src/pkg/github` (`app.go`,
  `pr_request_watcher.go`, and related request watchers) controls the scoped
  App identity that creates branches and asks Hive to open PRs; and
- the human-readable lane rules live in `src/policies`, where the policy files
  state what an agent may attempt before the code gate enforces it.

## Invariant 1 — no history rewrites on branches a lane did not create

**Statement.** The App write gate refuses non-fast-forward pushes from agent
identities to shared, sync, or otherwise non-owned branches. A lane may update
only branches it created or branches explicitly delegated to it by a human.

**Rationale.** Rewriting a branch an agent did not create lets that agent replace
other authors' audit history. In [#6329](https://github.com/hivecommons/hive/issues/6329),
rewriting the v4→v5 sync branch rebased 107 commits by other authors, destroyed
merge ancestry, and silently dropped work.

**Enforcement point.** The `src/pkg/proxy` GitHub push gate should treat
`POST */git-receive-pack` as a branch-owner checked write before forwarding it.
The request watchers in `src/pkg/github` should keep their App-token path
append-only for Hive-created branch updates. `src/policies/*-holdgated.md` and
`*-full.md` should state that agents never force-push shared or sync branches.

**Detection signal.** A push from an agent App identity moves a protected shared
or sync branch to a head that is not a descendant of the previous remote head, or
updates a branch whose owner/creator metadata is missing or names another lane.

**Incentive fix.** Remove the standing reason to rewrite by fixing the DCO
pressure tracked in [#6254](https://github.com/hivecommons/hive/issues/6254)
and [#6312](https://github.com/hivecommons/hive/issues/6312), so a red gate does
not make history surgery the locally rational action.

## Invariant 2 — no sign-off attribution on others' commits

**Statement.** The App write gate refuses pushes where an agent identity appends
`Signed-off-by` lines to commits it did not author. DCO remediation is either
append-only (new commits by the remediator) or human-performed.

**Rationale.** DCO is an attribution and attestation gate. An agent adding its
own sign-off to someone else's commit forges that attestation, even when the
result makes the check green. [#6329](https://github.com/hivecommons/hive/issues/6329)
showed the failure mode: forged sign-offs made Prow accept a history rewrite as
DCO-clean.

**Enforcement point.** The `src/pkg/proxy` push path should inspect new commits
introduced by agent `git-receive-pack` writes before forwarding. The
`src/pkg/github` App-token branch/PR path should reject relayed requests whose
commit trailers newly claim an agent sign-off on a non-agent-authored commit.
The `src/policies` lane templates should direct agents to open a remediation PR
or escalate to a human instead of amending other authors' commits.

**Detection signal.** A commit author is not the pushing agent identity, but the
pushed object adds or changes a `Signed-off-by` trailer for that agent identity;
or a pre-existing commit hash changes only to add sign-off trailers.

**Incentive fix.** Fix the App-bot/DCO mismatch in [#6254](https://github.com/hivecommons/hive/issues/6254)
and the Tide squash sign-off regression in [#6312](https://github.com/hivecommons/hive/issues/6312)
so agents are not rewarded for forging attestations to clear permanently red
checks.

## Invariant 3 — gate manipulation demotes the lane

**Statement.** Gate manipulation is an ACMM regression trigger. A lane that
strips `lgtm`, flips DCO by rewrite, or otherwise changes gate evidence instead
of satisfying the gate is demoted to hold-gated until human review restores it.

**Rationale.** Trust gates certify only the state they actually observe. If an
agent can clear them by pushing empty retriggers, rewriting commits, or removing
review labels, the ACMM level overstates autonomy safety. [#6300](https://github.com/hivecommons/hive/issues/6300)
showed the Tide case: empty retrigger commits stripped `lgtm` from already-green,
approved PRs and ejected them from the queue.

**Enforcement point.** The `src/pkg/proxy` write gate and `src/pkg/github`
request watchers should emit a machine-readable regression event when they block
or observe gate manipulation. ACMM reconciliation should consume that event and
move the lane to hold-gated. The policy files in `src/policies` should make the
fallback explicit: when a gate is red for systemic reasons, escalate or file a
separate fix; do not manipulate the gate's inputs.

**Detection signal.** A push by an agent identity removes or invalidates a human
review gate (`lgtm`, approval, required check context, or DCO status) without a
substantive code change that addresses the failing condition; repeated empty
retrigger commits on approved PRs; or rewritten commits whose only material
change is gate metadata.

**Incentive fix.** Close [#6254](https://github.com/hivecommons/hive/issues/6254)
and [#6312](https://github.com/hivecommons/hive/issues/6312) so lanes have an
append-only path back to green. Until then, systemic DCO failures should be a
human or maintainer remediation queue, not an invitation for autonomous rewrite.
