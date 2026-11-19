# Inception — Inception (spek) sessions

In the dashboard, **Inception (spek)** is the interactive way to turn an idea
(or an existing repo) into specs, plans and a starting project: named, shared
CLI-agent terminal sessions that open in the hive's shared spek workspace (a
checkout of the project repo with its spek skills installed). Everyone who can
see a session watches it live, one person at a time holds the keyboard, and
`read` users only watch. It is on by default (`dashboard.sessions.enabled`;
`HIVE_SESSIONS_ENABLED=false` or `enabled: false` hides it). See
[Inception (spek)](dashboard-sections/workflow-and-agents.md#inception-spek)
and the [Sessions API](api-reference/sessions.md). An Inbox recording can
seed a session directly (**🔗 Share ▾ → Hive → spek…**, see
[Inbox and voice sources](inbox-voice-sources.md#from-recording-to-an-inception-spek-session)).

A new session does not wait on the CLI's first-run questions. The hive answers
the vendor CLI's startup screens (first-run notice, trust for the session's
folder, onboarding) and then delivers what was sent to the session, so spek
starts on the request by itself. The pane header shows "Getting the CLI
ready…" and then "Started". It never answers a permission prompt and never
signs in: if the CLI needs a login the header says "Needs sign-in", and if it
stops at any other screen the header names it and the terminal is yours. See
[Startup auto-answer](api-reference/sessions.md#startup-auto-answer);
`dashboard.sessions.autostart: false` or `HIVE_SESSIONS_AUTOSTART=false` turns
it off.

## Retired: the inception API, watcher and brainstorm inception mode

The old ACMM L1 inception workflow (a fixed `capture → clarify → structure →
scaffold → complete` state machine that drove the `brainstorm` agent and
generated a downloadable project scaffold) was retired in favour of Inception
(spek) sessions. Its dashboard wizard and Campaigns tab were removed first.
Now the backend is gone too:

- **`/api/inception/*`**: all 14 routes (`start`, `scan`, `state`,
  `questions`, `answer`, `facts`, `scaffold`, `approve`, `reset`,
  `ideation-facts`, `download`, `has-files`, `wiki-name`, `import`) answer
  **`410 Gone`** for one release, with a JSON body:

  ```json
  {"ok": false, "error": "The inception API was retired. ...", "replacement": "/api/sessions", "docs": "https://github.com/hivecommons/hive/blob/v5/src/docs/inception.md"}
  ```

  Nothing is read or changed. A later release removes the routes, and they
  then answer `404`. Scripts that drove the workflow (`curl` against
  `/api/inception/start` and friends) should start an Inception (spek)
  session instead (`POST /api/sessions`, see the
  [Sessions API](api-reference/sessions.md)).
- **The inception watcher** that turned brainstorm beads into inception
  questions and facts no longer runs.
- **Inception approval as a run admission path** is gone. Spektacular runs are
  admitted through run triage; see [Runs](runs.md).
- **The brainstorm agent's inception mode** is gone. `brainstorm` is
  ideation-only: its kick template (`brainstorm-advisory.md`) records feature,
  architecture, strategy, integration and DX proposals as advisory beads. It
  is still on-demand and parked at boot. The `${INCEPTION_IDEA}`,
  `${INCEPTION_PHASE}`, `${INCEPTION_MODE}`, `${INCEPTION_ANSWERS}`,
  `${INCEPTION_SLUG}` and `${INCEPTION_REPO_URL}` template variables always
  render empty for one release, so a custom template that still names them
  keeps rendering. See [`policies/README.md`](../policies/README.md).

### What is kept

- **Knowledge facts from past inception runs.** The ideation facts those runs
  recorded (vision, constitution, requirements, constraints, stakeholders,
  acceptance criteria) live in the knowledge store and as Markdown files in
  the `/data/inception-wiki` vault. Nothing deletes them. On boot Hive still
  reconnects `/data/inception-wiki` as a knowledge vault, under the name the
  run saved (`inception-wiki` by default), so agents keep reading those facts.
- **Files on the PVC.** `/data/inception/state.json`, `/data/inception-wiki/`
  and `/data/inception/campaigns/` are left in place. The campaign archives
  are also bundled into the one-time export described below.
- **Ideation fact types** (`idea`, `vision`, `constitution`, `requirement`,
  `constraint`, `stakeholder`, `acceptance`) remain valid knowledge fact types.

## Retired campaigns export

Campaigns (`/api/campaigns`, the resume/release/revise/delete actions), campaign
Jam and campaign recheck were retired in favour of
[Inception (spek) sessions](dashboard-sections/workflow-and-agents.md#inception-spek).
Their endpoints are gone, and `runs.spektacular.recheck` in `hive.yaml` is
accepted but ignored with a one-line deprecation warning.

So nothing is lost, the first start after the upgrade writes a one-time,
read-only export when either of these exists:

- `/data/inception/campaigns/` (archived Inception sessions: each campaign's
  `state.json` and wiki snapshot), and
- `campaign-jam.json` in the metrics directory (`data.metrics_dir`, falling back
  to `data.logs_dir`, then `data.agents_dir`): Jam threads, suggestions, polls,
  decisions and spec revisions.

The export is `/data/exports/campaigns-<YYYY-MM-DD>.tar.gz`, written with mode
`0600` (directory `0700`). Hive logs its path once, at `WARN`, as
`retired campaigns exported`. Inside the tarball, archive files keep their
`inception/campaigns/<id>/...` paths, the Jam store is `campaign-jam.json`, and
`inception/state.json` is included when present. Once any
`campaigns-*.tar.gz` exists in `/data/exports/`, Hive does not write another.

The original files stay where they are for one release and are never deleted
by this change. Knowledge-base facts recorded by past inception runs (the
`inception-wiki` vault and the knowledge store) are separate from these files
and stay as they are. To read the export:

```sh
tar -tzf /data/exports/campaigns-*.tar.gz
```

## Related documents

- [Inception (spek)](dashboard-sections/workflow-and-agents.md#inception-spek)
  and the [Sessions API](api-reference/sessions.md).
- [ACMM policy matrix](acmm-policy-matrix.md) — L1 agent roster and policy
  mode for `brainstorm`/`guide`.
- [`policies/README.md`](../policies/README.md) — template variable reference,
  including the deprecated `${INCEPTION_*}` family.
- [Knowledge system design](design/knowledge-system.md) — how the inception
  wiki vault fits into the broader knowledge-base layering.
