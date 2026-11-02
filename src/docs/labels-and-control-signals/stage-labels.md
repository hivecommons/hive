# Hive Labels and Control Signals: Stage labels: the pipeline state machine

Part of [Hive Labels and Control Signals](../labels-and-control-signals.md).

## Stage labels (`stage/NNN-*`): the pipeline state machine

Operator request ([#12429](https://github.com/hivecommons/hive/issues/12429)):
"the labeling of issues and prs has to give hints as to what stage the work is
in … the labels must be clear and reflect the state machine … this needs to be
dead simple." The hive therefore owns ONE ordered, mutually exclusive
`stage/NNN-slug` family shared by issues and PRs, an orthogonal `role/*`
family that answers "would the hive auto-merge this?", an orthogonal
`author/*` family that answers "who wrote this?", and one red health
marker (`stage/stalled`) that says nobody is working the item. Exactly one
`stage/*` stage label per open item; closed, merged or superseded items carry
none (terminal = no stage). (`stage/regressed` was retired in
[#12534](https://github.com/hivecommons/hive/issues/12534): the review loop
050 → 060 → 072 → 070 → 050 is the intended cycle, so a backward move is
recorded as a one-line stage comment and in the dashboard's transition
history, not as a label.)

The engine lives in `src/pkg/stagelabels` (the table, the reconciler plan, the
persisted stage timing) and `src/pkg/github/automerge/automerge_stage_labels.go`
(the mapping from each automerge lane's per-PR reason to a stage, applied once
per sweep cycle under the `hive:stage_labels` REST caller). Labels come from
the PR list payload the sweep already fetched and are written only when the
planned set differs from the current one, so an unchanged item costs nothing.

**One writer.** Every stage write — the PR lanes, the issue reconciler
(`src/pkg/github/issue_stage_labels.go`) and the claim-ledger hooks that
mirror a new claim onto the issue — goes through `stagelabels.Enforce` /
`EnforceStage` ([#12512](https://github.com/hivecommons/hive/issues/12512)).
When a write is due, Enforce re-reads the item first and plans from that
fresh label set, so a stage another writer placed since the cycle's snapshot
(a contributor claim landing seconds after the reconciler staged the issue
`010`) is replaced rather than joined, and any item found carrying two
`stage/NNN-*` labels is healed on the spot (`healed` in the reconcile log
lines). The claim hooks always re-read: they know the stage a claim implies
but not what the lanes think of the author tier or health, which they keep.
Per issue the hooks also run in ledger order, so a release's re-read always
follows its own claim's label writes rather than racing them
(hivecommons/hive#14201).
The hub's label writes are also journaled against the item's `updated_at`
from that read: the mutation journal otherwise treats "remove `stage/020`
from #N" as applied forever, so a label another writer put back could never
come off again — the reconciler kept logging the remove while GitHub kept
the label (hivecommons/hive#12443).

**Issue stages and PR stages are disjoint.** The stage table marks each
stage `Issue` or `PR` (never both): `010`–`040` are issue stages, `050`–`100`
are PR stages. Enforce clamps every write to the item's kind
(`Stage.For(stagelabels.IssueItem | PRItem)`): a PR stage asked of an issue
becomes `040-covered-by-pr`, an issue stage asked of a PR clears the stage,
and the reconcile log line carries `clamped=true` with the stage that was
asked for. An issue's pipeline therefore ends at `040` while its PR is open;
the covering PR's own stage is recorded in the issue's stage *reason* (`open
PR #N covers it (PR at 060-reviewed)`, in `/data/stage-labels.json` and the
dashboard), never as a second label. Before this rule the reconciler mirrored
the PR's stage onto the issue, which is how hivecommons/hive#12451 came to
wear `stage/010-actionable` + `stage/040-covered-by-pr` +
`stage/050-review-pending` at once and #12483 (covered by a PR that merely
referenced it) a lone `stage/050-review-pending`
([#12451](https://github.com/hivecommons/hive/issues/12451)).

### Numbering rules

- **Sparse, zero-padded, 3 digits.** GitHub sorts labels lexically, so the
  padding is what keeps `stage/100-…` after `stage/090-…`.
- **100 is always the merge queue.** New stages are inserted *between*
  existing numbers (e.g. `035`), never above 100.
- **Renaming or inserting a stage = edit ONE table**, `stagelabels.Table`.
  Stages are keyed in code by Go identifiers (`ReviewPending`, `MergeQueue`,
  …); the label string (number + slug), description, dwell and colour are all
  derived from that row. The reconciler treats any `stage/*` label not in the
  current table as stale and removes it, so a renumber ripples to every open
  item on the next cycle; the old labels can then be deleted from the repo.
- **Colours**: every `stage/NNN-*` label is the same hive amber `#f5a623`
  (the colour the retired `🐝 covered by PR` used) so the eye tracks the family
  across lists; `stage/stalled` `#d73a4a` is red; `role/*` is neutral grey
  `#6e7781` except `role/trusted` `#0e8a16`; `author/human` is the same grey,
  `author/clanker` purple `#8250df`, `author/hive` blue `#1d76db`; `dco`
  shares `role/trusted`'s green and `no-dco` shares `stage/stalled`'s red
  (same constants, so the pair reads as one family). The retired
  `stage/regressed` is stripped from any item the sweep sees and is no longer
  provisioned.
  The ensure-label step creates missing labels and PATCHes the colour of an
  existing one only when it differs from the table, once per repo per
  process — a PATCH keeps the label on every PR wearing it, so an existing
  pill is recoloured, never removed and re-added.

### The table

| Label | Applies to | Meaning | Expected dwell (then `stage/stalled`) |
|---|---|---|---|
| `stage/010-actionable` | issue | triaged; eligible for the hive to pick up. A human-filed issue without `needs-triage` counts as triaged by its trusted reporter | 24h |
| `stage/015-triage-needed` | issue | actionable, but no maintainer has looked at it yet. Derived instead of 010 only when the reporter-trust gate is holding the issue: it carries the awaiting label `needs-triage` (`reporter_trust.awaiting_label` default) or sits in the #9665 gate's `reporter_triage` bucket. Who filed it is never consulted — issues from hive agents and from trusted humans are triaged by construction and stay 010. A **display and tracking state, not a gate**: the scanner and relay claim a 015 issue exactly as a 010 one (the reporter-trust gate withholds `needs-triage` issues separately, and that is unchanged), and 015 lights the activity monitor's *Actionable* column as "Triage needed". `015 → 010` (a maintainer triages) is never a regression; `010 → 015` is a reclassification. Because 015 is not a gate, a claim settles triage: whenever the sweep (or an out-of-cycle claim fact) writes a stage of 020 or later — 020, 030, 040 or a PR-mirrored stage — it removes `needs-triage` in the same label write, and an open issue already at 020+ still wearing it is reconciled on the next sweep; the label survives at 010, 017 and 045, which the table lets fall back to 015 (hivecommons/hive#14586). The Open column is untouched: an issue with no stage stays "Open" | 24h |
| `stage/017-possible-existing` | issue | **Possibly exists · needs author.** The prior-art gate found something that looks like the request already (knowledge bead, setting, merged PR, closed issue, doc or code) and parked the issue with the findings; the author confirms the gap (`/hive decision proceed — <gap>`) or closes it (`/hive decision exists`, label `already-exists`). The label itself is the park signal and an issue-level hard suppress, so no agent claims it; clanker-filed parks auto-close after `runs.triage.prior_art.bot_close_ttl`; hive-filed issues never wear it ([#13575](https://github.com/hivecommons/hive/issues/13575)) | none |
| `stage/020-claimed` | issue | a live claim by anyone — a human, a relay contributor or a hive agent's `hive-open-issue claim` / start mark — with no PR evidence yet (replaces `claimed`, `hive/in-progress`) | 2h |
| `stage/030-pr-in-progress` | issue | the claimant has a PR under way: its `hive-open-pr --issues` request named the issue or its start marker named a branch / PR (`pr_started=true` on the `hive:claim` marker). A claim or start mark without that evidence — the scanner's bare `hive-open-issue claim` included — stays at 020 ([#13069](https://github.com/hivecommons/hive/issues/13069)). If the in-progress evidence includes an open Hive-authored PR, the issue-stage lane applies the same PR-link safeguard described for 040: default-branch PRs that fully address the issue get a `Fixes #N` trailer when missing, while partial or non-default-branch PRs get an issue stage-entry comment linking the PR and explaining GitHub's icon limitation. If the claiming PR has already merged, the issue leaves 030: full-fix claims close it with one `<!-- hive:closed-by-merged-pr pr=N -->` comment (checked before posting, so a close that fails after the comment is retried without a duplicate), while `Refs` / `Part of` claims move back to 010 with a partial-landed note. | 90m |
| `stage/040-covered-by-pr` | issue | an open PR covers it (replaces `🐝 covered by PR`), or — when the hive's PR only *references* the issue without a closing keyword (`Refs #N`) — "an open PR addresses part of it": the issue is still parked here but its stage reason and tracker caption say the remainder stays open when the PR merges ([#15008](https://github.com/hivecommons/hive/issues/15008)). Coverage is derived only from a closing reference in the PR body or commits, a hive claim-ledger entry, or an explicit hive marker — a bare cross-reference (an incident deferral comment, `see also`, a backlog link) never puts an issue here; the issue stays here while the PR is open — the PR's own stage is named in the issue's stage reason, never written as its label ([#12451](https://github.com/hivecommons/hive/issues/12451)). For Hive-authored PRs on the repository default branch, the stage and claim-label sweeps append `Fixes #N` to the PR body when the PR appears to fully address the issue but lacks a closing keyword, so GitHub can show the Development sidebar link and issue-list PR icon. They also retrofit older Hive-authored default-branch PRs whose only claim line is `Refs #N` by rewriting that one keyword to `Fixes #N` once and stamping `<!-- hive:issue-link-fixed -->`; human-authored PR bodies are never edited. `Part of` / leave-open partial work and non-default-branch PRs get one issue stage-entry comment with the PR link and GitHub limitation instead. Merged full-fix claims close the issue; merged reference claims remove stale 040/030 and return the issue to 010 with a partial-landed note. | n/a (the PR carries health) |
| `stage/045-needs-decision` | issue | **the** issue-side park-for-a-person signal (owner decision, 2026-10-17: issues never wear `stage/085-needs-decision`; that stage is PR-only). Derived whenever the issue carries `needs-human` or a hard-suppress park label (`needs-spec`, the retired `needs-direction` / `needs-decision`, a pre-045 `stage/085-needs-decision`) **and no hold-class label** — a held issue (`hold`, `do-not-merge*`, the hive's `hive-pause/<id>`) is paused, not parked: it carries no stage label and the activity monitor shows it as "Open · on hold", the reconciler removing a 045 the hold caught it with ([#13012](https://github.com/hivecommons/hive/issues/13012)) — and written by the relay (`hive-open-issue --needs-decision`, or `label … stage/045-needs-decision`, the one issue stage label an agent may add) and by the dedupe/selfreport relays; the issue is outside the agent work queue (and off the good-first-issue list) until an eligible maintainer replies `/hive approve` / `/hive decision <text>`, which removes the park and adds `approved-direction`. On the ordinary un-park path, `needs-human` lifts regardless of whether Hive, a person or an unknown actor applied it, allowing 010 when no other gate remains ([#13075](https://github.com/hivecommons/hive/issues/13075)); keep or add `hold` to preserve the stop, and see the separate label-driven policy in the [`needs-human` reference](reference.md#needs-human). With `project.agent_issue_decision_policy: resolve` (default), Hive-authored non-risk engineering questions are not allowed to self-park here — the relay records `Decision taken by hive: …`, downgrades to `stage/010-actionable`, and audits the downgrade, while true parks include a `Risk: <category>` line naming security posture, data loss, irreversible/public-facing behaviour, governance/policy/licensing, spend, owner-only credentials/secrets/infra, or uninferable human intent; once `approved-direction` is on the issue the relay refuses an agent's re-park while the recorded decision is unchanged — the agent adds `needs-human` and `/unclaim`s instead; a lapsed claim on a parked issue never shows 030. A contributor verdict that parks here citing only the target repository's own confirmation gate ("AGENTS.md says offer a spec and wait", "confirm scope first") or a registered sibling checkout that is merely not cloned yet (`repo_location_missing`) is not a decision: the hub reclassifies it as idle, removes the label with a `<!-- hive:needs-decision-released kind=repo-local-gate -->` comment, and re-offers the issue once; the second such park on the same issue stays (`src/pkg/dashboard/contribute_gate_park.go`, `src/pkg/decisionpolicy`). Leaving 045 is never a regression. An issue still wearing 085 is re-staged to 045 by the reconciler in one write, and a one-shot startup sweep (`/data/issue-needs-decision-stage-v1.done`) catches issues the enumeration never sees (`src/pkg/github/issue_needs_decision_migration.go`) | none |
| `stage/050-review-pending` | PR | review swarm requested/dispatched; awaiting a verdict for the current head (or CI still pending with no verdict) | 1h |
| `stage/060-reviewed` | PR | swarm verdict approve but a gate is still pending (CI running, or the soak clock on a head the lane has not admitted — the trust-blocked 090 path) | 30m |
| `stage/070-fixing` | PR | hive-owned repair: App/taken-over PR red, changes requested (by the swarm, or by a trusted human's **changes requested** review / `/hive decision <text>` on the current head — reason `human-changes-requested`, which also kicks the author agent with the text verbatim, [#13041](https://github.com/hivecommons/hive/issues/13041)) or needs-rebase, **or** a non-App PR whose red head was caused by a hive-pushed commit that must be removed/re-homed; a hive agent is reworking it | 2h |
| `stage/071-needs-author` | PR | human-authored PR (`author/human`): CI red / changes requested / conflicting; the hive posted its diagnosis, the author must act. No nudge, no adoption — the person pushes or comments | 24h |
| `stage/072-returned-to-contributor` | PR | clanker PR (`author/clanker`, relay or external hive): CI red / changes requested / conflicting; the hive posted its diagnosis, the ball is in the contributor hive's court. (Briefly spelled `stage/072-needs-author` by [#12713](https://github.com/hivecommons/hive/pull/12713); the engine renames that label back in place) | 24h |
| `stage/074-adopted-by-hive` | PR | the author (a clanker) was unavailable — relay offline, never registered, or silent past the grace (see below); the hive took the PR over: in place on the PR's own branch when it may push there (the scanner's red-PR fix kick rebases and fixes it), otherwise via `hive/adopt-<n>` and a superseding App PR that keeps the author's commits (the original closes automatically when the superseder merges). Adoption is an **ownership** state, not a CI state: the PR reads 074 for as long as the hive owns it — through its fixes going pending, green, reviewed or red again (074 outranks 050/060/070/071/072, and a 085 whose only cause is the review swarm's `requires_human` / decision request — the hive answers that on the superseding PR, so the original leaves the merge gate for the CI/Review column with an "Adopted → #N" pill in the adopting pass, never "Needs human decision" (hivecommons/hive#14975 → #14980, #14947 → #14977); a hold, sentinel alert, `needs-human` a person applied, fork-guard or manual-merge lane rule keeps 085) — and leaves 074 only at the merge gate (080/090/100), when the adoption is released, or to 085 `adoption-stalled:<reason>` when the head sat unchanged past the dwell with CI idle and a real blocker the hive cannot push past — a red optional check alone never counts, and the escalation releases back to 074 as soon as the reason clears (an in-place adoption with a blocking review finding re-homes to `hive/adopt-<n>` first instead; one with a squash-unsafe hive commit elects a merge commit on a `role/trusted` PR — `merge-method/merge` label, lane continues — and otherwise immediately re-homes to an App-authored superseder (`signoff-rehome`), preserving commits and sign-offs while changing squash attribution; loss of push access re-homes immediately, and idle rebase work re-homes after the dwell). For in-place adoption, a changed head must be linked to the App bot by GitHub commit metadata (the stage store only remembers heads that already passed that check); otherwise the hive treats it as a contributor push and hands the PR back (072 if red, else the normal path). Forgeable git author/committer names and emails never keep 074. A PR whose author holds **admin** or **maintain** on the repository is never adopted or re-homed (one cached permission read per repo and author per 10 minutes; a failed read is not cached and defers: no adoption, and an existing adoption is neither released nor re-homed until the read succeeds), and an adoption of such an author is released on the next pass, closing a still-open hive superseder with a note first. | 3h |
| `stage/080-awaiting-merge-admission` | PR | nothing left for the author: CI green (no red check on the head, required or not), verdict approve, no modifiers — and the automerge lane has not admitted it yet (prefilter not evaluated for this head, labels not synced, lane tripped). A stall here means a lane problem, so the hive also posts one `<!-- hive:stage-stalled -->` comment with the lane's last skip reason | 30m |
| `stage/085-needs-decision` | PR | **PR-only** since the owner decision of 2026-10-17 (issues park at `stage/045-needs-decision`; before that this stage carried both, [#12683](https://github.com/hivecommons/hive/issues/12683)). A person owes a **decision**, not a merge: a `requires_human` swarm verdict (reason `reviewer-verdict:requires_human`), `sentinel-alert`, `hold` / `do-not-merge*` / `needs-decision` modifier, no reviewer configured, GitHub workflow approval is required for a trusted same-repo bot/App/clanker PR (`workflow-approval-required:<ctx>`); Hive has posted `/ok-to-test` and is waiting for the repo-local `hive-ok-to-test` workflow or a maintainer, or a human-only lane rule unrelated to trust (workflow-path edits, tests removed, human-merge path, reporter-trust hold). A trusted human's normal GitHub **Approve** review on the exact current head clears a `requires_human` verdict for any author kind/lane and records `reviewer-verdict:requires_human:human-approved@<login>`; a new head invalidates it. The same approval also clears the merge-lane review gate over any non-approved swarm outcome, a perspective's `changes_requested` included (the `review_evaluated` audit line records `human_approver` and `overrode`); without it, `changes_requested` / `reject` still require fixes or manual merge. Under `review.self_authored_decision_policy: resolve`, a Hive-authored PR's reviewer decision question parks here only when it matches a named risk class (`review.DecisionRiskClasses`: permissions/credentials, security, sensitive paths, data loss/irreversible, merge/trust policy, external side effects, licensing/legal, unverifiable) or carries `<!-- hive-decision-risk: high -->`; any other question is resolved by policy and recorded as an advisory reason on the approving aggregate. Hive-authored non-risk incomplete-fix reviews with concrete remediation are normalized to `changes_requested` at verdict load/aggregation time and by the stage-label detail selector, even when the finding was marked high severity; duplicate summary/changelog intent-alignment cleanup with a concrete trim/reword fix is included. Those PRs go to `stage/070-fixing`, not here. On a hive-authored PR the trusted human can also answer with a **changes requested** review on the head, or `/hive decision <what to change>`, which moves the PR to `stage/070-fixing` and kicks the author agent with the text verbatim ([#13041](https://github.com/hivecommons/hive/issues/13041)); `/hive decision` on a PR the hive did not open gets one reply naming the review-side equivalents. One `<!-- hive:stage-needs-decision -->` comment on entry names what is owed, and for a hive-authored `requires_human` park names the two actions: approve the head to merge as-is, or request changes and the hive pushes the fix. When the park is a `hold`, the comment says who set it — *the `hold` label was set by the hive itself: reporter trust (issue #N by @X, contributor)* or *by @login* — and for a hive-set hold offers *release the hold* / *keep it* (hivecommons/hive#15020) | none |
| `stage/090-ready-for-human-merge` | PR | the hive would merge this itself but trust/role blocks it: reviewer verdict approve at the **current** head, required checks green (tide excluded), DCO yes, no sentinel alert, no conflicts, soak elapsed, no hold — and the only remaining lane reason is the author's tier/role. A maintainer's `/lgtm` `/approve` is the only missing step. One `<!-- hive:stage-ready-for-human-merge -->` comment on entry carries the evidence. Always read with the `role/*` and `author/*` labels. (Was `stage/090-needs-human` before [#12528](https://github.com/hivecommons/hive/issues/12528); the engine renames the GitHub label in place) | none |
| `stage/095-pre-merge-soak` | PR | lane admitted; the pre-merge soak window (`automerge.contributor_lane.soak_minutes`, default 30) is running before the merge is attempted. The dashboard shows it as **Soaking** on the merge-gate column with the time left; a quiet 095 past its dwell is a lane bug (the soak elapsed but the lane never promoted), so the stall note says so. 100 → 095 on a re-evaluation is a reclassification, not a regression | 45m (30m soak + 15m grace) |
| `stage/100-merge-queue` | PR | lane admitted and soak complete: merge-eligible, waiting for tide / the merge window — and every check on the head is green (tide excluded). A red optional check is 070/071/072 like any red CI | 3h |

Dwell thresholds are overridable per slug under `governor.labels.stage.dwell`
(`review-pending: 1h`, `0` = never). The whole family is switched by
`governor.labels.stage.enabled` (default `true`); both are on the Settings →
Labels tab.

```mermaid
stateDiagram-v2
    direction LR
    [*] --> s010: issue triaged
    [*] --> s015: issue from an untrusted reporter (needs-triage / reporter_triage)
    s010: 010 actionable
    s015: 015 triage-needed
    s020: 020 claimed
    s030: 030 pr-in-progress
    s040: 040 covered-by-pr
    s045: 045 needs-decision (issue)
    s050: 050 review-pending
    s060: 060 reviewed
    s070: 070 fixing
    s071: 071 needs-author
    s072: 072 returned-to-contributor
    s074: 074 adopted-by-hive
    s080: 080 awaiting-merge-admission
    s085: 085 needs-decision (PR)
    s090: 090 ready-for-human-merge
    s095: 095 pre-merge-soak
    s100: 100 merge-queue
    s010 --> s020: scanner / relay claim
    s010 --> s045: needs-human / park label (agent escalates)
    s010 --> s015: needs-triage added (reclassification)
    s015 --> s010: maintainer triages: triage/accepted, hive/verified-open or approved-direction added, or needs-triage removed (not a regression)
    s015 --> s020: scanner / relay claim (015 is not a gate; needs-triage removed in the same write)
    s015 --> s045: needs-human / park label (agent escalates)
    s020 --> s045: claim lapses on a parked issue
    s045 --> s010: eligible /hive approve or /hive decision (ordinary un-park; needs-human lifts for every applier; hold remains)
    s045 --> s015: park label removed by hand on an untriaged issue (approved-direction itself counts as triage, so /hive approve lands on 010)
    s020 --> s030: PR under way (hive-open-pr request or start marker naming a branch)
    s020 --> s040: PR opened straight from the claim
    s020 --> s010: claim released or lapsed with no PR (not a regression: 020 is a reservation, nothing was built)
    s020 --> s015: claim released or lapsed with no PR on an untriaged issue (not a regression)
    s030 --> s040: PR opened
    s030 --> s010: PR request failed / start mark lapsed with no PR
    s030 --> s015: PR request failed / start mark lapsed with no PR on an untriaged issue
    s030 --> s020: PR evidence lapsed while the claim is still held (backward hop, commented)
    s040 --> s010: covering PR closed unmerged with no live claim or start marker (backward hop, narrated on the PR; not a regression when the PR merged and left hive/partially-addressed)
    s040 --> s015: covering PR closed unmerged on an untriaged issue (backward hop, narrated on the PR)
    s040 --> s030: the agent is back on the covering PR (live claim with the PR under way; not a regression)
    s040 --> s020: covering PR closed while the claim is still held (backward hop, narrated on the PR)
    s040 --> s050: the PR takes over (issue stays 040)
    s050 --> s060: swarm approve
    s060 --> s080: CI green, author side done
    s080 --> s095: lane admits, soak window running
    s080 --> s100: lane admits, no soak / soak already complete
    s080 --> s070: current head red, changes requested or being rebased by the train (hivecommons/hive#15825; backward hop)
    s080 --> s050: new push: CI and verdicts owed again on the current head (hivecommons/hive#15825; backward hop)
    s095 --> s100: soak complete
    s100 --> s095: pre-095 record reclassified while still soaking (not a regression)
    s050 --> s070: hive PR red / changes requested
    s050 --> s071: human PR red / changes requested
    s050 --> s072: contributor PR red / changes requested
    s050 --> s085: swarm requires_human (reason reviewer-verdict:requires_human)
    s060 --> s070: new head red
    s095 --> s070: new head red during the soak (backward hop, commented)
    s095 --> s072: contributor head red during the soak (backward hop, commented)
    s100 --> s070: new head red (backward hop, commented)
    s100 --> s072: contributor head red (backward hop, commented)
    s070 --> s050: fixed head pushed
    s070 --> s085: the hive's adoption copy (hive/adopt-N) still conflicts with the base after the fixer's two rebase-resolve attempts (rebase-failed-twice)
    s071 --> s050: author pushes a new head
    s072 --> s050: author pushes a new head
    s072 --> s074: contributor agent unavailable (offline / unregistered / grace), adopted in place or superseded
    s074 --> s080: in-place fix green and approved (adoption outranks 050/060; the merge gate takes it)
    s074 --> s095: lane admits the hive's head, soak running
    s074 --> s100: lane admits the hive's head, soak complete
    s074 --> [*]: superseder merged (original closed); close_first (default) closes the original in the adopting pass instead
    s074 --> s085: close-first copy closed unmerged or red at two heads → original reopened, adoption-copy-failed
    s074 --> s085: the superseding copy still conflicts with the base after the fixer's two rebase-resolve attempts (adoption-stalled:rebase-failed-twice; Adopt into fresh branch / Close as superseded)
    s074 --> s072: superseder closed unmerged, or contributor pushed over an in-place adoption (handed back; 050 if that head is green)
    s074 --> s085: adopted head unchanged past the dwell with CI idle and a real blocker the hive cannot push past (adoption-stalled:<reason>; a red optional check alone never counts), or the re-home failed
    s085 --> s074: the adoption-stalled reason cleared (CI re-ran, check green, verdict landed); escalation released
    s085 --> s074: a 085 that is only the review swarm's requires_human yields to a live adoption in the adopting pass (the hive answers on the superseder); a hold, sentinel, needs-human a person applied, fork guard or manual-merge lane rule keeps 085
    s050 --> s085: hold / sentinel / human-only lane rule
    s060 --> s090: untrusted tier, head otherwise mergeable, soak elapsed
    s085 --> s090: hold lifted / alert cleared, head mergeable
    s090 --> s085: hold or sentinel alert added
    s090 --> s050: new head pushed (verdict pending)
    s090 --> s072: head red / DCO missing / conflicts
    s085 --> s072: head red / DCO missing / conflicts
    s085 --> s070: hive PR, trusted human changes_requested review or /hive decision (author agent kicked)
    s090 --> [*]: maintainer lgtm+approve → merged
    s100 --> [*]: merged (labels removed)
```

#### The regressed marker: when it is set, when it clears

The dashboard's `regressed` chip and the `<!-- hive:stage-regressed -->`
stage comment both come from one per-item flag in the stage store
(`Record.RegressedFrom`, `src/pkg/stagelabels/store.go`), never from a GitHub
label (`stage/regressed` is retired and stripped on sight). Its lifecycle:

- **Set** by `Store.Observe` when a hop is a regression under
  `IsRegression(from, to)`: a numerically backward hop *except* the drawn
  exceptions above. Leaving a reservation is never a regression (020 → 010 /
  015 on a lapsed or released claim — nothing was built); 030 → 020 is the
  claim's PR evidence flickering (the issue is still claimed), not a fall;
  040 → 030 is the agent back on its PR; leaving 015, 045, 085, 090 and
  100 → 095 are reclassifications. A fall that is **progress** — 040 → 010
  because the covering PR merged and left `hive/partially-addressed` — is
  never booked either (`Decision.Progress`, `Store.ObserveProgress`).
- **Kept** across a same-stage re-observation and across a backward hop that
  is itself not a regression (a 040 → 020 fall followed by 020 → 010 stays
  regressed: the item has not moved ahead yet).
- **Cleared** by the first forward hop of any size (not only by climbing back
  to the stage it fell from), by a progress fall, or when the rule changes so
  the recorded fall no longer counts (old records heal on their next
  observation). The clear happens in the same tick that applies the new
  stage label, and the dashboard stops drawing the chip on its next
  `GET /api/stage-tracker` (`regressed_from` is absent, the modifier gone).

How the chip renders depends on whether a person is in the loop. On a
hive-driven stage with no human-gate modifier (`hold`, `needs-human`,
`needs-decision`, `needs-rebase`, `blocked`, `stalled`) and off the
human-gated stages (045 / 071 / 072 / 085 / 090), it draws as a muted,
dashed `stage ↓` pill whose hover text names the fall and ends "the hive is
handling it — no action needed; clears on the next forward step", and the
row stays out of **Needs attention** and keeps its ordinary headline. Only a
regression on a human-gated row keeps the red `regressed` pill and the
"Sent back" headline — red means someone has to act
([#14570](https://github.com/hivecommons/hive/issues/14570): freshly
filed issues whose claim lapsed wore a red `regressed` that read as a call to
action).

### How an issue's stage is decided

The enumeration cycle already sorts every open issue into a work bucket
(actionable, live claim, in-progress start mark, covered by an open PR,
filtered …) before the scanner kicks anything. `src/pkg/github/issue_stage_labels.go`
(`SyncIssueStageLabels`, REST caller `hive:issue_stage_labels`) runs once per
cycle after the claim guards and the in-progress sweep, so the buckets are
final, and maps each row ([#12436](https://github.com/hivecommons/hive/issues/12436)):

| Enumeration bucket | Stage |
|---|---|
| carries `hive/likely-done` (a PR merged, awaiting confirmation) | no stage — every hive-owned label removed, timing forgotten |
| actionable (no claim, no PR) | `010-actionable` |
| actionable but held by the reporter-trust gate (needs-triage, or the reporter_triage bucket) | `015-triage-needed` |
| live claim (`<!-- hive:claim … -->` marker / ledger) by anyone — a human, a relay contributor, or one of the hive's own agents (`hive-open-issue claim`, the in-progress start mark) — with **no PR evidence** | `020-claimed` |
| live claim whose marker carries `pr_started=true` — the holder's `hive-open-pr --issues` request named the issue, or its start comment's `<!-- hive:start branch=<name> -->` / `pr=<n>` marker named where the PR lives (`claims.Claim.PRStarted` / `stageengine.ClaimFact.PRStarted`) | `030-pr-in-progress` — the label promises a PR, so a bare agent claim never wears it ([#13069](https://github.com/hivecommons/hive/issues/13069)) |
| covered by an open PR, PR not yet staged by the automerge sweep | `040-covered-by-pr` |
| covered by an open PR the sweep has staged (≥ 050) | still `040-covered-by-pr`; the reason reads `open PR #N covers it (PR at <stage>)`. `050`+ are PR-only stages and never land on an issue ([#12451](https://github.com/hivecommons/hive/issues/12451)) |
| reporter triage (human-filed, held by the #9665 gate until the triage label lands) | `015-triage-needed` — the gate still withholds it from agents |
| exempt / filtered / advisory / dependency dashboard | no stage |
| **decomposed parent** — an issue with tracked child issues (GitHub sub-issues the relay linked with `parent: N`; see below) | keeps the stage the rows above give it while any child is open, **never `stage/stalled`**; once every child is closed the parent is **closed as completed** by the sweep ([#12857](https://github.com/hivecommons/hive/issues/12857)) |

Labels come from the issue list payload the cycle already fetched — no
per-issue GETs — and are written only when the planned set differs from the
current one. While an open PR covers the issue (040), `stage/stalled` belongs
to the PR and is not written on the issue; for the issue's *own* stages
(010–030) the dwell is still measured from stage entry, because the list
payload carries no comment count or activity timestamp the hive did not
bump itself (`updated_at` moves on every hive write, #12491) — the PR
engine's no-activity clock and `stalled.require_quiet` below do not apply
to issues. When the issue falls back between its own stages (a claim or
kick lapsed with no PR: 020 → 010, 030 → 010 or 030 → 020, all drawn in
the diagram so the transition monitor does not count them as anomalies) it
gets one explanatory comment and a history hop in the store. Falls out of
040 (covering PR closed: 040 → 010/030/020, also drawn) record the hop but
are not commented on the issue — the covering PR narrates its own close. The claim
relay (`hive-open-issue claim`) writes `hive/claimed-by-<who>` itself and
submits a *fact* to the stage engine (`stageengine.FactSink`, ADR-0025) —
`Claimed` / `InProgress` — which re-reads the issue and writes
`stage/020-claimed` so the label is right as soon as the claim lands; a
`hive-open-pr --issues` request (or a start comment naming a branch / PR)
is the PR evidence that re-records the claim with `pr_started=true`, which
derives `stage/030-pr-in-progress`; the PR opening submits `CoveredBy`, and
the engine swaps 020/030 for 040. Claim release, the PR-claim sync
(a closed or merged PR: `Uncovered`), parking on a decision (`Parked`) and
`/hive approve` / `/hive decision` (`Unparked`) are facts too. An agent
comment that *escalates* (opens with
`needs-decision`/`needs-direction`/…, "parking this", "Triage (x): not a …
defect") or a label request carrying only escalation labels is not a start
signal: it records no agent claim and never stages the issue 030. No code
outside the engine writes a `stage/*` label (a label request naming one is
turned into the matching fact): every out-of-cycle fact goes through a fresh
read so the derivation sees the labels GitHub has now and the mutation journal
keys the write to the item's `updated_at` — a label a human removed is
re-applied, not replayed as "already applied"; close-on-merge strips every
hive-owned label from an issue it closes. Timing shares the PR engine's state file
(`/data/stage-labels.json`, issues and PRs share a number space per repo);
an issue's record never holds a PR stage, so a PR falling `100 → 060` does
not read as the issue regressing.

> **Implemented (ADR-0025): central stage engine.**
> [ADR-0025](../adr/0025-central-stage-engine.md) puts one `pkg/stageengine`
> reconciler and a single `StageWriter` behind the two derivation paths on
> this page (issue sweep and automerge lanes) and every out-of-cycle stage
> write — the claim relay, in-progress sweep, PR-claim sync and dashboard
> submit *facts* instead of labels
> ([#13009](https://github.com/hivecommons/hive/issues/13009)); the
> per-kind rules above are the engine's derivers. The reconciler is built once per process
> (`cmd/hive/stage_engine.go`) and shared by both sweeps. Rollout is gated
> by `governor.labels.stage.engine.mode` (Settings → Labels → *Central
> stage engine mode*; takes effect on the next sweep, no restart):
>
> | mode | who derives | who writes | counted |
> |---|---|---|---|
> | `enforce` (default) | engine | engine's derivation | `stage_transition_anomaly_total{kind,from,to}` for out-of-order moves |
> | `shadow` | engine | nobody (pause; labels stay as found) | `stage_derivations_total{kind,stage}` only |
>
> Mode and the derivation / anomaly / healed counts are on
> `/api/status.stageEngine` and in the activity tracker's status strip;
> the operator notes are in
> [operator-reference.md](../operator-reference.md#central-stage-engine-rollout)
> and the plan in [stage-engine-plan.md](../stage-engine-plan.md).

### Before any stage: `000-new` in the activity monitor

A freshly opened issue or pull request carries no `stage/*` label and no
stage record until the stage-label pass of the next enumeration cycle
looks at it (and, for a human contributor's issue, until a maintainer
adds a reporter-trust label — `triage/accepted` by default,
`issues.filter.reporter_trust.untrusted_require_labels`). That gap is not a
label: nothing is written on GitHub for it. It *is* a state the dashboard
shows ([#15094](https://github.com/hivecommons/hive/issues/15094)): the
`opened` webhook re-read inserts the item into the activity monitor at once,
and until something places it the row reads `000-new` in the **Open** column
with a caption naming the next mover — a maintainer (the triage gate is the
next hop; the caption says which label to add) or the hive (the stage-label
pass marks it `010-actionable`, then the scanner claims it, `020`) — and the
governor's next-pass countdown. The gates ahead are always the same three,
in order: **triage** (reporter gate, skipped for admitted reporters and the
hive's own filings) → **actionable** (`010`, written by the stage-label pass)
→ **claimed** (`020`, the scanner). `triage/accepted` plus `help wanted` on
a human-filed issue is actionable work at every level: once the label lands
the next pass writes `010` and the scanner may claim it. The dashboard
legend is in [dashboard-sections.md](../dashboard-sections.md#hive-activity-monitor).

### How a PR's stage is decided

The automerge sweep already computes one reason per PR per lane each cycle
(self-authored, trusted-author, contributor lane — see
[hive-merge.md](../hive-merge.md)). `automerge_stage_labels.go` maps that reason,
the review-swarm verdict for the head, and the author tier the lane actually
used to admit the PR:

| Lane reason (examples) | Stage |
|---|---|
| merged / `closed` / `gone` | terminal: every hive-owned label removed, timing forgotten |
| `draft` | no stage (stale stage removed) |
| `contributor-lane:no-verdict`, `review-gate:no-review-bundle`, `review-gate:awaiting-review-approval`, CI pending with no verdict | `050-review-pending` |
| CI pending, verdict approve | `060-reviewed` |
| CI green, verdict approve, no modifier, yet the lane stopped on a reason no stage claims or tripped on a transient error (`review-state-check`, …) | `080-awaiting-merge-admission` (weak opinion: any lane's definite stage wins) |
| a posted reviewer decision question (`decision_requested`) on a conflicting / not-mergeable / changes-requested head | `085-needs-decision`, reason `contributor-lane:decision-requested` — no fixer is kicked for such a head, and a conflict is moot if the answer is to close it ([#14607](https://github.com/hivecommons/hive/issues/14607)) |
| a trusted maintainer answered that question on the same head — `/hive decision <instruction>` or a CHANGES_REQUESTED review (recorded in `review-human-change-requests.json`, head-scoped) | `070-fixing`, reason `contributor-lane:decision-answered`: the answer is folded into the reviewer verdict for that head on every tick and after a restart, so the stage never re-derives 085 from the stale `requires_human` verdict. A bare `/hive decision approve` instead answers that `requires_human` question; it does not remove a `hold` label or provide the broad GitHub-review override for `changes_requested`/`reject`. A new head draws a new verdict with no answer and asks again ([#14679](https://github.com/hivecommons/hive/issues/14679)) |
| CI red (`check-failing:*`, `required-check-failing:*`, `status-*`), `conflicting`, `not-mergeable`, `changes-requested`, verdict changes_requested/reject, `needs-rebase` present | `070-fixing` (self-authored lane or `author/hive`) / `071-needs-author` (`author/human`) / `072-returned-to-contributor` (`author/clanker`) — see [Stage 07x by author kind](#stage-07x-by-author-kind). A head whose App-linked commits carry a `Signed-off-by` the squash would misattribute to the PR author (`hive-signoff-attribution`) follows the same split: the branch owner is the only one who can rewrite them. |
| `contributor-lane:soaking(...)` — and no red check on the head | `095-pre-merge-soak` |
| `head-changed-since-eval`, `merge-failed`, `merge-queue-required` (the base branch's ruleset only accepts merges through GitHub's merge queue and the transport is a static token that cannot enqueue, so the direct REST merge is refused; with App auth on a repo opted in through `auto_merge.native_merge_queue` these bases are enqueued instead, see [native-merge-queue.md](../native-merge-queue.md); logged as a skip rather than a merge error, [#14018](https://github.com/hivecommons/hive/issues/14018); the self-authored sweep then skips the direct merge on that base for 30 minutes and logs the skip once per PR head, [#14068](https://github.com/hivecommons/hive/issues/14068); the trusted-author, queued and contributor-lane sweeps skip it the same way), `required-checks-expected`, `behind`, `updated-branch`, `behind-update-budget-exhausted` (a behind self-authored PR the sweep did not update only because this tick's branch-update budget was spent; retried next tick, waits on the hive, [#12607](https://github.com/hivecommons/hive/issues/12607)), `release-hold*`, `native-merge-queued: queued (position N, STATE)` (the PR was enqueued into GitHub's native merge queue) and the failures `native-merge-queue-read`, `native-merge-queue-enqueue`, `native-merge-queue-method` — and no red check on the head | `100-merge-queue` |
| CI red (required or optional check) while the PR is deferred to an open `[shared-ci]` incident (`<!-- hive-shared-ci-<n> -->`, the governor's `deferred_incident`) for the same head | `050-review-pending` / `060-reviewed`, reason `shared-ci-incident:<n>`: the base or a shared check is what is red, so the PR is never sent to 070/071/072, never nudged and never stalled; once the incident closes a still-red head is handed back as usual. Conflicts and requested changes stay author-facing ([#12606](https://github.com/hivecommons/hive/issues/12606)) |
| CI red (required or optional) while **every** failing check on the head is also red on the base branch's tip — the governor's `base_red_checks` / `base_red`, set during CI enrichment when a same-named latest check run on the base ref (meta checks excluded) concluded failure / timed_out / cancelled / action_required / startup_failure / stale **and** the PR's changed files include none of `.github/workflows/*`, `Dockerfile` / `Dockerfile.*` / `*.Dockerfile`, `go.mod` / `go.sum` (a PR touching those plausibly owns the check and stays author-facing; a lookup error or a diff beyond the file-list page budget also stays author-facing) | `050-review-pending` / `060-reviewed`, reason `base-branch-red:<check>,<check>` ("waiting for the base branch's CI to recover (…) — not this PR's fault"): never 070/071/072, never nudged, never stalled, still not merged (the lane's skip reason is the same token). The fix loop counts no attempt, kicks no fixer, and posts one `<!-- hive-base-branch-red -->` hold comment per head; an escalation the hive itself raised (ledger `label_applied`) over such a head has its `needs-human` removed with the hold wording. The next evaluation after the base goes green re-evaluates the head: a still-red PR is handed back as usual. Conflicts and requested changes stay author-facing (seen on [#13270](https://github.com/hivecommons/hive/pull/13270) and [#13258](https://github.com/hivecommons/hive/pull/13258), 2026-10-20) |
| CI red (required or optional) on a **fork** PR while **every** failing check's annotation says the check refuses forks (case-insensitive `fork pull request(s) cannot`, `maintainer must push this branch`, or `cannot publish` together with `fork` — a fork-publication / fork-secrets guard) | `085-needs-decision`, reason `fork-guard:<check>,<check>`: never 070/071/072, never nudged, never adopted (074) — neither the author nor the hive can turn the check green by pushing to the fork. The entry comment names the check and says a maintainer must push the branch to the repository (or approve the run as the repository requires). A same-repo PR with the same message, or a fork PR with any other red check, is handed back as usual ([#14273](https://github.com/hivecommons/hive/issues/14273)) |
| any **non-required** check on the current head is red, whatever the lane's reason (the lane gates on required checks only, so it still soaks and merges; a reader of the rollup sees ✗). Parking modifiers (`hold`, `needs-human`, admission reasons) still win | `070-fixing` / `071-needs-author` / `072-returned-to-contributor` (by author kind), reason `optional-check-failing:<check>` (the nudge lists every red check); never 080 or 100. A red *required* check is the lane's own `required-check-failing:*` → 070/071/072 ([#12538](https://github.com/hivecommons/hive/issues/12538), seen on [#12457](https://github.com/hivecommons/hive/pull/12457)) |
| `label:hold`, `held`, `label:needs-human`, `exempt-label`, `excluded-label:*`, `sentinel`, `human-merge-path*`, `intent-*`, `contributor-lane:workflow-path` / `tests-removed` / `no-reviewer` / `reporter-trust-hold` | human hand-off, **decision** cause → routed by the head's evidence (below); lands at `085-needs-decision` when the head is otherwise clean. `contributor-lane:workflow-path` reads "awaiting owner approval": a trusted merger's `APPROVED` review on the current head (or a dashboard `/hive decision approve`) releases the fence and the lane merges the PR itself (`owner_approval_releases_workflow_fence`, [#14664](https://github.com/hivecommons/hive/issues/14664)); only with the toggle off, or when the App lacks `workflows: write` for the merge (`hive:unauthorized`, `merge-workflow-path`), does an approved head park at `090-ready-for-human-merge` for a person's click |
| `merge-refused:review-required` (GitHub answered the merge with 405 "At least N approving review(s) is/are required by reviewers with write access"), `merge-refused:method-not-allowed` (405 "Merge commits / Squash merges / Rebase merges are not allowed on this repository") | human hand-off, **decision** cause, as above: lands at `085-needs-decision` when the head is otherwise clean, and the entry comment names the rule GitHub refused the merge on. Every sweep (self-authored, trusted-author, queued, contributor lane) logs these as skips, not `merge-failed` errors, and stops calling merge: a review refusal is remembered per PR head and re-armed when the head moves, when GitHub's `mergeable_state` for the PR changes (an approving review turns `blocked` into `clean`), or after 30 minutes; a method refusal is remembered per repository and method for 30 minutes. The hive never switches the merge method on its own (a forward merge or a `merge-method/merge` PR must not be squashed), so `method-not-allowed` is a manual-merge park. Any other 405, 409 or 422 is still `merge-failed` |
| `contributor-lane:no-claim` / `claim-*` / `author-association` / `no-linked-issue`, `untrusted-author-role`, `author-permission-missing` | human hand-off, **trust** cause → routed by the head's evidence (below); lands at `090-ready-for-human-merge` when the head is otherwise clean |
| hand-offs (`not-app-authored`, `trusted-bot-lane`, `trusted-author:app-authored`, `contributor-lane:app-authored`), transient fetch errors | no opinion — the owning lane decides; nothing is written. An App-authored PR is handed off by the trusted-author and contributor-lane tiers under their own reason; the self-authored lane owns it, and its `review-gate:no-review-bundle` is the stage's reason while the swarm has not reviewed the head ([#14482](https://github.com/hivecommons/hive/issues/14482)) |

When several lanes see the same PR the self-authored lane outranks the
trusted-author lane, which outranks the contributor lane; the trusted-author
lane's bare `untrusted-author-role` is a weak opinion the contributor lane
refines. A weak opinion (any admission reason — "this lane cannot vouch for
the author") is only written once every lane enabled for the repository has
reported on the PR in the same reconcile pass. Two drivers run the lanes
against one process-wide tracker (the engine's own ticker and the main loop's
queued sweep), so one pass can drain a maintainer PR's trusted-author
observation and the next hold only the contributor lane's `no-claim`; an
incomplete pass is treated as no opinion rather than written as 090 and
flipped back a minute later.

A maintainer's own PR is therefore not a 090 case at all: with
`auto_merge.trusted_authors` on (the hive's default spoke config —
`require_role: merger`, `require_github_permission: true`), the
trusted-author lane admits an `authorized_users` owner/merger with
write/maintain/admin permission and merges the PR itself once CI is green,
no `CHANGES_REQUESTED` review is outstanding and no `hold`/`do-not-merge`/
`needs-human` label is present. Only the lane's own reasons (CI pending or
red, `conflicting`, `label:needs-rebase`) hold it at 050/072; a human
`/lgtm`+`/approve` is never required.

#### Human hand-offs: 072 / 050 / 085 / 060 / 090

A lane stops at admission or at a hold before it looks at CI, so "a human
must act" alone says nothing about whether the merge button is safe. For
every such hand-off the engine gathers what the lane skipped — the reviewer
verdict for the **current** head, a digest of the head's checks (tide and
the other merge-metadata contexts excluded) and its DCO context, the
`sentinel-alert` / `hold` / `do-not-merge*` / `needs-decision` labels, the
conflict reason and the contributor lane's soak — and
`stagelabels.RouteHuman` decides, in this order
([#12528](https://github.com/hivecommons/hive/issues/12528)):

| Evidence | Stage |
|---|---|
| verdict changes_requested at the head, any red gating check, DCO sign-off missing, merge conflict (`conflicting` / `needs-rebase`) | `071-needs-author` (`author/human`) / `072-returned-to-contributor` (`author/clanker`); `070-fixing` when self-authored or `author/hive`. The 072 nudge lists every reason. Moving 085/090 → 071/072 is not a regression |
| no verdict for this head yet, or nothing has reported on the head (just pushed) and the verdict is not already `requires_human` | `050-review-pending` — never 090 without a current-head approve |
| verdict `requires_human` at the head (even on a fresh head nothing has reported on yet, [#13115](https://github.com/hivecommons/hive/issues/13115)), sentinel alert, hold-class label, no reviewer configured, or a non-trust human-only lane rule | `085-needs-decision` + one entry comment naming the decision ("Needs a decision, not a merge: a sentinel alert is open on this PR.") |
| checks still running / unreadable, or the lane's soak not yet elapsed | `060-reviewed` (not 095: that stage means the lane has *admitted* the head, which a trust-blocked PR never is) |
| everything above clean and only trust in the way | `090-ready-for-human-merge` + one entry comment: "Ready for a human merge: verdict approved @abc1234, 55/55 checks green, DCO yes, soak 1h0m0s elapsed. Blocked only by `role/newcomer` (below require_tiers)." |

The entry comments are posted once per stage visit (a flip 085 ↔ 090 is a
new visit) and never as a GitHub review: the hive does not approve on a
maintainer's behalf, so prow/tide see nothing until a person acts. The
check digest costs two REST calls per head and is cached 10 minutes; it is
read only for human-routed PRs.

### `role/*` — would the hive auto-merge this?

One per PR, derived from what the lane used for admission (registry tier,
claim-marker `tier=`, trusted-author list, App-authored), recomputed with the
stage but written only on change:

| Label | Source |
|---|---|
| `role/trusted` | App-authored or self-authored lane; trusted-author lane allowed the author; registry/claim tier in the contributor lane's `require_tiers` (default `trusted`, `merger`); or contributor-lane auto-promotion after `auto_merge.contributor_tier.auto_promote_after_merged` clean merged PRs with no reverts or sentinel alerts → auto-merge eligible. Paired with `author/clanker`, it also lets Hive post `/ok-to-test` for same-repo `action_required` workflow runs from trusted automation; the repo-local self-contained `hive-ok-to-test` workflow approves them with `GITHUB_TOKEN`, while fork heads and `sentinel-alert` PRs are excluded. |
| `role/contributor` | registry/claim tier `contributor` → needs human lgtm/approve |
| `role/newcomer` | registry/claim tier `newcomer` |
| `role/untrusted` | no claim and no registry profile (external PR), `revoked`, or not a trusted author |

**Renamed from `author/<tier>`** ([#12581](https://github.com/hivecommons/hive/issues/12581)).
The old prefix conflated trust with authorship: `author/trusted` sat on PRs a
maintainer pushed by hand and on PRs a contributor's clanker opened under that
contributor's login. Trust is now `role/*`; `author/*` says who wrote it (next
section). Migration is automatic: every reconcile that writes a PR removes a
retired `author/trusted|contributor|newcomer|untrusted` and adds the `role/*`
equivalent in the same write; `stagelabels.RoleOf` still reads the old
spelling until then so a claim hook never drops the tier. The role names are
not configurable.

### `dco` / `no-dco` — the hive-owned sign-off signal

One per PR, written by the stage pass from the PR's **commits** — not copied
from any CI context or bot label ([#12542](https://github.com/hivecommons/hive/issues/12542)).
On each new head the pass reads the PR's commits once (cached per head in
the stage store) and counts the non-merge commits without a `Signed-off-by:`
trailer:

| Label | Colour | Meaning |
|---|---|---|
| `dco` | green (`role/trusted`'s `#0e8a16`), description "All commits are DCO signed-off" | every non-merge commit on the head carries a `Signed-off-by:` trailer |
| `no-dco` | red (`stage/stalled`'s `#d73a4a`), description "A commit is missing Signed-off-by — run `git rebase --signoff`" | at least one does not |
| `merge-method/merge` | stage orange, description "Hive merges this PR with a merge commit (mixed authorship, each commit self-signed); never squash" | the same commit read found App-linked commits signed as the App beside the contributor's own on a `role/trusted` PR, every commit self-signed, repo not `auto_merge.squash_only`: the lane merges with a merge commit (see the attribution paragraph under [Stage 07x by author kind](#stage-07x-by-author-kind)). Reconciled per head like the DCO pair; left as found while the commit check is unread. |

The pair is mutually exclusive. A missing sign-off is the one author-side
gate the hive decides **without waiting for CI or a reviewer verdict**: the
PR goes to `stage/071-needs-author` (human) / `stage/072-returned-to-contributor`
(contributor agent) — `070-fixing` when self-authored — with reason `dco-missing` on the first evaluation of the head,
and, for an `author/clanker` PR, the 072 nudge / relay `fix-pr` task names
the exact fix — `git rebase --signoff HEAD~<n> && git push --force-with-lease`
where `<n>` is the PR's commit count (a human-authored PR simply waits at 071
wearing `no-dco`; see "Stage 07x by author kind"). Signing and force-pushing produces a new head, which
is re-read and returns the PR to the normal path. A PR with no non-merge
commits, or whose commits could not be read, is *unknown*: its existing DCO
pills are left alone and no stage decision is made from DCO.

The labels are written on every managed repo; the **072 routing is per
repo**. It applies only where the repo actually enforces DCO — detected when
a `dco` / `DCO` context is in the branch's required checks or reported on
the PR head (the DCO GitHub App, Prow's plugin) during the lane's CI walk,
no extra API call — or where `governor.labels.stage.dco.required` says so:

```yaml
governor:
  labels:
    stage:
      dco:
        required:          # "owner/repo", bare repo, or "*"; overrides detection
          hivecommons/hive: true
          acme/playground: false
```

On an ungated repo an unsigned head keeps the lane's stage, carries `no-dco`
as information only, and the nudge does not ask for a rebase.

Prow's `dco-signoff: yes` / `dco-signoff: no` (the Prow `dco` plugin's
hard-coded names, also written by the repo's DCO workflow) are **foreign**:
the hive never adds or removes them. #12544 tried to retire them from the
stage pass and produced a label war (#13125: the writer removed
`dco-signoff: yes`, Prow re-added it, every few minutes on every open PR),
so a PR on a Prow-gated repo shows both pills until the Prow plugin is
disabled in `plugins.yaml`. Readers (`stagelabels.DCOFromLabels`, used by
the merge-eligible export) honour the Prow pair when neither hive label is
present. GitHub cannot colour one label name two ways, which is why the red
case is `no-dco` rather than a red `dco`.


### `author/*` — who wrote this PR?

One per PR, mutually exclusive, decided by `pkg/authorkind` from evidence the
hive already leaves on a PR. It is orthogonal to `role/*`: a trusted
contributor whose own hive opened the PR carries `role/trusted` +
`author/clanker`. Unlike the pipeline labels it **stays on merged and closed
PRs** — who wrote a PR is a fact about it, not pipeline state, and the hub's
revert-rate-by-author-kind drill-down (#12428) reads it.

| Label | Meaning | Example |
|---|---|---|
| `author/human` | a person pushed the PR themselves | a maintainer's hand-written PR |
| `author/clanker` | a **contributor's own agent** (relay) authored the PR under that contributor's GitHub login — "clanker" is the project's word for a contributor's agent (it is their clanker, not a hive) | mendezr's or Karibhou's relay-opened PRs: human login, `— hive:` trailer |
| `author/hive` | **this repository's own hive** — the App or one of its agent lanes — authored the PR | `app/hivecommons-hive` PRs, anything stamped `agent/<lane>` |

Detection rules, first match wins (`authorkind.Explain` returns the rule
name, logged at debug as `stage labels: author kind`):

1. author login is the hive's App login (`[bot]` suffix tolerated) → `author/hive`
2. an `agent/<lane>` ownership stamp is on the PR → `author/hive` (stamps the
   same pass strips as strays from a non-App PR, #12545, are ignored first)
3. `<!-- hive:origin hive=<id> -->` in the body: `<id>` is this hive's
   `hive_id` → `author/hive`; another hive → `author/clanker`
4. the visible `— hive:` attribution trailer (what the hub's
   `ReconcilePRAttribution` stamps on every relay-reported PR) → `author/clanker`
5. the `🐝 **Hive Agent**` identity footer from `gh-wrapper.sh` → `author/clanker`
6. the login has a contributor profile on this hive (runs a relay that
   targets it) → `author/clanker` (weakest signal; only reached with no marker)
7. a non-App `[bot]` login (dependabot, …) → no `author/*` label
8. otherwise → `author/human`

Names are configurable so a repo can spell them its own way; each must keep
the `author/` prefix (so the reconciler still owns it) and the three must
differ:

```yaml
governor:
  labels:
    author:
      human: author/human      # default
      clanker: author/clanker  # default
      hive: author/hive        # default
```

### Stage 07x by author kind

A red PR — CI red, changes requested, DCO missing or conflicting — lands in
one of three stages that mean the same thing ("the author must act") and are
reached and left through the same lane transitions. The author kind
(`author/*`, derived by the engine as `stageObservation.AuthorKind`) picks
the label, and with it the tracker word and whether the hive has anyone to
*interrupt*:

| Kind | Stage | Tracker word | Contributor nudge (`<!-- hive:contributor-nudge -->` + relay interrupt) | Offline adoption (074) |
|---|---|---|---|---|
| `author/human` | `stage/071-needs-author` | Needs author | no — plain wait on the person; they push or comment (a `role/trusted` human still gets one `<!-- hive:human-071-note head=… -->` comment per head naming the reason, always on and not gated on `ContributorNudge`, [#13636](https://github.com/hivecommons/hive/issues/13636)) | no |
| `author/clanker` | `stage/072-returned-to-contributor` | Returned to contributor | yes — the contributor's clanker is poked to fix its own PR | yes — after the grace, the hive adopts |
| `author/hive` (and any self-authored PR) | `stage/070-fixing` | Fixing | n/a — the hive's own lanes rework the head | n/a |

The split also covers a head whose commits GitHub links to the hive App while
the PR author is someone else — a hive follow-up commit on the branch, or a
contributor who committed from inside a hive pod with the App's noreply
address (hivecommons/pluk#315 and the other OWNERS PRs of 2026-10-20). The
required "Sign-off survives the squash" check is red there, and no hive push
lands on a non-App-authored branch (`review.fix_human_prs` does not authorize
one and the hive must not forge the author's `Signed-off-by`), so the PR
goes back to its author by kind under `hive-signoff-attribution`: `071` for
a human, `072` for a contributor agent — never `070`, where nothing can act. The human
gets one comment per head (`<!-- hive:signoff-attribution head=… -->`) with
the exact fix (`git commit --amend --reset-author -s && git push
--force-with-lease`, or the `git rebase --exec` form for several commits);
the contributor agent's regular 072 nudge carries the same ask. An adoption cannot
fix it by rewriting (the hive may not touch the contributor's authorship,
and a supersede would carry the same head), and the hive's own in-place fix
commits create exactly this shape — a contributor commit beside a commit
signed as the App (hivecommons/hive#13777). So on a **`role/trusted`** PR
whose head mixes the contributor's commits with App-linked ones, every
commit self-signed and the repo not in `auto_merge.squash_only`, the lane
elects a **merge commit** instead of parking the PR: it puts the hive-owned
`merge-method/merge` label on the PR, posts one
`<!-- hive:merge-commit head=… -->` note per head ("merge commit: mixed
authorship, each commit self-signed"), and the lane proceeds — the
`Sign-off survives the squash` check re-runs on the label event and accepts a
bot-authored, bot-self-signed commit under that label (nothing is squashed,
so nothing is misattributed; a human commit signed as the bot still fails),
and every merge sweep merges the PR with `merge_method=merge`, recording
the reason on the merge claim. The label is reconciled per head: a head
that no longer mixes authorship drops it, and only the hive writes it
(labels need triage). An already-parked PR — at `085` as
`adoption-stalled:hive-signoff-attribution` from before this rule — is
re-evaluated on the next sweep with no new head and no human un-park: the
election clears the escalation back to `074` and the lane carries on. When
the election is unavailable, the adopted head instead re-homes to an
App-authored superseder (`signoff-rehome`), keeping all commits and sign-offs
unchanged without lifting squash-only or role policy. Unsigned commits use
the separate DCO repair path. A self-authored PR stays `070`.

Only a contributor-agent PR has a hive on the other end, so only 072 gets the nudge
comment, the relay interrupt and adoption; a human PR at 071 just sits there
until someone pushes a new head (and then stalls per the dwell table like any
other stage). A hive-authored PR never reaches 071/072: it is 070. Should an
`author/hive` PR still carry 071 or 072 (kind learned late), the next sweep
re-derives it to 070; a human-authored PR found at 072 (pre-071 labels) is
moved to 071 the same way. 071 and 072 are "author-blocked" everywhere the
rules below name 072 — dwell/stall, needs-rebase → 07x, regression checks and
the tracker's **CI / Review** column, which prints the stage word next to the
author-kind icon.

**Merge-forward for trusted human PRs** ([#13636](https://github.com/hivecommons/hive/issues/13636)).
When a repo is listed in `auto_merge.merge_forward_trusted_human` and a
`role/trusted` + `author/human` PR on a same-repo branch is `dirty`
(conflicting), the trusted-author lane merges the base into the PR head
through the GitHub merges API: one new App-authored merge commit with the
App's `Signed-off-by`, no human commit rewritten. If the merge is clean the
PR waits for CI on the new head (`check-pending`); if git hits a real
conflict nothing is pushed and the author gets one
`<!-- hive:merge-forward-conflict head=… -->` comment naming the files both
sides changed, and the PR stays at 071. Fork heads are never written (the
071 note still explains the conflict). `behind` keeps using GitHub
update-branch. Default off.

When the hive adds a fix to a human author's branch it prefers a
merge-forward commit (base merged into the branch) or a GitHub suggestion
comment the author applies; it never pushes a direct fix commit it would
later have to strip from the human's history.

### Decomposed parents — closed by their children, never stalled

When an agent splits an issue into ordered children it files each child
through the issue-request relay with `parent: N`. The relay links the child
as a native GitHub **sub-issue** of `N`, records the link in the spoke's
`split-parents.json` ledger (#9840) and writes a `<!-- hive:parent N -->`
marker into the child body, so the relation is machine-readable three ways.
The prose "Decomposed into …" comment an agent leaves on the parent is for
people and is never parsed. The parent has nothing left to implement; it is
waiting on its children — which is not what `stage/stalled` means
([#12857](https://github.com/hivecommons/hive/issues/12857): #12361 sat at
`stage/010-actionable` after all four children merged, was marked stalled
because its own timeline was quiet, and had to be closed by hand).

The issue stage sweep reads GitHub's `sub_issues_summary` block (total /
completed) off the same list payload it already decodes — no extra reads;
when the block is absent it inverts the ledger and the body markers against
the open-issue snapshot instead — and then:

- **Some children open** → the parent keeps whatever stage the table above
  gives it, is **never** marked `stage/stalled`, and an existing
  `stage/stalled` comes off. The store reason reads `…; waiting on 2 open
  child issue(s)`.
- **Every child closed** (any `state_reason`) → the sweep re-reads the
  children live (sub-issue list, or each ledger child), strips the parent's
  `stage/*` labels, closes the parent as **completed** and leaves one
  budgeted comment: `<!-- hive:parent-autoclose -->` / `All children
  delivered: #12394 → PR #12413, #12396 → PR #12421, …` (the PR is the merged
  PR GitHub lists as closing the child; omitted when none is known). An
  issue stays an issue: parents never move to `stage/090-ready-for-human-merge`,
  which is a PR stage. The reporter-confirmation close gate still applies to
  a human-filed bug parent; a refused close is retried next cycle and no
  "delivered" comment is posted until the close succeeds.
- **No children** → nothing changes.

Reopen the parent if something it asked for is still missing; the sweep does
not close it again until a child is reopened and closed once more (the
sub-issue summary is read fresh each cycle).

### `stage/stalled` — "nobody is working this"

On a PR, `stage/stalled` means **no activity by anyone but the hive for the
stage's dwell**: no head push, no comment, no review or reviewer verdict, no
CI status change and no label change for the time in the table
(`governor.labels.stage.dwell.<slug>` overrides it; `0` = never). On an
issue in its own stages (010–030) it still means "sat in the stage past the
dwell" (see above): the issue list payload has no activity signal the hive
does not bump itself.

- **Never while the item is waiting on the hive.** Required checks still
  running, or `050-review-pending` with no reviewer verdict for the current
  head, are waits on the hive's own lanes, not on a person — so they cannot
  stall whatever the elapsed time (specimen:
  [#12417](https://github.com/hivecommons/hive/pull/12417), 6h at 050 with CI
  green and no verdict, wrongly marked stalled). At 050 the label is possible
  only once the verdict exists and nobody acted on it for the dwell.
- **Hive-owned review fixes keep cycling.** A PR the hive owns — including an
  in-place `stage/074-adopted-by-hive` contributor branch — is not waiting for
  a maintainer `/lgtm` or `/approve`. A review verdict of `changes_requested`
  keeps an adopted PR in the fixer's work list every cycle until a new head
  lands. Existing pending fix rows are replaced with the next bounded attempt
  (adopted PRs at once; any other hive-owned PR once the row is older than
  `review.pending_fix_ttl`, default 2h, with the head unchanged);
  once the attempt cap is exhausted the PR is visibly parked for a human at
  085 with reason `fix-attempts-exhausted`, not left silent.
- **Cleared at once on any activity.** The next cycle that sees a new head,
  a new comment, a new review submission (an Approve / Request changes with
  only a body counts: the comment counters miss it) or a changed
  verdict/CI/label set removes it. The automerge engine's own comments
  (stage notes, nudges, regression lines, rebase requests, stale-approval
  and hold-release notes) and the hive app's own reviews do not count,
  whether posted before or after the cycle's reconcile; a comment from
  another hive process (the reviewer swarm's review comment, a merge note)
  is indistinguishable from a person's in the comment counter and does —
  so a stall can only come later than the rule says, never earlier.
- **No `updated_at`.** "Last activity" is derived from the head SHA, the
  PR's comment counters, the review submissions the lanes list (not every
  lane path lists them; a review seen by none is caught by the next cycle
  that does) and the verdict/CI/label digest the lanes already
  fetched — never `updated_at`, which GitHub bumps for the hive's own writes
  ([#12491](https://github.com/hivecommons/hive/issues/12491)). A change is
  dated by the cycle that first observes it (at most one sweep interval
  late; the list payload carries no commit or comment timestamps). State
  lives in `/data/stage-labels.json`; no extra API calls.
- `governor.labels.stage.stalled.require_quiet: false` restores the older
  rule (dwell measured from entering the stage); waiting on the hive still
  never stalls. The review queue's `stage.stalled` / `stage.past_dwell` and
  the `hive_held` count follow the same dwell overrides and `require_quiet`
  setting as the label.
- Only a stall in `080-awaiting-merge-admission` is explained in a comment
  (`<!-- hive:stage-stalled -->`, `stage 080-…: no activity for 30m0s: <lane
  reason>`), because that stall is the hive's fault, not the author's.
- **When the hive is the holder** — an item past its dwell *while* waiting
  on the hive — the PR gets no label. The engine logs one warning per item
  (`stage labels: PR waits on the hive past the stage dwell — reviewer lane
  needs attention`), counts it in `/api/review/queue` as `hive_held`, and the
  review-queue entry carries `stage.waiting_on_hive` + `stage.past_dwell`
  (rendered as a red "waiting on hive Xh" pill). That is the signal to look
  at reviewer capacity/cadence, not at the author.
- **Backward moves** (e.g. `100 → 070` because a new head went red) are not
  labels. The hive posts one short idempotent comment carrying
  `<!-- hive:stage-regressed -->` — `stage 100-merge-queue → 070-fixing:
  check-failing:unit` — and records the hop in the item's history
  (`stage.history[]` on the review-queue entry, newest last, capped at 12).
  Leaving a repair stage (`070`/`071`/`072`/`074`) for `050`/`060` is the repair
  route itself and is not commented; nor is a move *between* `070`/`071`/`072`
  (`070 → 071`/`072` when the author kind is learned late) — that re-attributes who owes the fix, it does not
  knock the head back.
- **A decomposed parent with an open child never stalls** (see above): it
  is waiting on its children, not on a person.
- **Filter `label:stage/stalled` across repos = every PR a person has
  walked away from, plus every issue that sat in 010–030 past its dwell.**

### Off the happy path: 072 → nudge → 074 → takeover

When a contributor-agent PR goes red the hive does not just relabel it (a human-authored one goes to `stage/071-needs-author` and waits) (full flow in
[hive-merge.md](../hive-merge.md#stage-labels)):

1. `stage/072-returned-to-contributor` and ONE `<!-- hive:contributor-nudge
   head=<sha> n=<1|2> mode=<comment|queued|spawn|preempt> -->` comment
   @-mentioning the author with the failing checks / review asks and "reply
   here or push a new head". Re-posted only for a new head; re-nudged once
   more after `governor.labels.stage.contributor_nudge.renudge_interval`
   (default = the stage's 24h dwell, i.e. at `stage/stalled`), then no more
   — **cap: 2 nudges per head**. `mode=` records how the relay side was
   handled so the comment itself answers "did their agent get interrupted?".
   `governor.labels.stage.contributor_nudge.enabled: false` turns the
   comment and the relay task off; the stage label still moves.
2. Relay contributors (the author has a contributor profile and a live relay
   connection) also receive the diagnosis in their relay session as a
   `fix-pr` task with `priority: interrupt`. Presence = a relay heartbeat
   within `governor.labels.stage.presence_window` (default 15m); an offline
   or external author gets the GitHub comment only (`mode=comment`). The hub
   picks the delivery mode from what the relay advertised at DECLARE:
   - `mode=spawn` — the relay declared `parallel_sessions` and
     `max_parallel_fixes` (its `HIVE_MAX_PARALLEL_FIXES`, default 1, only
     when the backend has a headless entry point): a bounded parallel
     sub-session fixes the red PR while the primary session keeps its task.
   - `mode=preempt` — no parallel capability: the primary agent is told to
     park its current issue (WIP committed/stashed; the claim and lease stay
     alive), fix the PR on its existing branch with the same sign-off, print
     `HIVE_FIX_PR_DONE <task_id>` (or `HIVE_FIX_PR_FAILED <task_id> <reason>`),
     then resume. A headless primary cannot be interrupted, so the relay
     defers the fix and runs it before its next `ready`.
   - `mode=queued` — the contributor is over their fix budget (below); the
     task waits server-side and the PR carries stage 072 + the comment only.
   Protocol fields and client behaviour:
   [contributor-relay.md § fix-pr tasks](../contributor-relay.md#fix-pr-tasks).
   The interrupt machinery in this step and the adoption in the next are for
   `author/clanker` PRs only — there is a hive on the other end to interrupt
   or stand in for. A human-authored PR waits at `071-needs-author`;
   a Hive-authored or self-authored PR goes to `070-fixing`, where the
   Hive's own lanes repair it. Neither gets the contributor relay nudge or
   offline adoption. Human 071 comments still follow the per-head rules in
   ([Stage 07x by author kind](#stage-07x-by-author-kind)).
3. If the author stays unavailable the hive adopts the PR
   (`governor.labels.stage.adoption.enabled`, default `true`). "Unavailable"
   is decided against the contributor's **live** presence on the hub at
   every sweep, not a snapshot taken when the nudge went out
   (hivecommons/hive#12527: the author's relay was polling for work during
   the nudge and dropped off 30 minutes later; a snapshot-based grace kept
   the PR at 072 indefinitely). The marker records which rule fired:
   The author's repository role does not change this. **Contributor-agent PRs are
   adoptable on disconnect regardless of the owner's repo role**
   (`governor.labels.stage.adoption.maintainer_policy`, default `adopt`;
   owner decision 2026-10-28 on hivecommons/hive#14524, where a
   maintainer's own `author/clanker` PR sat at 072 for 26h with its hive
   gone while every sweep logged "author maintains the repository; not
   adopting"): a PR a contributor's clanker opened is the hive's to stand in
   for when that hive goes away, whoever owns the account. `ask` holds a
   maintainer's PR for an owner's decision instead — the stage record
   notes `author-is-maintainer`, the activity monitor row reads
   "waiting on @author (maintainer, offline 26h) — adopt?" and its menu
   offers **Adopt**, which sets `adoption/supersede` (the owner-delegate's
   own request label) so the next sweep adopts as `contributor-offline`;
   `never` is the earlier hands-off rule (projectbluefin/utah-packages#619):
   never adopt, and release an existing adoption of a maintainer's head.
   Under every setting a human-authored PR (no `author/clanker`) is never
   adopted.
   The activity monitor shows the same presence the adoption rules read, so
   the operator can anticipate the next move ([hivecommons/hive#15051](https://github.com/hivecommons/hive/issues/15051)):
   every PR row waiting on a named person (071, 072, 017) carries
   `presence{login, state, kind, since, last_seen, next, adopt_at}` and
   its waiting-on pill an indicator before the `@login` — a filled **dot**
   when the author's contributor agent is online (`next: wait`: the hive waits for
   them through the online grace), a filled **square** when it is offline
   (`next: adopt`; `adopt_at` is the projection of the rule below, and the
   tooltip reads "@hanthor's clanker: offline 26h — hive will adopt at
   15:10 (in 38m)"), a **hollow ring** when no contributor agent is registered here
   (a human's PR: "no clanker registered — waiting for a human reply", pill
   `waiting on @zoe (human · no clanker)`; another hive's PR: the
   unregistered clock). A held adoption (`ask` / `never`, or
   `adoption.enabled: false`) keeps `next: wait` with no `adopt_at`, so the
   indicator never promises an adoption the pill text denies.
   An operator can end the wait early from the activity monitor
   row (hivecommons/hive#15038): **Adopt now** (decision API `adopt` with
   `force: true`) posts `<!-- hive:adoption-requested by=<login>
   force=true -->`, sets `adoption/supersede` and marks the stage record
   forced, so the next sweep adopts at once in supersede mode whatever the
   author's presence, the grace windows or `maintainer_policy` say (`hold`
   still blocks it); the marker that follows reads `reason=operator-forced
   by=@<login>` and, when the author's hive is online, the adoption comment
   says the maintainer took it over. **Reroute** (decision API `reroute`
   with `to: <login>|any`) hands the follow-up work to another online
   contributor agent's fix queue and posts `<!-- hive:rerouted to=@X by=@op
   reason=… -->` on the PR; if that relay has not picked the task up within
   `governor.reroute.pickup_timeout` (default 30m) the hive posts
   `<!-- hive:rerouted to=@X by=@op lapsed=true -->` and falls back to
   Adopt now on its own.
   - `reason=operator-forced by=@<login>` — an operator's Adopt now (or the
     reroute pickup fallback acting for them); presence, grace and
     `maintainer_policy` were not consulted;
   - `reason=contributor-offline` — the author has a contributor profile
     here and no relay heartbeat within `governor.labels.stage.presence_window`:
     adopted `governor.labels.stage.adoption.after_offline` (default 15m)
     after the later of its last heartbeat and the first nudge;
   - `reason=contributor-unregistered` — no profile here (another hive, or a
     relay that never registered, posted the PR): adopted
     `governor.labels.stage.adoption.after_unregistered` (default 15m) after
     the first nudge;
   - `reason=grace-elapsed` — the relay is online but the nudge went
     unanswered for `governor.labels.stage.contributor_grace_online` (default
     2h); and, on a hub that reports no presence at all, the nudge-time
     graces (`contributor_grace_online` 2h / `contributor_grace_offline` 15m
     after a comment-only nudge);
   - `reason=dco-rehome` (always `mode=supersede`) — not a fresh adoption
     but a second takeover: an in-place-adopted head stayed
     DCO-red with unsigned commits the hive cannot rewrite on the contributor
     branch (see the DCO paragraph below). The stage record's `adopt_reason`
     moves to `dco-rehome` as well.
   - `reason=signoff-rehome` (always `mode=supersede`) — an adopted head
     carries an App-signed fix that cannot survive a squash attributed to
     the contributor (`hive-signoff-attribution`). The lane still prefers
     a **merge commit** when its existing trusted-role and policy gates
     permit one. Otherwise it immediately opens an App-authored superseder:
     the same commits and sign-offs survive unchanged, but the squash is
     now attributed to the App. DCO and merge checks still apply. This also
     recovers heads parked by older builds (#13975).
   - `reason=push-rehome` — push access to an adopted contributor branch is
     no longer available; supersede immediately rather than wait on the
     offline contributor.
   - `reason=rebase-rehome` — `needs-rebase` / conflict work on a pushable
     adopted branch first reaches the normal fixer, which rebases onto the
     PR base and pushes to the actual fork with a head-bound lease,
     preserving authorship and sign-offs. If the head stays idle past the
     074 dwell, supersede in that sweep rather than escalate to 085. Each
     fixer attempt is routed once and gets
     `governor.labels.stage.adoption.rebase_attempt_timeout` (default 15m)
     to push; one with no push counts as failed and the next attempt is
     dispatched (the row reads "Rebasing · attempt 1/2 · waiting for
     scanner 12m" meanwhile). Once both attempts are spent the adoption
     supersedes at once instead of waiting out the dwell (#16023).
   - `reason=in-place-stalled` (always `mode=supersede`) — second takeover:
     an in-place-adopted head sat unchanged for the 074 dwell (3h) with CI
     idle (not pending) and the review swarm's latest verdict on it
     `changes_requested`. The in-place lane repairs CI only, so a green head
     with an open finding is never worked there; the superseding PR body
     names the carried-over finding and tells the fixer green CI alone is
     not done. The sweep re-evaluates existing 074 records every pass, so a
     record adopted before this rule existed converts too
     (hivecommons/hive#12858).
   - Stale-red recovery (no re-home): an in-place head red only from a
     since-fixed base/shared-CI flake — its failing check is green on the
     base tip — is updated from the base when behind, or has its failed
     jobs re-run when current, once per (head, base tip), before the dwell
     escalates it to 085 (hivecommons/hive#14500).
   **When hive adopts automatically.** One predicate —
   `automerge.AutoAdoption` — answers both "may the activity monitor offer
   Adopt / say when hive adopts" and "does this sweep adopt". Whenever the
   tracker would say `hive adopts it on the next sweep`, that sweep does it;
   no click is needed (owner rule, 2026-10-29 on hivecommons/hive#15565,
   #15566, #15609 and #15312: PRs by an offline clanker sat at 072 with
   "waiting on @author (offline 13m, 1 nudge)" until a person pressed
   Adopt, while the sweep's own clock ran from a later heartbeat). The hive
   adopts an `author/clanker` PR on its own when **all** of these hold:
   - the PR is at 072 with work its author's clanker is not doing: a
     conflict with the base, changes requested by the review swarm, a
     failing check — required, or an optional one the merge gate reads
     (`optional-check-failing:<name>`), DCO / sign-off, a diff the lane
     cannot read, or a fork the App cannot push to (`fork-not-editable`:
     hive cannot rebase it, so re-authoring on a hive branch is the only
     mover; the caption reads *fork not editable by hive*);
   - the author was nudged on **this** head and the head has not moved
     since — a clanker that is pushing is never adopted; a new head
     restarts the nudge and the clock;
   - the author is unavailable by the clock above: offline for
     `adoption.after_offline` (15m) counted from the later of the first
     nudge and the last heartbeat, where a heartbeat after the nudge
     ("acknowledged") buys **one** window at most — a relay that
     reconnects every few minutes without pushing no longer resets it;
     online but silent for `contributor_grace_online` (2h); unregistered
     for `adoption.after_unregistered` (15m). A `mode=queued` nudge (the
     task waits server-side) counts as offline, not online;
   - nothing holds it: no `hold` / `do-not-merge`, not person-authored, and
     `maintainer_policy` is `adopt` (the default) or the owner answered `ask`.
   The adoption log line carries `trigger=automatic:<reason>`
   (`operator-forced` / `operator-requested` for the manual paths); the
   tracker's 072 pill reads `… — hive adopts it in 3m unless a new head
   lands` while the clock runs and `… — hive adopts it on the next sweep`
   once it has. **Adopt now** stays as the override for a clock that has
   not run out yet. Under `maintainer_policy: never` the engine has not
   read the author's repository permission yet at this point — it only
   does so once due — so the pill cannot promise that outcome for every
   author; it names the exception instead: `… unless the author maintains
   this repository` (hivecommons/hive#15692).
   A PR carrying `hold`, `do-not-merge` or `do-not-merge/hold` is never
   adopted (a human parked it; one log line, the stage stays 072).
   A PR whose diff trips the contributor lane's diff fences — it touches
   `.github/workflows/` or `.github/actions/`
   (`contributor-lane:workflow-path`), deletes a test file, or guts tests
   (`contributor-lane:tests-removed`) — IS adopted: adoption is not a
   merge. Refusing it (hivecommons/hive#12850) left exactly the PR the
   fence protects with nobody working it: red CI, `needs-rebase`, an absent
   author, and `adoption due but refused by diff fence` on every sweep
   (hivecommons/hive#13212). The fence's purpose is to stop a fenced diff
   being laundered into an App-authored PR the self-authored lane then
   merges unattended, so the hive adopts and carries the fence forward
   instead. It logs `adopting contributor PR despite diff fence; merge
   stays manual` with `fence=<reason>`, records the reason on the stage
   record (`adopt_diff_fence`), and the adoption comment ends with "The
   merge stays manual: …". What that means per mode:
   - **in place**: the PR stays contributor-authored and the contributor
     lane re-reads its diff on every pass, so the fix lane rebases and
     repairs CI on the PR's branch (074) and the green head lands at
     `stage/085-needs-decision` with "a maintainer must review and merge
     this PR manually" — the lane does not merge it unattended, and the 085
     note says the hive adopted it to repair CI and why it still will not
     merge it (for `workflow-path`, a trusted merger's approval on the
     current head releases the fence, [#14664](https://github.com/hivecommons/hive/issues/14664)).
   - **supersede** (and the `dco-rehome`, `signoff-rehome` and
     `in-place-stalled` second takeovers): the App PR's body opens with
     `<!-- hive:carried-fence reason=<contributor-lane:…> from=#<n> -->`
     and a **Merge stays manual** paragraph. Every App merge lane
     (self-authored, trusted-author, `hive-merge`-queued) refuses a PR
     carrying that marker once it is green, with the carried
     `contributor-lane:*` reason, so it routes to 085 like a contributor PR
     with the same diff; `/hive approve` says so too. The hive's own
     pushes to the superseding branch do not lift it. A second takeover
     no longer releases the adoption back to 072 over the fence.
   Only a verdict the fence could not reach still defers adoption, with no
   attempt recorded, so the next sweep re-reads: the fence reads the PR
   itself and judges only the head the lane recorded, so a head that moved
   in between (`head-changed-since-eval`), a file list shorter than
   GitHub's `changed_files` count (`contributor-lane:files-evidence`) or a
   files-API error all wait for the next sweep. The same deferrals apply
   to the `dco-rehome` takeover, which keeps the adoption meanwhile.
   How the hive takes over depends on whether it can push to the PR's own
   branch (`governor.labels.stage.adoption.in_place`, default `true`):
   - **in place** (`mode=in-place`) — the head is in the base repo, or the
     fork allows edits by maintainers (`maintainer_can_modify`, resolved with
     one `GET pulls/N` at adoption time): the hive posts ONE
     `<!-- hive:adopted by=<hive-id> reason=<r> mode=in-place head=<sha> -->`
     comment, moves the PR to `stage/074-adopted-by-hive` and files it in
     `ci-failing.json` as its own (`"adopted": true`, `reachable_action:
     push`, conflicted rows included — ordinarily only hive-authored
     conflict PRs are listed) so the scanner's FIX-BEFORE-NEW kick rebases
     and fixes CI **on the PR's branch**. No superseding PR, no `hive/adopt-<n>`
     branch — with one exception, the DCO re-home described below, which
     converts the in-place adoption into a supersession. The hive's own
     pushes keep the adoption and the stage stays
     074 whatever CI or the review swarm say of them — pending, green,
     approved or red again — until the merge gate (080/085/090/100) takes
     the head (hivecommons/hive#12736). The hive owns that head: when it is
     green, reviewer-approved, DCO-clean and soaked, the contributor lane
     admits it under the hive's own authority
     (`contributor-lane:adopted-by-hive`, `governor.labels.stage.adoption.auto_merge`,
     default `true`) without the original author's trust tier, so 074 moves
     to 095/097 and merges instead of parking on a maintainer `/lgtm`. A later head keeps that ownership
     only when the commit's GitHub-linked author/committer is the App bot
     (the stage store remembers SHAs that already passed that check; it is
     not a separate evidence source)
     (`governor.labels.stage.adoption.strict_attribution`, default `true`).
     Hive launches push-capable App-authored agents with the App bot noreply
     email (`<bot user id>+<slug>[bot]@users.noreply.github.com`, resolved via
     `GET /users/<slug>[bot]` — the bot's user id, not the App id) as their
     git author/committer address, so normal fixer commits link
     to the App while the commit name/sign-off still show the lane. Commit
     author or committer **names/emails** that are not GitHub-linked are not
     trusted, because a contributor can forge them. On the first deploy (or
     old sessions) before any hive-push evidence has ever been recorded for
     the repo, the reconciler logs a warning and keeps ownership rather than
     reintroducing the yield loop; after evidence exists, a head without this
     hive-controlled evidence means the contributor is back: the hive yields — ONE
     `<!-- hive:adoption-yield head=<sha> -->` note, the adoption is forgotten
     and the PR is back on the ordinary 072 flow (a fresh nudge for the new
     head; it may be adopted again under the same rules).
     A review verdict of `changes_requested` on an in-place adoption is the
     hive's work: the review-fix dispatcher may push signed fixer commits to
     that adopted branch and keeps the PR in the fixer's work list every cycle
     until a new head lands. If the bounded fix-attempt cap is exhausted, the
     PR is escalated visibly with reason `fix-attempts-exhausted`. When no
     such fix lands for the 074 dwell the adoption converts to a supersession
     (`reason=in-place-stalled`, above) rather than sitting silently.
     **Stalled adoption → 085.** Whatever the mode, an adopted PR the hive
     owns on paper but nobody is working — the head unchanged for the 074
     dwell with CI idle and no re-home applicable, or a supersession whose
     `hive/adopt-<n>` push or PR create failed — moves to
     `stage/085-needs-decision` with lane reason
     `adoption-stalled:<lane reason>` (e.g.
     `adoption-stalled:reviewer-verdict:changes_requested`,
     `adoption-stalled:check-failing:unit`,
     `adoption-stalled:supersede-failed:dco-rehome`,
     `adoption-stalled:rebase-failed-twice` when the hive's copy still
     conflicts after the fixer's two rebase-resolve attempts) and ONE
     `<!-- hive:adoption-stalled head=<sha> reason=<r> -->` comment naming
     the exact blocker (the verdict and its summaries, the red check, or the
     failed re-home). The dwell is measured from the head, not the adoption:
     each hive push restarts the clock, and a record written before the head
     clock existed starts it at the first sweep that sees it rather than
     reading days back to the original adoption (hivecommons/hive#13040). A
     red **optional** check alone (`optional-check-failing:<name>`) is never
     the reason: the merge gate does not read it, so there is nothing for a
     person to decide and the PR stays 074. The escalation holds only while
     its reason holds — the sweep re-evaluates it every pass and when the
     blocker clears (CI re-runs, the check goes green, a verdict lands) the
     PR returns to 074 and the lane carries on; the comment is posted once
     per head even if the blocker flaps, and a hive push to a new head
     starts over. A pending `tide` or missing `lgtm` is never the reason —
     the lane has not admitted the head; the comment says so.
   - **supersede** (`mode=supersede`) — the fork forbids maintainer edits,
     `in_place: false`, or the adopted head has unsigned commits (`no-dco`)
     that the hive cannot repair by amending the contributor branch:
   - pushes `hive/adopt-<n>` in the base repo at the contributor's head SHA
     (the fork's commits, authorship and `Signed-off-by` preserved byte for
     byte — "sign-off survives the squash");
   - opens an App PR from it against the same base, titled like the original,
     whose body starts `Supersedes #N (adopted after CI went red and the
     contributor was unavailable; original commits by @author preserved)`,
     carries the original's `Closes/Fixes/Resolves` lines and instructs the
     fix agent to rebase on the base and fix CI **on that branch** without
     opening another PR;
   - **close-first** (`governor.labels.stage.adoption.supersede.close_first`,
     default on; hivecommons/hive#15135 — the operator saw two active PRs
     for one issue, #15029 and its copy #15123, and had not asked for
     either): in the same pass, right after the copy opens, posts ONE
     `<!-- hive:adopted by=<hive-id> reason=<r> mode=supersede
     superseded_by=M head=<sha> close_first=true -->` comment reading
     "Superseded by hive: re-authored as #M with the same commits and
     sign-off because <the author's clanker is offline / an operator asked
     for it / CI stayed red and the author was unavailable>. This PR is
     closed; if #M fails, hive reopens it.", applies `hive/superseded`, and
     **closes the original**. The order is push branch → open copy → note →
     label → close, never with a sweep between, so the note names the copy
     and the Activity Monitor never shows the original as active once the
     copy exists: the stage record is superseded (no row, no chevron, no
     needs-human) and the copy is the one PR for the issue. A close GitHub
     refuses (403/404) leaves the original open with the note, superseded
     all the same. The note, label and close are unsolicited writes on a
     contributor's PR, so the pass first asks the write-back gate as kind
     `contributor_adopt` (default L6 + opt-in label; see the write-back
     table in
     [operator-reference.md](../operator-reference.md#what-hive-writes-to-github-and-when)):
     refused, nothing is written and the original stays open on the
     keep-open path below until the copy merges. A clanker- or
     hive-authored original does not need the label: the gate admits its
     close as `superseded-agent-pr` by policy
     (`governor.labels.stage.adoption.close_superseded`, `on_adopt` by
     default, `on_merge` to keep it open until the copy merges), because
     the copy carries the same commits and sign-off
     (hivecommons/hive#15526). `close_first: false` restores the keep-open behaviour
     below: the note has no `close_first` field, the original moves to
     `stage/074-adopted-by-hive` (its linked issue mirrors the stage) and
     closes only when the copy merges. The hub drops the PR from the
     contributor's fix queue in every mode.
   - **Reopen on failure** (close-first only). The hive polls the copy from
     the stage store at most every 10 minutes (a closed original is in no
     lane's listing). Copy merged → done. Copy closed unmerged, or its
     required checks red at **two** distinct copy heads (the fixer's one
     repair push did not take; the same loop cap as the rebase/adoption
     guard) → the original is **reopened**, gets ONE
     `<!-- hive:adoption-copy-failed superseded_by=M head=<sha> why=copy-closed|copy-red-twice -->`
     note ("Hive's copy #M failed (<why>); reopening this PR; needs a
     person"), loses `hive/superseded`, and parks at `stage/085` with reason
     `adoption-copy-failed` (tracker word "Adopted → #M (copy failed) ·
     reopened, needs a person"; the park holds until a person acts, an
     `adoption/supersede` request re-runs the takeover from the same head).
     **Ping-pong guard:** one reopen per original per 24h — a second failed
     copy inside the window leaves the original closed with a `held=true`
     note saying so. The reopen, its note and the label removal also go
     through the `contributor_adopt` write-back gate: refused, nothing is
     written and the record stays due its next poll, so a lifted repo pause
     lands the reopen; a policy change after the close is warned in the log
     and leaves the hive-closed original shut until a person reopens it.
     The orphan recovery below never runs for a close-first
     original (its copy being closed is a failure, not an orphan); the
     `hive:adoption-copy-failed` note is a settled marker, so the marker scan
     never re-adopts a reopened original on its own.
   The superseding PR is App-authored and red with the same failures, so the
   existing machinery takes over: it lands in `ci-failing.json` and the
   scanner's FIX-BEFORE-NEW kick (the route every red hive-authored PR takes,
   [hive-merge.md](../hive-merge.md#stage-labels)) rebases and fixes it on
   `hive/adopt-<n>`; it then runs the normal `050 → 100` path, while the
   original stays 074 at the adopted head whatever its own CI says. The only
   second takeover is the DCO re-home of an in-place adoption (below); a
   superseded PR is never re-homed again. One adoption attempt per
   contributor head, whatever the outcome — the DCO re-home likewise records
   its attempt before touching GitHub, so a failed `hive/adopt-<n>` push is
   not retried on the next sweep. While in 074 (keep-open mode,
   `close_first: false`) the hive polls the superseder at most
   every 10 minutes: merged → one `<!-- hive:adopted-merged -->` note and the
   original is closed; closed unmerged → the adoption is **orphaned** and the
   hive recovers it rather than leaving the original parked
   (hivecommons/hive#15052, where the duplicate-fix sweep closed the hive's
   copy and the original sat at 085 with "Let hive decide" answering "no
   change"):
   - **Re-adopt.** The hive logs `stage labels: adoption orphaned,
     re-adopting` (old and new numbers), opens a fresh hive-authored copy
     from the same contributor head and posts a new `hive:adopted … mode=supersede
     superseded_by=<new>` marker; the original stays 074 and the tracker reads
     "Adopted → #old (closed) · recovering" until the new copy is on record,
     then "Adopted → #new".
   - **Revert.** If the head moved (or the original merged meanwhile) or the
     fresh copy could not be opened, the adoption is cleared and the original
     returns to its pre-adoption stage with one
     `<!-- hive:adoption-reverted head=<sha> superseded_by=<old> why=head-moved|readopt-failed -->`
     comment that says in plain words why.
   - **Loop guard.** If the re-adopted copy is itself closed unmerged within
     an hour of the first orphan, the hive stops opening copies: it posts
     `<!-- hive:adoption-child-closed-twice -->`, parks the original at
     `stage/085-needs-decision` with reason `adoption-child-closed-twice`, and
     the tracker shows "Adopted → #N (closed twice) · needs a person". Reopen
     the last copy, give direction, or close the original; the park is not
     escalated or re-adopted by the hive.
   - **Marker scan.** A hive restarted (or rolled) after the orphan happened
     has no adoption on record, so once per head a PR at 085 wearing
     `author/clanker` has its comments scanned for the last unsettled
     `hive:adopted … mode=supersede` marker; a merged copy closes the
     original the standard way, a closed one takes the recovery above, an
     open one is restored to the record. *Let hive decide* on such a PR does
     the same recovery instead of answering "no change" — its outcome reads
     "Re-adopting: hive's earlier copy #N was closed by mistake; opening a
     fresh copy" (or, past the loop guard, a recommendation to a person).
   If an adopted head is DCO-red, an empty signed commit is never enough.
   "Hive-authored" is decided the same way as head attribution above: an
   unsigned commit is the hive's own only when GitHub links its author or
   committer to the App bot; git author/committer names and emails — even a
   lane-looking `<lane>@<HIVE_GIT_BOT_EMAIL_DOMAIN>` address — are
   contributor-controlled and never count. The stage pass caches that split
   with the commit check (`dco_unsigned_hive`), so the decision does not
   depend on which labels the PR currently carries. When every unsigned
   commit is App-linked and the head lives in the **base repo**, an in-place
   adoption stays in place and the stage engine itself re-signs the branch
   once per head (`reason=dco-rehomed`): it replays the PR's commits through
   the git-data API, keeping each author, message, tree and parents (merge
   commits from a base sync included) and appending the App's
   `Signed-off-by`, then force-updates the ref only if it still points at the
   observed head. The engine's installation token cannot rewrite a fork
   branch, so a fork head — even one whose only unsigned commits are the
   hive's own — and any head with a contributor-authored or unknown unsigned
   commit converts to `mode=supersede` (`reason=dco-rehome`): the hive
   pushes `hive/adopt-<n>` at the head with the same re-signed replay
   (falling back to the unsigned head only when the replay cannot be
   built), opens the superseding PR and the fix agent re-homes the diff onto
   that hive-owned branch from the base: cherry-pick each original commit
   with `git cherry-pick -s`, or squash with `git commit --author=<original
   author> -s`, preserving authorship/credits while adding the hive
   submitter sign-off ([#14666](https://github.com/hivecommons/hive/issues/14666)). The replacement PR keeps the
   original title, starts with `Supersedes #N`, carries the original
   `Fixes/Closes/Resolves` target (including `Hive-Run: owner/repo#NN` as
   `Fixes #NN`), and the original PR is commented and closed so it does not
   sit forever on a DCO-required branch rule.
4. A new head from the author at any point before adoption re-arms the nudge
   (grace restarts) or, if green, returns the PR to the normal `050 → 100`
   path; no adoption. After a superseding adoption the author can still push
   a new head on the original and say so on the superseder; after an
   in-place adoption their push makes the hive yield (above).
   The contribute page's "PRs awaiting rework" list for a relay shows
   `adopting (contributor offline)` on a nudged PR whose relay connection is
   stale, and `adopted by hive` once the PR is in 074.
5. Backpressure — the per-contributor fix budget. Capacity = the relay's
   advertised `max_parallel_fixes` (fix sub-sessions beside the primary),
   clamped by `contribute.max_parallel_fixes` (default 2); an unadvertised
   relay has capacity 1 (preempt mode). At most that many `fix-pr` tasks are
   in flight per contributor; the rest wait in a per-contributor FIFO
   (oldest PR first, a human changes-requested before CI-red) carrying stage
   072 and the GitHub comment only. Preempt mode allows ONE preemption at a
   time and never preempts again until the parked task has resumed and run
   for `contribute.min_focus_window` (default 30m). Failures with the same
   cause (same failing check + error signature) are coalesced into one task
   listing every affected PR. A queued PR whose adoption grace expires goes
   to 074 instead of interrupting — the hive absorbs the overflow, not the
   human. Queue depth / in-flight is exposed as `fix_queue` on the
   contributor profile API and on the contribute page; the hub logs one line
   per enqueue/dequeue (`fixqueue: … event=…`).
6. Governing rule of the contributor fix scheduler, verbatim from the
   operator: **"contributors should clean up their mess before taking on new
   stuff."** Every ordering decision — task boundary, dequeue, a new-issue
   claim via the relay or the contribute page — checks the contributor's
   red-PR queue first: queued `fix-pr` tasks are handed out oldest first
   (even to a relay that advertises no fix-pr support, as an ordinary task)
   and a fresh issue is served only when that queue is empty. The contribute
   page shows "You have N PRs to fix first" and offers those PRs before new
   issues. Preemption of in-progress work still obeys the single-preempt +
   focus-window rule; this ordering applies at task-boundary time.
7. Capacity ≥ 2: fixes are dispatched immediately as parallel sub-sessions,
   up to the advertised count so the primary session keeps going and is
   never preempted; further failures queue, and each completed fix session
   (`task_complete` for the fix's `task_id`) frees a slot that is refilled
   automatically with the next queued `fix-pr`. A `task_failed` requeues the
   fix with backoff instead (three attempts, then the re-nudge / adoption
   path; see [contributor-relay.md § fix-pr tasks](../contributor-relay.md#fix-pr-tasks)).

All seven steps ship with the stage engine and the contribute hub
(hivecommons/hive#12436).

### Supersession: closed issue, another merged PR

When the supersession sweep finds an open PR whose closing claim (`Closes
#N`) names an issue a *different* merged PR #M already closed, who decides
depends on the author facet and on whether the PR adds anything #M did not
(`src/pkg/github/pr_supersession_sweep.go`, `pr_supersession_resolve.go`):

| PR | Files ⊆ #M's files | Files #M did not touch |
|---|---|---|
| `author/hive` at ACMM L6 | closed with `hive/superseded` | **self-resolved**: head behind base → GitHub update-branch requested, measured next pass; head current → body `Closes #N` rewritten to `Refs #N (closed by #M; this PR adds <files>)` with the `<!-- hive:supersession-resolved issue=N closer=M -->` marker, one notice, PR continues in its lane; nothing meaningful left → closed with `hive/superseded` |
| `author/hive` below L6 | `needs-human` + `Parked` fact → `stage/085-needs-decision` | same |
| `author/human` / `author/clanker` | notice, Review Queue decision, optional grace-window close | closing claim: `needs-human` + `Parked` fact → `stage/085-needs-decision`, notice lists the files; reference only (`Refs #N`, `Part of #N`, branch name): notice lists the files, no park |

**Conflicting hive PR (`governor.auto_close_superseded`, default on).**
A hive-authored PR whose closing targets are all closed, one of them by a
different merged PR, and whose head GitHub reports as conflicting
(`mergeable_state: dirty`, or update-branch refused with HTTP 422 "merge
conflict") has nothing of its own left to rebase and measure: at L6 the
hive closes it itself — `hive/superseded`, one notice naming #M (edited in
place when the sweep already left one), stage engine told it is closed —
without asking a person. The same rule runs in three places so no review
cycle is spent on it: the self-authored auto-merge lane, before any verdict
or gate is read; the supersession sweep's self-resolution; and
close-on-merge of #M (`CloseSupersededHivePRsForMergedPR`), so the
superseded PR closes in the same breath as the merge. A hive PR whose files
are a subset of #M's closes the same way whether or not it conflicts. A
human-authored PR is never auto-closed by this rule (it keeps the notice
and the decision); a hold / exempt label, `hive/supersession-keep-open`, a
`needs-human` a person applied, or a hive below L6 keeps the PR open. With
the knob off the previous behaviour stands: the refusal is reported once
and the PR waits for a person. hivecommons/hive#14965 (`Fixes #14952`,
closed by #14957's merge, conflicting) sat at `stage/085-needs-decision`
for over an hour this way until the owner clicked close-superseded.

The sweep never writes `needs-human` without the `Parked` fact, so the
chevron and the label agree in the same pass; and it never parks a hive PR
at L6 for a decision the hive can make. A contributor PR that only
*references* the closed issue never claimed to deliver it, so its closer did
not supersede the PR: it gets the notice, not the park (#13959 and #14090,
`Part of` / `Refs` a closed tracker, sat at 085 for this —
hivecommons/hive#14155). A `needs-human` a person applied is an
instruction: the sweep leaves that PR alone. The hive's own `needs-human`
clears with its cause: it is released when the hive takes a hive PR's
resolution over, and on any PR when the supersession the notice announced no
longer holds (claimed issue reopened, claim rewritten, link downgraded to a
reference) — the sweep's "Decision needed" notice plus a hive-applied label
is the sweep's park; the stage engine re-derives the lane stage on the
next auto-merge pass (leaving 085 is never a regression). The body marker is
the idempotence key: a resolved PR still references a closed issue, and
without the marker the next sweep would treat it as a fresh supersession.

### Modifiers, not stages

These existing labels qualify a stage and may coexist with any stage label;
the reconciler never touches them: `hold` / `hold/*`, `needs-human`,
`blocked`, `needs-rebase`, `hive:unauthorized` (the App lacks a permission
for the next step; see the reference table),
`hive/likely-done`, `hive/partially-addressed`, `hive/verified-open`,
`hive/claimed-by-<who>` (the grey "who" modifier a stage cannot carry),
`lgtm`, `role/*`, `author/*`, `dco` / `no-dco`, and the agent provenance
labels.

`hive/partially-addressed` and `hive/verified-open` are *merged-PR evidence*,
not stages, and stay coherent with the stage model: both describe what a PR
that already merged did (or did not) finish, while the stage says where the
issue is *now*. An issue wearing them is back in the pipeline — `010` once
nothing covers it, `020`/`030`/`040` as new work starts — and the two labels
tell the next kick and the close-on-merge sweep how to treat it: do not
re-verify, do not label `hive/likely-done` from the same stale claim,
implement the remainder (`src/pkg/scheduler/kickmessage.go`,
`src/pkg/github/prclaims.go`, `src/pkg/github/task_list_sweep.go`,
`src/cmd/hive/verifyonce.go`). They are not a second stage and are not
retired.

#### `hive/claimed-by-<who>`: one rule for the suffix

`<who>` names the **kind of party** accountable for the claim, derived from
the claim marker by `claims.ClaimedByLabel` (`src/pkg/claims/claimedby.go`).
A GitHub login is **never** part of a label name (owner decision 2026-10-19,
reported from #13041 / #13042 where `hive/claimed-by-clubanderson` appeared):
a per-login label mints one label per person in every repo and leaks
identities into the label vocabulary. The claimant's login lives in the
claim record (ledger), the `<!-- hive:claim who=<login> … -->` marker
comment and the dashboard claim detail (`@login` next to the claim).

| Claim | Marker | Label |
|---|---|---|
| A person — dashboard, `/claim`, hivectl, or a Copilot CLI / Claude Code / Codex session acting on their behalf | `kind=human who=<login>` | `hive/claimed-by-human` |
| A relay contributor or external bot | `kind=contributor` / `kind=external`, `who=<login>` | `hive/claimed-by-human` |
| A hive agent with no human principal | `kind=agent who=<agent>` | `hive/claimed-by-<agent>` (e.g. `hive/claimed-by-scanner`) |

A backend or model name (`copilot`, `claude`, `codex`, `gemini`, `pi`) is
**never** a suffix: it names the tool, not who answers for the work. A
hive-agent claim whose name is a backend falls back to
`hive/claimed-by-agent`; a human/contributor claim with no usable login gets
no label. An agent name is reduced to `[A-Za-z0-9_-]`; `human` is reserved
(no agent may be named `human`); `session=` / `hive=` are not inputs. The
writers all go through the helper — the claim relay (`hive-open-issue claim`,
`src/pkg/github/issue_inprogress.go`) and the claim-ledger hooks
(`src/cmd/hive/claimswire.go`, `Client.ApplyClaimedByLabel`), which add the
claim's own label and strip only backend-named `hive/claimed-by-*` labels
and the claim holder's own retired per-login label when a claim lands
(another party's label is kept unless a new agent claim finds trusted App
comments proving the latest human-principal claim was explicitly released;
then its stale `hive/claimed-by-human` label is cleared), and remove the holder's labels — only the
holder's, generic plus legacy per-login — when the ledger releases or hands
the claim over. The ledger maintains at most one live claim per (repo,
issue); thus, co-party `hive/claimed-by-*` labels on the same issue arise
only from hand-applied labels or persistent debris left by an earlier holder,
never from the ledger modeling concurrent co-parties. The issue reconciler
heals the malformed forms a hand-applied or pre-upgrade label can introduce:
a backend-named suffix is swapped for the live holder's label (dropped when
the issue carries no live claim) and the live human holder's
`hive/claimed-by-<login>` is swapped for `hive/claimed-by-human`; any other
label is never touched.

**Retired per-login spelling.** `hive/claimed-by-<login>` is still *read* as
a human claim everywhere — every consumer (GFI exclusion, the dashboard
claimed band, the `issue:claimed` your-turn kind, the origin-marker signal
list) matches the `hive/claimed-by-` prefix, so nothing changes meaning
during the transition. Nothing writes it any more. On boot the one-shot
`MigrateClaimedByHumanLabels` sweep (`src/pkg/github/claimed_by_human_migration.go`,
marker `/data/claimed-by-human-label-v1.done`, report
`/data/claimed-by-human-label-v1.json`) lists the `hive/claimed-by-*`
definitions in each configured repo, skips the generic label, the `agent`
fallback, backend names and this hive's agent names, moves every open item
carrying a remaining (per-login) label to `hive/claimed-by-human`, records
the login it carried in the report and the write audit
(`claimed_by_label_migrated`), and deletes the per-login definition once no
open item carries it. An item whose live ledger claim is a hive-agent claim
named by the label (another hive's agent on a shared repo) is skipped and
that definition kept. A failed write leaves the marker unwritten so the
next boot retries.

### Migration from retired labels

`🐝 covered by PR` (and its pre-#11888 spelling `hive/covered-by-pr`), the bare
`claimed` label and `hive/in-progress` are **retired**: their meaning is
carried by `stage/040-covered-by-pr`, `stage/020-claimed` and
`stage/020-claimed` / `stage/030-pr-in-progress`. Claim *detection* never read
those labels — it uses the `<!-- hive:claim … -->` marker comment
(`claim_source=marker`), assignees and the ledger — so removing them changes
no gate. On the first cycle after upgrade the reconciler removes them from
every open item it writes a stage for, so open items lose
`claimed` / `hive/in-progress` / `🐝 covered by PR` and gain their stage label;
the repo-level labels can be deleted afterwards. The legacy writers are gone:
the claim relay and ledger mirror write `stage/020-claimed` /
`stage/030-pr-in-progress`, the PR-claim sync writes `stage/040-covered-by-pr`,
and the in-progress sweep still recognises `hive/in-progress` as a trigger for
one migration cycle so a lapsed mark from an older spoke is cleared rather
than orphaned. `hive/claimed-by-<who>` is kept as the grey "who" modifier.

Prow's `dco-signoff: yes` / `dco-signoff: no` are **not** retired by the
hive (#13125): Prow and the DCO workflow own them and re-add them on every
sync, so the stage pass leaves them alone and only writes its own `dco` /
`no-dco`. Readers accept the Prow pair as a fallback verdict. Retiring them
is a Prow `plugins.yaml` (kubestellar/infra) change, not a hive write.
