# `bd` beads CLI reference

`bd` is the operator/contributor CLI for Hive's bead work ledger. It stores data in `BD_DIR`, or in the current agent's conventional bead directory (`/data/beads/<agent>` or `/data/agents/<agent>` → `/data/beads/<agent>`). Stores are created on first use.

## Work ledger commands

| Command | Use |
|---|---|
| `bd list [--json] [--status <s>] [--actor <a>]` | List beads; filter by status or actor. |
| `bd ready [--json] [--actor <a>]` | Show open, unblocked work ready to claim. |
| `bd create --title "..." --type <type> --priority <0-4> --actor <name> [--external-ref <ref>]` | Create work. Types include `bug`, `feature`, `task`, `epic`, `chore`, `decision`, and `advisory`; priority `0` is highest. |
| `bd update <id> --claim` | Mark a bead `in_progress`. |
| `bd update <id> --status open|in_progress|blocked|done|closed` | Change status directly. |
| `bd update <id> --set-metadata key=value` / `--unset-metadata key` | Add or remove custom metadata in the bead ledger. |
| `bd close <id>` | Close one bead. |
| `bd reset [--reason "..."]` | Close all open/in-progress/blocked beads with an audit reason. |
| `bd remember "fact"` | Quick-add an advisory bead authored by `system`. |
| `bd init` | No-op compatibility command; opening the store creates it. |
| `bd dolt push` | No-op compatibility command; v2 data is already persisted on disk. |

Examples:

```bash
export BD_DIR=/data/beads/scanner
bd ready --actor scanner
bd create --title "Document backup restore" --type task --priority 2 --actor guide --external-ref https://github.com/kubestellar/hive/issues/2986
bd update bead-123 --claim
bd update bead-123 --set-metadata reviewer=guide
bd close bead-123
```

## Knowledge commands

`bd kb` talks to the dashboard API (`BD_DASHBOARD_URL`, default `http://localhost:3001`) rather than the local bead store.

| Command | Use |
|---|---|
| `bd kb search "query"` | Search knowledge facts. |
| `bd kb read <slug>` | Print the body for a fact slug returned by search. |
| `bd kb import-url <url> [--name <n>]` | Import a document URL into the project layer. |
| `bd kb import-file <path> [--name <n>]` | Import a local document path into the project layer. |
| `bd kb ctx7-search <name>` | Search Context7 library IDs. |
| `bd kb import-ctx7 <library-id> [--name <n>] [--query <topic>]` | Import Context7 docs into the community layer. |
| `bd kb list-docs` | List imported documents. |
