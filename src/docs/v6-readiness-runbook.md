# v6 readiness live-exercise runbook

Use this page to finish the maintainer half of the v6 readiness issues after
the conformance suites have already proved the shared guard invariant. Each
exercise needs one live hive on the `v6` branch, a real operator account, and
links or screenshots that can be pasted into the surface issue and the live row
in [#7563](https://github.com/hivecommons/hive/issues/7563). Do not close a row
from unit tests alone.

Common setup for chat surfaces:

- Use a maintainer-controlled v6 hive with dashboard auth available to the chat
  spine. Chat commands call dashboard endpoints such as `/api/status` and
  `/api/kick/<agent>` (`src/pkg/chat/dashboard.go:15-19`,
  `src/pkg/chat/dashboard.go:127-143`), and notification delivery comes from the
  `/api/events` SSE stream (`src/pkg/chat/notify.go:85-124`).
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
  emitted from SSE snapshots (`src/pkg/chat/notify.go:150-212`).
- The command round-trip can be `!status` because it reads `/api/status` and
  returns a status message (`src/pkg/chat/dashboard.go:15-58`); `!kick <agent>
  readiness smoke` is also acceptable when the agent can safely be kicked.

## Evidence template

Paste one filled block into the surface issue and link the same comment from
[#7563](https://github.com/hivecommons/hive/issues/7563):

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
- [ ] #7563 **v6 readiness bar** row updated: `<link to tracker comment or checklist edit>`

Notes / redactions: `<what was redacted and why>`
```

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
- [ ] Paste the template, tick the GitHub @-mention live row in #7563's **v6 readiness bar** section, and
      link the evidence comment.

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
   `slack socket disconnected`, `slack socket ack failed`, or Slack
   `disconnect` / `refresh_requested` handling (`slackBackend.Listen` and `slackBackend.consumeSocket` in
   `src/pkg/slack/bot.go`).

Evidence checklist:

- [ ] `!status` command link/screenshot and bot reply.
- [ ] One notification delivery link/screenshot.
- [ ] Hive log line for `slack bot starting` or Socket Mode activity.
- [ ] Paste the template, tick the Slack live row in #7563's **v6 readiness bar** section, and
      link the evidence comment.

## Discord

Prerequisites:

- Use a v6 hive with the shared `pkg/chat` spine and Discord port merged on the
  v6 line; the roadmap records Discord plus reconnect/cancellation parity as
  shipped v6 work (`src/docs/roadmap.md:55`).
- Configure the Discord bot token, channel ID, and the maintainer's Discord user
  ID in `allowed_users`; the Discord config structure uses `bot_token`,
  `channel_id`, and `allowed_users` (`src/pkg/config/config.go:4144-4154`).
  The backend refuses to start without the bot token (`Start`, `src/pkg/discord/bot.go:101-108`).

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
   (`src/pkg/chat/notify.go:46-81`). If you instead interrupt Discord REST,
   save the `discord poll failed` log line and the later successful command or
   notification proving recovery (`Listen`, `src/pkg/discord/bot.go:188-220`).

Evidence checklist:

- [ ] `!status` command link/screenshot and bot reply.
- [ ] Notification parity screenshot/link after recovery.
- [ ] `discord SSE disconnected` or `discord poll failed` log line plus
      timestamp of subsequent recovery.
- [ ] Paste the template, tick the Discord live row in #7563's **v6 readiness bar** section, and
      link the evidence comment.

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
  `src/pkg/msteams/bot.go:177-195`).
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
- [ ] Paste the template, tick the Teams portion of the shared row in #7563's **v6 readiness bar** section,
      and link the evidence comment.

## Matrix

Prerequisites:

- Use the Matrix surface shipped on v6 alongside the shared chat spine
  (`src/docs/roadmap.md:55`).
- Configure `notifications.matrix.enabled`, `homeserver_url`, `access_token`,
  `room_id`, and an `allowed_users` entry containing the maintainer's MXID. The
  config field names and allowed-user contract are in `MatrixConfig`
  (`src/pkg/config/config.go:4113-4124`), and startup requires homeserver URL,
  access token, and room ID (`src/pkg/matrix/bot.go:160-170`).

Run:

1. Confirm `matrix bot starting` and `chat service starting` in hive logs.
2. In the configured room, send `!status`; save the Matrix event link or
   screenshot and bot reply.
3. Trigger one notification delivery via an agent pause/resume or state
   transition; save the Matrix event link/screenshot.
4. If retry/backoff occurs, save `matrix sync failed` with `retry_after`;
   successful inbound messages are delivered from `m.room.message` events after
   `ioscan` (`src/pkg/matrix/bot.go:235-250`, `src/pkg/matrix/bot.go:263-273`).

Evidence checklist:

- [ ] Matrix `!status` event link/screenshot and reply.
- [ ] Matrix notification event link/screenshot.
- [ ] Hive log line with `matrix bot starting` or sync evidence.
- [ ] Paste the template, tick the Matrix portion of the shared row in #7563's **v6 readiness bar** section,
      and link the evidence comment.

## Telegram

Prerequisites:

- Use the Telegram surface shipped on v6 alongside the shared chat spine
  (`src/docs/roadmap.md:55`).
- Configure `notifications.telegram.enabled`, `bot_token`, `chat_id`, and
  `allowed_users` with the maintainer's Telegram numeric user ID. The config
  field names and fail-closed allowed-user contract are in `TelegramConfig`
  (`src/pkg/config/config.go:4100-4110`), and startup requires bot token and
  chat ID (`src/pkg/telegram/bot.go:120-125`).

Run:

1. Confirm `telegram bot starting` and `chat service starting` in hive logs.
2. In the configured chat, send `!status`; save the Telegram message link or
   screenshot and bot reply.
3. Trigger one notification delivery via an agent pause/resume or state
   transition; save the Telegram notification screenshot/link.
4. If retry/backoff occurs, save `telegram poll failed`; successful inbound
   messages are delivered after chat-ID filtering and `ioscan`
   (`src/pkg/telegram/bot.go:171-188`, `src/pkg/telegram/bot.go:208-214`).

Evidence checklist:

- [ ] Telegram `!status` screenshot/link and reply.
- [ ] Telegram notification screenshot/link.
- [ ] Hive log line with `telegram bot starting` or poll evidence.
- [ ] Paste the template, tick the Telegram portion of the shared row in #7563's **v6 readiness bar** section,
      and link the evidence comment.

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
- [ ] Paste the template, tick/link the Email row in #7563's **v6 readiness bar** section, and
      link the evidence comment.

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
   `escalation_delivery_failed` on repeated sink failure
   (`src/pkg/escalate/dispatcher.go:78-123`). Provider delivery paths are
   `ntfy`, `pushover`, and `pagerduty` (`src/pkg/escalate/push.go:22-38`,
   `src/pkg/escalate/push.go:54-72`, `src/pkg/escalate/push.go:81-102`).

Evidence checklist:

- [ ] Redacted provider/device evidence for the page.
- [ ] Hive log/audit line showing severity `decision`/`page` and provider name.
- [ ] If no page arrived, include any `escalation_delivery_failed` line and do
      not tick #7563's **v6 readiness bar** section until a retry succeeds.
- [ ] Paste the template, tick/link the Push / on-call row in #7563's **v6 readiness bar** section, and
      link the evidence comment.

## GitHub Actions trigger

Prerequisites:

- Enable the shipped Actions trigger described in
  [GitHub Actions trigger](design/github-actions-trigger.md), which records
  `source=action` kicks, 👀 ack, and audits on v6
  (`src/docs/design/github-actions-trigger.md`).
- Configure `github.actions.enabled`, the managed repo list, and a
  `github.actions.default_agent` or an unambiguous action-capable agent. The
  config fields include `enabled`, `max_concurrent`, and the agent selection
  (`src/pkg/config/config.go`). The workflow file `hive-action-smoke` must be
  available in the target repository with `workflow_dispatch` trigger.
- The selected agent must be enabled, hold `Converse`, and be governor-kickable;
  the handler declines otherwise.

Run:

1. As a repository contributor with Actions permissions, dispatch the
   `hive-action-smoke` workflow manually against the v6 branch via the GitHub
   UI. Save the Actions run link.
2. Wait for the workflow to handle the dispatch. Save the hive log around the
   handling window and confirm the kick was recorded with `source=action`.
3. Confirm the audit event is `agent_action_kicked`; declined attempts use
   `agent_action_declined`.
4. If the run completes, link the run log or dashboard completion evidence.

Evidence checklist:

- [ ] GitHub Actions workflow_dispatch URL/link.
- [ ] Audit/log line containing `agent_action_kicked` with repository, workflow
      run ID, and agent.
- [ ] Kick/run evidence showing `source=action`.
- [ ] Paste the template, tick the GitHub Actions trigger live row in #7563's **v6 readiness bar** section, and
      link the evidence comment.

## Dashboard chat

Prerequisites:

- Use a v6 hive with dashboard auth available to the chat spine. Dashboard
  commands call the same chat backbone used by Slack, Discord, Teams, Matrix,
  and Telegram (`src/pkg/dashchat`, `src/pkg/chat/dashboard.go`).
- Pick an allowlisted human ID for the dashboard. The shared command router
  fails closed when `allowed_users` is empty. The panel author's dashboard user
  ID must match the chat allowlist exactly; the browser renders a 👀 ack and
  refusal line if not allowlisted (`src/pkg/dashboard/chat_commands.go`,
  `src/pkg/dashboard/chat_conformance_v6_test.go`).
- `allowed_users` entries may be `id` or `id:role`; roles are `read`,
  `read-write`, `merger`, and `owner`. The first bare entry is treated as
  `owner`, and later bare entries are treated as `read`.
- For the notification half, create a harmless visible state transition after
  the bot has started, such as pausing and resuming a non-critical agent from
  the dashboard. Valid notifications are the rendered `Working`, `Completed`,
  `Paused`, `Resumed`, `Off (cadence rule)`, or governor-mode-change messages
  emitted from SSE snapshots (`src/pkg/chat/notify.go:150-212`).

Run:

1. Confirm the hive log contains `dashchat service starting` or `chat service
   starting` (`src/pkg/dashchat`, `Service.Start` in `src/pkg/chat/chat.go`).
2. In the dashboard chat panel, send `!status`. Save the panel screenshot and
   the bot reply.
3. Trigger one notification delivery by pausing/resuming an agent or inducing a
   safe working→idle transition after the first SSE snapshot. Save the
   notification screenshot from the panel.
4. If refusals occur, confirm the 👀 ack and refusal line appear in the panel
   for non-allowlisted users (`src/pkg/dashboard/chat_conformance_v6_test.go`).

Evidence checklist:

- [ ] Dashboard chat panel `!status` screenshot and bot reply.
- [ ] One notification delivery screenshot from the panel.
- [ ] Hive log line for `dashchat service starting` or `chat service starting`.
- [ ] Paste the template, tick the Dashboard chat live row in #7563's **v6 readiness bar** section, and
      link the evidence comment.

## Inception via chat

Prerequisites:

- Use a v6 hive with the chat spine enabled. Chat commands are routed to the
  orchestrator, which can start a new run from the `!inception` command or
  inbound Slack/Discord/Teams/Matrix/Telegram thread.
- Configure `orchestrator.enabled: true` and the `inception` agent with
  `Converse` capability. The inception agent must be allowlisted in the chat
  `allowed_users` (`src/pkg/config/config.go`, `src/pkg/orchestrator`).
- The chat allowlist and the inception agent must be configured so that at
  least one human can execute `!inception <run-config>`.

Run:

1. From an allowlisted user account in the chat surface (dashboard, Slack, or
   other), send `!inception <run-config>` or trigger an inception via the chat
   surface. Save the chat message link/screenshot.
2. Confirm the inception agent receives the message and starts a greenfield run.
   Save the hive log showing the run initialization and the agent receiving the
   command.
3. Allow the run to proceed through all stages until it reaches `complete`.
   Save the run completion evidence and the chat notification showing the run
   finished.
4. Confirm the entire flow (message → agent → run completion → notification) is
   recorded in hive audit logs.

Evidence checklist:

- [ ] Inception command chat message link/screenshot.
- [ ] Hive log showing `agent_inception_kicked` or similar inception trigger
      with run key and agent.
- [ ] Run completion log or dashboard evidence showing `complete` status.
- [ ] Chat notification screenshot showing run completion alert.
- [ ] Paste the template, tick the Inception via chat live row in #7563's **v6 readiness bar** section, and
      link the evidence comment.

## Operator admin MCP

Prerequisites:

- Deploy the v6 admin MCP endpoint behind the dashboard. The endpoint is
  mounted at `/api/admin/mcp/call` and requires dashboard authentication
  (`src/pkg/dashboard`, `src/cmd/hive-admin-mcp`, `design/admin-mcp.md`).
- Alternatively, build and run the stdio admin MCP binary (`hive-admin-mcp`)
  pointing at a live hive. The stdio transport maintains a roster of hives and
  switches between them on demand.
- Configure at least one hive available to both transports; the endpoint
  administers only the hive serving it, while stdio may maintain a roster but
  operates on exactly one active hive at a time (`design/admin-mcp.md`).

Run:

1. **Endpoint transport:**
   - Use an MCP client (e.g. Claude desktop with MCP integration) to connect to
     `http://<hive>:<port>/api/admin/mcp/call` with dashboard authentication.
   - Invoke `tools/list` and verify the tool inventory is returned.
   - Invoke one read tool (e.g. `agent_status`) and save the response.
   - Invoke one write tool preview (e.g. `set_agent_mode`) and confirm the
     preview shows the proposed change and confirmation text.
   - Send the confirmation token and invoke the confirmed write. Verify the
     write executed and the hive log shows the action.
   - Save the MCP transcript showing all calls and responses.

2. **Stdio transport:**
   - Run `hive-admin-mcp` and use the roster prompt to select a hive.
   - Invoke `tools/list` and verify the tool inventory is returned.
   - Invoke one read tool and save the response.
   - Verify the hive selection is persistent across commands and the stdio
     roster works correctly.

3. **Shared evidence:**
   - Verify all tool responses are scrubbed (secrets redacted, sensitive paths
     masked) before being sent to the operator.
   - Confirm dashboard audit logs show the MCP operations with the operator's
     identity and action details.
   - Verify the confirmation text is verbatim and includes a clear statement of
     the change being made.

Evidence checklist:

- [ ] Endpoint `tools/list` response screenshot or transcript.
- [ ] Endpoint read tool response showing successful read with scrubbing applied.
- [ ] Endpoint write preview showing confirmation text and proposed change.
- [ ] Endpoint confirmed write execution log and dashboard audit entry.
- [ ] Stdio `tools/list` response and hive roster selection evidence.
- [ ] Stdio read operation response with scrubbing verification.
- [ ] Hive/audit evidence showing MCP operations recorded with operator identity.
- [ ] Paste the template, tick the Operator admin MCP live row in #7563's **v6 readiness bar** section, and
      link the evidence comment.

## Standby S5–S6

Prerequisites:

- Use a v6 hive with standby contributors enabled. Standby is the mechanism by
  which a paused lane offers its queue to approved standby contributors behind a
  per-lane capability floor, implemented according to RFC #7629
  (`docs/rfc-7629-standby-contributors.md`, `design/standby-contributors.md`).
- Configure `lanes.<lane-id>.standby_enabled: true` and
  `lanes.<lane-id>.min_model_capability` to a tier that at least one standby
  contributor meets (S1–S4 configuration; S5–S6 are live-exercise phases).
- Enroll standby contributors in the hive's contributor list with matching
  capability metadata. Record the standby protocol flow in
  `src/docs/design/standby-contributors.md` phases S1–S4.

Run:

1. Pause a configured lane that has queued items and at least one enrolled
   standby contributor who meets the capability floor. Save the lane state
   screenshot.
2. Confirm the hive offers the paused queue to standby via the internal state
   machine (not a user-visible surface yet). Save relevant log lines showing
   standby evaluation.
3. Confirm at least one standby contributor receives the offer and their work
   is assigned from the paused queue. Save assignment log entries.
4. Monitor one assigned run to completion. Save run completion logs and confirm
   the work was executed by a standby contributor (audit/capability checks in
   logs).
5. Resume the lane and confirm standby offers are revoked when the lane is no
   longer paused.

Evidence checklist:

- [ ] Paused lane screenshot or state showing queued work.
- [ ] Hive log lines showing standby offer evaluation and matching (S5 phase).
- [ ] Standby contributor assignment log with capability matching evidence (S6
      phase).
- [ ] Completed run audit showing the standby contributor's work and capability
      verification.
- [ ] Lane resume log showing standby offers revoked.
- [ ] Paste the template, tick the Standby S5–S6 live row in #7563's **v6 readiness bar** section, and
      link the evidence comment.

## How to close

For each issue, paste the filled evidence block, update the corresponding live
exercise row in [#7563](https://github.com/hivecommons/hive/issues/7563) with a
link to that evidence, and then close only that surface issue. The shared
Teams / Matrix / Telegram row should not be fully checked until all three
surface blocks are present.
