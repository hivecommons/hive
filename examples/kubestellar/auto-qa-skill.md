## Auto-QA: Autonomous Auto-QA PR Processor

Continuously discover and fix all open auto-qa issues and stalled copilot PRs on `${PROJECT_PRIMARY_REPO}`. Work through every item without asking — the user will review and merge PRs separately.

Use `unset GITHUB_TOKEN &&` before all `gh` commands. Use sub-agents (Agent tool) to parallelize independent work.

---

### 1. Discovery — Find All Open Auto-QA Work

```bash
# Open auto-qa issues
unset GITHUB_TOKEN && gh issue list --repo ${PROJECT_PRIMARY_REPO} --label "auto-qa" --state open --json number,title,labels,createdAt --limit 50

# Open PRs from copilot on auto-qa branches
unset GITHUB_TOKEN && gh pr list --repo ${PROJECT_PRIMARY_REPO} --state open --search "[Auto-QA]" --json number,title,headRefName,author,labels,additions,deletions,createdAt --limit 50

# Also check for GA4-Error issues (same workflow)
unset GITHUB_TOKEN && gh issue list --repo ${PROJECT_PRIMARY_REPO} --label "ga4-error" --state open --json number,title,labels,createdAt --limit 50

# Check for existing fix/auto-qa-* PRs by ${PROJECT_AI_AUTHOR} (already being worked on — skip these)
unset GITHUB_TOKEN && gh pr list --repo ${PROJECT_PRIMARY_REPO} --state open --search "auto-qa author:${PROJECT_AI_AUTHOR}" --json number,title,headRefName --limit 50
```

**Triage rules (no user interaction needed):**
- **Stalled copilot PR** = PR by `copilot-swe-agent` with `size/XS` label (0 additions, 0 deletions). → **Takeover.**
- **Issue with no PR** = Auto-QA or GA4-Error issue with no linked open PR. → **New fix.**
- **Issue with existing `fix/auto-qa-*` PR by ${PROJECT_AI_AUTHOR}** = Already being worked on. → **Skip.**
- **Active copilot PR with real changes** (additions > 0) = Copilot is working. → **Skip.**
- **Issue with `ai-needs-human` label** = Still attempt a fix. Only skip if the fix truly requires human judgment after reading the issue.

Print a brief summary table of all items found and which action will be taken, then immediately start processing — **do NOT wait for user input**.

Process items in this priority order:
1. GA4 bug fixes (`ga4-error`) — these are production errors
2. UI flicker fixes (`auto-qa:flicker`) — user-visible quality issues
3. Efficiency improvements (`auto-qa:nfr`)
4. Code centralization (`auto-qa:centralization`)
5. High complexity (`auto-qa:features`)
6. UI design (`auto-qa:ui-design`)

---

### 2. For Each Item — Autonomous Workflow

#### 2a. Close Stalled Copilot PR (if one exists for this issue)

**CRITICAL:** The copilot PR body contains `Fixes #XXXX` or `Closes #XXXX` which auto-closes the linked issue when the PR is closed. You MUST strip these references BEFORE closing.

```bash
# Step 1: Get the current PR body
unset GITHUB_TOKEN && gh pr view <PR_NUMBER> --repo ${PROJECT_PRIMARY_REPO} --json body -q .body > /tmp/pr_body_<PR_NUMBER>.txt

# Step 2: Remove all auto-close keywords (Fixes/Closes/Resolves + # + number)
# Use perl for reliable in-place editing with case-insensitive replace
perl -pi -e 's/(Fixes|Closes|Resolves)\s+#(\d+)/Related to #$2/gi' /tmp/pr_body_<PR_NUMBER>.txt

# Step 3: Update the PR body
unset GITHUB_TOKEN && gh pr edit <PR_NUMBER> --repo ${PROJECT_PRIMARY_REPO} --body "$(cat /tmp/pr_body_<PR_NUMBER>.txt)"

# Step 4: Now safe to close
unset GITHUB_TOKEN && gh pr close <PR_NUMBER> --repo ${PROJECT_PRIMARY_REPO} --comment "Closing stalled copilot PR. Taking over with a manual fix."

# Step 5: Delete the copilot branch
unset GITHUB_TOKEN && git push origin --delete copilot/<branch-name>
```

#### 2b. Read the Issue

```bash
unset GITHUB_TOKEN && gh issue view <ISSUE_NUMBER> --repo ${PROJECT_PRIMARY_REPO} --json body,title,labels
```

Parse the issue to extract:
- Affected files and line numbers
- Specific findings (consecutive setState, missing loading indicators, etc.)
- Suggested improvements

