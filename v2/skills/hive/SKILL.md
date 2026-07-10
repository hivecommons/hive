---
name: hive
description: Set up, verify, and operate Hive with Visual Hive as production repository testing and repair automation. Use when a user invokes /hive or $hive, asks to install Hive on a repository, choose testing coverage or autonomous authority, run a production scan, manage Hive findings/issues/repair PRs, pause or resume automation, or perform an immutable upgrade, rollback, or uninstall.
---

# Hive

Use Hive's MCP tools as the control plane. Keep coverage depth separate from GitHub write authority and take setup through a verified hosted run when the user asks for production setup.

## Setup workflow

1. Identify the `owner/repository`. Never request a pasted token; use the user's existing GitHub authorization flow.
2. Call `hive_setup_plan` before any mutation. Show detected stack, test layers, files, warnings, and required permissions in plain language.
3. If absent, ask for exactly two choices:
   - Coverage: `essential`, `standard`, `comprehensive`, or `custom`.
   - Automation: `advisory`, `issues`, `repair-pr`, or `auto-merge`.
4. Explain that `issues` can open/update/close findings but cannot create a branch; `repair-pr` can create and revise one linked PR but cannot merge; `auto-merge` can merge only after exact-SHA deterministic, required-check, protection, risk, and hold gates all pass.
5. Obtain explicit setup approval, then call `hive_setup_apply` with the same reviewed values and a stable persistent `state_dir`.
6. Report the one setup PR. If the user authorized taking setup to production, wait for its hosted checks, merge it only when green and reviewable, then continue. Never bypass checks, protections, or review requirements.
7. Call `hive_doctor`. Resolve every failed check; do not describe the installation as ready while `production_ready` is false.
8. Call `hive_run`. Verify the hosted run, provenance-bound evidence, lifecycle mutations allowed by the selected authority, and duplicate-free durable state. End with `hive_status`.

## Operating rules

- Use `hive_status` before changing an existing installation.
- Use `hive_set_coverage` only for test depth. Use `hive_set_automation` only for issue/PR/merge authority.
- Use `hive_pause` immediately when the user requests a stop or when an unexpected write occurs. Confirm status before `hive_resume`.
- Treat policy denial as a real stop. Never silently downgrade a denied issue, PR, close, reopen, or merge into advice.
- Keep Hive as the only GitHub lifecycle writer in integrated mode. Do not enable Visual Hive's standalone publishers concurrently.
- Never close a finding because a partial, stale, changed-files-only, expired, or unrelated scan omitted it. Closure requires an authoritative target-branch run that evaluated the affected contract after the recorded merge.
- For `repair-pr`, leave the issue open until post-merge verification. For `auto-merge`, require all gates returned by Hive; do not merge separately with a GitHub command.
- Use `hive_upgrade` and `hive_rollback` only with immutable 40-character Visual Hive commit SHAs. Review the generated PR.
- Use `hive_uninstall` only after explicit confirmation. Preserve local state unless the user explicitly requests permanent state deletion.

## Tool fallback

If Hive MCP tools are unavailable but the `hive` executable is installed, run the equivalent CLI with `--json` and the same stable `--state-dir`. Start with:

```text
hive setup --repo OWNER/REPO --coverage LEVEL --automation MODE --provider codex --visual-hive --start --json
hive doctor --json
hive run --json
hive status --json
```

Do not ask the user to hand-edit workflow YAML or repository variables. If neither MCP nor the CLI is available, report the missing installation rather than improvising repository writes.
