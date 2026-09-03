# hivectl

`hivectl` is the non-interactive command-line client for a running Hive
dashboard API. The `hive` binary runs the service; `hivectl` inspects and
operates it.

## Quick start

### Getting the binary

On a host installed with `bin/hive-podman-setup.sh` there is nothing to do:
the installer extracts `hivectl` from the Hive image onto the host —
`~/.local/bin/hivectl` rootless, `/usr/local/bin/hivectl` rootful — and
refreshes it on every re-run, so it always matches the running hive. This is
the supported route on image-based hosts (Fedora Silverblue/Bluefin, RHEL
image mode), which have no Go toolchain and a read-only `/usr` (#5646).

The same extraction works by hand against any pulled Hive image. The image
carries the binary as cargo at `/usr/local/share/hive/hivectl` (deliberately
off the container's PATH — it is a client of the dashboard API, not part of
the runtime; run it on the host, never inside the container):

```bash
podman create --name hivectl-extract ghcr.io/hivecommons/hive:stable
podman cp hivectl-extract:/usr/local/share/hive/hivectl ~/.local/bin/hivectl
podman rm hivectl-extract
```

Contributors working from a checkout build it from source instead:

```bash
go build -o bin/hivectl ./cmd/hivectl
```

### First commands

```bash
# Point at a server (default: http://127.0.0.1:3001) and provide the token
# via an environment variable, never as a flag value. The value must match the
# server's dashboard token — see "Generating and rotating HIVE_DASHBOARD_TOKEN"
# in env-vars.md (generate with: openssl rand -hex 32).
export HIVE_DASHBOARD_TOKEN="..."
hivectl system status

# On a Podman-standalone host, talk to the gateway's published port — the
# installer prints the exact URL at the end of its run:
hivectl --server http://127.0.0.1:3001 system status

# Enroll a repository in clusterless, zero-secret Hive lite mode.
hivectl enroll kubestellar/hive
```

(From a source checkout the binary is `bin/hivectl` instead.)

## Running against a local Hive

Read-only and operational commands (`system`, `agent list/get/logs/kick/...`,
`bead`, `governor get`, `observe`) work against a plain local Hive. A few
commands need extra setup that a hand-run instance may lack:

- **knowledge writes** (`create/update/delete/import`) require the knowledge
  layer to have a wiki `url` in `hive.yaml` — Hive does not bundle llm-wiki, so
  a `path`-only layer returns "no configured endpoint".
- **config writes** (`agent prompt/model/backend/pipeline set`, `governor
  *-set`) write to the server's `/data/policies` dir, which must be writable
  (e.g. a container or Linux run); some take effect only after reload/restart.
- **auth**: set `dashboard.auth_token` (or `HIVE_DASHBOARD_TOKEN`) and pass it
  via `--token-env`. Bearer auth is rejected on direct-route spokes
  (`authorized_users` set with `hub_proxied: false`), which require a per-user
  session instead.

## Global options

| Flag | Default | Description |
| --- | --- | --- |
| `--server` | `http://127.0.0.1:3001` | Dashboard API base URL |
| `--token-env` | `HIVE_DASHBOARD_TOKEN` | Env var holding the auth token |
| `--timeout` | `30s` | Per-request (or stream) timeout |
| `-o, --output` | `table` | Output format: `table`, `json`, `yaml`, `jsonl` |

Results go to stdout, errors to stderr. Exit codes:

| Code | Meaning |
| ---: | --- |
| 0 | Success |
| 1 | Local/unclassified failure |
| 2 | Invalid arguments or missing `--yes` |
| 3 | Authentication/authorization failure |
| 4 | Connection or timeout failure |
| 5 | Hive API/server-side error |

## Commands

### system — runtime state

```bash
hivectl system status -o json
hivectl system health
hivectl system version
hivectl system snapshot
hivectl system events --follow --timeout 2m -o jsonl   # SSE stream as JSONL
```

`events` is a stream, so `--follow` is required and output must be `jsonl`.

### agent — manage and operate agents

```bash
hivectl agent list
hivectl agent get quality -o yaml
hivectl agent logs quality --lines 200
hivectl agent kick quality --prompt "Review the work list"
hivectl agent pause|resume|restart quality
hivectl agent export quality -o yaml            # portable AgentDefinition
hivectl agent import --file agent.yaml --preview # validate without applying
hivectl agent delete ux-discovery --yes
```

Configure an agent (prompt template, model/backend, pipeline toggles):

```bash
hivectl agent prompt get quality --raw
hivectl agent prompt set quality --file quality.md
hivectl agent model-set quality claude-sonnet-4-6
hivectl agent backend-set quality claude
hivectl agent pipeline-set quality --file pipeline.yaml   # map of step: bool
```

### knowledge — inspect and maintain knowledge

```bash
hivectl knowledge search "user journey" --limit 20
hivectl knowledge list --layer project        # layer: personal|project|org|community
hivectl knowledge get project create-project
hivectl knowledge stats
hivectl knowledge export > hive-knowledge.md

hivectl knowledge create --file fact.yaml     # title + body required
hivectl knowledge update project create-project --file update.yaml
hivectl knowledge delete project create-project --yes
```

### bead — work items

```bash
hivectl bead list --agent quality -o json
hivectl bead create --agent quality --title "Review UX journey" --type advisory --priority 2
hivectl bead reset --agent quality --reason "replace stale findings" --yes
```

### governor — scheduler configuration

```bash
hivectl governor get -o yaml
hivectl governor thresholds-set --file thresholds.yaml
hivectl governor budget-set --file budget.yaml
hivectl governor repos-set --file repos.yaml
hivectl governor agent-add ux-discovery --backend copilot --model claude-sonnet-4-6
hivectl governor agent-remove ux-discovery --yes
```

### observe — read-only metrics

```bash
hivectl observe tokens -o json
hivectl observe audit
hivectl observe history
hivectl observe timeline
hivectl observe trends --range week        # or --hours 12 (1-720); not both
```

### login / logout — hold a per-user session from the terminal

```bash
hivectl login                                    # against the default --server
hivectl --server https://hive.example.com login  # against a specific hive
hivectl logout
```

A hive dashboard identifies callers two ways, and they are not
interchangeable: self-hosted hives accept the shared bearer token
(`HIVE_DASHBOARD_TOKEN`), while hub-hosted hives and spokes with an
`authorized_users` allowlist accept only a **per-user session** — the
`hive_session` cookie the GitHub device-flow login mints, resolved on every
request against the live allowlist. `hivectl login` runs that device flow from
the terminal ([#5651](https://github.com/kubestellar/hive/issues/5651)): it
prints a one-time code and `https://github.com/login/device`, waits for you to
approve there, and caches the minted session so every subcommand — and
`hivectl tui` — presents it automatically.

Details worth knowing:

- **The login proves identity only.** It requests no OAuth scope at all, and
  your role (owner or viewer) is re-resolved by the hive on every request —
  caching a session caches who you are, never what you may do.
- **The cache** lives at `$XDG_CONFIG_HOME/hive/sessions.json` (default
  `~/.config/hive/sessions.json`), owner-only (0600), keyed by dashboard URL
  so one operator can hold sessions for several hives. The credential is never
  printed.
- **Precedence:** an explicitly exported `HIVE_DASHBOARD_COOKIE` always wins
  over the cache. The token lane is independent — both credentials are
  presented when both exist, and the hive honours whichever lane its
  deployment implements.
- **A hive runs one device flow at a time.** If another operator's login
  starts while yours is pending, yours is replaced and `hivectl login` says
  so — just run it again.
- **On an allowlist spoke, an unauthorized GitHub account is refused** with
  the server's own explanation naming the account; nothing is cached.
- **Sessions expire** (30 days server-side). When a cached session stops
  working, hivectl says to run `hivectl login` again instead of showing a bare
  401.
- `hivectl logout` ends the session server-side via the existing endpoint and
  removes the cached credential; the cache is cleared even when the hive is
  unreachable.

### tui — live terminal dashboard

```bash
hivectl tui
```

A full-screen, keyboard-driven view of the fleet — agents, the governor,
token/cost spend, and recent activity — over the same dashboard API the
non-interactive subcommands above use. It is not a second Hive runtime, just
another client of the API: same auth token, same endpoints, same SSE stream
the web dashboard consumes. Requires a real terminal.

This is the v1 delivery of the `hive tui` epic
([#4907](https://github.com/kubestellar/hive/issues/4907)); the design
rationale and the fixed architecture decisions behind it are recorded in
[`src/docs/design/tui.md`](design/tui.md).

#### Launch and endpoint selection

`hivectl tui` takes no flags — it is a bare subcommand. In particular it does
**not** honour the root `--server` / `--token-env` flags the other `hivectl`
commands read: it builds its own client directly from three environment
variables, checked once at startup:

| Variable | Default | Meaning |
|---|---|---|
| `HIVE_DASHBOARD_URL` | `http://localhost:3001` | Dashboard API base URL |
| `HIVE_DASHBOARD_TOKEN` | *(empty)* | Shared dashboard token — the same variable `--token-env` defaults to |
| `HIVE_DASHBOARD_COOKIE` | *(empty)* | Session cookie header, for hives that do not accept the shared token — see [Credentials](#credentials) |

If you already have `HIVE_DASHBOARD_TOKEN` exported for the commands above,
`hivectl tui` picks it up for free — the token variable name is shared on
purpose. The base URL default differs by one detail from `--server`'s
(`http://localhost:3001` vs. `http://127.0.0.1:3001`): both resolve to the
same loopback dashboard, but set `HIVE_DASHBOARD_URL` explicitly if you also
pass `--server` to other commands against a non-default host, since the TUI
will not pick that flag up.

#### Credentials

The dashboard has two credential lanes and they are **not** interchangeable.
Which one your hive accepts is a property of how it was deployed, not a
preference:

| Your hive | What it accepts | Set |
|---|---|---|
| Self-hosted, `dashboard.auth_token` set, no `authorized_users` | Shared bearer token | `HIVE_DASHBOARD_TOKEN` |
| Spoke with an `authorized_users` allowlist (direct-route) | Per-user session **only** | `HIVE_DASHBOARD_COOKIE` |
| Hub-hosted | The hub's per-user session | `HIVE_DASHBOARD_COOKIE` |

The shared token grants unscoped owner and carries no per-user identity, which
is exactly why a spoke with an allowlist **disables** it — accepting it would
let any holder act as an owner and defeat the per-hive allowlist. Those hives
identify callers by the `hive_session` cookie the GitHub device-flow login
mints, resolved on every request against the live allowlist.

`HIVE_DASHBOARD_COOKIE` is a **cookie header value**, not a bare session id —
the same string a browser would send:

```bash
export HIVE_DASHBOARD_COOKIE='hive_session=8f3c…'
hivectl tui
```

Several cookies are joined with `; `, which is what a hub-hosted hive needs
when its per-hive terminal assertion rides alongside the hub session. Taking a
header value rather than a named id is what lets one variable serve every
cookie-based deployment without the TUI having to know which kind of hive it
is talking to.

Obtaining the value today means copying it out of a browser already logged in
to that dashboard (devtools → Application → Cookies). There is no
`hivectl login` yet; adding one so the credential can be acquired from the
terminal is tracked in
[#5651](https://github.com/kubestellar/hive/issues/5651).

Both variables may be set at once, and both are sent. That is not redundancy:
a shared-token hive ignores the cookie and an allowlist hive ignores the token,
so setting both is how one exported environment works against every hive you
operate.

**If the credentials are rejected, `hivectl tui` refuses to start** rather than
opening a screen of empty panes. A `401` is a standing answer — no keypress in
the TUI can fix it — so it prints which variables to set and exits, leaving
your scrollback intact. Every other failure still opens normally: a hive that
is merely *down* is one of the main reasons to open the TUI, and the panes fill
themselves when it returns. A `403` also opens: that is a working session whose
role is too narrow for some reads, which the panes already handle individually.

#### The four panes

A 2×2 grid — Agents and Governor on top, Tokens and Events on the bottom —
refreshed from two independent loops with different cadences:

| Pane | Source | Cadence |
|---|---|---|
| **Agents** | `GET /api/agents`, joined with live per-agent state pushed over SSE | reconciliation loop |
| **Governor** | `GET /api/status` (live mode/queue), `GET /api/config/governor` (eval interval) | reconciliation loop |
| **Tokens** | `GET /api/tokens` (counts, required) + `GET /api/cost` (estimate, optional) | activity loop, fixed |
| **Events** | `GET /api/audit` (newest-first operator/governor activity) | activity loop, fixed |

The **reconciliation loop** is what the SSE connection status affects: every
5 seconds while the stream is down or has not proven itself yet, stretching to
every 60 seconds once an event has actually been received on it (the stream
is doing the work at that point; the poll is just a reconciler catching a
roster change the stream does not announce, or a stream that stalled without
dropping the connection).

The **activity loop** — Tokens and Events — polls every 5 seconds
**unconditionally**, whether or not the SSE stream is connected. Nothing on
the stream carries token counts, estimated cost, or audit rows, so there is
no push event for those panes to wait on; tying them to the reconciliation
timer would make a *healthy* connection the reason they went stale. (This is
what `tui T32` / [#5421](https://github.com/kubestellar/hive/issues/5421)
fixed — earlier builds hung all seven reads off one timer, so a connected
stream paradoxically made the Tokens and Events panes twelve times staler.)

Both loops fetch once immediately on startup, so the frame fills in before
either interval elapses. A failed fetch never blanks a pane: the previous
successful frame stays on screen until the next successful read replaces it.
One field is the exception — a failed `/api/cost` read *clears* the cached
estimate rather than holding it, because a dollar figure attached to token
counts that have since moved by a fresh `/api/tokens` read is worse than an
honest `—`. Every write action below (pause/resume, model apply, kick,
ACMM apply, returning from a tmux attach) also triggers an immediate full
refresh of both loops, so the frame does not wait out an interval to show the
effect of the operator's own action.

`/api/audit` is the one poll-shaped read that needs owner (read-write) access;
a read-only token gets a 403 there like any other forbidden read, and it
travels the same swallowed-error path as a network failure — the Events pane
simply keeps its last successful snapshot rather than showing an error.

#### Keybindings

| Key | Action | Scope |
|---|---|---|
| `tab` / `shift+tab` | Cycle pane focus forward / backward | global |
| `j` / `k`, `↓` / `↑` | Move the row selection | Agents, Events panes |
| `p` | Pause or resume the selected agent (opens a y/n confirm) | Agents pane |
| `m` | Open the model picker for the selected agent | Agents pane |
| `K` | Kick the selected agent now | Agents pane |
| `A` | Open the ACMM level overlay | global |
| `a` | Attach to the selected agent's tmux session (local, or via the dashboard's terminal proxy) | Agents pane |
| `?` | Toggle the help overlay (lists this table; dismisses on any key) | global |
| `q` / `ctrl+c` | Quit | global |

Case is meaningful and deliberate: `K` (kick) and `A` (ACMM) are the two
actions with the widest blast radius bound to a bare key, so each sits on the
shifted member of a pair whose lowercase twin (`k` navigate, `a` attach) is
pressed constantly during normal use — a missed shift never fires the bigger
action by accident. Actions apply to the selection in the **focused** pane
only; pressing `p`/`m`/`K`/`a` while a pane other than Agents is focused is a
no-op, never a guess at "the current agent".

**Every overlay below is modal**: while one is open it consumes *every* key,
including `q`, `tab`, and the other action letters — closing or resolving the
overlay is the only way those reach the frame underneath again. The ACMM
overlay is the one exception to "letters are bindings": once its typed
confirmation is open, ordinary letters (including `p`, `a`, `A`, `K`) are
literal text being typed into the confirmation phrase, not actions.

#### Pause / resume

`p` on a selected agent opens a confirm dialog (`y` confirm, `n` / `esc`
cancel) rather than acting immediately. While the request is in flight the
dialog shows "Working…" and swallows further input; on failure it shows the
error in place and offers retry (`y`) or cancel. A 403 renders as `Pause
failed: owner access required` (or `Resume failed: …`) rather than the raw
HTTP error, since a forbidden pause/resume always means the same thing:
the token is not the hive owner's.

#### Model apply

`m` opens a picker for the selected agent's backend, fetching its model
catalogue asynchronously. The catalogue can carry two independent
qualifications, both shown as a note in the overlay when present:

- **Fallback** — endpoint discovery found nothing and the server substituted
  its static alias list; these ids are unverified guesses, not a confirmed
  catalogue.
- **Partial** — some of the backend's endpoints answered and others did not;
  a model's *absence* from the list proves nothing, since it may be served
  only by the endpoint that failed to answer.

`enter` applies the highlighted model. **Applying restarts the agent's
session** — the overlay states this before every apply — so the picker is not
a preview; choosing a model takes effect and interrupts in-flight work.
Applying is refused while a previous apply is still pending, so repeated
`enter` presses cannot queue a second restart. A 403 here renders as `Model
change failed: owner access required`.

#### Kick

`K` queues an immediate run for the selected agent and reports the outcome in
the footer rather than the pane: `kick queued for <agent>` on success, or
`kick already in flight for <agent> (request deduplicated)` if one was already
queued. A 403 renders as `Kick failed: owner access required`. Only one local
kick request is in flight at a time; `K` is a no-op while the previous one is
still pending.

#### ACMM apply

`A` opens the ACMM level overlay from anywhere — the level is a property of
the whole hive, not the focused pane's selection. It fetches the pack list
(the level definitions the server has configured) asynchronously; a 403 here
renders as `ACMM packs unavailable: owner access required`.

Selecting a level and pressing `enter` does **not** apply it directly — it
begins a **typed confirmation**: the overlay asks for the exact phrase
`APPLY L<n>`, and only an exact match arms the apply on the next `enter`.
Anything else typed is a no-op; `esc` during confirmation backs out to the
list rather than closing the overlay outright, so a mistyped phrase costs one
key. Selecting the level already in force skips confirmation entirely and
shows a "nothing to apply" message instead, since there is no write to
protect against.

On success the overlay **stays open**, now showing the reconciliation
receipt, until the operator dismisses it with `enter` or `esc` — it does not
flash past into a footer line, and a second `enter` cannot re-apply. On a
partial failure (the level persisted server-side but the fleet has not yet
reconciled to it — a 500 after the write took effect), the overlay still
shows an apply error, but the app also triggers an immediate refresh anyway,
because the panes underneath may already describe a hive that has moved.

#### Attach: local tmux, or the dashboard's terminal proxy

`a` on a selected agent attaches to that agent's tmux session. Where the
session actually is decides how ([#5644](https://github.com/kubestellar/hive/issues/5644)):

- **Local fast path.** When `HIVE_DASHBOARD_URL` is loopback (or unset) and a
  local `tmux has-session -t hive-<agent>` succeeds, the terminal is handed to
  `tmux attach` directly — the original, zero-network behavior.
- **Remote attach.** When the dashboard is not loopback, or the local session
  does not exist — the recommended Podman install, where the sessions live
  inside the container under other UIDs — the TUI dials the dashboard's own
  `/terminal` websocket instead: the same authenticated reverse proxy the web
  dashboard's "▶ terminal" links use, in front of the container's ttyd.
  Nothing new is exposed for this; ttyd's port stays loopback-only inside the
  container, and the attach carries the same credentials as every other TUI
  request (`HIVE_DASHBOARD_TOKEN` and/or `HIVE_DASHBOARD_COOKIE` — see
  [Credentials](#credentials)), plus ttyd's own basic-auth credential derived
  the way the container derives it (`hive:<token>`, overridable with
  `HIVE_TTYD_CREDENTIAL` if the deployment overrode it too).

The fallback is never silent: a remote attach prints one line before the
session's first byte saying which hive it went through and which session it
attached to, so there is no ambiguity about what you are typing into. That
line never contains the token.

Both paths preflight before suspending the TUI, so a missing `tmux` binary, a
session that does not exist anywhere, an unreachable dashboard, or a refused
authorization surfaces as a footer message (`Attach failed: …`) instead of a
redraw flicker — a 403 reads `owner access required` like the other actions,
a 401 names both credential variables. Inside a remote session every
keystroke, `ctrl+c` included, belongs to the agent's terminal; you leave with
tmux's own detach (`prefix + d`, i.e. `Ctrl-b d` by default). Returning from
the session (however it ends, on either path) restores the TUI and
immediately refreshes the fleet, since the agent's state may have moved while
attached. If the connection drops mid-session rather than closing cleanly,
the footer says so — your last keystrokes may not have arrived.

#### Connection status and the poll fallback

The header's `ws:` field is `connected` only once an event has actually been
**received** on the SSE stream — not merely dialled, since a successful
subscribe hands back its channels before the request is even confirmed.
Everything else — before the first event, during a reconnect's backoff, and
right after a drop — reads `not connected`, because all three mean the same
thing to an operator: the numbers on screen are coming from the fallback poll,
not the stream. `hive:` (identity) and `governor:` (mode) never blank on a
drop; only `ws:` changes, so a flapping connection does not make the header
look like the hive itself went away.

On a drop, reconnection backs off starting at 1 second and doubling to a
30-second cap, and the reconciliation loop's cadence drops back from 60s to 5s
immediately along with an out-of-cycle fetch, so the fallback's first data
does not wait out a whole interval. The activity loop (Tokens, Events) is
unaffected by any of this — see [The four panes](#the-four-panes) above.

#### Terminal size, help, resize, and quit

The grid needs at least **60×20** to stay readable; below that the frame is
replaced by a single centred `terminal too small (need at least 60x20)`
message rather than shrinking the panes into unreadable slivers. Resizing is
automatic — the layout re-derives itself from the terminal size on every
render, with no keybinding involved. `?` toggles a help overlay listing every
binding above; it dismisses on any keypress. Colors are chosen for contrast
against both light and dark terminal backgrounds, so the focused-pane border
and header stay legible either way. `q` or `ctrl+c` quits **from the top level
only** — while any overlay is open, `q` is swallowed like every other key, so
an operator dismisses the overlay first (see the modal rule above).

#### v1 boundaries

- **No login flow.** Both credentials must already exist in your environment:
  the TUI reads `HIVE_DASHBOARD_TOKEN` and `HIVE_DASHBOARD_COOKIE` and cannot
  acquire either. Session-based hives (hub-hosted, or a spoke with an
  `authorized_users` allowlist) are reachable, but the cookie has to be lifted
  from a browser — see [Credentials](#credentials) and
  [#5651](https://github.com/kubestellar/hive/issues/5651).
- **No terminal emulation in-frame.** Remote attach
  ([#5644](https://github.com/kubestellar/hive/issues/5644)) suspends the TUI
  and streams the ttyd websocket through your own terminal, exactly as the
  local path suspends into `tmux attach`; the session is not re-rendered
  inside a pane.
- **Feature parity with the web dashboard's operator loop, not visual
  parity.** The TUI does not attempt to look like the web dashboard.

#### Seeing it run

Everything above is recorded as a VHS tape at
[`src/docs/design/tui.tape`](design/tui.tape) — launch, the four populated
panes, focus and navigation, the help overlay, and each write action in its
safe state. Render it with [VHS](https://github.com/charmbracelet/vhs)
(`brew install vhs`) and a Go toolchain; it needs no Docker, no network and no
cluster:

```bash
cd src
go build -o bin/hivectl ./cmd/hivectl
go run ./pkg/tui/testdata/demohive &     # loopback fixture; kill %1 to stop
vhs docs/design/tui.tape                 # writes docs/design/_out/tui.gif
```

The tape records against `src/pkg/tui/testdata/demohive`, a single-file,
stdlib-only, loopback-only fixture that serves fixed literals for the routes
the TUI reads — so the recording never captures a real hive's identity, agent
names or spend, and renders identically every time. The GIF is a build product
and is gitignored; the tape is the committed source of truth.

The tape deliberately does **not** complete a model apply or an ACMM apply (it
drives each to its confirmation and cancels), and omits local tmux attach and
SSE degradation, both of which need state the fixture cannot produce
deterministically. Each omission is explained in a comment in the tape itself.

Track any further work under the `hive tui` epic
([#4907](https://github.com/kubestellar/hive/issues/4907)).

### enroll — spoke-based lite repo enrollment

```bash
hivectl enroll OWNER/REPO
hivectl enroll OWNER/REPO --acmm-level 1
hivectl enroll OWNER/REPO --installation-id 123456
```

`enroll` verifies local `gh` authentication and repository access, then asks the
hub to place the repo on a spoke. Existing owned spokes receive the repo through
the heartbeat project-config callback; otherwise the hub provisions a hosted
lite spoke. Lite mode is advisory-only and capped at ACMM L2. It writes no
repository PAT or long-lived secret.

## Input and safety

- Write commands take `--file <path>` or `--stdin` (JSON or YAML).
- Destructive actions (`agent delete`, `knowledge delete`, `bead reset`,
  `governor agent-remove`) require `--yes`.
- Request bodies are capped at 1 MiB, measured on the JSON-encoded payload;
  oversized input is rejected before anything is sent.
- Tokens are read from an environment variable, never passed as a flag value.
