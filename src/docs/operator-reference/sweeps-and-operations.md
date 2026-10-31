# Hive operator reference

[Contents](../operator-reference.md)

## Supersession sweep (`supersession_sweep`)

The supersession sweep (`src/pkg/github/pr_supersession_sweep.go`) looks for
open PRs whose claimed issue is already closed by a *different* merged PR. It
keeps one `<!-- hive:pr-supersession-sweep -->` notice per PR, edited in place.

- **Which issues count.** When the PR closes issues with a closing keyword
  (`Fixes`/`Closes`/`Resolves #N`), only those are candidates; a bare
  `Part of #N` / `Refs #N` reference is context and a closed parent reached
  that way never supersedes a PR. Only a PR with no closing keyword at all
  falls back to its branch-name issue and bare references. If **any**
  candidate issue is still open the PR is live work and the sweep leaves it
  alone — no notice, no label, no close (#12349: #12322, `Fixes` open
  #12117 and `Part of` closed #12080, was escalated to `needs-human`).
- **Hive-authored PRs** are closed immediately at ACMM L6 when every file they
  touch was also touched by the merged PR. When they touch files the merged
  PR did not, the hive resolves the overlap itself instead of asking a person
  (`pr_supersession_resolve.go`): if the head is behind the base it requests
  GitHub's update-branch (`branch_update` effect) and waits for the new head;
  once the head is current it re-reads the files, and either closes the PR
  (nothing left, or only files the merged PR already changed) or rewrites the
  body's `Closes #N` to `Refs #N (closed by #M; this PR adds <files>)`, stamps
  `<!-- hive:supersession-resolved issue=N closer=M -->`, edits the notice and
  lets the PR continue in its lane. A refused update (conflict) is noted once
  per head and left to the fix lane. The hive's own earlier `needs-human` is
  released when it takes the resolution over; one a person applied is
  honoured and the PR is left alone. Below L6 the PR is escalated instead.
- **Escalation goes through the stage engine.** Whenever the sweep hands a
  PR to a person (contributor PR with files the merged PR did not touch, or a
  hive PR below L6) it adds `needs-human` *and* submits a `Parked` fact, so
  the PR lands at `stage/085-needs-decision` in the same pass — never a bare
  `needs-human` beside an active lane stage (#13782 wore
  `stage/050-review-pending` + `needs-human` until the next auto-merge tick).
  Only a closing claim (`Closes #N`) parks a contributor PR; a PR that merely
  references the closed issue (`Refs #N`, `Part of #N`, a branch name) gets
  the notice with the files named and no park. The sweep's own `needs-human`
  clears with its cause: when the supersession it announced no longer holds
  (issue reopened, claim rewritten, link downgraded to a reference) it takes
  the label off again (`supersession-sweep-released-needs-human`); a person's
  `needs-human` stays (hivecommons/hive#14155).
  The notice names the files the merged PR did not touch. Once the notice is
  on the PR, the sweep re-escalates only when the notice changes (a different
  merged PR or reason); clearing the label for an unchanged notice sticks.
- **Human-authored PRs** get the notice and are surfaced to operators in the
  dashboard Review Queue (below); they are never closed automatically
  unless `supersession_sweep.close_contributor_prs` is set (and the repo is at L6).
  With it set, the sweep closes the PR only when **all** of these hold: every
  file it touches was also touched by the merged PR; it is
  `size/XS`–`size/M` (or under 100 changed lines when unlabeled);
  the author has no unresolved review threads; review threads could be read;
  and `grace_period` has passed since the notice announced the window with no
  human (author or maintainer) comment or review comment after the notice. Any
  doubt keeps the PR open and rewrites the notice to say why.
- A closed PR gets the `hive/superseded` label and one
  `<!-- hive:pr-supersession-autoclose -->` comment naming the merged PR and
  its merge time, with "reopen if that's wrong". Reopening and commenting
  keeps it open on the next pass.
- Every human-authored PR the sweep detects as superseded — whether or not
  automatic closure is on — is tagged `superseded` in the dashboard Review
  Queue with a link to the merged PR. When auto-close is active the
  entry also shows when the grace window ends; otherwise there is no
  countdown. An owner can **Close now** (same comment, label and close as the
  sweep) or **Keep open**, which adds `hive/keep-open`; the sweep never
  auto-closes a PR with that label.

| Key | Default | Meaning |
|---|---|---|
| `supersession_sweep.close_contributor_prs` | `false` | Allows closing superseded human-authored PRs after the grace window. Off: they get the notice and a Review Queue close/keep-open decision, but are never closed automatically. |
| `supersession_sweep.grace_period` | `24h` (`DefaultSupersessionGracePeriod`) when 0/unset | How long a human-authored PR stays open after the grace-window notice. Changing it rewrites the notice and restarts the window. |

## Stage labels (`governor.labels.stage`)

The hive owns one ordered `stage/NNN-*` label per open issue/PR
(`010-actionable` … `100-merge-queue`), a `role/*` admission-tier label and
an `author/*` authorship label (`human` / `clanker` / `hive`; names under
`governor.labels.author`) on PRs, and a red `stage/stalled` marker when nobody
but the hive has touched an item for the stage's dwell — never while checks
run or the reviewer verdict is pending, those are waits on the hive and
surface as a `hive_held` count on the review queue instead;
`label:stage/stalled` across repos is everything a person has walked away
from. Backward stage moves are a one-line comment and the review queue's
transition history (the `stage/regressed` label is retired, #12534). Default
on; `governor.labels.stage.enabled: false` stops the reconciler,
`governor.labels.stage.dwell.<slug>` re-times a stage's stalled threshold,
`governor.labels.stage.stalled.require_quiet: false` measures the dwell from
stage entry instead of last activity (Settings → Labels). When a clanker
PR enters
`stage/072-returned-to-contributor` the hive posts one nudge comment per head
(re-nudged once after `governor.labels.stage.contributor_nudge.renudge_interval`,
default = the 24h dwell; cap 2) and, for a contributor whose relay heartbeat is
within `governor.labels.stage.presence_window` (default 15m), pushes a `fix-pr`
interrupt to their relay session, bounded by the per-contributor fix budget
(`contribute.max_parallel_fixes` / `contribute.min_focus_window`).
`governor.labels.stage.contributor_nudge.enabled: false` turns the nudge and
the relay task off. The nudge and the adoption below apply to `author/clanker`
PRs only; human- and hive-authored PRs just wait at 072. A red PR whose author
is unavailable is adopted (`governor.labels.stage.adoption.enabled`, default
on) — judged against live hub presence each sweep: relay offline for
`governor.labels.stage.adoption.after_offline` (default 15m) since its last
heartbeat, no contributor profile here for `adoption.after_unregistered`
(default 2h) since the first nudge, or online but silent for
`contributor_grace_online` (default 2h; `contributor_grace_offline`, 15m, when
the hub reports no presence). `hold`/`do-not-merge` blocks adoption. The
author's repository role does not: clanker PRs are adoptable on disconnect
regardless of the owner's repo role
(`governor.labels.stage.adoption.maintainer_policy`, default `adopt`; owner
decision on hivecommons/hive#14524 — the PR a contributor's clanker opened is the
hive's to stand in for when that hive disconnects, maintainer or not).
`ask` holds a maintainer's PR for an owner: the activity monitor row says
"waiting on @author (maintainer, offline 26h) — adopt?" and offers **Adopt**
(decision API `adopt`, which sets `adoption/supersede` for the next sweep);
`never` keeps the earlier hands-off rule and releases any existing adoption
of a maintainer's head. Human-authored PRs stay unadopted under all three.
Dashboard: Settings → Labels → "When the PR author maintains the
repository". Whatever the policy and the clocks say, the row's ⋯ menu on a
072 / 071 clanker PR offers the operator **Adopt now — hive takes it over**
(decision API `adopt` + `force: true`: supersede adoption on the next nudged
sweep, bypassing presence, grace, `maintainer_policy` and an earlier
Approve's admission; `hold` still stops it; marker `hive:adopted
reason=operator-forced by=@login`; the row shows *adopting… (queued)* and
drops the action until the copy opens — see *Adopt now always wins over an
earlier Approve* in [App self-merge sweep](README.md#app-self-merge-sweep-auto_merge)) and
**Reroute to another clanker** (decision API `reroute` + `to: <login>|any`:
the follow-up work goes to an online contributor clanker's fix queue as a
`fix-pr` interrupt — the same mechanism as the 072 nudge — with a
`hive:rerouted` note on the PR). A rerouted task the target relay has not
taken within `governor.reroute.pickup_timeout` (default `30m`) falls back to
Adopt now by itself, with a system alert and a `repo_item_reroute_fallback`
audit entry; with no clanker online the Reroute item is disabled ("no
clankers online — use Adopt now"). Both are also options in the
Needs-human popover once the nudge grace has elapsed, and "Let hive decide"
picks Adopt now then (hivecommons/hive#15038;
[dashboard-sections.md](../dashboard-sections.md#hive-activity-monitor)). While
the row waits at 071 / 072 its waiting-on pill also says whether the rework
has been picked up — `· no response`, `· queued` (on the clanker's fix queue,
N ahead), `· acknowledged` (activity on the PR after the request),
`· working` (the clanker relay took the `fix-pr` task), `· re-review pending`
(a new head, routed back to 050 on the next sweep) — from the stage record,
the PR list and the fix queue the tracker already holds, with no extra
GitHub calls (hivecommons/hive#15280). Push detection needs the head saved
by a nudge: a never-nudged 071 row has only a stage-entry time, so a push
cannot produce `· re-review pending` and may appear as `· acknowledged`
from PR activity instead. The stage engine still routes the new head to
050 on its next sweep (hivecommons/hive#15286). Every PR row in the CI /
Review column also carries push progress (hivecommons/hive#15784): the
stage record keeps `waiting_push_since`, `last_push_at` and `last_push_sha`,
booked from the `pull_request` synchronize / `push` webhook re-read and, as
a fallback, from the head change the next enumerate sees. While a 071 / 072
row waits the pill ends with the wait's age (`waiting on 👤 @user to push ·
38m`), muted-amber once past `dashboard.stage_tracker.waiting_push_stale_h`
(default `4`; `0` keeps the default, negative disables the tone). Once a
push lands and CI / the re-review for that head still run, the chevron
returns to the column's own word — stage only, the 👤 clears because the
ball is back with the hive — and the gray pill names the push: `@user
pushed abc1234 · 2m` (links to the commit), `adopted from @x's #N · pushed
2m ago` on an adopted row, `pushed abc1234 · 2m` on a plain CI / Review
row; the hive line reads `push seen 2m ago → CI/re-review queued`. When the
PR allows maintainer edits (`adoption.in_place`, default on) the hive adopts
it in place: `stage/074-adopted-by-hive`, the PR's own branch filed in
`ci-failing.json` as the hive's (`adopted: true`) for the scanner's red-PR
fix-first kick. On a later head change, forgeable git names/emails never keep
ownership: the hive follows the head only when GitHub links the commit to the
App bot (the stage store only remembers SHAs that already passed that
check); otherwise it hands the PR back as a contributor push
(`governor.labels.stage.adoption.strict_attribution`, default on; first deploys
without any repo evidence log and keep adoption). Push-capable App-authored
agents use the App bot noreply email (keyed by the bot's GitHub user id, not
the App id) for git commits so fixer pushes normally carry that GitHub linkage while their commit name/sign-off still names the
lane. Once the hive's in-place head is green, reviewer-approved, DCO-clean
and soaked, the contributor lane merges it under the hive's own authority
(`contributor-lane:adopted-by-hive`) — the original author's trust tier is
not consulted, because the hive owns an adopted head
(`governor.labels.stage.adoption.auto_merge`, default on; `false` leaves an
untrusted contributor's adopted PR waiting for a maintainer `/lgtm`). If the hive cannot push in place, it
pushes `hive/adopt-<n>` at the contributor head, opens a superseding App PR
that preserves the author's commits, and — in the same pass — **closes the
original** with one note naming the copy and why ("Superseded by hive:
re-authored as #M … This PR is closed; if #M fails, hive reopens it."),
labelled `hive/superseded`, so one issue never shows two active PRs
(`governor.labels.stage.adoption.supersede.close_first`, default on;
hivecommons/hive#15135). A contributor head that **conflicts with the base**
(`needs-rebase` / `conflicting`) is first replayed onto `hive/adopt-<n>`
(a merge of the base); when that replay itself conflicts the copy still
opens, on the hive's own branch, and the fixer's rebase-resolve episode
takes it (`requested_by: adoption`, the same two-attempt guard as the
merge-train, hivecommons/hive#15104) — the row reads "Adopting · fixer
resolving conflicts". If both attempts fail the copy parks at `stage/085`
with `rebase-failed-twice` and the original (when still open) with
`adoption-stalled:rebase-failed-twice`; the Needs-human popover offers
**Adopt into fresh branch** / **Close as superseded**. A plain conflict
never hands a PR back to a contributor who cannot act (hivecommons/hive#15307):
an earlier attempt that opened nothing is re-admitted once the clanker has
stayed offline or unregistered for `governor.labels.stage.adoption.offline_grace`
(default 30m) since the attempt or its last heartbeat. (Without a fixer
wired the replay conflict is still terminal for the head: one note,
`supersede-conflict`.) If the copy is closed unmerged or its required
checks are red at two heads, the hive reopens the original, removes
`hive/superseded`, and parks it at `stage/085` with `adoption-copy-failed`
("needs a person"); at most one reopen per original per 24h. The note,
label, close and reopen are unsolicited writes on a contributor's PR, so each
first asks the write-back gate as kind `contributor_adopt` (L6 + opt-in label
by default; see [the write-back table](#what-hive-writes-to-github-and-when)):
refused, the original is not closed and stays open on the keep-open path
until the copy merges, and a due reopen waits for the next poll. A clanker-
or hive-authored original is exempt from the opt-in label: with
`governor.labels.stage.adoption.close_superseded: on_adopt` (the default;
`on_merge` keeps it open until the copy merges) the gate admits its close
as `superseded-agent-pr` once the copy carries its commits and sign-off, so
an agent's PR never lingers open beside hive's copy
(hivecommons/hive#15526) (a pause
lifting lands it; a policy change after the close is warned in the log and
leaves the PR closed until a person reopens it). `close_first:
false` keeps the earlier behaviour: the original moves to 074 and closes
only once the superseder merges. Whether that rework reaches each agent is visible per row
on Operations → Connected contributor agents: the relay's parallel capacity
(`capacity N · spawn|preempt`, or *not advertised*), one line per in-flight
item tagged **NEW** / **REWORK** with its stage label, and a red *N PRs
awaiting rework (not yet dispatched)* line when the contributor has
`stage/072` PRs the hub has not handed them (`parallel_capacity`,
`in_flight[]`, `fix_queue_depth`, `pending_rework` on `/api/contribute/fleet`;
see [contributor-relay.md](../contributor-relay.md#what-the-operator-sees-operations--connected-contributor-agents)). The retired `🐝 covered by PR`,
`claimed` and `hive/in-progress` labels are removed from open items on the
first cycle after upgrade. Full table, state diagram and migration note:
[labels-and-control-signals.md](../labels-and-control-signals/stage-labels.md#stage-labels-stagennn--the-pipeline-state-machine).

### Central stage engine rollout

Every stage label is derived by one `pkg/stageengine` reconciler behind
one writer ([ADR-0025](../adr/0025-central-stage-engine.md)), built once per
process and shared by the issue and PR sweeps. The rollout switch is
`governor.labels.stage.engine.mode` (Settings → Labels → *Central stage
engine mode*, or `hive.yaml`); the sweeps re-read it before each pass, so
a change takes effect on the next sweep without a restart, and the hive
logs one `WARN stage engine: mode changed` per transition.

| mode | behaviour |
|---|---|
| `enforce` (default) | engine's derivation is written; out-of-order transitions are counted as anomalies |
| `shadow` | pause — the engine derives and counts but writes nothing; every `stage/*` label stays exactly as found |

An unset mode is `enforce`. The pre-rollout value `off` (legacy
pass-through) is rejected at config load since the sweeps' own derivation
path was retired (plan PR-F); set `shadow` to pause stage writes instead.
Watch `/api/status.stageEngine` (`mode`, `derivations`, `anomalies`,
`healed`), the strip at the top of the activity tracker (`engine enforce ·
N anomalies`), `hive_stage_transition_anomaly_total{kind,from,to}` and
`hive_stage_duplicate_pills_healed_total{kind}` on `/metrics`; an anomaly
pair not explained by a documented event (merge, close, human label edit)
is a deriver or transition-table fix, never a reason to leave `enforce`.

The remaining series are `hive_stage_derivations_total{kind,stage}` and
`hive_stage_writer_writes_total{kind,op}`.

## Shared-CI rerun sweep (`shared_ci_rerun`)

When a fix lane defers a red PR to a `[shared-ci] <check> failing across
<owner/repo>` incident it stamps `<!-- hive-shared-ci-<n> -->` on the PR
(see [labels and control signals](../labels-and-control-signals.md)). Nothing on
the PR changes when the incident's fix lands, so the PR stays red until
someone re-runs it. The shared-CI rerun sweep
(`src/pkg/github/shared_ci_rerun.go`) closes that gap every ~15 minutes:

- It lists `[shared-ci]` issues closed in the last 72h in each watched repo,
  ignoring ones closed as "not planned". With none, it makes no other calls.
- For each open PR whose newest shared-CI marker names one of those incidents,
  it re-runs the failed jobs (`POST .../actions/runs/{id}/rerun-failed-jobs`)
  of the newest run per workflow on the PR head when that run concluded
  `failure` or `timed_out`, using the hive's App/daemon credential. It pushes
  nothing to the branch, so it applies to every author; PRs with a hold or
  `do-not-merge` label are skipped.
- It then posts one comment carrying `<!-- hive-shared-ci-rerun-<n> -->`,
  which keeps the PR to one re-run per incident. A PR with nothing red on its
  head gets no comment. At most 10 PRs are re-run per pass; each re-run is
  logged as `shared-ci rerun repo=… pr=… incident=#…` and audited as
  `shared-ci-rerun`.
- It is an unstick step (`shared_ci_rerun` in `api_budget.unstick_steps`): it
  keeps running in `conserve` and is shed only in `critical`.

| Key | Default | Meaning |
|---|---|---|
| `shared_ci_rerun.enabled` | `true` (unset) | Set `false` to stop re-running PRs when a `[shared-ci]` incident closes. |

## Open-PR dispatch guard and duplicate-fix sweep (`scanner`)

An issue whose fix is already open must not be dispatched again
([#14799](https://github.com/hivecommons/hive/issues/14799)). The claim
ledger the admission gates read is refreshed by the eval cycle, so a PR a
person opened by hand between scans was invisible to the `/contribute` relay
lane and the scanner, and the issue was handed out a second time; the two
same-issue PRs then landed at `stage/085-needs-decision` for the owner to pick
between. Two mechanisms close that gap
(`src/pkg/github/open_pr_links.go`, `src/pkg/github/pr_duplicate_fix_sweep.go`):

- **Pre-dispatch guard.** Before any automated producer claims an issue —
  the central `IssueClaimBlockReason` gate, the ledger admission check, the
  issue-request watcher and the `/contribute` relay lane — the hive lists
  the repo's open PRs (once per repo per sweep, cached for a minute) and
  parses each title/body with the claim ledger's closing-keyword grammar
  (`Fixes|Closes|Resolves #N`; a `Refs #N` is not a fix). When an open PR
  links the issue, the claim is skipped, the issue is labelled
  `has-open-pr` (created on first use; never re-added), the refusal is
  logged as `claim skipped: open PR already linked` with the PR number, URL
  and author, and the tracker's withheld list shows *An open pull request
  already links this issue (claim skipped)* with the PR as evidence
  (reason `open_pr_linked`). A failed listing fails **open**: the
  ledger-backed guards still apply, and a flaky listing must not stall every
  claim. The gate also refuses an issue the confirming read says is already
  **closed** (`issue closed`), so an issue closed between enumerations is
  never dispatched from the cached snapshot.
- **Webhook claim linking.** A `pull_request` delivery (opened, reopened,
  edited, synchronize, ready-for-review, draft, closed) is parsed with the
  same closing-keyword and branch-name grammar and written into the claim
  ledger the moment it lands
  ([#14814](https://github.com/hivecommons/hive/issues/14814);
  `src/pkg/github/webhook_pr_claims.go`): an open PR's claims are recorded
  (and the cached actionable snapshot re-filtered, so the hub's own agents
  stop seeing the issue too), a PR edited to stop naming an issue or closed
  without merging releases it, and a merged PR's claims become merged
  claims. Before this the link was only made on the full reconcile pass —
  about an hour apart on a busy spoke — and an owner PR opened with
  `Fixes #N` in between left #N offerable for that long; the `/contribute`
  lane handed [#14765](https://github.com/hivecommons/hive/issues/14765) out
  nine minutes after #14772 (Fixes #14765) opened. The receiver's response
  lists the issues as `claimed_issues`, the governor logs `webhook PR claims
  applied to claim ledger` with the PR, state and counts, and the open-PR
  listing cache for the repo is invalidated so the pre-dispatch guard
  re-lists on its next call. An `issues` closed delivery (or a PR merged
  with a closing keyword, which GitHub delivers as the issue's own close)
  already evicts the issue from the status snapshot the lane offers from.
  The same open delivery also **preempts a fixer already at work**
  ([#15469](https://github.com/hivecommons/hive/issues/15469)): the
  `/contribute` hub releases every in-flight contributor task on the
  covered issue whose session is not the PR's own author (reason
  `preempted: open PR #N by @login already covers this issue`) and hands
  the session other work, exactly as a claim takeover does. Before this
  the ledger stopped *offering* the issue but never told the contributor
  it had dispatched nine seconds after the issue was filed, so that
  fixer finished and opened a second PR
  ([#15473](https://github.com/hivecommons/hive/pull/15473)) for an issue
  the owner's PR ([#15470](https://github.com/hivecommons/hive/pull/15470))
  already covered.
- **Duplicate-fix sweep.** Every ~15 minutes (an unstick step, alongside the
  supersession sweep) the hive looks for an open issue two or more open PRs
  close and resolves the pair deterministically. The **earlier-created PR
  wins** while its checks are green or pending: the later hive-authored PR
  receives one comment — `Superseded by #<keeper> (earlier fix for #<issue>).
  Closing as duplicate.` — is labelled `hive/superseded` and closed, with the
  reason `duplicate-of:<keeper>` in the log and the `duplicate-fix-closed`
  audit entry. A later PR a person authored (`author/human`) is never
  closed: it gets the comment, with a request to close it or say why both
  should land, and stays open at its current stage
  (`duplicate-fix-commented`). A later PR by a **relay contributor
  registered on this hive** (the login has a contributor profile — the
  hive's own fixer under its contributor account) is read as the hive's
  own for the write-back gate, so the notice is never suppressed as
  unsolicited, but it is likewise left open with one comment
  (`Superseded by #<keeper> … held out of the merge queue and returned to
  its author`) rather than closed: the merge train holds it as
  `duplicate-of#<keeper>` and the stage engine parks it at
  `stage/072-returned-to-contributor` (071 for a person, 070 for the App's
  own) with the tracker pill *Duplicate of #keeper · needs a pick* — never
  "Approved; waiting its turn" — and the 07x pass neither nudges nor adopts
  it ([#15469](https://github.com/hivecommons/hive/issues/15469): a relay
  login with no `author/*` label yet read as a person, the L4 gate refused
  the notice, and both PRs rode the merge train). Only when the earlier PR is **red or stale**
  (no update for 24h) **and** the later PR is green does the hive ask the
  owner: the later PR is parked `needs-human` → `stage/085-needs-decision`
  with a comment naming both PRs and the check states
  (`duplicate-fix-escalated`). A closed issue is left to the supersession
  sweep, held / `do-not-merge` PRs are skipped, a PR already carrying the
  sweep's `<!-- hive:duplicate-fix -->` marker is never written to again,
  and at most 10 writes are made per pass. A stage-074 **adoption pair** is
  not a duplicate: the App PR whose body says `Supersedes #<original>`
  carries the original's commits and closes it when it merges, so neither
  side of the pair is closed as a duplicate of the other
  ([#14977](https://github.com/hivecommons/hive/pull/14977), the adoptee of
  #14947, was closed this way before the exemption).

| Key | Default | Meaning |
|---|---|---|
| `scanner.skip_issues_with_open_pr` | `true` (unset) | Set `false` to turn off both the pre-dispatch guard and the duplicate-fix sweep; the claim ledger alone then decides, as before. |
| `governor.auto_close_superseded` | `true` (unset) | At ACMM L6, a **hive-authored** PR whose closing targets are all closed, one of them by a different merged PR, and whose head conflicts with the base (or whose files are a subset of that PR's) is closed by the hive itself as superseded — `hive/superseded`, one notice naming the superseding PR — in the self-authored auto-merge lane (before any review cycle), the supersession sweep, and close-on-merge of the superseding PR. Human-authored PRs are never auto-closed. Set `false` to report the refusal once and leave the PR for a person, as before ([#14965](https://github.com/hivecommons/hive/pull/14965)). See [labels-and-control-signals.md § Supersession](../labels-and-control-signals/stage-labels.md#supersession-closed-issue-another-merged-pr). |

## What hive writes to GitHub, and when

Besides the writes an agent asks for through the relay (see
[github-write-surface.md](../github-write-surface.md)), the hive writes to GitHub
on its own schedule. Every such *unsolicited* write — a comment, label,
review, issue edit or close on an item hive did not open and nobody opted in
on — passes through one gate, `writeback.Allowed(repo, item, kind)` in
`pkg/writeback`, before it reaches the GitHub client. The gate never blocks
hive's own PRs and issues, replies to explicit `/hive …` commands, work the
operator started from the dashboard, or agent kicks on items already
labelled for hive — with one exception: a repo under an operator pause
([repo-pause.md](../repo-pause.md)) refuses **every** kind, own items included,
with reason `repo-paused`. The pause also silences the advisory digest
(`advisory digest skipped: repo paused`, once per update interval), which is
not a gate kind but a write all the same. Below the gate the sweeps still *scan*: sentinel findings
appear in **Security → Suspicious Activity → Sentinel findings** and
`/api/sentinel`, hold-gate mergeability notices become dashboard-only, and
PR stage tracking continues from internal state with no `stage/*` label on
the foreign PR.

The gate is per write-back *kind*. Each kind has a minimum ACMM level and
whether the review opt-in label (`review.require_labels`, default
`hive-review`) is required on the PR; both are overridable per kind under
`writeback.kinds.<kind>.{min_level, require_label, unlabeled_min_level}`.
The rationale for each default is the same question: does the hive have, at
that level, the power the write-back exists to govern?

| Path (code) | Trigger | What it writes | Target set | Gate kind → default |
| --- | --- | --- | --- | --- |
| Sentinel sweep + webhook (`pkg/github/sentinel_sweep.go`, `pkg/sentinel`) | 15-min reconcile; `pull_request` webhook | `<!-- hive-sentinel -->` "maintainer review required" comment + `sentinel-alert` label | every open PR in `project.repos`, human or bot | `sentinel` → L5 with opt-in label, L6 without; below: dashboard/API finding only |
| Hold guard (`cmd/hive/main_helpers.go` hold-drift, `maintainHeldPRMergeability`) | each governor eval | "⚠️ Hold-gate mergeability" / drift comment, re-applied `hold` label, **branch push** merging base | every `hold`-labelled PR, incl. human prow-held ones | `hold_gate_notice` → hive-authored PRs only, L≥4; never foreign PRs |
| Stage labeler (`pkg/github/stage_labeler.go`, `stage_writer.go`, `pkg/stagelabels`) | stage engine reconcile | `stage/NNN-*` add/remove, `hive:stage-needs-decision` comment, role/author labels | every tracked PR and issue | `stage_label`, `needs_decision_comment` → L5 + opt-in label on foreign PRs; issue stage labels follow the issue filter, not this gate |
| Supersession sweep (`pr_supersession_sweep.go`, `_autoclose.go`, `_resolve.go`) | hourly sweep | supersession notice comment, `hive/superseded` close, `needs-human` label | contributor PRs overlapping a hive PR | `supersession_notice` → L5 + label; `supersession_close` → L6 + label |
| Duplicate-fix sweep (`pr_duplicate_fix_sweep.go`, `open_pr_links.go`) | hourly sweep | `hive:duplicate-fix` comment, `has-open-pr` label, `hive/superseded` close | PR pairs fixing one issue | `duplicate_fix_notice` → L5 + label |
| Reporter-trust reconcile (`pr_reporter_trust.go`, `reporter_trust_wait.go`) | PR watcher | hold comment + `needs-human` | PRs tracing to issues from untrusted reporters | `reporter_trust_hold` → L5 + label. Held issues read **Triage needed** in the Activity Monitor from the moment the gate holds them and carry `needs-triage` (`reporter_trust.awaiting_label`) once the wait notice is posted. |
| PR↔issue linker (`pr_issue_link.go`, `pr_contributor_link.go`, `linked_prs.go`) | PR watcher | "covered by PR" label on the issue, link comments | issues referenced by any PR | `covered_by_pr_label` → L5 + label |
| Verdict / decision labels (`pr_decision_operator.go`, `human_decision_label.go`, `legacy_decision_labels.go`, `contribute_verdict_settle.go`) | review verdicts, dashboard decisions | `hive:verdict-*`, decision labels | reviewed PRs | `verdict_label` → L3 + label; dashboard-triggered decisions are solicited |
| Contributor adopt / nudge / bot takeover (`automerge/automerge_contributor_*.go`, `automerge_bot_takeover.go`) | auto-merge sweep | adopt comment, update-branch push, workflow approval, nudge comments | contributor PRs in the contributor lane | `contributor_adopt` → L6 + label |
| Adoption close-first (`automerge/automerge_adoption_close_first.go`) | auto-merge sweep, when a supersede adoption opens the hive's copy; close-first poll | close-first note, `hive/superseded` label and close on the original; reopen, copy-failed note and label removal when the copy fails | the contributor's original PR | `contributor_adopt` → L6 + label (an agent-authored original closes without the label, `adoption.close_superseded: on_adopt`); refused: the original stays open until the copy merges, and a due reopen waits for the next poll |
| Claims (`prclaims.go`, `claimed_by_label.go`, `cmd/hive/claimswire.go`, `claimescalation.go`) | claim ledger changes | `claimed-by:*` labels, claim/escalation comments | claimed issues | `claim_comment` → L4 + label (hive-claimed issues are own items) |
| Issue close / task-list / decomposed parents (`issue_close.go`, `task_list_sweep.go`, `decomposed_parents.go`, `close_on_merge.go`, `tracking_issue.go`) | merges, sweeps | close, body edits (checkbox ticks), `Refs` rewrites | issues linked from hive PRs | `issue_close`, `parent_issue_edit` → L5 + label; hive-filed issues are own items |
| Gate comments (`pkg/scheduler/clarity_gate.go`, `prior_art_gate.go`, `triage.go`, `issue_blocked.go`, `issue_recheck_soak.go`) | scheduler intake | clarity/prior-art/triage questions, `blocked` labels | issues admitted by the issue filter | `gate_comment` → L5 + label; items carrying the intake label are opted in |
| Welcome / outreach (`pkg/gfi`, `gfi_forge.go`, `pr_clanker_requested.go`) | good-first-issue selection | welcome comment, `good first issue` label | issues hive advertises | `welcome_comment` → L5 with label, L6 without |
| Issue filing (`pkg/upstreamwatch/github_filer.go`, `advisory.go`, scanner/deps/regression agents, `dedupe_advisory_close.go`) | agents, advisory digest, upstream watch | **new** issues hive authors, pinned advisory issue + comments | `project.repos` | `issue_file` → L3, no label (this is what listing a repo opts in to; `per_repo.mode: issues-only` keeps only this) |
| Shared-CI rerun (`shared_ci_rerun.go`) | CI failure watcher | `rerun-failed-jobs`, `hive-shared-ci-rerun-<n>` comment | PRs with failed shared workflows | `ci_rerun` → L6 + label |
| Level hold, self-authorization & reporter-trust releases (`pr_level_hold.go`, `automerge/self_authorization_release.go`, `reporter_trust_release.go`) | PR watcher | `hold` label add/remove | agent PRs | own items (hive-authored) — not gated |
| Migrations & retirements (`hive_hold_migration.go`, `hive_id_label_retire.go`, `claimed_by_human_migration.go`, `origin_labels.go`) | boot, one-shot | label renames | items carrying hive labels | opted in by carrying hive labels |

Paths not yet routed through the gate are counted by the ratchet
(`pkg/writeback/testdata/direct-writes-baseline.txt`): a test fails on any
new direct `CreateComment` / `AddLabels` / `CreateReview` / `Edit` call
outside `pkg/writeback`, and every migration lowers the baseline.

### Configuration

```yaml
writeback:
  unsolicited:
    min_level: 5          # raises every per-kind floor; never lowers one
    require_labels: true  # foreign PRs also need review.require_labels (fallback hive-review)
    backfill: false       # never touch items that predate first_seen / first write access
  per_repo:
    torch-spyre/torch-spyre: { mode: issues-only }   # off | issues-only | labels-only | full
  kinds:
    sentinel: { min_level: 5, require_label: true, unlabeled_min_level: 6 }
  dedupe:                 # content-aware suppression, see below
    enabled: true
    same_item_ttl: 24h
    burst_items: 2
    burst_window: 60s
    hourly_cap: 60
```

Everything is editable under **⚙ → Features → Autonomy & ACMM → Write-back
containment**, which also shows the per-kind matrix, a counter of suppressed
writes in the last 24 h with a drill-down (`GET /api/writeback/suppressed`),
and the backfill banner. `/api/status` carries the same state under
`writeback`. The per-repo mode is also set from each repo's Projects card.

### Backfill guard

The gate persists `first_seen` per repo in `/data/metrics/writeback.json`
(`HIVE_WRITEBACK_STATE_PATH`), and records the moment a repo first answers
2xx after 403/404s as `writable_at`. Items created before the later of the
two are never written to unsolicited while `backfill: false`. The sweep logs
one line — `writeback: skipped N pre-existing items on <repo> (backfill
off)` — and the dashboard shows "hive can now write to `<repo>`; N existing
PRs were left untouched — enable backfill to process them".

### Duplicate-write suppression

Policy decides whether a kind may write to an item; the content layer under
`writeback.dedupe` decides whether *this* write is worth making. It applies
only to comment kinds whose body the caller handed the gate as the preview
(`sentinel`, `needs_decision_comment`, `supersession_notice`,
`duplicate_fix_notice`, `reporter_trust_hold`, `contributor_adopt`,
`claim_comment`, `gate_comment`, `welcome_comment`); `Allowed()` without a
preview — labels, closes, hold-gate upkeep, task-list edits — stays
policy-only. Three checks run after the policy gate allows a write:

| Check | Key | Suppression reason | Default |
|---|---|---|---|
| Same item | `(repo, item, kind, hash)` of the body with `<!-- hive:… -->` marker arguments, SHAs, RFC 3339 timestamps, durations and whitespace normalised away | `duplicate` — "identical `<kind>` already written to #N at `<time>`" | `same_item_ttl: 24h` |
| Burst across items | `(repo, kind, hash)` of the body additionally stripped of `#N` references, `@mentions` and digits | `burst` — "same `<kind>` already sent to #A within 60s; held to avoid a notification burst"; one WARN per burst | `burst_items: 2`, `burst_window: 60s` |
| Hourly cap | `(repo, kind)` writes in the last hour | `rate-capped` | `hourly_cap: 60` (negative disables) |

Holds are suppressions, not a queue: nothing is replayed later, the next
sweep simply asks again. Merge-path kinds are exempt from the hourly cap
(`hold_gate_notice`, `verdict_label`, `needs_decision_comment`,
`contributor_adopt`, `ci_rerun`, `issue_close`, `supersession_close`,
`issue_file` and every label kind; `writeback.RateCapExemptKinds`). The
hash table is persisted beside the repo records in `writeback.json` so a
restart does not re-fire a notice, pruned by TTL and bounded to 5000
same-item entries. Suppressions appear in the drill-down with the reasons
above, and `/api/status` → `writeback.dedupe` carries the settings plus the
last-hour counters `dedupe_same_item`, `dedupe_burst` and `rate_capped`.
`enabled: false` turns the layer off.

### Notification mode (`github.notifications.mode`)

**Why:** every issue comment and every review the hive posts emails the PR
author and everyone watching the repository. Editing an existing comment or
review notifies nobody. Measured on one busy PR, the hive posted eight
comments and three reviews in two hours — a 071 note per new head (each
@-mentioning the author), a "stage regressed" note for a check that had
merely gone back to pending after a push, a "stalled" note for a PR that
was simply queued in the merge train, a duplicate-PR redirect, and three
"**Reviewed** — no findings" reviews. Multiplied over thirty open PRs that
is dozens of emails an hour saying nothing the Activity Monitor, the
evidence bundle and the stage labels did not already say.

```yaml
github:
  notifications:
    mode: quiet   # quiet (default) | normal
```

`quiet` (the default) changes how the hive delivers its own PR bookkeeping,
not what it records:

- **One status comment per PR, edited in place.** The stage-stalled and
  stage-regressed notes, the trusted-human 071 note and the duplicate-PR
  redirect are sections of a single comment marked `<!-- hive:status -->`.
  The comment is created the first time a note is due and PATCHed for every
  later note; only the creation sends email.
- **No "no findings" reviews.** A reviewer whose whole body is the clean
  acknowledgement has its verdict recorded through the relay and the
  evidence bundle, and no GitHub review is submitted (the reviewer prompts
  also stop asking for the line, whatever `review.acknowledge_no_findings`
  says). Only perspectives with findings post, and a head carries at most
  one hive review: a later perspective's findings for the same head are
  folded into the status comment instead of becoming a second review.
- **@-mentions once per cause.** The 071 note mentions the author the first
  time a cause class appears (conflict, red CI, changes requested) and is
  silent for further heads with the same cause; a new cause mentions again.

Independently of the mode, a check going back to pending after a push is
no longer reported as a stage regression, and a PR the hive itself is
holding — queued behind the merge-train front, being rebased by the train,
or waiting on a base refresh — is never marked stalled.

The status comment also carries, in quiet mode, the notes the hive used to
post on issues and merged PRs as comments of their own:

- the post-merge **evidence pointer** (`<!-- hive-review-evidence -->`), as
  an `Evidence:` section — the bundle is stored server-side and linked from
  the Activity Monitor;
- the **unblocked** note (`<!-- hive:unblocked -->`) on an issue whose
  blocking PR settled; it is also idempotent per cause in either mode (a
  second sweep that sees the same issue and the same settled PR posts
  nothing), and is skipped altogether when the same merge is closing the
  issue;
- the **review backlog summary** (`<!-- hive:review-backlog-summary -->`);
- a contributor's **claim marker** (`<!-- hive:claim … -->`) posted by the
  contribute hub — claim readers scan every hive comment body for the
  marker, so the section is as authoritative as the comment was;
- an agent's relayed **status notes**: `hive/awaiting-ci: …`, `Parking on
  stage/045-needs-decision: …` and `Already fixed …`. Any other relayed
  comment — a finding, a triage verdict with evidence, a question — is still
  posted as a comment.

Closing an issue leaves exactly one closing comment: when the issue-stage
lane has already said `Fixed by #N` the close-on-merge sweep closes without
repeating it, and vice versa.

The client counts what it did and logs one INFO line an hour,
`github notifications created=N edited=M suppressed=K` (cumulative since
start: comments created, comments edited in place, notes folded or found
already present), so the effect of the mode can be charted from the logs.

`normal` restores the previous behaviour: every note is its own comment,
every perspective's verdict its own review, every new head its own mention.

## Suspicious-activity alerts (`sentinel`)

The sentinel evaluates open PRs in the watched repos — human, bot, or agent —
and, when a PR matches one of the behaviors below, posts one
`<!-- hive-sentinel -->` comment per head SHA listing the matched rules and
paths (`pkg/sentinel`, `pkg/github/sentinel_sweep.go`). Since
[#13379](https://github.com/hivecommons/hive/issues/13379) it runs in two
ways. A verified `pull_request` `opened`/`synchronize`/`reopened` webhook
evaluates that one PR at once, reading only the PR and its changed files
(`SweepSentinelPR`); deliveries for the same PR coalesce while one is
pending. The full sweep (`SweepSentinel`) is the reconcile cadence: every
`github.SentinelReconcileInterval` (15 minutes) it enumerates open PRs from
the ForgeState cache when `forgestate.enabled` is on (registered at boot by
`cmd/hive/forgestate_boot.go`; with the cache off it lists the repo as
before), skips every head it has already
evaluated with no GitHub call, reads the remaining candidates once each, and
so bounds how long a PR whose delivery was lost sits unevaluated. The log
line `sentinel sweep complete` carries `cached_repos`/`listed_repos`. Both
paths share one option set and the detection, comment and label logic. For untrusted authors, it also adds the
alert label. The label is a hard hold: while present, Hive will not submit an
APPROVE review, apply LGTM/approval labels, or merge the PR through any
auto-merge lane (dashboard queue, self-authored App sweep, trusted bot authors,
or trusted authors). Trusted authors (the Hive App, configured trusted bots and
owner/trusted-author allow-list entries) default to an informational notice and
audit/dashboard finding only; no alert label is added unless the operator opts
into blocking them. A maintainer who removes the label keeps the PR clear until
the author pushes again: the sweep reads the clearance back from the alert
comment it left for that head (alert comment present, label absent), so it
survives a hub restart or a second replica and nothing else needs to be posted.
A dashboard **Approve** is the same clearance for the head it names: the hive
takes the label off as its own account, so the `unlabeled` event alone is not
read as a human clearance; the `/lgtm` + `/approve` comments the approval
posted carry a `hive:pr-approval head=<sha>` marker, and the sweep reads that
as the release for exactly that head (rewriting its alert comment to say so),
so it does not re-apply the label the owner just released (dibs#510). A later
push re-arms it.

| Behavior | Fires when |
|---|---|
| `sensitive_path` | Any changed file matches a sensitive-path glob. |
| `owner_self_nomination` | The PR author adds their own login to an OWNERS/CODEOWNERS/MAINTAINERS file. |
| `permission_escalation` | A workflow/CI file widens `permissions:` (`write-all`, `contents: write`, `id-token: write`, …), or a role/RBAC manifest grants `*`/cluster-admin. |
| `secret_exposure` | Added lines look like credentials (PEM blocks, AWS/GitHub/Slack tokens, `password=`), or a workflow echoes/uploads `secrets.*`. |
| `ci_gate_weakening` | Added `continue-on-error`, removed a required-check/branch-protection line, deleted a workflow or test job, added `--no-verify`/`skip ci`. |
| `test_removal` | Tests are deleted or heavily reduced while production code is not. |
| `security_policy_edit` | SECURITY.md, GOVERNANCE.md, `.github/rulesets/**`, dependabot/codeql config, or Hive's own policy/proxy rules change. |
| `remote_code_execution` | Added `curl … \| sh`, `wget … \| bash`, `eval $(curl …)`, base64-decoded execution, or an unpinned third-party action in a workflow. |

| Key | Default | Meaning |
|---|---|---|
| `sentinel.enabled` | **on** when unset | `false` disables the sweep entirely. |
| `sentinel.label` | `sentinel-alert` | Label added to untrusted flagged PRs (created red if missing). Neutral on purpose: it means "look closely", not "malicious". This configured label is also added to Hive's hold/exclude set and blocks Hive approval plus every auto-merge lane until a human removes it. |
| `sentinel.trusted_authors_block` | `false` | When `false`, trusted author findings are notice-only: comment, audit/dashboard record, no label, no approval/merge block. Set `true` to add the blocking label for trusted authors too. |
| `sentinel.sensitive_paths` | shipped defaults (`sentinel.DefaultSensitivePaths`) | Glob list (same syntax as `intent.guardrail_path_patterns`; `**` crosses directories) that **replaces** the defaults when set. Defaults cover OWNERS/CODEOWNERS/MAINTAINERS, SECURITY.md, GOVERNANCE.md, `.github/workflows/**`, actions, dependabot, codeql, rulesets, CI config, Makefile/Justfile, pre-commit hooks, `policies/**`, `hive.yaml*`, `gh-wrapper*`, proxy rules, Dockerfiles, `install.sh`, dependency manifests and lockfiles, release workflows, `.env*`/key material, `deploy/**`, Terraform, Helm/k8s manifests. |
| `sentinel.disabled_behaviors` | empty | Rule names from the table above to switch off. Unknown names are rejected. |
| `sentinel.exempt_logins` | empty | GitHub logins never flagged (e.g. a release bot). Use sparingly. |
| `sentinel.repos` | empty = all watched repos | Optional `owner/repo` allow-list. |
| `sentinel.max_actions` | `20` | Caps label/comment/remediation actions per pass. |

Owners edit all of this from **Settings → Security → Suspicious Activity**;
the dashboard writes the owner-only `/api/config/governor/security` overlay.
Every alert is also written to the dashboard audit log as `sentinel-alert`,
and, when `evidence.enabled` is on, recorded under `sentinel` in the flagged
head's [review evidence bundle](../review-evidence.md).

On the dashboard's Projects cards, every open PR that carries the configured
sentinel label is drawn as a red pill (status chip and main pill), taking
precedence over the held, needs-human and merge colours, and the pill legend
has a matching entry. The red clears on the next status refresh after a
maintainer removes the label. Trusted-author PRs that only received a sentinel
comment are not red.

## Image provenance and tags

Pre-built images are published by [`.github/workflows/docker.yml`](../../../.github/workflows/docker.yml) to `ghcr.io/hivecommons/hive` (plus `hive-contributor` and `hive-hub`) and mirrored **by digest** into the matching `ghcr.io/kubestellar/*` packages. Post-transfer, `hivecommons` is the native publishing org; the workflow retags the already-built digest into `kubestellar` so that spokes still pinned to the old org keep resolving, and both orgs serve digest-identical manifest lists for the same tag. (A missing cross-org credential is a hard failure in that direction precisely because a one-sided publish would leave `kubestellar` serving stale tags to live spokes.)

A build of a release line publishes, in one multi-architecture manifest operation:

- `ghcr.io/hivecommons/hive:<line>-latest` — the rolling tag for the current HEAD of that branch: `v4-latest` on `v4`, `v5-latest` on `v5`, `v6-latest` on `v6`;
- `ghcr.io/hivecommons/hive:<git-short-sha>` — immutable per-commit tags;
- that line's moving **release channel**, if it owns one — `v5` merge builds own `:candidate` (and `:latest`), `v6` merge builds own `:edge`; `v4` is a maintenance line and publishes **no** channel, only `v4-latest` and short-SHA tags ([#7721](https://github.com/hivecommons/hive/issues/7721) Phase 1). Channels are retags of the same digest; see [release-channels.md](../release-channels.md).

> **`:latest` follows `v5`.** Only `v5` builds move the global `:latest` tag (`INCLUDE_LATEST` is `false` in `docker.yml` on `v4` and `v6`), so it tracks the same digest as `candidate`. The cross-line ambiguity previously tracked in [#6711](https://github.com/hivecommons/hive/issues/6711) — where `:latest` alternated between lines depending on which run finished last — was resolved by the [#7721](https://github.com/hivecommons/hive/issues/7721) Phase 1 channel remap. For production, still prefer a named channel (`stable`) or a digest over `:latest`.

Channel ownership is deliberately **per-line**: without the split, every merge to one line would silently re-point another line's channel back onto its own build minutes after a deliberate promotion (`src/scripts/publish-image-tags.sh`). Two consequences follow that are easy to get backwards:

- `:stable` is **not** published by a branch build at all. The separate stable-promotion workflow advances it by digest to the newest `v5` build that crossed the 24-hour line, after the [soak gate](../stable-soak-policy.md) passes.
- `:edge` rides `v6`, so it is an **active-development build of the next line**, not a fresher `:stable`. It is the newest build, not the most proven one.

PR and short-lived branch builds compile the image as a CI gate, but only the long-lived release lines (`v4`, `v5`, and `v6`) push tags. Before tagging, the workflow verifies its SHA is still branch HEAD, so a stale queued build cannot move a rolling tag backward.

> **Note:** `v2-latest` was the rolling tag of the retired `v2` branch. Do not use it for new deployments — prefer `stable` for production, or pin a digest. (`src/docker-compose.yaml` was bumped off it in #4206; standalone image references now come from one source of truth, [`src/deploy/standalone-images.sh`](../../deploy/standalone-images.sh).)

### One source of truth for standalone assets

Standalone deployment assets do not carry their own image references. They take
them from [`src/deploy/standalone-images.sh`](../../deploy/standalone-images.sh),
which names the Hive, gateway, and auto-update-profile images in fully
qualified form. Change a reference there rather than in an individual asset.

Fully qualified is deliberate: `podman auto-update` refuses a short name, and
Podman's Quadlet generator warns on one, so a reference that resolves through
the host's `unqualified-search-registries` is not portable across the two
runtimes. Docker may still spell the same image without the `docker.io/`
prefix — the contract test compares the normalized forms, so the two spellings
are equal as long as they name the same image. Digest pins are carried through
unchanged and are checked for as well.

[`src/deploy/test_standalone_image_refs.sh`](../../deploy/test_standalone_image_refs.sh)
runs in CI and fails the build when an asset stops agreeing with that file,
when a digest pin is dropped, or when the retired `v2-latest` tag reappears. It
already covers the Podman asset paths, so drift detection switches on by itself
when those assets land.

### Choosing a tag

- **Production:** `stable`, or pin a digest for full reproducibility.
- **Following mainline:** `v4-latest`.
- **Reproducing an incident:** the `<git-short-sha>` tag from the workflow run.

### Verify and pin a digest

```bash
docker buildx imagetools inspect ghcr.io/hivecommons/hive:stable
docker pull ghcr.io/hivecommons/hive@sha256:<digest>
```

In Compose, replace the tag with the digest form:

```yaml
services:
  hive:
    image: ghcr.io/hivecommons/hive@sha256:<digest>
```

To relate an image to source, compare the `<git-short-sha>` tag published by the same workflow with commits on `v4`, or inspect the Docker workflow run for the commit SHA that produced the digest.

## Scheduled-upgrade progress and history

A hub-instructed upgrade is a short state machine, and both dashboards expose it as data so a window that does not roll is never silent ([#14856](https://github.com/hivecommons/hive/issues/14856)). The hub derives it per hive on `/api/saas/my-hives` (`upgradeState`, `upgradeHistory`), and the spoke derives the same vocabulary locally on `/api/status.upgradeProgress` from its drain latch, self-upgrade marker, last landed outcome and the hub's heartbeat policy (`upgrade_policy.next_window_at`, `upgrade_policy.upgrade_history`).

| State | Meaning | Extra fields |
| --- | --- | --- |
| `scheduled` | Nothing in flight. | `nextWindowAt` — the hive's own daily/weekly slot (always, even on `stable`, where `nextUpdateAt` is the channel's promotion ETA instead). |
| `draining` | The spoke received the instruction and is waiting for kicked agent turns to end. | `drainDeadline`, `drainRemainingS`, `agentsInTurn[]` (`agent`, `elapsedS`), `agentsTotal`; `detail` reads `2/5 agents in turn: reviewer 38m, scanner 12m — proceeds in 40m`. |
| `pulling` | Instruction delivered; the spoke acknowledged it, or (older build) may be draining without reporting it. | `reason` qualifies a silent spoke or a Deployment-patch fallback. |
| `rolling` | No heartbeat since the instruction — the pod is restarting onto the target. | |
| `verifying` | The spoke is back and reports the target commit; the hub has not yet closed the attempt. | |
| `done` | The most recent attempt landed (shown for 24 h, then `scheduled`). | `endedAt` |
| `missed` | The attempt or window did not land. | `reason`: drain lapsed, hub restarted, spoke unreachable, image pull failed, abandoned after N attempts, or the schedule gate's hold reason while the window is still open. |

**Drain window.** When the instruction arrives mid-turn the spoke holds the restart for up to `hub.upgrade_drain_timeout_s` (default 45 min) and reports what it is waiting on every heartbeat (`upgrade_drain.turns`). The hub's orphan sweep leaves a draining spoke alone for that window plus a 20-minute grace; the countdown both dashboards show is anchored on the last heartbeat that carried the report.

**History.** The hub persists the last `HIVE_HUB_UPGRADE_HISTORY_MAX` (default 14) attempts and held windows per hive under `/data/saas/upgrade-history/<hive>.json`, so they survive a hub restart. Each row carries `target`, `from`, `trigger` (`scheduled` / `instant` / `manual` / `observed` / `window`), `startedAt`, `endedAt`, `outcome` (`done` / `missed`) and `reason`. A daily window the gate held records exactly one `window` row per calendar day (ET), updated in place if the hold reason changes; a window that fired records the attempt instead. Deleting a hive drops its history.

**Where it shows.** On the hub, the hive card's Version cell gains a pill for the `draining`, `pulling`, `rolling`, `verifying` and `missed` states — `Draining · 14m left`, with the agents still in turn (`2/5 agents in turn: reviewer 38m, scanner 12m`) or a missed window's reason in the pill's hover title so the Version column stays narrow — and a `history` badge whose tooltip lists the recent attempts (it turns red and counts the misses when any are present); a quiet `scheduled` row renders as before. On the spoke, `/api/version` carries the same `upgradeProgress` and `upgradeHistory`, and the operator dashboard's version menu shows a **Progress** row with the pill and an **Upgrade history** list; the header chip adds a `Draining 14m` / `Missed window` pill while either state holds.

**Upgrade now (interrupt turns).** While a hive is `draining` or `missed`, its owner sees an *Upgrade now (interrupt turns)* action on the hub card. It is the ordinary upgrade request — `POST /api/saas/hives/{id}/upgrade?interrupt=1`, same owner/admin check as every other hub mutation — with an interrupt flag the hub audit-logs (`audit: hosted hive upgrade requested … interrupt_turns=true`), records on the hive timeline, and hands to the spoke on the next heartbeat (`upgrade_interrupt: true` beside `upgrade_to`). The spoke then skips the drain and restarts at once; the agents listed as in-turn are cut and their owners must re-run those turns. The request is held in hub memory for 2 hours and cleared when the upgrade closes, so a stale interrupt never cuts a later upgrade's turns; a hub restart drops it (the spoke falls back to draining). While it is armed the card shows `interrupt requested` instead of the action.

## Governor cadence and budget

- Agent cadences are evaluated from persisted state: the last-kick map lives in `/data/hive-state.json` and is honored across pod restarts — a Deployment roll does **not** re-kick every cadenced agent at boot ([#3817](https://github.com/hivecommons/hive/pull/3817)). A fresh install (no persisted state) still kicks every cadenced agent on the first eval. There is no global default interval; a zero/absent interval means the agent is never cadence-kicked.
- Continuous mode is a generic per-mode cadence value for any configured agent: set a mode cell to `continuous`, optionally with `continuous_cooldown` (default `60s`) and `continuous_budget_pct` (default `80`); `scanner` is only a common first user, not a special case. The legacy `agents.<name>.continuous: true` bool remains a shorthand for every non-QUIET mode. Continuous uses the agent manager's existing turn-end signal — the CLI is back at its input prompt and no work marker is active — to schedule `nextKick = ended_at + cooldown`; the run loop wakes on that time (at most one early eval per 30s, backing off 1m→15m while the eval cycle keeps skipping a due agent for a dashboard pause, first-scan deferral, provider-budget gate, or upgrade drain) instead of waiting for the next cadence eval tick, and status exposes `lastKickEnd` / `idleSince` for the gap. Enabling continuous on an idle agent, including entering a mode whose cadence is `continuous`, schedules its first continuous kick after one cooldown; entering a mode where the agent is not continuous clears pending continuous kicks and status reports `continuousBlocked: "not_in_mode"`. It supersedes interval cadence while active, but only after an observed session end or idle enablement. It never types into a busy session and still respects `enabled: false`, operator/fleet-breaker pause, governor-mode `pause`/`off` (including QUIET), on-demand, non-kick channels, provider rate-limit/quota backoff, budget/provider holds, and upgrade/restart holds. The continuous budget guard stops re-kicks once the current token-budget window reaches `continuous_budget_pct` of `governor.budget.total_tokens`; normal cadence resumes until the window falls below the threshold or resets. Failed deliveries back off exponentially (capped) and `/api/status` exposes `continuous`, `continuousModes`, `continuousBlocked`, any `continuousBackoff`, and per-agent `continuousKicks` / `continuousTokens` counters. If a kicked turn never reports an end (missed pane turn-end signal), `continuous_turn_ceiling` (default `45m`, `-1` disables) presumes it over, logs `continuous turn never reported an end`, and re-arms after the cooldown rather than idling until a pod restart; a non-zero `presumedTurnEnds` in status means turn-end detection is missing for that agent. Operators set per-mode `Continuous` values in the agent settings **Cadences** form; the Governor cadence table is read-only and shows an `∞ continuous` chip in each continuous mode cell.
- Manual dashboard/API kicks that rely on Hive's generated work list wait for the first governor scan after boot. Until that scan populates the scheduler snapshot, `POST /api/kick/{agent}` returns `202` with `status: "deferred"` and Hive delivers one deduplicated kick for that agent as soon as the first scan completes.
- Kick-visibility conditions surfaced on the dashboard include `copilot-question-form`, which means the Copilot CLI asked an unattached human for clarification; Hive dismisses that form with Escape and retries delivery instead of dropping the kick.
- The governor token budget uses a rolling window of `governor.budget.period_days` (default 7 days), with a soft warning at `governor.budget.critical_pct` (default 90%). When spend reaches the limit, kicks are suppressed for all agents except those explicitly budget-exempt.
- Bob coin budgets are configured under the same governor budget window. The coin conversion comes from your Bob account/team; the values below are illustrative only (they are not Hive's built-in defaults and not public Bob pricing guidance) — always set them to the conversion your Bob team gives you:

```yaml
governor:
  budget:
    usd: 25.00                    # optional period cap in Bob-equivalent USD
    coins:
      bob:
        tokens_per_coin: 1000000  # illustrative only; use the conversion your Bob team gives you
        usd_per_coin: 1.00        # illustrative only; use the conversion your Bob team gives you
        label: "BC"
        budget: 50                # optional period cap in Bob coins
```

  `HIVE_BOB_TOKENS_PER_COIN`, `HIVE_BOB_USD_PER_COIN`, `HIVE_BOB_COIN_BUDGET`, and `HIVE_BOB_USD_BUDGET` override those values at runtime. `budget.usd` and `budget.coins.bob.budget` are first-class caps: set either or both. When both are positive, the governor trips on whichever cap is exhausted first and surfaces the exhausted unit. The Cost panel shows Bob coins alongside the configured USD equivalent so operators can enter and compare either unit. Coin conversion values are account-specific; always confirm the conversion with your Bob team rather than relying on Hive's built-in fallback values.

### Kick size and the list caps

Every kick is typed into the agent's terminal after a `/clear`, so the whole prompt is paid for — in model input tokens and in delivery time (about 400 runes/s, so roughly 2.5 s per KiB) — on every cadence tick. `governor.kick_limits.max_issues` / `max_prs` are the only knobs that change that size without editing a policy, which makes it tempting to lower them. The numbers below ([#12133](https://github.com/hivecommons/hive/issues/12133)) show why the shipped defaults stay at `100` / `50`.

**Where the bytes go.** Measured with `go test ./pkg/scheduler -run TestKickSizeMeasurement -v` (an ACMM L6 `scanner` in `ISSUES_PRS_MERGE` on the shipped `scanner-automerge.md`, three authorised repos, 70-character titles, merge-eligible and CI-failing lists scaled with the backlog — 100 and 20 at the 302-PR row):

| Open issues / PRs | Caps | Kick size | Of which lists |
|---|---|---|---|
| 0 / 0 | 100 / 50 | 32.9 KiB | — (policy, seams, overlays, repos) |
| 10 / 5 | 100 / 50 | 35.8 KiB | 2.9 KiB |
| 25 / 10 | 100 / 50 | 38.8 KiB | 5.9 KiB |
| 40 / 20 | 100 / 50 | 43.1 KiB | 10.2 KiB |
| 60 / 30 | 100 / 50 | 47.7 KiB | 14.8 KiB |
| 100 / 50 | 100 / 50 | 56.8 KiB | 23.9 KiB |
| 302 / 302 (the [#7368](https://github.com/hivecommons/hive/issues/7368) backlog) | 100 / 50 | 63.4 KiB | 30.5 KiB (issues 11.5, PRs 6.9, merge-eligible 4.8, CI-failing ≈ 7) |
| 302 / 302 | 40 / 20 | 47.0 KiB | 14.1 KiB |

The fixed cost — the policy text and the mandatory seams — is 32.9 KiB: 52 % of the worst-case kick and ~85 % of a typical one. Lowering the caps to `40` / `20` saves nothing until a repo set has more than 40 open issues or 20 open PRs, and at most ~16 KiB (≈ 40 s of typing) at the 302-PR extreme. It would also make the merge-eligible list drain a 100-PR automerge queue in five kicks instead of two, which at a 10-minute cadence is the starvation the caps were sized against ([`docs/scale-envelope.md`](../scale-envelope.md)). The lever that actually moves kick size is the policy itself, not the list caps.

**Watch it rather than guess.** Each rendered kick logs one line:

```
level=INFO msg="kick rendered" agent=scanner repo="" kick_bytes=58123 issues_total=137 issues_listed=100 prs_total=61 prs_listed=50 max_issues=100 max_prs=50
```

`kick_bytes` is the prompt as typed (canary preamble included). When `issues_listed` / `prs_listed` sit well below the caps on every kick, lowering them changes nothing; when `*_total` is persistently far above `*_listed`, the backlog — not the cap — is the problem, and the overflow marker in the prompt already tells the agent the list is partial. Per-agent kicks share the same global caps; set them under **Settings → Repos → Kick prompt list caps** or in `governor.kick_limits`, with `0` meaning unlimited.

### What consumes tokens while agents are paused

Pausing agents (including the navbar fleet breaker) does not take the Hive instance down: the dashboard, governor scans, GitHub polling, token scanners, repo-cost collectors, telemetry/heartbeats, and provider-headroom probes can keep running. Those components should not make model calls by themselves. Model usage is expected from agent sessions reached through governor/manual/CEL/resume kicks; those kick paths share the same pause and budget gates. Knowledge priming reads configured knowledge stores and attaches facts to kicks; it does not independently call the model while every agent is paused. Provider-budget probes are the exception by design: after a provider spend-limit rebuff, Hive may release one kick after `governor.provider_budget.probe_interval_s` to test recovery.
- The Governor dashboard **PRs by model** panel includes rework evidence for 7d/30d/all windows: first-pass merge rate, average/worst review rounds, fix attempts, follow-up commits after first review, human change requests, median time to merge, and a top-10 **Most reworked PRs** list. The data is served by `/api/governor/pr-models` from the same cached PR snapshot as the model outcome counts.
- The **provider** spending limit is a separate signal from the token budget above ([#4294](https://github.com/hivecommons/hive/issues/4294)): the token budget counts what the hive spends, while this is the inference gateway refusing to spend more money — a LiteLLM key past its daily dollar cap, a project out of quota, an account out of credit. It is detected from the gateway's own error body (never from a bare 429, which stays on the ordinary retry path), raises an error-level dashboard alert naming the limit that was hit, and withholds every agent kick while it is in force. It does **not** pause agents: pause state stays a human decision.
### GitHub API budget governor

Spoke GitHub App installations share one core REST bucket across the scan loop and all agent-requested writes. Hive now derives an API budget mode from the observed core rate-limit window:

| Mode | Condition | Default behavior |
| --- | --- | --- |
| `normal` | `remaining >= github.api_reserve` | Full eval fan-out. Optional sweeps run every `governor.optional_sweep_every_n_cycles` cycles (default `1`). |
| `conserve` | `remaining < github.api_reserve` (default `800`) | Critical paths and unstick steps (escalation, SHA hold, supersession sweep, shared-CI rerun) continue; optional advisory/recommendation/duplicate/review-thread/collector enrichment sweeps are skipped. Eval interval is multiplied by `governor.conserve_interval_multiplier` (default `2`). |
| `critical` | `remaining < github.api_critical` (default `250`) | Same shedding as conserve plus the unstick steps, with the eval interval stretched by `4x`. |

Mode exits use hysteresis: Hive leaves conserve/critical only after quota rises above the threshold plus 100 calls or after the reset time passes. The effective interval is capped by `governor.eval_interval_max_s` (default `1800`). `/api/status` and `/api/gh-rate-limits` expose `api_budget` with the mode, remaining/limit/reset, mode `since`, `skipped_steps`, `unstick_steps` and `enrichment_steps`; the dashboard shows an API budget pill beside the GitHub rate-limit display.

- Recovery from a provider spending limit is automatic, via a probe. Withholding kicks also withholds the inference calls that would reveal the provider is serving again, so the hive suppresses only while the last refusal is recent and then lets a single kick through to test the gateway; the probe re-arms suppression the moment it is released, so at most one probe run flies per interval. A still-clipped key refuses the probe and suppression resumes for another interval; once the provider's window resets the probe succeeds, normal kicking resumes with no operator action, and a one-time recovery notification is sent (the entering notification is likewise sent once per clip, not once per cycle). Tune with `governor.provider_budget.probe_interval_s` (default 1800 — 30 minutes):

```yaml
governor:
  provider_budget:
    probe_interval_s: 1800
```

  Lower it to resume sooner after a reset at the cost of a rebuffed run per probe; raise it to waste less while noticing later.

## Merge-hotspot advisor (`governor.hotspot_*`)

The hive already predicts PR file overlap for the merge train. The advisor reuses that evidence — the train's per-PR file lists and the conflicts it hands to the rebase resolver, plus the rebase follow-through's conflict records — to find files that concurrent PRs keep colliding on, and files one modularize issue per hotspot for the ci-maintainer lane. It never lists PRs or files from GitHub itself; its only reads are the hotspot file's current content (size and top-level symbols) and the state of issues it filed earlier.

- A file is a **hotspot** when, over the rolling window, at least `hotspot_min_prs` distinct PRs touched it and at least `hotspot_min_overlaps` pairs of them were open at the same time. Score = concurrent pairs × a size weight (1 / 1.25 / 1.5 / 2 at 800 / 1500 / 4000 lines) + 2 × attributed conflicts. Hunk-level "same region" weighting is not available from the train's records and is skipped.
- **Don't splinter.** A split is only recommended for code when the file has at least `hotspot_min_lines` lines **and** two or more top-level symbol groups (Go: funcs and types via `go/ast`, methods grouped under their receiver; JS/TS/Python: top-level declarations; Markdown: `##` headings). The proposal targets at most 4 files of at least 150 lines, no behaviour change, exported API unchanged, tests move with the code.
- **By kind.** Docs (`*.md`, `*.rst`, `*.txt`) are reported but only recommended for a by-section split above 1500 lines (the `docs/api-reference/<area>.md` pattern). Generated files (`// Code generated … DO NOT EDIT`, `*.pb.go`, the dashboard SPA under `pkg/dashboard/static/` → `static-src/`) point at their source — unless that source tree is already split into more than 4 files, in which case they are report-only (regeneration makes the output touch every source change). Lock and data files are report-only.
- **One issue per file.** Title `[ci-maintainer] refactor: <path> is a merge hotspot — split into <N> units` (the tag names the same agent as the label, as every hive-filed issue title does), labels `agent/ci-maintainer` + `kind/cleanup`, origin `governor`, body marker `<!-- hive:hotspot file=<path> -->`, evidence table, the top-level units, a concrete split proposal and the don't-splinter rule. While that issue is open no second one is filed; after it closes the file waits `hotspot_refile_days` before it can be filed again. **Capped.** At most `hotspot_max_open` modularize issues are open per repo at once (an issue that already has a PR still counts) and at most `hotspot_max_per_pass` are filed per advisor pass; the highest-ranked uncapped hotspot is filed when a slot frees, and the advisor logs one INFO line per pass when the cap holds it back. Issues go through the writeback gate as `issue_file` (a paused repo or `writeback.per_repo[<repo>].mode: off` stops them) and are skipped when the GitHub budget governor is skipping optional sweeps.
- **Where to look.** `GET /api/hotspots` (or `hivectl governor hotspots [--repo owner/name]`) lists every hotspot with its recommendation and issue; `/api/status` carries `hotspots: {count, actionable, filed}`. State lives in `/data/hotspots.json`; the analysis runs at most hourly in the eval cycle, the intake on every merge-train sweep. A PR the train has not seen for two hours has its interval closed at its last sighting.

| Key | Default | Meaning |
|---|---|---|
| `governor.hotspot_enabled` | `true` | Set `false` to keep the report and `/api/hotspots` but never file an issue. |
| `governor.hotspot_window_days` | `7` | Rolling window of merged and open PRs considered. |
| `governor.hotspot_min_prs` | `6` | Distinct PRs touching a file in the window before it is reported. |
| `governor.hotspot_min_overlaps` | `3` | Concurrent PR pairs on the file before it is reported. |
| `governor.hotspot_min_lines` | `800` | Smallest code file a split is recommended for. |
| `governor.hotspot_refile_days` | `30` | Wait after a modularize issue closes before the same file may be filed again. |
| `governor.hotspot_max_open` | `2` | Most modularize issues open at once per repo; no new one is filed until one closes. |
| `governor.hotspot_max_per_pass` | `1` | Most modularize issues filed in one advisor pass. |

## Delivery SLA

The **Throughput** section of the dashboard carries a **Delivery SLA** block
(below the merged/opened metric tiles; moved there from the Overview) that
answers "how long does it take this hive to turn an issue into a merged PR?"
without spending any GitHub API quota. Everything is derived from the
lifecycle timeline the hive already keeps ([#11841](https://github.com/hivecommons/hive/issues/11841)
follow-on): the issue's `created_at` retained when it is enumerated, the
`classified` stage, the `started` entry written with the `stage/030-pr-in-progress`
label (formerly `hive/in-progress`), the `pr_opened` entry recorded when a hive PR that `Fixes`/`Closes` the
issue is opened, and the merge observed by the PR watcher, the close-on-merge
ledger or the audit stream, fanned out to every issue the PR linked.

Per repo and fleet-wide, five spans are aggregated over rolling 7-day and
30-day windows as `{p50_h, p95_h, n}` (nearest-rank percentiles, hours):

| Span | From → to |
|---|---|
| `open_to_triage` | issue created → classified |
| `triage_to_start` | classified → `stage/030-pr-in-progress` (`started`) |
| `start_to_pr` | `started` → linked PR opened |
| `pr_to_merge` | linked PR opened → merged |
| `open_to_merge` | issue created → merged |

A journey enters a window by the timestamp of the span's *end* event, so a
PR merged today counts in today's `pr_to_merge` even if it was opened last
month.

In the dashboard the three headline tiles (start → PR, PR → merge,
open → merge) sit under the Throughput merged/opened metrics and follow its
repository filter; they show the 7-day percentiles for every period pill
except `all`, which switches to the 30-day window. **Repos ▾** expands a
per-repo table and the stale list, and the collapsed Throughput summary line
appends the open → merge p50 and the stale in-progress count.

**History.** Daily buckets (`day`, p50 of `start_to_pr`, `pr_to_merge` and
`open_to_merge`, plus `n_started`, `n_pr`, `n_merged`) for the last 30 days
are persisted to `/data/sla-history.json` (written atomically, pruned to
30 days, throttled to one write a minute and flushed on shutdown). Restarting the hive or evicting old
journeys from the timeline never shrinks a stored day, so the Throughput
sparklines always have 30 days behind them. A corrupt file is moved aside
as `sla-history.json.corrupt` and history restarts from the live timeline.

**Stale in-progress.** Issues that have carried `stage/030-pr-in-progress` (formerly `hive/in-progress`) for longer
than the agent claim TTL (`claims.agent_ttl_s`, default 2h) with no linked
PR are listed as `stale_in_progress` with their start time, age and agent.
The badge in the Delivery SLA header counts them and links to each issue; it is the first
place to look when an agent claimed work and went quiet.

**Targets.** Each span's p50 is coloured green at or under its target,
yellow up to 1.5× the target and red beyond. Targets are hours in
`hive.yaml` and can also be set from **Settings → Thresholds → Delivery SLA
targets** (`PUT /api/config/sla`), applying on the next status poll:

```yaml
sla:
  targets:
    start_to_pr_h: 4      # started → linked PR opened
    pr_to_merge_h: 24     # PR opened → merged
    open_to_merge_h: 72   # issue created → merged
```

`0` or an omitted key means the default. Values above one year are
rejected.

**Reading the panel.** The three tiles show the current-window p50, `n`,
p95 and target with a 30-day sparkline of daily p50s; `7d`/`30d` toggles
the window. **Repos ▾** expands a per-repo table (start→PR p50/p95,
PR→merge p50/p95, merged in 7d, 30-day open→merge sparkline) and the
stale-issue list. The same numbers are in `GET /api/status` under `sla`
(fleet) and `repos[].sla`; see [api-reference.md](../api-reference.md).
