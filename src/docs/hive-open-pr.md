# `hive-open-pr` — open a PR as the App bot

`bin/hive-open-pr.sh` is how an agent opens a pull request. Agents call it
**instead of `gh pr create`**.

It does not open the PR itself. It writes a request file that the hive's
PR-request watcher picks up and acts on using the App installation token, so
the PR is authored by the App bot (`<slug>[bot]`) rather than by whatever login
the agent's CLI happens to be authenticated as.

## Why it exists

Two reasons, and the second is the load-bearing one.

**Authorship.** A PR opened directly by an agent carries the backend CLI's user
identity — for Copilot-backed agents, a human's login. Routing through the App
makes hive-authored work identifiable as such.

**Forge resistance.** The script runs **as the agent**, in that agent's tmux
session under that agent's UID, so the request file it writes is owned by that
UID. The watcher re-derives the requesting agent from the **file's owner**, not
from anything written inside the file. An agent cannot claim to be another
agent by editing its own request.

`hive-open-pr` **adds no privilege**. The watcher enforces the same per-agent
ACMM write-gate and forge-resistance checks as the direct path
(`AuthorizePROpen`); the wrapper only changes *who* opens the PR.

## Usage

A drop-in for the common `gh pr create` shape:

```sh
hive-open-pr --repo <owner/repo> --head <branch> [--base <branch>] \
             --title "<title>" --body "<body>" [--issues <N[,N...]>]
```

| Flag | Required | Default |
| --- | :---: | --- |
| `--repo` / `-R` | yes | — |
| `--head` / `-H` | effectively | the current git branch |
| `--base` / `-B` | no | the target repo's default branch |
| `--title` / `-t` | yes | — |
| `--body` / `-b` | yes (or `--body-file`) | — |
| `--body-file` / `-F` | — | read the body from a file, or stdin with `-` |
| `--issues` / `--issue` | no | declare the originating issue number(s) |

`--repo`, `--head`, and `--title` must resolve or the script exits `2`. Both
`--flag value` and `--flag=value` forms work.

