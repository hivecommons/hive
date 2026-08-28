# Agent host confinement on the default launch path (#4918)

Status: **historical investigation; contributor-local confinement is now
implemented for every backend that has a real mechanism, and the rest refuse
to launch unconfined.** This page records the evidence and options as
assessed before that work landed (#5011, then this follow-up). Claude/LiteLLM
and Codex local launches use their own OS-enforced sandboxes with hard-fail
startup and no unsandboxed retry; Copilot local launches now use Copilot
CLI's own `--sandbox` (OS-enforced, same underlying technology class); opencode
gets a command-name deny-list (a floor, not a boundary — it has no OS sandbox);
goose, agy, bob, pi, and aider have **no confinement mechanism this repo can
wire at all**, verified against each CLI's own current docs, and local mode
for them now refuses to launch without an explicit per-backend operator
opt-in rather than launching silently unconfined. See
`src/docs/sandbox-isolation.md`'s per-backend confinement matrix for the
current, authoritative state — the analysis below is left as the historical
record of how each decision was reached, not a live status report. The
hub-side Podman sandbox's double gate (`agent_sandbox.enabled` +
per-agent `sandbox.enabled`) remains an open concern, unchanged by this pass.

All citations are against `origin/v4` at `1b54c69e` unless noted.

## The reported incident, in one line

An agent doing entirely correct work on an assigned third-party repo
(`projectbluefin/bluefin`) ran that repo's own `bats` unit suite; a latent test
defect let a stubbed `rpm-ostree` hook escape to the real binary, which issued
`rpm-ostree kargs --append-if-missing=usbcore.autosuspend=-1` against the
operator's live boot deployment. Three interactive polkit dialogs fired on the
operator's desktop. Nothing was written — polkit declined the unprivileged
caller — but the incident needed no compromise: legitimate repo, correct agent,
the command came from that project's own tests (issue #4918 body).

## What the guarantee is claimed to be

`src/docs/sandbox-isolation.md` was edited by #4938 to state the guarantee
precisely (`src/docs/sandbox-isolation.md:9-13`):

- **Holds on every path**: no agent receives a GitHub token or pushes directly;
  authorship goes through the App-gated `gh` wrapper and the push broker.
- **Holds only on the opt-in Podman sandbox path**: "it can only modify its
  mounted workspace." On the default tmux path there is no container — the
  backend CLI runs as the operator's own user, on the operator's own host, with
  nothing scoping its filesystem access to the workspace
  (`src/docs/sandbox-isolation.md:12-13`).

