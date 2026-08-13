# Inception — turning an idea into a scaffolded project

Inception is the ACMM **L1 ("Inception")** workflow. It drives the `brainstorm`
agent through a fixed phase sequence that turns a raw idea (or an existing
repo) into structured knowledge-base facts and a downloadable project
scaffold. This guide covers the operator-facing workflow, the API surface
behind it, and how to configure the agent that powers it.

This guide documents only what the code in `pkg/dashboard/inception_handlers.go`
and `pkg/knowledge/inception.go` actually does — see those files for
implementation detail.

## When to use it

Use inception when you are standing up a brand-new hive at ACMM level 1 and
want a starting point beyond an empty repo: a `README.md`, `AGENTS.md`,
`CLAUDE.md`, `CONTRIBUTING.md`, CI workflows, a `.gitignore`, and
language-specific stubs (Go, Python, TypeScript, JavaScript, Rust, Java, or
shell), generated from a short description of the project.

Two modes are supported:

- **Greenfield** — you have an idea but no code yet. Inception interviews you
  and produces a scaffold from scratch.
- **Brownfield** — you have an existing repo URL. Inception scans it and
  produces facts/amendments rather than a from-scratch skeleton.

## Prerequisites

- The hive must be running the `brainstorm` agent (present by default in the
  `level-1` ACMM pack — see `pkg/config/packs/level-1.yaml`).
- You must be signed in with the **owner** role. Every state-changing
  inception endpoint calls `requireOwnerRole`, which checks the
  `X-Hive-Role` header and the hub's owner-verification proof
  (`X-Hive-Proxy-Auth`). Read-only endpoints (state, scaffold preview,
  ideation-facts, download, has-files) do not require owner role.
- Only one inception run can be in progress at a time. Starting a new one
  while `phase != complete` returns an error unless you pass `force: true`,
  which resets the existing run first.

## The phase sequence

Inception state moves through a strict, server-enforced state machine
(`InceptionPhase` in `pkg/knowledge/types.go`). Each phase transition is
gated — the corresponding handler rejects the call if the engine isn't in the
expected phase:

```
capture → clarify → structure → scaffold → complete
```

| Phase | What happens | Who acts |
|---|---|---|
| `capture` | You submit an idea (or a repo URL for brownfield). The engine seeds a `FactIdea` KB fact and creates the inception state. The dashboard kicks the `brainstorm` agent, which must generate 5–7 clarification questions as beads. | Operator starts; agent generates questions |
| `clarify` | Questions are posted to the dashboard (`POST /api/inception/questions` records them and advances the phase). You answer them. | Operator answers |
| `structure` | Your answers are submitted (`POST /api/inception/answer`); the agent is re-kicked with the idea + answers and must create fact beads — 1 vision, 1 constitution, 2+ requirements, plus constraints/stakeholders/acceptance criteria as needed. You record them (`POST /api/inception/facts`), which writes each fact to the inception wiki vault and advances to `scaffold`. | Agent produces facts; operator records them |
| `scaffold` | `GET /api/inception/scaffold` (or `/download`) generates the project scaffold from the recorded facts — README, AGENTS.md, CLAUDE.md, CONTRIBUTING.md, CI workflows, `.gitignore`, and language-specific stubs inferred from the constitution fact (falling back to language cues in the idea text). | Operator reviews the generated files |
| `complete` | `POST /api/inception/approve` marks the run finished and re-pauses the `brainstorm` agent so the governor doesn't kick it again with generic ideation prompts. | Operator approves |

Notes on the state machine, verified from `pkg/knowledge/inception.go`:

- `SetQuestions` only succeeds from `capture` and moves to `clarify`.
- `SubmitAnswers` is accepted in both `clarify` and `structure` (the agent
  can race ahead of the user submitting answers); it only advances the phase
  when called from `clarify`.
- `RecordFacts` only succeeds from `structure` and moves to `scaffold`.
- `AdvanceToComplete` (approve) only succeeds from `scaffold` (or is a no-op
  if already `complete`).
