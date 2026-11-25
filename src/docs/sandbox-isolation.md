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

**The per-agent toggle writes a complete block, or refuses (#18525).** The dashboard General tab's **Run in sandbox** toggle (`PUT /api/config/agent/{name}/general` with `sandboxEnabled`) used to write `sandbox.enabled: true` and nothing else, so an agent turned on while `agent_sandbox` had no image fell back to Podman with no image and failed every kick with "sandbox image is not configured". Turning the toggle on now resolves the agent's effective block — its own `sandbox` fields first, then the `agent_sandbox` defaults — and:

- when that block is complete (an image, a runtime hive has, and `job.workspace_claim` for the Job runtime, unless `workspace_transport: copy`), writes it with the image and runtime pinned and `enabled: true`;
- when the request names another agent with `copy_from` (the dashboard sends `sandboxCopyFrom`), copies that agent's complete block instead, **minus its `publish` block**: `publish.verify` commands are written for one repository's work, so the receiving agent keeps its own;
- otherwise refuses with HTTP 400 naming what is missing and the agents a block could be copied from.

When the toggle is switched on, the General tab shows a **Sandbox settings from** picker: the `agent_sandbox` defaults when they are complete, plus every agent that already has a complete block. If neither exists it shows an inline error instead, and the save would be refused. `GET /api/config/agent/{name}` carries the inputs as `general.sandboxDefaultsProblem` (empty when the defaults are complete) and `general.sandboxCopySources`. Turning the toggle off keeps the block and only sets `enabled: false`.

**Switching an agent into sandbox mode stops its tmux session.** A sandboxed agent never runs a CLI in tmux, but the session it had before the switch used to stay alive and keep the agent in `running`, so every governor kick was refused. When an agent's config changes it into sandbox mode (the toggle, a config reload, or the global gate turning on), the manager archives the session's scrollback as a kick log, reaps the CLI, kills the session and parks the agent in `idle`, and writes a `sandbox_mode_entered` audit line. A kick refused because the agent is still `running` now says which state is holding it: `a sandbox kick is already running`, or `agent has a live interactive session; sandbox kicks are blocked until it stops`.

The second gate is deliberate rather than an oversight, and it is not safe to simply collapse. A sandboxed agent runs a different execution model — no tmux CLI at all, and every kick is a Podman run against the primary repo — and `startSandboxKickLocked` has **no fallback to the tmux path**: an agent opted in without a resolvable image fails every kick outright rather than degrading. Making the global flag sufficient would therefore convert working agents into permanently failing ones on any hive that set it without an image. Changing that default is a fleet-affecting decision that wants measurement, not a code-reading; the warning above is the part that is safe today.

**Cline's provider capability is separate from the Git workspace.** Its bounded
worker uses dedicated provider Secrets through existing `job.env_from_secrets`
for Jobs, or an opaque, per-run selected-provider file mounted `ro,Z` outside
the workspace for Podman. The latter requires an explicit absolute
`HIVE_CLINE_CONFIG_DIR`; it does not grant arbitrary host mounts, share the full
operator config, or change other engines' mounts. Private UID-owned tmpfs state,
container teardown and staged-file revocation/cleanup are part of that scoped
capability. GitHub credentials still never enter. No silent tmux fallback,
expanded credential environment allowlist, or confinement bypass is provided.
See [Cline sandbox setup](integrations/cline.md#disposable-sandbox-kicks).

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
   **The PR is credited to the agent** ([#18575](https://github.com/hivecommons/hive/issues/18575)). Hive records the PR the same way it records one an agent opens from its pane through the pr-request relay: the `-- hive: agent=<name> …` attribution trailer on the body (when `governor.attribution_trailer` is on) and an `agent_pr_created` audit entry for `repo=<owner/name>, number=<n>`, plus `via=sandbox`. Every governor path that maps a PR to its agent reads that entry: the FIX-BEFORE-NEW block, the red-CI webhook kick, the review-thread monitor and held-PR routing. Red CI on a sandbox PR therefore kicks the agent that opened it, not `scanner`. When the PR client cannot record the entry itself (the sidecar runtime's callback adapter), the runtime emits the same `agent_pr_created` entry through the sandbox audit callback, so the PR is still credited, without the trailer. As a last resort, the governor also reads the agent out of the work branch name, `hive/<agent>/<yyyymmddhhmmss>`.

### Fix kicks on an existing PR

A sandboxed fix kick can add commits to one of the agent's own open PRs instead of opening a new one ([#18574](https://github.com/hivecommons/hive/issues/18574)). Hive picks the target from hive-written parts of the kick message. The first match wins:

1. An explicit `[hive:target-pr <owner>/<repo>#<n>]` first line (after an optional `[agent:<name>]` line).
2. The first line of the targeted red-PR kick, `🔴 FIX-BEFORE-NEW — <owner>/<repo>#<n> is red and has not moved`.
3. The first line of the red-CI webhook kick, `CI failed on <owner>/<repo>#<n> (<check>) …`.
4. The first row of the kick's FIX-BEFORE-NEW block (`## 🔴 FIX-BEFORE-NEW — your open PRs with failing CI or merge conflicts`). An adopted contributor PR is skipped.
5. The agent's topic claim, when the kick's topic-claims block lists exactly one PR. With several claims the kick does not say which topic it is for, so it opens a new PR as before.

A target in another repository is ignored, because the workspace clones the primary repo only. The kick needs push rights (ACMM `CanPush` and `project.prs_allowed`) and a PR client that can read PR targets: the in-process hive client can, and the sidecar's callback adapter cannot yet. Without one, hive logs a warning and the kick opens a new PR as before. Review kicks and run-stage kicks never get a target.

**Workspace prep.** Hive reads the PR and checks it before the sandbox starts. The PR must be open. Its head branch must live in the kick's repository, so a fork PR is refused. The PR's `agent_pr_created` record must credit it to this agent. If that record is older than the audit log's 90-day lookback, hive reads the body's attribution trailer instead, but only on a PR the hive's App authored. Hive then fetches `+refs/heads/<head>:refs/remotes/origin/<head>` with the clone token and checks the head out on `<head>`. If the fetched commit differs from the head GitHub reported, the kick is refused. Hive also prepends a `[hive:sandbox-target-pr]` section to the prompt. It names the PR, branch and head commit, and tells the agent to commit on top, without rebasing or opening a PR. The run's commits are counted from that head.

**The push.** After the publish gate and `publish.verify`, hive reads the PR again. It must still pass every check above, keep the same head branch, and sit at the same head commit. The broker then runs its usual empty-commit, sign-off, protected-path and secret scans over the kick's own commits only (`base = <checked-out head>`). The sign-off scan refuses an outgoing commit that carries the lane's own `Signed-off-by` when a different identity authored or committed it: a lane signs off only commits it wrote, so a `git rebase --signoff` over a person's commit never leaves the sandbox, and the refusal names the commit, its author and the fix (leave the author's commits as written and ask them to sign off). Commits the lane authored and committed must carry its trailer; a person's commit left untouched passes. It also requires those commits to descend from the checked-out head. It pushes `HEAD:refs/heads/<head>` with `--force-with-lease=refs/heads/<head>:<checked-out head>`, so the push lands only if the remote branch still points at that commit. Together these make the push a fast-forward of exactly the head the kick started from: it never overwrites a commit someone else pushed. No new PR is opened. The run record's PR link is the existing PR, and `sandbox_complete` carries `target_pr=<n>`.

**Refusals fail the kick and push nothing.** A closed PR, a fork PR, a PR another agent opened, a PR no hive agent opened, a head that moved since checkout, an unreadable PR or a failed scan all fail the kick with a reason that names the PR. The run record carries the reason, and the audit log gets a `sandbox_target_pr_refused` line with `repo=`, `number=`, `source=` and `reason=`, or the broker's usual `sandbox_broker_rejected`. If the refusal comes after the run, the commits stay in the workspace, and the run record notes them as withheld.

## Setting up a sandbox from the dashboard

Nothing about a sandbox needs a hand edit of the config files (hivecommons/hive#18797). Everything below saves through the normal config API, which writes both `/data/hive.yaml.runtime` and `/data/hive.yaml.dashboard`.

- **Hive-wide defaults.** Settings, Security, "Advanced sandbox: default image and Job settings" edits `agent_sandbox.image`, `runtime`, the Job defaults (workspace claim and mount, service account, scheduler name, TTL) and `agent_sandbox.job.env_from_secrets`. The section opens by itself when the sandbox is on and no image is set. API: `PUT /api/config/governor/security` with `agentSandboxImage`, `agentSandboxRuntime`, `agentSandboxJobWorkspaceClaim`, `agentSandboxJobEnvFromSecrets` and the other `agentSandbox*` keys.
- **Hive-wide scheduling, preflight and read relay** ([#19553](https://github.com/hivecommons/hive/issues/19553)). Inside that section, "Job scheduling, resources, preflight, setup and GitHub read relay" edits `agent_sandbox.job.node_selector`, `resources.limits` and `resources.requests` (one `key=value` per line, extended resources such as `ibm.com/spyre_pf=1` included), `tolerations` (one per line in taint syntax: `key=value:Effect`, `key:Effect`, `key`, or `*`), `image_pull_secrets`, `schedule_timeout_s`, the hive-wide `agent_sandbox.preflight` (commands, one per line, and its three timeouts), the hive-wide `agent_sandbox.setup` under "Workspace setup" (setup commands and health checks, one per line, and its two timeouts; [#19574](https://github.com/hivecommons/hive/issues/19574)) and `agent_sandbox.job.github_read_proxy` (`enabled`, `listen_addr`, `endpoint`, `no_proxy`). The read relay starts at boot, so a change to it takes effect when hive restarts. API keys: `agentSandboxJobNodeSelector`, `agentSandboxJobResources` (`{limits, requests}`; a key left out is unchanged), `agentSandboxJobTolerations` (`[{key, operator, value, effect}]`), `agentSandboxJobImagePullSecrets`, `agentSandboxJobScheduleTimeoutS`, `agentSandboxPreflight` (`{commands, timeoutS, totalTimeoutS, retryDelayS}`; an empty list or null removes it), `agentSandboxSetup` (`{commands, healthCommands, timeoutS, totalTimeoutS}`; empty lists or null remove it) and `agentSandboxJobGithubReadProxy` (`{enabled, listenAddr, endpoint, noProxy}`).
- **One agent.** The agent's General tab has "Sandbox settings: image, runtime and Job" with its own `sandbox.image`, `sandbox.runtime` and Job fields. A blank field inherits the hive default, shown as the placeholder. API: `PUT /api/config/agent/<name>/general` with `sandboxImage`, `sandboxRuntime` and `sandboxJob` (`workspaceClaim`, `workspaceClaimMount`, `serviceAccount`, `schedulerName`, `ttlSeconds`, `envFromSecrets`, `imagePullSecrets`, and since #19553 `nodeSelector`, `resources`, `tolerations` and `scheduleTimeoutS`, in the shapes above). The same tab has "Node preflight": with "Use this agent's own preflight" off the agent uses `agent_sandbox.preflight`; on, it uses its own list, and an empty list turns preflight off for that agent (API: `sandboxPreflight`, null to inherit). "Workspace setup" below it works the same way with "Use this agent's own setup" (API: `sandboxSetup`, null to inherit, empty lists to turn it off). Affinity, volumes and the relay image have no field; they travel with the copy action below and survive an edit.
- **What is in effect.** "In effect for this agent" lists every resolved Job value, the node preflight and the workspace setup, and where each comes from: this agent's override, `agent_sandbox` defaults, or not set. A per-agent `job` block replaces the hive-wide node selector, resources, tolerations, service account and scheduler name as a whole (the claim, TTL, schedule timeout and pull secrets still fall back one by one), so setting any one Job field on an agent drops the hive-wide node selector for it. The tab warns, naming each hidden `agent_sandbox.job` field, when that happens.
- **Validated twice.** The browser explains a bad line before Save and leaves that field out of the save; the API refuses it again with 400 and changes nothing: label keys and values and resource names must be Kubernetes names, quantities must look like `2`, `500m` or `16Gi`, toleration operators are `Equal` or `Exists` and effects `NoSchedule`, `PreferNoSchedule` or `NoExecute`, pull secrets are Secret names only, preflight and setup commands keep the config bounds (32 commands per list, 4096 bytes each) and are refused when they carry a credential shape, and the read relay needs a `host:port` listen address and an `https://` endpoint without a user or password.
- **Copy from another agent.** "Copy sandbox settings from..." lists the agents whose block is complete (an image and a usable runtime, plus a workspace claim for `runtime: job`) and copies the block, turning the sandbox on. API: `PUT /api/config/agent/<name>/general` with `{"sandboxCopyFrom": "<agent>"}`. The copy is deep. The source's `publish.verify` commands are not copied (they are written for the source's repository); the receiving agent keeps its own.
- **Secrets are names only.** `env_from_secrets` holds Kubernetes Secret names. The API refuses anything that is not a valid name (a `KEY=value` pair, for one), so a value can never be written to the config file through these controls.
- **Refused without an image.** Turning the sandbox on, or saving an enabled sandbox, when no image resolves from the agent or `agent_sandbox.image` is refused with 400 and a message naming `sandbox.image` and `agent_sandbox.image`. The "no sandbox image" warning on the agent card and in the General tab links to the agent's Sandbox settings and to the default image controls.

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

To run on any Spyre node except a known bad host, add Kubernetes node affinity
under `job` (replace `bad-spyre-node` with the node's actual hostname):

```yaml
agent_sandbox:
  job:
    node_selector: { accelerator: spyre }
    affinity:
      nodeAffinity:
        requiredDuringSchedulingIgnoredDuringExecution:
          nodeSelectorTerms:
            - matchExpressions:
                - key: kubernetes.io/hostname
                  operator: NotIn
                  values: [bad-spyre-node]
```

The node selector and required node affinity both have to match. Do not pin
`node_selector` to a particular `kubernetes.io/hostname` if the Job should
move among Spyre nodes; a hostname selector can restrict it to one node or
conflict with the exclusion and leave the Job pending. If using a custom
`scheduler_name`, ensure that scheduler honors Kubernetes `nodeAffinity`;
otherwise the exclusion is not guaranteed.

### Node preflight and failing fast on an unschedulable Job

Two checks keep a bad or missing node from costing a whole kick ([#18811](https://github.com/hivecommons/hive/issues/18811)). Both are for `runtime: job`.

**`sandbox.preflight`: a node health check before the agent starts.** An accelerator card can be unusable on one node (a VFIO mmap failure, a crashed device) while the pod starts fine; without a check hive learns that only when the agent's own tests fail, minutes in. `preflight.commands` is a list of shell commands run in the Job's main container after hive's relay check and before the agent, in the same image, workspace and environment. Each runs as `sh -lc <cmd>`, like `publish.verify`, under `timeout_s` (default 120 s) and a `total_timeout_s` for the whole list (default 300 s). It is off unless `commands` is set, per agent (`agents.<name>.sandbox.preflight`) or hive-wide (`agent_sandbox.preflight`); an agent's block replaces the global one, and an empty `commands` there turns it off for that agent. Timeouts left at zero fall back to the global block, then the defaults.

```yaml
agent_sandbox:
  preflight:
    commands:
      # A device smoke test: fails on a node whose Spyre card is unusable.
      - python -c 'import torch; torch.ones(1).to("spyre")'
    timeout_s: 120          # per command
    total_timeout_s: 300    # all commands
    retry_delay_s: 0        # wait before the retry on another node; 0 = immediately
```

A failing command ends the container in seconds with exit code 75 after printing `hive-node-preflight: FAILED command 1 of 1 exited 1: <cmd>`, and hive reports the kick as `node_unhealthy`:

- The command's output is in the kick transcript and the kick log, the run record carries `failReason: node_unhealthy` and the `node` the pod ran on (`spec.nodeName`), and the audit log names the node (`sandbox_node_unhealthy`).
- It is **not an agent failure.** The agent never ran, so it returns to `idle` rather than `failed`, nothing that watches failed agents (restart, escalation, a failure-fix loop) sees it, and the next cadence tries again. The dashboard shows `node unhealthy` with the node, distinct from `last run failed`.
- **One retry on another node.** Hive re-creates the Job once, after `retry_delay_s`, with a node-affinity `NotIn` on `kubernetes.io/hostname` for the failed node, added to every required term of the `affinity` above and never pinning a node (`sandbox_node_unhealthy_retry` in the audit log; the first attempt's output is kept at the top of the transcript). If the retry fails its preflight too, the kick fails as `node_unhealthy`. A cluster whose node names differ from the `kubernetes.io/hostname` label, or a custom `scheduler_name` that ignores node affinity, cannot honor the exclusion.

The commands come from configuration only, never from a kick message, like `publish.verify`. They are part of the Job manifest and their output is in the pod log, so never put a credential on a command line; the script prints nothing from the environment. A preflight set for a runtime other than `job` is logged and ignored, and only agent kicks run it (the `publish.verify` Jobs do not).

**The agent is told which hardware it was given.** When a `runtime: job` kick's resolved `job.resources` request an extended resource (any name with a domain outside `kubernetes.io`, such as `vendor.com/device`; `cpu`, `memory`, `ephemeral-storage` and `hugepages-*` do not count), hive prepends a `[hive:sandbox-environment]` section to the prompt ([#19549](https://github.com/hivecommons/hive/issues/19549)). It names the runtime and each extended resource with its count, says whether the configured `preflight` commands passed on the node (the agent only starts after they do) or that none is configured, and tells the agent that a device test which cannot see the device is an environment failure to report in its review, not a reason to call the host card-free or to skip device tests silently. The section is built from configuration only, never from the kick message, and carries resource names and counts only: no Secret, service account, node selector or preflight command. Podman and the other runtimes, and Jobs that request no extended resources, get no section. How to run the project's device suite stays in the repository's own `AGENTS.md`.

**`job.schedule_timeout_s`: fail fast when no node takes the pod.** While waiting on the Job, hive also reads its pod. If the pod stays `PodScheduled=False` with reason `Unschedulable` for `schedule_timeout_s` (default 600 s, in `job`, with the global value as the fallback for an agent's own `job` block), hive fails the kick as `waiting_for_node` with the scheduler's message (for example `0/38 nodes are available: ...`), deletes the Job and releases the agent, instead of holding its slot for the whole kick timeout plus the cluster-side deadline margin. There is no retry; the next cadence tries again. While it waits the dashboard shows `Job waiting for a node` with the scheduler's message, distinct from `Job running`, and the audit log records `sandbox_waiting_for_node`, then `sandbox_waiting_for_node_failed` if it gives up. If your cluster autoscaler needs longer than ten minutes to add an accelerator node, raise `schedule_timeout_s`; the kick timeout still bounds the wait.

| Reason | Meaning | What to check |
|--------|---------|---------------|
| `node_unhealthy` | `preflight`, or a `setup.health_commands` check, failed on the node, and again on the retry node | The command's output in the kick log; the node named in the run record; cordon or fix the node |
| `setup_failed` | a `setup.commands` command failed or timed out; no retry | The command's output in the kick log; the setup script and the package mirror it installs from |
| `waiting_for_node` | the pod stayed Unschedulable past `schedule_timeout_s` | The scheduler's message in the kick log: `node_selector`, `affinity`, `tolerations`, device `resources` and `scheduler_name` against the cluster's nodes; a retry excluding the only fitting node ends here |

### Workspace setup before the agent

**`sandbox.setup`: install what every run needs, then check the node with it** ([#19574](https://github.com/hivecommons/hive/issues/19574)). Some images deliberately leave out a toolchain that each run must install, for example a pinned ML stack from an internal package mirror via a script in the repo. Without a setup step that only happens if the agent reads the repo's `AGENTS.md` and decides to do it, and `preflight` cannot check the accelerator through a stack that is not installed yet. `setup` runs in the Job's main container after the preflight and before the agent, for `runtime: job` only. The full order of a Job-runtime agent kick:

```text
hive pod                          Job container (one pod, same image, same environment)
--------                          -----------------------------------------------------
clone / check out the workspace   1. relay check (hive-relay-preflight)
on the workspace claim     --->   2. sandbox.preflight commands       fail: node_unhealthy, retry on another node
write the prompt                  3. sandbox.setup.commands           fail: setup_failed, no retry
create the Job                    4. sandbox.setup.health_commands    fail: node_unhealthy, retry on another node
                                  5. the agent
```

```yaml
agent_sandbox:
  setup:
    commands:
      # Install the pinned stack from the mirror; runs in the checked-out repo.
      - ./scripts/install-ml-stack.sh
    health_commands:
      # Check the device through the stack just installed.
      - python -c 'import torch; torch.ones(1).to("<device>")'
    timeout_s: 900          # per command (default 900)
    total_timeout_s: 1800   # commands and health_commands together (default 1800)
agents:
  deps:
    sandbox:
      setup:                # replaces the hive-wide block for this agent
        commands: []        # empty lists turn setup off for this agent
```

- **Where it runs.** Each command runs as `sh -lc <cmd>` in the Job's image with the checked-out workspace (`/workspace`, the container's working directory) as its current directory, so a repo script is called by its relative path. A file the step writes to the workspace (a virtualenv, a wheel cache) is there for the agent.
- **What it can see.** Exactly the agent's own environment, nothing more: the allowlisted variables, the keys of the Secrets named in `job.env_from_secrets`, the read relay's `HTTPS_PROXY` and trust bundle when it is on, and hive's relays on `PATH`. The step gets no Secret, mount or variable the agent does not get, and hive's GitHub token never enters the Job. A mirror credential goes in a Secret named in `env_from_secrets`, never on the command line.
- **A failed setup command is `setup_failed`.** The first non-zero exit or timeout ends the container with exit code 70 after printing `hive-sandbox-setup: FAILED setup command 1 of 1 exited 1: <cmd>`. The kick fails: the run record carries `failReason: setup_failed` and the node, the agent shows `setup failed` and goes to `failed` like any failed kick, and the audit log records `sandbox_setup_failed` and `sandbox_failed`. There is no retry on another node: the cause is the script, the mirror or what it installs, not the node.
- **A failed health check is `node_unhealthy`.** It exits 75 after printing `hive-sandbox-setup-health: FAILED health command 1 of 1 exited 1: <cmd>` and takes the preflight's path: not an agent failure, one retry on another node after `preflight.retry_delay_s`. The retry runs the whole Job again, setup commands included, on the same workspace, so they must be safe to repeat.
- **Merge and bounds.** Off unless `commands` or `health_commands` is set. Like `preflight`: an agent's `setup` block replaces the hive-wide one, empty lists there turn it off for that agent, timeouts left at zero fall back to the global block and then the defaults, at most 32 commands per list and 4096 bytes per command. Setup time counts toward the kick's `timeout_s`.
- **Output and trust.** The commands come from configuration only, never from a kick message. They are in the Job manifest; their output is in the pod log, the kick transcript and the kick log, scrubbed of token shapes like the rest of the kick output. A setup block for a runtime other than `job` is logged and ignored, and only agent kicks run it (the `publish.verify` Jobs do not).
- **The agent is told.** When the kick gets the `[hive:sandbox-environment]` section (a Job requesting a device), it also says how many setup commands and health checks ran before the agent started, so the agent does not repeat the install.

**Workspace transport is a shared claim, not a copy.** The Job mounts `workspace_claim` with a `subPath` pointing at the per-kick workspace, at the same `/workspace` mount the Podman launcher uses. The sandbox `workspace_dir` must therefore live under `workspace_claim_mount` (it does by default: `/data/agents/sandbox`). When the Job is scheduled onto a different node than hive, which is the whole point of a node selector, the claim must be **ReadWriteMany**. Hive cannot check the access mode from inside its pod; a Job that fails to start with a multi-attach error is the symptom.

**Copy transport for clusters without ReadWriteMany storage (#19621).** Opt in with `workspace_transport: copy` under `agent_sandbox.job` (or an agent's `sandbox.job`; a per-agent block without it inherits the global value). The Job then never mounts hive's data claim, and `workspace_claim` is not needed:

```yaml
agent_sandbox:
  runtime: job
  job:
    workspace_transport: copy   # default: claim
```

1. Hive prepares the workspace on its own claim with its own credentials, as before.
2. The Job's `/workspace` is a pod-local `emptyDir`. A `hive-bridge` init container from the hive image (`relay_image`, resolved like the relays) copies hive's `crux` binary into a second `emptyDir`, and the kick container runs `crux job-bridge -- <kick command>`.
3. Hive reaches the bridge at the pod IP on port 8477 and pushes the workspace as a tar (`PUT /workspace`). The bridge then runs the kick command; its output stays the pod log and its exit code stays the container's.
4. Once the command exits, hive pulls the workspace back (`GET /workspace`, answered 409 while the command runs), swaps it in on its own claim, and sends `POST /done`. The push broker and PR path run unchanged on that workspace.

Every bridge request but `/healthz` carries a random per-run bearer token. It lives in a per-Job Secret that the pod reads through `secretKeyRef` (never a literal in the Job spec), is removed from the kick command's environment, and the Secret is deleted with the Job, so hive's service account needs `create` and `delete` on `secrets` in the Job namespace. A per-Job NetworkPolicy admits only pods with hive's own labels to port 8477: with `network: none` it is the deny-all policy plus that one ingress rule (egress stays denied); on other networks it is an ingress-only policy and egress is unchanged. Hive's own pod must carry labels for that, and any NetworkPolicy on hive's side must allow it to reach Job pods on that port.

Limits of this first version: the review, issue and topic relays are off (their spool needs a directory hive and the Job share while the kick runs), the kick needs an explicit command, `relay_image` cannot be `none`, and the `crux` binary must match the Job node's architecture, so point `relay_image` at a multi-arch hive image when Jobs land on other architectures. Files the agent writes reach hive when the kick ends, not while it runs.

**The credential boundary.** The Job receives exactly the Secrets named in `env_from_secrets`, resolved by Kubernetes from the pod template; hive never reads them. Environment variables from the allowlist are copied in with the same credential-name filter Podman applies. An allowlisted `ANTHROPIC_BASE_URL` that points at a pod-local proxy (such as `127.0.0.1:18444`) is not reachable from a Job pod, so operators must supply a routable endpoint or model credentials via `env_from_secrets`, or turn on the inference listener described below, which keeps the gateway key in hive. Hive's brokered GitHub token **never enters the Job**, so a Job cannot push or open a PR itself; hive does both from the returned workspace, with the same scan and scoped-token minting as the Podman path. If a workload needs its own registry or model credentials, they go in a Secret named here, and they are that workload's to protect.

**What the operator sees.** The kick shows on the agent card like any sandboxed kick, with the runtime named in the output buffer and the audit line. The pod log is the transcript — hive collects it before the Job is deleted and archives it as the kick log, so "full log" on the card and the kick history keep working after the Job is gone — the container exit code is the sandbox exit code, and the kick timeout deletes a Job that is still running. A Job that **failed** is left in place until `ttl_seconds` (default one hour) so `kubectl describe job` and `kubectl logs` still work; a completed Job is deleted at once. Jobs carry the label `app.kubernetes.io/managed-by=hive-kubejob`.

**Orphaned Jobs fail and are reaped (#18525).** The kick timeout lives in hive's memory, so a hive restart mid-kick used to leave its Job with no owner, and `ttl_seconds` only applies after a Job finishes; one Job sat `Pending` for 11 days. Two backstops now cover that:

- Every Job carries `activeDeadlineSeconds`: the kick timeout (`timeout_s`, default 45 minutes) plus a fixed 15-minute margin (`kubejob.DeadlineMargin`), or the run's own context deadline plus the margin when that is later. Hive's own timeout always fires first; the deadline makes the cluster fail a Job that is still `Pending`, `Unschedulable` or running once hive has lost track of it.
- At boot, when any agent (or `agent_sandbox`) selects the Job runtime, hive lists the Jobs labelled `app.kubernetes.io/managed-by=hive-kubejob` in its namespace and deletes each unfinished agent-sandbox Job (`hive-<agent>-sandbox` kicks and `hive-<agent>-verify` preflights) that no live kick owns and that is older than the longest Job-runtime kick timeout, plus any that never got a ready pod and are past the deadline. Each one is logged (`audit: reaped orphaned sandbox job`) and recorded in the audit log as `sandbox_job_reaped`. Finished Jobs are left to `ttl_seconds`; Jobs without the label, and contained runs (`crux-run-<id>`), are never touched. The sweep uses the same `list` and `delete` grants on `batch/jobs` as the launcher.

**The terminal pane.** A job-runtime agent has no tmux session in the hive pod, so the dashboard's ▶ terminal does not attach ttyd for it. Instead it opens `/terminal/job/<agent>`, a read-only viewer that streams the newest Job pod's log through the same in-cluster client and RBAC the launcher already uses ([#14798](https://github.com/hivecommons/hive/issues/14798)): it follows the pod while the kick runs (waiting while the pod is `Pending`, e.g. for an accelerator node), prints the container exit code when it ends, and between kicks, when the completed Job has already been deleted, shows the last run's persisted transcript (`<workspace>/.hive/sandbox-transcript.log`, which now ends with an `exit code: N` line) or a clear "no sandbox run yet" state. The pane is observation only; there is no input path into the Job. The same terminal handoff and role check as the tmux pane apply.

**Private-registry images (#18819).** The Job pod's `imagePullSecrets` are the deduplicated union, in this order, of: hive's own pod secrets (so the `hive-relays` init container can pull the hive image), the Job service account's `imagePullSecrets`, and `sandbox.job.image_pull_secrets`. Kubernetes adds a service account's pull secrets only to a pod that has none of its own, so hive reads the service account through the API (`get serviceaccounts`) and merges the names itself. When that grant is missing or the read fails, hive logs a warning and carries on with the other two lists; it never fails the kick. `image_pull_secrets` holds Secret **names** only (DNS-1123 names, at most 32; a value is refused). A per-agent `job` block that names none inherits the `agent_sandbox.job` list. Hive reads, logs and writes names, never Secret contents. When `service_account` is set and `image_pull_secrets` is not, the boot and reload warning reminds the operator to set it if the grant is absent. The dashboard sandbox controls do not carry this field yet; the per-agent API accepts `sandboxJob.imagePullSecrets`.

```yaml
agent_sandbox:
  job:
    service_account: hive-spyre-job
    image_pull_secrets: [ registry-pull ]
```

**Authenticated GitHub reads for Jobs (#18865).** A Job runs in its own pod, so by default every `api.github.com` call it makes is anonymous: GitHub allows 60 requests per hour per source IP, shared by every agent on the node, and `uv lock --upgrade` on a repo with git dependencies falls back to full clones when the API call fails. The opt-in GitHub read relay fixes that without giving the Job a credential. Default off:

```yaml
agent_sandbox:
  job:
    github_read_proxy:
      enabled: true
      listen_addr: ":8443"                           # required when enabled; there is no default
      endpoint: https://hive.hive-system.svc:8443   # how Job pods reach hive; required, https only
      no_proxy: [ models.internal.example ]          # optional extra direct hosts
```

The feature is **off by default**. When `enabled` is true, `listen_addr` is required (there is no default bind, so hive never opens a port you did not name) and `endpoint` must be `https://`; config validation fails with an error naming the key otherwise. With it on, hive listens on `listen_addr`, **serving TLS only** with a certificate issued by hive's existing proxy CA for the `endpoint` host (a DNS or IP SAN; no new CA, and no CA key is written anywhere new), and every `sandbox.runtime: job` kick (the agent run, not the `publish.verify` preflights) gets `HTTPS_PROXY` set to `endpoint` (an `https://` proxy URL) with a **per-kick relay key**, plus `SSL_CERT_FILE`, `GIT_SSL_CAINFO`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE` and `NODE_EXTRA_CA_CERTS` pointing at a trust bundle hive writes to `<workspace>/.hive/github-read-ca.pem` (the system roots plus hive's proxy CA, so the Job still trusts the real internet). `localhost`, `.svc` and `.cluster.local` bypass the relay. The block is read from `agent_sandbox.job` only; a per-agent `job` block cannot enable or repoint it. You must expose the listener to the Job pods yourself: a Service for `listen_addr`, and a NetworkPolicy allowing only the Job pods to reach it.

**Two layers guard the listener.** The first is the NetworkPolicy: it decides which pods can open a connection to the relay port at all. The second is the per-kick relay key: a connection that gets through must still present a live key, or it gets a 407 and nothing else. The key is random per kick, held by hive only as a hash, never logged, and revoked when the kick ends. Because the listener serves TLS only, the key never crosses the pod network in cleartext, and a plain-HTTP connection is dropped before any proxy handling. Example NetworkPolicy, applied in hive's namespace, allowing ingress on the relay port only from pods in the Jobs' namespace (replace the placeholders):

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: hive-github-read-relay
  namespace: <hive-namespace>
spec:
  podSelector:
    matchLabels:
      <label-selecting-the-hive-pod>: <value>
  policyTypes: [ Ingress ]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: <jobs-namespace>
          podSelector:
            matchLabels:
              app.kubernetes.io/managed-by: hive-kubejob
      ports:
        - protocol: TCP
          port: 8443   # your listen_addr port
```

Client support for an `https://` proxy URL varies. Go, Node (through its proxy agents) and libcurl 7.52 or newer (curl, and git built on it) can speak TLS to a proxy. libcurl checks an https proxy against its own CA setting rather than the origin's, so hive also sets `http.proxySSLCAInfo` through `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_0`/`GIT_CONFIG_VALUE_0` (skipped when the Job already defines `GIT_CONFIG_COUNT`). Some clients, for example Python HTTP stacks built on urllib3 older than 2.0, cannot speak TLS to a proxy; for those the Job's reads go out anonymously, as without the relay. Hive does not downgrade to a cleartext proxy for them.

What the relay does:

| Host | Request | What hive does |
|---|---|---|
| `api.github.com`, `codeload.github.com`, `raw.githubusercontent.com` | `GET`, `HEAD` | Strips any `Authorization` the Job sent and attaches hive's read token. |
| `github.com` | `GET /<repo>.git/info/refs?service=git-upload-pack`, `POST /<repo>.git/git-upload-pack` (git fetch and clone, uv's git resolution) | Same, as Basic `x-access-token`. |
| `github.com` | other `GET`/`HEAD` (archives, release downloads) | Forwarded with no credential. |
| the four hosts above | every other method, GraphQL, `git-receive-pack` (push discovery included) | Refused with a 403 and never sent to GitHub. |
| any other host | `CONNECT` on 443 | Opaque tunnel, no interception and no credential, through the same destination guard as the agent proxy (loopback, link-local metadata and private ranges are refused). |

The credential is the existing **read tier** (`shared-uid-read`: metadata, contents, actions, checks, pull requests and issues, all read), minted from the Hive App and narrowed to `project.repos`. It is never the write token and never enters the Job: the Job holds only the relay key, which authenticates it to hive's listener and is worthless to GitHub. Hive caches the token for 45 minutes and logs decisions, never token bytes. The key is revoked when the kick ends. When no read token can be minted (no App auth, a repo outside the installation), reads go out anonymously, as they did before; the Job never receives an error it would not have had without the relay. Public repositories outside `project.repos`, such as a git dependency, are readable with that token because GitHub serves public data to any authenticated caller.

This is separate from `HIVE_PROXY_INJECT_GH_AUTH`, which governs the placeholder token of agents running on the hive pod; the relay does not need it and is not affected by it. Limits to know: a Job that sets its own `HTTPS_PROXY` or `NO_PROXY` can route around the relay and is back to anonymous reads; `publish.verify` Jobs do not use it; GraphQL reads from a Job are refused because the relay cannot prove a query is read-only. A shared git dependency cache ([#18865](https://github.com/hivecommons/hive/issues/18865), option 3) is a separate, complementary change.

**Inference for Jobs on a model gateway backend (#19619).** An agent on a model gateway backend reaches its model through hive's inference translator on `127.0.0.1:18444`, which a Job pod cannot reach. The opt-in inference listener serves the same translator to Job pods, so a Job holds no gateway key. Default off:

```yaml
agent_sandbox:
  job:
    inference_proxy:
      enabled: true
      listen_addr: ":18446"                                   # required when enabled; there is no default
      endpoint: https://hive-inference.hive-system.svc:18446  # how Job pods reach hive; required, https only
```

The rules match the read relay: when `enabled` is true, `listen_addr` is required and `endpoint` must be `https://` without credentials, and the block is read from `agent_sandbox.job` only. With it on, hive serves the translator on `listen_addr` over TLS only, with a certificate from its proxy CA for the `endpoint` host. Every `sandbox.runtime: job` kick of an agent on a model gateway backend sets the agent's translator route as a tmux launch does and gets `ANTHROPIC_BASE_URL` set to `endpoint`, a **per-kick key** in `ANTHROPIC_API_KEY`, and `NODE_EXTRA_CA_CERTS` pointing at a trust bundle hive writes to `<workspace>/.hive/inference-ca.pem`. The listener maps the key to the kick's agent, attaches the gateway credential itself, records usage as it does for agents that are not sandboxed, and refuses any other key with a 401, including the loopback `sk-hive-<agent>` form. The key is random per kick, held by hive only as a hash, never logged, and revoked when the kick ends. Because the gateway key never leaves hive, Model Gateways key rotation needs no Secret sync. These explicit variables take precedence over the same names in an `env_from_secrets` Secret, so a Secret recipe keeps working while the listener is off. The loopback translator and its socket-UID check are unchanged. As with the read relay, you add a Service for `listen_addr` and a NetworkPolicy that allows only the Job pods to reach it (the read relay example above, with this port).

**RBAC.** The spoke's ServiceAccount needs a Role for `batch/jobs` (create, get, list, delete), `pods` plus `pods/log` (get, list) and, optionally, `serviceaccounts` (get) so the Job service account's pull secrets are merged; the terminal viewer needs nothing beyond that same `pods/log` grant. The standalone kustomize base ships it as `sandbox-job-rbac.yaml`, and the hub provisioner grants it to every hosted spoke as `hive-sandbox-jobs`.

Hive warns at boot and on reload (`config.AgentSandboxGateWarnings`) when a job-runtime agent resolves no `workspace_claim` (unless `workspace_transport: copy`) or names a runtime hive does not have, and refuses the kick with the same message rather than failing it after the agent is marked running.

### Posting reviews from a sandbox

A reviewer can run in a sandbox too, so a review of an accelerator change can actually run it on the card node ([#14986](https://github.com/hivecommons/hive/issues/14986)). `sandbox.runtime: job` is a legal setting for a `reviewer`-base agent; the review runs on the node the Job lands on.

```yaml
agents:
  reviewer:
    sandbox:
      enabled: true
      runtime: job
      image: registry.example/project-dev:latest   # needs bash and python3; hive supplies its relays
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

#### Issue and topic requests through the spool

The spool is typed ([#18572](https://github.com/hivecommons/hive/issues/18572), [#18573](https://github.com/hivecommons/hive/issues/18573)). Besides reviews, it carries two more kinds of request, each run through the handler that serves it in-pod:

- **`issue`**: `hive-open-issue` (open, comment, claim, close, label, request-review). The request runs through the issue relay's own handler. The repository gate, the ACMM `CanCreateIssues` gate, the lane write allowlist, repo pause, repo scope, dedupe by exact title, consolidation, the canary scan and the `agent_issue_created` (or comment, claim, close, label) audit all apply unchanged, for the manifest's agent.
- **`topic`**: `hive-topic` (`claim`, `get`, `list`, `release`, the same arguments as `bd topic`). The request is applied to the manifest agent's own bead store, the one `bd topic` writes in-pod. A claim needs a repo the agent is scoped to. `hive-topic` waits for hive's reply and prints it, exiting 0 on success, 2 when the topic is held through another PR (or `get` finds none), and 1 on any other error or when no reply arrives in time (`--timeout`, default 120 seconds; `--no-wait` returns at once).

Hive sets the spool path and key under two more names, `HIVE_SANDBOX_REQUEST_DIR` and `HIVE_SANDBOX_SPOOL_KEY`, which `hive-open-issue` and `hive-topic` read first. Both fall back to the `HIVE_REVIEW_*` names, and both write to the pod-local drop box (or, for `hive-topic`, run `bd topic`) when no spool key is set. The envelope names its kind, `{"kind": "issue", "request": "...", "mac": "..."}`, and every kind but `review` MACs `"<kind>\n" + request`. So a request signed as one kind is refused as any other, and the replay guard covers all kinds. An envelope with no `kind` is a review MACed over the request alone, so `hive-review` is unchanged and an older copy still works. An unknown kind is set aside as `.rejected`.

**A topic claim for the PR hive opens.** A sandboxed kick has no PR number: hive opens the PR from the kick's branch after the sandbox exits. So the manifest also records the kick's repo and branch, and `hive-topic claim <key>` without `--pr` records a **pending** claim on that branch. On the next eval cycle the governor binds it to the PR whose head branch it is (an open PR wins over a closed one), and the claim then follows the PR like any other: hive releases it when the PR merges or closes. Until then the kick's topic block shows the claim as "PR not opened yet". A pending claim left by a kick that opened no PR moves to the next kick's branch when that kick claims the topic again. A review-only kick opens no PR, so it records no branch, and a claim from it needs `--pr`.

While any spool is registered, the watcher scans on its short poll interval: a Job writes its spool over a shared volume, often from another node, where no file-change notification reaches the hive pod. After the sandbox exits, the kick waits up to a minute for the relay to take any request still in the spool, because a run-stage worktree is removed when the kick returns. The manifest is dropped once the kick has ended and nothing is left to deliver, or after 24 hours.

The relay touches the spool only through an `os.Root` opened at the workspace. A symlink the sandbox plants can't redirect a read, rename or write outside its own workspace, and a request file that is a symlink, FIFO or oversized is set aside as `.rejected`. The relay ignores a manifest directory or manifest that is not owned by its own uid or is group- or world-writable, and a manifest without a spool key. In the sidecar runtime (`CRUX_MODE=sidecar`), the runtime and hive must therefore share both `/data` and a uid. On `sandbox.runtime: job`, the key is in the Job's pod spec, so anything that can read Jobs or Pods in the namespace through the Kubernetes API can read it; agents must not hold that permission. A group member can still delete or alter a request in the spool, which loses the review but cannot post one.

Out of scope: the quality and deps lanes are unchanged, and nothing inside a Job writes to GitHub directly.

### Hive's relays inside the sandbox

The project image does not need to carry `hive-review` or any other hive script ([#18512](https://github.com/hivecommons/hive/issues/18512)). On every sandboxed agent kick, hive supplies its **sandbox-safe relays** itself, read-only at `/opt/hive/relays`, and runs the kick command through a short `/bin/sh` preflight that prepends that directory to `PATH`. The relays are `hive-review`, `hive-open-issue` and `hive-topic`, which work from a sandbox through the spool above. Relays that need the pod-local drop boxes (`hive-open-pr`, `hive-merge`, `hive-push-branch`) are left out until they have a spool of their own.

- **Job runtime.** The Job gets an init container, `hive-relays`, from the **running hive image by digest**. Hive reads its own pod (`$POD_NAME`, else `$HOSTNAME`) through the `pods` get grant the launcher already has, and takes the hive container's `imageID`. When the runtime reports no pullable digest, hive falls back to the image as written. Hive's own `imagePullSecrets` are copied onto the Job so the node can pull it. The init container runs `cp /usr/local/bin/hive-review /usr/local/bin/hive-open-issue /usr/local/bin/hive-topic /opt/hive/relays/` into a 16Mi `emptyDir`, and the kick container mounts that `emptyDir` read-only. The init container passes OpenShift's restricted SCC and the Kubernetes `restricted` Pod Security Standard: `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, every capability dropped, `RuntimeDefault` seccomp. On OpenShift (hive's pod carries the `openshift.io/scc` annotation) it sets no uid, and the SCC assigns one from the namespace range. Elsewhere the hive image's default user is root, which `runAsNonRoot` refuses, so it runs as uid 65534. Two `job` fields override this:
  - `relay_image` pins the init container's image, for example a mirror on an internal registry. `none` turns the relays off.
  - `relay_run_as_user` pins its uid. It must be positive.

  The first kick on a node pulls the hive image, which is large. Later kicks reuse it (`imagePullPolicy: IfNotPresent`). If hive cannot read its own pod, the kick fails with a message that names both fields, and no Job is created.
- **Podman.** Hive copies its own relays (found on its `PATH`, or in `$HIVE_SANDBOX_RELAY_DIR` when that is set) into a private temporary directory. It bind-mounts that directory with `-v <dir>:/opt/hive/relays:ro,Z` and removes it after the run. If a relay cannot be found, the kick fails with a message that says so.
- **Preflight.** The relays need `bash` and `python3` in the image. If either is missing, the kick exits with code 78 before the agent starts, and its transcript reads `hive: this sandbox image has no python3 on PATH ... add python3 to the image.` The image also needs `/bin/sh`. A kick that relies on the image's own entrypoint (no command) cannot be wrapped: the relays are mounted but not put on `PATH`.
- **Precedence.** Hive's copy always comes first on `PATH`, and a copy in the image is ignored. The relay and the spool protocol it speaks ship together in the hive image, so an older or newer copy baked into a project image could sign or shape a request that this hive rejects. There is deliberately no "use the image's copy if newer" option.
- **OpenShell and Substrate.** Not yet. On these runtimes the gateway or the actor API builds the sandbox pod, and getting hive's image into it needs runtime-specific work: an init container in the OpenShell template, and Landlock read access to `/opt/hive/relays`. Until that follow-up lands, the image still has to include `hive-review` on these runtimes.

### What a sandboxed reviewer sees

The sandbox has no GitHub credential, so `gh pr view`, `gh pr diff` and `gh pr checkout` fail inside it. Hive does that work on the host before the sandbox starts ([#18513](https://github.com/hivecommons/hive/issues/18513)).

**Which kicks count as review kicks.** Hive reads the PR targets from the hive-written header of the kick message, never from text a PR author supplied:

- A review swarm kick opens with `[review-perspective:…]` followed by `Review PR <owner>/<repo>#<n>`. That gives one target.
- A reviewer-lane kick opens with `[agent:<name>]` followed by `REVIEWER — …`. Each escalated PR in its work list has a `    checkout: gh pr checkout <n> --repo <owner>/<repo>` line, and each of those lines is a target. The lane sends at most 3 PRs per kick. The sandbox takes at most 5.

Hive only checks out targets in the hive's primary repo, because that is the only repo the workspace clones. Any other kick has no targets and behaves as before.

**What workspace prep does for each target `N`:**

1. It reads the PR through hive's own GitHub client: title, body, author, state, base and head refs and SHAs, head repo (and whether the PR comes from a fork), labels, changed files (up to 3000), the unified diff, and every conversation comment, review, and inline review-thread comment. Comments are not filtered by author. Each one carries its author association.
2. It runs `git fetch origin +refs/pull/N/head:refs/heads/pr-N +refs/heads/<base>:refs/remotes/origin/<base>` with the same clone token it used for the clone. `refs/pull/N/head` exists on the base repo for fork PRs too, so fork PRs need no access to the fork.
3. It checks out the first target's `pr-N`. With several targets, the other branches are already fetched, and the reviewer switches between them with `git checkout pr-M`.
4. It writes `.hive/pr/N.json`. The file has `"untrusted": true` and a notice saying that every field except `repo`, `number`, `checkout` and the hive's notes came from the PR author or commenters. It also has a `checkout` block that hive wrote from git (`ref`, `branch`, `sha`, `base_tracking_ref`, `diff_command`). If the fetched head differs from the head GitHub reported, `checkout.head_moved` says so. The diff is capped at 512 KiB and the description at 64 KiB. A truncated or missing diff (GitHub refuses to render very large diffs) gets a `diff_note` that points at `checkout.diff_command`, which works offline because the base is fetched.
5. It prepends a `[hive:sandbox-review-checkout]` section to the kick prompt. The section names each checked-out branch, its head SHA, its PR file and the offline diff command, and it repeats the untrusted-data warning.

Hive writes the prompt and the PR files after the PR head is checked out, through an `os.Root` opened at the workspace. A `.hive` symlink in the PR's tree therefore cannot redirect a host write outside the workspace.

**Failures fail the kick.** If hive cannot fetch `refs/pull/N/head` or the base, or the GitHub client returns an error or a different PR, the kick fails before the sandbox starts. The error names the PR and includes git's output. A reviewer that cannot see the PR must not post a review of the wrong tree. If the host has no PR reader at all, the head is still checked out. In that case the PR file has `"metadata_available": false`, and the reviewer works from git alone.

**Review kicks never push.** The manager wires no push broker and no PR client for a kick that has targets, and the executor refuses to push one even if a caller wires them. Commits the reviewer leaves on `pr-N` are discarded, and the run record notes them as withheld. The review itself is posted through the review spool described above. A run-stage kick that carries review targets is refused.

## Running sandboxed kicks in an OpenShell sandbox

`sandbox.runtime: openshell` runs an agent's sandboxed kicks in an [NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell) sandbox instead of a Podman container or a plain Kubernetes Job ([ADR-0026](adr/0026-sandbox-runtime-selection.md), epic [#17642](https://github.com/hivecommons/hive/issues/17642)). OpenShell adds what the other two runtimes lack: the workload runs with no capabilities under Landlock and seccomp mediation, and **egress is default-deny** — every outbound connection goes through the gateway's proxy and is checked against a per-binary policy. Phase 1 is opt-in; nothing changes for a hive that does not select it.

Everything above about the kick path still holds. Hive prepares the workspace on its own data PVC, writes the prompt, and after the sandbox exits reads the commits back and hands them to the push broker and PR path. Only step 2 changes: the launcher is `sandbox.OpenShellLauncher`, which talks to an OpenShell **gateway** over gRPC (`github.com/ai-openshell/sdk-go` v0.1.8); the gateway owns the sandbox pod.

```yaml
agent_sandbox:
  enabled: true
  runtime: openshell
  openshell:
    gateway_url: https://openshell-gateway.openshell.svc:8443   # host:port or http(s)://
    namespace: hive                   # the OpenShell workspace (their term), default "default"
    token_env: OPENSHELL_TOKEN        # env var holding the gateway bearer token; never the token itself
    workspace_claim: hive-data        # the PVC hive's /data lives on
    workspace_claim_mount: /data      # where hive sees it (default)
    allow_hosts: [litellm.example.com, hub.hivecommons.dev]   # on top of github.com / api.github.com
    timeout_s: 1800                   # bounds one sandbox from create to delete
agents:
  deps:
    sandbox:
      enabled: true
      image: registry.example/project-dev@sha256:…   # digest-pinned
      openshell:
        node_selector: { kernel: "6.12" }
        tolerations:
          - { key: sandbox, operator: Exists, effect: NoSchedule }
        binaries: [/usr/bin/git, /usr/lib/git-core/git-remote-https, /usr/local/bin/node]
```

The gateway, workspace, token, TLS and claim fields are cluster facts and fall back from the per-agent block to the global one; `allow_hosts`, `binaries`, `node_selector`, `tolerations` and `runtime_class_name` are per-agent choices and replace the global values when set.

**Workspace transport is the shared claim again, not a copy.** The SDK's file transfer is single-file and not available on every gateway, so the OpenShell runtime mounts `workspace_claim` at a `sub_path` pointing at the per-kick workspace, at the same `/workspace` mount the other launchers use, through the gateway's Kubernetes driver config. That needs two things on the cluster side: the gateway setting `allow_driver_config = true`, and the claim labelled `openshell.ai/sandbox-attachable: "true"` and `openshell.ai/sandbox-attachable-workspace: "<namespace>"`. The same ReadWriteMany caveat as the Job runtime applies when the sandbox lands on another node, and `agent_sandbox.workspace_dir` must live under `workspace_claim_mount`.

**The egress policy.** Hive derives an OpenShell network policy from the hosts its own proxy brokers for a Podman sandbox: `github.com` read-write (git's fetch-pack is a `POST` even when it only reads) and `api.github.com` **read-only** — a sandbox that somehow obtained a token still could not mutate through the API — plus whatever `allow_hosts` names, as `host` (443) or `host:port`. URLs, userinfo, wildcards and anything but a literal host name are rejected, and a unit-level prover renders the policy for the shipped examples to keep it that way. Everything else is unreachable, including a raw TCP connect to an arbitrary IP. OpenShell matches rules per **real binary path** and allows nothing for an unlisted binary, so the policy is attached to a default set (`git`, `git-remote-https`, `curl`, `node`, `python3`, `crux` at their usual paths) that `binaries` overrides when an image puts them elsewhere; a denied connection shows the path OpenShell identified in the sandbox log. `network_mode: none` drops every rule. `policy_profile` is recorded on the sandbox as the annotation `hive.dev/policy-profile` for gateway admission to act on; the SDK has no create-time profile selection.

**The credential boundary.** Same rule as the Job runtime: environment variables from the allowlist are copied in with the same credential-name filter, hive's brokered GitHub token never enters the sandbox, and hive pushes and opens the PR from the returned workspace. There is **no** Secret-reference path into the sandbox in phase 1, so model credentials have to arrive through an allowlisted non-credential variable pointing at a routable gateway named in `allow_hosts`. The Cline backend, which needs a provider file or Secret, is refused on this runtime.

**Lifecycle.** The sandbox is named `hk-<agent>-<unix, base36>` (at most 19 characters, the gateway's limit for DNS-routable names) and labelled `app.kubernetes.io/managed-by=hive-openshell`. Hive waits for it to become ready, runs the kick command with the sanitized environment in `/workspace`, collects stdout, stderr and the exit code (capped at 16 MiB), and deletes the sandbox — on success, on failure, and on the kick timeout, in which case the kick ends with exit code `-1` and `context deadline exceeded`, exactly like a timed-out Job. Cleanup runs on a fresh, bounded context when the kick's own has expired, so a finished sandbox is not left behind because the deadline fired during log collection.

**What hive checks, and what it cannot.** Hive warns at boot and on reload (`config.AgentSandboxGateWarnings`) when an openshell-runtime agent resolves no `gateway_url` or no `workspace_claim`, and refuses the kick with the same message before the agent is marked running. It also preflights each distinct gateway (`Health().Check` and `Config().GetGateway`, read-only, cached for five minutes, bounded to 30 s) and adds a warning when the gateway is unreachable or unhealthy, `allow_driver_config` is not enabled, or telemetry is on. It cannot see the node kernel, the CNI or the claim's labels from inside its pod; those remain the operator prerequisites in [deploy/openshell.md](../deploy/openshell.md).

**The terminal pane.** An openshell-runtime agent has no tmux session either, so its ▶ terminal opens the same read-only `/terminal/job/<agent>` viewer as a job-runtime agent, behind the same handoff and role check ([#17645](https://github.com/hivecommons/hive/issues/17645)). While a kick runs it follows the newest live `hk-<agent>-<unix, base36>` sandbox's gateway log (`GetLogs`, polled at up to the gateway's 2,000-line buffer per read, scrubbed of token-shaped values like the pod log); between kicks, once the launcher has deleted the sandbox, or when the gateway cannot be listed, it shows the last persisted transcript exactly as the job viewer does. The live view is weaker than the job pod log, and the pane says so: the gateway log carries the sandbox's supervisor, policy (OCSF) and tracing events — the network and filesystem decisions made around the kick — not the kick's stdout, because the kick runs as an exec session and [exec output reaches neither the gateway log nor the container log](https://docs.nvidia.com/openshell/observability/accessing-logs). Likewise the exit code the gateway reports on the sandbox is its main process's, not the kick's; the launcher takes the kick's exit code from the exec result and deletes the sandbox at once. So the pane ends by saying the kick has finished and that its output and exit code are in the transcript, which it shows on the next open. The dashboard reaches the gateway with the agent's own `gateway_url`, `namespace`, `token_env` and `insecure_tls`.

**How to verify.** With a gateway reachable and the claim attachable, run the env-gated integration test from a pod that has the claim mounted: `OPENSHELL_GATEWAY_URL=… OPENSHELL_WORKSPACE_CLAIM=hive-data OPENSHELL_IMAGE=<digest-pinned> go test ./crux/sandbox -run TestOpenShellIntegration -v`. It writes a file through `/workspace`, reads it back on the hive side, and logs the round-trip latency. Then run a kick and confirm in the sandbox log that a `POST` to `api.github.com` is denied while `git fetch` succeeds — that denial is the whole point of the runtime.

**Limitations (phase 1).** Kubernetes driver only (no Podman-driver path for standalone hosts yet); no Secret references into the sandbox; `policy_profile` is advisory; kernel and claim-label prerequisites are not preflighted; latency and image size are not yet measured ([#17643](https://github.com/hivecommons/hive/issues/17643)).

## Running sandboxed kicks in a Substrate actor

`sandbox.runtime: substrate` runs an agent's sandboxed kicks as an [Agent Substrate](https://github.com/agent-substrate/substrate) **actor** ([ADR-0026 phase 2](adr/0026-sandbox-runtime-selection.md#implementation-notes-phase-2-agent-substrate), epic [#17695](https://github.com/hivecommons/hive/issues/17695)). Substrate is a CNCF Sandbox project donated by Google: a `WorkerPool` of pods runs workloads under gVisor (`runsc`) or in a KVM microVM, every actor gets a SPIFFE identity, and all egress leaves through `atenet-router` under an `EgressPolicy`. It is the fourth runtime and, like OpenShell, opt-in; nothing changes for a hive that does not select it.

Everything above about the kick path still holds; only step 2 changes. The launcher is `sandbox.SubstrateLauncher`, which talks to Substrate's gRPC **Control API** (`ate.control.v1`) with a trimmed, wire-compatible client generated in-repo (`crux/sandbox/substrateapi`; the upstream Go module needs a newer Go than hive builds with).

```yaml
agent_sandbox:
  runtime: substrate
  substrate:
    endpoint: api.ate-system.svc:443       # Control API, host:port or https://
    atespace: hive                          # Substrate's tenancy unit, default "default"
    token_env: ATE_TOKEN                    # bearer token env var (or token_file for a projected SA token)
    ca_file: /var/run/ate/ca.crt            # Control API CA; insecure_tls: true only for a lab
    router_url: https://atenet-router.ate-system.svc:8444
    router_client_bundle: /var/run/ate/credential-bundle.pem   # hive's own mTLS client identity
    router_ca_file: /var/run/ate/router-ca.crt
    image: ghcr.io/hivecommons/crux-sandbox@sha256:…   # digest-pinned; tags are refused
    cpu_millicores: 2000
    memory_bytes: 4294967296
    worker_selector: {ate.dev/sandboxClass: gvisor}   # pick the pool (gvisor | microvm)
    snapshot_on_idle: false
    allow_hosts: [models.example.com, hub.example.com:8443]
    timeout_s: 1800
agents:
  reviewer:
    sandbox:
      substrate:
        snapshot_on_idle: true   # per-agent overrides merge onto the global block
```

**One actor per kick.** Hive creates an `ActorTemplate` (one container running the bridge command, the workspace as a `durableDir`, the operator's limits and `worker_selector`) and an `Actor` from it, both named `hive-kick-<agent>-<unix>`, attaches the egress policy, waits for the actor to report `RUNNING`, and then talks to a small HTTP **bridge** inside the actor (`crux substrate-bridge`, overridable with `bridge_command`, port `bridge_port`) through the router's mTLS `CONNECT` tunnel: it pushes the workspace as a tar, runs the kick with the sanitized environment, and pulls the workspace back. The hive-side workspace remains the source of truth and feeds the push broker exactly as for Podman. Substrate has no exec API and hive shares no PVC with the worker nodes, which is why the workspace is copied rather than mounted; the copy is bounded by the kick timeout.

**The egress policy.** Hive derives an `atenet-egress` policy from the hosts its proxy brokers for a Podman sandbox — `github.com` and `api.github.com` on 443 — plus `allow_hosts` as `host` or `host:port`, all as TLS passthrough; URLs, userinfo and wildcards are rejected at validation, before any actor is created. Everything else, including all UDP and raw IP connects, is denied. Substrate's policy language has no per-method read-only mode, so `api.github.com` is reachable; the actor holds no token (`SanitizedEnv`) and hive performs every GitHub write itself after the kick. `network_mode: none` drops every rule. Actors run credential-free: there is no `HTTPS_PROXY` or MITM broker inside the actor and no credential enters it, and hive brokers pushes after the kick from the returned workspace (decision on [#17695](https://github.com/hivecommons/hive/issues/17695), [ADR-0026](adr/0026-sandbox-runtime-selection.md)). Substrate's own credential injection is not used; SVID-authenticated broker delegation over the atenet mTLS channel is a future phase 3.

**Lifecycle.** Success, failure and the kick timeout all delete the actor and then its template, on a fresh bounded context when the kick's own has expired, with exit code `-1` and `context deadline exceeded` on timeout, like a timed-out Job. With `snapshot_on_idle: true` the actor is **suspended** (snapshotted to Substrate's object storage) instead of deleted; the template is still deleted and the snapshot is not yet resumed on the next kick ([#17697](https://github.com/hivecommons/hive/issues/17697)). At most one suspended actor is retained per agent per hive process: the previous one is deleted after the next successful suspend (skipped with a warning if the agent's `atespace` changed in between), and actors left by an earlier hive process are not reclaimed because the Control API has no `ListActors`. `policy_profile` is passed to the actor as `HIVE_POLICY_PROFILE` for in-actor tooling; Substrate has no annotations.

**What hive checks, and what it cannot.** At boot and on reload (`config.AgentSandboxGateWarnings`) a substrate-runtime agent is warned about, and its kick refused, when `endpoint` or `router_url` is missing or `image` is not digest-pinned. Hive also preflights each distinct Control API (read-only `ListWorkers`, cached for five minutes, bounded to 30 s) and warns when it is unreachable, has no `ACTIVE` worker matching `worker_selector`, or its workers advertise no sandbox class. It cannot see the CRDs, the operator, the pod-certificate feature gates or `/dev/kvm` from inside its pod; those are the operator prerequisites in [deploy/substrate.md](../deploy/substrate.md) and the prover in [#17698](https://github.com/hivecommons/hive/issues/17698).

**The terminal pane.** A substrate-runtime agent has no tmux session either, so its ▶ terminal opens the same read-only `/terminal/job/<agent>` viewer behind the same handoff and role check ([#17777](https://github.com/hivecommons/hive/issues/17777)). The Control API has no `ListActors` and Substrate has no logs RPC, so the launcher — which runs in the same hive process as the dashboard — records each kick for the pane: the actor's `<atespace>/<name>`, every state it is seen in (`RESUMING`, `RUNNING`, then `SUSPENDED` with `snapshot_on_idle` or `DELETING`), and the worker's SPIFFE id from the actor's worker assignment. The bridge's `POST /run` streams the kick's stdout and stderr when asked (`"stream": true`, newline-delimited `RunResponse` frames; an older bridge answers with one complete response and still works), and the pane follows that output live, scrubbed of token-shaped values like the pod log, with each actor state change as a status line. Unlike OpenShell this is the kick's own output, and the pane ends with the kick's exit code and whether the actor was suspended or deleted. Between kicks it names how the last actor ended above the last persisted transcript; after a hive restart, before the agent's next kick, it shows the transcript alone.

**Limitations (phase 2).** Workspace copied, not shared; `crux substrate-bridge` entrypoint and a published sandbox image land with the prototype ([#17697](https://github.com/hivecommons/hive/issues/17697)); no resume of suspended actors; `api.github.com` not read-only at the policy layer; latency and image size not yet measured.

## Health and metrics for sandboxed agents

A sandboxed agent (Podman or Job runtime) has no tmux session, no in-pod process and no `pluk` log between kicks: the manager parks it in the `idle` state and each kick runs to completion in a container or Job. The dashboard therefore must not read "not running" as "session down" for these agents, and the token collector cannot find their CLI session files because the CLI's home directory lives in the sandbox, not in the hive pod.

**Last-run record.** When a sandboxed kick ends, `crux/agent` builds a `SandboxRun` (runtime, start/end, succeeded, timed out, error, commit count, branch, PR URL, `prWithheld` when the publish gate kept commits local, `logId` of the archived kick log, summary, usage) and keeps it on the agent process as `sandboxLastRun`. It is also written to `<work_dir>/sandbox-runs/<agent>.json` (restored on restart, so a pod roll does not blank the card) and appended to `<work_dir>/sandbox-runs/runs.jsonl`, which is compacted to its newest 1000 records once it passes 2 MiB (`work_dir` is `HIVE_WORK_DIR`, default `/data/agents`). The summary is the agent's `.hive/agent-report.json` `summary` when it left one, otherwise the scrubbed tail of the transcript.

**Health.** `/api/status` carries a `sandboxRun` object on every sandboxed agent with a `health` verdict the SPA uses instead of the session check:

| `health`  | Meaning | Dot |
|-----------|---------|-----|
| `ok`      | last run succeeded (or no run yet) and the next cadence occurrence is still in the future | green |
| `running` | a kick is in flight (container or Job running) | amber |
| `waiting_for_node` | a kick is in flight but its Job's pod is Pending and Unschedulable; `waitDetail` has the scheduler's message | amber |
| `node_unhealthy` | the last kick never reached the agent: the node `preflight` or a setup health check failed, also on the retry; `failReason` and `node` say why and where | red |
| `failed`  | the last run failed or timed out, see the agent's log; `failReason: setup_failed` when a `setup` command failed before the agent ran | red |
| `overdue` | the cadence had a due occurrence that never started, older than the grace (half the period, clamped to 15 min – 2 h); `dueAt` names it | red |

On-demand agents, paused agents and `pause`/`continuous` cadences are never overdue. A never-kicked interval agent counts its first period from the time the agent came up. The card's blocker line reads `Job ok` / `Job running` / `Job ready` (or `sandbox …` for Podman) / `last run failed` / `last run timed out` / `run overdue` for sandboxed agents instead of `session down`, and the "Now:" line shows the last run's outcome, duration, PR link and summary.

**Tokens and sessions.** The collector reads `sandbox-runs/runs.jsonl` and turns every run that reported usage into one session (`sandbox-<agent>-<startMs>`, backend `sandbox`), so TOKENS/SESSIONS, by-agent and by-model totals and cost include sandboxed work. Usage is parsed from the transcript: Claude's `--output-format json`/`stream-json` `result` line and Copilot's "Breakdown by AI model" footer are recognised. A backend that prints plain text only (Claude without `--output-format`) reports no usage; such runs stay healthy but contribute no tokens rather than an estimate.

## Network trade-off

`network_mode: none` remains available and maps to Podman's `--network=none`, but it only works for non-inference jobs or runtimes that already expose a local/socket model proxy inside the container. The default sandbox network mode is `restricted`: operators must provide a Podman network/proxy policy that allows only the inference endpoint and MITM proxy required by the selected backend. This is a compromise until every supported backend can run through a credential-free local socket without general egress.

## Remaining gaps

- The default target is the hive primary repo and default base ref; richer per-kick repo/ref selection is still future work.
- The Job runtime is a first slice: one Job per kick, workspace shared through a PVC, no per-kick Secret minting on the claim transport. Clusters without ReadWriteMany storage can use `workspace_transport: copy` (#19621), which has no relays yet.
- Live Podman execution is covered by skip-when-absent tests; CI still needs a rootless-Podman runner lane for always-on integration coverage.
- Sandboxed inference depends on an operator-provided restricted network/proxy policy. `none` is stronger but not yet usable for all model backends.
- Stronger runtimes than plain containers ship opt-in: [ADR-0026](adr/0026-sandbox-runtime-selection.md) (Accepted) covers NVIDIA OpenShell (phase 1) and Agent Substrate (phase 2); both still need their prototype measurements and cluster preflights.

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

## Containment posture (#19457, #19458)

Podman sandbox runs always pass `--cap-drop=all`, `--security-opt=no-new-privileges` and `--pids-limit` (default 4096, which is above podman's own 2048 so dependency-heavy builds and test runs fit; set `agent_sandbox.pids_limit` to change it for every run, publish.verify included).

Hive checks `/proc/self/status` (`NoNewPrivs`, `CapEff`, `Seccomp`) and the root filesystem at boot (a root filesystem `statfs` cannot inspect is a gap too), and the sandbox entry point (the relay preflight) checks the same status fields inside the sandbox. A status file either check cannot read counts as a gap, so strict mode fails closed when containment cannot be verified. Every gap is logged as a warning. Set `HIVE_REQUIRE_HARDENED=1` to make any gap fatal. Hive itself then refuses to start, and a Podman agent kick that goes through the relay preflight exits with status 78. Strict mode is not enforced everywhere:

- Kubernetes job pods (kubejob) drop all capabilities, disallow privilege escalation and use the `RuntimeDefault` seccomp profile. Hive sets `HIVE_REQUIRE_HARDENED=1` in the pod when it runs in strict mode, so the relay preflight there fails the kick on a gap, like a Podman kick.
- OpenShell runs and relay-less Podman runs, such as `publish.verify`, get the variable (Podman) or nothing (OpenShell) but never run the preflight, so a gap does not fail them.

Strict mode for hive itself needs `--read-only` and an empty effective capability set, so no ambient `NET_ADMIN` or any other capability. A read-only root filesystem is only checked for hive itself, because `--read-only` is optional for sandboxes.
