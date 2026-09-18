# Slack integration: the second backend on the chat spine

Status: Proposed for **v6** — Track 3 of the epic
[#7563](https://github.com/hivecommons/hive/issues/7563) (folds in the original
request, #7559). Design only; nothing described here changes v4/v5 behaviour.
Implementation targets the `v6` branch and depends on the chat spine
(`pkg/chat`, Track 1) landing first.

Every code reference below was checked against `v4` at `e81d51750` while
writing this page. Line numbers may drift; the named symbols are the stable
handles.

---

## Problem

Hive speaks exactly one chat protocol: Discord. `pkg/discord` carries both the
transport (Discord REST calls) and everything that is not Discord at all — the
command router, the dashboard REST/SSE client, status-diff notifications, the
message queue, agent identities. An operator whose team lives in Slack gets
nothing, and adding Slack by copying `bot.go` would fork 869 lines of logic
that has nothing to do with either protocol.

The v6 theme is dashboard-optional operation: meet operators where they are.
Slack is the largest "where they are" that Hive cannot reach today.

## Shape: a `chat.Backend`, nothing more

Track 1 extracts the transport-agnostic core into `pkg/chat`:

```go
type Backend interface {
        Name() string
        Send(content string) error
        SetTopic(topic string) error // ErrTopicUnsupported when the transport can't
        Listen(ctx context.Context, deliver func(Message))
}
```

Everything else — the command registry and aliases, the fail-closed
allowlist (the SECURITY F8 / CWE-862 contract on `Config.AllowedUsers`:
empty means commands disabled), the dashboard client, SSE status diffing,
the send queue and rate pacing, heartbeats — lives in `chat.Service` and is
shared verbatim. **Slack is only a transport.** `pkg/slack` implements the
four methods and nothing else; if a piece of the work does not fit behind
`Backend`, it belongs in `pkg/chat` and Discord gets it for free too.

## Transport choice: Socket Mode first

Slack offers two inbound paths:

| Path | Ingress | Ops burden | Fit |
|---|---|---|---|
| Events API (HTTP) | public HTTPS endpoint + URL verification | TLS, exposure, request signing | poor for self-hosted hives |
| **Socket Mode** | outbound WebSocket, app-level token | none — works behind NAT | **chosen** |

Hive installations are frequently on private networks (the dashboard itself is
not internet-facing by default). Socket Mode requires no inbound port: the bot
opens `apps.connections.open`, receives a WSS URL, and Slack pushes events over
it. This mirrors how the Discord bot avoids the Gateway today by *polling* —
but Socket Mode is strictly better than polling and costs nothing extra, so
Slack starts there. An Events API receiver can be added later behind the same
`Backend` without touching `pkg/chat`.

Outbound stays plain Web API over HTTPS: `chat.postMessage` for `Send`,
`conversations.setTopic` for `SetTopic`.

## Mapping the Backend

| `chat.Backend` | Slack call | Notes |
|---|---|---|
| `Send` | `chat.postMessage` | one channel, from config; 4,000-char segmenting below |
| `SetTopic` | `conversations.setTopic` | needs `channels:manage` / `groups:write`; degrade to log-only if the scope is missing |
| `Listen` | Socket Mode WSS | filter to `message` events in the configured channel; ack every envelope within 3s |
| `Message.FromBot` | `bot_id` present or `subtype=bot_message` | the spine drops bot-authored messages — same loop-prevention as Discord |
| `Message.AuthorID` | Slack user ID (`U…`) | allowlist entries are Slack user IDs, never display names (display names are spoofable) |

**Formatting.** Discord messages use Markdown (`**bold**`, code fences);
Slack uses mrkdwn (`*bold*`, no nested fences in attachments). The spine
composes messages once, so the seam is a translation hook on the backend:
`Send` runs a small Markdown→mrkdwn pass (bold, inline code, links) before
posting. No `chat.Service` change; Discord's backend passes text through
untouched.

**Limits.** Discord truncates at 1,900 chars; Slack's practical ceiling is
4,000 per message. The spine's limit is per-backend config, so Slack sets its
own and long statuses split on paragraph boundaries rather than truncating.

**Pacing.** Slack Web API tier for `chat.postMessage` is ~1 message/second per
channel — coincidentally close to the spine's existing 1,200 ms drain
interval, which Slack simply keeps. On `429` the backend honours
`Retry-After` before the next drain.

**Reconnect.** Socket Mode URLs expire and Slack sends `disconnect` envelopes
(`refresh_requested`). `Listen` reconnects with the same 5 s → 60 s capped
backoff the Discord SSE consumer uses; the spine never sees the gap.

## Configuration

```yaml
slack:
  enabled: false            # default off, like discord
  app_token: ${SLACK_APP_TOKEN}   # xapp-… (Socket Mode)
  bot_token: ${SLACK_BOT_TOKEN}   # xoxb-… (Web API)
  channel_id: C0123456789
  allowed_users: []         # Slack user IDs; EMPTY = commands disabled (fail closed)
```

Validation mirrors `DiscordConfig`: enabling without both tokens or the
channel is a config error; `allowed_users` keeps the F8 contract — an empty
allowlist means *no one* may command, not everyone. Tokens come from env
expansion, never committed. App manifest (documented alongside the config
reference): scopes `chat:write`, `channels:history`, `channels:manage`,
`connections:write`; event subscription `message.channels`.

## The guard invariant, restated

Slack adds **no authorization surface of its own** (epic #7563 invariant):

- commands are gated by the same allowlist mechanism, fail closed;
- everything a command can do is a dashboard REST call carrying the same
  bearer the Discord bot uses — the dashboard remains the authz choke point;
- inbound text that becomes a kick passes `ioscan.EnforceInput`
  (`src/pkg/ioscan/enforce.go:24`) exactly where the spine already applies it;
- outbound messages pass the same canary/secret scrub as every other surface;
- a Slack summon can never reach an agent or capability the mode ladder
  would deny at the proxy.

## Phasing

1. **PR A — `pkg/slack` backend**: Socket Mode listener, Web API sender,
   mrkdwn translation, reconnect/backoff, config + validation, wiring in
   `cmd/hive` beside the Discord bot start. Full command parity arrives free
   via the spine. httptest + fake-WSS tests to the same coverage bar as
   `pkg/discord`.
2. **PR B — polish**: threaded replies for command responses (keep channel
   noise down), `conversations.setTopic` presence line, docs page under
   `src/docs/` for operator setup.
3. **Later**: Events API receiver as an alternative ingress; slash commands
   (`/hive status`) if teams prefer them over message commands.

## What this deliberately does not do

- No Slack-side agent identity/emoji management beyond what the spine's
  identity registry already renders into text.
- No per-user DMs; one operations channel, like Discord today.
- No OAuth install flow — a single workspace app with static tokens is the
  v6 scope; multi-workspace distribution is out.
