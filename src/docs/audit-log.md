# Audit log format (`/data/audit.jsonl`)

The audit log is the accountability record for actions a hive takes: who did
what, when, and to which agent. Several docs reference it — the
[watchdog](agent-watchdog.md) records observed restarts here, [hooks](hooks.md)
record firing failures here, and the
[security self-assessment](security-self-assessment.md) names it as the
attribution substrate — but until now none of them defined its shape.

This page defines it, so a consumer can parse the file without reading Go.

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

Two fields are omitted entirely rather than emitted empty, so **a consumer must
treat a missing `detail` or `agent` as absent, not as an empty string**.

## Parsing `detail`

`detail` is a flat string of `key=value` pairs joined by `, ` — not a nested
object. It is built by `InvocationMeta.AuditDetail` from a generic pair list, so
**the keys present vary by action** and there is no typed accessor.

```
repo=hivecommons/hive, number=4911, agent=guide, backend=copilot
```

Consequences worth knowing before you build on it:

- **`repo` is not a first-class field.** It appears only because each call site
  passes it explicitly. A new audit site that forgets it drops out of any
  repo-based analysis silently, with nothing failing.
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

## In-memory ring vs the file

The dashboard also keeps the most recent 500 entries in memory for fast display.
That ring is **capped and reset on restart**; the file is the durable record.

Consumers that need completeness must read the file, not the API surface backed
by the ring.

## Related

- [Agent self-healing watchdog](agent-watchdog.md) — writes
  `watchdog-restart-observed` entries in observe mode
- [Hooks](hooks.md) — writes `hook_failed` entries when an action's sink is
  unwired
- [Security self-assessment](security-self-assessment.md) — how the audit log
  functions as the attribution substrate
