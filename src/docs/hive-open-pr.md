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
             --title "<title>" --body "<body>"
```

| Flag | Required | Default |
| --- | :---: | --- |
| `--repo` | yes | — |
| `--head` | effectively | the current git branch |
| `--base` | no | `main` |
| `--title` | yes | — |
| `--body` | no | empty |

`--repo`, `--head`, and `--title` must resolve or the script exits `2`. Both
`--flag value` and `--flag=value` forms work.

Flags `gh` accepts but this path does not need — `--draft`, `--fill`, `--web`,
`--no-maintainer-edit` — are **accepted and ignored**, so an agent's existing
command line does not need rewriting. Note that `--draft` being ignored means
**you cannot open a draft PR this way**; the PR opens ready for review.

## It is asynchronous, by design

On success the script prints the request path and returns `0`. **The PR has not
been opened yet at that point.** It opens on the next watcher tick.

This is deliberate: the agent's job is to *request* a PR; the hive owns opening
it. An agent that treats exit `0` as "the PR exists" and immediately tries to
comment on it will fail.

To confirm, poll the `.result.json` written next to the request file, or simply
look for the PR.

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

### The 404 that means "your push failed"

Every gate on the PR-open path begins by comparing `base...head`. If the
agent's branch was never pushed, GitHub answers 404 — and the message used to
read as though the *branch* were the problem, which sends an operator to
investigate branch creation. That is the wrong place: the branch is missing
because the **push** failed.

The watcher now investigates a 404 before reporting it ([#5343](https://github.com/kubestellar/hive/issues/5343),
fixed in [#5352](https://github.com/kubestellar/hive/pull/5352)). It probes the
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
