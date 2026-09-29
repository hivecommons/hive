# Dashboard chat

Dashboard chat is the browser transport on the v6 chat spine. The panel posts operator input to `POST /api/chat`; the dashboard submits the message to `pkg/dashchat`, which implements `chat.Backend` and routes commands through the same `pkg/chat` service used by Slack, Discord, Teams, Matrix, and Telegram.

The transport keeps the v6 guard invariant from [#7563](https://github.com/hivecommons/hive/issues/7563):

- authenticated, read-write dashboard access is required to submit messages;
- inbound text is enforced with `ioscan` before it is delivered to the spine;
- command execution still fails closed unless the dashboard user is in the chat allowlist derived from dashboard collaborators;
- outbound bot text is scrubbed before it reaches the in-memory outbox;
- the browser polls `GET /api/chat/messages?since=<seq>` (every 2 s while the panel is open, every 10 s while it is closed, never while the tab is hidden), leaving `/api/events` unchanged for status SSE;
- `!`-prefixed text is never answered by the dashboard's local intents (`pkg/dashboard/chat_commands.go`); every command reaches the spine's allowlist and role checks. a command the spine refuses (author not allowlisted, or an empty allowlist) is shown in the panel as a visible `❌ … refused` line via the `chat.CommandRefuser` hook — the decision stays in the spine, dashboard chat only renders it. `src/pkg/dashboard/chat_conformance_v6_test.go` fails if a local intent answers a `!` command, if a started bot replies to a non-allowlisted author, or if the refusal is not visible ([#9136](https://github.com/hivecommons/hive/issues/9136)).

The v6 readiness tracker is [#7563](https://github.com/hivecommons/hive/issues/7563). Its dashboard-chat evidence row is satisfied only after conformance passes and one live `!status` round trip from the panel is linked.

## One shared channel

The dashboard panel is a **shared channel**, not a private conversation: every
dashboard reader (`RoleRead` and up) polls the same in-memory outbox, exactly as
every member of a Slack or Discord channel sees the same bot. Bot replies carry
no target user, so a per-viewer feed is not expressible on the spine. Each
outbox entry names its author (`author_id`) and each poll names the caller
(`viewer`, from the authenticated request). Only a line whose `author_id`
equals `viewer` is rendered as the viewer's own; anyone else's — or an
unattributed line — is a labelled peer line kept out of the viewer's own
conversational context (the `history` posted back with `POST /api/chat`), so one
operator's `!runs reject …` never shows up in another's panel as their own words
([#9135](https://github.com/hivecommons/hive/issues/9135)).

## Poll cursor contract

`GET /api/chat/messages?since=<seq>` returns an envelope, not a bare list
(`Poll`, `src/pkg/dashchat/bot.go:50`; `handleChatMessages`,
`src/pkg/dashboard/api.go:6669`):

```json
{"messages":[…], "next": 42, "epoch": "<per-process id>", "gap": false, "viewer": "alice"}
```

- `seq` is a per-process counter, so a cursor is only meaningful inside the
  `epoch` that minted it. The browser persists `{epoch, seq}` alongside the
  transcript (`hive-chat-cursor-v1`), so a reload resumes where it left off
  instead of replaying the retained outbox with fresh unread badges.
- A new `epoch` (hive restart) or a `next` below the cursor resets the cursor
  to `0`, prints a restart notice, and re-polls at once; before this, the
  browser silently discarded the first N replies of the new process.
- The outbox is a 200-entry ring. `gap: true` means entries newer than `since`
  were already evicted; a browser that was tracking the channel renders a gap
  marker rather than presenting the retained tail as complete.
- Non-2xx responses and network failures back off exponentially (base cadence
  × 2ⁿ, capped at 60 s) and a 2xx resets the backoff.

## Run decisions

The shared chat spine exposes the same run controls to the dashboard panel and
external chat backends:

- `!runs` lists active staged runs with key, stage, wait target, and age.
- `!runs <key>` shows one run, including reported receipt/artifact links.
- `!runs approve <key>` approves the held run checkpoint using the compact
  `/api/runs/{key}/checkpoint` payload and its lease-generation fence.
- `!runs reject <key> <reason>` rejects the held run checkpoint using the same
  fenced payload and records the operator's reason in the chat transcript.

When a run reaches `waiting_on=human`, the spine posts a one-line checkpoint
prompt from `GET /api/runs/{key}/checkpoint`: `Run <key> stage <stage> gen
<gen> needs a decision: <bounded summary>. Reply approve or reject <reason>.
Full artifact: <dashboard link>.` An allowlisted owner with exactly one pending
run checkpoint may reply with plain `approve` or `reject <reason>`. If more
than one run is pending for that author, the bot lists the run keys and requires
the explicit `!runs approve <key>` or `!runs reject <key> <reason>` form. No
other plain language is interpreted as a run decision, and stale generations are
refused by the dashboard endpoint.
