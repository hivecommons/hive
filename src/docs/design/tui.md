# `hive tui` — a terminal dashboard for Hive (#4907)

Status: **design only.** Nothing described here is shipped. The scaffold is
proposed in [#4916](https://github.com/kubestellar/hive/issues/4916) (PR #4919,
open); every pane, every client call, and every action below is a separate
unmerged task in the [#4907](https://github.com/kubestellar/hive/issues/4907)
task graph. Read this as intent, not as behaviour.

Two things in the epic's drafted text do not match this repository, and both
are corrected here with the evidence: the `v2/` path prefix
([Paths](#paths-the-epics-v2-prefix-predates-3996)) and the assumption that
`dashboard/openapi.json` is a usable contract for the action tasks
([Contract status](#contract-status-the-spec-now-covers-writes)).
Citations are against `origin/v4` at `45a13d5`.

---

## 1. Scope

Add a full-screen terminal UI to Hive that covers the day-to-day operator loop
currently served by the web dashboard: watch the agent fleet, watch the
governor, watch token spend, and take the common actions (pause/resume an agent,
switch a model, apply an ACMM level, kick an agent now). Functional model is
**k9s** — interactive, action-capable, keyboard-driven; visual inspiration is
**btop** — dense live panes. This is **not a second implementation of Hive
logic**: it is a second *client* of the existing dashboard API, consuming the
same endpoints, the same auth token, and the same SSE stream as the web
dashboard. Hive operators are terminal people by construction — agents live in
tmux, the relay is a CLI, self-hosters are SSH'd into a box — so exposing :3001
or tunnelling it just to check governor mode is friction the web UI imposes. A
second API consumer also hardens the contract: anything the TUI cannot do
through the published spec is a gap worth an issue anyway, which is exactly what
[section 2.1](#contract-status-the-spec-now-covers-writes)
turned out to be.

**Non-goals for v1**, quoted from the epic:

- Hub-hosted hives / OAuth login flows. v1 targets self-hosted hives with the
  dashboard token. Hub auth is a follow-up epic.
- Remote terminal embedding via the ttyd WebSocket. v1 offers local tmux attach
  only (T22). Remote attach is a follow-up epic.
- Pixel parity with the web dashboard. Feature parity for the operator loop,
  not visual parity.

---

## 2. Fixed architecture decisions

Quoted verbatim from #4907. These are settled; sub-issues should not relitigate
them.

> - **Language/framework:** Go, `charmbracelet/bubbletea` + `bubbles` +
>   `lipgloss`. Stays in-family with the existing Go module, and teatest
>   gives us golden-file testing that runs in plain CI with no TTY.
> - **Delivery:** a subcommand of the existing binary (`hive tui`) if the
>   CLI entrypoint has subcommand structure; otherwise a sibling
>   `cmd/hive-tui` main in the same module. Task T1 resolves which.
> - **Data source:** the Node proxy on `:3001` (auth + SSE live there),
>   not the internal Go API on `:3002`. Base URL from `HIVE_DASHBOARD_URL`
>   (default `http://localhost:3001`), token from `HIVE_DASHBOARD_TOKEN`.
> - **Contract:** `dashboard/openapi.json` is the source of truth for every
>   client task. If an operation the dashboard visibly performs is missing
>   from the spec, the sub-issue's deliverable converts to filing a scoped
>   follow-up issue describing the gap. That is a valid completion.
> - **Testing:** every task is verifiable with `go build ./...` and
>   `go test ./...` alone. No task requires a running Hive stack, Docker,
>   or network access. Client code tests against `httptest` fixture
>   servers; rendering tests use teatest golden files.

Everything below this line is **not** part of the quoted decisions. It is what
those decisions resolve to against this repository.

### Delivery: `hivectl tui`

T1 resolves the open question in the Delivery decision. Of the two candidate
entrypoints in the module, only one has subcommand structure:

- **`hive` (`src/cmd/hive`) is the spoke daemon, not a CLI.** It parses a single
  `-config` flag, takes a process-wide singleton flock whose purpose is to
  refuse a second `hive` process (`src/cmd/hive/main.go`, `singletonLockEnv`),
  then runs the agent manager, dashboard and heartbeat for the container's
  lifetime. There is no subcommand dispatch to hang `tui` off.
- **`hivectl` (`src/cmd/hivectl` + `src/pkg/hivectl/commands`) is the
  operator-facing cobra CLI**, and it already carries the plumbing the Data
  source decision specifies: its persistent `--server` defaults to
  `http://127.0.0.1:3001` and `--token-env` defaults to `HIVE_DASHBOARD_TOKEN`
  (`src/pkg/hivectl/commands/root.go`).

So the command is **`hivectl tui`**. Note the one deviation this forces from the
Data source decision as written: the base URL comes from `--server`, whose
default is `http://127.0.0.1:3001` rather than the epic's
`HIVE_DASHBOARD_URL` / `http://localhost:3001`. Reusing hivectl's existing flag
is worth more than a second, differently-spelled way to say the same thing; a
task that wants `HIVE_DASHBOARD_URL` honoured should add it as the flag's
default source rather than as a parallel path.

### Paths: the epic's `v2/` prefix predates #3996

The epic and its sub-issues specify `v2/internal/tui/...` and `v2/docs/tui.md`.
**There is no `v2/` directory in this repository and no module at the repo
root.** The Go module is `src/` (`github.com/kubestellar/hive`), which is where
`go build ./...` and every CI test shard run from
(`.github/workflows/v2-tests.yml`, `working-directory: src`). A tree at the repo
root would sit outside the module and never be compiled or tested at all.

The prefix is not a typo — it is a stale spelling. **#3996 renamed the `v2/`
source tree to `src/`**, recorded in `src/pkg/reach/mapping.go:36-37`:

> `#3996 renamed the v2/ source tree to src/, but a merged PR's file list is`
> `immutable history: PRs merged before the rename report v2/... paths from`
> `the forge API forever`

Within `src/`, code goes under `pkg/` rather than `internal/`: every non-`main`
package in this module already lives there and the tree contains no `internal/`
directory, so `internal/tui` would introduce a second layout convention for one
package. The mapping for the whole epic:

| Epic spelling | This repository |
|---|---|
| `v2/internal/tui/app.go` | `src/pkg/tui/app.go` |
| `v2/internal/tui/client/` (T2, T4, T6, T8, T10, T13a) | `src/pkg/tui/client/` |
| `v2/internal/tui/panes/` (T3, T5, T7, T9, T11, T17, T19, T23) | `src/pkg/tui/panes/` |
| `v2/internal/tui/panes/testdata/` (golden files) | `src/pkg/tui/panes/testdata/` |
| `v2/docs/tui.md` (T0) | `src/docs/design/tui.md` — this page |
| `v2/docs/README.md` (T26) | `src/docs/design/README.md` |

Verification for every code task is `go test ./pkg/tui/...`, run from `src/`.

### Contract status: the spec now covers writes

The Contract decision names `dashboard/openapi.json` as the source of truth and
tells a sub-issue what to do when an operation is missing from it. This section
records how large that gap actually is.

**Originally measured at `45a13d5`, the spec was GET-only: 32 routes, zero
writes.** [#5023](https://github.com/kubestellar/hive/pull/5023) closed most of
that — it found the 32-vs-298 comparison had been made against
`dashboard/server.js`, a legacy Node prototype that `dashboard/README.md`
states v2 production never starts, and re-measured against the live Go server.

Current state:

| Method | Published in `dashboard/openapi.json` |
|---|---:|
| Distinct `/api` paths | 255 |
| `GET` | 131 |
| `POST` | 83 |
| `PUT` | 64 |
| `DELETE` | 14 |

The Phase 2 action tasks now have a contract to build against, and the two read
tasks that were blocked are unblocked: **`/api/agents` is in the spec** (`get`,
`post`), as is `/api/acmm/evaluation` and `/api/events`.

The Phase 2 action endpoints are all published — note the path parameter is
`{agent}`, not `{name}`: `POST /api/pause/{agent}`, `POST /api/resume/{agent}`,
`POST /api/kick/{agent}`, plus `/api/agents/{name}/kicks`.

**One endpoint remains unpublished by design.** `GET /api/health` (used by
`hivectl system health`, `commands/system.go`) sits in the parity test's
documented exception set as "not part of the client data contract", alongside
`/api/health/deep`, `/api/livez`, `/api/docs`, `/api/contribute/ws`,
`/api/terminal/assertion/renew` and the legacy `/api/v1/` catch-all. T2 should
treat it as a deliberate exception rather than a gap to fill.

`TestOpenAPISpecCoversEveryRegisteredRoute`
(`src/pkg/dashboard/openapi_route_parity_test.go`, added by #5023) now fails if
a registered route and the spec diverge in either direction, so this section
should not go stale again silently.

**What this means for sub-issues.** The epic's own escape hatch applies and
should be used deliberately rather than as a surprise: a task whose endpoint is
absent should file the scoped spec-gap issue *and* build against the endpoint
`hivectl` already calls, citing this table — not stall. The alternative reading,
that a missing spec entry blocks the task, would block six of them at once and
leave the TUI read-only. Widening `dashboard/openapi.json` to cover the write
surface is worth its own issue; it is not something any single pane task should
absorb.

---

## 3. Default layout

Header bar, a 2×2 pane grid, and a footer keybinding strip. The sketch is the
target for T3 (grid) and T5/T7/T9/T11 (pane contents); it is not a golden file.

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ hive: acme-prod            governor: ●BUSY            ws: connected  12:04:31 │
├───────────────────────────────────────┬──────────────────────────────────────┤
│ AGENTS                            [1] │ GOVERNOR                         [2] │
│  NAME          STATE     BACKEND      │  mode          BUSY                  │
│ ▸scanner       running   claude       │  queue depth   7 actionable          │
│  quality       running   copilot      │  next eval     in 3m12s              │
│  reviewer      paused    claude       │  eval interval 5m                    │
│  ux-discovery  idle      codex        │  budget        41% of daily          │
│                                       │  acmm level    L4                    │
├───────────────────────────────────────┼──────────────────────────────────────┤
│ TOKENS                            [3] │ EVENTS                           [4] │
│  AGENT         IN       OUT    COST   │  12:04:02  kick scanner (governor)   │
│  scanner       1.2M     88.1k  $4.10  │  12:03:41  pr #4919 opened           │
│  quality       410.3k   31.0k  $1.32  │  12:01:58  quality → idle            │
│  reviewer      96.7k    12.4k  $0.38  │  11:58:20  reviewer paused (operator)│
│  ─────────────────────────────────    │  11:57:03  bead bd-1042 closed       │
│  total         1.7M     131.5k $5.80  │  11:55:44  governor QUIET → BUSY     │
├───────────────────────────────────────┴──────────────────────────────────────┤
│ tab focus  j/k move  p pause  m model  K kick  A acmm  a attach  ? help  q quit│
└──────────────────────────────────────────────────────────────────────────────┘
```

Notes the sketch encodes, for the tasks that implement it:

- **Header** carries hive name, a governor mode badge, and connection state.
  Connection state is its own field because the SSE stream can drop while the
  process stays up (T13b); a TUI that silently shows stale numbers is worse than
  one that says it is disconnected.
- **Focus** is shown two ways — the `[n]` pane index and the `▸` row cursor —
  because only one pane has focus but every pane keeps its own selection.
- **Numbers are illustrative.** No pane task should treat these as expected
  output; the golden files in each pane's `testdata/` are the assertion.
- T24 owns the below-minimum-size path: at small terminals the grid is not
  shrunk, it is replaced by a single message.

---

## 4. Keybindings

| Key | Action | Scope | Task |
|---|---|---|---|
| `tab` / `shift+tab` | Cycle pane focus forward / backward | global | T3 |
| `j` / `k`, `↓` / `↑` | Move the selection within the focused pane | focused pane | T3, T11 |
| `p` | Pause or resume the selected agent | Agents pane | T15 |
| `m` | Open the model picker for the selected agent | Agents pane | T17 |
| `K` | Kick the selected agent now | Agents pane | T21 |
| `A` | Open the ACMM level overlay | global | T19 |
| `a` | Attach to the selected agent's tmux session (**local only**) | Agents pane | T22 |
| `?` | Toggle the help overlay | global | T23 |
| `q` / `ctrl+c` | Quit | global | T1 |

Conventions these bindings assume:

- **Case is meaningful.** `K` kicks and `A` opens ACMM, while `k` moves the
  cursor up and `a` attaches. The destructive-ish member of each pair is the
  shifted one, deliberately: `k` is pressed constantly during navigation and
  must never be one missed shift away from an action.
- **Actions apply to the selection in the focused pane**, never to a global
  "current agent". A key that acts on an agent while the Agents pane is not
  focused should be a no-op, not a guess.
- **Confirmation is per-risk, not uniform.** T15 specifies a confirm modal for
  pause/resume and T19 specifies *typed* confirmation for an ACMM change,
  because an ACMM level applies fleet-wide and is not obviously reversible from
  the same screen.
- `q` quits from the top level only once overlays exist; an open overlay should
  consume it (T23).

---

## 5. Testing convention

Every task is verifiable with `go build ./...` and `go test ./...` alone — no
running Hive, no Docker, no network.

- **Client code** (`src/pkg/tui/client/`) tests against `httptest` fixture
  servers. Fixtures are captured response bodies, not live calls.
- **Rendering** (`src/pkg/tui/panes/`) uses teatest golden files under
  `src/pkg/tui/panes/testdata/`, regenerated with:

  ```
  cd src && go test ./pkg/tui/panes/... -update
  ```

  `-update` is registered by `github.com/charmbracelet/x/exp/golden`
  (`golden.go:15`), which `teatest.RequireEqualOutput` calls; it is not a flag
  this repository defines. Regenerated files must be reviewed in the diff like
  any other change — a golden file updated without reading it asserts nothing.
- **Behaviour that is not layout** — key handling, quit, focus movement — should
  be asserted directly rather than through a golden file, so that a deliberate
  layout change does not force an unrelated behavioural test to be regenerated.
  The scaffold's `TestAppQuits` is the pattern: it drives the model through
  teatest so the assertion covers `tea.Quit` actually reaching the program, not
  merely being returned.
- **Golden files are terminal output.** They contain escape sequences and are
  width-sensitive, so every golden test must pin its size with
  `teatest.WithInitialTermSize`; a test that inherits a default size will
  produce a diff on someone else's machine.

---

## Related

- [#4907](https://github.com/kubestellar/hive/issues/4907) — the tracker, task
  table, and filing protocol.
- [`hivectl`](../hivectl.md) — the non-interactive client for the same API, and
  the command this TUI is a subcommand of.
- [`api-reference.md`](../api-reference.md) — the dashboard API as documented
  for humans; see [section 2.1](#contract-status-the-spec-now-covers-writes)
  for how much of it `dashboard/openapi.json` actually publishes.
