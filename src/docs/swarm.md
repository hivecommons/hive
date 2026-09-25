# Swarm mode

Swarm mode focuses the hive on one repository for a bounded window.

## Discord

When Discord notifications are configured, Hive announces swarm starts and endings in the configured channel. Start messages include the swarm display name, repository, scheduled end time, and prep agents. End and expiry messages include the reason, issues closed, PRs merged, and plain GitHub logins for participants without Discord mentions.

## Theming

Operators can customize the default event name, call-to-arms, and leaderboard title with the `swarm:` config block or `HIVE_SWARM_EVENT_NAME` / `HIVE_SWARM_CALL_TO_ARMS`. Per-repo themes can be seeded with `swarm.themes`, `HIVE_SWARM_THEMES_JSON`, or updated by owners through `PUT /api/swarm/themes` with `{ "repo": "owner/name", "theme": { "event_name": "Gondor Calls for aid!", "call_to_arms": "All hands on deck!", "leaderboard_title": "Record of triumph" } }`. The selected theme is stored on each swarm history record and used in dashboard and Discord announcements.

## Leaderboard

The dashboard exposes a stable `#swarm-leaderboard` anchor and a read-only public JSON endpoint at `/api/leaderboard/swarm`. The endpoint returns completed swarm history, repo leaderboard, top players, and achievement metadata only; it does not expose tokens, prompts, or private agent state.

## Scoring and objectives

Swarm score rewards software-engineering outcomes: closed issues, merged PRs, completed Spektacular/spec-plan-implement work, local-model-attributed PRs, and completed SDLC objectives. The default objective checklist covers spec written, plan approved, review done, and docs updated. Owners can update the active checklist with `PATCH /api/swarm/objectives`. Achievements highlight participation, local-model work, spek work, and SDLC completion instead of raw inference volume.
