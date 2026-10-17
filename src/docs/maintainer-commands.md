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
| `/hive approve` | Accepts the recommendation already written on a parked issue. | Human commenter with `write`, `maintain`, or `admin` repository permission. | Open issues carrying `needs-human` or `stage/085-needs-decision` (or the retired `needs-decision` / `needs-direction`, until migrated); pull requests are ignored. | Next un-park sweep, normally within 5 minutes. | Removes `stage/085-needs-decision` (and the retired spellings) when present; adds `approved-direction`; **keeps** `needs-human` (the issue stays at 085 and out of the agent queue until a person removes it); never removes `hold`. |
| `/hive decision <text>` | Gives explicit direction for a parked issue. | Human commenter with `write`, `maintain`, or `admin` repository permission. | Open issues carrying `needs-human` or `stage/085-needs-decision` (or the retired `needs-decision` / `needs-direction`, until migrated); pull requests are ignored. | Next un-park sweep, normally within 5 minutes. | Removes `stage/085-needs-decision` (and the retired spellings) when present; adds `approved-direction`; **keeps** `needs-human` — a decision never lifts a hold for a person ([#12680](https://github.com/hivecommons/hive/issues/12680)); never removes `hold`. An agent's later re-park on 085 is refused while `approved-direction` is present and the recorded decision text is unchanged. |
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

The un-park sweep only honors `/hive approve`, `/hive decision <text>`, and the
sweep fallback for `/hive help` when the command is the first non-empty line of
the comment, outside quoted text and fenced code. The comment must be newer than
seven days, unedited, authored by a human rather than the hive or another bot,
and posted by someone who currently has `write`, `maintain`, or `admin`
permission on the repository. A `/hive decision` must include non-empty
instructions.

The on-demand `/hive help` workflow uses the same first-line command shape, is
ignored for bot comments, and allows `triage`, `write`, `maintain`, or `admin`
because it only posts reference text and changes no labels.

PRs use GitHub's native review signal instead of `/hive approve` or
`/hive decision`. When the L6 self-authored review gate is enforcing and the
swarm has not approved the current head because it is incomplete or
`requires_human`, a human who is trusted to queue merges can submit a normal
GitHub **Approve** review on that exact head SHA to clear the review hold. No
special review body is required; the queued human-merge lane's
`Approved by @... for Hive auto-merge on green CI.` body remains specific to
that queue flow. A swarm `changes_requested` or `reject` verdict is not
overridden by a human approval; the head must be fixed or merged manually.

`/fixed` and `/reopen` are intentionally narrower: they work for the issue's own
reporter or for `write`/`maintain`/`admin` collaborators. `/fixed` plain prose is
only meaningful when Hive has already asked whether a merged referenced fix can
close the issue; an explicit `/fixed` works on any open issue. Ambiguous prose
with negation or remaining-work language asks the commenter to use `/fixed`
instead of closing.

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
`stage/085-needs-decision` with a comment asking the question.

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
| `triage/accepted` (or configured approval label) applied by a trusted actor | Releases the issue: Hive-added `stage/085-needs-decision` (and retired spellings) removed, `approved-direction` added, one reply; `needs-human` is kept even when Hive added it | Counts as human sign-off, like `approved-direction` | Unchanged: still one of the labels that admits an untrusted reporter's issue if listed in `untrusted_require_labels` |
| same label applied by an untrusted actor / unreadable timeline | No effect (fail closed) | Not sign-off | Unchanged |
| `approved-direction` | Issue is not parked | Human sign-off (unchanged) | n/a |
| `stage/085-needs-decision` added by Hive | Removed on label approval or `/hive approve` | Parked → no sign-off | n/a |
| `stage/085-needs-decision` added by a person | Kept; issue stays parked, Hive posts one "still held" note | Parked → no sign-off | n/a |
| `needs-human` (whoever added it) | Kept: it is held for a person, and only a person removes it; the issue stays derived at 085 and out of the agent queue ([#12680](https://github.com/hivecommons/hive/issues/12680)) | Parked → no sign-off | n/a |
| retired `needs-decision` / `needs-direction` ([#12683](https://github.com/hivecommons/hive/issues/12683)) | Migrated by the issue stage reconciler: `stage/085-needs-decision` added, retired label removed; a person's retired label therefore becomes a Hive-added 085 and `/hive approve` can clear it | Parked → no sign-off | n/a |
| `hold`, `do-not-merge` | Kept; issue stays parked (unchanged) | Hold (unchanged) | n/a |

## How the hive sees comments

The un-park path is polling, not a webhook: every sweep lists open issues, keeps
the "What to reply" notice on issues parked by `needs-human` or
`stage/085-needs-decision`, and then considers the newest eligible
comments within a bounded seven-day window and a per-sweep action cap. Agents may
still read issue threads directly in their prompts, but the deterministic label
transition to `approved-direction` happens only when the sweep accepts
`/hive approve` or `/hive decision <text>`.
