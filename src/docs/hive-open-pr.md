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
