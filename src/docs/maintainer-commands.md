# Maintainer commands

Maintainers steer Hive by commenting commands on issues and pull requests. Hive
does not depend on a webhook for the state-changing `/hive` issue commands: the
spoke scans comments during its issue un-park sweep, which runs about every five
minutes. The `/hive help`, `/fixed`, `/reopen`, label-helper, and assignment
helper commands are GitHub Actions conveniences that answer new comments as
soon as Actions schedules the workflow.

Canonical v5 URL:
https://github.com/hivecommons/hive/blob/v5/src/docs/maintainer-commands.md

## Command table

| Command | What it does | Who may use it | Where it works | Pickup speed | Labels changed |
| --- | --- | --- | --- | --- | --- |
| `/hive approve` | Accepts the recommendation already written on a parked issue. On a pull request it is a trusted merger's release of the swarm's `requires_human` hold (see [PR approvals](#pr-approvals)). | Human commenter with `write`, `maintain`, or `admin` repository permission; on a pull request, a trusted merger. | Open issues carrying `needs-human` or `stage/045-needs-decision` (or the retired `needs-decision` / `needs-direction` / a pre-045 `stage/085-needs-decision`, until migrated). Open pull requests parked at `stage/085-needs-decision`: the merge lanes read the reply as a hold release and the un-park sweep answers it once, explaining what it does and does not clear and whether a `needs-human` label still blocks the PR ([#13087](https://github.com/hivecommons/hive/issues/13087)). On a hive-authored PR it also lifts a `needs-human` **Hive** added, adds `reviewer-passed` and removes `reviewer-handoff` ([#13309](https://github.com/hivecommons/hive/issues/13309)); it changes no other PR labels. | Next un-park sweep, normally within 5 minutes. | Removes `stage/045-needs-decision` (and the retired spellings) when present; adds `approved-direction`; removes `needs-human` **whoever added it** — your approval is the human input the label asked for ([#13075](https://github.com/hivecommons/hive/issues/13075), [docs#419](https://github.com/hivecommons/docs/issues/419)); the reply names who had added it. Never removes `hold`: add `hold` to keep agents off an issue through a decision. |
| `/hive decision <text>` | Gives explicit direction for a parked issue. On a hive-authored pull request it is a *changes requested* review in comment form (see [PR approvals](#pr-approvals)). | Human commenter with `write`, `maintain`, or `admin` repository permission; on a pull request, a trusted merger. | Open issues carrying `needs-human` or `stage/045-needs-decision` (or the retired `needs-decision` / `needs-direction` / a pre-045 `stage/085-needs-decision`, until migrated). Open pull requests parked at `stage/085-needs-decision`: on a hive-authored PR it moves the PR to `stage/070-fixing` and kicks the author agent with your text ([#13041](https://github.com/hivecommons/hive/issues/13041)); on any other PR it gets one reply naming the review-side equivalents. | Next un-park sweep, normally within 5 minutes. | Removes `stage/045-needs-decision` (and the retired spellings) when present; adds `approved-direction`; removes `needs-human` **whoever added it** — your decision is the human input the label asked for, and left in place it would re-derive 045 two minutes later ([#13075](https://github.com/hivecommons/hive/issues/13075), [docs#419](https://github.com/hivecommons/docs/issues/419)); the reply names who had added it. Never removes `hold`: add `hold` to keep agents off an issue through a decision. An agent's later re-park on 045 is refused while `approved-direction` is present and the recorded decision text is unchanged. |
| `/hive done` | Says you did a step only the owner can do (a repository setting, secret, app install, …). Hive un-parks the issue exactly as `/hive approve` does and records the decision "the owner-only step is done; Hive re-checks it and closes the issue once the fix is verified" in its reply; that reply is the hand-off — the lane that next picks the issue up reads it, re-checks the step and closes the issue once verified. No label or queue state carries the decision. The "What to reply" notice offers it on issues an agent parked with `hive-open-issue comment --needs-decision` or filed with `hive-open-issue issue --needs-decision`, while that text lists no lettered options. `/hive help` does not answer the step; only `/hive approve`, `/hive decision` or `/hive done` does. | Human commenter with `write`, `maintain`, or `admin` repository permission. | Open issues carrying `needs-human` or `stage/045-needs-decision` (or the retired `needs-decision` / `needs-direction` / a pre-045 `stage/085-needs-decision`, until migrated); pull requests are ignored. | Next un-park sweep, normally within 5 minutes. | Removes `stage/045-needs-decision` (and the retired spellings) when present; adds `approved-direction`; removes `needs-human` **whoever added it**, exactly as for `/hive approve` ([#13075](https://github.com/hivecommons/hive/issues/13075), [docs#419](https://github.com/hivecommons/docs/issues/419)); never removes `hold`. |
| `/hive decision proceed — <gap>` | Confirms a feature request parked by the prior-art gate is a real gap. | The issue author, or a human commenter with `write`, `maintain`, or `admin`. | Open issues carrying `stage/017-possible-existing` below a `<!-- hive:prior-art-gate -->` comment. | Next un-park sweep. | Removes `stage/017-possible-existing`, appends a **Scope vs existing** section with `<gap>` to the issue body, records the un-park so the issue re-derives `stage/010-actionable` ([#13575](https://github.com/hivecommons/hive/issues/13575)). |
| `/hive decision exists` | Agrees the request already exists. | Same as above. | Same as above. | Next un-park sweep. | Adds `already-exists`, closes the issue `not_planned`, replies with a pointer at the quoted findings. Reopen the issue to continue; the gate does not ask again. |
| `/hive help` | Posts this command list. On parked issues it also repeats the issue-specific approve/decision options. | Any human commenter with at least `triage` repository permission for the on-demand workflow; the sweep path on parked issues requires `write`, `maintain`, or `admin`. | Any open issue or pull request. | GitHub Actions on the new comment; if Actions is unavailable on a parked issue, the next un-park sweep can also answer. | None. |
| `/fixed` | Confirms that an open issue's fix is verified — or that what a parked issue waited on was done, in or outside the repository — and closes it as completed. Alias: `/close`. Plain-language confirmations such as "yes, this has been fixed" are accepted only when Hive is already waiting for reporter confirmation. | The issue reporter, or a human with `write`, `maintain`, or `admin` repository permission. On a parked issue (`stage/045-needs-decision`, `needs-human`, or a retired park spelling) the un-park sweep honours it from a human with `write`, `maintain`, or `admin` permission. | Open issues only, not pull requests. | GitHub Actions on the new comment where the repository runs the `/fixed` workflow; on parked issues, the next un-park sweep, normally within 5 minutes ([dibs#459](https://github.com/hivecommons/dibs/issues/459)). | Closes the issue as completed. The workflow adds `hive: reporter-confirmed` and removes `hive/likely-done` and `needs-reporter-confirmation` when present. On a parked issue the sweep removes `needs-human` and the retired park spellings, the stage engine strips `stage/*`, and Hive posts one acknowledgement; `hold` is never touched. |
| `/reopen` | Reopens a closed issue that was closed too early. | The issue reporter, or a human with `write`, `maintain`, or `admin` repository permission. | Closed issues only, not pull requests. | GitHub Actions on the new comment. | Removes `needs-reporter-confirmation` when present. The reopen sticks: close-on-merge does not close the issue again for a PR that merged before the reopen, whether Hive (`<!-- hive-close-on-merge -->` comment) or GitHub's closing keyword closed it; Hive records the latter with a `<!-- hive-reopened-after-merge pr=<pr> -->` comment. A later PR can still close it. |
| `/help-wanted` | Adds the `help wanted` label. | Any non-bot commenter accepted by the label-helper workflow. | Issues and pull requests. | GitHub Actions on the new comment. | Adds `help wanted`. |
| `/good-first-issue` | Adds the `good first issue` label. | Any non-bot commenter accepted by the label-helper workflow. | Issues and pull requests. | GitHub Actions on the new comment. | Adds `good first issue`. |
| `/hacktober-fest` | Adds the `hacktober-fest` label. | Any non-bot commenter accepted by the label-helper workflow. | Issues and pull requests. | GitHub Actions on the new comment. | Adds `hacktober-fest`. |
| `/kind <kind>` | Validates or requests a kind label. The Hive label helper reports invalid values and lists the accepted kind names; repositories may also have Prow or other automation that applies valid labels. | Any non-bot commenter accepted by the label-helper workflow, subject to any repository label automation that applies the label. | Issues and pull requests. | GitHub Actions on the new comment. | The helper itself changes no labels for valid `/kind` comments; companion repository automation may add `kind/<kind>` or equivalent labels. |
| `/area <area>` | Validates or requests an area label. The Hive label helper reports invalid values and lists the accepted area names; repositories may also have Prow or other automation that applies valid labels. | Any non-bot commenter accepted by the label-helper workflow, subject to any repository label automation that applies the label. | Issues and pull requests. | GitHub Actions on the new comment. | The helper itself changes no labels for valid `/area` comments; companion repository automation may add `area/<area>` or equivalent labels. |
| `/assign` | Assigns the commenter where the repository's assignment automation supports slash-command assignment. The Hive assignment helper deliberately ignores the exact command because Prow handles it, but it points natural-language assignment requests to this command. | Repository assignment automation decides; the helper response is available to non-bot issue commenters. | Issues. | Repository assignment automation or GitHub Actions helper on the new comment. | Assignment state changes, not labels. |
| `/unassign` | Removes the commenter's assignment where the repository's assignment automation supports it. | Repository assignment automation decides; the helper response is available to non-bot issue commenters. | Issues. | Repository assignment automation or GitHub Actions helper on the new comment. | Assignment state changes, not labels. |

## Eligibility rules

Hive-authored issues normally arrive actionable rather than parked when the question is only an engineering choice (sequencing, naming, numbering, config shape, scope, or approach A vs B). With `project.agent_issue_decision_policy: resolve` (the default), the relay downgrades those attempted parks and records `Decision taken by hive: …`; maintainers can still override with `/hive hold` or `needs-human`. Parked Hive-authored issues should include `Risk: <category>` for the reason a person is required.

The un-park sweep only honors `/hive approve`, `/hive decision <text>`,
`/hive done`, `/fixed` (alias `/close`), and the sweep fallback for `/hive help` when the command is the first non-empty line of
the comment, outside quoted text and fenced code. The comment must be newer than
seven days, unedited, authored by a human rather than the hive or another bot
(the account named in `project.ai_author` counts as the hive, so a maintainer
who is also `ai_author` cannot use these commands; Hive replies saying so),
and posted by someone who currently has `write`, `maintain`, or `admin`
permission on the repository. A `/hive decision` must include non-empty
instructions.

"Parked" means the issue carries `needs-human`, `needs-decision`, or
`needs-direction`, or a label the operator configured for one of those buckets
in `project.issue_filter.hard_suppress_labels`; the sweep reads the same
configured names as the dashboard's "Your turn" inbox, so a renamed label still
gets the notice and is cleared by the commands. `blocked` parks an issue too
([#12376](https://github.com/hivecommons/hive/issues/12376)): it is a gate
only a person lifts, so the issue gets the notice, and `/hive approve` (lift it
and re-offer the issue) or `/hive decision <text>` (lift it with instructions)
removes it with the other parking labels. `needs-spec` (and its renamed
labels) is never handled by these commands.

The "What to reply" notice lists the lettered options (`Option A:`,
`**A.** …`) the issue body offers, or the newest Hive comment that lists them.
Each option bullet ends with `: <summary>`, the rest of the option's first
line with whitespace collapsed, HTML comments and `@mentions` removed, and cut
to 160 characters with a trailing `…`. When the options came from a comment
rather than the body, the notice opens with
`The options are in this comment.`, where "this comment" is a Markdown link
to that comment's URL, so a reader can find them.
`/hive approve` picks the recommended option when the text names one, else the
first option, and records that choice as the decision ("go with the
recommendation (option B: …)", "go with option A: …"). When there are no
options and no recommendation, the notice offers only `/hive decision <your
instructions>`, and a `/hive approve` gets a reply saying the park names no
recommendation; the issue stays parked. On a `blocked` park and on an owner
step `/hive approve` keeps its meaning (the blocker is cleared; accept the
proposed step) and needs no recommendation.

### When a park's premise moves

A park is written once, often by a contributor relay, and names the work it
waits on ("#722 is in progress", "the precedent has not been set"). At the
hard-suppress reconcile cadence (`issues.hard_suppress_reconcile_interval`,
default 30m) Hive re-reads every open issue at `stage/045-needs-decision`.
When, after the park began, a PR or issue the park text names merged or
closed, or a PR that refers to the issue ("Refs #N", a closing keyword, "part
of", …, a list of at most 10 references) merged, Hive posts one note
(`<!-- hive:park-recheck -->`) listing what changed, and edits it in place as
more moves. When such a PR closed unmerged because the hive adopted it, Hive
follows it to the hive's copy (the App PR whose body says `Supersedes #N`)
and lists the copy too if it merged or closed unmerged; when a person closed
the copy, the note quotes the first paragraph of their closing comment (at
most 300 characters, mentions defanged) and adds the replies that act on it,
`/fixed` or `/hive decision <your instructions>`
([projectbluefin/utah#13](https://github.com/projectbluefin/utah/issues/13)).
The note never un-parks the issue: leaving 045 stays a maintainer's reply.

The on-demand `/hive help` workflow uses the same first-line command shape, is
ignored for bot comments, and allows `triage`, `write`, `maintain`, or `admin`
because it only posts reference text and changes no labels.

### PR approvals

On a PR the approval is a trusted merger's: a normal GitHub **Approve**
review on the current head SHA, or a `/hive approve` reply posted after the
swarm's `requires_human` verdict (the reply is how an owner releases a PR
they opened themselves, since GitHub refuses the self-review). Either clears
`stage/085-needs-decision` for any author kind/lane on the next pass. No
special review body is required; the queued human-merge lane's
`Approved by @... for Hive auto-merge on green CI.` body remains specific to
that queue flow. A swarm `changes_requested` or `reject` verdict is not
overridden by a human approval; the head must be fixed or merged manually.
Neither form bypasses CI, `hold`-class labels, a sentinel alert or a lane
that only merges by hand (CI workflow / action files edited, tests removed,
an `auto_merge.human_merge_paths` match): the `stage/085-needs-decision`
entry comment names, per lane, what actually clears the park — the trusted
merger's approval, the label a person must remove, or a manual merge
([#13087](https://github.com/hivecommons/hive/issues/13087)). A `/hive
approve` reply on a parked PR gets one reply (deduplicated per comment)
saying exactly that; a reply from someone who is not a trusted merger is
ignored in silence, like every other ineligible command.

The verdict release does not touch labels, so a `needs-human` label still
blocks the PR on every lane. On a **hive-authored** PR parked at
`stage/085-needs-decision`, a trusted merger's `/hive approve` also lifts a
`needs-human` that **Hive** added — the supersession sweep's or the
escalation's ask for a person, which your approval answers — and applies the
transition the adjudicator's hand-off prescribes: `reviewer-passed` is added
and `reviewer-handoff` removed, so no label has to be edited by hand
([#13309](https://github.com/hivecommons/hive/issues/13309)). This mirrors the
issue rule of [#13075](https://github.com/hivecommons/hive/issues/13075): a
`needs-human` a person added, or one whose applier the timeline cannot name,
stays, no label is touched, and the reply says it still blocks the PR. CI,
soak, `hold` / `do-not-merge` and the verdict gates are unchanged. The reply
to every `/hive approve` on a PR says whether a `needs-human` label still
blocks it.

For a PR whose closing claims were superseded by another merged PR, a trusted
maintainer with repository write access can reply `/hive decision reject` or
`/hive decision close superseded by #N`. Hive rechecks that the claimed issues
are closed and a different merged PR closed them; an explicit `#N` must match
that closer. It comments and closes the PR with `hive/superseded`, including
PRs opened by contributors, without pushing to their branches. Hold and
`do-not-merge` gates still prevent the close. When nothing supersedes the PR,
a bare `reject` keeps its ordinary meaning (changes requested, nudging the
author), while an explicit `close superseded by #N` is answered with why the
PR stays open. The sweep's
`hive/supersession-pending` cause label keeps the unresolved decision at 085
through CI or rebase failures; reopening the issue, rewriting the claim, or
choosing `hive/keep-open` clears the sweep's park on its next pass.

On a **hive-authored** PR (`author/hive`) parked at `stage/085-needs-decision`
the same trusted human has a second lever
([#13041](https://github.com/hivecommons/hive/issues/13041)): a GitHub
**changes requested** review on the current head, or a `/hive decision <what
to change>` comment, moves the PR to `stage/070-fixing` and kicks the author
agent once per head with the review/decision text quoted verbatim plus the
swarm's outstanding findings; the swarm verdict for that head is superseded,
and the new head the agent pushes gets the normal swarm re-review. One kick per
(head, review/comment): a second sweep over the same head does not re-kick, a
dismissed review or a later approval by the same reviewer clears it, and a
changes-requested review left on an older head is outdated. The fix kick is
delivered ahead of the governor's own kick to the same agent in that eval
cycle, so it does not time out behind routine queue work; a kick that still
cannot be sent is logged as `review swarm kick not delivered` with the PR and
audited as `review_kick_undelivered`, and the planner re-plans it next cycle.
The whole comment after `/hive decision` is the instruction — numbered points
on the lines below the command are kept and quoted verbatim — and the
decision on the current head outranks the swarm's decision-requested verdict
on that head, so the stage moves in the same pass as the acknowledgement
rather than waiting on a re-review. A `/hive decision` whose first word is
`approve`, `approved`, or `lgtm` (a trailing comma or period is fine) is
recorded as approval; the rest of the comment is retained as its rationale. A
bare `yes` approves too, but `yes, but …` is a conditional answer and is routed
as an instruction. To request changes, start with the instruction
(e.g. `/hive decision change the PR body to Refs #123`). No fix is kicked
for an approval, requires_human is released for that head and the
PR follows the normal merge path. An acknowledged decision whose record did
not survive a pod restart is rebuilt from the comment thread; a decision
answered for an older head is left alone
([#14679](https://github.com/hivecommons/hive/issues/14679)).
The sweep also reads `/hive decision` at `stage/070-fixing`: approve the
current head after a body or metadata fix to release a standing request without
pushing a cosmetic commit. A later trusted GitHub APPROVED review on the same
head also supersedes the recorded request; an older approval does not. If
that review is later dismissed, or the reviewer asks for changes again on the
same head, the review-backed approval is dropped and the earlier instruction
stands again — a `/hive decision approve` comment is durable and is not.
Issue-link repair preserves edited `Refs` bodies and PRs with a recorded
decision, so it cannot restore auto-close over the owner's direction.
The 085 entry comment names both actions. The commenter must hold `write` or better on the
repo *and* be in the hive's trusted-merger set (the bar a human approval
already meets). `/hive decision` on a PR the hive did **not** open, or `/hive
approve` / `/hive help` on any PR, gets one reply naming the review-side
equivalents instead of silence; the hive never pushes to a PR it did not open.

The self-merge sweep's review verdict hold (App-authored and trusted-author
PRs, [review-swarm.md](review-swarm.md)) is released by a person's GitHub
**Approve** review or a `/hive approve` reply posted after the swarm's
`requires_human` or `reject` verdict. Once the sweep has seen that release, the
PR also leaves `human_hold` on `/api/review/pipeline` and the owner's "your
turn" inbox; a newer blocking verdict puts it back. A fresh verdict drawn only
by a clean base-branch merge (a `Merge branch 'main'` update that leaves the
PR's own non-merge commits and its patch against the merge-base, compared like
a patch-id, unchanged) is not newer in that sense: the release counts from the
first blocking verdict on that content, on every lane below too. A merge that
carries conflict resolution, or whose cleanliness cannot be read, starts a new
hold like a new commit or a rebase (hivecommons/hive#13086).

On the contributor lane (PRs opened under a person's login, including the
owner's own) a trusted merger releases a `requires_human` verdict the same
way: an **Approve** review or a `/hive approve` reply posted after the verdict.
GitHub does not let the author approve their own PR, so the reply is how an
owner who opened the PR records the decision. Only a trusted merger's release
counts there, so a contributor cannot release their own PR. The lane records
the release in the same ledger, so the PR leaves "your turn" and
`stage/085-needs-decision` (hivecommons/hive#13085).

The merge-request relay an agent uses to land a PR refuses one the swarm holds
with `requires_human` or `reject` until a trusted merger has released it that
way, whatever `review.require_approval` says (hivecommons/hive#13085).

`/fixed` and `/reopen` are intentionally narrower: they work for the issue's own
reporter or for `write`/`maintain`/`admin` collaborators. `/fixed` plain prose is
only meaningful when Hive has already asked whether a merged referenced fix can
close the issue; an explicit `/fixed` works on any open issue. Ambiguous prose
with negation or remaining-work language asks the commenter to use `/fixed`
instead of closing. On a parked issue — many park for an action a person must
take outside the repository — the un-park sweep itself honours a maintainer's
`/fixed`, so a repository without the `/fixed` workflow still gets the close,
the label clean-up and one acknowledgement.

## Release-only owner steps

A parked issue can carry a typed `kind=release` owner-step marker naming a tag
and immutable commit SHA (see [hive-open-issue.md](hive-open-issue.md#recording-a-release-only-owner-step)).
By default it remains manual: cut the release and reply `/hive done`, or use
`/hive decision <text>` to change direction.

On a repo with `project.repo_policies[].release_on_approve: true`, the issue's
"What to reply" notice says that `/hive approve` creates the named tag at the
full immutable SHA; `/hive done` and `/hive decision <text>` remain available.
An eligible `/hive approve`, or the dashboard Your-turn menu's
"Release `<tag>`" action, makes the hub App create exactly that tag and GitHub
Release. The dashboard action has the same repository-write/owner gate as
"Release hold" and calls the same executor as the command. On a repo with
`label_driven: true` the sweep answers `/hive approve` with the triage-label
pointer before reaching the release step, so `release_on_approve` has no effect
there and the dashboard offers no "Release" action either.
Immediately before the write, Hive confirms that the target is still the
default-branch head or its named ancestor, the tag is absent, and every required
check is green. When `release_version_source` names a repository-relative text
file such as `VERSION`, its value (with an optional leading `v`) must also match
the tag. A refusal names the failed condition and leaves the issue parked; a
success posts the release URL, records an audit event, and clears only Hive's
parking labels. Replaying an answered command cannot create another release.
The App performs this operation; the agent `gh release create` prohibition is
unchanged.

## Prose replies on parked issues

Plain approvals such as "approved", "go ahead", "sounds good", or "ship it" do
not un-park an issue. When an eligible maintainer writes prose that looks like
assent, Hive posts a hint asking for `/hive approve` or `/hive decision <text>`
and leaves all labels unchanged. This avoids turning casual agreement into a
state-changing instruction.

## Idempotency markers

Hive marks its standing parked-issue notice with
`<!-- hive:unpark-notice:9879 -->` and edits that comment in place when the
issue options change. Each answer to a specific maintainer comment is marked
with `<!-- hive:unpark-reply:9879 id=<comment-id> -->`. The sweep and the
on-demand `/hive help` workflow both check that per-comment marker before
posting, so reruns and sweep/action overlap do not duplicate replies or repeat
label changes.

The `/fixed` workflow uses the reporter-confirmation labels and comments
described above. While Hive waits, issues carry `needs-reporter-confirmation`
(and may also carry `needs-human` when the reporter is a maintainer). `/reopen`,
label-helper, and assignment-helper commands are single-comment workflows;
repeat comments can repeat their documented action.

However an issue closes — `/fixed`, a merged PR's `Fixes #N`, or by hand — Hive
clears the stage labels it left on it: the closed-issue sweep (every 10
minutes) strips `stage/*` and a Hive-added `needs-human` from recently closed
issues, so a closed issue never sits in the dashboard tracker as needing a
decision ([homebrew-hive#127](https://github.com/hivecommons/homebrew-hive/issues/127)).
A person's `needs-human` and any `hold` stay.

## The `hive/selfreport` label

Issues labelled `hive/selfreport` were filed by a hive about itself
(`[selfreport] <class>: …`, see [fleet-report.md](fleet-report.md)). Each
affected hive is listed in the `Affected hives (N)` block of the body; hives
that hit the same condition add themselves rather than filing duplicates, so
the body block is the fleet-wide tally. Hives post a "cleared on <hive>" comment when the
condition stops — at most once per hive per 24h for a flapping condition, and
never under a closed issue (the hive reads the state first and records the
close) — but never close these issues; close them when the fix has rolled
out. Closing a self-report silences its reporter for that fingerprint for as
long as the reporter keeps observing the condition (the 7-day cooldown runs
from the last observation, not the close), so an old build that still carries
the bug does not re-file daily after an "already fixed on v5" close. A hive
that was quiet about it for 7 days and then sees it again files a new issue,
so `/reopen` the original instead if the fix did not hold.

## Label-driven repositories

Some repositories accept and park issues entirely with labels and their own
lifecycle bot (for example `needs-triage` → `triage/accepted`, with the bot
removing `needs-human`). Clearing those labels from Hive would fight that
workflow, so an operator can opt a repository in per repo — in the config file
or from the dashboard (Settings → Repos → "Label-driven approvals
(Prow-style)", write or owner role):

```yaml
project:
  label_driven_approval_labels: [triage/accepted]   # default
  label_driven_trust_org_members: true              # default (Prow's rule)
  label_driven_trusted_bots: [kubestellar-prow[bot]]   # default
  # label_driven_supersede_park_actors: ["github-actions[bot]"]   # default empty
  repo_policies:
    - repo: projectbluefin/common
      label_driven: true
      # label_driven_approval_labels: [accepted]    # optional per-repo list
      # label_driven_supersede_park_actors: ["github-actions[bot]"]   # optional per-repo list
```

This is strictly opt-in (hivecommons/hive#11313). Repositories without
`label_driven: true` behave exactly as described in the sections above; the only
global changes are that an `issues` label webhook lets the un-park sweep run
early, and once-only reply markers are trusted only when the hive itself wrote
them.

On an opted-in repository the un-park sweep:

- posts no "What to reply" notice (`<!-- hive:unpark-notice:9879 -->`) and no
  hint for prose assent;
- treats a **trusted** addition of an approval label as approval: it removes
  the park labels Hive itself added, adds `approved-direction`, and replies once
  per acceptance (`<!-- hive:label-approval:11313 event=N -->`). No `/hive`
  comment is needed;
- answers `/hive approve` and `/hive decision <text>` the same way (Hive-added
  park labels only), and also accepts them from collaborators with the GitHub
  **triage** role; `/hive help` gets a one-line reply pointing at the labels;
- never removes a park label a person added, nor `hold` / `do-not-merge`. When
  one of those is present the issue stays parked and Hive posts one
  "accepted, but still held by …" note naming the labels.

#### Lifecycle bots that park every new issue

Some repositories run a lifecycle bot that adds `needs-human` to every new
issue and never removes it; Hive's rule of removing only its own park labels
would then keep an accepted issue parked forever
([#13605](https://github.com/hivecommons/hive/issues/13605)). List that bot in
`label_driven_supersede_park_actors` (project-wide, or per repo under
`repo_policies[]`) and a trusted acceptance added **after** the bot's park
label supersedes it: Hive removes the label, says so in the release comment
("`needs-human` (added by @github-actions[bot]), superseded by the acceptance
as this repository's configuration allows"), and releases the issue. A park
label added by a person, by an unlisted bot, or by a listed bot *after* the
acceptance still blocks exactly as before, and `hold` / `do-not-merge` are
never superseded. The list is empty by default.

Parked issues are still filtered from enumeration, and Hive may still add
`stage/045-needs-decision` with a comment asking the question. (Owner
decision, 2026-10-17: issues park at `stage/045-needs-decision`;
`stage/085-needs-decision` is PR-only. The un-park sweep still clears a
pre-045 085 from an issue.)

### Who may apply the approval label

Only the actor of the most recent `labeled` event for the label is checked
(removing and re-adding re-evaluates the newest event). Trusted, in order:

1. a bot in `label_driven_trusted_bots` (Prow command bots relaying
   `/triage accepted`). The default list is only `kubestellar-prow[bot]`.
   `github-actions[bot]` is not trusted by default because it is the shared
   identity of every workflow in the repo; if your Prow emulation runs as
   `github-actions[bot]`, add it explicitly per deployment;
2. a collaborator whose GitHub `role_name` is `triage`, `write`, `maintain` or
   `admin` (custom roles that build on triage count as triage);
3. a public member of the repository's owning org, when
   `label_driven_trust_org_members` is on (default).

Anything else — an untrusted person, an unlisted bot, a label Hive applied
itself, or a timeline that could not be read — is **not** approval; the sweep
fails closed and logs once.

### What each label does on a label-driven repository

Label approval layers on top of the existing label paths rather than replacing
them; the table states every effect in one place.

| Label | Un-park sweep (label-driven repo) | PR self-authorization sign-off | Reporter-trust admission (`issue_filter.reporter_trust`) |
|---|---|---|---|
| `triage/accepted` (or configured approval label) applied by a trusted actor | Releases the issue: Hive-added `stage/045-needs-decision` (and retired spellings) removed, `approved-direction` added, one reply; `needs-human` is kept even when Hive added it (only the `/hive` commands lift a Hive-added one) | Counts as human sign-off, like `approved-direction` | Unchanged: still one of the labels that admits an untrusted reporter's issue if listed in `untrusted_require_labels` |
| same label applied by an untrusted actor / unreadable timeline | No effect (fail closed) | Not sign-off | Unchanged |
| `approved-direction` | Issue is not parked | Human sign-off (unchanged) | n/a |
| `stage/045-needs-decision` added by Hive | Removed on label approval or `/hive approve` | Parked → no sign-off | n/a |
| `stage/045-needs-decision` added by a person | Kept; issue stays parked, Hive posts one "still held" note | Parked → no sign-off | n/a |
| `needs-human` added by Hive | Removed on `/hive approve` / `/hive decision` / `/fixed` — the maintainer's answer to the hive's own ask ([#13075](https://github.com/hivecommons/hive/issues/13075)); kept on label approval; removed when the issue closes | Parked → no sign-off | n/a |
| `needs-human` added by a bot listed in `label_driven_supersede_park_actors` before the acceptance ([#13605](https://github.com/hivecommons/hive/issues/13605)) | Removed on label approval, named in the release comment; added after the acceptance it is kept like a person's | Parked → no sign-off | n/a |
| `needs-human` added by a person (or unreadable timeline) | Kept on a label-driven repo: the lifecycle bot owns it, and only a person removes it; the issue stays derived at 045 and out of the agent queue. (Elsewhere `/hive approve` / `/hive decision` / `/fixed` lift it whoever added it — [docs#419](https://github.com/hivecommons/docs/issues/419).) | Parked → no sign-off | n/a |
| retired `needs-decision` / `needs-direction` ([#12683](https://github.com/hivecommons/hive/issues/12683)) | Migrated by the issue stage reconciler: `stage/045-needs-decision` added, retired label removed; a person's retired label therefore becomes a Hive-added 045 and `/hive approve` can clear it | Parked → no sign-off | n/a |
| `hold`, `do-not-merge` | Kept; issue stays parked (unchanged) | Hold (unchanged) | n/a |

## How the hive sees comments

The un-park path is polling, not a webhook: every sweep lists open issues, keeps
the "What to reply" notice on issues parked by `needs-human` or
`stage/045-needs-decision`, and then considers the newest eligible
comments within a bounded seven-day window and a per-sweep action cap. Agents may
still read issue threads directly in their prompts, but the deterministic label
transition to `approved-direction` happens only when the sweep accepts
`/hive approve` or `/hive decision <text>`.
