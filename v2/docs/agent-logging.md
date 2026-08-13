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

The event envelope, as read by Hive's own code, is:

```json
{"type": "<event-type>", "session": "<tmux-session>", "data": {...}}
```

`type` is a string discriminator; `data` is a flat string-to-string map whose
keys depend on `type`. Hive's dashboard subscribes to this stream for the
`brainstorm` session (`pkg/dashboard/inception_watcher.go`, `plukEvent`) and
reacts to a subset of event types:

- `raw_output` — captured pane output. `hive-panes.sh` reads the text from
  `data.line`; the dashboard's own Go subscriber reads it from `data.message`
  (see `handlePlukEvent` in `inception_watcher.go`). Both are read from real
  pluk output in this codebase — if you are writing a new consumer, check
  both keys rather than assuming one, since pluk's own event shape is outside
  this repo's control and the two existing readers do not agree on the field
  name.
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

`hive-panes` (installed by `v2/deploy/hive-panes.sh` when the deployment
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
logic in `pkg/agent` or `v2/deploy` for these files. `/var/run` is typically
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

## Related

- [`v2/deploy/README.md`](../deploy/README.md) — `hive-panes.sh` inventory
  entry.
- [Agent configuration](agent-configuration.md) — where `hive-panes` is
  introduced as an agent-facing capability.
- [Architecture](architecture.md) — system-level mention of the read-only
  peer-observation workflow.