**Pass `--base`, and take the title from the target repository**
([#7159](https://github.com/hivecommons/hive/issues/7159)). Both defaults here
are hive's guesses about a repository it cannot see:

- The default branch is the wrong base for any repository on a promotion model,
  where the default branch is the *released* line and PRs land on an
  integration branch. `projectbluefin/bluefin` says so in its `AGENTS.md` and
  its CI enforces it; two hive PRs failed that gate. Read the repo's
  `AGENTS.md`, `CONTRIBUTING` and pull-request template, and pass what they
  name.
- A title is free-form here but not in the repository receiving it. A
  `[<lane>]` prefix is hive's own house style; repositories enforcing
  Conventional Commits reject it on the first character. The prefix remains
  **required on issue titles**, which the hive routes by lane
  (`pkg/classify.classifyLane`), and is not used on PRs.

**An empty body is refused**, loudly, with exit `2` and no request written.
Every shipped policy requires a real PR body; an empty one at this point means
the body was lost on the way in — the observed failure was `--body-file` being
silently dropped by an older parser, which opened PRs whose entire body was the
attribution footer. The floor is "non-blank" only: minimal bodies like
`Closes #12` still pass.

**On a host without `python3`, a control character in any field is refused**
with exit `3` and no request written. The request JSON is normally encoded by
python3; without it, a fallback escaper handles newline, carriage return, and
tab — everything a PR body legitimately contains — but refuses the remaining C0
controls rather than emit invalid JSON. Before
[#7839](https://github.com/hivecommons/hive/issues/7839) that refusal was
swallowed by a command substitution and the request was written with the
offending field silently blanked — the exact loss the empty-body guard above
exists to prevent; now nothing is written. Install `python3`, or strip the
control character, and rerun.

`--issues` declares which issue(s) this PR is for. The watcher then verifies
the body actually references each declared issue — `Closes #N`, or `Refs #N`
when part of the issue deliberately stays open — and rejects the request
otherwise. Pass it whenever the run started from an issue, so a truncated or
replaced body cannot open a PR that orphans its issue.

When the PR body resolves an issue (`Closes #N`, `Fixes #N`, or
`Resolves #N`), add the issue-author trailer before the first push with
`src/scripts/issue-coauthor.sh --amend N` (repeat once per resolved issue).
`issue-coauthor.sh` is the single source of truth for identity resolution: it
uses the GitHub API, emits the GitHub noreply address form, and skips cases
where no human credit is needed, such as bot-filed and self-filed issues.

`hive-open-pr` does not rewrite commits or force-push branches, because the
branch may already have been pushed or be under review. Instead, it asks
`issue-coauthor.sh` what trailer a closing issue should have and warns when
`HEAD` does not already contain it. Resolution failures warn and the PR request
continues: missing attribution is worth fixing, but must not block a shipped
fix. A `Refs #N` mention is deliberately not checked because it does not claim
the issue is resolved. `Co-authored-by:` is only attribution; it is **not** a
DCO sign-off. Never add `Signed-off-by:` for the issue author or anyone else
unless that person actually signed off on the commit.

Before writing `Refs #N`, answer the question directly: *does merging this PR
leave anything for issue #N to track?* If nothing, use `Closes #N` — that is
the default. Reserve `Refs #N` for an epic/tracker or a deliberately partial
fix, and say on the same line what remains open. #6411 (`Refs #6319, #6410`,
a follow-up tracker) and #6434 (a doc recording tracker state) are correct
uses of `Refs`; most PRs are not those, and #6152/#6547 exist because agents
defaulted to `Refs` out of caution rather than answering the question. Note
that even a correct `Closes #N` can be downgraded to `Refs #N` by the watcher
when auto-closing the issue would be unsafe — see
[Policy gates](#policy-gates-that-change-or-reject-your-request) below.

Flags `gh` accepts but this path does not need — `--draft`, `--fill`, `--web`,
`--no-maintainer-edit` — are **accepted and ignored**, so an agent's existing
command line does not need rewriting. Note that `--draft` being ignored means
**you cannot open a draft PR this way**; the PR opens ready for review. Any
other unrecognized flag is ignored with a warning on stderr naming it — with
one deliberate exception:

`--label`/`-l` is **accepted silently and its value discarded**
(`bin/hive-open-pr.sh:149`). No warning is printed, on purpose: `--label hold`
is in every hold-gated policy template, so it arrives on essentially every
agent PR, and "ignoring unrecognized flag `--label`" would read to an agent
mid-run as "your PR will not be held" — the opposite of the truth. The `hold`
label does land, but not because of the flag: the PR-request watcher applies it
server-side from authoritative ACMM config after the PR is created
(`src/pkg/github/pr_request_watcher.go:393`), and treats a failure to apply it
as a failed request, not a cosmetic miss. Every **other** label in the flag's
value (`--label documentation,hold`, say) is simply lost — the request file has
no label field. If you want a non-`hold` label on your PR, add it after the PR
exists, e.g. `gh pr edit <number> --repo <repo> --add-label documentation`.

## It is asynchronous, by design

On success the script prints the request path and returns `0`. **The PR has not
been opened yet at that point.** It opens on the next watcher tick.

This is deliberate: the agent's job is to *request* a PR; the hive owns opening
it. An agent that treats exit `0` as "the PR exists" and immediately tries to
comment on it will fail.

To confirm, poll the `.result.json` written next to the request file, or simply
look for the PR.

## The opened PR carries an attribution trailer

Because the PR is authored by the App bot, the bot identity alone cannot answer
"which agent/backend/model produced this?". So when the watcher opens the PR,
it appends a visible one-line trailer to the body you wrote
(`pkg/github.AppendTrailer`): the `— hive:` prefix followed by
space-separated `key=value` pairs, e.g. `agent=quality backend=bob model=auto
bobshell=1.0.6 requested_by=@octocat`. (A verbatim trailer line is not
reproduced here on purpose: the watcher's content check rejects a request whose
*committed files* contain one — run metadata belongs in the PR body or commit
trailer, not in the tree.)

Fields the hive does not know at launch are omitted rather than guessed; the
possible fields are `agent`, `backend`, `model`, `effort`, `<tool>=<version>`,
`session`, and `requested_by`. The trailer is gated by
`governor.attribution_trailer` (default ON), but the matching audit entry
(`agent_pr_created` in `audit.jsonl`, same key=value pairs — see
[audit-log.md](audit-log.md)) is written regardless of the toggle. The `— hive:`
prefix doubles as the stacking guard: a request the watcher retries after a
partial failure will not gain a second trailer. Do not write a `— hive:` line
into your own body — the watcher will treat it as an existing trailer and skip
its own.

### `requested_by` credits the human who asked

Since [#7208](https://github.com/hivecommons/hive/issues/7208), the trailer
credits — and, being an `@login` mention, notifies — the human who opened the
issue the PR answers:

- The watcher resolves it from the **same rationale issues the
  self-authorization gate reads**: the `Closes`/`Fixes`/`Refs` claims in your
  title and body plus the declared `--issues` list, in the same order. Citing
  your issue correctly is therefore also what routes credit to its opener.
- Only a **human** opener is credited. When every cited issue was filed by the
  hive itself or another bot, the field is omitted — there is nobody to thank.
- Lookups are capped at the first five cited issues, and a failed lookup is
  skipped, never fatal: attribution is a courtesy on top of the PR, and a PR
  opened without `requested_by` is still correct. Its absence is not an error
  to chase.

This is body-trailer credit for the *requester*; it is independent of the
`Co-authored-by:` **commit** trailer for a closing issue's author described
above (`issue-coauthor.sh`), and like it, it is not a DCO sign-off.

## Policy gates that change or reject your request

Beyond the empty-body and `--issues` checks above, the watcher applies three
policy gates before opening the PR. Two can rewrite what you wrote; one rejects
the request outright.

### `Closes #N` can be silently downgraded to `Refs #N`

The watcher (`pkg/github.validatePRRequestClaims`) looks up every issue your
title or body claims to close and rewrites `Closes #N` (also `Fixes`/`Resolves`)
to `Refs #N` in the **opened PR** when auto-closing that issue on merge would be
unsafe:

- **The issue is a tracker or epic** — an `epic`/`tracker`/`meta-tracker`
  label, an `[epic]`/`[tracker]` title prefix, or tracker-shaped content.
- **The issue delegates unfinished work to other issues** — an unchecked
  task-list item whose subject is another issue (`- [ ] #123`,
  `- [ ] owner/repo#12`) means the issue tracks work your PR cannot land, so
  merging it does not finish the issue. A task list of plain deliverables —
  the completion criterion every policy template asks a filing agent to write
  for an unsplittable finding — does **not** trigger this, and boxes inside a
  fenced code block never count, so quoting the policy's own `- [ ]` example is
  free ([#7156](https://github.com/hivecommons/hive/issues/7156)).
- **The issue is a human-filed bug that the reporter has not confirmed fixed**
  ([#6781](https://github.com/hivecommons/hive/issues/6781)). Merges land under
  the App bot, and GitHub does not let a reporter without write access reopen
  an issue the App bot closed — so an unverified auto-close strands the
  reporter (that dead end produced [#6762](https://github.com/hivecommons/hive/issues/6762)
  and [#6767](https://github.com/hivecommons/hive/issues/6767)). A reporter can
  now comment `/reopen` on their own closed issue to reopen it
  ([#6799](https://github.com/hivecommons/hive/issues/6799),
  `.github/workflows/issue-reopen-command.yml`), but that is a backstop — this
  downgrade gate is the primary control that stops the bad close from
  happening at all. The gate
  triggers only when **all** of these hold: a bug-family label (`bug`,
  `kind/bug`, `type/bug`, `type:bug`, `adoption-blocker`), no `— hive:`
  attribution trailer in the body (so agent-filed findings are unaffected), and
  a non-Bot author. The reporter or a maintainer opts back in to auto-close by
  adding `hive: reporter-confirmed` to the issue body or applying it as a
  label; the downgrade then does not fire and `Closes #N` goes through.

A downgraded reference says so in the PR body it lands in:

```text
Refs #318 — closing keyword withheld by the hive watcher: `issue is a tracker`. Merging this PR will not close the issue.
```

so a maintainer reading the PR can tell a watcher rewrite from a deliberate
`Refs` ([#7156](https://github.com/hivecommons/hive/issues/7156)). The title is
rewritten without the note. The hive log carries the same reason under
`pr-request watcher: downgraded closing reference to Refs`, but that log has a
retention window and the PR body does not. A merged fix is evidence the code landed, not that the
reporter's symptom is gone; the issue stays open until the reporter confirms.

### Base drift: a branch cut from the wrong line is rejected

A head more than **100 commits behind its base** is rejected permanently
([#6807](https://github.com/hivecommons/hive/issues/6807)). A branch cut from
the base's own tip is behind by minutes-to-hours of commits; one cut from a
*different* branch (typically the repository default when the PR targets
another line) carries the full divergence — observed at 546 commits, producing
170+-file unmergeable PRs whose diff is mostly the target branch's own history
rendered as deletions. No metadata change can fix this, so the request is
quarantined as `.rejected` with re-cut instructions in `.result.json`:

```sh
git fetch origin <base> && git checkout -b <branch> origin/<base>
# re-apply your commits, push, and issue a fresh hive-open-pr request
```

Always create working branches from `origin/<target-branch>` — the branch the
PR will target — never from whatever the checkout happens to be on.

### Title claims are checked against the diff

A title containing `workflow`, `test`, or `migration` is verified against the
compared file list: if the diff contains no file of that kind, the request is
rejected (`title claims test but diff contains no test file`). Retitle the PR
to describe what the diff actually changes.

## Diagnosing a PR request that never opens

When the PR does not appear, the request file's `.result.json` is the record of
why. It sits next to the request in `/var/run/hive-metrics/pr-requests`, named
`<request>.result.json`, and on failure carries `"ok": false` with an `error`
string.

The request file itself is also renamed once it stops being retried, and the
suffix tells you which kind of failure it was:

| Suffix | Meaning | Retried? |
| --- | --- | --- |
| *(none)* | still queued | yes, with backoff |
| `.failed` | transient failure that never recovered within the give-up horizon | no longer |
| `.rejected` | a policy gate refused the content — the request or branch must change | no |
| `.denied` | authorization refused (ACMM write-gate, forge-resistance) | no |

A transient failure backs off exponentially from 30s to a 15-minute ceiling and
is quarantined as `.failed` only after **24 hours** without success. So a
freshly failing request is normal-looking for a while: read `.result.json`
rather than waiting for a rename.

### Workflow-file pushes rejected by App tokens

A branch push that changes any file under `.github/workflows/` needs the
GitHub App installation to have the **Workflows** permission. Without that
permission, GitHub rejects the push before `hive-open-pr` can request a PR:

```text
! [remote rejected] ci/dco-waive-01bd2469 -> ci/dco-waive-01bd2469
  (refusing to allow a GitHub App to create or update workflow
   `.github/workflows/dco-post-merge.yml` without `workflows` permission)
```

The branch and filename vary; the trigger is any pushed change under
`.github/workflows/`.

This is specific to pushes authenticated with the GitHub App installation
token. A maintainer's own `gh`/git credentials are unaffected: a maintainer
probe against this repository pushed a branch touching
`.github/workflows/dco-post-merge.yml` successfully, then deleted the branch.
Treat that as current operational evidence, not a permanent guarantee; an org
admin can grant or change the App permission at any time. The outstanding App
permission change is tracked in [#6985](https://github.com/hivecommons/hive/issues/6985).

When an agent hits this rejection, do not claim that nobody can change the
workflow. Post a patch on the tracking issue for a maintainer to apply with
their own credentials. A useful patch comment contains:

1. the full path of every changed workflow file;
2. a complete unified diff that applies from the target branch; and
3. the verification command/output the maintainer should run after applying
   the patch, such as the docs checker, focused script test, or affected
   workflow dry run.

Until the App installation grants Workflows read/write, App-authored
workflow-file PRs cannot be delivered directly. See the
[troubleshooting runbook](troubleshooting.md#workflow-file-pushes-are-rejected-by-github-app-tokens)
for the owner-side permission update steps.

### The 404 that means "your push failed"

Every gate on the PR-open path begins by comparing `base...head`. If the
agent's branch was never pushed, GitHub answers 404 — and the message used to
read as though the *branch* were the problem, which sends an operator to
investigate branch creation. That is the wrong place: the branch is missing
because the **push** failed.

The watcher now investigates a 404 before reporting it ([#5343](https://github.com/hivecommons/hive/issues/5343),
fixed in [#5352](https://github.com/hivecommons/hive/pull/5352)). It probes the
**repository** first, then the **head ref**, and reports one of three distinct
outcomes. Only a definitive 404 is investigated — a 403, a rate limit, or a 5xx
keeps its own identity so the existing retry and rate-limit handling still
recognise it.

**1. The App cannot see the repository.** The repo probe 404s:

> cannot open a PR on `<owner>/<repo>`: this hive's GitHub App cannot see that repository (404). Check that the App is installed on it and that the installation grants contents+pull_requests access — this is NOT a problem with branch `<branch>`

The branch is not implicated at all. Fix the App installation — see
[GitHub App setup](github-app-setup.md).

**2. The repository is visible but the head ref is genuinely absent.** This is
the push-authentication case, and the error says so:

> branch `<branch>` was never pushed to `<owner>/<repo>` — the commits exist only in the agent's working copy. This is almost always a PUSH AUTHENTICATION failure, not a branch-creation problem

It names the two causes to check, in order:

1. **The git credential helper is not reachable from the agent's UID.**
   `su -s /bin/sh hive-<agent> -c 'git config --get-regexp credential'` must
   list `/usr/local/bin/git-credential-hive.sh`. It is wired system-wide in
   `/etc/gitconfig` precisely because agents do not share the dev user's
   `$HOME`.
2. **The agent's scoped token file is absent or unreadable by that UID.** The
   path is in `$HIVE_AGENT_TOKEN_CACHE`. Check readability only — never print
   its contents.

This stays a **retryable** error rather than a policy rejection: once the agent
pushes the branch, the same request becomes valid and succeeds on retry. That
is exactly what the bounded retry is for, so fix the credential problem and let
the queued request land — you do not need to re-issue it.

**3. The head ref exists, so the 404 was about something else.** Usually the
base:

> GitHub returned 404 although branch `<branch>` exists — check that the base branch `<base>` exists on the remote

Note the watcher deliberately does **not** try to read the agent's git state or
test the credential helper itself: it runs in the hive process, not in the
agent's UID, so any such check would answer a different question than the one
that failed. It names the causes; you verify them from the agent's UID.

### The log line to grep for

Case 2 is also logged at ERROR on its own line, because it is work an agent
already completed and cannot publish — and it is invisible in the fleet view,
since the agent's session ended healthy:

```sh
kubectl -n hive logs deploy/hive | grep 'pr-request watcher'
```

Look for `head branch is not on the remote — the agent's push did not
authenticate`, which carries `repo`, `head`, `agent`, and the full `diagnosis`.

For the broader symptom — an agent that completed a session with no branch and
no PR, and how to tell a credential-helper failure from a mid-task token
refresh failure — see
[Troubleshooting: an agent session completes but no branch or PR appears](troubleshooting.md#an-agent-session-completes-but-no-branch-or-pr-appears).

## Where things live

| Path | What |
| --- | --- |
| `/var/run/hive-metrics/pr-requests` | request files the watcher consumes |
| `/var/run/hive/uid-map.json` | UID → agent-name map, used for a nicer log line |

The UID map is **informational only** here. The watcher re-derives ownership
from the file's UID regardless, which is why a stale or missing map cannot be
used to misattribute a request.

## Related

- [Security threat model](security-threat-model.md) — forge resistance and the
  UID-ownership anchor
- [Agent configuration](agent-configuration.md) — ACMM levels and the write
  gate that governs whether an agent may open a PR at all
- [Audit log](audit-log.md) — PR creation is recorded there with the requesting
  agent
