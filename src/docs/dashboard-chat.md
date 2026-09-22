# Dashboard chat

Dashboard chat is the browser transport on the v6 chat spine. The panel posts operator input to `POST /api/chat`; the dashboard submits the message to `pkg/dashchat`, which implements `chat.Backend` and routes commands through the same `pkg/chat` service used by Slack, Discord, Teams, Matrix, and Telegram.

The transport keeps the v6 guard invariant from [#7563](https://github.com/hivecommons/hive/issues/7563):

- authenticated, read-write dashboard access is required to submit messages;
- inbound text is enforced with `ioscan` before it is delivered to the spine;
- command execution still fails closed unless the dashboard user is in the chat allowlist derived from dashboard collaborators;
- outbound bot text is scrubbed before it reaches the in-memory outbox;
- the browser polls `GET /api/chat/messages?since=<seq>` every two seconds, leaving `/api/events` unchanged for status SSE.

The v6 readiness tracker is [#7683](https://github.com/hivecommons/hive/issues/7683). Its dashboard-chat evidence row is satisfied only after conformance passes and one live `!status` round trip from the panel is linked.