- `ProduceScaffold` (the scaffold/download handlers) can be called from any
  phase where facts exist — it doesn't itself change phase.
- Starting a new inception (`Start` / `StartBrownfield`) is rejected unless
  the previous run reached `complete`, or `force: true` is passed.

## Operator workflow (dashboard)

1. Open the Inception panel on the dashboard. With no active run, it shows a
   mode selector (idea text box for greenfield, repo URL box for
   brownfield).
2. Submit an idea (greenfield) or a repo URL (brownfield). This calls
   `POST /api/inception/start` or `POST /api/inception/scan`, which also
   kicks the `brainstorm` agent (unpausing it first if the governor had
   paused it).
3. Wait for the agent to post 5–7 clarification questions as beads. The
   dashboard polls `GET /api/inception/state` (every 3s while the panel is
   open) and renders the questions once `POST /api/inception/questions` has
   recorded them.
4. Answer the questions in the dashboard form; this posts
   `POST /api/inception/answer`. The agent is re-kicked (via `SendKick`, not
   a full restart, to avoid losing its clarify-phase context) with the idea,
   phase, and your answers injected via `${INCEPTION_*}` template variables.
5. Wait for the agent to create fact beads, then record them via
   `POST /api/inception/facts`. This writes each fact as a Markdown file
   with YAML frontmatter into an `inception-wiki` vault directory and
   connects that vault as a knowledge source for all agents.
6. Review the generated scaffold (`GET /api/inception/scaffold`) or download
   it as a zip (`GET /api/inception/download`).
7. Approve (`POST /api/inception/approve`) to mark the run complete. This
   also re-pauses `brainstorm` so the L2+ governor doesn't restart it with
   unrelated ideation prompts.
8. If anything goes wrong, `POST /api/inception/reset` clears the in-memory
   state and state file (but intentionally leaves prior wiki facts visible
   in the KB until a *new* inception writes fresh ones).

Optional wiki-management calls, usable at any point once an inception has
produced wiki files:

- `GET /api/inception/has-files` — whether the inception wiki has any `.md`
  files, used by the dashboard to decide whether to show wiki-related
  controls.
- `PUT /api/inception/wiki-name` — rename the inception wiki vault
  (persisted into inception state).
- `POST /api/inception/import` — upload a zip of Markdown files
  (10 MiB max, 1 MiB per file, `.md` only, path-traversal guarded) to seed
  or supplement the inception wiki without going through the agent.

## API reference

All 14 endpoints live under `/api/inception/*`, registered in
`pkg/dashboard/api.go` and implemented in `pkg/dashboard/inception_handlers.go`.
"Owner required" means the handler calls `requireOwnerRole` before doing
anything else.

