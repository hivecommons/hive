# Advisor lane phase 1: roles, records, and the Claude Code adapter (#9722)

Phase 1 of the advisor lane epic (#9638, design record
`src/docs/design/advisor-lane.md`): a second, independently chosen model reads
each turn an advised hub-launched agent finishes and can object before the
agent takes its next step.

- A `model_roles` map in `hive.yaml` — named backend/model/reasoning-effort
  triples referenced as `@<role>` — and an `advisor` block with a fleet
  default (enabled, model or `@role`, standing instructions, daily token
  budget, timeout, consecutive-block limit) plus a per-agent override that may
  change any part of it or opt the agent out.
- The advisor evaluator rides the trajectory lane's reviewer endpoint
  resolution and fail-open posture; reviews time out, budget-cap and downgrade
  runaway blockers, and every review — skips included — produces one record.
- Advisor records surface on the agent's dashboard card (🗣 advisor) and over
  `GET /api/advisor/records`, behind the dashboard's existing role checks.
- The `hive advisor-hook` turn-end hook and the Claude Code `Stop` hook,
  rendered into the settings argument at launch — a projection, never an edit
  of the agent's own configuration. Inbound transcripts pass
  `ioscan.EnforceInput`; outbound advisor text is canary/secret scrubbed; the
  hook carries no new credential (loopback endpoint, socket-UID identity).
