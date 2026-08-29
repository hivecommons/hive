# Credential-free sandbox isolation

## Threat model

Hive agents are untrusted code executors: prompts, tool output, and cloned repositories can all contain hostile instructions. The safe target is therefore **no credentials in the agent sandbox** and a network policy that is as close to default-deny as the configured inference runtime allows.

Two halves of that target hold differently, and the difference matters:

- **Credentials and pushes are constrained on every path.** No agent receives a GitHub token or pushes directly; authorship goes through the App-gated `gh` wrapper and the push broker.
- **Workspace write confinement depends on the launch path and backend.** The Podman sandbox below is the hub-side boundary. Contributor container mode has its own container boundary. In contributor local mode: claude/litellm and codex have OS-enforced sandboxes; copilot has its own OS-enforced sandbox, wired the same way, gated on the installed CLI actually supporting it; opencode has no sandbox but does get a command-name deny-list (a floor, not a boundary); goose, agy, bob, pi, and aider have **no confinement mechanism this repo can wire at all** and refuse to launch in local mode unless the operator explicitly opts in per backend. See the [per-backend matrix](#per-backend-confinement-on-the-contributor-local-path) below.

**#4918 is what that costs in practice, and it did not require a compromise.** An agent doing correct work on an assigned third-party repo ran that repo's own test suite; a latent defect in two of its tests let a hook escape its stubs and issue `rpm-ostree kargs --append-if-missing=...` against the operator's real deployment. Nothing was written, and the only reason is that the process happened to lack privilege. Benign behaviour was a sufficient precondition, so this is a routine exposure rather than an exceptional one.

The claude-family launch path carries host-state denials for exactly this class — privilege escalation (`sudo`, `pkexec`, `doas`, `su`) and the boot/deployment tools that reach polkit without needing escalation of their own (`rpm-ostree`, `bootc`, `ostree`, `grubby`, `bootctl`, `efibootmgr`). Those denials remain only a command-list floor. On the contributor local path, Claude Code's native sandbox is now the OS-enforced write boundary around them.

## Which confinement is available depends on the path you are on

This is the part that is easy to get wrong, because the two paths have different levers and only one of them has the Podman sandbox at all.

| | Hub / pod agents (`pkg/agent`) | Contributor relay (`just contribute-hive`) |
|---|---|---|
| Runs where | The hive spoke's own container | The contributor's machine |
| Podman agent sandbox (`agent_sandbox`) | Available, opt-in — see below | **Does not exist on this path.** `SandboxEnabled` is read only by `pkg/agent`; nothing in `bin/contributor-relay.sh`, `bin/contributor-agent.sh` or the `Justfile` consults it |
| The confinement lever | `agent_sandbox` + the per-agent opt-in | Container mode (the default), or a backend-native sandbox in local mode — see the matrix below, coverage varies by backend |
| Host-state denials (#4938) | Yes | Yes (`config/backends.conf`) |
| Credentials / pushes | Constrained | Constrained |

**The #4918 incident happened on the contributor relay's local mode**, so enabling `agent_sandbox` would not have prevented it. Claude-family local launches now use Claude Code's native sandbox with hard-fail startup and unsandboxed retry disabled; codex keeps its `workspace-write` sandbox; copilot local launches now use Copilot CLI's own OS-enforced sandbox. Container mode remains the stronger backend-independent remedy and the `just contribute-hive` default.

Operators running hive on a machine they care about should therefore prefer container mode on the contributor path, use only a locally sandboxed (or, for opencode, at least denylisted) backend when local mode is necessary, and enable the sandbox below on the hub path.

### Per-backend confinement on the contributor local path

This table is the ground truth for `just contribute-hive <backend> local` — the only mode where "the operator's host" means a real desktop with no container boundary. Verified against each backend's own current CLI documentation, not assumed; see `config/backends.conf` for the implementation and `src/pkg/dashboard/contribute_local_mode_backend_matrix_test.go` for the tests that pin it.

| Backend | Mechanism | What it actually bounds | Escape hatch (unconfined opt-in) |
|---|---|---|---|
| `claude` / `litellm` | Claude Code's native OS sandbox (`--settings` sandbox JSON; Seatbelt on macOS, bubblewrap on Linux) | Filesystem writes confined to the agent cwd and `HIVE_WORKSPACE_DIR`; `failIfUnavailable: true`, no unsandboxed fallback | `HIVE_CLAUDE_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX=1` |
| `codex` | Codex's own `--sandbox workspace-write` | Filesystem writes confined to the workspace root(s) codex is given | `HIVE_CODEX_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX=1` |
| `copilot` | Copilot CLI's own `--sandbox` flag (MXC: Seatbelt on macOS, bubblewrap on Linux, ProcessContainer on Windows Insiders) | Filesystem/network/process access of the commands and tools Copilot runs, restricted by the OS-level backend; `--add-dir` grants the exact workspace | `HIVE_COPILOT_DANGEROUSLY_BYPASS_SANDBOX=1`, or automatic fallback with a loud warning if the installed CLI predates `--sandbox` (copilot-cli < 1.0.60) |
| `opencode` | opencode's own `permission.bash` deny rules (inline via `OPENCODE_PERMISSION`), denying the same host-state command family the claude deny-list covers | **Not a filesystem boundary.** A command-name floor only — anything not on the list, or reached another way, is unconstrained. Deny rules are documented to hold even under `--auto`. | `HIVE_OPENCODE_DANGEROUSLY_ALLOW_HOST_STATE=1` |
| `goose` | **None.** `GOOSE_MODE` (`auto`/`approve`/`chat`/`smart_approve`) governs interactive approval only; no mode confines writes to a directory, and only `auto` is usable unattended. | Nothing — local mode refuses to launch without explicit opt-in | `HIVE_GOOSE_DANGEROUSLY_RUN_UNCONFINED=1` |
| `agy` | **None.** Antigravity CLI's execution modes (`default`/`accept-edits`/`plan`) govern approval only, same shape as goose; `--dangerously-skip-permissions` is what hive already passes and there is no lesser mode that confines the filesystem. agy 1.1.22 also advertises `--sandbox`, but its binary's own strings point to Google's remote/cloud sandbox machinery (a `Sandbox` proto with a network endpoint+port), not a local OS boundary — treat it as unrelated to host confinement until verified otherwise. | Nothing — local mode refuses to launch without explicit opt-in | `HIVE_AGY_DANGEROUSLY_RUN_UNCONFINED=1` |
| `bob` | **None.** No sandbox, approval mode, or path-restriction mechanism documented anywhere in Bob Shell's own docs. | Nothing — local mode refuses to launch without explicit opt-in | `HIVE_BOB_DANGEROUSLY_RUN_UNCONFINED=1` |
| `pi` | **None.** `@earendil-works/pi-coding-agent` ships with no sandbox by default; directory confinement exists only via a third-party extension (`pi-permission-modes`) hive does not install or depend on. | Nothing — local mode refuses to launch without explicit opt-in | `HIVE_PI_DANGEROUSLY_RUN_UNCONFINED=1` |
| `aider` | **None.** No Docker/OS isolation option of any kind. | Nothing — local mode refuses to launch without explicit opt-in | `HIVE_AIDER_DANGEROUSLY_RUN_UNCONFINED=1` |

The five backends with no mechanism (goose, agy, bob, pi, aider) are a hard stop, by design: `just contribute-hive <backend> local` prints an honest refusal and a non-zero exit rather than a silent unconfined launch, unless the operator sets that backend's own escape-hatch env var. This is deliberately not a blanket `HIVE_DANGEROUSLY_RUN_UNCONFINED` — a single shared flag would let opting into one unconfined backend silently opt into all five.

For `agy` specifically, this local-mode refusal used to be a dead end (#5048): `src/Dockerfile.contributor` never installed the `agy` binary, so container mode — the only real boundary any of these five backends can get on this path — was unavailable too, leaving no working path at all. That is now fixed: the image installs `agy` from Google's published, checksummed release tarball, so `just contribute-hive agy` (container mode, the default) actually works. Nothing above about agy's *local*-mode posture changed — it still has no sandbox and still refuses without the escape hatch, exactly like goose/bob/pi/aider.

## Current wiring

Sandbox execution is opt-in and the tmux path remains unchanged for all agents unless **both** gates are set — the global one and a per-agent one:

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

**Both gates are required, and the global one alone does nothing.** `agent_sandbox.enabled: true` with no per-agent `sandbox.enabled: true` sandboxes zero agents. That matters more than it reads, because the dashboard's Security tab writes *only* the global flag and is the only sandbox control the UI offers: an owner can turn "agent sandbox" on, be told the setting was updated, and have every agent keep running unconfined. Hive now logs a `agent sandbox posture` warning at boot and on every config reload when the sandbox is enabled globally but some or all agents are not opted in (`config.AgentSandboxGateWarnings`).

The second gate is deliberate rather than an oversight, and it is not safe to simply collapse. A sandboxed agent runs a different execution model — no tmux CLI at all, and every kick is a Podman run against the primary repo — and `startSandboxKickLocked` has **no fallback to the tmux path**: an agent opted in without a resolvable image fails every kick outright rather than degrading. Making the global flag sufficient would therefore convert working agents into permanently failing ones on any hive that set it without an image. Changing that default is a fleet-affecting decision that wants measurement, not a code-reading; the warning above is the part that is safe today.

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
