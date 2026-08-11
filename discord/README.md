# Hive Discord bot

An optional Discord bot that lets operators interact with a hive from a Discord
channel — check status, kick, pause, and resume agents — and receive alerts. It
talks to the hive's dashboard API; it does **not** need cluster access.

This is separate from the one-way **`notifications.discord.webhook`** channel
(see [`v2/docs/notifications.md`](../v2/docs/notifications.md)). The webhook only
pushes alerts; this bot is two-way.

## Setup

1. Create a Discord application and bot at
   <https://discord.com/developers/applications>, and invite it to your server
   with permission to read and send messages in the target channel(s).
2. Install and run:

   ```bash
   cd discord
   npm install
   npm start
   ```

3. Configure via environment variables (required unless noted):

   | Variable | Purpose |
   |----------|---------|
   | `DISCORD_BOT_TOKEN` | Bot token from the Discord developer portal. |
   | `DISCORD_CHANNEL_PRIMARY` | Channel ID the bot listens in for commands. |
   | `DISCORD_CHANNEL_ALERTS` | Optional channel ID for alert posts. |
   | `HIVE_DASHBOARD_URL` | Base URL of the hive dashboard API the bot drives. |
   | `HIVE_METRICS_DIR` | Optional path to the hive metrics dir for local reads. |
   | `HIVE_PROJECT_CONFIG` | Optional path to the project config for agent names/prefix. |

   The command prefix defaults to `!` and can be overridden with
   `command_prefix` in the project config.

## Commands

Send these in the primary channel (default prefix `!`):

| Command | Aliases | What it does |
|---------|---------|--------------|
| `!status` | `!s`, `!st` | Fleet status summary from the dashboard. |
| `!governor` | | Current governor mode and cadences. |
| `!kick <agent> [prompt]` | `!k <agent>` | Kick an agent (optionally with a prompt). |
| `!pause <agent>` | `!p <agent>` | Pause an agent. |
| `!resume <agent>` | `!r <agent>` | Resume a paused agent. |
| `!help` | `!h`, `!?` | Command usage. |

Unknown agents and unreachable-dashboard conditions are reported back in-channel.

## Security

The bot token and any dashboard auth token are secrets — supply them via
environment variables, never commit them. Restrict the bot to a private
operator channel; anyone who can post in `DISCORD_CHANNEL_PRIMARY` can kick,
pause, and resume agents.
