# v6 readiness live-exercise runbook

Use this page to finish the maintainer half of the v6 readiness bar after the
conformance suites have already proved the shared guard invariant. Each
exercise needs one live hive on the `v6` branch, a real operator account, and
links or screenshots that are posted as an evidence comment on
[#7563](https://github.com/hivecommons/hive/issues/7563) and linked from that
surface's live row. #7563 is the single live-evidence tracker: the per-surface
issues #8041–#8048 were folded into it on 2026-09-22 and closed, so there is no
surface issue to paste into or close. Do not tick a row from unit tests alone.

Common setup for chat surfaces:

- Use a maintainer-controlled v6 hive with dashboard auth available to the chat
  spine. Chat commands call dashboard endpoints such as `/api/status` and
  `/api/kick/<agent>` (`src/pkg/chat/dashboard.go:15-19`,
  `src/pkg/chat/dashboard.go:127-143`), and notification delivery comes from the
  `/api/events` SSE stream (`consumeSSE`, `src/pkg/chat/notify.go:108-161`).
- Pick an allowlisted human ID for the target surface. The shared command router
  fails closed when `allowed_users` is empty and logs ignored commands from
  non-allowlisted users (`Service.routeMessage` in `src/pkg/chat/router.go`).
- `allowed_users` entries may be `id` or `id:role`; roles are `read`,
  `read-write`, `merger`, and `owner`. The first bare entry is treated as
  `owner`, and later bare entries are treated as `read`.
- For the notification half, create a harmless visible state transition after
  the bot has started, such as pausing and resuming a non-critical agent from
  the dashboard. Valid notifications are the rendered `Working`, `Completed`,
  `Paused`, `Resumed`, `Off (cadence rule)`, or governor-mode-change messages
  emitted from SSE snapshots (`diffAgents`, `src/pkg/chat/notify.go:253-297`;
  `diffGovernor`, `src/pkg/chat/notify.go:398-403`).
- The command round-trip can be `!status` because it reads `/api/status` and
  returns a status message (`src/pkg/chat/dashboard.go:15-58`); `!kick <agent>
  readiness smoke` is also acceptable when the agent can safely be kicked.

## Evidence template

Post one filled block as a comment on
[#7563](https://github.com/hivecommons/hive/issues/7563), then tick the
surface's row in its **v6 readiness bar** section and link the comment from the
row. The sections below add a surface-specific evidence checklist; include
every item from it in the same comment.

```markdown
### v6 live exercise evidence — <surface>

- [ ] Hive/version/commit: `<v6 SHA, deployment name>`
- [ ] Operator identity used: `<GitHub login / Slack U… / Discord ID / MXID / etc.>`
- [ ] Config confirmed: `<redacted hive.yaml keys or env names, no secrets>`
- [ ] Command/action performed: `<exact command or action>`
- [ ] Round-trip evidence: `<message link, screenshot, issue comment link, email Message-ID, provider event URL>`
- [ ] Notification/escalation evidence: `<message link, screenshot, email Message-ID, provider event URL>`
- [ ] Hive evidence: `<redacted log line, audit event, API endpoint evidence, timestamp>`
- [ ] Guard evidence: `<allowlist/role used; ioscan/capability path unchanged; any relevant audit>`
- [ ] #7563 **v6 readiness bar** row ticked and linked to this comment: `<link to the tracker edit>`

Notes / redactions: `<what was redacted and why>`
```

<a id="github-mention"></a>

## GitHub @-mention triggers

Prerequisites:

- Enable the shipped mention trigger described in
  [GitHub @-mention triggers](design/github-mention-triggers.md), which records
  `source=mention` kicks, 👀 ack, and audits on v6
  (`src/docs/design/github-mention-triggers.md:3-11`).
- Configure `github.mentions.enabled`, the managed repo list, a
  `github.mentions.default_agent` or an unambiguous mention-capable agent, and
  either a trusted dashboard role floor or explicit summoners. The config fields
  and defaults include `summoners`, `min_role`, and the default `eyes` ack
  reaction (`src/pkg/config/config.go:1865-1885`). If webhook acceleration is
  used, set `github.mentions.webhook_secret_env`; polling alone is acceptable
  (`src/pkg/config/config.go:1874-1876`, `src/pkg/mention/webhook.go:32-41`).
- The selected agent must be enabled, hold `Converse`, have a mention channel,
  and be governor-kickable; the handler declines otherwise
  (`src/pkg/mention/types.go:177-223`).

Run:

1. As the allowlisted maintainer, comment on a real managed issue or PR:
   `@<app-login> ask <agent> v6 readiness live exercise: please acknowledge this thread`.
2. Wait for the poller or webhook accelerator to handle the comment. Save the
   comment permalink, the 👀 reaction link/screenshot, and the hive log around
   the handling window.
3. Confirm the kick was recorded with `source=mention:<node id>` and the audit
   event is `agent_mention_kicked`; declined attempts use
   `agent_mention_declined` (`src/pkg/mention/types.go:14-16`,
   `src/pkg/mention/types.go:100-124`).
4. If the run completes, link the App-bot completion reply or the run log. The
   responder promotes pending mention kicks when it sees `kick-delivered` /
   `kick-log-archived` and can post a thread reply (`src/pkg/mention/responder.go:34-61`,
   `src/pkg/mention/responder.go:87-97`).

Evidence checklist:

- [ ] Issue/PR mention comment URL.
- [ ] 👀 ack screenshot or reaction URL.
- [ ] Audit/log line containing `agent_mention_kicked` with repo, number,
      comment ID, author, and agent.
- [ ] Kick/run evidence showing `source=mention`.
- [ ] Paste the template as a comment on #7563, tick the GitHub @-mention row in
      its **v6 readiness bar** section, and link the evidence comment from the row.

## GitHub Actions trigger

Covers both Actions rows in the [live-exercise table](v6-readiness.md#3-live-exercise-per-surface):
comment relay (the row on #7563) and hub OIDC dispatch. Both go through the same
mention handler, so the guard path, 👀 ack, dedupe, and audit are the ones the
@-mention section above already exercises; this section proves the
Actions-originated entry points reach it.

Prerequisites:

- Everything the @-mention section needs, plus `github.actions.enabled: true`
  with `status` in `github.actions.allowed_commands` (the smoke dispatches
  `status`) and `github-actions[bot]` in `trusted_comment_authors` (the
  default). For the OIDC row also set `github.actions.oidc.enabled: true` and
  `github.actions.oidc.audience`; the workflow's `audience` input must match it
  exactly. The keys are documented in
  [GitHub Actions trigger](github-actions-trigger.md#hive-configuration).
- The smoke is [`hive-action-smoke`](../../.github/workflows/hive-action-smoke.yml),
  a `workflow_dispatch` with inputs `issue`, `prompt`, `transport`
  (`comment` | `oidc`), `hub_url`, and `audience`. The comment transport posts on
  an issue **in the repository the workflow runs in**, so run it from a
  repository the live hive manages: this repository if it is in the hive's repo
  list, otherwise a copy of the workflow in a managed repository. Bot actors are
  refused on repositories the hive does not govern.
- The relay comment body is `@hive status <prompt>` plus a hidden
  `<!-- hive:source=action … transport=comment -->` marker
  (`.github/actions/hive/action.yml`). The parser matches `@<app-login>` or the
  login without its `[bot]` suffix (`findMention` in
  `src/pkg/mention/grammar.go`), so the hive's App bot login must be
  `hive[bot]` for the relay comment to be seen as a mention.
- The `github.actor` who dispatches must hold the required Hive role directly,
  or be mapped to a dashboard identity that does through
  `github.actions.identity_map`.

Run:

1. **Comment relay.** Dispatch `hive-action-smoke` with `transport=comment`
   and `issue=<real managed issue>`. Save the workflow run URL and the relay
   comment permalink.
2. Wait for the poller or webhook accelerator. Save the 👀 reaction on the relay
   comment and the hive log for the handling window. The kick source is
   `action:<repo>:<run_id>:<run_attempt>` (`kickSource` in
   `src/pkg/mention/types.go`) and the audit event is `agent_mention_kicked`
   with `source=action`, `actor`, `run_id`, `run_attempt`, `workflow`, `ref`,
   and `transport=comment` in its detail (`Handler.audit`, same file). A
   refusal is `agent_mention_declined`; the Actions-specific reasons are
   `action-author`, `action-disabled`, `action-command`, and `action-run`.
3. Re-run the same workflow attempt and confirm no second kick: reruns are
   deduped by `run_id` and `run_attempt`.
4. **Hub OIDC dispatch.** Dispatch again with `transport=oidc`, `hub_url`, and
   `audience`. The workflow's "Assert OIDC receipt output" step fails unless the
   hub returned a receipt with `kind: stage_receipt` and
   `stage_receipt.schema_version: stage-receipt/v1`; save the run URL with that
   step green. The hub audits `github_actions_dispatch` with `result=accepted`
   and then `result=receipt`, each carrying `transport=oidc`, `repo`, `actor`,
   `run_id`, and `run_attempt`; refusals are `result=refused` with a `guard`
   field (`Server.auditActionDispatch` and its callers in
   `src/pkg/dashboard/actions_oidc.go`). The resulting kick's
   `agent_mention_kicked` detail carries `transport=oidc`.

Evidence checklist:

- [ ] Comment relay: workflow run URL, relay comment permalink, and 👀 ack
      screenshot or reaction URL.
- [ ] `agent_mention_kicked` audit/log line with `source=action`, `run_id`,
      `run_attempt`, `actor`, and `transport=comment`, plus the kick source
      `action:<repo>:<run_id>:<run_attempt>`.
- [ ] Rerun of the same attempt showing no second kick.
- [ ] OIDC: workflow run URL with the receipt assertion step green, the
      archived `receipt` output, and the `github_actions_dispatch`
      `result=accepted` / `result=receipt` audit lines.
- [ ] Dispatching actor and the Hive identity it resolved to (direct or via
      `identity_map`).
- [ ] Paste the template as a comment on #7563, tick the GitHub Actions trigger
      row in its **v6 readiness bar** section, and link the evidence comment
      from the row. Update both Actions rows in
      [v6-readiness.md](v6-readiness.md#3-live-exercise-per-surface) with the
      same link.

<a id="slack"></a>

## Slack Socket Mode

Prerequisites:

- Use the v6 Slack Socket Mode backend, not the unbuilt Events API accelerator.
  The Slack design says Socket Mode is the shipped, pull-only-friendly transport
  (`src/docs/design/slack-integration.md:3-12`,
  `src/docs/design/slack-integration.md:54-69`).
- Configure the `slack` notification block with `enabled: true`,
  `app_token: ${SLACK_APP_TOKEN}`, `bot_token: ${SLACK_BOT_TOKEN}`,
  `channel_id`, and `allowed_users` containing the maintainer's Slack user ID
  (`src/docs/design/slack-integration.md:104-117`). The backend refuses to
  start without app token, bot token, and channel ID
  (`Bot.Start` in `src/pkg/slack/bot.go`).
- Slack app scopes/events must cover Socket Mode and messages as documented:
  `chat:write`, channel history/manage as needed, `connections:write`, and
  `message.channels` (`src/docs/design/slack-integration.md:116-119`).

Run:

1. Restart or confirm the hive log contains `slack bot starting` and
   `chat service starting` (`Bot.Start` in `src/pkg/slack/bot.go`,
   `Service.Start` in `src/pkg/chat/chat.go`).
2. In the configured Slack channel, send `!status`. Save the Slack message link
   or screenshot and the bot reply.
3. Trigger one notification delivery by pausing/resuming an agent or inducing a
   safe working→idle transition after the first SSE snapshot. Save the Slack
   notification link or screenshot.
4. Save any relevant Socket Mode lines. Valid reconnect evidence includes
   `slack socket refreshed`, `slack socket disconnected`, `slack socket ack failed`, or Slack
   `disconnect` / `refresh_requested` handling (`slackBackend.Listen` and `slackBackend.serveSocket` in
   `src/pkg/slack/bot.go`).

Evidence checklist:

- [ ] `!status` command link/screenshot and bot reply.
- [ ] One notification delivery link/screenshot.
- [ ] Hive log line for `slack bot starting` or Socket Mode activity.
- [ ] Paste the template as a comment on #7563, tick the Slack row in its
      **v6 readiness bar** section, and link the evidence comment from the row.

## Discord

Prerequisites:

- Use a v6 hive with the shared `pkg/chat` spine and Discord port merged on the
  v6 line; the roadmap records Discord plus reconnect/cancellation parity as
  shipped v6 work (`src/docs/roadmap.md:55`).
- Configure the Discord bot token, channel ID, and the maintainer's Discord user
  ID in `allowed_users`; the Discord config structure uses `bot_token`,
  `channel_id`, and `allowed_users` (`src/pkg/config/config.go:4144-4154`).
  The backend refuses to start without the bot token (`Start`, `src/pkg/discord/bot.go:105-112`).

Run:

1. Confirm the hive log has `discord bot starting` and `chat service starting`
   (`Start` in `src/pkg/discord/bot.go`, `Service.Start` in `src/pkg/chat/chat.go`).
2. In the configured channel, send `!status`; save the Discord message link or
   screenshot and the bot reply.
3. Trigger one notification delivery by pausing/resuming an agent or waiting for
   a real agent state transition; save the notification link/screenshot.
4. Induce one safe disconnect and recovery observation. Prefer briefly
   interrupting the dashboard SSE connection, because the shared spine logs
   `discord SSE disconnected` and backs off before reconnecting
   (`sseLoop`, `src/pkg/chat/notify.go:70-106`). If you instead interrupt Discord REST,
   save the `discord poll failed` log line and the later successful command or
   notification proving recovery (`Listen`, `src/pkg/discord/bot.go:192-224`).

Evidence checklist:

- [ ] `!status` command link/screenshot and bot reply.
- [ ] Notification parity screenshot/link after recovery.
- [ ] `discord SSE disconnected` or `discord poll failed` log line plus
      timestamp of subsequent recovery.
- [ ] Paste the template as a comment on #7563, tick the Discord row in its
      **v6 readiness bar** section, and link the evidence comment from the row.

<a id="teams"></a>

## Microsoft Teams

Prerequisites:

- Use the Teams surface shipped on v6 alongside the shared chat spine
  (`src/docs/roadmap.md:55`).
- Configure `notifications.msteams.enabled`, `tenant_id`, `client_id`,
  `client_secret`, `team_id`, `channel_id`, `webhook_url`, and an
  `allowed_users` entry for the maintainer's Azure AD object ID. The config
  field names and fail-closed allowed-user contract are in `MSTeamsConfig`
  (`src/pkg/config/config.go:4127-4141`), and validation/startup require all
  Teams connection fields (`src/pkg/config/validate.go:107-125`,
  `src/pkg/msteams/bot.go:180-198`).
- `webhook_url` must be a Teams **Workflows** webhook URL: in the target
  channel, add the "Post to a channel when a webhook request is received"
  workflow and copy its URL. Hive posts Adaptive Card message envelopes to it.
  Office 365 Incoming Webhook connectors were retired by Microsoft in May 2026;
  connector URLs no longer work and must not be used.
- Inbound commands are read from top-level channel posts and from thread
  replies under the bot's most recent posts (for example checkpoint prompts).
  Replies under other users' posts are not observed; send commands there as a
  new top-level post.

Run:

1. Confirm `msteams bot starting` and `chat service starting` in hive logs.
2. In the configured Teams channel, send `!status`; save the Teams message
   link/screenshot and bot reply.
   Also reply `!status` in the thread under one of the bot's posts and confirm
   the bot answers.
3. Trigger one notification delivery via an agent pause/resume or state
   transition; save the Teams notification link/screenshot.
4. If anything fails, capture `msteams poll failed`, `msteams delta token
   expired`, or Graph rate-limit/backoff logs (`Backend.Listen` in
   `src/pkg/msteams/bot.go`).

Evidence checklist:

- [ ] Teams `!status` screenshot/link and reply.
- [ ] Teams notification screenshot/link.
- [ ] Hive log line with `msteams bot starting` or relevant poll evidence.
- [ ] Paste the template as a comment on #7563, tick the Teams portion of the
      shared row in its **v6 readiness bar** section, and link the evidence
      comment from the row.

## Matrix

Prerequisites:

- Use the Matrix surface shipped on v6 alongside the shared chat spine
  (`src/docs/roadmap.md:55`).
- Configure `notifications.matrix.enabled`, `homeserver_url`, `access_token`,
  `room_id`, and an `allowed_users` entry containing the maintainer's MXID. The
  config field names and allowed-user contract are in `MatrixConfig`
  (`src/pkg/config/config.go:4113-4124`), and startup requires homeserver URL,
  access token, and room ID (`src/pkg/matrix/bot.go:164-174`).

Run:

1. Confirm `matrix bot starting` and `chat service starting` in hive logs.
2. In the configured room, send `!status`; save the Matrix event link or
   screenshot and bot reply.
3. Trigger one notification delivery via an agent pause/resume or state
   transition; save the Matrix event link/screenshot.
4. If retry/backoff occurs, save `matrix sync failed` with `retry_after`;
   successful inbound messages are delivered from `m.room.message` events after
   `ioscan` (`src/pkg/matrix/bot.go:239-254`, `src/pkg/matrix/bot.go:267-277`).

Evidence checklist:

- [ ] Matrix `!status` event link/screenshot and reply.
- [ ] Matrix notification event link/screenshot.
- [ ] Hive log line with `matrix bot starting` or sync evidence.
- [ ] Paste the template as a comment on #7563, tick the Matrix portion of the
      shared row in its **v6 readiness bar** section, and link the evidence
      comment from the row.

## Telegram

Prerequisites:

- Use the Telegram surface shipped on v6 alongside the shared chat spine
  (`src/docs/roadmap.md:55`).
- Configure `notifications.telegram.enabled`, `bot_token`, `chat_id`, and
  `allowed_users` with the maintainer's Telegram numeric user ID. The config
  field names and fail-closed allowed-user contract are in `TelegramConfig`
  (`src/pkg/config/config.go:4100-4110`), and startup requires bot token and
  chat ID (`src/pkg/telegram/bot.go:124-129`).

Run:

1. Confirm `telegram bot starting` and `chat service starting` in hive logs.
2. In the configured chat, send `!status`; save the Telegram message link or
   screenshot and bot reply.
3. Trigger one notification delivery via an agent pause/resume or state
   transition; save the Telegram notification screenshot/link.
4. If retry/backoff occurs, save `telegram poll failed`; successful inbound
   messages are delivered after chat-ID filtering and `ioscan`
   (`src/pkg/telegram/bot.go:175-192`, `src/pkg/telegram/bot.go:212-218`).

Evidence checklist:

- [ ] Telegram `!status` screenshot/link and reply.
- [ ] Telegram notification screenshot/link.
- [ ] Hive log line with `telegram bot starting` or poll evidence.
- [ ] Paste the template as a comment on #7563, tick the Telegram portion of
      the shared row in its **v6 readiness bar** section, and link the evidence
      comment from the row.

## Dashboard chat

Prerequisites:

- The v6 dashboard chat transport described in
  [Dashboard chat](dashboard-chat.md): the panel posts to `POST /api/chat`,
  `pkg/dashchat` hands `!` commands to the shared spine, and the browser polls
  `GET /api/chat/messages?since=<seq>` for replies.
- The submitting session needs `read-write` dashboard access; below that,
  `POST /api/chat` answers `403 read-write access required` for `!` commands
  (`Server.handleChat` in `src/pkg/dashboard/api.go`).
- **Identity.** The spine runs a command only when the panel author is in the
  dashboard chat allowlist, which is `dashboard.authorized_users`
  (`dashboardChatAllowedUsers` in `src/cmd/hive/main.go`), replaced at runtime
  on a hub-managed spoke by the heartbeat's authorized-user list
  (`AuthorizedUsersCallback`, same file). The author is the request's
  `X-Hive-User`, or `local` when that header is empty (`requestUser` in
  `src/pkg/dashboard/audit.go`), and the lookup is an exact string match: no
  case folding, no `github:` prefix stripping (`parseAllowedUser` in
  `src/pkg/chat/chat.go`). Until
  [#9131](https://github.com/hivecommons/hive/issues/9131) lands, the panel
  author therefore has to match an allowlist entry byte-for-byte. In practice:
  sign in with a per-user dashboard session whose login matches an entry
  exactly. The shared dashboard token, and open or local-dev spokes, submit as
  `local`, and their commands are refused.
- `!status` needs only allowlist membership, not a particular role.

Run:

1. Confirm the hive log contains `dashboard chat started`. A
   `dashboard chat failed to start` line, or `POST /api/chat` returning
   `503 dashboard chat is not configured`, means the transport is not running.
2. Open the Hive Chat panel and send `!status`. The POST returns
   `{"accepted": true, "seq": N}` and the bot reply appears on the next poll.
   If the panel shows a `❌ … refused` line instead, the author did not match the
   allowlist (see Identity above). Fix the identity and retry; do not tick the
   row on a refusal.
3. Save the `GET /api/chat/messages?since=<N-1>` response containing the reply.
   The envelope's `viewer` is the identity the dashboard resolved for you.
4. Save the `chat.dashboard.message` audit entry carrying `seq=N`.

Evidence checklist:

- [ ] Panel screenshot showing the `!status` bubble and the bot reply.
- [ ] Poll envelope (redacted) showing the reply and `viewer`.
- [ ] `chat.dashboard.message` audit entry with the matching `seq`.
- [ ] Identity used (dashboard login) and the allowlist entry it matched
      exactly (redacted as needed), plus how the session authenticated
      (per-user session or hub proxy, not the shared token).
- [ ] Hive log line `dashboard chat started`.
- [ ] Paste the template as a comment on #7563, tick the Dashboard chat row in
      its **v6 readiness bar** section, and link the evidence comment from the
      row.

## Inception via chat

The row requires one greenfield inception to reach `complete` driven only from
chat: no Inception-tab form, no direct `/api/inception/*` calls to move it
forward. `just runs-e2e` does not cover this row: it runs
`go test -tags integration ./test -run Runs`, which selects the staged-run
acceptance tests (`TestRunsE2EAcceptance`, `TestRunsE2EV6Surfaces`). Those do
not start an inception, and `TestInceptionE2E` drives the REST API rather than
the chat spine. The exercise is manual.

Prerequisites:

- Any chat surface on the spine: the dashboard chat panel (see the identity
  prerequisite above) or an external backend from the sections above.
- The chat author must resolve to `owner` in that surface's allowlist (an
  `id:owner` entry, or the first bare entry). Every state-changing
  `!inception` subcommand refuses below owner with `❌ owner role required`
  (`requireCommandOwner` in `src/pkg/chat/inception.go`).
- The spine's own dashboard calls must pass the inception owner gate, which
  requires an owner `X-Hive-Role` and the owner-verification header
  (`requireOwnerRole` in `src/pkg/dashboard/api.go`). A
  `❌ Failed to start inception: … owner access required` reply means the spine's
  dashboard credential cannot do that on this hive. Record it as a blocker and
  do not tick the row.
- The `brainstorm` agent is enabled (see [Inception](inception.md#prerequisites)),
  and no inception is in progress. Only one run can be active; archive an
  earlier one with `!inception reset` first.

Run:

1. Send `!inception start <one-line greenfield idea>`. Expect
   `✅ Inception started.`
2. Wait for the spine to post the numbered clarification questions when the
   engine enters `clarify`. Answer each one with an ordinary unprefixed reply;
   each reply is submitted to the next unanswered question.
   `!inception answer <n> <text>` is the explicit form.
3. Wait for the brainstorm agent's fact beads. The inception watcher records
   them and advances `structure → scaffold`
   (`InceptionWatcher` in `src/pkg/dashboard/inception_watcher.go`). Use
   `!inception state` to watch the phase. If transcript-derived facts are
   listed as proposals, confirm them from chat, or record facts explicitly with
   `!inception facts <json-array-of-facts>`.
4. Once `!inception state` reports `scaffold`, send `!inception approve`.
   Expect `✅ Inception approved and brainstorm re-paused.`
5. Save `GET /api/inception/state` showing `phase: complete` as read-only
   evidence. Reading state does not drive the run, so it does not break the
   chat-only requirement.

Evidence checklist:

- [ ] Chat transcript (links or screenshots) from `!inception start` through
      the `approve` reply, including the posted question list and the answers.
- [ ] `!inception state` output at `scaffold`, and the final state showing
      `phase: complete` with the recorded fact count.
- [ ] Surface and identity used, and the allowlist entry that resolved to
      `owner`.
- [ ] Hive log/audit for the `/api/inception/*` calls made on the spine's
      behalf (start, answer, approve).
- [ ] A statement that no dashboard-form or direct API call drove any phase.
- [ ] Paste the template as a comment on #7563, tick the Inception via chat row
      in its **v6 readiness bar** section, and link the evidence comment from
      the row.

<a id="email"></a>

## Email escalation

Prerequisites:

- Use the v6 outbound email sink. The escalation design records SMTP email as
  shipped on v6 and explicitly says inbound reply-to-act is not built yet
  (`src/docs/design/escalation-surfaces.md:3-10`). Record that deferral in the
  evidence unless a later PR adds the inbound path.
- Configure `escalation.email.enabled`, `smtp.host`, optional `smtp.port`,
  `smtp.username`, `smtp.password`, `from`, and at least one `to` recipient;
  digest recipients are optional. The design shows the operator-facing block
  (`src/docs/design/escalation-surfaces.md:73-103`), while the config schema
  and validation require host/from/to when enabled
  (`src/pkg/config/config.go:6709-6727`, `src/pkg/config/validate.go:182-201`).

Run:

1. Trigger one real `HUMAN DECISION NEEDED` / `requires_human` escalation to an
   operator mailbox. Save the redacted email screenshot, Message-ID, recipient,
   subject, and timestamp.
2. Confirm the event severity is `decision` or `page`; those severities send
   immediate email (`src/pkg/escalate/types.go:8-20`,
   `src/pkg/escalate/email.go:56-68`).
3. Save SMTP/hive evidence. The sink scrubs and sends through SMTP in `send`,
   so logs should never include secrets (`src/pkg/escalate/email.go:138-153`).
4. For the inbound half, record one of these explicitly:
   - **Current v6:** reply-to-act deferred; no IMAP poller/signed reply parser is
     shipped (`src/docs/design/escalation-surfaces.md:106-127`).
   - **If a later v6 PR has shipped inbound:** send one allowlisted reply that
     acts, then paste the action/audit log and update this runbook in the same
     PR that ships the inbound path.

Evidence checklist:

- [ ] Redacted HUMAN DECISION NEEDED email screenshot or Message-ID.
- [ ] Hive log/audit around the escalation producer and email sink.
- [ ] Explicit inbound reply-to-act deferral (current v6) or allowlisted reply
      action evidence (future v6).
- [ ] Paste the template as a comment on #7563, tick the Email row in its
      **v6 readiness bar** section, and link the evidence comment from the row.

<a id="push-on-call"></a>

## Push / on-call

Prerequisites:

- Use the v6 push/on-call sinks. The escalation design records ntfy, Pushover,
  and PagerDuty as the shipped push providers (`src/docs/design/escalation-surfaces.md:130-155`).
- Configure `escalation.push.enabled`, `min_severity` (`decision` or `page`),
  and at least one provider: `ntfy.url` plus optional `token`, Pushover
  `app_token` and `user_key`, or PagerDuty `routing_key`. The config schema and
  validation enforce provider presence and the severity values
  (`src/pkg/config/config.go:6729-6749`, `src/pkg/config/validate.go:202-225`).

Run:

1. Configure `min_severity: decision` for the exercise if the readiness event is
   a `requires_human` / `decision` verdict; otherwise use a real `page` event.
2. Trigger one `requires_human` verdict that pages a real device through at
   least one provider. Save the redacted device screenshot, ntfy message,
   Pushover receipt, or PagerDuty incident/event link.
3. Save hive evidence showing the event entered the dispatcher and the provider
   sink delivered it. Dispatch queues events by severity and logs/audits
   `escalation_delivery_failed` once a delivery's bounded retries run out or
   the provider rejects it outright (`Dispatch` and `deliver`,
   `src/pkg/escalate/dispatcher.go:91-161`). Provider delivery paths are
   `ntfy`, `pushover`, and `pagerduty` (`src/pkg/escalate/push.go:45-63`,
   `src/pkg/escalate/push.go:89-117`, `src/pkg/escalate/push.go:126-147`).

Evidence checklist:

- [ ] Redacted provider/device evidence for the page.
- [ ] Hive log/audit line showing severity `decision`/`page` and provider name.
- [ ] If no page arrived, include any `escalation_delivery_failed` line and do
      not tick #7563's **v6 readiness bar** section until a retry succeeds.
- [ ] Paste the template as a comment on #7563, tick the Push / on-call row in
      its **v6 readiness bar** section, and link the evidence comment from the
      row.

<a id="standby"></a>

## Standby contributors (S5–S6)

The row is steps 1–4 of the end-to-end runbook in the standby design
([End to end on a real hive](design/standby-contributors.md#end-to-end-on-a-real-hive-runbook)),
run on a v6 edge hub with one contributor container. Its evidence also
unblocks S8 on [#7629](https://github.com/hivecommons/hive/issues/7629).

Prerequisites:

- A self-hosted v6 edge hub and one contributor container whose relay can
  declare standby.
- On the target lane: `standby.enabled: true`, `min_model_capability` left at
  the default `T1`, and `daily_cap_per_contributor` of at least `1`. `0`, the
  default, dispatches nothing. Under `hub:`, `standby_contributors` lists the
  contributor's GitHub login, and `standby_model_tiers` maps the contributor's
  backend/model/effort to `T2`, so step 2 fails to qualify
  ([Configuration schema](design/standby-contributors.md#configuration-schema)).
- An owner or read-write dashboard session for dispatch and clear
  (`POST /api/contribute/standby/dispatch`, `POST /api/contribute/standby/clear`).
  The chat equivalents are `!standby <lane> [owner/repo#number]` and
  `!standby-clear <contributor> <backend> <model> [effort]`, which need `owner`
  in the chat allowlist (`Service.cmdStandbyDispatch` and
  `Service.cmdStandbyClear` in `src/pkg/chat/dashboard.go`).

Run:

1. **(S1) Force the pause.** Spend the lane's budget. Confirm the tile shows
   the lane paused with reason `out of budget` and the queue depth.
2. **(S4) Approve and fail to qualify.** With the relay declaring standby on
   the T2 configuration, confirm the tile reads `0 qualify` (`/api/status`
   `governor.qualified_standbys` has `0` for the lane), the lane stays paused,
   and no surface proposes lowering the floor.
3. **(S5) Lower the floor deliberately and dispatch.** Set the lane's
   `min_model_capability: T2` in `hive.yaml`. Confirm `1 qualifies`. Dispatch
   one item by hand from the dashboard or with `!standby <lane> <owner/repo#n>`.
   Confirm the `contribute_standby_dispatch` audit entry (`lane`, `task`), and
   that the PR arrives from the **contributor's** account, carries the `hold`
   label, and has `standby_lane` and `standby_tier` in its `— hive:` trailer
   (`src/pkg/github/attribution.go`). Record the reviewer minutes spent on the
   PR; that number is the evidence for the design's open question 4.
4. **(S6) Suspend and clear.** Close two donated PRs unmerged in a row. Confirm
   the configuration is suspended: the tile says so, `/api/status`
   `governor.suspended_standbys` counts it, and the relay's next
   `standby_declare` is rejected with `suspended`. Clear it as owner from the
   dashboard or with `!standby-clear`, confirm the `contribute_standby_clear`
   audit entry, and confirm the configuration returns to standby after it
   re-declares.

Step 5 of the design runbook (cadence accounting) is not part of this row.
Record it in the same comment if you run it.

Evidence checklist:

- [ ] Tile screenshots for steps 1, 2 (`0 qualify`), 3 (`1 qualifies`), and 4
      (suspended, then cleared).
- [ ] `/api/status` excerpts showing `qualified_standbys` and
      `suspended_standbys` for the lane.
- [ ] Redacted `hive.yaml` diff lowering the floor to `T2`.
- [ ] Donated PR URL showing the contributor author, the `hold` label, and the
      `— hive:` trailer with `standby_lane` / `standby_tier`, plus the reviewer
      minutes.
- [ ] URLs of the two closed-unmerged donated PRs, the `suspended` rejection
      from the relay log, and the `contribute_standby_dispatch` /
      `contribute_standby_clear` audit entries.
- [ ] Paste the template as a comment on #7563, tick the Standby S5–S6 row in
      its **v6 readiness bar** section, link the evidence comment from the row,
      and cross-link it from #7629.

## Operator admin MCP

Prerequisites:

- The endpoint is `POST /api/admin/mcp` on the live hive (`EndpointPath` in
  `src/pkg/adminmcp/adminmcp.go`). Call it with
  `Authorization: Bearer <dashboard token>`. Confirmed writes re-authenticate
  with that token alone, so a caller admitted another way (per-user session,
  hub proxy proof, internal header), or any caller on a direct-route spoke,
  sees `writes_enabled: false` with a `writes_unavailable_reason`
  ([Write contract](design/admin-mcp.md#write-contract-phase-3)).
- Writes are off by default. Set `HIVE_ADMIN_MCP_ENABLE_WRITES=1` on the hive
  process for the endpoint write steps.
- For the stdio half, build `hive-admin-mcp` from `src/cmd/hive-admin-mcp` and
  set `HIVE_ADMIN_MCP_HIVES` to a roster naming the live hive plus at least one
  other entry, with `HIVE_ADMIN_MCP_ACTIVE` (or roster order) making the
  **other** entry active at start-up. That makes `select_hive` a real switch
  ([Configuring the stdio binary](design/admin-mcp.md#configuring-the-stdio-binary)).
  The roster carries dashboard tokens; never paste it into evidence.
- Pick a non-critical agent for the low-risk write. The runbook uses
  `agent.pause` followed by `agent.resume`.

Run:

1. **Endpoint `tools/list`.** `initialize`, then `tools/list`. Save the tool
   names, including `write_preview` / `write_confirm`, and `writes_enabled`.
2. **One read.** Call one read tool, for example `settings_read` or
   `hive_status`. Save the result as returned. Values under credential-named
   keys render as `[masked:<label>]` and credential-shaped strings as
   `[masked:secret-like]`. Point these out if present; do not seed a real
   secret to produce one.
3. **Write preview.** Call `write_preview` with
   `{"operation": "agent.pause", "args": {"agent": "<name>"}}`. Save the
   preview: the REST request (`POST /api/pause/<name>`), effects, widening
   disclosure (`No widening: this operation targets exactly one named agent.`),
   confirmation text (`Confirm pausing agent <name>.`), `confirmation_id`, and
   `expires_at`.
4. **Confirmed write.** Call `write_confirm` with that `confirmation_id`. Save
   the result, the dashboard showing the agent paused, and the dashboard audit
   entry for `pause`. That entry is attributed to `local` because confirmed
   writes send only the dashboard token
   ([Known gap: attribution](design/admin-mcp.md#known-gap-attribution)).
   Preview and confirm `agent.resume` the same way to restore the agent.
5. **stdio roster select and read.** Start `hive-admin-mcp` from an MCP client
   and call `select_hive` with the live hive's roster name. Save the
   `{"data": {"selected": true, "active_hive": …}}` result, then run one read
   tool and save its result.

Evidence checklist:

- [ ] Endpoint `tools/list` output (tool names and `writes_enabled`).
- [ ] One endpoint read result showing the wrapped, scrubbed envelope.
- [ ] `write_preview` output with the confirmation text, request, and widening
      disclosure.
- [ ] `write_confirm` result, plus the dashboard audit entry and API/dashboard
      outcome for the confirmed write (and the restoring `agent.resume`).
- [ ] stdio `select_hive` result naming the live hive, and one stdio read
      result.
- [ ] Redaction note confirming no roster token or dashboard token appears in
      the evidence.
- [ ] Paste the template as a comment on #7563, tick the Operator admin MCP row
      in its **v6 readiness bar** section, and link the evidence comment from
      the row.

## How to close

For each surface, post the filled evidence block and that section's checklist
as a comment on [#7563](https://github.com/hivecommons/hive/issues/7563), tick
the corresponding live-exercise row in its **v6 readiness bar** section with a
link to that comment, and, where the
[live-exercise table](v6-readiness.md#3-live-exercise-per-surface) has a
matching row, update it in a PR so the doc and the tracker stay in sync. There
is no per-surface issue to close.
Leave the shared Teams / Matrix / Telegram row unticked until all three blocks
are posted. A run that hits a blocker leaves its row unticked: post the partial
evidence and link the blocking issue instead.
