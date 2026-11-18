# Outbound text: no em-dashes

Nothing hive or its agents send out of the hive may contain an em-dash
(U+2014), or an en-dash (U+2013) used as a dash. This page lists where that
rule is enforced and how CI keeps new send paths from skipping it
([#18970](https://github.com/hivecommons/hive/pull/18970),
[#19103](https://github.com/hivecommons/hive/issues/19103)).

## The filter

There is one filter, `src/crux/prosefilter`. The root module reaches it
through `src/pkg/prosefilter`, which only delegates. The filter lives in the
`crux` module so the egress proxy, which cannot import `pkg/`, uses the same
function.

- `Sanitize` rewrites multi-line Markdown prose. An em-dash becomes ` - `
  (or `- ` / ` -` at the start or end of a line). An en-dash with spaces on
  both sides becomes ` - `. An en-dash in a range such as `3-5` written with
  U+2013 is left alone.
- `SanitizeTitle` does the same for a single line.
- `SanitizeJSON` applies the same rule to the string values under named keys
  of a JSON document, at any depth, and leaves every other value unchanged.
- Fenced code blocks, inline code spans and blockquote lines are never
  changed, because they carry quoted or pasted content.

Agents are also told the rule in every kick (the plain-dash rule in the kick
header), so most text never needs rewriting.

## Where it applies

| Path | Boundary | Filtered |
| --- | --- | --- |
| Agent relay requests (PR, issue, comment, review bodies and titles) | `relayBody`, `CreatePR`, the issue-request watcher (`src/pkg/github`) | yes |
| Agent GitHub writes through the egress proxy (`gh api`, `gh pr comment`, `gh pr create`, REST and GraphQL) | `sanitizeAgentWriteProse` in `src/crux/proxy/outbound_prose.go` | yes |
| Agent Linear writes through the egress proxy | same, Linear mutation variables | yes |
| Agent commit messages | HEAD commit message in the push broker (`src/crux/pushbroker/commit_message.go`) | yes, HEAD only (see below) |
| Hive-signed commits (`createCommitOnBranch`) | `src/pkg/github/pr_request_signed.go` | yes |
| Squash and merge commit messages hive writes | `MergePRVia` in `src/pkg/github/pullrequest.go` | yes |
| Hive-rendered GitHub comments, issues and status notes | source templates | enforced at the source by the emit ratchet |
| ntfy, Slack and Discord webhook alerts | `notify.(*Notifier).Send` | yes |
| Slack DMs from the hub | `hub.sendSlackDM` | yes |
| Discord bot messages, embeds, edits, threads, topics | `discord.(*Bot).do`, `setChannelTopic` | yes |
| Hub Discord webhooks (requests, GitHub activity feed) | `hub.postDiscordRequestWebhook`, `hub.(*GitHubActivityFeed).postDiscord` | yes |
| Hub ntfy alert | `hub.(*HubServer).pushAuthAuditAlert` | yes |
| Meeting chat and spoken text (Quorum `chat.post`, `say`, `say.prewarm`) | `meetings.(*Client).post` | yes |
| Persona voice sample | `dashboard.meetingsSpeak` | yes |
| Hub-filed GitHub issues and comments (self-reports, feedback) | `hub.postHubGitHubIssue`, `hub.postHubGitHubIssueComment` | yes |
| Hub seen-log and affected-hives edits | `pkg/fleetreport` templates | emit ratchet |
| Linear and Jira work-source writes | `worksource.linearGraphQL`, `worksource.(*jiraSource).doJSON` | yes |
| Linear agent activity | `linearagent.doGraphQL` | yes |
| DIBS inbox ideas | `inbox.(*DibsDestination).Deliver` | yes |
| GitLab, Gitea and Forgejo writes | `pkg/forge` | emit ratchet |
| Email replies | no sender is wired yet | not applicable |

## The egress proxy

An agent that writes to GitHub itself talks to GitHub through the hive's MITM
egress proxy. Before #19103 the relay forwarded an allowed write verbatim, so
an agent's `gh api` comment kept its em-dashes even though hive's own writes
were filtered. The proxy now rewrites, just before forwarding:

- REST JSON bodies: `body`, `title` and `message` at any depth, which covers
  issue and PR comments, PR and issue titles and bodies, review bodies and
  their inline `comments[].body`, review-thread replies and contents-API
  commit messages.
- GraphQL mutation variables: `body` and `title` when the document names a
  comment, review, review-thread reply, PR or issue mutation (`addComment`,
  `addPullRequestReviewComment`, `addPullRequestReviewThreadReply`,
  `createPullRequest`, `updatePullRequest`, `createIssue`, `updateIssue` and
  the related review and comment-edit mutations). The query text is never
  changed, so a string literal written inline in the query is not rewritten.
- Linear mutation variables: `body`, `title` and `description`.

It never changes a body that is not JSON, a body that declares a non-JSON
`Content-Type`, any other field, git smart-HTTP or LFS traffic, or the hive's
own control-plane requests. A body over 1 MiB is forwarded unchanged rather
than truncated. When a body changes, its `Content-Length` is recomputed. One
debug line records the agent, method, path and the number of fields and dashes
changed, never the text.

## Commit messages

A commit message travels inside the pushed git objects, so it cannot be
rewritten in transit without changing object ids. The push broker, which
already amends an agent's HEAD commit for the trailing-newline rule, rewrites
the HEAD commit message with the same filter before pushing. Only HEAD is
rewritten: an older commit in the same push keeps its message, because the
broker never rebases an agent's branch. Hive squash-merges, and the squash
commit message it writes is filtered, so the commit that lands on the default
branch is clean either way. Hive installs no git hooks in agent workspaces
(hooks are disabled with `core.hooksPath=/dev/null`), so this is done by the
broker rather than a `commit-msg` hook.

## Switch

`HIVE_AGENT_PROSE_FILTER` turns off the rewriting of text an agent wrote
itself (its writes through the proxy and its HEAD commit message). It is on by
default; `false`, `0`, `off` or `no` turns it off. Text hive composes is always
filtered.

## CI guards

- `TestOutboundSitesInventory` (`src/pkg/prosefilter/outbound_sites_test.go`)
  scans every non-test Go file under `src/pkg`, `src/cmd` and `src/crux` for
  functions that send an HTTP request with a body. Each one must be listed in
  its inventory as filtered (it calls the filter), filtered upstream by a
  named function that calls the filter, a hive template under the emit
  ratchet, or machine traffic with a reason. A new sender that is not listed
  fails CI, as does a filtered site that stops calling the filter. The test
  also pins the proxy, push broker, relay, PR, merge and notification
  boundaries.
- `TestNoNewDashesInEmittedText` (`src/pkg/prosefilter/emit_guard_test.go`)
  is a ratchet on hive's own templates: the dash count of each emitter file
  may only go down.

## Follow-ups

- GraphQL string literals inlined in the query text are not rewritten.
- Commits other than HEAD in a multi-commit push keep their messages.
