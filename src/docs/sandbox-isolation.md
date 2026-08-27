# Credential-free sandbox isolation

## Threat model

Hive agents are untrusted code executors: prompts, tool output, and cloned repositories can all contain hostile instructions. The safe target is therefore **no credentials in the agent sandbox** and a network policy that is as close to default-deny as the configured inference runtime allows.

Two halves of that target hold differently, and the difference matters:

- **Credentials and pushes are constrained on every path.** No agent receives a GitHub token or pushes directly; authorship goes through the App-gated `gh` wrapper and the push broker.
- **Workspace confinement holds only on the sandbox path below.** "It can only modify its mounted workspace" is a property of the Podman sandbox, not of hive generally. On the default tmux path there is no container: the backend CLI runs as the operator's own user, on the operator's own host, with nothing scoping its filesystem access to the workspace.

**#4918 is what that costs in practice, and it did not require a compromise.** An agent doing correct work on an assigned third-party repo ran that repo's own test suite; a latent defect in two of its tests let a hook escape its stubs and issue `rpm-ostree kargs --append-if-missing=...` against the operator's real deployment. Nothing was written, and the only reason is that the process happened to lack privilege. Benign behaviour was a sufficient precondition, so this is a routine exposure rather than an exceptional one.

The claude-family launch path now carries host-state denials for exactly this class — privilege escalation (`sudo`, `pkexec`, `doas`, `su`) and the boot/deployment tools that reach polkit without needing escalation of their own (`rpm-ostree`, `bootc`, `ostree`, `grubby`, `bootctl`, `efibootmgr`), matching the workspace-write posture Codex has had since #3512. That is a floor, not a sandbox: it names commands rather than enforcing a boundary, and an agent still runs unconfined against everything not on the list. **Operators running hive on a machine they care about should enable the sandbox below.**

## Current wiring

Sandbox execution is opt-in and the tmux path remains unchanged for all agents unless both gates are set:

```yaml
agent_sandbox:
  enabled: true
  image: ghcr.io/example/hive-agent:latest
  # Default is "restricted" so local/proxy inference can still work.
  # Use "none" for non-inference/test runs that require full network isolation.
  network_mode: restricted
  timeout_s: 2700
agents:
  scanner:
    sandbox:
      enabled: true
```

For sandboxed agents, a kick now follows this path:

1. Hive prepares a per-kick host workspace under the sandbox workspace root by cloning/fetching the primary repo with hive-owned credentials before the sandbox starts. The clone URL and sandbox environment are sanitized so GitHub/token variables are not carried into the workspace or container.
2. Hive writes the kick prompt to `.hive/kick-prompt.txt` in the workspace and launches `pkg/sandbox.Launcher` with a rootless Podman `LaunchSpec`, workspace mount, explicit env allowlist, and configured network mode.
3. The manager marks the agent busy while the sandbox runs and returns it to idle or failed when the timeout/completion path finishes. Dashboard status uses the existing agent status structures.
4. Hive collects the transcript at `.hive/sandbox-transcript.log` and any `agent-report*.json` artifact following `pkg/outputschema` conventions.
5. If the sandbox produced commits, `pkg/pushbroker.Broker` scans the committed diff for token-like secrets and protected-path edits, mints a short-lived scoped GitHub App token outside the sandbox, pushes the branch, and opens a PR through the existing App-authored GitHub client. Broker rejection records audit detail and nothing is pushed.

## Network trade-off

`network_mode: none` remains available and maps to Podman's `--network=none`, but it only works for non-inference jobs or runtimes that already expose a local/socket model proxy inside the container. The default sandbox network mode is `restricted`: operators must provide a Podman network/proxy policy that allows only the inference endpoint and MITM proxy required by the selected backend. This is a compromise until every supported backend can run through a credential-free local socket without general egress.

## Remaining gaps

- The default target is the hive primary repo and default base ref; richer per-kick repo/ref selection is still future work.
- Live Podman execution is covered by skip-when-absent tests; CI still needs a rootless-Podman runner lane for always-on integration coverage.
- Sandboxed inference depends on an operator-provided restricted network/proxy policy. `none` is stronger but not yet usable for all model backends.

## Agent guardrails (defense in depth)

Beyond the credential-free sandbox above, Hive's isolation model is defense in depth rather than a single sandbox switch. Agents run as long-lived CLI sessions, but the deterministic pipeline and proxy layers decide what work they see and what writes they can perform.

### Isolation layers

1. **Policy mode** — ACMM selects advisory, measured, hold-gated, or full behavior per agent.
2. **Deterministic admission** — Go and shell checks classify work, apply holds, and decide whether an agent is kicked.
3. **Scoped credentials** — contributor relays and spoke agents use the GitHub identity and token scope appropriate to that actor; a delegated ClankeR role does not grant spoke secrets.
4. **MITM GitHub proxy** — GitHub API writes are attributed and constrained according to the current mode.
5. **Merge gates** — hold labels, green checks, self-merge bans, and auto-merge sweeps are enforced outside the LLM prompt.

### Operator notes

- Prefer the least-capable ACMM level that matches the project phase.
- Keep privileged delegated roles (`ci-maintainer`, `sec-check`, `architect`) behind explicit grants.
- Use the docs in this index for concrete setup: [ACMM policy matrix](acmm-policy-matrix.md), [Agent configuration](agent-configuration.md), and [Contributor trust tiers and delegated agent roles](contributor-trust-and-roles.md).
