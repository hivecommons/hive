# Credential-free sandbox isolation (phase 1)

## Threat model

Hive agents are untrusted code executors: prompts, tool output, and cloned repositories can all contain hostile instructions. The safe target is therefore **no credentials and no network inside the agent sandbox**. If an agent is compromised, it can only modify its mounted workspace; it cannot call GitHub directly, exfiltrate tokens, or bypass the outer MITM policy.

## Phase-1 scope

This phase adds the reusable plumbing, while tmux remains the default runner:

- `pkg/sandbox` builds and runs rootless Podman specs with `--network=none`, `--userns=keep-id`, a workspace mount, and an explicit environment allowlist. GitHub/token-like environment variables are filtered even if requested.
- `agent_sandbox:` is a disabled-by-default global config gate. Individual agents opt in with `agents.<name>.sandbox.enabled: true` plus optional image/env allowlist overrides.
- `pkg/pushbroker` is the trusted post-step. It inspects committed workspace changes, rejects token-like secrets using the shared `logscrub` token detector, rejects guardrail-critical paths (`.github/workflows/`, gh wrapper paths, `policies/`, `OWNERS`), mints a short-lived scoped GitHub App token outside the workspace, and pushes with a sanitized environment.
- Push broker results are structured for audit logs: repo, branch, commit, changed files, rejection reason, and push status. Tokens are never recorded.

## Deferred wiring

Full migration off tmux is intentionally deferred. The next phases should:

1. Prepare per-kick workspaces for sandboxed agents instead of reusing long-lived tmux panes.
2. Execute the agent kick through `pkg/sandbox.Launcher` and collect stdout/stderr/artifacts.
3. Run `pkg/pushbroker.Broker` automatically after sandbox completion when commits exist on the work branch.
4. Extend PR creation/commenting into the same trusted post-step so no GitHub credential is ever present in the sandbox.
5. Add live Podman CI on a runner with rootless Podman available; current package tests skip live execution when Podman is absent.

This keeps phase 1 safe and reviewable: the security-critical broker and sandbox contract are complete and tested, while production agents continue to use the existing tmux path unless explicitly wired in a later phase.
