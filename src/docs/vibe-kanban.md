# vibe-kanban mirror (report-only)

[vibe-kanban](https://github.com/BloopAI/vibe-kanban) is a local kanban board for coding-agent CLIs. It has no issue intake, triage, or admission. Hive has all three but no per-developer board. `bin/vibe-kanban-mirror.js` copies hive's admitted work for **one repository** onto a vibe-kanban project so a developer can see it on the board.

This covers Phases 0 and 1 of [#10641](https://github.com/hivecommons/hive/issues/10641) and works **one way only**. Hive still owns admission and acceptance, and the board is only a view. The mirror never writes to hive, and moving a card on the board has no effect on hive's queue. With a shadow log configured (Phase 1), the mirror also records when a developer picks a queued item up on the board; see [Observing board pick-ups](#observing-board-pick-ups-shadow). An extwork host adapter that lets hive dispatch a vibe-kanban workspace (Phase 2) is still open on the proposal.

## What it reads and writes

| Side | Surface | Notes |
| --- | --- | --- |
| Read (hive) | `GET /api/contribute/queue` | The ready-work queue: the same admissible set the hub offers from. Rows the operator has held count as *parked*. Public and read-only. |
| Read (hive) | `GET /api/contribute/fleet` | In-flight leases (`work[]`, `kind: issue`). Public and read-only. |
| Write (board) | vibe-kanban MCP stdio server | `list_tags`, `list_issues`, `create_issue`, `update_issue`, `add_issue_tag`. |
| Write (local) | `VIBE_KANBAN_SHADOW_LOG` | Optional. A local JSONL file of shadow pick-up observations. Nothing is sent to hive. |

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

## Observing board pick-ups (shadow)

When a developer starts a vibe-kanban workspace on a mirrored issue (`start_workspace` with the issue linked), vibe-kanban runs *their* executor locally and moves the card forward. Set `VIBE_KANBAN_SHADOW_LOG` to a file path to have the mirror record that pick-up as a **shadow** external execution, the same shape as the OMP and Flue shadow modes in [External workflow admission](design/external-workflow-admission.md).

The mirror only counts items that hive still reports as `queued`. A `leased` item is already hive's own execution, and a `parked` item is held by the operator. A queued item counts as picked up when either of these is true:

| Board signal | Recorded extwork state |
| --- | --- |
| Card in the `inprogress` column | `running` |
| Card in the `inreview` column, or a linked PR that is open | `waiting` |
| Card in the `done` column, or the linked PR merged or closed | `terminal` |

Each observation is appended to the shadow log as one JSON line in the `extwork.ProgressEvent` shape plus `observed_at`:

```json
{"action":"ext_work_shadow_observed","execution_key":"vibe-kanban:<project uuid>/<issue uuid>","assignment_id":"","state":"running","fields":{"external_start":false,"host":"vibe-kanban","work_key":"acme/widgets#42","board_issue_id":"<issue uuid>","board_status":"In progress","simple_id":"VK-12"},"observed_at":"2026-10-06T00:00:00.000Z"}
```

Observation is read-only on both sides:

- Hive dispatches nothing. No second run starts, and the mirror never calls `start_workspace` or any session tool.
- Nothing is written to hive. Acceptance still requires hive's own receipt or PR path.
- The log records a fact only when it changes (state, column, or linked PR) and re-reads the file on start, so reruns and restarts add no duplicate lines.
- A picked-up card stays in the developer's column. Without a shadow log, the Phase 0 behaviour applies and the next pass moves the card back to the hive-mapped column.
- `--dry-run` prints `would record` lines and leaves the log alone.

The vibe-kanban MCP surface does not expose which workspace or execution belongs to an issue: `list_workspaces` returns no linked issue, and `get_execution` needs an execution ID that the MCP tools do not list per issue. The mirror therefore reads the pick-up from the issue itself (its column and its linked PR). Correlating the native workspace and execution is left to the Phase 2 host adapter.

## Enabling it

The mirror is **off by default**. It does nothing and exits 0 unless both `VIBE_KANBAN_MCP_CMD` and `VIBE_KANBAN_PROJECT_ID` are set. It needs no new credentials: both hive endpoints are public reads, and the MCP server runs locally.

| Variable | Flag | Meaning |
| --- | --- | --- |
| `VIBE_KANBAN_MCP_CMD` | `--mcp-cmd` | Command that starts the MCP server, usually `npx -y vibe-kanban@latest --mcp`. It is split on whitespace and quotes and is never passed to a shell. |
| `VIBE_KANBAN_PROJECT_ID` | `--project-id` | UUID of the vibe-kanban project to mirror into. `list_projects` on the MCP server lists the available projects. |
| `VIBE_KANBAN_REPO` | `--repo` | The single `owner/name` repository to mirror. Required once the mirror is enabled. |
| `HIVE_DASHBOARD_URL` | `--hive-url` | Base URL of the hive dashboard or hub to read from. |
| `VIBE_KANBAN_SYNC_INTERVAL_S` | `--interval` | Seconds between passes in loop mode. Default 300. |
| `VIBE_KANBAN_SHADOW_LOG` | `--shadow-log` | Optional. Path of a local JSONL file where board pick-ups of queued items are recorded as shadow executions. Unset means no observation. |
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

```bash
export VIBE_KANBAN_SHADOW_LOG="$HOME/.hive/vibe-kanban-shadow.jsonl"
node bin/vibe-kanban-mirror.js --once             # also records board pick-ups
```

## Tests

`bin/vibe-kanban-mirror.test.js` runs in CI against a fake MCP stdio server, `bin/testdata/vibe-kanban-mcp/fake-server.js`. The fake keeps its board state on disk, which lets the tests show that a second `--once` pass makes no changes. The tests also edit that state between passes to simulate a developer picking a card up, and check that the pick-up is recorded once, kept on the board, and never dispatched.

## Related

- [Integration guide](integration-guide.md)
- [External workflow admission](design/external-workflow-admission.md): the report-only gating this mirror follows
- [Hub API reference](api-reference.md)
