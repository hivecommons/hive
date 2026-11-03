# Alternative state backend: SQLite

The main examples use [beads](https://github.com/steveyegge/beads) (`bd` CLI) as a git-backed coordination ledger. This works well for multi-host, multi-agent setups where you need structured work claims and actor-level ownership.

For **single-machine deployments** — one Mac or one VM running all your agents — SQLite is a simpler, faster alternative that requires no extra tooling.

---

## When to use SQLite vs beads

| Dimension | SQLite | beads |
|---|---|---|
| Setup | Zero — ships with macOS/most Linux | `curl` + `tar` + `bd init` |
| Multi-host sync | Manual (rsync, Syncthing) | Git-backed (built-in) |
| Multi-agent claims | `UPDATE … WHERE status='open' LIMIT 1` (advisory, not atomic across processes) | `bd update --claim` (atomic) |
| Query power | Full SQL — joins, aggregates, window functions | `bd list --json \| jq` |
| Schema | You define it — full control | Fixed bead schema + metadata KV |
| Tooling dependency | `sqlite3` (preinstalled) | `bd` binary |
| Best for | Single-machine, custom schema, high scan volume | Multi-machine, structured actor ownership |

**Rule of thumb**: if all your agents run on the same machine and you want full SQL, use SQLite. If agents span machines or you want the actor/claim/sync semantics built-in, use beads.

---

## Reference schema

```sql
CREATE TABLE IF NOT EXISTS items (
  repo TEXT NOT NULL,
  type TEXT NOT NULL,           -- 'issue' or 'pr'
  number INTEGER NOT NULL,
  title TEXT,
  author TEXT,
  created_at TEXT,
  status TEXT DEFAULT 'open',   -- open, triaged, fixing, fixed, closed, skip
  last_seen TEXT,
  fix_attempts INTEGER DEFAULT 0,
  fix_pr TEXT,                  -- 'repo#number' of the fix PR
  notes TEXT,
  PRIMARY KEY (repo, type, number)
);

CREATE TABLE IF NOT EXISTS cycles (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  started_at TEXT NOT NULL,
  completed_at TEXT,
  total_issues INTEGER,
  total_prs INTEGER,
  items_fixed INTEGER DEFAULT 0,
  items_closed INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS repo_counts (
  repo TEXT NOT NULL,
  cycle_id INTEGER NOT NULL,
  issues INTEGER,
  prs INTEGER,
  PRIMARY KEY (repo, cycle_id)
);
```

### Status flow

```
open → triaged → fixing → fixed
                        ↘ closed
open → skip (with notes explaining why)
open → closed (detected as no longer open on GitHub)
```

### Key queries

```bash
# Items ready for the AI agent to work on
sqlite3 state.db "SELECT repo, type, number, title FROM items WHERE status IN ('open','triaged') ORDER BY created_at ASC;"

# Cycle history (last 10)
sqlite3 state.db "SELECT id, started_at, total_issues, total_prs, items_fixed, items_closed FROM cycles ORDER BY id DESC LIMIT 10;"

# Per-repo current counts
sqlite3 state.db "SELECT repo, SUM(CASE WHEN status='open' THEN 1 ELSE 0 END) as open, SUM(CASE WHEN status='fixed' THEN 1 ELSE 0 END) as fixed FROM items GROUP BY repo;"

# Items that failed 3+ fix attempts (backoff candidates)
sqlite3 state.db "SELECT repo, type, number, title, fix_attempts, notes FROM items WHERE fix_attempts >= 3;"
```

---

## Scanner writes, agent reads

The recommended pattern is a **separation of concerns**:

1. **Scanner** (bash script on a timer) — scans repos, upserts findings into SQLite, detects closures, sends ntfy summaries. No AI needed. See [`worker.sh.example`](worker.sh.example).

2. **Agent** (AI in tmux or triggered by skill) — reads the SQLite DB, triages items, makes fixes, updates status. Uses full reasoning.

This separation means:
- Scanning continues even when the AI session is down or restarting
- State is never lost — it's in a durable file, not the AI's context window
- The agent can start any time and immediately know what needs work
- You get a complete audit trail (`cycles` table) of every scan

---

## Connecting to Copilot CLI skills

If your AI agent is Claude Code, you can use [Copilot CLI skills](https://docs.github.com/en/copilot/how-tos/copilot-on-github/customize-copilot/add-custom-instructions/add-repository-instructions) (`.claude/commands/*.md`) as the policy delivery mechanism instead of raw markdown files in memory.

The skill file reads the SQLite DB and executes the fix loop:

```markdown
# Fix Loop Skill (.claude/commands/fix-loop.md)

Read the state database and fix everything actionable.

## State

SQLite database: `~/.my-project-fix-loop/state.db`

## Steps

1. Read all items with status IN ('open', 'triaged')
2. For each: triage → fix → commit → PR → merge → update status
3. Send ntfy notification for each action

## Rules
- Always use feature branches
- DCO-sign all commits
- Track every action in SQLite
```

The operator (or launchd) triggers the skill after a scan cycle completes, or the agent picks it up on its next `/loop` iteration.

---

## Coordination with beads (hybrid)

You can use both — SQLite for high-volume scan state, beads for cross-agent work coordination:

```
scanner.sh → SQLite (raw scan data, cycle history, repo counts)
                ↓
agent reads SQLite, triages, files beads for cross-agent work
                ↓
beads ledger (work claims, lane transfers, peer supervision)
```

This makes sense when your scanner produces hundreds of data points per cycle but only a few turn into actionable cross-agent work items.