| Method | Path | Owner required | Purpose |
|---|---|---|---|
| `POST` | `/api/inception/start` | Yes | Begin a greenfield inception from idea text. `force: true` resets any in-progress run first. |
| `POST` | `/api/inception/scan` | Yes | Begin a brownfield inception against a repo URL (`https://` or `http://` only). `force: true` resets first. |
| `GET` | `/api/inception/state` | No | Current inception state (`null`/`active: false` if none in progress). |
| `POST` | `/api/inception/questions` | Yes | Record clarification questions generated by the agent; advances `capture` → `clarify`. |
| `POST` | `/api/inception/answer` | Yes | Submit answers to clarification questions; advances `clarify` → `structure` (accepted in both phases). |
| `POST` | `/api/inception/facts` | Yes | Record structured KB facts produced by the agent; advances `structure` → `scaffold`. |
| `GET` | `/api/inception/scaffold` | No | Generate and return the scaffold file set (does not change phase). |
| `POST` | `/api/inception/approve` | Yes | Advance `scaffold` → `complete`; re-pauses the `brainstorm` agent. |
| `POST` | `/api/inception/reset` | Yes | Clear inception state and the persisted state file; re-pauses `brainstorm`. |
| `GET` | `/api/inception/ideation-facts` | No | List recorded ideation facts (falls back to the inception engine's own fact gathering if the knowledge API has none). |
| `GET` | `/api/inception/download` | No | Download the scaffold as a zip, named from the idea slug. |
| `GET` | `/api/inception/has-files` | No | Whether the inception wiki vault has any `.md` files. |
| `PUT` | `/api/inception/wiki-name` | Yes | Rename the inception wiki vault (max 80 characters). |
| `POST` | `/api/inception/import` | Yes | Upload a zip of Markdown files into the inception wiki (10 MiB max upload, 1 MiB max per file). |

See [Dashboard API reference](api-reference.md) for these endpoints in the
project-wide route index, and the [OpenAPI spec](../../dashboard/openapi.json)
for a machine-readable version.

## Template variables

The `brainstorm` agent's kick prompt (`pkg/policies/defaults/brainstorm-advisory.md`)
is built by the scheduler (`pkg/scheduler/scheduler.go`) with these
inception-specific substitutions, documented in full in
[`policies/README.md`](../policies/README.md):

| Variable | Value |
|---|---|
| `${INCEPTION_IDEA}` | Current inception idea text. Empty when no run is active — the template then falls through to "Normal Ideation Mode". |
| `${INCEPTION_PHASE}` | Current phase (`capture`, `clarify`, `structure`, `scaffold`, or `complete`). |
| `${INCEPTION_MODE}` | `greenfield` or `brownfield`. |
| `${INCEPTION_ANSWERS}` | Markdown-rendered Q&A, injected during the `clarify`/`structure` kick. |
| `${INCEPTION_SLUG}` | The idea/repo slug, used as the bead `--external-ref` (`inception/<slug>`) so the engine can distinguish inception beads from ordinary ideation beads on reap. |
| `${INCEPTION_REPO_URL}` | The scanned repo URL, brownfield mode only. |

The template branches on `${INCEPTION_PHASE}` to give the agent
phase-specific instructions (generate questions in `capture`, review answers
in `clarify`, produce fact beads in `structure`, emit scaffold content in
`scaffold`). When `${INCEPTION_IDEA}` is empty, the agent falls back to
"Normal Ideation Mode" — general feature/architecture/strategy proposals
recorded as advisory beads, unrelated to any specific inception run.

## Configuring the brainstorm agent

The `brainstorm` agent is defined per ACMM pack, e.g.
`pkg/config/packs/level-1.yaml`. Two fields control how it participates in
inception:

- **`on_demand: true`** — the agent is not kicked on a fixed cadence like
  other agents; it is driven entirely by the inception workflow (via
  `kickBrainstorm`/`sendKickBrainstorm` in `inception_handlers.go`) and
  direct user queries. This is why `approve` and `reset` explicitly re-pause
  the agent afterward — without `on_demand` semantics the governor would
  otherwise pick it back up on its normal eval cycle.
- **`stale_timeout: 7200`** (seconds) — how long the agent's session may sit
  idle before the supervisor considers it stale. Set this higher than the
  time you expect a real user to take answering clarification questions,
  since the session must stay alive across the `capture` → `clarify` →
  `structure` round trip.

`clear_on_kick: true` is also set — each inception kick clears prior CLI
context (`/clear`) before sending the new prompt, so multi-turn inception
state doesn't leak stale context from a previous kick.

See [Agent configuration](agent-configuration.md) for the full field
reference and [ACMM policy matrix](acmm-policy-matrix.md) for how `brainstorm`
evolves in mode and responsibility from L1 onward.

## Related documents

- [ACMM policy matrix](acmm-policy-matrix.md) — L1 agent roster and policy
  mode for `brainstorm`/`guide`.
- [`policies/README.md`](../policies/README.md) — full template variable
  reference, including the `${INCEPTION_*}` family.
- [Dashboard API reference](api-reference.md) — project-wide endpoint index.
- [Knowledge system design](design/knowledge-system.md) — how the inception
  wiki vault fits into the broader knowledge-base layering.
