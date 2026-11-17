# `bd` beads CLI reference

`bd` is the operator/contributor CLI for Hive's bead work ledger. It stores data in `BD_DIR`, or in the current agent's conventional bead directory (`/data/beads/<agent>` or `/data/agents/<agent>` → `/data/beads/<agent>`). Stores are created on first use.

## Work ledger commands

| Command | Use |
|---|---|
| `bd list [--json] [--status <s>] [--actor <a>]` | List beads; filter by status or actor. |
| `bd ready [--json] [--actor <a>]` | Show open, unblocked work ready to claim. |
| `bd create --title "..." --type <type> --priority <0-4> --actor <name> [--external-ref <ref>]` | Create work. Types include `bug`, `feature`, `task`, `epic`, `chore`, `decision`, and `advisory`; priority `0` is highest. `--type knowledge` creates a knowledge fact instead; see [Knowledge commands](#knowledge-commands). |
| `bd update <id> --claim` | Mark a bead `in_progress`. |
| `bd update <id> --status open|in_progress|blocked|done|closed` | Change status directly. |
| `bd update <id> --set-metadata key=value` / `--unset-metadata key` | Add or remove custom metadata in the bead ledger. |
| `bd close <id>` | Close one bead. |
| `bd reset [--reason "..."]` | Close all open/in-progress/blocked beads with an audit reason. |
| `bd remember "fact"` | Quick-add an advisory bead authored by `system`. |
| `bd init` | No-op compatibility command; opening the store creates it. |
| `bd dolt push` | No-op compatibility command; bead data is already persisted on disk. |
| `bd topic claim <key> --repo <owner/name> --pr <n> [--branch <b>] [--actor <a>]` | Claim a topic for one pull request. See [Topic claims](#topic-claims). |
| `bd topic get <key> [--json]` / `bd topic list [--json]` / `bd topic release <key>` | Read, list or release topic claims. |
| `bd decompose <epic-id> [--plan <file>] [--actor <lane>] [--auto-approve] [--print-prompt]` | Decompose an epic bead into a DAG of child task beads. See below. |

## Topic claims

A long-lived agent that owns one ongoing topic (a dependency-bump PR, a release
PR) records which pull request carries it, so it finds that PR by number on
every later kick instead of by branch name (hivecommons/hive#18493).

```bash
bd topic claim deps-bump --repo acme/widget --pr 412 --branch deps/bump-go
bd topic get deps-bump          # deps-bump → acme/widget#412 (branch deps/bump-go)
bd topic list --json
bd topic release deps-bump
```

- The claim is one `in_progress` chore bead in the agent's own store, with
  `external_ref` `topic:<key>` and metadata `topic_key`, `topic_repo`,
  `topic_pr`, `topic_branch` and `topic_state`. Keys are lowercased and may
  not contain whitespace.
- At most one claim per key is open. Claiming an open key again for the same
  PR is idempotent and updates the branch; claiming it for a different PR is
  refused with exit code `2` and names the PR that holds it. `bd topic get`
  also exits `2` when no claim is open.
- `--actor` defaults to `$HIVE_AGENT`.
- Hive releases a claim when its PR merges or closes (`topic_state` becomes
  `merged` or `closed`): on the hive's own merge or close, on an observed
  terminal PR, and on each eval cycle's snapshot. The same eval-cycle pass
  updates `topic_branch` when the PR's head branch was renamed.
  `bd topic release` records `released`. `bd reset` closes open claims too.
- A sandboxed kick has no `bd`: it runs `hive-topic` with the same arguments,
  which hive applies to the agent's store through the sandbox request spool.
  `hive-topic claim <key>` without `--pr` claims the PR hive opens from the
  kick; the claim is pending until that PR exists. See
  [sandbox-isolation.md](sandbox-isolation.md#issue-and-topic-requests-through-the-spool).
- The kick shows an agent its open claims in a `📌 Topic claims` block, also
  available to kick templates as `${TOPIC_CLAIMS}` (which renders the usage
  lines even when the agent holds no claim). The FIX-BEFORE-NEW and hold-gated
  PR blocks tag a claimed PR with `[topic: <key>]`.

## Decomposing an epic

`bd decompose` turns one epic bead into a graph of child task beads, wiring
their dependencies so `bd ready` only offers a child once its predecessors
close.

```bash
bd decompose <epic-id> --plan plan.txt
cat plan.txt | bd decompose <epic-id>          # or read the plan from stdin
```

| Flag | Effect |
|---|---|
| `--plan <file>` | File holding the planner's task list. Defaults to stdin. |
| `--actor <lane>` | Override the child beads' actor/lane. Defaults to `classifier`/`architect`. |
| `--auto-approve` | Approve the plan immediately — children are claimable at once, with no review gate. |
| `--print-prompt` | Print the architect decomposition prompt for this epic and exit without writing anything. |

**This does not run an agent.** The task breakdown is read from `--plan` or
stdin, so the trigger stays manual and the planning package carries no
agent/tmux dependency. Wiring the architect lane's live output in place of the
file/stdin source is later-phase work — see `cmdDecompose` in
`src/cmd/bd/decompose.go`.

Without `--auto-approve` the children land behind a review gate, so use
`--print-prompt` first when you want to see what an epic would expand into
before committing anything to the ledger.

Examples:

```bash
export BD_DIR=/data/beads/scanner
bd ready --actor scanner
bd create --title "Document backup restore" --type task --priority 2 --actor guide --external-ref https://github.com/hivecommons/hive/issues/2986
bd update bead-123 --claim
bd update bead-123 --set-metadata reviewer=guide
bd close bead-123
```

`--type advisory` is the one type that **upserts**: re-creating an advisory bead whose title matches an open one refreshes that bead's last-seen stamp (and raises its priority if the new report is more severe) instead of adding a duplicate. That re-report is what keeps the finding alive — an advisory bead nobody re-files within `governor.advisory.staleness_days` is auto-closed. See [Advisory digest](advisory.md).

## Knowledge commands

`bd kb` and `bd create --type knowledge` talk to the dashboard API (`BD_DASHBOARD_URL`, default `http://localhost:3001`) rather than the local bead store.

| Command | Use |
|---|---|
| `bd kb search "query"` | Search knowledge facts. |
| `bd kb read <slug>` | Print the body for a fact slug returned by search. |
| `bd kb import-url <url> [--name <n>]` | Import a document URL into the project layer. |
| `bd kb import-file <path> [--name <n>]` | Import a local document path into the project layer. |
| `bd kb ctx7-search <name>` | Search Context7 library IDs. |
| `bd kb import-ctx7 <library-id> [--name <n>] [--query <topic>]` | Import Context7 docs into the community layer. |
| `bd kb list-docs` | List imported documents. |
| `bd create --type knowledge --title "..." (--body "..." \| --body-file <path\|->) [--state draft\|approved] [--tag <tag>]... [--layer <name>] [--fact-type <type>]` | Create one knowledge fact through `POST /api/knowledge/create`. Defaults: layer `project`, fact type `pattern`, state `approved`. `--body-file -` reads stdin (256 KiB max). |

`--state draft` writes the fact into the local channel (vault) named by `--layer`, with `state: draft`, so it stays out of every kick until an operator approves it (`hivectl knowledge state <id> approved` or `PUT /api/knowledge/entry/{id}/state`). Remote wiki layers only record a status, so the dashboard refuses a draft for a layer that is not a local channel, and it refuses a draft whose title already names an entry in that channel (HTTP 409) rather than overwrite it. Only `draft` and `approved` are accepted at create time.

A fact tagged `cli-memory` claims to come from a CLI's memory store ([memory → knowledge drain](agent-knowledge-and-memory.md)). The dashboard accepts one only as `--state draft`, and only with the tags the drain itself writes: `cli-memory`, `agent:<name>`, `cli:<claude|codex|gemini>`, `memhash:<hex>` and `repo:<org>/<repo>`.

```bash
bd create --type knowledge --state draft --layer notes \
  --title "Read the failing job log first" --body-file note.md \
  --tag cli-memory --tag agent:scanner --tag cli:codex
```
