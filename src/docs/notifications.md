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
| `notifications.discord.webhook` | yes for webhook notifications | Discord webhook URL. The sender posts `{"content":"**title**\nmessage","flags":4,"allowed_mentions":{"parse":[]}}` — `flags: 4` is Discord's `SUPPRESS_EMBEDS`, so URLs in a notification render as plain links without a preview card, and the empty `allowed_mentions.parse` stops untrusted text (issue titles, repo names) from pinging `@everyone`, `@here`, roles or users. |
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

### Per-recipient attention budget (opt-in, off by default)

Per-condition reminder limits do not add up to an attention budget: ten
conditions on ten items each reminding daily is still ten pings a day for one
person. `pkg/attentionbudget` caps **non-urgent** attention writes (first
pings and reminders) per responsible recipient per rolling 24 hours, across
items, and never drops an ask that goes over the cap
([#16271](https://github.com/hivecommons/hive/issues/16271), part of
[#16075](https://github.com/hivecommons/hive/issues/16075)).

| Field | Default | Notes |
|---|---|---|
| `github.attention.recipient_budget` | `0` (off) | Max non-urgent attention writes per recipient per rolling 24h. `0` keeps today's behaviour; maximum `1000`. |
| `github.attention.digest_recipients` | empty | GitHub logins (case-insensitive, optional `@`) that opted into the digest. |

These keys live in the `github.attention` block owned by the condition-episode
ledger ([#16267](https://github.com/hivecommons/hive/issues/16267)), which
admits every non-urgent write through the budget: the ledger's first ping for
a newly actionable episode (`Store.Report`) and every attention-pilot reminder
(`SweepAttentionQueue`). The budget is seeded from the ledger's delivered
history (`paged_to`/`paged_at` and the last reminder) when the ledger opens, so
the rolling window survives restarts. An over-budget ask is never dropped: its
episode stays open and would-notify, is marked `overflowed` in the ledger,
stays in the repo's attention queue issue, and is posted on a later pass once
the recipient's budget frees up (or cleared when it is acknowledged, snoozed
or resolved). Producers mark exempt asks with `Observation.Class`
(`security`, `required_review`, `protocol_command`, `hold`); an unset class is
an ordinary non-urgent ping.

- **Off (shadow mode).** With `recipient_budget: 0` every write is delivered
  exactly as before. The budget still counts, per recipient, how many writes
  would have overflowed a candidate cap of 5 (`ShadowBudget`) and reports
  `delivered`, `exempt`, `overflowed` and `would_overflow` per login in the
  `attention budget:` log line (emitted on every attention-queue sweep), so an
  operator can choose N from evidence before enabling it.
- **Enabled, recipient opted into the digest.** The (N+1)th non-urgent ping or
  reminder in the window is not posted. It stays an open, overflowed ask: it is
  listed in the item's status card (`attention-overflow` section), in the
  repo's action queue, and in the recipient's digest.
- **Enabled, recipient not opted in.** Per-episode delivery is kept: newly
  actionable asks are always posted; only reminders over the cap are deferred
  (and remain visible in the status card and action queue).
- **Digest.** One comment per recipient per repo, identified by
  `<!-- hive:attention-digest recipient=<login> -->`, posted on the repo's
  attention queue issue, created once through the existing marker-comment
  upsert and edited in place afterwards (edits notify nobody). It serves as the reminder for the asks it lists. No external
  service is involved.
- **Never budgeted.** Materially new security risk or imminent destructive
  action, mandatory/required reviews, machine-consumed protocol commands, and
  every security/merge hold notice always deliver immediately and do not
  consume budget. Unclassified writes are treated as exempt.

### Attention action contract

Every admitted attention event must let the recipient act without digging
([#16359](https://github.com/hivecommons/hive/issues/16359), part of
[#16270](https://github.com/hivecommons/hive/issues/16270)). Producers build
the event body with `attention.NewObservation`, which refuses an incomplete
`attention.Event`; `Event.Render` emits, in order:

| Field | Meaning |
|---|---|
| **Who** | The person or team asked to act. |
| **What changed / why now** | The change that made the condition need attention now. |
| **Requested action** | The single concrete thing asked of the recipient. |
| **Evidence** | What the condition was observed from (run, finding, SHA). |
| **What Hive tried** | What Hive already attempted on its own. |
| **What happens next** | What Hive will do if nobody acts. |
| **How to ack, snooze or resolve** | `/hive ack`, `/hive snooze <duration>`, and what clears the condition. |

Every event ends with the same wording: *Acknowledgement is not approval:
`/hive ack` and `/hive snooze` only quiet reminders. They release no security,
merge or review hold, change no label and open no merge gate.* See
[maintainer commands](maintainer-commands.md#attention-ack-and-snooze).

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

## Review requests

An agent's `request_review` relay (`hive-open-issue request-review`, or
`gh pr edit --add-reviewer`) skips any user or team whose review request on
the PR is still pending: asking again would only re-ping them for the same
unresolved ask. The skipped names are reported in the request result as
`reviewers_already_pending` / `team_reviewers_already_pending` and in the
audit entry as `already_pending`; when everyone named is already pending, no
GitHub call is made. Someone who has already submitted a review is no longer
pending, so they can still be asked again for a new head. If the pending list
cannot be read, everyone named is requested, so a needed review is never
dropped.

## Mention hygiene

Text the hive copies into a body it authors (issue/PR titles, quoted comments,
evidence, attribution) must never notify the people it names. The status
comment writer (`prstatus.Write`) passes every section through
`advisory.NeutralizeMentions`, the same convention the advisory digest and
duplicate-sweep renderers use: `@login` becomes `` `login` `` (only the `@` is
dropped inside code spans and fences). Email addresses, URLs and `#123`
references are untouched, and the rewrite is idempotent. Sections of a status
comment another author posted are neutralised too when they are merged into
the hive-owned comment, since that body is re-edited on every pass. This
applies regardless of the attention-pilot flag.

Deliberate mentions are posted as their own comments or reviews through the
notification-class framework and are not rewritten. The one deliberate mention
that lives in the status comment, the trusted-human 071 note's `@author` in
quiet-notification mode, goes through `prstatus.WriteMentioning`, which posts
the section verbatim; its composer neutralises the copied check names and
review asks itself, so the author is the only person it notifies.

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

An issue is an RFC when it carries the `rfc` label, its title starts with `RFC` after any leading emoji or `[tag]` (`RFC: ...`, `[RFC] ...`, `📝 RFC: [v6] ...`), or one of its leading `[tags]` contains the word `RFC` (`📝 [v5 RFC] ...`, `[RFC v6] ...`; `[RFC #N]` references an RFC and does not count). A hive that manages several repos prefixes the number with the repo (`owner/repo#N`).

The default is empty, which turns the notices off. `channel_id` is not required: the notices only post with the bot token and never start the command bot. The hive polls GitHub every three minutes for issues updated since its last poll (one list call per repo) and keeps per-issue records in `/data/rfc-notify.json`, so a restart never re-announces a transition. The first poll after the key is set only records the current open RFCs; transitions announce from the next poll on. Each post is also written to the audit log as `rfc-notify`.

```yaml
notifications:
  discord:
    bot_token: ${DISCORD_BOT_TOKEN}
    rfc_channel_id: "1540404487099777136"
```

### Rich mode, the designated announcer, and private repositories

These keys sit next to `rfc_channel_id`. All of them default off, and with none of them set the notices are the one-line posts above, unchanged.

| Key | Default | Effect |
| --- | --- | --- |
| `rfc_format` | `""` (= `line`) | `line` posts one line per transition. `card` posts the transition text (including a linked RFC for accepted documents) alongside a Discord summary card (embed; with `rfc_threads: true` the post is plain text and the embed renders only when threads are off) with target, status, scrubbed summary, open-decision count and colour by state (held grey, needs-decision amber, ready green, closed completed blue, closed not-planned dark); malformed bodies fall back to the line. Card posts share the bounded `429` retry of every Discord call (see *Rate limits and failures* under [Threads](#one-thread-per-rfc-rfc_threads)). Any other value is a config error. |
| `rfc_threads` | `false` | Rich mode: one Discord thread per RFC in the existing text channel (see [Threads](#one-thread-per-rfc-rfc_threads)). Needs `rfc_announcer`. |
| `rfc_announcer` | `false` | Marks this hive as the one designated announcer for `rfc_channel_id`. Rich mode posts only from the announcer. |
| `rfc_backlink_comment` | `false` | With `rfc_threads`, comments `Discussion thread: <url>` on the RFC once, after the thread exists. |
| `rfc_include_private` | `false` | Authorizes posting RFCs from private repositories to the channel. |

- **Announcer rule.** Rich mode means `rfc_format: card` or `rfc_threads: true`. A hive in rich mode without `rfc_announcer: true` posts nothing and logs `rfc notify: rich mode requires rfc_announcer; not posting` once. It keeps recording transitions, so turning on the announcer later does not backfill old ones. Set `rfc_announcer: true` on exactly one hive per channel. Line mode ignores the key.
- **RFC documents proposed as pull requests (rich mode only).** A pull request is an RFC document when its title starts with `📝 proposal:` or `📝 RFC`, or it adds or changes `docs/rfc-*.md` or `src/docs/rfc-*.md`. The announcer posts `RFC doc proposed: #N <title> - <url> (by @author)` when one opens, titled by the document's first heading when the hive read it, and `RFC doc accepted (RFC #M): ...` when it merges, attributed to the RFC issue its body links (`Closes`/`Refs #M` or an issue URL), or `RFC doc accepted: ...` when it links none the hive tracks. PRs already open when the hive takes its baseline are not announced, and a PR closed without merging posts nothing. Each poll lists pull requests updated since the last poll, plus the changed files of those whose title does not already mark them. Line mode stays issues-only.
- **Lifecycle changes (rich mode only).** On the announcer the hive also posts one line per change to an open RFC: `RFC decisions: 2 → 1 open: ...` when the open count under *Decisions for maintainers* changes (at most one per RFC per 30 minutes; changes inside that window collapse into the next notice with the latest count), `RFC target: v5 → v6: ...` and `RFC status: <old> → <new>: ...` (values cut to 100 characters) when the RFC field table changes (a body that no longer parses is not a change), and `RFC needs a decision: ...` when `stage/045-needs-decision` or `needs-human` is added. A retitle posts nothing: with `rfc_threads` it edits the opened card in place and renames the thread. An RFC opened by the same author with the same title (lowercased, RFC prefix, leading bracketed tags such as `[v5 RFC]` or `[v6]` and emoji removed, leading/trailing punctuation stripped, whitespace collapsed) as an open RFC in the same repo opened up to an hour earlier gets no card: the hive posts `RFC duplicate filed as #M - <url>` once into the first RFC's thread, and closing the duplicate posts nothing. These lines are never cards. With `rfc_threads` they go into the RFC's thread; otherwise to the channel. The 30-minute and one-hour windows are fixed. Line mode posts none of them.
- **Private rule (every mode, line mode included).** An RFC in a private or `internal` repository is not posted unless `rfc_include_private: true`. This keeps its title and link out of the channel. The hive resolves each repo's visibility once and caches it for an hour. A repo whose visibility cannot be resolved counts as private (fail closed). A withheld transition is still recorded, so it is never posted later.

```yaml
notifications:
  discord:
    bot_token: ${DISCORD_BOT_TOKEN}
    rfc_channel_id: "1540404487099777136"
    rfc_format: card
    rfc_announcer: true          # only on the one announcing hive
    rfc_include_private: false   # default: private-repo RFCs are not posted
```

#### One thread per RFC (`rfc_threads`)

On the announcer, `rfc_threads: true` keeps the text channel (no Forum) and turns each RFC into a card plus a thread:

- **Opened** posts the card, then starts a thread on it named `#N <title>` (cut to Discord's 100 characters). The card and the thread ID are stored per RFC in `/data/rfc-notify.json`, which is now state version 2; a version 1 file loads unchanged, and a file written by a newer hive is refused rather than downgraded.
- **Closed, ready to implement, doc accepted and the lifecycle changes above** post into the RFC's thread. Closed and ready also echo one line to the channel. An RFC announced before threads were enabled has no thread and keeps getting the plain channel line; it is never back-filled.
- **Idempotency.** Before the card is posted the hive saves a write-ahead intent with a deterministic nonce (a hash of `repo#number` plus `opened`, 25 characters) and sends it with Discord's `nonce` and `enforce_nonce`, so a retry inside Discord's dedup window returns the same message. Channel history is never treated as a claim between hives; the single announcer is the only writer.
- **Reconciliation.** At startup, and whenever an RFC has an intent without a confirmed card or a card without a thread, the hive does one lookup of the channel's last 50 messages for a message of its own bot with the card's nonce or exact text. A match is adopted (no second card); with no match the card is re-posted once more with the same nonce. A failed lookup re-posts nothing. Card posts and thread starts are each tried at most 3 times across polls and restarts, then logged and left alone. Only RFCs this hive began announcing are reconciled, so there is no historical burst.
- **Backlink (`rfc_backlink_comment: true`).** Once the thread is confirmed the hive comments `Discussion thread: <thread URL>` on the RFC, through the per-issue bot comment budget. The flag is saved after it succeeds, so restarts do not repeat it; a failure leaves the flag unset and is retried up to 3 times, then logged. Default off: the hive never comments on issues otherwise.
- **Rate limits and failures.** Every Discord call honours a `429` `retry_after` with at most 3 bounded retries (each wait capped at 15 seconds). Failures count in the sweep's `errors`, are logged as warnings, and are written to the audit log as `rfc-notify` lines with `threads`, `reconciled`, `backlinks` and `errors`.

```yaml
notifications:
  discord:
    bot_token: ${DISCORD_BOT_TOKEN}
    rfc_channel_id: "1540404487099777136"
    rfc_threads: true
    rfc_announcer: true            # only on the one announcing hive
    rfc_backlink_comment: false    # opt in to comment the thread link on the RFC
```

The bot needs the `Create Public Threads` and `Send Messages in Threads` permissions, plus `Read Message History` for reconciliation.

## GitHub writes: inventory and shadow mode (`github.notifications.shadow`)

Every new issue/PR comment, review submission, review request and @-mention
the hive makes on GitHub emails the people involved and everyone watching the
repository. [`github.notifications.mode`](operator-reference/sweeps-and-operations.md#notification-mode-githubnotificationsmode)
already folds the hive's own bookkeeping into one edited status comment;
shadow mode measures what is left so a later change can be judged against a
baseline ([#16075](https://github.com/hivecommons/hive/issues/16075)).

```yaml
github:
  notifications:
    shadow: true   # default false
```

Shadow mode is **instrumentation only**: nothing is suppressed, delayed or
rerouted, and the GitHub writes are byte-for-byte the same with the flag on
or off. With the flag on, each notification-capable write the Go client makes
is attributed to its producer (the inventoried function below), classified,
and given a *would-notify* decision:

- **Record** — `{repo, item, producer, class, condition key, would_notify,
  reason}` plus the logins the write mentions or requests. Comment bodies,
  other recipients and inbox contents are never kept. The last 512 records
  stay in memory (`Client.NotificationShadowRecords`).
- **Condition key** (provisional) — `(repo, item, class, subject)`, where the
  subject is what the writer supplies (the status-comment section key for
  `PostNoteOrFold`) or the producer name. A later change defines the
  authoritative, producer-independent key.
- **Decision** — a write whose condition key was already written within 24
  hours is `would_notify=false`, reason `repeat_condition`, **only** for the
  `status` and `attention` classes. `protocol_command`, `required_review` and
  `security_event` writes are always `would_notify=true` (a repeat gets reason
  `exempt_class_repeat`). A failed write is `delivery_failed`.

### Counters

The hourly `github notifications` log line gains `shadow_would_suppress`,
`shadow_reviews`, `shadow_review_requests`, `shadow_mentions`,
`shadow_delivery_failures` and `shadow_active_items` while shadow mode is on
(the line is unchanged while it is off). The opt-in Prometheus endpoint
(`HIVE_METRICS_ENABLED` + `HIVE_METRICS_TOKEN`) exports:

| Series | Labels | Meaning |
|---|---|---|
| `hive_github_comments_total` | `op` = `created` / `edited` / `suppressed` | The always-on comment tally. |
| `hive_github_notification_comments_created_total` | `producer`, `class` | New comments. |
| `hive_github_notification_reviews_submitted_total` | `producer`, `class` | Review submissions. |
| `hive_github_notification_review_requests_total` | `producer`, `class` | Review requests. |
| `hive_github_notification_mentions_total` | `producer`, `class` | @-mentions in posted comments and reviews. |
| `hive_github_notification_suppressed_repeats_total` | `producer`, `class` | Notes the hive actually did not post (folded or already present). |
| `hive_github_notification_would_suppress_repeats_total` | `producer`, `class` | Writes shadow mode *would* suppress. |
| `hive_github_notification_delivery_failures_total` | `producer`, `class` | Failed writes. |
| `hive_github_notification_active_items` | — | Items with a notification-capable write in the last 24 hours. |

All counts are API writes, a proxy for notifications — never "emails
delivered". Counters are cumulative since process start.

### Comparing baseline windows

Compare **per active item**, over **matched windows**: the same length (a
whole week, so weekday traffic is matched), similar open-item load, and no
change to `github.notifications.mode` or the hive version inside either
window. For each window compute

```
writes      = increase(hive_github_notification_comments_created_total[7d])
            + increase(hive_github_notification_reviews_submitted_total[7d])
            + increase(hive_github_notification_review_requests_total[7d])
per_item    = writes / avg_over_time(hive_github_notification_active_items[7d])
repeat_rate = increase(hive_github_notification_would_suppress_repeats_total[7d])
            / avg_over_time(hive_github_notification_active_items[7d])
```

broken down by `class` and `producer`. The baseline is a shadow-on window
before any suppression ships; the candidate is the same metrics after it. The
80% reduction in repeat/non-actionable writes per active item named in #16075
is an **acceptance target**, not a measured or claimed result: shadow mode
only produces the baseline that target will be checked against.

### Write-site inventory

Every function under `src/pkg` and `src/cmd` that calls a notification-capable
write primitive (`CreateIssueComment`, `CreateIssueCommentBudgeted`,
`PostNoteOrFold`, `PostClaimNote`, `PullRequests.CreateReview`,
`RequestReviewers`) or records a suppressed note is listed here and in
`notificationWriteSites` (`src/pkg/github/notification_write_sites.go`).
`TestNotificationWriteSitesInventory` fails when a new call site is added
without a class. Review submissions and review requests are always counted as
`required_review`, whichever producer made them.

- `status` — bookkeeping readable on demand; repeats may be would-suppress.
- `attention` — asks a person to look or act; repeats may be would-suppress.
- `required_review` — a review or review request a merge depends on; never would-suppress.
- `protocol_command` — machine-consumed (claim markers, `/lgtm`, `/approve`, `/ok-to-test`); never would-suppress.
- `security_event` — trust or integrity signal; never would-suppress.

| Producer | Class | Notes |
|---|---|---|
| `dashboard.(*ContributeWSHub).markAlreadyDoneIssue` | `status` |  |
| `dashboard.(*ContributeWSHub).releaseGateParkLabel` | `status` |  |
| `dashboard.(*ContributeWSHub).releaseToolingBlockedLabel` | `status` |  |
| `dashboard.(*Server).armRebaseThenMerge` | `status` |  |
| `dashboard.(*Server).executeDelegatedDecision` | `status` |  |
| `dashboard.(*Server).handlePullRequestMergeDecision` | `status` |  |
| `dashboard.(*Server).handleRepoItemAdoptNow` | `status` |  |
| `dashboard.(*Server).handleRepoItemBlockerClosePartial` | `status` |  |
| `dashboard.(*Server).handleRepoItemDecision` | `status` |  |
| `dashboard.(*Server).handleReviewQueueSupersededClose` | `status` |  |
| `dashboard.(*Server).mirrorPlanWith` | `status` |  |
| `github.(*Client).CloseIssueForConfiguredLinePR` | `status` |  |
| `github.(*Client).ApproveIssueRelease` | `status` | solicited authenticated release approval response |
| `github.(*Client).CreateIssueCommentBudgeted` | `status` | passthrough wrapper; attributed to its caller |
| `github.(*Client).PostFleetReportCleared` | `status` |  |
| `github.(*Client).PostFleetReportRecovery` | `status` |  |
| `github.(*Client).PostFleetReportRollup` | `status` |  |
| `github.(*Client).PostNoteOrFold` | `status` | passthrough wrapper; attributed to its caller |
| `github.(*Client).SweepLegacyFleetReportIssues` | `status` |  |
| `github.(*Client).UpsertIssueMarkerComment` | `status` | passthrough wrapper; attributed to its caller |
| `github.(*Client).EditIssueMarkerComment` | `status` | passthrough wrapper; attributed to its caller |
| `automerge.(*engineStatusForge).CreateComment` | `status` | passthrough wrapper shared by normal-mode stage notes and the quiet-mode status-comment forge under `prstatus.Write`; attributed to its caller |
| `github.(*clientStatusForge).CreateComment` | `status` | passthrough wrapper (status-comment forge under `prstatus.Write`); attributed to its caller |
| `github.(*Client).PostIssueNote` | `status` | passthrough wrapper (issue status card, `attention_pilot`); attributed to its caller |
| `github.(*Client).closeDecomposedParent` | `status` |  |
| `github.(*Client).closeIssueForMergedPRClaimWithIssue` | `status` |  |
| `github.(*Client).closeSupersededPRDecision` | `status` |  |
| `github.(*Client).fileReviewBacklog` | `status` |  |
| `github.(*Client).joinFleetReportIssue` | `status` |  |
| `github.(*Client).markInProgressStarted` | `status` |  |
| `github.(*Client).postUnblockedNote` | `status` |  |
| `github.(*Client).priorArtGateClose` | `status` |  |
| `github.(*Client).reconcileBlockedTargets` | `status` |  |
| `github.(*Client).recordReviewEvidenceMerge` | `status` |  |
| `github.SyncIssueStageLabelsWith` | `status` |  |
| `github.ensureIssueStagePRLink` | `status` |  |
| `github.githubIssueBacklogSink.AppendToItem` | `status` |  |
| `github.githubIssueBacklogSink.CloseItem` | `status` |  |
| `github.githubIssueBacklogSink.LiftBlock` | `status` |  |
| `github.handleMergedPRIssueStage` | `status` |  |
| `github.runRecheckSoak` | `status` |  |
| `main.postReviewAcknowledgements` | `status` |  |
| `main.rfcNotifySource.Comment` | `status` |  |
| `main.unparkEscalatedPR` | `status` |  |
| `mention.(*StageCommenter).Post` | `status` | deduplicated stage transition note |
| `mergelane.(*RESTGitHub).CreateIssueComment` | `status` | passthrough wrapper; attributed to its caller |
| `dashboard.(*ContributeWSHub).parkWrongBase` | `attention` |  |
| `dashboard.(*ContributeWSHub).noteAlreadyDoneUnverified` | `attention` | asks a maintainer to verify an unsupported already-done claim |
| `dashboard.(*ContributeWSHub).needsDecisionParkPost` | `attention` | the question a contributor's needs-decision park was left without |
| `dashboard.(*Server).handleRepoItemReroute` | `attention` |  |
| `dashboard.(*Server).handleReviewPipelineSendToHuman` | `attention` |  |
| `dashboard.(*Server).reroutePickupLapsed` | `attention` |  |
| `dashboard.(*Server).settleRebaseMergeLedger` | `attention` |  |
| `github.(*Client).PostIssueAttention` | `attention` | the one explicit announcement of a newly required action (`attention_pilot`) |
| `github.(*Client).NudgeTrackingIssueClosingRefs` | `attention` |  |
| `github.(*Client).appendFoldedFinding` | `attention` |  |
| `github.(*Client).bindProwLgtm` | `attention` |  |
| `github.(*Client).commentOnHumanFiledTwin` | `attention` |  |
| `github.(*Client).createIssue` | `attention` |  |
| `github.(*Client).handleOneIssueRequest` | `attention` |  |
| `github.(*Client).parkClankerRequestedPR` | `attention` |  |
| `github.(*Client).redirectToExistingPR` | `attention` |  |
| `github.(*Client).remindAttention` | `attention` | attention-pilot reminder: at most one per unresolved episode per `reminder_interval`; write-back gate consulted first |
| `github.(*Client).resolveDuplicateFixGroup` | `attention` |  |
| `github.attentionDelivery.Post` | `attention` | condition-episode page; live write-back policy rechecked before the write |
| `main.(*repoForgeRouter).CreateIssueComment` | `attention` | passthrough wrapper; attributed to its caller |
| `main.claimEscalationLookup` | `attention` |  |
| `main.liveTriageCommenter.CreateIssueComment` | `attention` | passthrough wrapper; attributed to its caller |
| `main.noteHeldPRUpdateStopped` | `attention` |  |
| `main.sweepDependencyBotConflicts` | `attention` | asks a person to rebase or retry a dependency-bot PR |
| `mention.(*Responder).HandleAgentEvent` | `attention` | solicited completion reply to an explicit mention |
| `mergelane.(*round).ensureForkWaitComment` | `attention` | asks the fork author to refresh the branch |
| `worksource.(*forgeWorkSource).AddComment` | `attention` | passthrough wrapper; attributed to its caller |
| `worksource.(*githubIssuesSource).AddComment` | `attention` | passthrough wrapper; attributed to its caller |
| `main.rfcCoverageForge.CreateComment` | `attention` |  |
| `main.runEscalationSweep` | `attention` |  |
| `prfollowup.postStaleHeadPushNotes` | `attention` |  |
| `scheduler.(*Scheduler).judgePriorArt` | `attention` |  |
| `scheduler.(*Scheduler).parkUnclearIssue` | `attention` |  |
| `scheduler.(*Scheduler).postTriageClarifyComment` | `attention` |  |
| `github.(*Client).QueuePRAutoMergeWithNoteResultAtHead` | `required_review` |  |
| `github.(*Client).RequestReviewers` | `required_review` | passthrough wrapper; attributed to its caller |
| `github.(*Client).handleOneReviewRequest` | `required_review` |  |
| `dashboard.postClaimComment` | `protocol_command` |  |
| `github.(*Client).PostClaimNote` | `protocol_command` |  |
| `github.(*Client).reportOperatorPullRequestApproval` | `protocol_command` |  |
| `main.githubClaimHooksOn` | `protocol_command` |  |
| `github.(*Client).RecordVerdictBlocker` | `protocol_command` | carries the machine-consumed blocked-on marker |
| `github.(*Client).handleOnePRRequest` | `security_event` |  |
| `github.(*Client).markReporterTrustAwaiting` | `security_event` |  |
| `github.(*Client).reconcileReporterTrustSignal` | `security_event` |  |
| `main.enforceHoldGuard` | `security_event` |  |

Hive-maintained GitHub Actions workflows that comment (not instrumented by the
Go client; listed for completeness):

| Workflow | Class | What it posts |
|---|---|---|
| `changelog-fragment-guard.yml`, `changelog-reminder.yml`, `docs-index-reminder.yml` | `status` | PR reminders about changelog fragments and the docs index. |
| `issue-closed-reopen-hint.yml`, `issue-reopen-command.yml` | `status` | Reopen hint on closed issues; reply to a reopen command. |
| `backend-smoke.yml`, `ci-runner-canary.yml`, `coverage-hourly.yml`, `dco-post-merge.yml`, `flue-smoke.yml`, `omp-smoke.yml`, `wavefront-smoke.yml`, `v2-ci.yml`, `v2-tests.yml` | `attention` | "Still failing" comments on an open failure-tracking issue. |
| `promote-stable.yml` | `attention` | Emergency stable-promotion exception note. |
| `weekly-competitor-report.md` (`.lock.yml`) | `status` | Weekly report comment on the tracking issue. |
| `hive-ok-to-test.yml` | `protocol_command` | The `/ok-to-test` command that releases held workflow runs. |

## Attention pilot and the issue status card (`github.notifications.attention_pilot`)

The attention pilot is the opt-in gate for the attention changes in
[#16270](https://github.com/hivecommons/hive/issues/16270); every
attention-pilot behaviour reads this one flag. It is **off by default**, and
with it off the hive behaves exactly as before.

```yaml
github:
  notifications:
    attention_pilot: true   # default false
```

Quiet mode (`github.notifications.mode: quiet`) folds the hive's PR
bookkeeping into one edited `hive:status` comment but leaves issue notes as
comments of their own, so one issue lifecycle (partial merge, remainder,
close) could still email its watchers several times. With the pilot on, the
issue gets the same card, written through the same single status writer
(`prstatus.Write`, serialised per item, duplicates merged into the oldest):

- **Folded into the card** (section key in brackets): partial work left by a
  non-closing merged PR (`issue-partial:<pr>`), close/auto-close narration —
  "Fixed by #N … Reply /reopen if the problem persists" and the decomposed
  parent's all-children-delivered close — (`issue-close`),
  reopened-after-merge (`issue-reopened`), a lapsed claim's stage regression
  (`stage-regressed`), the covering-PR stage note (`issue-pr-link`), the
  relay's one-line state notes — awaiting CI, parking, already fixed —
  (`agent-note:<state>`), the decision-downgrade note left when a label
  change walks back an earlier maintainer decision
  (`issue-decision-downgrade`) and the blocker-settled note (`unblocked`,
  whatever `mode` says). Claim notes and the relay state notes already fold
  under `claim` and `agent-note:<state>` in quiet mode; the
  decision-downgrade note folds under the pilot only. The legacy markers
  stay inside each section, so every reader that greps for them still finds
  them, and the reopen/release guidance stays visible on the card. Section
  text that quotes issue-authored prose has the card's own
  `<!-- hive:status… -->` markers neutralised, so it cannot close its
  section or open another.
- **Native events notify instead.** The issue is still closed (with its
  state reason) by the API call, which is GitHub's own notification; the
  hive no longer adds a "for visibility" comment beside it.
- **A newly required action is still announced once.** Editing a comment
  notifies nobody, so an action asked of a person (today: confirming a
  merged fix with `/fixed` or `/reopen`) is recorded on the card under
  `attention:<cause>` *and* announced by exactly one notifying write —
  creating the card when the issue has none, otherwise one explicit
  comment tagged `<!-- hive:attention cause=<cause> -->`. A later pass that
  sees the tag only refreshes the card; a new cause gets its own single
  announcement. The explicit comment consults the write-back gate like
  every other unsolicited comment; a refusal is returned to the caller as
  `writeback.ErrSuppressed` before anything is written (no comment, no tag
  on the card), so the action is not reported as announced and the next
  pass retries once the policy admits it.
- **Not folded:** the blocked sweep's re-target note (`<!-- hive:blocked-on … -->`)
  stays a comment, because the sweep reads the *newest* blocked-on marker
  by comment order and a section in an older card would not be newest.

Acknowledge/snooze commands, reminders and the per-repo attention queue are
separate pilot steps ([#16359](https://github.com/hivecommons/hive/issues/16359),
[#16361](https://github.com/hivecommons/hive/issues/16361)–[#16363](https://github.com/hivecommons/hive/issues/16363))
and are not part of this flag's first behaviour.

### Attention section, tracking issue and reminders

The reminder policy ([#16361](https://github.com/hivecommons/hive/issues/16361))
and the GitHub-native action queue ([#16362](https://github.com/hivecommons/hive/issues/16362))
now run under the same flag, on repositories whose `github.attention.admitter`
is this hive. The un-park sweep (every 5 minutes) makes one pass per
repository after it has recorded that pass's `/hive ack` and `/hive snooze`:

- **Attention section.** Each item's `hive:status` card carries an
  **Attention** section (key `attention`): one line per unresolved episode
  naming the action, who owes it, since when, any snooze deadline and how
  many reminders were sent. It is written by the single status writer and
  edited in place.
- **Per-repo tracking issue.** One issue titled `[governor] Hive attention
  queue` (label `origin/governor`, body tag `<!-- hive:attention-queue -->`)
  lists every open episode in the repository. It is created once, when the
  first episode is open — its only notifying write — and afterwards edited in
  place only when the list changes; item numbers are code spans and logins
  are neutralised, so an edit never mentions or cross-references anyone. A
  queue issue a person closed is left alone; reopen it to resume updates.
- **Closure.** When an item with open episodes is closed, its episodes are
  resolved, its Attention section is removed from the card and it drops out
  of the tracking issue, so no stale ask or reminder outlives it.
- **Reminders.** At most one reminder comment per unresolved episode per
  `github.attention.reminder_interval` (default `24h`), counted from the page,
  the previous reminder or the recipient's last action, whichever is latest.
  None while the episode is acknowledged or snoozed, none for a never-paged
  or uncertain episode, and none when the recipient has commented, reviewed
  or changed a label on the item since the last ping. A reminder consults
  the write-back gate like any unsolicited comment, goes through
  `CreateIssueComment` (so shadow mode counts it under
  `github.(*Client).remindAttention`) and refreshes the card. Hive never
  removes and re-adds a reviewer or requests a review to manufacture a
  reminder, so a request GitHub, CODEOWNERS or Prow already owns is never
  duplicated.

```yaml
github:
  attention:
    reminder_interval: 24h   # default 24h
```

### Hive-maintained Actions workflows that defer to the card

Some comments come from GitHub Actions workflows in this repository rather
than from the runtime. When one of them says what a `hive:status` card
section already says, it now defers (the check is
`.github/scripts/hive-status-card.sh has <repo> <number> <section>`, which
looks for `<!-- hive:status:section=<key> -->` inside the comment carrying
`<!-- hive:status -->`, considering only comments by a Bot account so a
human quoting the card's invisible markers cannot suppress a workflow; a
caller that has already fetched the comments passes them via
`HIVE_COMMENTS_FILE` instead of re-fetching). The companion
`hive-status-card.sh upsert <repo> <number> <marker> <body-file>` edits the
workflow's own earlier comment carrying `<marker>` in place, or creates it;
it only edits comments authored by `HIVE_COMMENT_AUTHOR` (default
`github-actions[bot]`, the identity `${{ github.token }}` posts as), never a
third party's comment that happens to contain the marker:

| Workflow / script | Comment it posts | Behaviour |
| --- | --- | --- |
| Refs sweep (`.github/workflows/refs-sweep.yml`, `src/scripts/comment-merged-refs.sh`) | "PR #N has merged and referenced this issue with a non-closing `Refs`" (`<!-- hive-post-merge-refs-sweep: issue=N -->`) | **Skips** when the card carries `issue-partial:<pr>` for that PR; otherwise unchanged (still once per issue). |
| Issue Closed Hint (`.github/workflows/issue-closed-reopen-hint.yml`) | "@reporter this issue was closed by … comment `/reopen`" | **Pending** (tracked in #16363; the workflow edit must be landed by a maintainer). Once applied: **skips** when the card carries `issue-close`, which already says "Reply /reopen"; otherwise **edits** its earlier comment (`<!-- issue-closed-reopen-hint -->`) instead of adding one per closure. Until then it posts a new comment on every closure as before. |

Editing a comment notifies nobody, so a workflow comment that is edited in
place stops re-mentioning the reporter on every close/reopen cycle. Both
paths apply only where a card exists; with
`github.notifications.attention_pilot` off there is no issue card, and the
workflows behave as before.

A failure to read the card is not treated as "no card": when the comments
API call fails or its response is not a JSON list of comment objects,
`hive-status-card.sh` writes a `hive-status-card: error:` line and exits 3
(`has` otherwise exits 0 for present, 1 for absent), and `upsert` writes
nothing. The refs sweep then skips that issue with an `::error::` annotation
and exits non-zero instead of posting a possible duplicate.
`.github/scripts/test-hive-status-card.sh` and
`src/scripts/test-comment-merged-refs.sh` pin this behaviour.

**Outside Hive's control.** Comments from external bots, such as Prow, tide
and kubestellar-prow, are posted by their own services and cannot be edited,
folded or skipped from this repository. Hive does not try to suppress them;
configure those bots in the project that runs them.
