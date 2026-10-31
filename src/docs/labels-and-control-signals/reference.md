# Hive Labels and Control Signals: Labels reference: axes, categories and reference table

Part of [Hive Labels and Control Signals](../labels-and-control-signals.md).

## First use: choose the control

Start from the item's current reason, not a stage number. In the dashboard, open the row's **⋯** menu; on GitHub, inspect its labels and latest explanation comment. Dashboard writes require the role allowed by that action; having a label visible does not grant permission to remove it.

| Your task | Control and expected result | If it does not move |
| --- | --- | --- |
| Pause an issue or stop a PR merging | Use the dashboard hold or a matching hold label. Held work leaves normal agent/merge queues; a held red PR can still get CI repair without releasing the hold. | Check every hold label, including the Hive-scoped `hive-pause/<hive-id>`. |
| Resume held work | Release the hold only when its reason is resolved and you have authority to do so. | A new head may trigger holdguard and require fresh human approval. Do not have an agent remove a person's hold. |
| Wait for another PR or outside blocker | Inspect `blocked` and the linked blocker/deadline. A blocked issue is excluded from agent enumeration and automated claims, not merely coloured differently. | Follow the [blocked reference](#blocked); human-applied/re-applied blocks are not automatically lifted. |
| Admit an untrusted reporter's issue | Review it, then apply the configured acceptance label (default `triage/accepted`) or change the reporter-trust policy as an operator. | The ordinary required-label filter still applies after reporter triage. |
| Answer a direction or design question | Use the offered decision action or the exact `/hive decision` form in the reference. | Re-read the park reason; a hold, access restriction or owner-only question is not bypassed by a stage edit. |
| Find who repairs a red PR | Read `author/*`: Hive/self-authored → 070; human → 071; contributor agent → 072. | Use [Stage 07x by author kind](stage-labels.md#stage-07x-by-author-kind), not the trust-role label, to find the responsible party. |
| Understand a Projects band | Treat it as a display group, not permission to claim or merge. | Check the actual holds, review, CI, trust and repository policy gates. |

After an action, check the resulting label/stage, latest comment and dashboard result. A recorded approval is not proof of a merge. See [Dashboard sections explained](../dashboard-sections.md#hive-activity-monitor) for row actions and their receipts.

### The ordinary path

An admitted issue becomes **010 Actionable**, then **020 Claimed** when someone takes it. Work moves through **030 PR in progress** / **040 Covered by PR** to **050 CI / Review**. A reviewed PR still has to enter the merge gate; **095 Soaking** means admitted but waiting, and **100 Merge queue** means admitted and merging. Human decisions, repair, holds and drafts can interrupt this path. Changing a stage label is not a way around those checks.

A **stage** is the current step; a **lane** is the worker or processing path; the **head** is the PR's current tip commit. A review **verdict** is a result, **dwell** is time at a step, and **soak** is a pre-merge waiting period. A review **swarm** combines reviewers. **Adoption** means Hive takes responsibility for contributor-agent work; **re-home** means continuing it in a replacement PR, preserving authorship. A **ratchet** permits the old baseline but rejects increases. See the [reader glossary](../dashboard-glossary.md#terms).

### Find the precise rule

- [Holds and pause controls](#holds-and-pause-controls)
- [Issue admission and triage](#issue-admission-and-triage)
- [PR review and merge controls](#pr-review-and-merge-controls)
- [Contributor routing and other signals](#contributor-routing-and-other-signals)
- [Retired names and compatibility signals](#retired-names-and-compatibility-signals)
- [Stage derivation](stage-labels.md#stage-labels-stagennn--the-pipeline-state-machine), [matching rules](glossary-and-operations.md#matching-rules) and [implementation trace](glossary-and-operations.md#how-labels-reach-agents)

Policy names matter more than issue numbers. **Self-authorization hold** (#5117) requires acknowledgment of Hive-filed rationale when enabled. **Reporter-trust hold** (#9665) concerns the linked issue's reporter, not the PR author's trust. **Hive-owned suppress reconciliation** (#12002) can lift only eligible Hive-applied holds/blocks; it is not permission to release human-owned controls.

## Label axes

Hive-authored labels answer different questions. Read them as independent axes, not as one taxonomy: an issue can carry one label from each axis at the same time, and swapping a label on one axis never changes what the others mean.

| Axis | Shape | Question it answers | Signal? | Notes |
| --- | --- | --- | --- | --- |
| **Stage** | `stage/<step>` | Where is this item in the pipeline? | Control (read by claims/auto-merge) | See [Stage labels](stage-labels.md#stage-labels-stagennn--the-pipeline-state-machine) below. |
| **Agent** | `agent/<lane>` | Which agent works it and, for agent-filed issues, which lane filed it? | Routing + ownership + lane provenance | The suffix is always the lane name from `HIVE_AGENT`. The classifier routes on it, PR ownership compares against it, and `authorkind` treats it as this hive's author stamp. Agent-filed issues carry exactly one lane/provenance label: `agent/<lane>`. |
| **Origin** | `origin/<source>` | Which non-lane hive component created this item? | Provenance only, never a gate | Reserved for hive filers that have no `agent/<lane>` equivalent: `origin/selfreport`, `origin/governor`, `origin/dashboard`, `origin/forge`, and `origin/convergence`. Agent lanes do not use `origin/<lane>`; the startup reconciler (`Client.MigrateOriginLabels`, `src/pkg/github/origin_labels.go`) removes redundant lane-origin and bare-lane labels while ensuring `agent/<lane>`. `origin/reviewer`, `origin/review`, and `from-review` are retired reviewer-lane spellings and map to `agent/reviewer`. |
| **Role** | `role/<tier>` | How far does the hive trust the author's login? | Control (auto-merge) | Was `author/<tier>` before #12581; the reconciler rewrites the old spelling. See [`role/*`](stage-labels.md#role--would-the-hive-auto-merge-this). |
| **Author** | `author/<kind>` | Who wrote it — a person, a contributor's clanker, or this repo's hive? | Informational (gates the 072 nudge/adoption) | `author/human`, `author/clanker`, `author/hive`; names configurable under `governor.labels.author`. See [`author/*`](stage-labels.md#author--who-wrote-this-pr). |
| **Modifiers** | `blocked`, `hold`, `needs-*`, `hive-pause/<hive-id>` | What is stopping it right now? | Hold / suppress | See the reference table; these are the hard gates. |

### Origin sources

The `<source>` is the component that created the issue, never the person or hive that operates it. Agent lanes are not origin sources: an agent filing an issue gets `agent/<lane>` instead, and a bare `<lane>` label requested by that same agent is stripped as duplicate provenance. A caller-supplied non-lane `origin/*` label is kept and canonicalised; retired lane-origin spellings are mapped to `agent/<lane>`.

| `origin/<source>` | Created by | Code path |
| --- | --- | --- |
| `origin/selfreport` | Fleet runtime selfreports (`[selfreport] …`), filed directly or relayed by the hub. Sits next to the deprecated `hive/selfreport` for now. | `src/pkg/github/fleet_report.go`, `src/pkg/fleetreport/runtime.go`, `src/pkg/hub/selfreport_relay.go` |
| `origin/governor` | Governor automation: the advisory meta issue, the recommendations issue, upstream-watch filings. Task-list sweeps, stage regress and unpark notices do not file issues today; if they ever do, they use this source. | `src/pkg/github/advisory.go`, `src/pkg/github/recommendations.go`, `src/pkg/upstreamwatch/render.go` |
| `origin/dashboard` | Operator actions in the SPA that file an issue (ACMM eval filing). | `src/pkg/dashboard/api_acmm_eval.go` |
| `origin/forge` | The forge issue seam, when its consumer passes no origin label of its own. | `src/pkg/forge/github_forge.go` |
| `origin/convergence` | The convergence publisher and its GitHub channel. | `src/pkg/convergence/publish/publisher.go`, `src/pkg/convergence/publish/channel.go` |
| *(none)* | A human filing directly. **`origin/human` is deliberately never applied**: the absence of an origin label is what "a person filed this" looks like, and `author/*` already says who wrote a PR. | — |

Work-source sinks other than GitHub (Jira, Linear) carry no GitHub labels and are outside the axis.

Some hive-authored labels look like provenance but are read as signals, so they stay outside `origin/*` on purpose: `hive/selfreport` is the dedupe key the self-report joiner searches (`label:hive/selfreport` in `src/pkg/github/fleet_report.go`) and the hub relay re-applies it — it is **deprecated** in favour of `origin/selfreport`, which every new selfreport also carries, and will be removed once the joiner searches the origin label instead; `hive/advisory` makes an issue a standing meta issue that enumeration excludes (`src/pkg/github/standing_issues.go`); `hive/<hive-id>` is **retired** (#12539): nothing writes it any more, the origin marker in the body says which hive wrote an item, and the boot sweep strips leftovers (see the reference row). `hive/sentinel`, `hive/fleet` and `fleet` are only consulted by the good-first-issue exclusion list (`src/pkg/gfi/gfi.go`) and are no longer written by this code base.

## Categories

- **Informational / display-only** changes how Hive explains an item, not whether agents can act.
- **Workflow / status** marks a process state such as reviewer outcome or rebase need.
- **Gate / permit** admits, blocks, or releases a specific workflow.
- **Hold / suppress** removes work from agent/contributor/merge lanes until cleared.
- **Contributor eligibility / routing** affects `/contribute` offers or agent lane choice.
- **Human approval / acknowledgement** records that a human accepted a direction or design.

## Reference table

This reference replaces the former nine-column table with one headed entry per control. Each entry gives the effect first, then who applies it, when it is checked, how it is released and its exact configuration. Current controls are grouped by task; retired spellings are separated at the end.

### Holds and pause controls

#### `hold`, `on-hold`, `hold/review`

**Applies to:** Issues, PRs. **Kind:** Hard hold.

Any label containing a configured hold substring is held; held issues/PRs leave agent kicks, PR merge lanes, and the task-list sweep. Held red PRs still route back to their owning agent for CI repair, with instructions not to remove the hold.

**Who applies it:** Human, Hive level gate, #5117 gate, holdguard, SHA-hold.

**Checked by:** GitHub enumeration, auto-merge sweeps, task-list sweep. **When:** Enumeration and sweeps.

**Release or override.** Remove the matching label; Hive releases only literal `hold` it applied itself (level gate, #5117, SHA-hold)

**Configuration and implementation.** Built-in `HoldLabels`; dashboard exact hold via `Client.SetHoldLabels` (`src/pkg/github/client.go:1317`, `src/pkg/github/client.go:3314-3369`, `src/pkg/scheduler/policy_overlays.go:210-220`)

#### `hive-pause/<hive-id>`

**Applies to:** Issues, PRs. **Kind:** Hard hold.

Exact, hive-scoped dashboard hold. It is not provenance and intentionally avoids the substring `hold`.

**Who applies it:** Dashboard operator.

**Checked by:** GitHub enumeration, dashboard hold toggle, auto-merge sweeps, task-list sweep. **When:** Enumeration and sweeps.

**Release or override.** Dashboard Release removes labels causing the hold, including this one

**Configuration and implementation.** Canonicalized from hive id (`src/pkg/github/client.go:3371-3380`, `src/pkg/github/client.go:3386-3392`, `src/docs/dashboard.md:80-96`)

#### `hold` from ACMM level gate

**Applies to:** PRs. **Kind:** Hard merge gate.

Agent PRs at L3-L5 get literal `hold`; `outreach` PRs whose target repo is not one the hive manages (cross-repo outreach) are held at every level, while same-repo outreach PRs follow the level. The watcher, not the policy prompt, applies it from the PR's target repo (`config.AgentOwnsRepo`).

**Who applies it:** Hive App.

**Checked by:** PR-request watcher, self-authored auto-merge release. **When:** PR creation, later self-authored auto-merge release.

**Release or override.** Auto-release only when current policy no longer requires the level hold, latest hold event was by the App, and the release path runs; otherwise a human removes it

**Configuration and implementation.** Hive-wide ACMM and agent (`src/pkg/github/pr_request_watcher.go:452-524`, `src/pkg/github/pr_level_hold.go:16-124`)

#### `hold` from #5117 self-authorization

**Applies to:** PRs. **Kind:** Hard merge gate.

A PR whose only rationale is unacknowledged hive-filed issues gets literal `hold`. Acknowledging the issue later does not remove an existing PR hold by itself. At ACMM L6 (Autonomous), the unset default is off; lower levels default on.

**Who applies it:** Hive App.

**Checked by:** PR-request watcher and self-authorization release. **When:** PR creation; evaluated only when no level hold applies.

**Release or override.** Acknowledge the issue and remove the PR `hold`; disabled policy release can remove Hive's own hold. At L6, that release path also clears existing Hive-applied #5117 holds when no explicit config keeps the policy on.

**Configuration and implementation.** `github.self_authorization_hold`, per-repo `project.repo_policies[].self_authorization_hold`, `HIVE_SELF_AUTHORIZATION_HOLD` (`src/pkg/github/pr_self_authorization.go:14-226`, `src/pkg/github/pr_request_watcher.go:452-524`)

#### `hold` from #9665 reporter trust

**Applies to:** PRs **not authored by the hive**. **Kind:** Hard merge gate.

A PR whose rationale traces to an issue filed by an untrusted reporter gets literal `hold` at **every** ACMM level, including L6, plus `needs-human` (so it surfaces as waiting on a person, #10773) and a marked notice (`<!-- hive:reporter-trust-hold:9665 -->` + `<!-- hive:reporter-trust-metadata {…} -->`) naming the untrusted reporter and stating that the hold concerns the issue reporter, not the PR author. The rationale is **only the linked issues**: closing-keyword links (`Fixes`/`Closes`/`Resolves`) and the request's declared issues — bare `#N` mentions are not consulted, and a link that resolves to a pull request is skipped rather than judged (hivecommons/hive#15020: `Refs #15009 (closed by #15017)` made the gate judge sibling PR #15017 and attribute a `CONTRIBUTOR` who had merely commented).

A hive-filed issue (`[selfreport]`, app or AI author) counts as trusted. The gate is **skipped entirely for hive-authored PRs** (App bot, `project.ai_author`, `author/hive`): the hive is the author, so the reporter's trust says nothing about whether its own fix may merge; it still applies to contributor PRs and to PRs opened under a shared PAT account. Any one untrusted citation holds. Evaluated even at hold-gated levels, because the notice is what stops promotion to L6 from releasing the label.

**Who applies it:** Hive App.

**Checked by:** PR-request watcher, level-hold release. **When:** PR creation; re-checked by the level-hold release path.

**Release or override.** A human removes `hold` on a contributor PR; Hive never auto-releases it there. On a **hive-authored** PR still wearing a hive-set reporter-trust hold (newest `hold` event labeled by the App bot with the notice at or after it — a person's hold is left alone), the stage-label pass releases `hold` (and `needs-human` when the notice says the gate added it), posts one `<!-- hive:reporter-trust-release -->` comment and logs `stage labels: releasing self-imposed reporter-trust hold`.

The pass also records the hold's provenance (`by`, `source`, `reason`, `since`) on the stage store, so the tracker pill reads *Held by hive · reporter trust (issue #N by @X, contributor)* and the 085 comment names who set the hold and why. Holdguard re-holds on a new head SHA.

**Configuration and implementation.** `github.reporter_trust_hold` (nil follows `reporter_trust.enabled`), per-repo `project.repo_policies[].reporter_trust_hold`, `HIVE_REPORTER_TRUST_HOLD` (`src/pkg/github/pr_reporter_trust.go`, `src/pkg/github/pr_level_hold.go`, `src/pkg/github/automerge/reporter_trust_release.go`)

#### `hold` from holdguard

**Applies to:** PRs. **Kind:** Hard merge gate.

If the head SHA changes while held, lifting the hold causes Hive to comment and re-apply literal `hold`; the next human removal is the fresh approval. Exception (#13603): when a human (not a Hive account, not a bot) removed the gate label after the current head arrived on the branch, they reviewed the current head, so the lift clears without a comment or re-hold. The head is accepted only on evidence a contributor cannot forge: the server-stamped `head_ref_force_pushed` timeline event that produced it (dated before the lift), or the hive itself having observed that exact head on the PR while the gate was still on (`observed_held_head_sha` in the holdguard ledger).

Commit committer dates and SHA-keyed `check_suite` / `status` times are never used; a push after the lift, or a head the hive never saw while held, still re-holds.

**Who applies it:** Hive.

**Checked by:** Holdguard ledger. **When:** Governor holdguard pass.

**Release or override.** Human removes the re-applied hold

**Configuration and implementation.** Built-in `holdguard.ReHoldLabel` (`src/pkg/holdguard/holdguard.go:1-43`)

#### `hold` from SHA-hold

**Applies to:** Issues. **Kind:** Hard issue hold.

Human-filed primary-repo `kind/bug` without a 7-40 hex SHA gets `hold` and a notice carrying `<!-- hive:sha-hold -->`; once a SHA appears in body/comments, Hive removes `hold` only if the current hold is its own (newest `hold` event labeled by the App bot, paired with an App-authored notice at or after it). A human's hold, a re-hold, or another subsystem's hold stays. Reporters whose author association is trusted (`issue_filter.reporter_trust.trusted_associations`, default OWNER/MEMBER/COLLABORATOR) get the soft `needs-sha` row below instead, and a SHA-hold `hold` already on such an issue is swapped for it (#12002).

The hard-suppress reconcile also lifts this `hold` once a PR referencing the issue merges after it was applied.

**Who applies it:** Hive.

**Checked by:** SHA-hold sweep. **When:** Eval tick SHA sweep; hard-suppress reconcile.

**Release or override.** Add SHA evidence or merge the fix; sweep removes its own `hold`, a human removes any other

**Configuration and implementation.** Primary repo and SHA-hold config (`src/pkg/github/client.go` `EnforceSHAHold`, `src/pkg/github/hard_suppress_reconcile.go`)

#### `needs-sha` from SHA-hold

**Applies to:** Issues. **Kind:** Soft (informational).

Trusted-reporter `kind/bug` without a SHA gets this soft label plus the same `<!-- hive:sha-hold -->` notice instead of `hold`; the issue stays actionable. The sweep removes it once a SHA appears.

**Who applies it:** Hive.

**Checked by:** SHA-hold sweep. **When:** Eval tick SHA sweep.

**Release or override.** Add SHA evidence, or remove the label

**Configuration and implementation.** `issues.sha_hold_trusted_label` (default `needs-sha`; `hold` restores the hard hold for everyone)

### Issue admission and triage

#### GitHub "blocked by" dependency

**Applies to:** Issues. **Kind:** Soft hold.

An open issue whose GitHub dependency list names an open blocker is enumerated with `DependsOn` edges and kept out of the ACTIONABLE ISSUES an agent picks from; the kick names it once in a `Blocked by open dependencies` footer with its blockers. A blocker that is closed resolves the edge; a mutual pair (A↔B) is dropped with a warning so neither hides the other. The blocker list failing to load fails open (issue offered).

**Who applies it:** Human (GitHub UI) or agent via `hive-open-issue --blocked-by` when splitting ordered children.

**Checked by:** GitHub enumeration, kick-list assembly. **When:** Enumeration.

**Release or override.** Close the blocker, or remove the dependency on GitHub

**Configuration and implementation.** None (`src/pkg/github/issue_dependencies.go`, `src/pkg/scheduler/kickmessage.go` `partitionBlockedIssues`/`formatBlockedIssuesNote`)

#### `blocked`

**Applies to:** Issues. **Kind:** Hard suppress.

The issue waits on an unmerged PR (or another outside blocker): the review backlog files a finding `blocked` when its evidence file exists only on the reviewed PR (absent on the base branch), and `hive-open-issue comment --blocked-on-pr <n>` parks an existing one; both write `<!-- hive:blocked-on pr=owner/repo#N -->`. A `blocked` issue is filtered (reason `blocked on owner/repo#N`), derives no stage label, is refused by every automated claim producer (`blocked: owner/repo#N`) including the contributor relay, and never carries `help wanted` ([#13093](https://github.com/hivecommons/hive/issues/13093)).

When a blocked filing folds into an already-open twin, the twin's own state governs: `blocked` is not applied to it, because the folded body (and its marker) is discarded and the twin's earlier evidence stands on its own; the blocked sweep also lifts a hive-applied `blocked` that has no marker, no deadline and no ops-tracking marker, leaving a human-applied one alone ([#13364](https://github.com/hivecommons/hive/issues/13364)). With no blocked-on marker, a body deadline `<!-- hive:recheck-after <RFC3339> -->` files it `blocked until <RFC3339>` and the activity monitor's blocked chip reads `re-check at <t>` ([#13310](https://github.com/hivecommons/hive/issues/13310)).

With the marker, the activity monitor's pill reads `blocked on #N`, links the PR and carries its cached state (`blocked_on_pr` in `/api/stage-tracker`), and the hive's own removal of the label re-reads the issue live rather than waiting for the next enumeration ([#14952](https://github.com/hivecommons/hive/issues/14952)). It is also always skipped by contributor admission and shown in the default waiting band.

**Who applies it:** Hive (reserved for agents; relay only).

**Checked by:** Enumeration, stage derivation, claim gate, review backlog, blocked sweep. **When:** Review backlog filing; blocked-on comment relay.

**Release or override.** The blocked sweep after each stage-label sync removes it, with one `<!-- hive:unblocked -->` note, once the tracked PR is merged or closed, or once a recheck-after deadline passes (`deadline <t> passed; re-triaging`) unless the body also carries `<!-- hive:ops-tracking -->`. An ops-tracking issue on the hive's own project keeps `blocked` once its deadline passes and instead gets soak triage: the hub's `/api/hub/hives` (or, with no hub, the spoke's own `/api/version`) is probed for every `stable`-channel hive (fresh heartbeat, no restart loop, running the candidate `sha256:` digest or `v5 <short-sha>` named in the body), ONE `<!-- hive:recheck-evidence at=<RFC3339> -->` comment is posted, only the checklist lines that evidence proves are ticked, and `<!-- hive:recheck-done -->` is appended to the body so the sweep does not repeat ([#13311](https://github.com/hivecommons/hive/issues/13311)); a human may remove it at any time.

The hard-suppress reconcile (#12002) can also lift a Hive-applied `blocked` after a referencing PR merges, but only when the newest label event was by the App bot; human-applied or re-applied labels are not lifted. It posts `<!-- hive:hard-suppress-lifted -->`.

**Configuration and implementation.** Fixed label (`src/pkg/github/issue_blocked.go`, `src/pkg/github/issue_recheck_soak.go`, `src/pkg/github/issue_stage_labels.go`, `src/pkg/github/issue_claim_gate.go`); contributor/dashboard controls: `hub.contribute_skip_labels`, dashboard bands (`src/pkg/config/config.go:4585-4685`, `src/docs/dashboard.md:105-140`)

#### `needs-human`

**Applies to:** Issues. **Kind:** Hard suppress.

Issue is filtered, not held: no agent kick or contributor offer; task-list sweep adds it only when a merged PR carries the explicit `Refs #N (needs-human: <reason>)` marker (never on a held issue; remainder prose yields `hive/handoff` instead, [#12002](https://github.com/hivecommons/hive/issues/12002)), and the claim escalation gate adds it instead of a third no-progress claim by the same agent (`governor.claims.escalate_after_claims`, [#10527](https://github.com/hivecommons/hive/issues/10527)). The un-park sweep keeps a "What to reply" comment on every parked issue listing the commands.

See [Maintainer commands](../maintainer-commands.md).

**Who applies it:** Hive or human.

**Checked by:** Enumeration, task-list sweep, claim escalation gate and un-park sweep. **When:** Enumeration/task-list sweep/kick building.

**Release or override.** A person can remove the label directly. On the ordinary issue un-park path, an eligible maintainer's `/hive approve` / `/hive decision <text>` clears `needs-human` **regardless of who applied it**, including a person or an unknown actor (`stageengine.DecisionLiftsNeedsHuman`). The command is the human answer the label requested. It also removes `stage/045-needs-decision` and retired park spellings (including an issue's pre-045 `stage/085-needs-decision`), adds `approved-direction`, and lets the engine derive 010 when no other gate remains.

The maintainer must have write, maintain or admin repository permission. The label timeline attributes the reply; it does not veto an otherwise eligible command. Leaving `needs-human` behind would re-derive 045 and undo the un-park ([#13075](https://github.com/hivecommons/hive/issues/13075), seen on [#12984](https://github.com/hivecommons/hive/issues/12984)). Earlier provenance-sensitive descriptions ([#12680](https://github.com/hivecommons/hive/issues/12680), [#12681](https://github.com/hivecommons/hive/issues/12681)) do not describe this current command rule.

**Keep or add `hold` if work must stay stopped after the decision.** These commands never remove it. Label-driven repositories use their separate configured approval/lifecycle policy: `unparkLabelDrivenCommand` removes only Hive-applied park labels, including `needs-human`; ordinary un-park commands do not bypass that route's ownership rules.

Agents may ADD it through the relay `label` command when an owner's direction reserves the work for people (then `/unclaim`); they may never remove it (`src/pkg/github/issue_unpark_command.go`, `src/pkg/github/issue_request_label.go`)

**Configuration and implementation.** Fixed label (`src/pkg/github/client.go:1798-1803`, `src/pkg/github/task_list_sweep.go:611-641`, `src/pkg/github/task_list_sweep.go:874-880`, `src/cmd/hive/claimescalation.go`)

#### `needs-spec` from the clarity gate

**Applies to:** Issues. **Kind:** Hard suppress.

With the gate on (default off), a human-filed, unclaimed issue whose clarity score (goal, acceptance criteria, scope, detail, open choices) is under `runs.triage.clarity_gate.threshold` gets one `<!-- hive:clarity-gate -->` comment with concise questions for its author and the `needs-spec` hard-suppress label instead of a claim; it derives `stage/045-needs-decision` and leaves the agent queue ([#13577](https://github.com/hivecommons/hive/issues/13577)).

**Who applies it:** Hive.

**Checked by:** Scheduler kick assembly (`runs.triage.clarity_gate`). **When:** Before kicks are built.

**Release or override.** The issue author, or a non-hive commenter with write/maintain/admin, replies after the marker and the resume sweep removes the hive's own `needs-spec` and records the un-park so the issue re-derives `stage/010-actionable` ([#13588](https://github.com/hivecommons/hive/issues/13588)); a `needs-spec` a person added is never lifted by the sweep. Or remove `needs-spec` by hand. The marker keeps the gate from asking again

**Configuration and implementation.** `runs.triage.clarity_gate` (`src/pkg/scheduler/clarity_gate.go`, `src/pkg/classify/clarity.go`, `src/pkg/github/clarity_gate_resume.go`)

#### `stage/017-possible-existing` from the prior-art gate

**Applies to:** Issues. **Kind:** Hard suppress.

With the gate on (default on), an unclaimed feature / RFC / epic issue whose key terms match an existing knowledge bead, setting, merged PR, closed issue, doc or code identifier at `runs.triage.prior_art.threshold` (default 70) gets one `<!-- hive:prior-art-gate -->` comment quoting the findings and the `stage/017-possible-existing` label instead of a claim; hive-filed issues get a `<!-- hive:prior-art-findings -->` note and are never parked ([#13575](https://github.com/hivecommons/hive/issues/13575)).

**Who applies it:** Hive.

**Checked by:** Scheduler kick assembly (`runs.triage.prior_art`). **When:** Before kicks are built.

**Release or override.** The author or a write+ commenter replies `/hive decision proceed — <gap>` (label lifted, a *Scope vs existing* section appended to the body, re-derives 010) or `/hive decision exists` (closed `not_planned` with `already-exists`); a plain reply lifts the label too. A clanker-filed park with no reply is closed `already-exists` after `runs.triage.prior_art.bot_close_ttl` (default 72h); reopen to continue. A 017 a person added is never lifted by the sweep

**Configuration and implementation.** `runs.triage.prior_art` (`src/pkg/priorart`, `src/pkg/scheduler/prior_art_gate.go`, `src/pkg/github/prior_art_gate_resume.go`)

#### `hive/handoff`

**Applies to:** Issues. **Kind:** Informational.

Soft hand-off marker. When a merged PR's `Refs #N` remainder section says in prose that the rest needs a person or another lane ("needs a human", "fix lane", "quality lane", "production change", …) without the explicit `needs-human:` marker, the sweep adds this label and notes "handed off to the next lane" in its remainder comment instead of parking the issue. It is not a hold, suppress, or enumeration filter: the issue stays actionable, so the next lane can still claim it ([#12002](https://github.com/hivecommons/hive/issues/12002)).

Not added when the same sweep is applying `needs-human`.

**Who applies it:** Hive (task-list sweep).

**Checked by:** Task-list sweep. **When:** Task-list sweep.

**Release or override.** Human or the claiming lane removes it; Hive never removes it

**Configuration and implementation.** Fixed label (`src/pkg/github/task_list_sweep.go:85`, `src/pkg/github/task_list_sweep.go:576-596`, `src/pkg/github/task_list_sweep.go:635-665`)

#### `hive/advisory`

**Applies to:** Issues. **Kind:** Hard exclusion.

Hive advisory report is never actionable and never appears on the Hold list.

**Who applies it:** Hive.

**Checked by:** Standing meta classifier. **When:** Before holds/exempts.

**Release or override.** Remove label, but exact advisory title still excludes

**Configuration and implementation.** Fixed label/name (`src/pkg/github/standing_issues.go:1-57`, `src/pkg/github/client.go:1726-1744`)

#### `triage/advisory`

**Applies to:** Issues. **Kind:** Hard exclusion.

Advisory-only dedupe checklist (`Decision required: no`, #11864) is never actionable and never appears on the Hold list; action-required checklists are parked with `needs-decision` instead. The close-on-merge backfill closes it once every `#N` it references is closed or merged, or after 7 days, and swaps it for `needs-decision` plus the "What to reply" notice if line two turns to `Decision required: yes` (#11871).

**Who applies it:** Dedupe agent.

**Checked by:** Standing meta classifier. **When:** Before holds/exempts.

**Release or override.** Remove label

**Configuration and implementation.** Fixed label/name (`src/pkg/github/standing_issues.go`)

#### Human assignee

**Applies to:** Issues. **Kind:** Non-label approval.

Human assignee acknowledges hive-filed direction and moves issue into rank tier 2.

**Who applies it:** Human.

**Checked by:** #5117 gate and ranking. **When:** PR creation/ranking.

**Release or override.** Unassign

**Configuration and implementation.** N/A (`src/pkg/github/pr_self_authorization.go:180-226`, `src/pkg/github/client.go:4748-4777`)

#### Human comment

**Applies to:** Issues. **Kind:** Non-label approval.

Any human comment in the first 100 issue comments acknowledges for #5117, but does not change ranking tier by itself.

**Who applies it:** Human.

**Checked by:** #5117 gate. **When:** PR creation.

**Release or override.** N/A

**Configuration and implementation.** `selfAuthCommentPageSize` (`src/pkg/github/pr_self_authorization.go:14-18`, `src/pkg/github/pr_self_authorization.go:180-226`)

#### `needs-reporter-confirmation`

**Applies to:** Issues. **Kind:** Hard suppress / human signal.

Marks an issue whose opted-in fix appears to have landed but still needs the reporter or a maintainer to reply `/fixed`. The `CloseIssue` reporter-confirmation gate applies it only when `hive: needs-confirmation` or `issues.reporter_confirmation` requested the gate; if the reporter has write/maintain/admin access Hive also adds `needs-human` so the dashboard's human queue catches it.

**Who applies it:** Hive App.

**Checked by:** Issue-close gate, dashboard. **When:** Enumeration/dashboard.

**Release or override.** `/fixed`, deliberate close, or `/reopen` removes this label. Hive does not remove `needs-human` here because it may have been set by another workflow.

**Configuration and implementation.** Fixed label (`src/pkg/github/issue_close.go`, `src/scripts/issue-confirm-fixed.sh`)

#### `hive: needs-confirmation` label/body text

**Applies to:** Issues. **Kind:** Gate input.

Opts one human-filed bug-family issue into reporter verification before closing. PR bodies keep `Closes`/`Fixes`; the close path leaves the issue open pending confirmation until `/fixed`, `hive: reporter-confirmed`, an override, or marker removal.

**Who applies it:** Human/filer/operator.

**Checked by:** Issue-close gate. **When:** `CloseIssue`.

**Release or override.** Remove label/text or confirm fixed

**Configuration and implementation.** Fixed phrase (`src/pkg/github/pr_request_claims.go`, `src/pkg/github/issue_close.go`)

#### `hive: reporter-confirmed` label/body text

**Applies to:** Issues. **Kind:** Permit.

Legacy marker showing the reporter/maintainer verified the fix. Close-on-merge is now the default, so this is usually redundant; it still bypasses an active `hive: needs-confirmation` or `issues.reporter_confirmation` gate.

**Who applies it:** Human/reporter.

**Checked by:** Issue-close gate. **When:** `CloseIssue`, `/fixed` workflow.

**Release or override.** Remove label/text

**Configuration and implementation.** Fixed phrase (`src/pkg/github/pr_request_claims.go`, `src/pkg/github/issue_close.go`)

#### `hive: close-on-merge` label/body text

**Applies to:** Issues. **Kind:** Permit.

Legacy filing-time marker for bugs whose merged fix is the verification. Close-on-merge is now the default, so this is usually redundant; it still bypasses an active reporter-confirmation config gate. `hive-open-issue --close-on-merge` still adds it for compatibility.

**Who applies it:** Human/filer.

**Checked by:** Issue-close gate. **When:** `CloseIssue`.

**Release or override.** Remove label/text

**Configuration and implementation.** Fixed phrase (`src/pkg/github/pr_request_claims.go`, `src/pkg/github/issue_close.go`)

#### `design-approved`

**Applies to:** Issues. **Kind:** Planning gate.

Approves a requested architect design; counts after a design exists.

**Who applies it:** Human.

**Checked by:** Planning design gate. **When:** Planning label sweep.

**Release or override.** Remove/re-apply design labels as needed

**Configuration and implementation.** `planning.design_approved_label` (`src/pkg/planning/design.go:68-125`, `src/pkg/planning/design.go:320-345`)

#### Lane-name label or `agent/<role>` segment

**Applies to:** Issues. **Kind:** Soft routing.

Routes issue to a lane after title prefix and before keyword routing. Scanner sees every issue; other agents see their lane.

**Who applies it:** Hive/human.

**Checked by:** Classifier/scheduler. **When:** Classification before kicks.

**Release or override.** Change/remove label

**Configuration and implementation.** `agents.*.lane_keywords` (`src/pkg/classify/classifier.go:249-276`, `src/pkg/scheduler/templates.go:298-303`)

#### `auto-qa`, `auto-qa-finding`, `kind/security`, `kind/regression`

**Applies to:** Issues. **Kind:** Soft classification.

`auto-qa`/`auto-qa-finding` make Simple; `kind/security`/`kind/regression` make Complex.

**Who applies it:** Hive/human.

**Checked by:** Classifier. **When:** Classification.

**Release or override.** Remove label

**Configuration and implementation.** Classifier tables (`src/pkg/classify/classifier.go:279-303`)

#### `run/spec`, `run/fix`

**Applies to:** Issues. **Kind:** Gate when enabled.

Forces spec or direct-fix triage when runs triage is enabled.

**Who applies it:** Human/Hive.

**Checked by:** Runs triage. **When:** Triage.

**Release or override.** Remove label

**Configuration and implementation.** `runs.triage.enabled` (`src/pkg/classify/triage.go:1-55`)

#### `runs.triage.spec_labels` / `fix_labels` (v6/edge Spek)

**Applies to:** Issues. **Kind:** Gate when enabled.

Configured exact labels route to spec or fix; mark this v6/edge when documenting Spek behavior.

**Who applies it:** Operator/human.

**Checked by:** Spek/runs triage. **When:** Triage.

**Release or override.** Remove label/config

**Configuration and implementation.** `runs.triage.spec_labels`, `runs.triage.fix_labels` (`src/pkg/classify/triage.go:35-55`)

#### `tracker`, `meta-tracker`, `tracking`, `epic` (last segment)

**Applies to:** Issues. **Kind:** Soft for agents; hard for contributors.

Marks coordination-only/tracker issues; agents see tracker tags, contributors do not get them.

**Who applies it:** Human/Hive.

**Checked by:** Tracker detector, PR claim rewrite, contributor queue. **When:** Enumeration/contributor admission.

**Release or override.** Remove label or close tracker

**Configuration and implementation.** Tracker detector (`src/pkg/github/client.go:3634-3683`)

#### `epic` (any case; `governor.claims.never_claim_labels`)

**Applies to:** Issues. **Kind:** Hard for agent claims.

Umbrella issue is never a unit of agent work: every automated claim producer (relay `claim`, start signal, contributor lane) is refused with `never-claim label: <label>`, and a claim or start mark that exists on one never derives `stage/020`/`stage/030` — the epic stays `stage/010-actionable` ([#12999](https://github.com/hivecommons/hive/issues/12999)).

**Who applies it:** Human.

**Checked by:** Issue-claim admission, issue stage derivation. **When:** Claim admission, stage derivation.

**Release or override.** Remove label or set `never_claim_labels: []`

**Configuration and implementation.** `src/pkg/github/issue_claim_gate.go`, `src/pkg/github/issue_stage_labels.go`, `src/pkg/claims/neverclaim.go`

#### `hive-plan`, `hive-design`

**Applies to:** Issues. **Kind:** Gate when enabled.

`hive-plan` mints/decomposes an epic; `hive-design` asks architect for design first. Only works when planning-from-label is enabled and ACMM >= 5.

**Who applies it:** Human.

**Checked by:** Planning label sweep. **When:** Planning sweep.

**Release or override.** Remove label or process plan/design

**Configuration and implementation.** `planning.plan_from_label`, `planning.plan_labels`, `planning.design_labels` (`src/pkg/config/config.go:400-460`, `src/pkg/planning/issue.go:483-510`)

#### `hive/likely-done`

**Applies to:** Issues. **Kind:** Display-only.

Merged PR is verified as related while issue remains open; issue remains actionable until GitHub/operator closes or confirms. Requires a closing keyword or branch-name claim: a merged `Refs #N` / `Part of #N` applies `hive/partially-addressed` instead ([#12383](https://github.com/hivecommons/hive/issues/12383)).

**Who applies it:** Hive.

**Checked by:** PR-claim label sync and dashboard. **When:** Claim sync after enumeration.

**Release or override.** Hive removes when actionable evidence no longer says likely done

**Configuration and implementation.** Fixed label (`src/pkg/github/prclaims.go`)

#### `hive/partially-addressed`

**Applies to:** Issues. **Kind:** Display; suppresses likely-done.

A merged PR covered only part of the issue ([#11931](https://github.com/hivecommons/hive/issues/11931)): applied instead of `hive/likely-done` when the PR has no closing keyword (`Fixes`/`Closes`/`Resolves #N`) for the issue, its body carries a remainder/out-of-scope paragraph, or none of the backtick-quoted paths cited in the issue body is among its changed files. No `/fixed` prompt is posted, `hive/likely-done` is suppressed, and the issue stays in the actionable pool; the `<!-- hive:task-list-sweep -->` comment lists the cited paths no merged PR changed as `Not yet touched: ...`.

**Who applies it:** Hive.

**Checked by:** Close-on-merge sweep, PR-claim label sync and task-list sweep. **When:** Close-on-merge sweep for issues behind a confirmation gate.

**Release or override.** Hive removes it when a later merged PR fully covers the issue and `hive/likely-done` is applied; humans may remove it

**Configuration and implementation.** Fixed label (`src/pkg/github/close_on_merge.go`, `src/pkg/github/task_list_sweep.go`)

#### `hive/verified-open`

**Applies to:** Issues. **Kind:** Suppresses likely-done; kick wording.

A `hive/likely-done` / already-done claim from a merged PR was checked and the issue is NOT finished ([#11931](https://github.com/hivecommons/hive/issues/11931)): the agent that verified it labels the issue (`src/pkg/scheduler/kickmessage.go` "verify once" instruction) so later kicks say "already verified — implement the rest" and the claim sync never re-applies `hive/likely-done` from the same stale claim (`src/pkg/github/prclaims.go`). Merged-PR evidence, orthogonal to the stage: the issue keeps moving `010 → 020 → 030 → 040` as new work starts.

**Who applies it:** Agent (verify-once kick) / Hive.

**Checked by:** Verify-once kick path, PR-claim label sync and close-on-merge. **When:** Verify-once kick, claim sync.

**Release or override.** Human removes when the issue is really done, or close-on-merge closes the issue

**Configuration and implementation.** Fixed label (`src/pkg/github/prclaims.go` `VerifiedOpenLabel`, `src/cmd/hive/verifyonce.go`)

#### `hive/already-done`

**Applies to:** Issues. **Kind:** Contributor hard skip; dashboard display; conditional close.

Contributor already-done verdict/confirmation; default contributor skip label and done band. When the verdict cites a PR that the API verifies as merged on the default branch, Hive labels the issue and runs the normal completed close path; human-filed bugs close too unless `hive: needs-confirmation` or `issues.reporter_confirmation` opted into the reporter gate. A `wrong_base` verdict is routing evidence, never already-done evidence: it cannot apply this label or enter the completed-close path.

**Who applies it:** Hive/contributor flow.

**Checked by:** Contributor queue, already-done verdict close path, and dashboard. **When:** Contributor admission, verified verdict settlement, issue close gate.

**Release or override.** Human removes/reopens or confirms reporter-gated bugs

**Configuration and implementation.** `hub.contribute_already_done_label`, fixed label (`src/pkg/config/config.go:4701-4924`, `src/pkg/dashboard/contribute_verdict_settle.go`, `src/pkg/github/issue_close.go`)

#### `needs-triage`, `discussion`, `question`, `tracking`, `epic`

**Applies to:** Issues. **Kind:** Contributor hard skip.

Default contributor skip labels/patterns.

**Who applies it:** Human/Hive.

**Checked by:** Contributor queue. **When:** Contributor admission.

**Release or override.** Remove label or config

**Configuration and implementation.** `hub.contribute_skip_labels`, `HIVE_CONTRIBUTE_SKIP_LABELS` (`src/pkg/config/config.go:4585-4685`)

#### Contributor allow/deny label filters

**Applies to:** Issues. **Kind:** Contributor hard gate.

Hive-wide label filters can run in deny mode (skip if any label matches) or allow mode (offer only if at least one label matches); per-repo filters layer on top and can only narrow offers. Patterns use Hive wildcard/substring matching, not GitHub hold matching.

**Who applies it:** Operator.

**Checked by:** Contributor queue. **When:** Contributor admission.

**Release or override.** Edit filter mode/list or per-repo filter

**Configuration and implementation.** `hub.contribute_labels_mode`, `contribute_deny_labels`, `contribute_allow_labels`, `contribute_repo_filters` (`src/pkg/config/config.go:4388-4420`, `src/pkg/config/config.go:6940-6994`, `src/pkg/config/config.go:7008-7055`)

#### `1-triage`, `2-discussing`, stage labels

**Applies to:** Issues. **Kind:** Display-only.

Stage vocabulary is display-only unless another consumer names the same label. `2-discussing` ships in the default waiting band; `1-triage` is just a visible repo taxonomy label unless configured elsewhere.

**Who applies it:** Human/repo automation.

**Checked by:** Dashboard repo-card bands/legend. **When:** Dashboard render.

**Release or override.** Remove label or change band config

**Configuration and implementation.** `dashboard.issue_bands` (`src/docs/dashboard.md:105-140`)

#### `origin/<source>` (non-lane only) / `agent/<lane>` (agent lanes)

**Applies to:** Issues. **Kind:** Informational.

Marks which hive component created an issue (see [Origin sources](#origin-sources)). Agent filings use exactly one lane/provenance label, `agent/<lane>`; the redundant `origin/<lane>` and bare `<lane>` aliases are stripped. Non-lane hive filers use `origin/selfreport`, `origin/governor`, `origin/dashboard`, `origin/forge`, or `origin/convergence`. Humans get none. No code path uses `origin/*` as a gate. At startup, open items carrying retired reviewer spellings (`from-review`, `origin/review`, `origin/reviewer`) are swapped to `agent/reviewer`, redundant lane-origin labels are removed, and older `hive/selfreport` issues are backfilled with `origin/selfreport`.

**Who applies it:** Hive.

**Checked by:** Every hive issue-create path, GitHub proxy, origin-label reconciler. **When:** Issue creation/display; one-time migration at startup (`/data/origin-label-migration-v4.done`).

**Release or override.** Remove label

**Configuration and implementation.** `review.backlog.labels` (default `[agent/reviewer]`; `from-review`, `origin/review`, and `origin/reviewer` accepted and mapped) (`src/crux/config/origin_labels.go`, `src/pkg/github/origin_labels.go`, `src/pkg/github/review_backlog.go`)

#### `hive: churn-triaged`

**Applies to:** Issues. **Kind:** Workflow signal.

Marks churn triage acknowledgement for contributor flow.

**Who applies it:** Human/Hive.

**Checked by:** Contributor churn guard. **When:** Contributor/churn checks.

**Release or override.** Remove label to re-triage

**Configuration and implementation.** Fixed phrase in contributor flow docs/config (`src/pkg/dashboard/contribute_admission.go:302-338`)

#### `good first issue`, `help wanted`, `bug`, `enhancement`

**Applies to:** Issues. **Kind:** Soft ordering.

Small ordering boost/visibility in contributor panels; `good first issue`, `help wanted`, and `bug` also appear in default actionable priority labels for human-filed issues.

**Who applies it:** Human/Hive.

**Checked by:** Contributor opportunistic ordering. **When:** Contributor queue/ranking.

**Release or override.** Remove label

**Configuration and implementation.** Contributor/ranking config (`src/pkg/github/client.go:4665-4698`)

#### `good first issue` + `help wanted` (hive-curated, `<!-- hive:gfi -->` comment)

**Applies to:** Issues. **Kind:** Soft (advertising).

On by default (`good_first_issue.enabled: false` opts out), hive applies both labels to a few qualifying open, unclaimed issues and leaves one welcome comment linking its relay. Not a hold — the issue stays in the hive's queue; a hive agent starting it removes the label and marks the comment superseded. Never in the hard-suppress set. See [good-first-issue.md](../good-first-issue.md).

**Who applies it:** Hive.

**Checked by:** Contributor advertising. **When:** Sweep (`src/pkg/gfi`).

**Release or override.** Hive agent start, or remove label

**Configuration and implementation.** `good_first_issue.*`

### PR review and merge controls

#### `project.repo_policies[].auto_merge: false`

**Applies to:** PRs. **Kind:** Hard merge gate.

Below L6 this resolves off for every repo; at L6, PRs can still be opened, reviewed and held when the per-repo switch is off, but Hive refuses every merge path for that repo. Existing `hold` / `hive-pause/<hive-id>` labels stay put because the disabled repo is skipped before hold release or merge.

**Who applies it:** Operator/dashboard.

**Checked by:** `hive-merge` relay, App self-authored auto-merge sweep, proxy. **When:** Merge relay request, self-merge sweep tick, proxy direct merge attempt.

**Release or override.** Dashboard repo-card switch or config edit sets `auto_merge: true`/removes the override at L6; enabling is rejected below L6

**Configuration and implementation.** `project.repo_policies[].auto_merge` (`src/pkg/config/repo_policy.go`, `src/crux/agent/manager_modes.go`, `src/pkg/github/automerge/automerge_sweep.go`, `src/crux/proxy/rules.go`)

#### `do-not-merge` and `do-not-merge*`

**Applies to:** Issues, PRs. **Kind:** Hard suppress.

Permanently exempt; exact match is case-insensitive but prefix matching follows Go `strings.HasPrefix` on the original label.

**Who applies it:** Human.

**Checked by:** Exempt filter, merge sweeps, task-list sweep. **When:** Enumeration/sweeps.

**Release or override.** Human removes

**Configuration and implementation.** Built-in `PermanentExemptLabels` (`src/pkg/github/client.go:1317`, `src/pkg/github/client.go:3575-3589`)

#### `governor.labels.exempt` entries

**Applies to:** Issues, PRs. **Kind:** Hard suppress.

Same exempt behavior as `do-not-merge`; wins over required-label admission. Defaults include `nightly-tests`, `LFX`, `meta-tracker`, `auto-qa-tuning-report`, `adopters`, `changes-requested`, `waiting-on-author`.

**Who applies it:** Operator/dashboard.

**Checked by:** Exempt filter. **When:** Enumeration/sweeps.

**Release or override.** Remove label or config entry

**Configuration and implementation.** `governor.labels.exempt` (`src/pkg/config/config.go:3388-3396`, `src/pkg/config/config.go:5800-5808`)

#### `hive:unauthorized`

**Applies to:** PRs (issues when a step on one is blocked). **Kind:** Human-gated hold.

The hive's GitHub App was refused (`403 Resource not accessible by integration`) on an API call the lane needs to advance, so only a person can take the next step. First wired call site: approving held `action_required` workflow runs for a registry-trusted author needs `actions: write`, which an operator may decline to grant ([#14589](https://github.com/hivecommons/hive/issues/14589)). The lane reports `unauthorized:<step>` (`unauthorized:approve-workflow-runs`), the stage engine parks the PR at 085, one `<!-- hive:unauthorized head=<sha> step=<step> -->` comment names the missing permission, the exact human step (**Approve and run** on the checks banner or Actions tab, with the run ids) and the alternative of granting the permission, and the hive logs `lane unauthorized` once per head at INFO instead of a WARN per tick.

The activity monitor draws a muted `unauthorized` pill (counted under Needs attention) whose tooltip repeats the permission and the step, and the 085 cell reads **Awaiting human: approve workflow runs**.

**Who applies it:** Hive.

**Checked by:** Contributor lane (`markUnauthorized`), dashboard activity monitor. **When:** Every lane tick that hits the refused call.

**Release or override.** Removed by the hive in the tick the step is no longer needed (the required checks for the head report, the lane advances); a new head re-posts the comment once. No human label edit required; removing it by hand is harmless — it is re-applied if the step is still refused

**Configuration and implementation.** Not configurable; outside `stage/`, so the reconciler never strips it (`src/pkg/github/automerge/automerge_unauthorized.go`)

#### `needs-human`

**Applies to:** PRs. **Kind:** Hard fix-dispatch and merge gate.

Applied when red-CI fix budget is exhausted or a workflow needs a person; stops automated fix dispatch, sends the PR to the human/reviewer lane, and blocks auto-merge sweeps while present. It is not a PR enumeration filter. The stage engine reads it as a park (`label:needs-human` → `stage/085-needs-decision`), so a writer that adds it must also submit a `Parked` fact (`FactSink.Parked(ref, "needs-human")`) in the same pass; the label alone leaves the previous lane stage standing until the next auto-merge tick.

The supersession sweep does this for a contributor PR that touches files the merged closer PR did not, and for a hive PR below ACMM L6; it never adds the label to a hive PR at L6, which it resolves itself (see [Supersession](stage-labels.md#supersession-closed-issue-another-merged-pr)).

**Who applies it:** Hive.

**Checked by:** Escalation ledger, reviewer lane, self-authored auto-merge sweep, queued auto-merge sweep, supersession sweep. **When:** Escalation pass/kick building/auto-merge sweeps/supersession sweep.

**Release or override.** Human removes after root cause or reviewer un-escalates; budget resets after grace; auto-merge reconsiders on the next tick because no persisted hold state is kept; the supersession sweep removes only the `needs-human` it applied itself (label timeline actor is a hive account) when it takes a hive PR's resolution over

**Configuration and implementation.** Fixed label (`src/pkg/escalation/escalation.go:400-470`, `src/pkg/escalation/escalation.go:1185-1205`)

#### `agent/<role>`

**Applies to:** Issues, PRs. **Kind:** Informational + routing.

Marks the filing/owning agent. The wrapper always derives the suffix from the lane name (`HIVE_AGENT`), never the display name, so PR ownership and label routing see the same token the scheduler compares against. On PRs it is written only when the PR's author is the hive's own App identity: editing, commenting on or merging a contributor's PR (a `review.fix_human_prs` review-fix kick, a description touch-up) does not transfer ownership and adds no label.

The stage reconciler strips a stray `agent/*` or `hive/<hive-id>` from any open PR the App did not author (#12545). Adoption (stage/074) opens a new App PR, which carries both legitimately; the adopted contributor PR keeps its own authorship.

**Who applies it:** Hive.

**Checked by:** Provenance, ownership, dashboard bands. **When:** Issue/PR creation and display.

**Release or override.** Remove if wrong; the reconciler removes it from non-hive PRs

**Configuration and implementation.** Agent identity settings (`bin/gh-wrapper.sh` `_hive_owns_pr`, `src/pkg/github/automerge/automerge_stage_labels.go` `stripStrayOwnershipLabels`, `src/pkg/scheduler/pr_annotations.go:146-158`)

#### `lgtm`

**Applies to:** PRs. **Kind:** Permit.

Queue label for human/owner merge action; the sweep also requires its normal authorization and skips held/exempt PRs. Adding by hand is not enough if the App approval/authorization is absent.

**Who applies it:** Dashboard/merger.

**Checked by:** Queued auto-merge sweep. **When:** Auto-merge sweep.

**Release or override.** Remove label; head changes can de-queue

**Configuration and implementation.** `governor.labels.automerge`, default `lgtm` (`src/pkg/config/config.go:3388-3396`, `src/pkg/config/config.go:4952-4957`, `src/pkg/github/automerge/automerge_sweep_loop.go:13-82`)

#### `reviewer-passed`

**Applies to:** PRs. **Kind:** Workflow.

Marks one reviewer pass/de-escalation; with no `needs-human`, un-escalates.

**Who applies it:** Reviewer lane/Hive.

**Checked by:** Reviewer lane/escalation reconciliation. **When:** Reviewer/escalation pass.

**Release or override.** Remove only if intentionally re-reviewing

**Configuration and implementation.** Fixed label (`src/pkg/escalation/escalation.go:1185-1205`)

#### `reviewer-recommend-close`

**Applies to:** PRs. **Kind:** Workflow.

Reviewer recommends closing rather than continuing automated repair.

**Who applies it:** Reviewer lane.

**Checked by:** Reviewer lane. **When:** Reviewer pass.

**Release or override.** Human decides/clears

**Configuration and implementation.** Fixed label (`src/pkg/scheduler/reviewer_lane.go:296-305`)

#### `review.human_decision_label`

**Applies to:** PRs. **Kind:** Informational.

Mirrors review `requires_human` verdict to an existing repo label. The label itself gates nothing and Hive does not create/remove it.

A trusted human's normal GitHub **APPROVED** review on the exact current head can override **every non-approved swarm outcome** at the enforcing merge review gate, including `requires_human`, `changes_requested` and `reject`. The gate records `human_approver` and the overridden outcome in `overrode`; a new head needs new approval (`mergeReviewGate` in `src/pkg/github/automerge/automerge_review_gate.go`).

Do not confuse that broad GitHub-review override with a recorded **`/hive decision approve`**: for swarm-review purposes, the latter answers the `requires_human` question on that head, through `trustedHumanApprovalForRequiresHuman` / `recordedHumanApprovalForHead`; it is not a blanket override for other swarm outcomes. Clearing the review gate is not an unconditional merge: independent CI, DCO, conflict, hold, sentinel, soak, applicable admission/repository-policy checks and separate human-review requirements still apply.

**Who applies it:** Hive if label exists.

**Checked by:** Human-decision mirror. **When:** Review verdict.

**Release or override.** Human removes

**Configuration and implementation.** `review.human_decision_label` (`src/pkg/github/human_decision_label.go:1-36`, `src/pkg/config/config.go:7186-7203`)

#### `needs-rebase`

**Applies to:** PRs. **Kind:** Informational.

Fills mergeability annotation in kick data.

**Who applies it:** Scanner/Hive.

**Checked by:** Scanner/kick annotation. **When:** PR status processing.

**Release or override.** Remove after rebase

**Configuration and implementation.** Fixed label (`src/pkg/scheduler/pr_annotations.go:117-144`)

#### `<!-- hive:first-timer-welcome -->` comment

**Applies to:** PRs. **Kind:** None (advertising).

On by default (`governor.first_timer_welcome.enabled: false` opts out), hive leaves one welcome comment on the first PR a human first-time contributor opens in a repo, inviting them to run their own hive (profile, leaderboard, trust tiers). Never on bot, `author/clanker`/`author/hive` or draft PRs; once per author per repo. See [good-first-issue.md](../good-first-issue.md#welcoming-first-time-contributors).

**Who applies it:** Hive.

**Checked by:** Contributor onboarding. **When:** Sweep (`src/pkg/firsttimer`).

**Release or override.** n/a

**Configuration and implementation.** `governor.first_timer_welcome.*`

### Contributor routing and other signals

#### `triage/accepted` (or `project.issue_filter.reporter_trust.untrusted_require_labels`); `needs-triage` while waiting

**Applies to:** Human-filed issues from untrusted reporters. **Kind:** Hard admission gate (off by default).

With `reporter_trust.enabled: true`, an issue whose reporter's GitHub `author_association` is outside the trusted set (default `OWNER`, `MEMBER`, `COLLABORATOR`) and whose login is not in `trusted_logins` is not actionable until it carries one of these labels. On the first excluded scan, core `github.Client.fetchIssues` posts one marked explanation comment and applies `needs-triage` (or `awaiting_label`); later scans do not repeat the comment. This is a poller-side write, not an agent prompt, and is capped at 10 new notices per poll.

When the triage label appears, the same poll loop removes `needs-triage` only if the marker records that Hive added it, then the issue enters the backlog. Independently of the marker, the stage sweep removes `needs-triage` once the issue reaches 020 or later (claiming settles triage; see `stage/015-triage-needed` below) — this is what retires the `needs-triage` that `fleetreport` stamps on hive-filed self-reports at filing. Trusted reporters' issues pass untouched.

Runs after holds/exempts and before `require_labels`, so the ordinary allow-list still applies afterwards. Hive- and bot-filed issues are not judged here (#5117 owns them). Counted as "needs triage" on the repo card, not as a generic filter refusal.

**Who applies it:** Human; Hive App for the visibility comment/label.

**Checked by:** GitHub enumeration. **When:** Enumeration.

**Release or override.** Add the label, trust the login or association under **Settings → Labels → Reporter trust**, or disable the gate

**Configuration and implementation.** `project.issue_filter.reporter_trust.{enabled,trusted_associations,trusted_logins,untrusted_require_labels,awaiting_label,comment}` (`src/pkg/config/reporter_trust.go`, `src/pkg/github/client.go` `fetchIssues`)

#### `approved-direction`

**Applies to:** Agent-filed issues. **Kind:** Gate input + soft ranking.

A PR opened after the label is present avoids #5117 hold; the issue ranks ahead of unacknowledged hive-filed backlog.

**Who applies it:** Human.

**Checked by:** #5117 gate, ranking, kick tag. **When:** PR creation and issue ranking.

**Release or override.** Remove label; human assignee/comment can still acknowledge for #5117

**Configuration and implementation.** Fixed `HumanAckLabel` (`src/pkg/github/pr_self_authorization.go:14-226`, `src/pkg/github/client.go:4665-4679`)

#### Relay-linked parent

**Applies to:** Hive-filed issues the relay split out of a parent. **Kind:** Non-label approval.

A child the issue-request relay itself linked as a GitHub sub-issue inherits acknowledgement from an **open, unheld** parent that is human-filed or carries `approved-direction`/a human assignee; one level only, and only for links the relay made (recorded in `/data/split-parents.json`), never for sub-issue links added later in the UI. The kick line shows `[hive-filed+parent-ack #N]`.

**Who applies it:** Hive (from the parent's human signal).

**Checked by:** #5117 gate, ranking, kick tag. **When:** Enumeration and PR creation.

**Release or override.** `hold` the child, or close/hold the parent

**Configuration and implementation.** Fixed ledger (`src/pkg/github/split_parents.go`)

#### Priority labels (`triage/accepted`, `ai-fix-requested`, `approved-direction`, `kind/bug`, `bug`, `priority/critical-urgent`, `priority/important-soon`, `help wanted`, `good first issue`)

**Applies to:** Human-filed issues. **Kind:** Soft ordering.

Human-filed issues with these labels go to tier 0 in kick lists.

**Who applies it:** Human/Hive.

**Checked by:** Ranking. **When:** Ranking.

**Release or override.** Remove label

**Configuration and implementation.** `HIVE_ACTIONABLE_PRIORITY_LABELS` (`src/pkg/github/client.go:4665-4698`)

### Retired names and compatibility signals

#### `hive/<hive-id>` (**retired**, #12539)

**Applies to:** Issues, PRs. **Kind:** None.

Formerly marked which hive filed or touched an item. Replaced by the `<!-- hive:origin hive=<hive-id> at=… -->` marker `bin/gh-wrapper.sh` appends after the identity footer (`**Instance:** \`<hive-id>\``) on every body it injects; `github.OriginHive` reads the marker and falls back to a leftover label. Fleet reports name the hive in `<!-- hive:selfreport … hive=… -->`, the seen-marker comment and the `Affected hives` block; claim markers carry `hive=`. Never a hold (#9371).

**Who applies it:** Nobody (was: `gh-wrapper.sh`, self-report joiner, hub relay).

**Checked by:** None. **When:** —.

**Release or override.** `Client.RetireHiveIDLabels` runs at every boot until clean: strips the label from open items, deletes the definition once nothing open carries it, writes `/data/hive-id-label-retire-<hive-id>.json` and a `.done` marker

**Configuration and implementation.** Hive id (`src/pkg/github/origin_marker.go`, `src/pkg/github/hive_id_label_retire.go`)

#### `🐝 covered by PR` (formerly `hive/covered-by-pr`; **retired in favour of `stage/040-covered-by-pr`**, see [Stage labels](stage-labels.md#stage-labels-stagennn--the-pipeline-state-machine); the stage reconciler removes it from open items)

**Applies to:** Issues. **Kind:** Display-only.

Open PR is verified as related; issue remains actionable. For a closing or branch-name claim Hive also links the PR to the issue via GitHub's `addCloseIssueReferences`, so GitHub's own issue list shows the PR icon ([#11888](https://github.com/hivecommons/hive/issues/11888), [#12091](https://github.com/hivecommons/hive/issues/12091)). A reference claim (`Refs #N`, `Part of #N` — including a branch named after the issue when the body says `Refs #N`) is never linked, because a closing link makes GitHub close the issue when the partial PR merges; if the hive App itself linked such a pair earlier, the next sweep retracts the link with `removeCloseIssueReferences`, while links made by a person are left in place and logged ([#12144](https://github.com/hivecommons/hive/issues/12144)).

The dashboard's own `🔗` pill does not depend on the GitHub link. Labels are synced only for actionable issues, so stale labels can remain on held/exempt/filtered issues.

**Who applies it:** Hive.

**Checked by:** PR-claim label sync and dashboard. **When:** Claim sync after enumeration.

**Release or override.** Hive removes when actionable issue no longer has open PR evidence

**Configuration and implementation.** Fixed label (`src/pkg/github/prclaims.go:1380-1435`)

#### `needs-direction` (retired)

**Applies to:** Issues. **Kind:** Hard suppress until migrated.

Retired ([#12683](https://github.com/hivecommons/hive/issues/12683)): nothing writes it. `stage/045-needs-decision` is the single "issue parked for a maintainer ruling" signal (owner decision: `stage/085-needs-decision` is PR-only). An open issue still carrying it is read as parked (same hard suppress, same un-park), the issue stage reconciler adds `stage/045-needs-decision` and then removes the retired label, and the label definition is deleted from the repo once no open item carries it. The scanner's "needs-direction (scanner): …" comment may remain as the explanation text accompanying the 045 transition — no label.

**Who applies it:** — (legacy).

**Checked by:** Issue stage reconciler (migration only). **When:** Issue stage reconciler.

**Release or override.** Migrated to `stage/045-needs-decision` (`src/pkg/github/legacy_decision_labels.go`)

**Configuration and implementation.** Fixed legacy spelling (`src/pkg/config/issue_filter.go`)

#### `needs-decision` (retired)

**Applies to:** Issues, PRs. **Kind:** Hard suppress until migrated.

Retired ([#12683](https://github.com/hivecommons/hive/issues/12683)): nothing writes it. The relay's `hive-open-issue --needs-decision` and the configured `project.issue_filter.hard_suppress_labels.needs_decision` / `hub.contribute_needs_decision_label` now default to `stage/045-needs-decision`; a configured legacy spelling (or `stage/085-needs-decision`) is upgraded. Migration as for `needs-direction`.

**Who applies it:** — (legacy).

**Checked by:** Issue stage reconciler (migration only); PR lanes read it as a hold modifier until gone. **When:** Issue stage reconciler.

**Release or override.** Migrated to `stage/045-needs-decision` (`src/pkg/github/legacy_decision_labels.go`)

**Configuration and implementation.** `hub.contribute_needs_decision_label` (`src/pkg/config/config.go`)

#### `claimed` (**retired → `stage/020-claimed`**), `preempted:<login>`, `hive/claimed-by-human` / `hive/claimed-by-<agent>` (the per-login `hive/claimed-by-<login>` is **retired**, read-only during migration)

**Applies to:** Issues. **Kind:** Display-only.

A claim is a comment; the `claimed` label is a mirror Hive keeps in sync. Go claim gates use comments/assignees/ledger, not these labels.

**Who applies it:** Hive/claim flows.

**Checked by:** Claims/dashboard. **When:** Dashboard render.

**Release or override.** Cleared by claim flow or human

**Configuration and implementation.** `governor.claims.*` (`src/docs/dashboard.md:105-140`)

#### `hive/in-progress` (**retired → `stage/020-claimed` / `stage/030-pr-in-progress`**; the claim marker comment below remains the detection signal)

**Applies to:** Issues. **Kind:** Kick withhold while marker live; display.

States that a hive agent (normally the scanner) has started work on a PR for this issue ([#11841](https://github.com/hivecommons/hive/issues/11841)). Applied by `hive-open-issue claim` next to `hive/claimed-by-<agent>` in one label call, together with a bot comment carrying `<!-- hive:claim who=<agent> kind=agent hive=<spoke> at=<RFC3339> until=<at+agent TTL> -->` (the ledger's 🔒 comment carries the same `at=` when `governor.claims` is on). While the marker is live (`until` in the future) every hive scanning the repo withholds the issue from kicks, so two spokes never start the same issue; the `at=` instant is the start timestamp for per-repo SLA spans (`kicked → started → pr_opened`).

**Who applies it:** Hive.

**Checked by:** Agent claim relay, ledger mirror, enumeration sweep and dashboard in-progress band. **When:** Claim relay, ledger `OnClaimed`, enumeration sweep.

**Release or override.** Hive removes when the agent's PR opens (`hive-open-pr --issues`), when the ledger releases the agent claim, or at enumeration once PR evidence exists (`hive/covered-by-pr`, `hive/likely-done`, linked PRs) or the marker lapsed (default 2h `agent_ttl_s`); `governor.claims.label: false` disables it

**Configuration and implementation.** Fixed label (`src/pkg/github/issue_inprogress.go`, `src/cmd/hive/claimswire.go`)
