# Notifications

Hive can push operator alerts — merges, reviews, health warnings — to one or
more channels. All channels are optional; configure only the ones you want under
the top-level `notifications` block in `hive.yaml`.

## ntfy

[ntfy](https://ntfy.sh) is a simple pub/sub notification service. Point it at the
public server or your own self-hosted instance:

```yaml
notifications:
  ntfy:
    server: https://ntfy.sh   # or your self-hosted ntfy URL
    topic: my-hive-alerts      # subscribe to this topic in the ntfy app
```

Subscribe to the same `topic` in the ntfy mobile/desktop app or via
`curl -s https://ntfy.sh/my-hive-alerts/json` to receive alerts. Choose a
hard-to-guess topic name — anyone who knows a public-server topic can read its
messages.

## Slack

Create an [Incoming Webhook](https://api.slack.com/messaging/webhooks) for the
target channel and pass its URL. Keep the URL in an environment variable rather
than committing it:

```yaml
notifications:
  slack:
    webhook: ${SLACK_WEBHOOK_URL}
```

## Discord (webhook)

Create a channel webhook (Channel → Edit → Integrations → Webhooks) and pass its
URL:

```yaml
notifications:
  discord:
    webhook: ${DISCORD_WEBHOOK_URL}
```

This is one-way alert delivery. For a two-way bot that responds to `!hive`
commands, see the separate Discord bot below.

## Discord bot (optional, two-way)

A separate optional Discord bot (in [`discord/`](../../discord/)) responds to
`!hive` commands in a channel. It is configured under the top-level `discord`
key, **not** under `notifications`:

```yaml
discord:
  bot_token: ${DISCORD_BOT_TOKEN}
  channel_id: "1234567890"
```

See [`discord/README.md`](../../discord/README.md) for bot setup and the command
surface.

## Notes

- Webhook URLs and bot tokens are secrets — supply them through environment
  variables (`${VAR}` interpolation) or a Kubernetes Secret, never inline in a
  committed `hive.yaml`.
- Multiple channels can be enabled at once; each configured channel receives the
  same alerts.