This is no longer an overstatement in the current doc — #4938 already rewrote
this section to draw exactly this line and cite #4918 by name
(`src/docs/sandbox-isolation.md:9-15`). The doc-correction half of the original
ask is done; see [Doc status](#doc-status-already-corrected-by-4938) below.

## What the launch path actually does

### Default tmux path (both relay and hub-pod agents)

The claude-family permission flags are `--dangerously-skip-permissions
--permission-mode bypassPermissions` unconditionally
(`config/backends.conf:69`, mirrored at `src/pkg/agent/manager.go:2563`, base
string `--model %s --dangerously-skip-permissions%s`). Full tool-permission
bypass is not gated by anything added in #4938 — it is the unconditional base.

Workspace "confinement" is a `cd`, not a boundary:

- Relay (`contribute-hive ... local`): `HIVE_WORKSPACE_DIR` is exported and the
  tmux pane is started with `-c "$HIVE_WORKSPACE_DIR"`
  (`bin/contributor-agent.sh:463-472`). That sets the pane's initial working
  directory only; nothing stops the CLI process — running as the contributor's
  own uid — from reading or writing any path that uid can reach.
- Hub-pod agents: `launchInTmux` starts the session with `new-session -d -s
  session -c dir` (`src/pkg/agent/manager.go:1964`), then, when the agent has a
  UID assigned, runs the tmux command through `su-exec`/an exec-user spec
  (`agentExecUserSpec`, `src/pkg/agent/manager.go:1976-1984`,
  `src/pkg/agent/manager.go:2005-2010`) so the CLI runs as `hive-<agent>` or a
  synthetic `uid:gid`, not as the same uid as every other agent. This is
  **process-identity separation between agents on the same pod**, not
  filesystem confinement to the workspace — there is no mount namespace, no
  chroot, no bind mount restricting that uid's view of the pod's filesystem.
  The pod itself is the confinement boundary here, and its blast radius is
  bounded by whatever the pod's own `securityContext` and volumes expose
  (`src/deploy/k8s/deployment.yaml:34-79` sets pod-level security context; not
  audited further here — out of scope for this pass).

So "the operator's host" means two different things depending on deployment
mode, and the blast radius differs by orders of magnitude between them — see
[Deployment modes](#deployment-modes) below. The reported incident is the
`contribute-hive ... local` / native-relay shape: a real uid, on a real
desktop, with a real bootloader.

## What #4938 covers, precisely

PR #4938 (merged `6ceb3e2e`) adds a **command denylist**, not confinement. Two
families, applied via Claude Code's `--disallowed-tools 'Bash(<cmd>:*)'`
mechanism:

```
Bash(sudo:*), Bash(pkexec:*), Bash(doas:*), Bash(su:*),
Bash(rpm-ostree:*), Bash(bootc:*), Bash(ostree:*),
Bash(grubby:*), Bash(bootctl:*), Bash(efibootmgr:*)
```

(`src/pkg/agent/manager.go:7282-7288`, shell mirror at
`config/backends.conf:63`, parity enforced by
`src/pkg/agent/host_state_deny_test.go:82-95` `TestShellAndGoDenyListsAgree`,
which sources `config/backends.conf` and diffs the two lists byte-for-byte).

Applied on both launch paths: relay (`config/backends.conf:69-77`
`claude_family_perm_flag`) and hub-pod (`src/pkg/agent/manager.go:2567`, `base +
claudeGitHubWriteDenyFlags + claudeHostStateDenyFlags()`). An escape hatch,
`HIVE_CLAUDE_DANGEROUSLY_ALLOW_HOST_STATE`, drops the deny fragment entirely on
either path when set truthy (`src/pkg/agent/manager.go:7304-7309`,
`config/backends.conf:71-76`).

The PR body documents that this was verified against a real CLI (Claude Code
2.1.231) before merge: a deny does survive
`--dangerously-skip-permissions --permission-mode bypassPermissions`, and
matching is command-token-aware so `Bash(su:*)` does not also catch `subl`
(PR #4938 description, "Verified against the real CLI" table). That
verification is real and narrows one uncertainty — the deny mechanism itself
is not a no-op under bypass mode.

### What this does not cover

**This is a denylist keyed on how Claude Code's own `Bash` tool parses a
command's leading token, not a boundary enforced by the OS.** It has the shape
of every denylist: it stops the literal named binaries when invoked as a bare
command word through the `Bash` tool, and nothing establishes that it stops
equivalent effects reached another way. None of the following are covered by
anything in `src/pkg/agent/manager.go:7264-7320` or
`config/backends.conf:41-77`, and no test in
`src/pkg/agent/host_state_deny_test.go` exercises them:

- **Absolute or relative path invocation.** `Bash(rpm-ostree:*)` matches the
  leading token `rpm-ostree`; whether Claude Code's matcher also catches
  `/usr/bin/rpm-ostree` or `./rpm-ostree` was not verified in the PR's own
  verification table (which tested the bare command only) and is not asserted
  by any test in this repo. This is the shape the issue names explicitly: "a
  test suite invoking the binary by absolute path."
- **A wrapper or indirection.** A shell function, alias, `env rpm-ostree`,
  `bash -c "rpm-ostree ..."`, `xargs rpm-ostree`, or a Makefile/test-harness
  target that execs the real binary several process-creation steps away from
  the literal `Bash(rpm-ostree:*)` token the CLI parsed. The original incident
  was itself an indirection — the command came from inside a `bats` test
  helper, not typed by the agent — and the fix works only because that
  particular indirection still surfaced as a `Bash` tool call whose argv began
  with the literal token.
- **Any other polkit-reachable action not on the list.** The two families
  (escalation binaries, boot/deployment binaries) are the ones the reported
  incident implicated. Nothing establishes they are exhaustive over
  `org.freedesktop.*` / `org.projectatomic.*` polkit actions generally —
  `systemctl` unit operations, `NetworkManager` reconfiguration,
  `udisks2`/`loginctl` calls, package-manager operations that write outside the
  workspace, or a future host-management CLI are all unlisted and unconfined.
- **Any tool surface other than `Bash`.** `--disallowed-tools` entries here are
  all `Bash(cmd:*)` patterns. Filesystem-writing or process-spawning tool calls
  through other registered tools (MCP servers, `Task`/subagent tool calls that
  themselves invoke `Bash`) are a different matching surface and are not
  addressed by this list; whether they inherit the same deny is unverified —
  see [Open questions](#open-questions).
- **The rest of the filesystem.** The denylist stops named host-state
  *commands*; it does nothing about direct file writes to any path the
  operator's uid can reach — `~/.ssh`, shell rc files, cron, systemd user
  units, other repositories' working trees, credentials for other tools. An
  agent (or, again, a test suite it runs) writing there is entirely unaddressed
  by #4938 because it never shells out to a denied binary at all.

The PR's own body is explicit that this is the intended scope, not an
oversight: *"This is a floor, not a sandbox — it names commands, it does not
enforce a boundary"* and *"the broader sandbox-by-default question #4918 also
raises is deliberately left for its own PR"* (PR #4938 description). That
matches this investigation's finding: #4938 raises the cost of the **specific
reported command family** and does not bound the general problem the issue
title names.

## Deployment modes

The exposure is not uniform. Three shapes exist in this repo today:

1. **Hub-hosted pod agents (containerized hive deployment).** `src/pkg/agent/manager.go`
   is the manager for this mode; the K8s manifest at `src/deploy/k8s/deployment.yaml`
   defines the pod's own security context. Agents on this path get per-agent
   uid separation via `agentExecUserSpec`/`su-exec`
   (`src/pkg/agent/manager.go:1976-1984`) but no filesystem namespace scoping —
   "the host" an agent can reach here is the container's filesystem view,
   bounded by whatever the pod's volumes and security context expose. This is
   the mode where "the operator's host" is a fungible pod, not a workstation;
   blast radius is real (container/node escape, credential exposure inside the
   pod, other agents' data) but categorically smaller than a desktop with a
   bootloader.
2. **`contribute-hive` containerized (default, recommended path).** `just
   contribute-hive` runs the relay and CLI inside `src/Dockerfile.contributor`,
   which drops to `USER dev` (`src/Dockerfile.contributor:191`) — a container
   boundary applies regardless of the `--disallowed-tools` denylist. This
   mode's actual isolation properties (capability set, seccomp profile, whether
   the container has host device or network access) were not audited further
   in this pass — see [Open questions](#open-questions).
3. **`contribute-hive ... local` (host mode) and any native/systemd hive
   install.** `just contribute-hive claude local` runs "relay + CLI directly on
   your machine, in a tmux session" (`src/docs/contributor-relay.md:45`). This
   is the mode the reported incident occurred in: no container, no distinct
   uid from the contributor's own login, direct access to whatever that uid
   can reach — including, as reported, polkit-gated boot configuration. This is
   also the mode `src/docs/contributor-relay.md` explicitly documents as
   running "on the contributor's own machine"
   (`src/docs/contributor-relay.md:117`).

Mode 3 is where the reported incident happened and where the blast radius is
largest relative to what's protected: a contributor's single daily-driver
machine, no container boundary, no per-agent uid, agent behavior driven by
whatever the assigned (often third-party, unvetted) repo's own tooling does.
Mode 1 has a real but categorically smaller blast radius bounded by the pod.
Mode 2 was not characterized far enough in this investigation to state its
boundary with confidence.

## The opt-in Podman sandbox, and why it's off by default

`src/pkg/sandbox/sandbox.go` and `src/pkg/agent/sandbox_executor.go` implement
a working rootless-Podman path: `PodmanArgs` builds `run --rm --userns=keep-id
-v <workspace>:<mount>:Z --network=<mode>` with an explicit env allowlist that
filters credential-shaped names (`src/pkg/sandbox/sandbox.go:80-116`,
`isCredentialName` at `:150-152`). This is real workspace confinement: the
container only has the one bind-mounted workspace directory, not the host
filesystem.

It is double-gated off: `AgentConfig.SandboxEnabled` requires both the global
`agent_sandbox.enabled` bool and a per-agent `sandbox.enabled` pointer to be
true (`src/pkg/config/config.go:1027-1033`); both default to their zero value
(`false`/`nil`), so an agent is on the sandbox path only if an operator finds
and sets both knobs. `sandbox-isolation.md` confirms this is deliberate:
"Sandbox execution is opt-in and the tmux path remains unchanged for all
agents unless both gates are set" (`src/docs/sandbox-isolation.md:19`).

The sandbox path also does not eliminate the network/inference trade-off:
`network_mode: none` only works for non-inference jobs or runtimes exposing a
local socket proxy; the default `restricted` mode still requires an
operator-provided network/proxy policy for the backend's inference endpoint
(`src/docs/sandbox-isolation.md:41-43`).

## Options, with honest costs

None of these were implemented or prototyped in this investigation. Costs are
estimated from reading the code, not measured.

### A. Reduce blast radius now (cheap, partial)

- **Extend the denylist's coverage of the `Bash` tool surface.** Verify (not
  assumed) whether Claude Code's `Bash(cmd:*)` pattern matches absolute-path
  invocation and shell-function/alias indirection, and either confirm coverage
  or add patterns/preprocessing that closes the gap. Cost: small, same
  mechanism as #4938, same "floor not boundary" ceiling — still a list of
  named badness, still bypassable by anything not on the list or not reached
  via `Bash`.
- **Warn operators loudly at launch and in `contribute-hive` output** that the
  default posture is unconfined, mirroring the `HIVE_CODEX_*` naming
  convention already established. Cost: near zero; does not reduce risk, only
  ensures informed consent. The issue's own suggestion 3 ("at minimum, correct
  the docs and warn at launch") — the docs half is done by #4938; a
  launch-time runtime warning is not implemented anywhere found in this pass.
- **Default the Podman sandbox on for non-primary (third-party) repos**, per
  the issue's suggestion 2's strongest form. Cost: moderate code change (a new
  default-routing rule in the kick path) but very large operational
  blast-radius change for existing fleets — flips agents onto a path that
  requires podman and a built sandbox image to be present, which is not true
  of every existing deployment. `sandbox-isolation.md`'s own "Remaining gaps"
  section already flags that live Podman execution is covered only by
  skip-when-absent tests, with no always-on integration lane yet
  (`src/docs/sandbox-isolation.md:47`) — meaning the sandbox path's own
  reliability under continuous operation is not yet proven at the confidence
  level you'd want before making it the default.

### B. Actually confine (expensive, correct)

- **Single-gate or default-on the existing Podman sandbox** (drop one of the
  two `SandboxEnabled` gates, or make it default-true with an explicit opt-out
  for operators who cannot run podman). Blocks essentially everything outside
  the mounted workspace — the strongest option already built. Breaks: any
  agent workflow needing host package managers, host git config/credential
  helpers not already proxied, or genuinely needing broader filesystem access
  (e.g., a spoke agent whose job is to manage the host itself, which
  `contributor-relay.md`'s escape-hatch language already anticipates as a
  legitimate case). Operator cost: requires podman + a maintained sandbox image
  on every host running agents, including every `contribute-hive` contributor
  machine — a meaningfully higher bar for the "clone the repo and run one
  command" onboarding story `contribute-hive` currently offers.
- **`bwrap` (bubblewrap) or `systemd-run` with `ProtectSystem=strict`,
  `PrivateDevices=yes`, `ProtectHome=`, `NoNewPrivileges=yes`, etc., around the
  CLI process itself**, without requiring a full container image. Cheaper to
  adopt than Podman for the host-mode/native-relay case (no image build/pull),
  and `systemd-run` in particular composes naturally with a systemd-managed
  relay. Blocks the same filesystem-escape shape the Podman sandbox blocks, if
  configured correspondingly (read-only `/`, workspace as the one writable
  bind). Breaks the same things a container would (git credential helpers,
  network tooling, package managers a test suite might invoke) unless
  explicitly allowed through, and `NoNewPrivileges` specifically would prevent
  the polkit-authenticate-and-succeed variant of this incident (an operator
  who clicks "Allow" on the dialog) as well as the denied-by-privilege variant
  actually observed. Not currently used anywhere in this repo — greenfield
  work, not a config flip.
- **Rootless user namespaces without a full container runtime** (raw
  `unshare`/`clone(CLONE_NEWUSER|CLONE_NEWNS|...)`) — lowest external
  dependency (no podman/bwrap binary required) but the most implementation and
  maintenance burden: hive would own namespace setup, mount propagation, and
  the corresponding failure modes directly instead of delegating to a
  maintained tool. Likely not worth it given bwrap and Podman already exist
  as options with far less code to own.
- **Seccomp profile** restricting syscalls (in particular anything reaching
  polkit's D-Bus interface, or broader host-management syscalls). Narrower
  than filesystem confinement — stops a specific class of host interaction
  without necessarily restricting file writes elsewhere — and is normally
  layered with, not instead of, a namespace/container boundary; the existing
  Podman path does not currently set an explicit seccomp profile beyond
  Podman's own default (`src/pkg/sandbox/sandbox.go:80-116` sets no
  `--security-opt seccomp=`). Cost: profile authorship and maintenance as agent
  tool use evolves; a too-strict profile silently breaks legitimate git/build
  tooling in ways that are hard to diagnose from an agent transcript.
- **Dedicated low-privilege UID with no polkit/sudo rights, for the
  `contribute-hive local` / native case specifically.** Hive already has a
  precedent for per-agent UID separation on the hub-pod path
  (`src/pkg/agent/uidmap.go`, `agentExecUserSpec` at
  `src/pkg/agent/manager.go:1976-1984`), but that map is scoped to hub-pod
  agents inside an already-containerized deployment (`UIDMapPath =
  "/var/run/hive/uid-map.json"`, `src/pkg/agent/uidmap.go:17`) — nothing
  currently wires an equivalent mechanism into `contribute-hive local`, which
  is the mode that actually reached the operator's bootloader. Applying the
  same idea there (a `hive-agent` system user with no polkit rule granting it
  the boot-config action, no sudoers entry) is a real partial answer:
  polkit-gated actions specifically would fail the same way the reported
  incident did, by design rather than by accident of privilege — but this is
  policy configuration on the **contributor's own machine**, which hive cannot
  provision or verify remotely, and it does nothing for the plain-filesystem
  writes a denylist and polkit both leave alone (`~/.ssh`, shell rc files,
  other repos). Cost: requires contributor-side one-time setup (create the
  user, configure sudo/polkit denial for it, arrange the relay to run as it)
  that `contribute-hive`'s current one-command onboarding does not ask for
  today, and hive cannot enforce that a contributor actually did it.
- **A VM or disposable host for `contribute-hive`.** Strongest possible
  isolation for that specific mode — a compromised or misbehaving agent's
  worst case is a disposable VM, not the daily driver. Cost: the largest
  operator-experience hit of any option here — it changes "clone and run one
  command on your laptop" into "provision and maintain a VM," which cuts
  directly against what makes `contribute-hive` easy to adopt. Realistically a
  documented recommendation for risk-averse contributors rather than a shipped
  default.

## Recommendation

> **Update:** this recommendation is about the hub-side Podman sandbox
> (`agent_sandbox`), which is a separate axis from the contributor-local
> per-backend work this page's status line now describes — that work closed
> the specific incident path (`contribute-hive ... local`) without touching
> the Podman double-gate discussed here. The recommendation below is
> unimplemented and still stands as an open item.

**This investigation's single strongest recommendation: single-gate or
default-on the existing Podman sandbox for `contribute-hive` (both
containerized and, especially, `local` mode), with the current denylist
(#4938) kept as the floor for hosts where the sandbox genuinely cannot run.**
The sandbox is already built, already does real workspace confinement via a
bind mount and `--userns=keep-id`
(`src/pkg/sandbox/sandbox.go:80-116`), and is exactly the shape the Codex
comparison in the original issue argues for. The double gate
(`src/pkg/config/config.go:1027-1033`) is a configuration default, not a
missing capability — closing it is materially cheaper than any option in
category B that requires new code (bwrap/seccomp/VM), and it directly answers
the mode where the incident actually happened (`local`, no container, no
distinct uid).

The qualifier that keeps this from being an unconditional "just flip it":
`sandbox-isolation.md`'s own gaps section says the Podman path is covered only
by skip-when-absent tests, with no always-on integration lane yet
(`src/docs/sandbox-isolation.md:47`), and the network trade-off for inference
backends is real and unresolved (`src/docs/sandbox-isolation.md:41-43`). Making
it the default for hosts that may not have podman, or before that CI gap is
closed, would trade a known, narrow risk (host-state commands, now partly
denylisted) for an unknown, broader one (a sandbox path that has not run
continuously in CI reaching the same"unattended for hours" workload the tmux
path handles today). So: close the CI gap first, then default the sandbox on
for `contribute-hive`, with the denylist remaining as-is for any path that
still opts out.

Distinct from that: **extending the #4938 denylist's coverage of absolute-path
and wrapper-indirected invocation is cheap, independent, and worth doing
regardless of the sandbox decision** — it is a same-shaped, low-cost follow-up
to work already merged, not a new architecture, and it narrows the exposure on
every host that has not yet moved to the sandbox for whatever reason.

## Doc status (already corrected by #4938)

The original ask under this issue included fixing `sandbox-isolation.md` if it
overstated the guarantee. It did, and #4938 already corrected it: the current
text draws the credentials/pushes-vs-workspace-confinement line explicitly,
cites #4918 and the incident by name, and states the denylist is "a floor, not
a sandbox" in the same paragraph
(`src/docs/sandbox-isolation.md:9-15`). No further doc change is proposed by
this investigation beyond what's already merged. This page's own
[bypass-shape findings](#what-this-does-not-cover) are additive detail the
current doc does not carry — mentioning that the denylist's bypass shape is
unverified for absolute-path/wrapper invocation would strengthen
`sandbox-isolation.md` further, but is not itself a correction of an
overstated claim in the shipped text.

## Open questions

- Whether Claude Code's `Bash(cmd:*)` deny pattern matches absolute-path
  invocation (`/usr/bin/rpm-ostree`) or only the bare command word. Not tested
  in `src/pkg/agent/host_state_deny_test.go`; the PR's own verification table
  tested the bare-command case only.
- Whether a shell function/alias/wrapper that ultimately execs a denied binary
  is caught by the same `Bash` tool-call parsing, or bypasses it.
- Whether non-`Bash` tool surfaces (MCP servers, subagent/`Task` tool calls)
  inherit the `--disallowed-tools Bash(...)` denials, are separately
  denyable, or are entirely unaddressed.
- The actual capability/seccomp/device posture of the containerized
  `contribute-hive` image beyond `USER dev` (`src/Dockerfile.contributor:191`)
  — not audited in this pass.
- Whether `src/deploy/k8s/deployment.yaml`'s pod-level `securityContext`
  (`:34-79`) meaningfully bounds what a hub-pod agent could reach outside its
  own workspace if it escaped the process-identity separation
  `agentExecUserSpec` provides — not audited in this pass.
- Whether any operators are currently running hive natively (systemd, no
  container at all) for hub-side agents, which would put that deployment mode
  in the same "real desktop, real bootloader" risk class as
  `contribute-hive local` — not established either way in this investigation.
