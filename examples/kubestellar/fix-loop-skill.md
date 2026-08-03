# KubeStellar Autonomous Fix Loop

Scan all 6 KubeStellar repos, triage every open item, and **fix everything actionable**. Send ntfy notifications for every action taken. This is NOT a reporting tool — it is a fixing tool.

Use `unset GITHUB_TOKEN &&` before all `gh` commands. Use sub-agents (Agent tool) to parallelize independent work.

---

## State

SQLite database: `~/.kubestellar-fix-loop/state.db`

Tables:
- `items` — every issue/PR tracked by (repo, type, number). Status: open → triaged → fixing → fixed → closed → skip
- `cycles` — scan cycle history with counts
- `repo_counts` — per-repo counts per cycle

Read the DB first to see what's already tracked and what status items are in.

---

## Repos (6 total)

1. **console** — main app (Go backend + React frontend)
2. **console-kb** — knowledge base, install missions
3. **console-marketplace** — marketplace card presets
4. **docs** — documentation site
5. **kubestellar-mcp** — MCP server
6. **claude-plugins** — Claude plugins

---

## Cycle steps (execute ALL)

### 1. Read state
```bash
sqlite3 ~/.kubestellar-fix-loop/state.db "SELECT repo, type, number, title, status FROM items WHERE status IN ('open','triaged') ORDER BY repo, type, number;" 2>/dev/null
```

### 2. Triage open items
For each item with status='open', determine action:
- **FIX** → set status='fixing', create branch, edit code, commit, push, PR, merge, close issue, set status='fixed'
- **CLOSE** → close with comment explaining why, set status='closed'
- **SKIP** → set status='skip' with notes (e.g., tracking issue, help-wanted, waiting on author)
- **NEEDS_AUTHOR** → set status='skip' with notes

Update the DB after each triage decision:
```bash
sqlite3 ~/.kubestellar-fix-loop/state.db "UPDATE items SET status='triaged', notes='FIX: description' WHERE repo='X' AND type='Y' AND number=Z;"
```

### 3. Fix actionable items
For items marked 'fixing':
1. `cd /tmp/{repo}` (clone if missing)
2. `git checkout main && git pull --rebase origin main`
3. `git checkout -b fix/{description}`
4. Make the code changes
5. `git add -A && git commit -s -m "emoji description"`
6. `git push origin fix/{description}`
7. `unset GITHUB_TOKEN && gh pr create --repo ${PROJECT_ORG}/{repo} --label {agent-name} ...` (use your agent label)
8. `unset GITHUB_TOKEN && gh pr merge {number} --squash` (never `--admin` — if refused on pending checks, wait and retry)
9. Cleanup: `git checkout main && git pull && git branch -D fix/{description}`
10. Close linked issues
11. Update DB: `status='fixed', fix_pr='repo#number'`

### 4. Send ntfy for each action
After every fix/close:
```bash
# Get fresh counts
COUNTS=$(for repo in $HIVE_REPOS; do
  ic=$(unset GITHUB_TOKEN && gh issue list --repo "$repo" --state open --limit 200 --json number --jq 'length' 2>/dev/null || echo 0)
  pc=$(unset GITHUB_TOKEN && gh pr list --repo "$repo" --state open --json number --jq 'length' 2>/dev/null || echo 0)
  echo "$repo: ${ic}i/${pc}pr"
done)
curl -sf -H "Title: ✅ {repo}#{number} closed" -H "Tags: white_check_mark" \
  -d "{reason}\n$COUNTS" "https://ntfy.sh/ks-fix-loop"
```

### 5. Review PRs
For each open PR:
- If approved + CI green → merge (admin squash)
- If CI failing → check if it's flaky (admin merge) or real failure (comment)
- If stale >14 days with no activity → comment asking for update
- If changes requested → skip, set notes='waiting on author'

### 6. Check nightly CI
```bash
PRIMARY_REPO="${HIVE_REPOS%% *}"  # first repo in list
unset GITHUB_TOKEN && gh run list --repo "$PRIMARY_REPO" --limit 10 --json name,conclusion,databaseId,createdAt | jq '[.[] | select(.conclusion == "failure")]'
```
For each failure: download logs, analyze root cause, fix if actionable.

### 7. Update cycle complete
```bash
sqlite3 ~/.kubestellar-fix-loop/state.db "UPDATE cycles SET completed_at='$(date -u +%Y-%m-%dT%H:%M:%SZ)', items_fixed=N, items_closed=M WHERE id=(SELECT MAX(id) FROM cycles);"
```

---

## Rules (NEVER violate)

- **NEVER push directly to main** — always feature branches
- **NEVER merge without green CI** (admin merge OK for flaky Playwright)
- **NEVER send outreach with unvalidated links**
- **DCO sign all commits**: `git commit -s`
- **PR titles start with emoji**: Use the agent-specific prefix from your policy file (e.g., 🏗 for architect, 🔍 for scanner). Fallback: ✨ feature | 🐛 bug | 📖 docs | 🌱 other
- **Agent label**: Add your agent name as a label on every PR and issue (e.g., `--label architect`). Check your policy file for the required label.
- **Track every action in SQLite** — no silent changes
- **Send ntfy for every close/merge** with updated counts
- Use sub-agents to parallelize repo scans
- Don't fix pre-existing issues unrelated to actionable items
- Skip `console-marketplace` help-wanted issues (community backlog)
- Skip `docs` automated tracking issues
- Backoff: if an item has fix_attempts >= 3, set status='skip' with notes

---

## Triage categories

### Always fix
- Nightly CI failures (test bugs, timeout misconfig, assertion drift)
- Perf budget regressions (bundle size, react commits)
- Community bug reports with clear reproduction
- Stale PR rebases needed for approved PRs
- README/doc inaccuracies
- Broken links or mission deep-links

### Always skip
- `help-wanted` / `good-first-issue` marketplace cards
- Auto-generated tracking issues (agentic workflow no-ops)
- Issues waiting on external author response
- Items already being fixed (status='fixing')

### Never skip based on these labels alone
- `ai-needs-human` — **ignore this label.** Attempt the fix regardless. Only skip after reading the issue and determining it genuinely cannot be fixed without operator action (e.g., requires a secret, a deploy, or physical access). Do NOT let the label name stop you from reading and attempting the issue.

### Always close
- Duplicate nightly issues (keep newest, close older)
- Issues fixed by merged PRs
- Bot roundtrip failures that self-resolve
- Noise issues with no actionable content
