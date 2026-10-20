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
| `/hive approve` | Accepts the recommendation already written on a parked issue. On a pull request it is a trusted merger's release of the swarm's `requires_human` hold (see [PR approvals](#pr-approvals)). | Human commenter with `write`, `maintain`, or `admin` repository permission; on a pull request, a trusted merger. | Open issues carrying `needs-human` or `stage/045-needs-decision` (or the retired `needs-decision` / `needs-direction` / a pre-045 `stage/085-needs-decision`, until migrated). Open pull requests parked at `stage/085-needs-decision`: the merge lanes read the reply as a hold release and the un-park sweep answers it once, explaining what it does and does not clear ([#13087](https://github.com/hivecommons/hive/issues/13087)); it changes no PR labels itself. | Next un-park sweep, normally within 5 minutes. | Removes `stage/045-needs-decision` (and the retired spellings) when present; adds `approved-direction`; `needs-human` lifts when **Hive** added it (an agent parked the issue on a person — your approval is that person's answer, and the stage engine derives 010; [#13075](https://github.com/hivecommons/hive/issues/13075)) and **stays** when a person added it or the timeline cannot say who did (the issue stays at 045 and out of the agent queue until a person removes it; [#12680](https://github.com/hivecommons/hive/issues/12680)); never removes `hold`. |
| `/hive decision <text>` | Gives explicit direction for a parked issue. | Human commenter with `write`, `maintain`, or `admin` repository permission. | Open issues carrying `needs-human` or `stage/045-needs-decision` (or the retired `needs-decision` / `needs-direction` / a pre-045 `stage/085-needs-decision`, until migrated); pull requests are ignored. | Next un-park sweep, normally within 5 minutes. | Removes `stage/045-needs-decision` (and the retired spellings) when present; adds `approved-direction`; `needs-human` lifts when **Hive** added it (your decision is the human input Hive asked for; left in place it would re-derive 045 two minutes later, [#13075](https://github.com/hivecommons/hive/issues/13075)) and **stays** when a person added it or the timeline cannot say who did — a decision never lifts a person's hold ([#12680](https://github.com/hivecommons/hive/issues/12680)); never removes `hold`. An agent's later re-park on 045 is refused while `approved-direction` is present and the recorded decision text is unchanged. |
| `/hive done` | Says you did a step only the owner can do (a repository setting, secret, app install, …). Hive un-parks the issue exactly as `/hive approve` does and records the decision "the owner-only step is done; Hive re-checks it and closes the issue once the fix is verified" in its reply; that reply is the hand-off — the lane that next picks the issue up reads it, re-checks the step and closes the issue once verified. No label or queue state carries the decision. The "What to reply" notice offers it on issues an agent parked with `hive-open-issue comment --needs-decision` or filed with `hive-open-issue issue --needs-decision`, while that text lists no lettered options. `/hive help` does not answer the step; only `/hive approve`, `/hive decision` or `/hive done` does. | Human commenter with `write`, `maintain`, or `admin` repository permission. | Open issues carrying `needs-human` or `stage/045-needs-decision` (or the retired `needs-decision` / `needs-direction` / a pre-045 `stage/085-needs-decision`, until migrated); pull requests are ignored. | Next un-park sweep, normally within 5 minutes. | Removes `stage/045-needs-decision` (and the retired spellings) when present; adds `approved-direction`; `needs-human` lifts when **Hive** added it and **stays** when a person added it, exactly as for `/hive approve` ([#13075](https://github.com/hivecommons/hive/issues/13075)); never removes `hold`. |
| `/hive help` | Posts this command list. On parked issues it also repeats the issue-specific approve/decision options. | Any human commenter with at least `triage` repository permission for the on-demand workflow; the sweep path on parked issues requires `write`, `maintain`, or `admin`. | Any open issue or pull request. | GitHub Actions on the new comment; if Actions is unavailable on a parked issue, the next un-park sweep can also answer. | None. |
| `/fixed` | Confirms that an open issue's fix is verified and closes it as completed. Alias: `/close`. Plain-language confirmations such as "yes, this has been fixed" are accepted only when Hive is already waiting for reporter confirmation. | The issue reporter, or a human with `write`, `maintain`, or `admin` repository permission. | Open issues only, not pull requests. | GitHub Actions on the new comment. | Adds `hive: reporter-confirmed`; removes `hive/likely-done` and `needs-reporter-confirmation` when present. |
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
`/hive done`, and the sweep fallback for `/hive help` when the command is the first non-empty line of
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
`/hive approve` picks the recommended option when the text names one, else the
first option. When there are no options and no recommendation, the notice
offers only `/hive decision <your instructions>`.

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
instead of closing.

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

## The `hive/selfreport` label

Issues labelled `hive/selfreport` were filed by a hive about itself
(`[selfreport] <class>: …`, see [fleet-report.md](fleet-report.md)). Each
affected hive is listed in the `Affected hives (N)` block of the body; hives
that hit the same condition add themselves rather than filing duplicates, so
the body block is the fleet-wide tally. Hives post a "cleared on <hive>" comment when the
condition stops but never close these issues — close them when the fix has
rolled out. A fingerprint closed within the last 7 days is not re-filed; a
recurrence after that reopens the search and a new issue may appear, so
`/reopen` the original instead if the fix did not hold.

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
  repo_policies:
    - repo: projectbluefin/common
      label_driven: true
      # label_driven_approval_labels: [accepted]    # optional per-repo list
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
| `needs-human` added by Hive | Removed on `/hive approve` / `/hive decision` — the maintainer's answer to the hive's own ask ([#13075](https://github.com/hivecommons/hive/issues/13075)); kept on label approval | Parked → no sign-off | n/a |
| `needs-human` added by a person (or unreadable timeline) | Kept: it is held for a person, and only a person removes it; the issue stays derived at 045 and out of the agent queue ([#12680](https://github.com/hivecommons/hive/issues/12680)) | Parked → no sign-off | n/a |
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
