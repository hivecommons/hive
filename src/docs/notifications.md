# Notifications

Hive can send operator notifications through three outbound channels configured under the top-level `notifications:` block in `hive.yaml`:

- ntfy topics
- Slack incoming webhooks
- Discord webhooks

The same block also configures the two-way chat bots (Discord, Slack Socket Mode, Telegram, Matrix, Teams); see [Chat bots](#chat-bots-slack-socket-mode-telegram-matrix-teams) below.

The notifier is implemented in `src/pkg/notify/notify.go` and is constructed from `config.NotificationsConfig` in `src/pkg/config/notifications_config.go`. The same notification is sent to every configured channel.

Every notification title is prefixed with the sending hive's ID as `[<hive-id>] <title>` (`Notifier.SetHiveID` in `src/pkg/notify/notify.go`). Filters or routing rules that match on title - an ntfy topic shared by several hives, for example - should account for the prefix.

## Configuration

```yaml
notifications:
  ntfy:
    server: https://ntfy.sh
    topic: my-hive-alerts
  slack:
    webhook: ${SLACK_WEBHOOK_URL}
  discord:
    webhook: ${DISCORD_WEBHOOK_URL}
    factory_webhook: ${DISCORD_FACTORY_WEBHOOK_URL}
  github_activity:
    enabled: true
    # org defaults to project.org; keep explicit for hub/factory feeds.
    org: hivecommons
    # optional: defaults to 5 minutes
    poll_interval_s: 300
    # empty repos means all org repos; otherwise list repo names in this org.
    repos: []
    # empty events means the default full event set.
    events:
      - issue_opened
      - issue_closed
      - issue_reopened
      - issue_claimed
      - issue_released
      - pr_opened
      - pr_ready_for_review
      - pr_review
      - pr_ci_status
      - pr_merged
      - pr_closed
      - hourly_digest
    # default true; bot/dependabot noise is suppressed unless allow-listed.
    filter_bots: true
    filter_dependabot: true
    allow_authors: []
    deny_authors: []
```

All three channels are optional. Configure one channel or several. Webhook URLs are secrets; keep them in environment variables or Kubernetes Secrets rather than committing literal URLs.

### ntfy

| Field | Required | Notes |
|---|---:|---|
| `notifications.ntfy.server` | yes | ntfy base URL, for example `https://ntfy.sh` or your own HTTPS ntfy server. |
| `notifications.ntfy.topic` | yes | Topic name appended to the server URL. |

Hive posts the notification body to `<server>/<topic>` and sets `Title` and `Priority` headers. Priorities are `high`, `default`, or `low`.

Self-hosted ntfy works the same way as ntfy.sh as long as the Hive process can reach the HTTPS endpoint. On the public ntfy.sh server, choose a hard-to-guess topic because anyone who knows the topic can subscribe to it.

### Slack

| Field | Required | Notes |
|---|---:|---|
| `notifications.slack.webhook` | yes | Slack incoming webhook URL. The sender posts `{"text":"*title*\nmessage"}`. |

The notifier accepts `http://` and `https://` URLs in the Go sender, but production configs should use Slack's HTTPS webhook URLs and keep the value in an environment variable.

### Discord webhook

| Field | Required | Notes |
|---|---:|---|
| `notifications.discord.webhook` | yes for webhook notifications | Discord webhook URL. The sender posts `{"content":"**title**\nmessage","flags":4}` — `flags: 4` is Discord's `SUPPRESS_EMBEDS`, so URLs in a notification render as plain links without a preview card. |
| `notifications.discord.factory_webhook` | yes for `notifications.github_activity.enabled` | Dedicated Discord webhook URL for org-wide GitHub issue/PR activity. The sender posts one-line `{"content":"🐝 [repo] ... <url>","flags":4}` messages; the `<url>` wrapping and `SUPPRESS_EMBEDS` flag keep the channel free of link preview cards. |

Use a Discord **webhook URL**, not a bot token, for notification delivery.

### GitHub activity feed

`notifications.github_activity` enables the hub's org-wide issue/PR activity
poller. It is **off by default**. When enabled, hub mode enumerates repositories
from `orgs/<org>/repos`, diffs issue/PR state across poll cycles, and sends
concise activity lines to the dedicated
`notifications.discord.factory_webhook`. The state and sent-event dedupe keys
are persisted under the hive data directory so a hub restart seeds from disk
instead of replaying previously observed activity.

| Field | Required | Default | Notes |
|---|---:|---|---|
| `notifications.github_activity.enabled` | no | `false` | Turns the feed on. |
| `notifications.github_activity.org` | no | `project.org` | GitHub organization to enumerate with `orgs/<org>/repos`. |
| `notifications.github_activity.api_url` | no | configured GitHub API URL / `https://api.github.com` | Override for GitHub Enterprise or tests. |
| `notifications.github_activity.poll_interval_s` | no | `300` | Hub poll cadence. |
| `notifications.github_activity.repos` | no | empty | Empty enumerates every org repository; non-empty limits polling to those repo names. |
| `notifications.github_activity.events` | no | empty/full set | Selected factory-feed events: `issue_opened`, `issue_closed`, `issue_reopened`, `issue_claimed`, `issue_released`, `pr_opened`, `pr_ready_for_review`, `pr_review`, `pr_ci_status`, `pr_merged`, `pr_closed`, `hourly_digest`. |
| `notifications.github_activity.filter_bots` | no | `true` | Suppresses `*[bot]` authors unless allow-listed. Forward-merge PRs are still allowed to emit their single merged line. |
| `notifications.github_activity.filter_dependabot` | no | `true` | Suppresses Dependabot authors unless allow-listed. |
| `notifications.github_activity.allow_authors` | no | empty | Case-insensitive author logins that bypass the default bot filters. |
| `notifications.github_activity.deny_authors` | no | empty | Case-insensitive author logins that are always suppressed. |

Hub admins can edit the factory webhook, enablement, repo scope, event set, and bot filter in `/dashboard` under **Hub Admin — Notifications**. Saves persist to `hive.yaml` and hot-reload the poller by cancelling the old run and starting a new one against the same state file, so already-sent dedupe keys are not replayed.

Event keys are deduped as `(repo, number, event, sha-or-version)`. Forward-merge
PR chatter is collapsed so only the final merged message is posted:

```text
🐝 [hive] PR #8562 merged into v6 — 🌱 Forward-merge v5 into v6 (4 commits, hand-resolved) · @clubanderson
```

Current v1 event coverage:

| Area | Event | Notes |
|---|---|---|
| Issue | opened | New open issues observed after the initial seed. |
| Issue | closed completed / not planned | Uses GitHub `state_reason` when present. |
| Issue | reopened | Closed → open transition. |
| Issue | hive label added | `claimed`, `hive/*`, and `priority/*`. |
| Issue | assigned/claimed | Newly added assignee. |
| Issue | claim marker taken/released | Diffs `<!-- hive:claim ... -->` in issue body when present in API responses. |
| PR | opened | New non-draft open PRs observed after the initial seed. |
| PR | ready for review | Draft → ready transition. |
| PR | review requested | Newly requested reviewers from the PR list response. |
| PR | approved / changes requested | Latest submitted PR review decision. |
| PR | CI red/green transition | Terminal combined-status changes (`success`, `failure`, `error`) on the head SHA. |
| PR | merged | Includes the target branch (`merged into v6`) and merge/head SHA dedupe. |
| PR | closed unmerged | Open → closed without merge. |

## What triggers notifications

The Hive Go process sends notifications for these events:

| Event | Priority | Source |
|---|---|---|
| Weekly token budget crosses the warning threshold. | default | `applyBudgetAlerts` in `src/cmd/hive/main_eval_cycle.go` |
| Weekly token budget is exhausted and non-exempt kicks are suspended. | high | `applyBudgetAlerts` in `src/cmd/hive/main_eval_cycle.go` |
| Actionable issues exceed the SLA threshold; up to three `SLA 2x breach` notifications are sent per eval cycle for issues older than 60 minutes (`maxSLANotificationsPerCycle`, `doubleSLAMinutes`). | high | `selectSLABreachNotifications` in `src/cmd/hive/eval_cycle_seams.go`, sent from `runEvalCycle` in `src/cmd/hive/main_eval_cycle.go` |
| A running agent pane matches a configured login-required pattern; Hive pauses that agent and sends the backend-specific login instruction (`Login required: <agent>`). | high | `scanForLoginRequired` in `src/cmd/hive/main.go` |
| Trajectory review detects divergence and either pauses the agent or flags the divergence. | high | `src/pkg/dashboard/trajectory_sink.go` and `src/pkg/trajectory/lane.go` |
| The inference provider starts rebuffing kicks with spending-limit errors; sent once per crossing of the latch, not per cycle (`Provider spending limit reached`). | high | provider-budget kick gate in `runEvalCycle`, `src/cmd/hive/main_eval_cycle.go` |
| A probe kick succeeds after a spending-limit clip and agent kicks resume (`Provider spending limit lifted`). | default | provider-budget recovery path in `runEvalCycle`, `src/cmd/hive/main_eval_cycle.go` |
| A PR stays red after exhausting its automated fix-attempt budget and is escalated to a human (`Fix loop escalated`). | high | `runEscalationSweep` in `src/cmd/hive/main_sweeps.go` |
| The planning stall-replan lane re-kicks the architect on a stalled plan (`Plan replan`) or hits the replan cap (`Plan replan-cap`). | high | `src/pkg/planning/replan.go` via `src/pkg/dashboard/replan_sink.go` |
| The convergence rollout mode changes at runtime (settings PUT, YAML reload, or env override); sent once per transition, not per cycle (`Convergence mode changed`). | default | `applyConvergenceKickAdmission` in `src/cmd/hive/convergence_kick.go` |

The dashboard **Settings → Notifications** tab can set ntfy, Discord, and Slack webhooks and choose which hook transitions should notify. The selected event set is persisted as `notifications.events`; the dashboard also manages matching `notify` hooks for `sweep_completed`, `escalation_red`, `stage_completed`, `issue_claimed`, and `issue_released` so those transitions use the same fanout.

Operator-defined [hooks](hooks.md) with the `notify` action send through the same fanout: title, message, and priority come from the hook definition (`ActionNotify` in `src/pkg/hooks/action.go`, bridged by `notifierAdapter` in `src/cmd/hive/hookwire.go`). An unknown priority falls back to `default`, so any transition a hook can observe can also page these channels.

The legacy shell scripts in `bin/` also use `bin/notify.sh` for events such as stale agents, rate limits, backend switches, and kick status when those scripts are deployed. Those scripts read environment variables (`NTFY_TOPIC`, `NTFY_SERVER`, `SLACK_WEBHOOK`, `DISCORD_WEBHOOK`) rather than the `notifications:` YAML block.

## Testing delivery

Test the target channel directly before putting it in `hive.yaml`:

```bash
# ntfy
curl -s -H 'Title: Hive test' -H 'Priority: default' \
  -d 'notification test from hive operator' \
  https://ntfy.sh/my-hive-alerts

# Slack incoming webhook
curl -s -X POST -H 'Content-type: application/json' \
  --data '{"text":"*Hive test*\nnotification test from hive operator"}' \
  "$SLACK_WEBHOOK_URL"

# Discord webhook
curl -s -X POST -H 'Content-type: application/json' \
  --data '{"content":"**Hive test**\nnotification test from hive operator"}' \
  "$DISCORD_WEBHOOK_URL"
```

After editing `hive.yaml`, restart or reload Hive through your normal deployment path. Dashboard saves for hive notifications persist ntfy, Discord, Slack, and event selections; hub factory-feed saves hot-reload without a hub restart.

## Discord webhook vs Discord bot

Webhook notifications are the `notifications.discord.webhook` field above.

The Discord bot is part of the Hive Go process (`src/pkg/discord`), started when both `notifications.discord.bot_token` and `notifications.discord.channel_id` are set. It is not required for webhook notifications, and a bot token cannot replace `notifications.discord.webhook`; those fields start the bot integration and do not by themselves send ordinary webhook notifications unless `notifications.discord.webhook` is also configured.

> A legacy standalone Node.js bot previously lived under `discord/` and used
> `DISCORD_BOT_TOKEN`/`DISCORD_CHANNEL_PRIMARY`. It was removed on `v6` in
> hivecommons/hive#9140 (it had been crash-looping on a `SyntaxError` since May
> and lacked the command-allowlist/ioscan/scrub guards the Go bot enforces).
> Migrate any host still running it: `systemctl disable --now
> hive-discord.service`, then set `notifications.discord.bot_token`,
> `notifications.discord.channel_id`, and `notifications.discord.allowed_users`
> in `hive.yaml` instead of `DISCORD_BOT_TOKEN`/`DISCORD_CHANNEL_PRIMARY`.

