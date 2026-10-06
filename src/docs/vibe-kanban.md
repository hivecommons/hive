# vibe-kanban mirror (report-only)

[vibe-kanban](https://github.com/BloopAI/vibe-kanban) is a local kanban board for coding-agent CLIs. It has no issue intake, triage, or admission. Hive has all three but no per-developer board. `bin/vibe-kanban-mirror.js` copies hive's admitted work for **one repository** onto a vibe-kanban project so a developer can see it on the board.

This is Phase 0 of [#10641](https://github.com/hivecommons/hive/issues/10641) and works **one way only**. Hive still owns admission and acceptance, and the board is only a view. The mirror never writes to hive, and moving a card on the board has no effect on hive. The later phases, observing board pick-ups as shadow executions and an extwork host adapter, are still open on the proposal.

## What it reads and writes

| Side | Surface | Notes |
| --- | --- | --- |
| Read (hive) | `GET /api/contribute/queue` | The ready-work queue: the same admissible set the hub offers from. Rows the operator has held count as *parked*. Public and read-only. |
| Read (hive) | `GET /api/contribute/fleet` | In-flight leases (`work[]`, `kind: issue`). Public and read-only. |
| Write (board) | vibe-kanban MCP stdio server | `list_tags`, `list_issues`, `create_issue`, `update_issue`, `add_issue_tag`. |

The mirror does **not** write to vibe-kanban's SQLite database or call its undocumented HTTP routes. The MCP server is the only surface upstream documents. vibe-kanban is a board host, not a CLI backend, so it is not a `KNOWN_BACKENDS` entry.

## Status mapping

| Hive state | Board status key | Default column name |
| --- | --- | --- |
| `queued` | `todo` | `To do` |
| `leased` | `inprogress` | `In progress` |
| `review` | `inreview` | `In review` |
| `merged` / `closed` | `done` | `Done` |
| `parked` (operator hold) | `cancelled` | `Cancelled` |

The read path above currently reports `queued`, `parked`, and `leased`. If an item drops out of both views, its board card stays as it is. The mirror does not guess from a missing item whether the work merged, closed, or cooled down. `update_issue` matches column names case-insensitively. For a project with renamed columns, set `VIBE_KANBAN_STATUS_NAMES` to a JSON object, for example `{"todo":"Backlog"}`.

## Idempotency

vibe-kanban issues do not have an external-id field. Each mirrored issue's description therefore ends with a trailer that holds the hive work key:

```text
https://github.com/acme/widgets/issues/42

Mirrored from hive (report-only). Hive owns admission and acceptance; changes made on this board are not written back.

[hive-work-key: acme/widgets#42]
```

On each pass the mirror searches for that trailer with `list_issues`. If no issue matches, it creates one, moves it to the mapped column, and adds the project's `hive` tag. If an issue matches, it updates the issue only when the status or title has changed. A rerun with no hive changes makes no board writes. The brackets keep the match exact, so `#4]` never matches `#42]`.

Create a tag named `hive` in the vibe-kanban project to have mirrored issues tagged. If the tag is missing, the mirror logs a warning and still mirrors the issues.

## Enabling it

The mirror is **off by default**. It does nothing and exits 0 unless both `VIBE_KANBAN_MCP_CMD` and `VIBE_KANBAN_PROJECT_ID` are set. It needs no new credentials: both hive endpoints are public reads, and the MCP server runs locally.

| Variable | Flag | Meaning |
| --- | --- | --- |
| `VIBE_KANBAN_MCP_CMD` | `--mcp-cmd` | Command that starts the MCP server, usually `npx -y vibe-kanban@latest --mcp`. It is split on whitespace and quotes and is never passed to a shell. |
| `VIBE_KANBAN_PROJECT_ID` | `--project-id` | UUID of the vibe-kanban project to mirror into. `list_projects` on the MCP server lists the available projects. |
| `VIBE_KANBAN_REPO` | `--repo` | The single `owner/name` repository to mirror. Required once the mirror is enabled. |
| `HIVE_DASHBOARD_URL` | `--hive-url` | Base URL of the hive dashboard or hub to read from. |
| `VIBE_KANBAN_SYNC_INTERVAL_S` | `--interval` | Seconds between passes in loop mode. Default 300. |
| `VIBE_KANBAN_STATUS_NAMES` | — | Optional JSON object that overrides column names (keys: `todo`, `inprogress`, `inreview`, `done`, `cancelled`). |

```bash
export VIBE_KANBAN_MCP_CMD='npx -y vibe-kanban@latest --mcp'
export VIBE_KANBAN_PROJECT_ID='<project uuid>'
export VIBE_KANBAN_REPO='acme/widgets'
export HIVE_DASHBOARD_URL='https://hive.example.org'

node bin/vibe-kanban-mirror.js --once --dry-run   # print the plan, change nothing
node bin/vibe-kanban-mirror.js --once             # one pass
node bin/vibe-kanban-mirror.js                    # loop every VIBE_KANBAN_SYNC_INTERVAL_S
```

`--dry-run` still reads the board (`list_tags` and `list_issues`) so that its plan is accurate, but it never creates, updates, or tags an issue. With `--once`, the exit code is 1 if any item failed to sync and 0 otherwise.

## Tests

`bin/vibe-kanban-mirror.test.js` runs in CI against a fake MCP stdio server, `bin/testdata/vibe-kanban-mcp/fake-server.js`. The fake keeps its board state on disk, which lets the tests show that a second `--once` pass makes no changes.

## Related

- [Integration guide](integration-guide.md)
- [External workflow admission](design/external-workflow-admission.md): the report-only gating this mirror follows
- [Hub API reference](api-reference.md)
