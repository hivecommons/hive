# Credential-free sandbox isolation

## Threat model

Hive agents are untrusted code executors: prompts, tool output, and cloned repositories can all contain hostile instructions. The safe target is therefore **no credentials in the agent sandbox** and a network policy that is as close to default-deny as the configured inference runtime allows. If an agent is compromised, it can only modify its mounted workspace; it cannot receive GitHub tokens or push directly.

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
