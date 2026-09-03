# Agent peer-awareness logging (pluk)

Hive attaches [`pluk`](https://www.npmjs.com/package/@kubestellar/pluk) — an
external, per-agent output-capture layer — to every agent's tmux session. Pluk
writes a structured JSONL event stream for that session, and other agents (or
operators) can read it for read-only peer awareness without attaching to the
agent's tmux socket.

This page documents what pluk is, the log format Hive's own code depends on,
and how `hive-panes` uses it. It does not document pluk's internals — pluk is
a separate npm package (`@kubestellar/pluk`), not part of this repo — only the
parts of its output that Hive code and scripts actually consume.

## What pluk is

Pluk is installed in the container image as an npm global package
(`@kubestellar/pluk`, pinned by `PLUK_VERSION` in `Dockerfile`). It is not a
Hive-authored component; Hive only shells out to it.

When the agent manager creates a tmux session for an agent
(`pkg/agent/manager.go`), it looks up `pluk` on `PATH` and, if found, attaches
it to that session's pane with:

```
pluk watch <tmux-session> --cli=<backend>
```

via `tmux pipe-pane`. `<backend>` is the agent's configured CLI backend
(`claude`, `codex`, etc.), defaulting to `claude` when the backend is empty or
is an inference backend. If the `pluk` binary is not on `PATH`, attachment is
skipped silently — the agent still runs, it just has no peer-visible log.

Pluk also exposes a `send` subcommand. `pkg/dashboard/inception_handlers.go`
uses `pluk send hive-<session> --text=<message> --enter` as a fallback way to
deliver a kick message to an agent's pane when the normal `SendKick` path
fails, because it writes through a named FIFO and confirms delivery via a
`command_received` event rather than relying on `tmux send-keys`.

## Availability conditions

- Pluk attachment is automatic for every agent tmux session Hive creates — no
  per-agent configuration is required.
- It requires the `pluk` binary to be present on `PATH` in the container. It
  is installed unconditionally in the shipped image, but a custom or stripped
  image without it will run agents with no pluk logs and no `hive-panes`
  output.
- The run directory (`/var/run/pluk` — `logs/` and `commands/` subdirectories)
  is created ahead of time in the image (`Dockerfile`) and re-created
  defensively before each attach by `ensurePlukRunDirs`
  (`pkg/agent/pluk_dirs.go`). Both subdirectories are `0770` plus setgid,
  owned by the `dev` user and the shared `node` group, so any agent UID in
  that group can read another agent's log without world-readable
  permissions.
- If directory setup fails, Hive logs a warning
  (`"pluk run directory setup failed; pluk publisher may be degraded"`) and
  still attempts the attach — a degraded or absent log is non-fatal to the
  agent itself.

## Log location and format

Each agent's events land in one file per tmux session:

```
/var/run/pluk/logs/<tmux-session>.jsonl
```

Session names follow Hive's `hive-<agent-name>` convention (e.g.
`hive-brainstorm.jsonl` for the `brainstorm` agent). Each line is one JSON
object, newline-delimited (JSONL), appended as the agent produces output.

