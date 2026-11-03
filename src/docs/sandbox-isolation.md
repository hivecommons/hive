# Credential-free sandbox isolation

## Threat model

Hive agents are untrusted code executors: prompts, tool output, and cloned repositories can all contain hostile instructions. The safe target is therefore **no credentials in the agent sandbox** and a network policy that is as close to default-deny as the configured inference runtime allows.

Two halves of that target hold differently, and the difference matters:

- **Credentials and pushes are constrained on every path.** No agent receives a GitHub token or pushes directly; authorship goes through the App-gated `gh` wrapper and the push broker.
- **Workspace write confinement depends on the launch path and backend.** The Podman sandbox below is the hub-side boundary. Contributor container mode has its own container boundary. In contributor local mode: claude/litellm and codex have OS-enforced sandboxes; copilot has its own OS-enforced sandbox, wired the same way, gated on the installed CLI actually supporting it; opencode has no sandbox but does get a command-name deny-list (a floor, not a boundary); goose, agy, bob, pi, aider, kilo, omp, openhands, and omnigent have **no confinement mechanism this repo can wire at all** and refuse to launch in local mode unless the operator explicitly opts in per backend. See the [per-backend matrix](#per-backend-confinement-on-the-contributor-local-path) below.

**#4918 is what that costs in practice, and it did not require a compromise.** An agent doing correct work on an assigned third-party repo ran that repo's own test suite; a latent defect in two of its tests let a hook escape its stubs and issue `rpm-ostree kargs --append-if-missing=...` against the operator's real deployment. Nothing was written, and the only reason is that the process happened to lack privilege. Benign behaviour was a sufficient precondition, so this is a routine exposure rather than an exceptional one.

The claude-family launch path carries host-state denials for exactly this class — privilege escalation (`sudo`, `pkexec`, `doas`, `su`) and the boot/deployment tools that reach polkit without needing escalation of their own (`rpm-ostree`, `bootc`, `ostree`, `grubby`, `bootctl`, `efibootmgr`). Those denials remain only a command-list floor. On the contributor local path, Claude Code's native sandbox is now the OS-enforced write boundary around them.

## Which confinement is available depends on the path you are on

This is the part that is easy to get wrong, because the two paths have different levers and only one of them has the Podman sandbox at all.

| | Hub / pod agents (`pkg/agent`) | Contributor relay (`just contribute-hive`) |
|---|---|---|
| Runs where | The hive spoke's own container | The contributor's machine |
| Podman agent sandbox (`agent_sandbox`) | Available, opt-in — see below | **Does not exist on this path.** `SandboxEnabled` is read only by `pkg/agent`; nothing in `bin/contributor-relay.js`, `bin/contributor-agent.sh` or the `Justfile` consults it |
| The confinement lever | `agent_sandbox` + the per-agent opt-in | Container mode (the default), or a backend-native sandbox in local mode — see the matrix below, coverage varies by backend |
| Host-state denials (#4938) | Yes | Yes (`config/backends.conf`) |
| Credentials / pushes | Constrained | Constrained |

Hub-launched Codex agents use `--dangerously-bypass-approvals-and-sandbox`,
mirroring Claude's unattended posture. The hub path already relies on the pod,
the optional Podman agent sandbox, the egress proxy, and Hive's mode gates as
the boundary; Codex's `workspace-write` sandbox is not used there because it
also disables the network path `gh`/`git` need through the proxy and would
replace one unattended posture with approval prompts in an unwatched pane.

**The #4918 incident happened on the contributor relay's local mode**, so enabling `agent_sandbox` would not have prevented it. Claude-family local launches now use Claude Code's native sandbox with hard-fail startup and unsandboxed retry disabled; codex keeps its `workspace-write` sandbox; copilot local launches now use Copilot CLI's own OS-enforced sandbox. Container mode remains the stronger backend-independent remedy and the `just contribute-hive` default.

Operators running hive on a machine they care about should therefore prefer container mode on the contributor path, use only a locally sandboxed (or, for opencode, at least denylisted) backend when local mode is necessary, and enable the sandbox below on the hub path.

### Per-backend confinement on the contributor local path

This table is the ground truth for `just contribute-hive <backend> local` — the only mode where "the operator's host" means a real desktop with no container boundary. Verified against each backend's own current CLI documentation, not assumed; see `config/backends.conf` for the implementation and `src/pkg/dashboard/contribute_local_mode_backend_matrix_test.go` for the tests that pin it.

| Backend | Mechanism | What it actually bounds | Escape hatch (unconfined opt-in) |
|---|---|---|---|
| `claude` / `litellm` | Claude Code's native OS sandbox (`--settings` sandbox JSON; Seatbelt on macOS, bubblewrap on Linux) | Filesystem writes confined to the agent cwd, `HIVE_WORKSPACE_DIR`, and the hive-created build-cache root `HIVE_AGENT_CACHE_DIR` (#6100); `failIfUnavailable: true`, no unsandboxed fallback | `HIVE_CLAUDE_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX=1` |
| `codex` | Codex's own `--sandbox workspace-write` | Filesystem writes confined to the workspace root(s) codex is given | `HIVE_CODEX_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX=1` |
| `copilot` | Copilot CLI's own `--sandbox` flag (MXC: Seatbelt on macOS, bubblewrap on Linux, ProcessContainer on Windows Insiders) | Filesystem/network/process access of the commands and tools Copilot runs, restricted by the OS-level backend; `--add-dir` grants the exact workspace | `HIVE_COPILOT_DANGEROUSLY_BYPASS_SANDBOX=1`, or automatic fallback with a loud warning if the installed CLI predates `--sandbox` (copilot-cli < 1.0.60) |
| `muse` | Muse Code's own OS sandbox (bubblewrap/seccomp + namespaces on Linux, seatbelt/`sandbox-exec` on macOS), **on by default**; hive narrows it with `--workspace $HIVE_WORKSPACE_DIR`, `--sandbox-network proxy-only`, and `--no-foreign-personal-context` | Filesystem/network reach of the shell muse runs, bounded by the OS mechanism; `--workspace` roots its policy-gated workspace tools at the exact workspace. Approval is turned off with `--approval-mode never`, which is a *policy*, not the boundary — the sandbox stays on. `--no-foreign-personal-context` additionally stops muse loading the operator's own `~/.claude` personal skills into an unattended agent. | `HIVE_MUSE_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX=1` |
| `opencode` | opencode's own `permission.bash` deny rules (inline via `OPENCODE_PERMISSION`), denying the same host-state command family the claude deny-list covers | **Not a filesystem boundary.** A command-name floor only — anything not on the list, or reached another way, is unconstrained. Deny rules are documented to hold even under `--auto`. | `HIVE_OPENCODE_DANGEROUSLY_ALLOW_HOST_STATE=1` |
| `goose` | **None.** `GOOSE_MODE` (`auto`/`approve`/`chat`/`smart_approve`) governs interactive approval only; no mode confines writes to a directory, and only `auto` is usable unattended. | Nothing — local mode refuses to launch without explicit opt-in | `HIVE_GOOSE_DANGEROUSLY_RUN_UNCONFINED=1` |
| `agy` | **None.** Antigravity CLI's execution modes (`default`/`accept-edits`/`plan`) govern approval only, same shape as goose; `--dangerously-skip-permissions` is what hive already passes and there is no lesser mode that confines the filesystem. agy 1.1.22 also advertises `--sandbox`, but its binary's own strings point to Google's remote/cloud sandbox machinery (a `Sandbox` proto with a network endpoint+port), not a local OS boundary — treat it as unrelated to host confinement until verified otherwise. | Nothing — local mode refuses to launch without explicit opt-in | `HIVE_AGY_DANGEROUSLY_RUN_UNCONFINED=1` |
| `bob` | **None.** No sandbox, approval mode, or path-restriction mechanism documented anywhere in Bob Shell's own docs. | Nothing — local mode refuses to launch without explicit opt-in | `HIVE_BOB_DANGEROUSLY_RUN_UNCONFINED=1` |
| `pi` | **None.** `@earendil-works/pi-coding-agent` ships with no sandbox by default; directory confinement exists only via a third-party extension (`pi-permission-modes`) hive does not install or depend on. | Nothing — local mode refuses to launch without explicit opt-in | `HIVE_PI_DANGEROUSLY_RUN_UNCONFINED=1` |
| `aider` | **None.** No Docker/OS isolation option of any kind. | Nothing — local mode refuses to launch without explicit opt-in | `HIVE_AIDER_DANGEROUSLY_RUN_UNCONFINED=1` |
| `omp` | **None.** OMP exposes `--auto-approve`, but that controls confirmation prompts rather than filesystem or host-state confinement; Hive does not pass it on the interactive contributor path. | Nothing — local mode refuses to launch without explicit opt-in | `HIVE_OMP_DANGEROUSLY_RUN_UNCONFINED=1` |
| `claude-managed-agents` | Claude Managed Agents `self_hosted` worker in the Hive pod, with `forced-egress`/`proxy-only` and the extwork `shadow`/`report-only` gate | Tool traffic stays on Hive's egress path; the worker gets an environment key and per-session work secret, never `ANTHROPIC_API_KEY` or a GitHub write token | No direct GitHub path is accepted; `github_repository`, vaulted-secret, and GitHub-MCP rows remain report-only and are refused for write-capable stages |
| `openhands` | **None.** The bare `openhands` CLI runs the agent directly on the host (OpenHands' own README: "the agent will have full access to your filesystem!"); `--always-approve` governs approval only, and the Docker sandbox exists only behind `openhands serve`/Agent Canvas, which have no one-shot task shape hive can drive. | Nothing — local mode refuses to launch without explicit opt-in | `HIVE_OPENHANDS_DANGEROUSLY_RUN_UNCONFINED=1` |
| `omnigent` | **None.** Omnigent provides an orchestration and policy layer over Claude Code, Codex, Cursor, Pi, and custom agents, with optional agent-level sandboxing (`enforce_sandbox`), but the local CLI invocation (`omnigent run --server local`) runs directly on the host with no OS-level boundary wired by Hive. `--server local` keeps the Omnigent server inside the Hive boundary (remote-server mode is unsupported); its internal policies evaluate additively, while Hive's proxy remains strictly authoritative for all repository writes and pushes. | Nothing — local mode refuses to launch without explicit opt-in | `HIVE_OMNIGENT_DANGEROUSLY_RUN_UNCONFINED=1` |

**The build-cache root, and why it is a grant rather than a hole (#6100).** Every
compiled toolchain writes to a cache under `$HOME` by default — `GOCACHE` at
`~/.cache/go-build`, `ccache`, `$TMPDIR` — and all of those sit outside the
workspace. So `go build`, `go test` and `go vet` failed on a hive contributor
task, in hive's own Go repo, with an error naming a cache file rather than a
sandbox boundary; eight local-mode sessions over five days each rediscovered it
and invented its own `GOCACHE` redirect. The launcher now creates
`${XDG_CACHE_HOME:-~/.cache}/hive/agent-build`, exports `GOCACHE`, `GOMODCACHE`,
`GOTMPDIR`, `TMPDIR` and `CCACHE_DIR` into it, and the sandbox grants it as a
write root. That is strictly narrower than widening the sandbox: one directory
hive owns, rather than `$HOME`. It is a **write root only** — deliberately not in
`permissions.allow` and not an `--add-dir`, because Bash is what compilers run
under, an agent has no reason to `Edit` a build cache, and adding it as a working
directory would put megabytes of object files in the agent's view of the tree.

The backends with no mechanism (goose, agy, bob, pi, aider, kilo, omp, openhands, omnigent) are a hard stop, by design: `just contribute-hive <backend> local` prints an honest refusal and a non-zero exit rather than a silent unconfined launch, unless the operator sets that backend's own escape-hatch env var. This is deliberately not a blanket `HIVE_DANGEROUSLY_RUN_UNCONFINED` — a single shared flag would let opting into one unconfined backend silently opt into all nine.

For `agy` specifically, this local-mode refusal used to be a dead end (#5048): `src/Dockerfile.contributor` never installed the `agy` binary, so container mode — the only real boundary any of these five backends can get on this path — was unavailable too, leaving no working path at all. That is now fixed: the image installs `agy` from Google's published, checksummed release tarball, so `just contribute-hive agy` (container mode, the default) actually works. Nothing above about agy's *local*-mode posture changed — it still has no sandbox and still refuses without the escape hatch, exactly like goose/bob/pi/aider.


### Omnigent policy composition with Hive

Omnigent ships with its own policy engine and configurable guardrails (such as `github_policy`, `ask_on_os_tools`, and `enforce_sandbox`). When Omnigent is run under Hive:

1. **Hive policy strictly wins on GitHub writes.** All repository writes, PR submissions (`POST /repos/{owner}/{repo}/pulls`), PR merges (`PUT /repos/{owner}/{repo}/pulls/{n}/merge`), and git pushes are mediated by Hive's egress proxy and GitHub App authentication broker. Proxy denials remain authoritative: Omnigent's internal policy engine or harness configuration cannot grant or bypass write permissions that Hive denies.
2. **Omnigent's sandbox is additive, never a substitute for Hive proxy or container isolation.** If an Omnigent agent spec configures `enforce_sandbox` (e.g. Linux bwrap or macOS seatbelt) or targets remote cloud sandboxes, this adds an extra layer of host isolation. However, Hive treats Omnigent as unconfined on the bare contributor local path because Hive does not wire or guarantee an OS-level boundary through CLI launch flags.
3. **Underlying harness credential flow.** Omnigent orchestrates underlying harnesses (Claude Code, Codex, Cursor, Pi). In Hive, provider credentials pass through ambient environment variables or staged configs, but repository write tokens are never exposed directly to the harnesses.

### Codex in container mode is bounded by the container, not by `workspace-write` (#6653)

The matrix above is about **local** mode, and codex's `workspace-write` entry there is unchanged. Container mode is different, and the difference was a bug for as long as the two shared one hard-coded flag.

`--sandbox workspace-write` is implemented with bubblewrap, and bubblewrap's first act is to create an unprivileged user namespace. The contributor container runs under the runtime's default seccomp profile with nothing relaxed, which denies that syscall — so asking for `workspace-write` there did not produce a weaker sandbox, it produced **no working command at all**: every model-generated command died with `bwrap: No permissions to create a new namespace`, including both of Codex's patch-application paths, leaving the agent to rediscover a working edit mechanism by trial and error on each task. The error text also names a *host* sysctl, pointing operators at a machine that was never the problem.

Hive now resolves the value instead of fixing it, on two independent checks made at launch:

- **Does an outer boundary exist?** The root-owned `/etc/hive/contributor-mode` marker baked into `src/Dockerfile.contributor` — a file, not an env var, so an agent running as `dev` cannot forge it.
- **Can the nested sandbox actually work?** A direct `unshare --user --map-root-user` probe, the same capability bwrap needs.

Only "container, and no user namespaces" falls back to `danger-full-access`, and it says so in one line on stderr. This is not a weakening: the container is the boundary on that path, and it is the same boundary that was already load-bearing. **Local mode is never downgraded** — there is no outer boundary there, so a blocked probe leaves `workspace-write` in place rather than widening access to the operator's host. An operator who would rather have the nested sandbox can relax the runtime (`--security-opt seccomp=unconfined`) and the probe restores `workspace-write` on its own; the contributor image now ships a real `bubblewrap`, so that path uses a distro-maintained binary instead of Codex's bundled fallback. `HIVE_CODEX_SANDBOX_MODE` pins a value and skips the probe entirely.

### Optional krun microVM for the contributor container (#12199)

Contributors can opt in, with `HIVE_CONTAINER_OCI_RUNTIME=krun`, to running the contributor container inside a libkrun microVM under rootless podman, headless or interactive through the VM console (`podman attach`; there is no `exec` route). It adds a guest kernel between the agent and the contributor's host kernel; it does not change what the agent can reach — credentials in the guest, network egress and the read-write mounts are the same as a plain container — and libkrun's guest and VMM share one security context, so container hardening flags apply to the VMM rather than the guest. Once requested, the launch refuses every unsupported combination (docker, rootful podman, local mode, `pi`, backends with no headless entry when run headless, macOS) and checks `/dev/kvm` and a krun smoke run before starting; it never falls back to a plain container. It does not change the isolation of the hive's own agents. Details: "Hardened mode" in [contributor-relay.md](contributor-relay.md).

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
  # Optional deterministic preflight before any push/PR; see the publish
  # preflight note under "kick path" below.
  publish:
    verify: []
agents:
  scanner:
    sandbox:
      enabled: true
```

**Both gates are required, and the global one alone does nothing.** `agent_sandbox.enabled: true` with no per-agent `sandbox.enabled: true` sandboxes zero agents. That matters more than it reads, because the dashboard's Security tab writes *only* the global flag and is the only sandbox control the UI offers: an owner can turn "agent sandbox" on and, without the fix below, be told the setting was updated while every agent kept running unconfined.

Hive logs an `agent sandbox posture` warning at boot and on every config reload when the sandbox is enabled globally but some or all agents are not opted in (`config.AgentSandboxGateWarnings`), and — as of the #4918 fix — the same diagnosis also reaches the dashboard itself: `GET /api/config/governor`'s `security.sandboxWarnings` array carries it, and the Security tab renders it both in the page's "Coherence warnings" box and as an inline warning directly beneath the sandbox toggle, naming which agents are still unconfined and the exact `sandbox: {enabled: true}` key that fixes it. The array is empty (and nothing renders) on the documented default — sandbox off globally — and on a fully opted-in hive, so the warning appears only when it is actionable.

The second gate is deliberate rather than an oversight, and it is not safe to simply collapse. A sandboxed agent runs a different execution model — no tmux CLI at all, and every kick is a Podman run against the primary repo — and `startSandboxKickLocked` has **no fallback to the tmux path**: an agent opted in without a resolvable image fails every kick outright rather than degrading. Making the global flag sufficient would therefore convert working agents into permanently failing ones on any hive that set it without an image. Changing that default is a fleet-affecting decision that wants measurement, not a code-reading; the warning above is the part that is safe today.

For sandboxed agents, a kick now follows this path:

1. Hive prepares a per-kick host workspace under the sandbox workspace root by cloning/fetching the primary repo with hive-owned credentials before the sandbox starts. The clone URL and sandbox environment are sanitized so GitHub/token variables are not carried into the workspace or container.
2. Hive writes the kick prompt to `.hive/kick-prompt.txt` in the workspace and launches `pkg/sandbox.Launcher` with a rootless Podman `LaunchSpec`, workspace mount, explicit env allowlist, and configured network mode.
3. The manager marks the agent busy while the sandbox runs and returns it to idle or failed when the timeout/completion path finishes. Because the agent sits in `idle` between kicks, dashboard health for sandboxed agents comes from the last-run record rather than the session check; see [Health and metrics](#health-and-metrics-for-sandboxed-agents).
4. Hive collects the transcript at `.hive/sandbox-transcript.log` and any `agent-report*.json` artifact following `pkg/outputschema` conventions. The container's captured stdout/stderr is also archived as the kick's log under the kick-log directory (`<kick_log_dir>/<agent>/<kick-start>-sandbox.log`, same retention as tmux kick logs — see [Agent logging](agent-logging.md#per-kick-log-history)). That archive, not the workspace transcript, is what the card's "full log" / "log text" buttons and the 🕘 kick history serve for a sandboxed agent, because the workspace is swept after two hours and a Job's pod is gone as soon as it completes.
   **Publish gate.** Commits alone do not earn a PR. If the agent's report carries `"status": "failed"` or `"status": "blocked"`, or the sandbox exited non-zero, hive leaves the branch local, opens no PR, records the run as failed with the reason in `prWithheld` (shown on the card as "no PR opened: …"), and writes a `sandbox_pr_withheld` audit line naming the workspace, commit count and branch. The `status` field is optional; a report without one, or no report at all, does not block publishing, so agents that predate the field behave as before. Kick templates for sandboxed agents whose work must build or pass a smoke test before it is worth a PR should tell the agent to write `"status": "failed"` (with the failure in `summary`) instead of committing when that step fails.
   **Publish preflight (`publish.verify`).** The report gate trusts the agent; the preflight is a determinism/regression check over the agent's own tree. When the run left commits and the gate above passed, hive runs each configured verify command — `sandbox.publish.verify` per agent or `agent_sandbox.publish.verify` globally — as `sh -lc <cmd>` in a fresh container/Job of the same image, workspace, env allowlist and network mode, with `HIVE_CHANGED_FILES` and `HIVE_CHANGED_TESTS` (newline-separated paths from the run's commits, base..HEAD) and `HIVE_BASE_SHA` exported. The first non-zero exit, launcher error or timeout (`publish.verify_timeout_s`, default 900 s per command) withholds the push and the PR exactly like a failed report: `prWithheld` reads `verify failed (1/2): <cmd> (exit N)` followed by the command's output tail, the card shows the first line with the tail in its tooltip, and the kick log archive gets a `---- verify: <cmd> (exit N) ----` section with the full output. With no commits there is nothing to publish and the preflight is skipped; with no commands configured it is a no-op. Typical use is a whole-suite `pytest --collect-only -q` plus running the tests the agent added — the two checks that would have caught a test-file basename clash that broke collection for an entire repository ([torch-spyre/spyre-inference#1412](https://github.com/torch-spyre/spyre-inference/pull/1412)):
   > **Note:** Config is the only source of verify commands. A `<!-- hive:publish.verify … -->` block in a kick message is logged and ignored: the rendered message interpolates issue and PR titles, knowledge facts and delegated prompts, and none of that text may pick the commands that run against the agent's tree before it is pushed.
   >
   > **Note:** `publish.verify` runs the verify commands in the run's workspace at HEAD, so test-runner configuration the agent committed (conftest.py, pytest.ini, Makefile) is executed as-is. This is a determinism check, not an integrity gate; if stronger guarantees are needed, runner config can be reset to `HIVE_BASE_SHA` before verify in a future hardening.

   ```yaml
   agents:
     quality:
       sandbox:
         enabled: true
         runtime: job
         publish:
           verify:
             - uv run --no-sync pytest --collect-only -q
             - sh -c '[ -z "$HIVE_CHANGED_TESTS" ] || uv run --no-sync pytest $HIVE_CHANGED_TESTS -q'
           verify_timeout_s: 900
   ```
5. If the sandbox produced commits, `pkg/pushbroker.Broker` scans the committed diff for token-like secrets and protected-path edits, mints a short-lived scoped GitHub App token outside the sandbox, pushes the branch, and opens a PR through the existing App-authored GitHub client. Broker rejection records audit detail and nothing is pushed.
   The PR title and body come from `.hive/pr-title.txt` (first line) and `.hive/pr-body.md` if the agent wrote them in the workspace, otherwise from the HEAD commit's subject and body; a generic `[<agent>] sandbox changes` placeholder is used only when neither exists. Kick templates for sandboxed agents should ask the agent to write those two files.

## Running sandboxed kicks as Kubernetes Jobs

`sandbox.runtime: job` runs an agent's sandboxed kicks as a Kubernetes `Job` in the hive's own namespace instead of a rootless Podman container on the hive pod's node ([#6311](https://github.com/hivecommons/hive/issues/6311)). It exists for work that must run in a **different runtime than the hive image** - a different base OS, a heavyweight toolchain, or on **accelerator hardware** reached through a device plugin - while hive stays the orchestrator.

Everything above about the kick path still holds. Hive prepares the workspace on its own data PVC, writes the prompt, and after the Job exits reads the commits back and hands them to the push broker and PR path. Only step 2 changes: the launcher is `pkg/kubejob` instead of Podman.

```yaml
agent_sandbox:
  enabled: true
  job:
    workspace_claim: hive-data        # the PVC hive's /data lives on
    workspace_claim_mount: /data      # where hive sees it (default)
agents:
  deps:
    sandbox:
      enabled: true
      runtime: job
      image: registry.example/project-dev:latest
      job:
        node_selector: { accelerator: spyre }
        resources:
          limits: { "vendor.com/device": "2" }
        tolerations:
          - { key: accelerator, operator: Exists, effect: NoSchedule }
        scheduler_name: spyre-scheduler   # only if the cluster's admission webhook requires a custom scheduler for device pods
        env_from_secrets: [ artifact-registry ]
        volumes:
          - { name: cache, claim: compile-cache, mount_path: /cache }
        ttl_seconds: 3600
```

**Workspace transport is a shared claim, not a copy.** The Job mounts `workspace_claim` with a `subPath` pointing at the per-kick workspace, at the same `/workspace` mount the Podman launcher uses. The sandbox `workspace_dir` must therefore live under `workspace_claim_mount` (it does by default: `/data/agents/sandbox`). When the Job is scheduled onto a different node than hive, which is the whole point of a node selector, the claim must be **ReadWriteMany**. Hive cannot check the access mode from inside its pod; a Job that fails to start with a multi-attach error is the symptom.

**The credential boundary.** The Job receives exactly the Secrets named in `env_from_secrets`, resolved by Kubernetes from the pod template; hive never reads them. Environment variables from the allowlist are copied in with the same credential-name filter Podman applies. An allowlisted `ANTHROPIC_BASE_URL` that points at a pod-local proxy (such as `127.0.0.1:18444`) is not reachable from a Job pod, so operators must supply a routable endpoint or model credentials via `env_from_secrets`. Hive's brokered GitHub token **never enters the Job**, so a Job cannot push or open a PR itself; hive does both from the returned workspace, with the same scan and scoped-token minting as the Podman path. If a workload needs its own registry or model credentials, they go in a Secret named here, and they are that workload's to protect.

**What the operator sees.** The kick shows on the agent card like any sandboxed kick, with the runtime named in the output buffer and the audit line. The pod log is the transcript — hive collects it before the Job is deleted and archives it as the kick log, so "full log" on the card and the kick history keep working after the Job is gone — the container exit code is the sandbox exit code, and the kick timeout deletes a Job that is still running. A Job that **failed** is left in place until `ttl_seconds` (default one hour) so `kubectl describe job` and `kubectl logs` still work; a completed Job is deleted at once. Jobs carry the label `app.kubernetes.io/managed-by=hive-kubejob`.

**The terminal pane.** A job-runtime agent has no tmux session in the hive pod, so the dashboard's ▶ terminal does not attach ttyd for it. Instead it opens `/terminal/job/<agent>`, a read-only viewer that streams the newest Job pod's log through the same in-cluster client and RBAC the launcher already uses ([#14798](https://github.com/hivecommons/hive/issues/14798)): it follows the pod while the kick runs (waiting while the pod is `Pending`, e.g. for an accelerator node), prints the container exit code when it ends, and between kicks, when the completed Job has already been deleted, shows the last run's persisted transcript (`<workspace>/.hive/sandbox-transcript.log`, which now ends with an `exit code: N` line) or a clear "no sandbox run yet" state. The pane is observation only; there is no input path into the Job. The same terminal handoff and role check as the tmux pane apply.

**RBAC.** The spoke's ServiceAccount needs a Role for `batch/jobs` (create, get, list, delete) and `pods` plus `pods/log` (get, list); the terminal viewer needs nothing beyond that same `pods/log` grant. The standalone kustomize base ships it as `sandbox-job-rbac.yaml`, and the hub provisioner grants it to every hosted spoke as `hive-sandbox-jobs`.

Hive warns at boot and on reload (`config.AgentSandboxGateWarnings`) when a job-runtime agent resolves no `workspace_claim` or names a runtime hive does not have, and refuses the kick with the same message rather than failing it after the agent is marked running.

### Posting reviews from a sandbox

A reviewer can run in a sandbox too, so a review of an accelerator change can actually run it on the card node ([#14986](https://github.com/hivecommons/hive/issues/14986)). `sandbox.runtime: job` is a legal setting for a `reviewer`-base agent; the review runs on the node the Job lands on.

```yaml
agents:
  reviewer:
    sandbox:
      enabled: true
      runtime: job
      image: registry.example/project-dev:latest   # must include hive-review
      job:
        node_selector: { accelerator: spyre }
        resources:
          limits: { "vendor.com/device": "1" }
```

A sandbox cannot reach the pod-local review relay (`/var/run/hive-metrics/review-requests`), and it never holds a GitHub credential. Instead, every sandbox kick (Podman or Job) gets a **review spool**:

1. Before launch, hive creates `.hive/review-requests` in the kick workspace and sets `HIVE_REVIEW_REQUEST_DIR=/workspace/.hive/review-requests` inside the sandbox. `hive-review` writes its request there instead of the pod-local dir, in the same JSON schema.
2. Hive registers the spool with a manifest under `<work_dir>/sandbox-review-spools/` (`work_dir` is `HIVE_WORK_DIR`, default `/data/agents`). The manifest names the kick's agent. It lives outside every workspace, in a directory only hive's uid may write, so the sandbox can neither see nor forge it.
3. The review-request watcher moves each spooled request into a hive-owned staging directory beside the manifest and runs it through the same handler as the pod-local drop box. `review.max_reviews_per_head`, repo pause, the lane write allowlist, the sentinel approval block, the contributor COMMENT-only guard, the canary scan and the `agent_pr_reviewed` audit all apply unchanged. The request is **attributed to the manifest's agent**, whatever its own `agent` field says; the per-agent authorization (ACMM `CanPush` or the `converse` capability) is checked for that agent.
4. The manifest says which workspace a request came from, not who wrote it: the spool is group-writable, so any agent sharing hive's group could drop a file into it. So hive also mints a random 256-bit **spool key** for each kick, keeps it in the manifest, and hands it to the sandbox only through `HIVE_REVIEW_SPOOL_KEY` in its environment (the Podman launcher passes env by name, so the value never appears on a command line). `hive-review` writes `{"request": "<request JSON>", "mac": "<hex HMAC-SHA256(key, request JSON)>"}`. The relay stages only a request whose MAC matches this run's key and that this spool has not already accepted. An unsigned request, one signed with another run's key, or a copy of an accepted one is set aside as `.rejected`, logged at WARN, and answered with a `rejected:` result file. This stands in for the file-owner check the pod-local relay does, which can't apply here because a Job's uid is not an agent's.
5. The result is copied back to `<request>.result.json` in the spool, where the sandboxed agent reads it exactly as it would in-pod. A bad `--verdict-file` is refused with the validator's message, so the agent can fix the verdict and resubmit within the same kick.

While any spool is registered, the watcher scans on its short poll interval: a Job writes its spool over a shared volume, often from another node, where no file-change notification reaches the hive pod. After the sandbox exits, the kick waits up to a minute for the relay to take any request still in the spool, because a run-stage worktree is removed when the kick returns. The manifest is dropped once the kick has ended and nothing is left to deliver, or after 24 hours.

The relay touches the spool only through an `os.Root` opened at the workspace. A symlink the sandbox plants can't redirect a read, rename or write outside its own workspace, and a request file that is a symlink, FIFO or oversized is set aside as `.rejected`. The relay ignores a manifest directory or manifest that is not owned by its own uid or is group- or world-writable, and a manifest without a spool key. In the sidecar runtime (`CRUX_MODE=sidecar`), the runtime and hive must therefore share both `/data` and a uid. On `sandbox.runtime: job`, the key is in the Job's pod spec, so anything that can read Jobs or Pods in the namespace through the Kubernetes API can read it; agents must not hold that permission. A group member can still delete or alter a request in the spool, which loses the review but cannot post one.

Out of scope: the quality and deps lanes are unchanged, and nothing inside a Job writes to GitHub directly.

## Health and metrics for sandboxed agents

A sandboxed agent (Podman or Job runtime) has no tmux session, no in-pod process and no `pluk` log between kicks: the manager parks it in the `idle` state and each kick runs to completion in a container or Job. The dashboard therefore must not read "not running" as "session down" for these agents, and the token collector cannot find their CLI session files because the CLI's home directory lives in the sandbox, not in the hive pod.

**Last-run record.** When a sandboxed kick ends, `crux/agent` builds a `SandboxRun` (runtime, start/end, succeeded, timed out, error, commit count, branch, PR URL, `prWithheld` when the publish gate kept commits local, `logId` of the archived kick log, summary, usage) and keeps it on the agent process as `sandboxLastRun`. It is also written to `<work_dir>/sandbox-runs/<agent>.json` (restored on restart, so a pod roll does not blank the card) and appended to `<work_dir>/sandbox-runs/runs.jsonl`, which is compacted to its newest 1000 records once it passes 2 MiB (`work_dir` is `HIVE_WORK_DIR`, default `/data/agents`). The summary is the agent's `.hive/agent-report.json` `summary` when it left one, otherwise the scrubbed tail of the transcript.

**Health.** `/api/status` carries a `sandboxRun` object on every sandboxed agent with a `health` verdict the SPA uses instead of the session check:

| `health`  | Meaning | Dot |
|-----------|---------|-----|
| `ok`      | last run succeeded (or no run yet) and the next cadence occurrence is still in the future | green |
| `running` | a kick is in flight (container or Job running/pending) | amber |
| `failed`  | the last run failed or timed out — see the agent's log | red |
| `overdue` | the cadence had a due occurrence that never started, older than the grace (half the period, clamped to 15 min – 2 h); `dueAt` names it | red |

On-demand agents, paused agents and `pause`/`continuous` cadences are never overdue. A never-kicked interval agent counts its first period from the time the agent came up. The card's blocker line reads `Job ok` / `Job running` / `Job ready` (or `sandbox …` for Podman) / `last run failed` / `last run timed out` / `run overdue` for sandboxed agents instead of `session down`, and the "Now:" line shows the last run's outcome, duration, PR link and summary.

**Tokens and sessions.** The collector reads `sandbox-runs/runs.jsonl` and turns every run that reported usage into one session (`sandbox-<agent>-<startMs>`, backend `sandbox`), so TOKENS/SESSIONS, by-agent and by-model totals and cost include sandboxed work. Usage is parsed from the transcript: Claude's `--output-format json`/`stream-json` `result` line and Copilot's "Breakdown by AI model" footer are recognised. A backend that prints plain text only (Claude without `--output-format`) reports no usage; such runs stay healthy but contribute no tokens rather than an estimate.

## Network trade-off

`network_mode: none` remains available and maps to Podman's `--network=none`, but it only works for non-inference jobs or runtimes that already expose a local/socket model proxy inside the container. The default sandbox network mode is `restricted`: operators must provide a Podman network/proxy policy that allows only the inference endpoint and MITM proxy required by the selected backend. This is a compromise until every supported backend can run through a credential-free local socket without general egress.

## Remaining gaps

- The default target is the hive primary repo and default base ref; richer per-kick repo/ref selection is still future work.
- The Job runtime is a first slice: one Job per kick, workspace shared through a PVC, no per-kick Secret minting. A clone-inside-the-Job transport for clusters without ReadWriteMany storage is future work.
- Live Podman execution is covered by skip-when-absent tests; CI still needs a rootless-Podman runner lane for always-on integration coverage.
- Sandboxed inference depends on an operator-provided restricted network/proxy policy. `none` is stronger but not yet usable for all model backends.
- A stronger runtime than plain containers is under evaluation: [ADR-0026](adr/0026-sandbox-runtime-selection.md) (Proposed) compares NVIDIA OpenShell and OpenSandbox against this contract.

## Agent guardrails (defense in depth)

Beyond the credential-free sandbox above, Hive's isolation model is defense in depth rather than a single sandbox switch. Agents run as long-lived CLI sessions, but the deterministic pipeline and proxy layers decide what work they see and what writes they can perform.

### Isolation layers

1. **Policy mode** — ACMM selects advisory, measured, hold-gated, or full behavior per agent.
2. **Deterministic admission** — Go and shell checks classify work, apply holds, and decide whether an agent is kicked.
3. **Scoped credentials** — contributor relays and spoke agents use the GitHub identity and token scope appropriate to that actor; a delegated ClankeR role does not grant spoke secrets.
4. **MITM GitHub proxy** — GitHub API writes are attributed and constrained according to the current mode.
5. **Merge gates** — the literal `hold` level-gate label, dashboard `hive-pause/<hive-id>` manual holds, green checks, self-merge bans, and auto-merge sweeps are enforced outside the LLM prompt.

### Operator notes

- Prefer the least-capable ACMM level that matches the project phase.
- Keep privileged delegated roles (`ci-maintainer`, `sec-check`, `architect`) behind explicit grants.
- Use the docs in this index for concrete setup: [ACMM policy matrix](acmm-policy-matrix.md), [Agent configuration](agent-configuration.md), and [Contributor trust tiers and delegated agent roles](contributor-trust-and-roles.md).
