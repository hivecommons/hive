# Token-access audit log (`/var/run/hive-metrics/token-access.jsonl`)

The token-access log records each time an agent *used* a GitHub credential:
every `gh` CLI invocation through the wrapper and every git credential lookup
through the credential helper. It answers "which agent touched GitHub, when,
and with what command" - the question the general
[audit log](audit-log.md) (`/data/audit.jsonl`) does not, because that file
records dashboard/hive actions, not per-agent credential use.

Until now the only trace of this log in the docs was a single route row in the
[API reference](api-reference.md). This page defines its writers, schema,
lifecycle, and - just as important - what it does **not** capture.

## Writers and line schemas

One JSON object per line (JSONL), append-only. Two shell writers, each emitting
its own shape:

**`bin/gh-wrapper.sh`** - on every `gh` call by a hive agent (not in
contributor mode), after the per-agent scoped token gate passes:

```json
{"ts":"2026-09-08T13:10:37Z","agent":"guide","uid":1201,"op":"gh","cmd":"gh pr view 6347 --json title"}
```

**`bin/git-credential-hive.sh`** - on every credential `get` (i.e. every
clone, fetch, and push against the configured GitHub host):

```json
{"ts":"2026-09-08T13:10:41Z","agent":"guide","uid":1201,"op":"git-credential","host":"github.com"}
```

| Key | Present for | Meaning |
| --- | --- | --- |
| `ts` | both | RFC 3339 UTC timestamp. |
| `agent` | both | Agent name - `HIVE_AGENT` (gh) / `AGENT` (git); `"unknown"` when unset. |
| `uid` | both | Numeric UID of the writing process (`id -u`). |
| `op` | both | `"gh"` or `"git-credential"`. |
| `cmd` | `op:"gh"` only | The full command line, `"gh "` + all arguments verbatim. |
| `host` | `op:"git-credential"` only | The host git asked a credential for; `"unknown"` when git sent none. |

`cmd` is recorded **verbatim and unredacted** - it includes `--title`,
`--body`, repo names, and anything else the agent passed on the command line.
That is why the read API below is owner-gated. It is also why nothing secret
may ever be passed to `gh` as an argument.

## Reading it: `GET /api/token-access`

`handleTokenAccess` (`pkg/dashboard/api.go`) serves the log to the dashboard:

- **Owner role required** (#3936, CWE-284). The log enumerates the hive's
  entire GitHub operation history including full `gh` arguments, so it is
  gated at owner, consistent with config download and self-upgrade.
- **Last 100 lines only** (`tokenAccessMaxEntries`). The file itself is
  append-only and unrotated; the API is a tail, not a query surface.
- Each line is passed through as raw JSON (`{"entries":[...]}`); a malformed
  line is passed through malformed. Parse defensively.
- If the file is unreadable the response is
  `{"entries":[],"error":"no audit log"}` - indistinguishable in the
  `entries` field from a hive that has done nothing. Check `error`.

## Lifecycle and permissions

The log lives in `/var/run/hive-metrics`, which is deliberately `0755
dev:node` and **not agent-writable** (#4044: its `agent-tokens/` subdirectory
holds the bot-identity file the gh-wrapper author gate trusts, and the same
no-agent-writes rule covers the parent). Agents therefore cannot create the
file; the container entrypoint (`src/deploy/entrypoint.sh`) pre-creates it as
`dev:node 0664` (#6106) so that only this one file in the directory accepts
agent appends.

Both writers append inside a `{ ...; } 2>/dev/null || true` group, so a failed
append is **silent by design** (#4043/#6106 - an audit append must never break
or noise up a `gh` call or a git fetch). Consequences:

- On a hive whose entrypoint predates the pre-creation fix (#6106), every
  append fails silently and the log is empty forever. **An empty log is not
  evidence of no GitHub activity.**
- `/var/run/hive-metrics` is not a volume in the standard deployments (it is
  neither on `/data` nor a tmpfs mount): the log lives in the container's
  writable layer, nothing rotates or truncates it, and it is lost whenever the
  container is recreated (image upgrade, pod reschedule). Treat it as a
  recent-activity window, not an archive. The durable accountability record is
  [`/data/audit.jsonl`](audit-log.md).

## What it does NOT capture

- **Contributor-mode `gh` calls** - the gh-wrapper append runs only on the
  non-contributor branch.
- **Anything that bypasses the wrappers** - direct `curl` to the GitHub API,
  MCP GitHub tools, or a raw `gh` binary invoked outside the wrapper `PATH`
  shim writes no line here.
- **Whether the call succeeded** - a line means the credential was handed out
  / the command was launched, not that GitHub accepted it.
- **git-over-ssh** - the credential helper answers only for `https`.

Use it to attribute *which agent* drove observed GitHub traffic and when; do
not use it as a complete or durable ledger of GitHub effects. For those, use
GitHub's own audit surfaces and `/data/audit.jsonl`.