#### 2c. Create Worktree and Branch

**ALWAYS use a git worktree.** Never work on the main checkout.

```bash
cd /tmp/kubestellar-console
git fetch origin main
git worktree add /tmp/kubestellar-console-auto-qa-<issue-number> -b fix/auto-qa-<issue-number> origin/main
cd /tmp/kubestellar-console-auto-qa-<issue-number>
```

#### 2d. Implement the Fix

Read the affected files and implement changes. Follow these principles:

- **Keep PRs small and focused** — One category of fix per PR. If an issue has multiple categories (e.g., setState batching AND missing loading indicators), pick the highest-impact category and fix that. The remaining categories will be picked up in the next run.
- **Don't over-engineer** — Only fix what's flagged. Don't refactor surrounding code.
- **Verify imports** — When adding hooks like `useReducer`, `useCallback`, `useMemo`, verify they exist in the import.
- **No magic numbers** — Use named constants for any numeric values.
- **Guard against undefined** — Use `(arr || []).join()` and `for (const x of (data || []))` patterns.
- **isDemoData wiring** — If touching cards with `useCached*` hooks, ensure `isDemoData` is destructured and passed to `useCardLoadingState()`.
- **DeduplicatedClusters()** — If iterating clusters, always use `DeduplicatedClusters()`.
- **Run build and lint:**

```bash
cd /tmp/kubestellar-console-auto-qa-<issue-number>/web
npm install  # worktrees share git but not node_modules
npm run build
npm run lint
```

If build or lint fails, fix the errors and retry. If stuck after 3 attempts on the same error, skip this item and move to the next.

#### 2e. Commit and Create PR

```bash
cd /tmp/kubestellar-console-auto-qa-<issue-number>
git add <specific-files>
git commit -s -m "$(cat <<'EOF'
fix: <description of what was fixed>

Fixes #<ISSUE_NUMBER>
EOF
)"
unset GITHUB_TOKEN && git push -u origin fix/auto-qa-<issue-number>
```

Create the PR:

```bash
unset GITHUB_TOKEN && gh pr create --repo ${PROJECT_PRIMARY_REPO} \
  --title "<emoji> <Short description>" \
  --body "$(cat <<'EOF'
## Summary
- <bullet points describing changes>
- Fixes auto-qa findings from issue #<ISSUE_NUMBER>

## Test plan
- [x] `npm run build` passes
- [x] `npm run lint` passes
- [ ] Visual regression check for affected components

Fixes #<ISSUE_NUMBER>
EOF
)"
```

**PR title emoji rules:**
- `🐛` for bug fixes (flicker, errors, crashes, GA4 errors)
- `✨` for enhancements (performance improvements, code centralization)
- `⚠️` for breaking changes

#### 2f. Move to Next Item

After creating the PR, immediately move to the next item. Do NOT wait for review or CI. The user will review and merge PRs on their own schedule.

---

### 3. Auto-QA Issue Categories — Fix Reference

**UI Flicker (`auto-qa:flicker`)**
- Consecutive `setState` calls → Batch into single update or use `useReducer`
- Missing loading indicators → Add skeleton/spinner components
- `useLayoutEffect` misuse → Only use for DOM measurements

**Code Centralization (`auto-qa:centralization`)**
- Repeated layout patterns → Extract to shared components (only if 5+ occurrences)
- Inconsistent hook usage → Migrate to standardized hooks
- Modal state patterns → Use shared `useModal` hook (only if creating one is justified)

**Efficiency (`auto-qa:nfr`)**
- Inline style objects → Move to constants or `useMemo`
- Inline arrow functions → Wrap in `useCallback` when passed as props
- Full library imports → Use named imports in test files

**GA4 Errors (`ga4-error`)**
- `uncaught_render` → Find and fix rendering errors (check error boundaries, null guards)
- `chunk_load` → Fix lazy loading / code splitting (add retry logic, improve chunk error boundary)
- `unhandled_rejection` → Add proper `.catch()` or try/catch for promises

**High Complexity (`auto-qa:features`)**
- Split large components into smaller, focused sub-components
- Extract complex logic into custom hooks

**UI Design (`auto-qa:ui-design`)**
- Inconsistent component patterns → Standardize to shared components (e.g., use `<Button>` instead of raw `<button>`)

---

### 4. Final Report

After ALL items are processed, print a summary:

| # | Issue | Action | PR Created | Build | Lint |
|---|-------|--------|------------|-------|------|

Include links to all created PRs. Note any items that were skipped and why.
