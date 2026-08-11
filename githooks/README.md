# Git hooks

This directory contains optional Git hooks for maintainers and contributors who want local guardrails. Git does not run these hooks unless you opt in.

## `post-checkout`

`post-checkout` runs after `git checkout`/`git switch` operations. It only acts on branch checkouts (`BRANCH_FLAG=1`) and exits immediately for file checkouts. It also exits in linked worktrees, because linked worktrees have a `.git` file instead of a `.git` directory.

In the primary worktree, if the checkout leaves `HEAD` on any branch other than `main`, the hook prints a warning and runs `git checkout main --quiet`. The warning tells contributors to create feature branches in separate worktrees. The hook is intended for a long-running primary checkout used by the dashboard, where branch changes would affect the served files.

## Install

Enable the repository hooks in the checkout you want to protect:

```bash
git config core.hooksPath githooks
```

This writes a local Git config setting. It is not committed and it does not affect other clones unless you set it there too.

## Bypass or uninstall

If you do not install the hook, normal Git checkout behavior applies. To disable it after installing, unset or override the hooks path:

```bash
git config --unset core.hooksPath
# or point this checkout at a different hook directory
git config core.hooksPath .git/hooks
```

For feature work, keep the protected primary checkout on `main` and create a linked worktree instead:

```bash
git worktree add ../hive-my-feature -b my-feature origin/main
```
