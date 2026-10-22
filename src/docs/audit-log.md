# Audit log format (`/data/audit.jsonl`)

The audit log is the accountability record for actions a hive takes: who did
what, when, and to which agent. Several docs reference it — the
[watchdog](agent-watchdog.md) records observed restarts here, [hooks](hooks.md)
record firing failures here, and the
[security self-assessment](security-self-assessment.md) names it as the
attribution substrate — but until now none of them defined its shape.

This page defines it, so a consumer can parse the file without reading Go.

A related but distinct record (which agent used a GitHub credential, and with
what `gh` command) lives in the
[token-access audit log](token-access-log.md), not here.

## Format

One JSON object per line (JSONL), append-only. Written by
`pkg/dashboard/audit.go`.

```json
{"ts":"2026-08-27T14:31:02Z","user":"clubanderson","action":"config_governor_save","detail":"level=4","agent":""}
```

## Fields

| JSON key | Go field | Always present | Meaning |
| --- | --- | :---: | --- |
| `ts` | `Timestamp` | yes | RFC 3339 UTC timestamp of the action. |
| `user` | `User` | yes | Who performed it. May be a pseudo-user — see below. |
| `action` | `Action` | yes | The action name, e.g. `config_governor_save`, `pr_merged`, `hook_failed`. |
| `detail` | `Detail` | no (`omitempty`) | Free-form `k=v` pairs joined by `, `. See the parsing note. |
| `agent` | `Agent` | no (`omitempty`) | The agent the action concerns, when it concerns one. |
| `repo` | `Repo` | no (`omitempty`) | Repository a hive-mediated GitHub write went to. Set on write entries since [#9587](https://github.com/hivecommons/hive/issues/9587); see [GitHub write surface](github-write-surface.md). |
| `target` | `Target` | no (`omitempty`) | Issue or PR number that write went to, when it has one. |
| `user_name` | `UserName` | **API responses only** (`omitempty`) | Display name for an opaque OIDC actor key. Stamped at serve time by `GET /api/audit` — **never written to the file or the ring**. See below. |
| `sensitive` | `Sensitive` | **API responses only** (`omitempty`) | `true` when the entry's action is in the dashboard sensitive-action vocabulary used by the collapsed Audit Log risk count. Stamped at serve time by `GET /api/audit` — **never written to the file or the ring**. |

Optional fields are omitted entirely rather than emitted empty, so **a consumer
must treat a missing `detail`, `agent`, `repo`, `target`, `user_name`, or `sensitive` as
absent, not as an empty string**.

### `user_name` exists only on the API surface

When `user` is an opaque OIDC identity key, `handleAuditLog` attaches the
hub-delivered display name as `user_name` on the copies it serves. The on-disk
`audit.jsonl` and the in-memory ring keep only the raw key, so history survives
display-name changes. Consequences:

- A parser of the **file** will never see `user_name`; a consumer of the
  **API** may. Do not treat its absence in the file as data loss.
- `user_name` is cosmetic. Correlate entries by `user` (the stable key), never
  by `user_name`.

## Parsing `detail`

`detail` is a flat string of `key=value` pairs joined by `, ` — not a nested
object. It is built by `InvocationMeta.AuditDetail` from a generic pair list, so
**the keys present vary by action** and there is no typed accessor.

```
repo=hivecommons/hive, number=4911, agent=guide, backend=copilot
```

Consequences worth knowing before you build on it:

- **Prefer the typed `repo` field.** Entries for hive-mediated GitHub writes
  carry `repo` (and `target`) as first-class fields since #9587, and keep the
  `repo=` pair in `detail` too. Entries written before that, and entries from
  audit sites that are not GitHub writes, have the pair only, so fall back to
  parsing `detail` when the field is absent.
- **Do not assume a key exists.** Match on the key you need and skip lines that
  lack it, rather than positionally.
- **Values are not escaped.** A value containing `, ` or `=` will parse
  ambiguously. In practice values are repo names, numbers, and enum-like
  strings, but a parser should be defensive.

## Pseudo-users

`user` is not always a person. The values `""`, `system`, `local`, and `unknown`
are pseudo-users written by automated paths. They are recorded in the file like
any other entry — the distinction only affects the dashboard's "last action by
user" display, which skips them.

Do not treat a pseudo-user entry as less authoritative; a `system` entry is
still a real recorded action.

One `system` writer worth knowing by name: CLI model discovery (#7384). When a
backend's live model probe fails — or degrades to a catalog that is not the
one the CLI uses — the dashboard writes `model_discovery_failed` with
`backend=…, platform=<os/arch>, fallback=…, degraded=…, served=…, error=<the
probe's own message>` — plus `state=credential_missing, recovery=<operator
action>` when the failure is copilot's runtime `Not authenticated` refusal
(#12947), naming the dashboard Copilot Login that clears it — and
`model_discovery_recovered` (with `previous=` the failure it recovered from)
once a clean probe answers again. Entries are written on transitions only,
never per 30 s probe, and the ordinary not-configured fallbacks (no credential
found, no helper installed) are not failures and write nothing. The `Not
authenticated` case is different: the helper ran and refused, so it does write
a `model_discovery_failed` entry (with `state=`/`recovery=`) even though no
fleet self-report is filed for it (see `fleet-report.md`). Before this
existed, a spoke served a wrong catalog for two days and the only record was a
server log line.

## Contributor relay rows

The hub records the [contributor relay](contributor-relay.md)'s lifecycle in
this same file (hivecommons/hive#13441); there is no separate file, store or
endpoint. The rows are written from frames the hub already receives, so an
unmodified relay needs no change. Unlike the rows above, the principal is the
**contributor**, not a dashboard user: `user` is the contributor's GitHub login
(within the pseudo-user contract above — a consumer that expects an OIDC actor
key must accept a login here), `agent` is the contributor session identity
(`<contributor id>` or `<contributor id>#<session>`), and task rows carry the
task's `repo` and issue/PR `target`.

| Action | Written when | `detail` keys |
| --- | --- | --- |
| `relay_connected` | The socket authenticated (`auth_ok` sent) | `backend`, `model`, `tier`, `relay_version` |
| `relay_rejected` | A recognised contributor was refused at auth | `reason` (`revoked`, `model_not_accepted`, `effort_below_floor`, `role_claim_rejected`) |
| `relay_disconnected` | The socket closed | `reason` (`client`, `heartbeat_timeout`, `server_shutdown`, `revoked`), `uptime_s`, `tasks` |
| `relay_task_assigned` | A `task_assign` frame was sent | `task`, `generation`, `kind`, `mcp` |
| `relay_task_accepted` | The relay's first `task_accepted` for the task it holds | `task` |
| `relay_task_declined` | The relay declined an offered task in its preflight | `task`, `reason` |
| `relay_token_minted` | An installation token was issued **or re-minted** for a task and sent to the relay (assignment, mid-task refresh, resume) | `task`, `expires_at`, `installation`, `scopes` |
| `relay_mcp_lease_issued` / `relay_mcp_lease_revoked` | Reserved for the task-MCP lease lifecycle; the hub does not issue a task-MCP lease yet, so nothing writes these today | `task`, `expires_at` |
| `relay_task_completed` | `task_complete` was accepted for the held task | `task`, `pr_url` (only a PR the hub verified), `verdict`, `duration_s`, `model` as the relay reported it |
| `relay_task_failed` | `task_failed` was accepted for the held task | `task`, `class` (the normalised failure kind: `environment`, `task` or `unspecified`), `duration_s` |
| `relay_task_revoked` | The hub took a task back: operator requeue or yank, or a resume refused by the admission gate | `task`, `reason` |
| `relay_task_abandoned` | The task ended with no terminal frame: lease expired, or the socket dropped and the task was not re-adopted within the disconnect grace | `task`, `last_progress_at` (lease expiry only) |

Things to know when you query them:

- **Never a token.** `relay_token_minted` records when the credential was sent,
  how long it lives and the installation it came from — `installation` is the
  join key to GitHub's installation-token audit — but never the token itself.
  Together with the task's terminal row it bounds the window in which a given
  contributor could have written to GitHub.
- **Sensitive.** `relay_token_minted` and `relay_task_revoked` are in the
  sensitive-action vocabulary, so they count toward `sensitive_24h` and carry
  `sensitive: true`. `relay_connected` is not: it fires on every reconnect.
- **Not audited.** `task_progress`, heartbeat and `ping`/`pong` frames are
  telemetry, not actions; they would evict operator actions from the 500-entry
  ring. A connection that never identifies a contributor (missing or invalid
  registration token) writes no row either.
- **No remote address.** `relay_connected` omits the peer address; the reverse
  proxy's access log has it.
- **Volume.** Expect roughly 6–10 rows per contributor task, plus one
  `relay_token_minted` per mid-task refresh on tasks that run past the token
  refresh period.

## Rotation and retention

Rotation is handled by lumberjack:

| Setting | Value |
| --- | --- |
| Max size before rotation | 5 MB |
| Backups kept | 3 |
| Max age | 90 days |
| Compression | yes (rotated backups are `.gz`) |

**Rotation is size-triggered, so the effective lookback is a function of event
rate, not calendar time.** A busy hive rotates through its three backups in
well under 90 days; a quiet one may hold the full window. Any analysis over the
audit log should report the window it actually covered rather than assuming 90
days.

Reads that need history beyond the current file must walk the rotated and `.gz`
backups. `OutputActionsSince` does this, with a decompression cap that bounds a
malicious or corrupt `.gz` — see [security](security.md).

## `GET /api/audit`

The dashboard serves recent entries at `GET /api/audit`
(`pkg/dashboard/audit.go`, `handleAuditLog`):

- **Auth**: requires at least the read-write dashboard role; lesser roles get
  `403`.
- **Shape**: a JSON object `{"entries": [...]}` — the entries are wrapped in an
  envelope, not served as a bare array. Entries include a serve-time
  `sensitive: true` flag when their action uses the same sensitive predicate as
  the collapsed dashboard count.
- **Ordering**: newest first.
- **Cap**: at most 200 entries (`auditMaxEntries`), which is *smaller* than the
  500-entry in-memory ring — the API never returns the whole ring, let alone
  the whole file.
- **Sensitive filter**: `?sensitive=1` returns sensitive entries from the same
  24-hour window as `sensitive_24h`, scanning the in-memory ring so an entry
  older than the default 200-row page can still be found.
- **Contributor filter**: `?contributor=<login>` returns only entries whose
  `user` is that GitHub login (case-insensitive) — the principal of the
  [contributor relay rows](#contributor-relay-rows) above. It scans the
  in-memory ring like `?sensitive=1`, keeps the same 200-entry cap and the same
  role gate, and combines with `?sensitive=1` (both filters apply). The
  summary fields (`histogram`, `sensitive_24h`, `today`, `last`) still cover
  the whole ring. The contribute page's Operations **Your assignments** card
  reads the caller's own `relay_*` rows through it, and the Audit Log's
  `relay_` chip narrows the loaded page to `relay_*` actions.
- **Enrichment**: `user_name` is stamped here, on served copies only (see
  above).

## In-memory ring vs the file

The dashboard also keeps the most recent 500 entries in memory
(`auditRingCap`) for fast display. On restart the ring is **rebuilt from the
current `audit.jsonl`** — the last 500 entries of the current file only;
rotated backups are not walked (`loadFromDiskPath`). The file is the durable
record.

Consumers that need completeness must read the file (including rotated
backups), not the API surface backed by the ring.

## Related

- [Agent self-healing watchdog](agent-watchdog.md) — writes
  `watchdog-restart-observed` entries in observe mode
- [Hooks](hooks.md) — writes `hook_failed` entries when an action's sink is
  unwired
- [Security self-assessment](security-self-assessment.md) — how the audit log
  functions as the attribution substrate
