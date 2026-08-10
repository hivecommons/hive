# Policy and prompt templates

Hive kicks an agent by resolving a Markdown prompt template, substituting runtime variables, and typing the result into that agent's CLI session. This directory is the human-editable source mirror for the shipped policy templates. The Go binary embeds the copies under `v2/pkg/policies/defaults/`; keep both locations in mind when changing defaults.

## Resolution order

For a scheduled kick, Hive uses the agent's configured `kick_template` when present. ACMM packs set that field explicitly in `v2/pkg/config/packs/level-*.yaml`; for example L4 sets `scanner-issues.md` and `guide-issues.md`, L5 sets `*-holdgated.md`, and L6 sets `*-full.md`/`scanner-automerge.md`.

The scheduler resolves the template from the configured policies directory (`policies.local_dir` plus `policies.path`, commonly `/data/policies/examples/kubestellar/agents/`) and falls back to the embedded defaults in this package. A prompt source may also fetch Markdown from an allowlisted GitHub repository at kick time; if the fetch fails, Hive uses its last cached copy or the inline/baked template rather than sending an empty kick.

If an agent has no explicit template, Hive falls back by convention: first an agent-local `CLAUDE.md` style policy when present, then `<agent>.md`, then the embedded default for the role.

## Mode variants

Template suffixes mirror the ACMM interaction mode:

| Suffix | Mode | Meaning |
| --- | --- | --- |
| `-advisory` | `ADVISORY` | Analyze and write advisory beads; no GitHub writes. |
| `-issues` | `ISSUES_ONLY` | May open/update issues; no pull requests. |
| `-holdgated` | `ISSUES_AND_PRS` at L5 | May open issues and PRs; PRs are hold-gated for humans. |
| `-full` | `ISSUES_AND_PRS` outside L5 | May open issues and PRs without the L5 hold-gate wording. |
| `-automerge` | `ISSUES_PRS_MERGE` | Scanner may merge on green CI when policy allows. |
| `-nogithub` | advisory variant | Explicit no-GitHub policy used by supervisor packs. |

Examples in the current packs:

- `guide-issues.md` is the L4 guide policy: documentation issues are allowed, PRs are not.
- `scanner-issues.md` is the L4 scanner policy: issue filing only.
- `supervisor-nogithub.md` is the supervisor policy from L2 through L6; it keeps orchestration separate from GitHub mutation.
- `brainstorm-advisory.md` is the only brainstorm template referenced by v2 HEAD, including L1 inception. `brainstorm-inception.md` is mentioned only by stale documentation, so operators should not create or rely on that filename unless they also set `kick_template` to it.

## Customizing prompts

Use one of these supported paths:

1. **Policy checkout** — set `policies.repo`, `policies.path`, and `policies.local_dir` in `hive.yaml`. Put files with the same names as the pack templates in that directory. Hive polls the checkout and the next kick picks up changes.
2. **Per-agent override** — set `agents.<name>.kick_template` to a filename in the policy checkout/defaults.
3. **Dashboard or `hivectl`** — use the agent prompt editor or `hivectl agent prompt set <agent> --file policy.md`; writes land in the server's writable policy directory.
4. **Portable agent definition** — import an `AgentDefinition` with `spec.promptTemplate`; Hive stores a baked copy and can keep selected safe fields linked to an allowlisted source.

Do not put secrets in policy Markdown. Prompts are shown in logs, dashboard history, and agent panes.

## Variables

Templates are rendered with runtime context such as `${AGENT_NAME}`, `${PROJECT_ORG}`, `${PROJECT_PRIMARY_REPO}`, `${ISSUE_LIST}`, `${PR_LIST}`, `${KNOWLEDGE}`, and mode/auth fragments. Variables that are not available for a mode render empty; for example advisory/no-GitHub policies do not get write-capable GitHub auth instructions.

## Operator checklist

- Match the template to the agent's `mode`; do not give an advisory agent a hold-gated template and expect GitHub writes to work.
- Keep `stale_timeout` longer than the longest cadence in the selected ACMM pack.
- Prefer replacing a shipped filename in a policy checkout over editing embedded files; embedded defaults change only when Hive is rebuilt.
- After changing prompts, kick the agent once and inspect the prompt history or pane output to verify the expected file was used.