The file is written by a shell redirect, not by pluk. `pluk watch` classifies
its stdin and prints events to stdout; it opens no file of its own. `tmux
pipe-pane` feeds the pane into that command's stdin but does nothing with its
stdout, so the publisher invocation in `pkg/agent/manager.go` has to end in
`>> /var/run/pluk/logs/<session>.jsonl` for anything to be written at all. It
did not between [#1759](https://github.com/hivecommons/hive/pull/1759) — which
replaced the Go `pluk-publish` binary, a publisher that wrote the log itself,
with the TypeScript `pluk watch` — and
[#4285](https://github.com/hivecommons/hive/issues/4285). Throughout that
window this directory was empty and every consumer below was reading files that
did not exist.

Hive pre-creates each session log `0660` and `dev:node` before attaching
pipe-pane. `hive-panes` is run by one agent to read every *other* agent's log,
and agent UIDs differ, so the shared `node` group is what makes cross-agent
reads work — leaving creation to the shell's `>>` would take whatever umask the
pane shell carries and a tight one yields a log no peer can read.

Nothing rotates or caps these files. `hive-panes` only ever reads the last 2000
lines and the inception subscriber seeks to the end, so no consumer needs the
history, but the files themselves grow for the life of the container.

The event envelope, as read by Hive's own code, is:

```json
{"type": "<event-type>", "session": "<tmux-session>", "data": {...}}
```

`type` is a string discriminator; `data` is a flat string-to-string map whose
keys depend on `type`. Hive's dashboard subscribes to this stream for the
`brainstorm` session (`pkg/dashboard/inception_watcher.go`, `plukEvent`) and
reacts to a subset of event types:

- `raw_output` — captured pane output. The text is on `data.line`, and only
  there. Both readers now agree: `hive-panes.sh` has always read `data.line`,
  and the dashboard's Go subscriber (`handlePlukEvent` in
  `inception_watcher.go`) read `data.message` until
  [#4285](https://github.com/hivecommons/hive/issues/4285) corrected it, which
  had left its whole `raw_output` arm dead. Earlier revisions of this page
  advised new consumers to check both keys; that was wrong, and generalised
  from the `error`/`rate_limit` shape below. pluk builds the event as
  `createEvent(..., 'raw_output', { line })` in `dist/classifier.js`.

  `raw_output` is opt-in: `pluk watch` defaults `includeRaw` to false, so the
  publisher has to pass `--include-raw` or no such event is ever emitted. It is
  the only event type `hive-panes.sh` consumes, so peer awareness depends on
  that flag.
- `state_change` — `data.state` is `"working"` or `"idle"`. The inception
  watcher uses idle transitions to decide when to re-kick a stalled agent or
  fall back to auto-generating questions/facts.
- `rate_limit` — `data.message` describes a provider rate limit; the watcher
  suppresses retries for a cooldown window when it sees this.
- `error` — `data.message` describes an agent-side error; logged, not acted
  on beyond that.
- `tool_call_completed` — no payload used; the watcher treats this as a cue
  to poll bead state immediately (e.g. after a `bd create`/`bd update`)
  instead of waiting for the next timer tick.

Only the `brainstorm` session is subscribed to for real-time event handling
today. Other agents' logs exist under the same directory in the same format
but are not tailed by any in-process Hive consumer — they exist for
`hive-panes` and manual/operator inspection.

## How `hive-panes` reads it

`hive-panes` (installed by `src/deploy/hive-panes.sh` when the deployment
includes that script) is a read-only peer-awareness command available inside
an agent's shell:

```
hive-panes [lines]     # default: 30
```

It:

1. Lists every `hive-*.jsonl` file in `/var/run/pluk/logs`.
2. Skips the file for the calling agent's own session, identified by the
   `HIVE_PROXY_AGENT` environment variable (so an agent never has to filter
   its own output out of the results).
3. For each remaining file, takes the last 2000 raw lines, parses each as
   JSON, keeps only entries where `type == "raw_output"`, and extracts
   `data.line`.
4. Strips ANSI escape sequences and cursor-position control codes, drops
   blank lines, and prints the last `N` cleaned lines per agent under a
   banner naming that agent.

It never attaches to another agent's tmux socket — it only reads the JSONL
file on disk, so it cannot send input or otherwise disturb another agent's
session. If `/var/run/pluk/logs` does not exist, it exits with an error
rather than printing empty output, which is the fastest signal that pluk
never attached (binary missing, or the agent manager could not create the
run directory).

## Retention and rotation

This repo does not implement rotation or a retention policy for
`/var/run/pluk/logs/*.jsonl` — there is no cron, size cap, or truncation
logic in `pkg/agent` or `src/deploy` for these files. `/var/run` is typically
an in-memory or ephemeral filesystem inside the container, so logs do not
outlive the agent's tmux session/container lifecycle, but within that
lifetime the files grow unbounded from Hive's side. Any rotation behavior
(e.g. size-based rollover) would come from pluk itself; check the installed
`@kubestellar/pluk` version's own behavior if you need to reason about disk
growth, since Hive does not manage it.

The dashboard's own subscriber (`runPlukSubscriber` in
`inception_watcher.go`) detects rotation defensively: it compares the open
file handle's inode against the on-disk file on each idle poll and, if they
differ, logs `"pluk log rotated, restarting subscriber"` and returns so the
caller can reopen — but this is a consumer-side safeguard, not evidence that
Hive itself rotates the file.

## Using peer awareness as an agent

- Use `hive-panes [lines]` when you need situational awareness of what other
  agents in the fleet are currently doing — for example, before duplicating
  work, or to check whether another agent already touched a file you are
  about to change.
- Treat its output as recent raw terminal text, not structured data — it is
  ANSI-stripped CLI output, not a machine-readable event feed. Do not parse
  it for automation; if you need structured events, read the JSONL files
  directly the way `inception_watcher.go` does.
- Avoid depending on it being present. It requires `hive-panes.sh` to be
  deployed and `pluk` to have attached successfully; on a deployment that
  omits either, `/var/run/pluk/logs` will not exist and the command exits
  with an error. Code and prompts should not treat pluk logs as a guaranteed
  input.
- It is read-only by design: it cannot be used to send input to another
  agent's session. Use each agent's own kick/messaging path
  (`SendKick`/`pluk send`) for that, not `hive-panes`.

## Terminal scrollback and full-log capture

Separate from pluk's JSONL stream, each agent's tmux session keeps a deep
terminal scrollback:

- Agent sessions are created with tmux `history-limit` **50000** (tmux's own
  default is 2000, which silently truncated long runs —
  [#3790](https://github.com/hivecommons/hive/pull/3790)). Override with
  `HIVE_TMUX_HISTORY_LIMIT` (positive integer); it must be set at session
  creation — raising it later never deepens an existing pane. The attach-time
  twin `HIVE_TTYD_HISTORY_LIMIT` (also 50000) only affects panes created after
  a browser attach.
- The browser terminal attaches with tmux mouse mode on, so the scroll wheel
  drives copy-mode scrollback; hold Shift (Option on macOS) for native browser
  text selection ([#3722](https://github.com/hivecommons/hive/pull/3722)).
- **Full-log view/download:** `GET /api/agents/{name}/log` returns the entire
  retained buffer for the agent's latest run as `text/plain` (add `?download=1`
  for a `Content-Disposition` download named
  `hive-<agent>-<timestamp>.log`) — [#3711](https://github.com/hivecommons/hive/pull/3711).
  In the dashboard these are the `📄 full log` and `⬇ log` controls on the
  agent card. Normal dashboard auth applies (any authenticated role); output
  passes through token redaction, so it is not a redaction bypass.

The live capture is scoped to the agent's **current** tmux session: a restart
kills and recreates the session, and a hive upgrade replaces the whole
container. Per-kick log archiving (below) is what preserves run logs across
those boundaries.

## Per-kick log history

Hive archives each kick's terminal output to a durable file so operators can
read the logs of the last several runs — not just the latest —
([#4296](https://github.com/hivecommons/hive/issues/4296),
[#4295](https://github.com/hivecommons/hive/issues/4295)):

- **When snapshots happen.** The scrollback is captured to a file *before*
  it can be destroyed: when the next kick is delivered (the previous kick's
  output is archived and the tmux history is cleared, so each archive covers
  exactly one kick), before `kill-session` on an agent restart, and on
  graceful shutdown (SIGTERM — pod roll or hive upgrade) for every agent
  with un-archived kick output.
- **Where they live.** `/data/logs/kicks/<agent>/<timestamp>-<reason>.log`
  (override the root with `HIVE_KICK_LOG_DIR`). `/data` is the persistent
  volume on hosted hives, so archives survive agent restarts, pod rolls, and
  hive image upgrades. Each file starts with a small header (agent, archive
  time, snapshot reason, kick start time, prompt snippet) followed by the raw
  scrollback. `<reason>` is `kick`, `restart`, or `shutdown`.
- **Retention.** Per agent, the newest **10** archives are kept
  (`HIVE_KICK_LOG_RETENTION`; `0` disables archiving) within a **64 MiB**
  per-agent size cap (`HIVE_KICK_LOG_MAX_BYTES`); the oldest files are pruned
  first and the newest archive is never deleted. An agent with less history
  than normal (fresh install, retention just lowered) simply lists fewer
  entries — never an error.
- **Dashboard access.** The `🕘 past kicks` control on the agent card opens
  `GET /agents/{name}/kicks`, an index of the live log plus every archived
  kick with view/download links. Programmatic access:
  `GET /api/agents/{name}/kicks` (JSON list, newest first) and
  `GET /api/agents/{name}/kicks/{id}` (`text/plain`; `?download=1` for an
  attachment). Same auth rule as the full-log endpoint (any authenticated
  role), and archived content passes through the same token redaction.
- **Limitations.** Sandbox-executor kicks have no tmux session and are not
  archived here. The archive holds at most the retained scrollback
  (`history-limit`, 50000 lines by default), so an extremely long run
  self-truncates from the top. A hard kill (SIGKILL, node loss) skips the
  shutdown snapshot; the previous kicks' archives on `/data` are unaffected.

## Related

- [`src/deploy/README.md`](../deploy/README.md) — `hive-panes.sh` inventory
  entry.
- [Agent configuration](agent-configuration.md) — where `hive-panes` is
  introduced as an agent-facing capability.
- [Architecture](architecture.md) — system-level mention of the read-only
  peer-observation workflow.