### Bot prerequisites (hivecommons/hive#9141)

The Go bot reads messages over Discord's REST API
(`GET /channels/{id}/messages`), but Discord's Message Object contract applies
to REST reads the same as gateway events: an app receives **empty**
`content` (and `embeds`/`attachments`/`components`) unless it has been granted
the **MESSAGE_CONTENT privileged intent**. Without it, the bot logs `discord
bot starting`, posts "bot online", and polls successfully, but every command
is silently ignored because `msg.Content` is always `""`.

Before enabling the bot, in the [Discord Developer Portal](https://discord.com/developers/applications):

1. Select your application → **Bot** → **Privileged Gateway Intents** → enable
   **Message Content Intent**.
2. Grant the bot, in the target channel, at least: **View Channel**, **Read
   Message History**, and **Send Messages** (plus **Manage Channels** if you
   want `SetTopic`/dashboard-status topic updates to work).

If commands still appear ignored after enabling the intent, check the hive
log for `discord poll failed` — that indicates a permissions problem
(e.g. missing Read Message History), not a missing intent, since a missing
intent produces no error at all, only empty content.

## Chat bots: Slack Socket Mode, Telegram, Matrix, Teams

Besides the one-way webhooks above, v6 ships two-way chat bots on the shared
chat spine (`src/pkg/chat`). Each bot receives the same notifications as the
webhooks (relayed from the dashboard SSE stream) and accepts `!` commands such
as `!status` from allowlisted users. All of them live under the same
`notifications:` block; the struct is `NotificationsConfig` in
`src/pkg/config/notifications_config.go`.

Every bot block below shares the same two rules:

- `enabled: true` starts the bot (Discord is the exception: it starts when
  `bot_token` and `channel_id` are set, see above). Startup fails closed with
  `<surface> <field>, <field> ... must be configured` if a required field is
  empty.
- `allowed_users` is the command allowlist. Entries are `id` or `id:role`
  (`read`, `read-write`, `merger`, `owner`; see
  [operator-reference](operator-reference.md)). An **empty list disables
  commands** for that surface: the bot still posts notifications but ignores
  every `!` command. Use `${ENV_VAR}` interpolation for tokens.

```yaml
notifications:
  slack:
    enabled: true
    app_token: ${SLACK_APP_TOKEN}      # xapp-… (Socket Mode)
    bot_token: ${SLACK_BOT_TOKEN}      # xoxb-…
    channel_id: C0123456789
    allowed_users: ["U0123456789:owner"]
  telegram:
    enabled: true
    bot_token: ${TELEGRAM_BOT_TOKEN}   # from @BotFather
    chat_id: "-1001234567890"
    allowed_users: ["123456789:owner"] # numeric Telegram user id
  matrix:
    enabled: true
    homeserver_url: https://matrix.org
    access_token: ${MATRIX_ACCESS_TOKEN}
    room_id: "!abcdef:matrix.org"
    allowed_users: ["@you:matrix.org:owner"]
  msteams:
    enabled: true
    tenant_id: ${MSTEAMS_TENANT_ID}
    client_id: ${MSTEAMS_CLIENT_ID}
    client_secret: ${MSTEAMS_CLIENT_SECRET}
    team_id: <team id>
    channel_id: <channel id>
    webhook_url: ${MSTEAMS_WEBHOOK_URL}  # Teams Workflows (Power Automate) flow URL
    allowed_users: ["<aad object id>:owner"]
```

| Surface | Required fields | Provider-side setup | Startup log line |
|---|---|---|---|
| Slack (`notifications.slack`) | `enabled`, `app_token`, `bot_token`, `channel_id` | Slack app with **Socket Mode** on; bot scopes `chat:write`, `connections:write`, `channels:history` (plus `channels:manage` if you want topic updates); event subscription `message.channels`; install to the workspace and invite the bot to `channel_id`. Socket Mode is outbound-only, so it works from pull-only clusters without ingress. Design: [slack-integration](design/slack-integration.md). | `slack bot starting` |
| Telegram (`notifications.telegram`) | `enabled`, `bot_token`, `chat_id` | Create the bot with @BotFather. Send it one message, then read `chat.id` (and your `from.id` for `allowed_users`) from `https://api.telegram.org/bot<token>/getUpdates`. Messages from any other chat are ignored. | `telegram bot starting` |
| Matrix (`notifications.matrix`) | `enabled`, `homeserver_url`, `access_token`, `room_id` | A dedicated bot account on any homeserver; log it in once to obtain `access_token`; create the room and invite the bot. `allowed_users` entries are full MXIDs. | `matrix bot starting` |
| Teams (`notifications.msteams`) | `enabled`, `tenant_id`, `client_id`, `client_secret`, `team_id`, `channel_id`, `webhook_url` | An Entra app registration (client credentials) that can read channel messages, plus a Teams Workflows "post to a channel when a webhook request is received" flow for `webhook_url`. Legacy Office 365 connector URLs are not supported. `allowed_users` entries are Entra (AAD) user object IDs. Inbound replies are only read under the five newest bot-authored posts. | `msteams bot starting` |

Discord's two-way bot is configured by `notifications.discord.bot_token`,
`channel_id` and `allowed_users`; see
[Discord webhook vs Discord bot](#discord-webhook-vs-discord-bot).

To verify a bot end to end: watch the hive log for the startup line and
`chat service starting`, send `!status` in the configured channel, and expect
a reply. A bot that starts but never answers usually has an empty
`allowed_users` (commands disabled) or, for Discord, a missing Message Content
Intent.

## RFC lifecycle notices (Discord bot)

Set `notifications.discord.rfc_channel_id` (or the env var `HIVE_DISCORD_RFC_CHANNEL_ID`, which overrides it) together with `notifications.discord.bot_token` and the hive posts one line to that channel when an RFC issue in a managed repo:

- is opened: `RFC opened: #N <title> - <url> (by @author)`
- is closed: `RFC closed (completed): ...` or `RFC closed (not planned): ...`, from GitHub's close reason
- is taken off hold so it can be implemented: `RFC ready to implement (hold removed): #N <title> - <url>`, when the `hold` label (Prow `/unhold`) comes off an open RFC, or a held RFC moves to `stage/010-actionable`. One notice per hold episode.

An issue is an RFC when it carries the `rfc` label or its title starts with `RFC` after any leading emoji or `[tag]` (`RFC: ...`, `[RFC] ...`, `📝 RFC: [v6] ...`). A hive that manages several repos prefixes the number with the repo (`owner/repo#N`).

The default is empty, which turns the notices off. `channel_id` is not required: the notices only post with the bot token and never start the command bot. The hive polls GitHub every three minutes for issues updated since its last poll (one list call per repo) and keeps per-issue records in `/data/rfc-notify.json`, so a restart never re-announces a transition. The first poll after the key is set only records the current open RFCs; transitions announce from the next poll on. Each post is also written to the audit log as `rfc-notify`.

```yaml
notifications:
  discord:
    bot_token: ${DISCORD_BOT_TOKEN}
    rfc_channel_id: "1540404487099777136"
```
