# Dashboard chat

Dashboard chat is the browser transport on the v6 chat spine. The panel posts operator input to `POST /api/chat`; the dashboard submits the message to `pkg/dashchat`, which implements `chat.Backend` and routes commands through the same `pkg/chat` service used by Slack, Discord, Teams, Matrix, and Telegram.

The transport keeps the v6 guard invariant from [#7563](https://github.com/hivecommons/hive/issues/7563):

- authenticated, read-write dashboard access is required to submit messages;
- inbound text is enforced with `ioscan` before it is delivered to the spine;
- command execution still fails closed unless the dashboard user is in the chat allowlist derived from dashboard collaborators;
- outbound bot text is scrubbed before it reaches the in-memory outbox;
- the browser polls `GET /api/chat/messages?since=<seq>` every two seconds, leaving `/api/events` unchanged for status SSE.

The v6 readiness tracker is [#7683](https://github.com/hivecommons/hive/issues/7683). Its dashboard-chat evidence row is satisfied only after conformance passes and one live `!status` round trip from the panel is linked.

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
