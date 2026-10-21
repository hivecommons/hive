# Security model: how hive keeps autonomous agents from doing damage

This page is for the maintainer or security reviewer at another organisation asking *"you run autonomous coding agents against real repositories — what stops one from doing damage?"* It is the complete inventory of hive's security and safety controls, each grounded in the tree (file paths, config keys, ADR numbers). Nothing here is aspirational: if a control is a proposal rather than code, it says so.

Hive's design rule applies doubly to safety: *if a human would give the same answer every time, it belongs in infrastructure, not in a prompt.* The controls below are enforced by Go, shell and kernel mechanisms — authentication middleware, a policy proxy, scoped tokens, file permissions, iptables, deterministic merge gates — not by asking an LLM to behave. Everything described is verifiable on branch `v5`.

This is an inventory, not a promise that every running hive has enabled every control. Check effective configuration, deployment and repository rules before relying on a gate. Deterministic authorization, model-based detection, advisory signals and proposed controls have different guarantees; an enabled dashboard toggle alone is not evidence of enforcement.

Companion pages: [security-threat-model.md](security-threat-model.md) is the attacker-oriented view (assets, trust boundaries, actors, residual risk); [security.md](security.md) covers log scrubbing and secret redaction; [securing-your-hive.md](securing-your-hive.md) is the first-time operator's decision guide to ACMM level, reporter trust and repo scope; [security-self-assessment.md](security-self-assessment.md) is the CNCF TAG-Security artifact. This page is the map of the mechanisms.

## 1. Threat model

What hive defends against, in the order a reviewer usually asks:

- **Prompt injection.** Issue/PR titles, bodies, labels, comments, cloned repositories and tool output are attacker-controlled text that reaches an agent's prompt. Assume every agent *will* eventually be told to do something hostile; design so that it cannot.
- **Runaway or stuck agents.** An agent that loops, burns tokens, re-dispatches itself forever, or wedges a pane and silently stops producing.
- **Credential exfiltration.** An agent (prompt-injected or merely careless) printing, pushing, or sending off-host a GitHub token, provider key, or hub secret — or one agent reading another's.
- **Bad merges.** Code landing on a protected branch without CI, review, human sign-off where policy requires it, or at a different SHA than the one that was judged.
- **Malicious or over-trusted contributors.** Humans and bots filing issues or PRs that steer agents, claim work above their trust, self-nominate as owners, weaken CI, or get their PR auto-merged.
- **Quota and cost abuse.** Denial-of-wallet through token burn, GitHub API rate-limit exhaustion, or label/comment write storms.
- **Platform compromise.** Unauthorised dashboard/terminal access, forged hub↔spoke messages, cross-tenant exposure on the hosted hub.
- **Supply chain.** A poisoned base image, npm postinstall, unpinned Action, or vulnerable Go dependency entering the hive image or CI.

## 2. Control inventory at a glance

| Control | Mitigates | Where it lives | How to verify it is on |
|---|---|---|---|
| Bounded kicks with context reset (`clear_on_kick`) | prompt injection persistence, runaway agents | `src/pkg/agent/manager_kick.go`, `src/pkg/config/config.go` (`clear_on_kick`, `continuous_turn_ceiling`) | `hive.yaml` agent block; `GET /api/agents` |
| Agent watchdog + crash-loop pause | stuck/runaway agents | `src/pkg/agent/watchdog_fleet.go`, `src/pkg/watchdog`, [agent-watchdog.md](agent-watchdog.md) | `conditions[]` in `GET /api/agents`; `HIVE_WATCHDOG_PAUSE` unset |
| Per-agent UIDs, tmux sockets, `su-exec` 4750 | cross-agent access, privilege escalation | `src/deploy/entrypoint.sh`, `src/Dockerfile`, `src/scripts/check-suid-contract.sh` | `/var/run/hive/uid-map.json`; `suid-contract` CI job |
| Per-agent tool denylists, GitHub MCP writes denied | direct GitHub writes from tools | `src/pkg/agent/manager_launch.go` (`--deny-tool`, `--disallowed-tools`) | agent launch command in tmux pane |
| Host-state denials + shell env scrub | host damage, token reuse in tool shells | `bin/agent-env-scrub.sh`, `bin/agent-launch.sh`, [sandbox-isolation.md](sandbox-isolation.md) | `BASH_ENV` in agent shell |
| Credential-free Podman/K8s sandbox + push broker (opt-in) | credential exfiltration, host damage | `src/pkg/sandbox`, `src/pkg/kubejob`, `src/pkg/pushbroker` (`agent_sandbox.enabled`) | `agentSandboxEnabled` in `GET /api/config/governor` |
| Beads ledger + knowledge store, not conversation memory | injection persistence, context sprawl | `src/pkg/beads`, `src/pkg/knowledge`, ADR-0004, ADR-0011 | `bd` CLI; `hivectl knowledge` |
| ioscan input/output scanning + canaries (default on) | prompt injection, secret emission | `src/pkg/ioscan`, `src/pkg/scheduler/ioscan_enforce.go`, ADR-0008 | `ioscanEnabled`, `ioscanFailMode`, `ioscanCanaries` in `GET /api/config/governor` |
| Trajectory review (default on) | goal drift, multi-step injection | `src/pkg/trajectory`, ADR-0009, [trajectory-review.md](trajectory-review.md) | `governor.trajectory.enabled`; `hive_trajectory_reviews_total` |
| MITM policy proxy + repo allowlist | credential exfiltration, out-of-scope writes | `src/pkg/proxy/rules.go`, `src/pkg/proxy/github_proxy.go`, ADR-0002 | `403` + `X-Hive-Proxy-Blocked`; boot log line |
| Forced egress via iptables (`CAP_NET_ADMIN`) | proxy bypass | `src/deploy/entrypoint.sh`, [net-admin-requirement.md](net-admin-requirement.md) | exit 77 / `HIVE_PROXY_ADVISORY_OK` absent |
| Tiered GitHub App tokens per agent mode | blast radius of a leaked token | `src/pkg/agentmode/agentmode.go`, `src/pkg/agent/manager_tokens.go`, ADR-0007 | token tier in agent card; App installation scope |
| Proxy-side credential injection (opt-in) | token exfiltration | `src/pkg/proxy`, `HIVE_PROXY_INJECT_GH_AUTH` | `security.credentialInjection` in `GET /api/config/governor` |
| Inference-key placeholder swap | provider key exfiltration | `src/pkg/apiproxy`, [apiproxy.md](apiproxy.md) | agent env shows `sk-hive-<agent>` |
| `gh` wrapper + per-agent restriction files | enumeration abuse, denied commands | `bin/gh-wrapper.sh`, `/etc/hive/restrictions/<agent>.json` | `gh` resolves to `/usr/local/bin/gh` |
| `hive-open-pr` / `hive-merge` relays (UID-attributed, SHA-bound) | forged authorship, TOCTOU merges | `bin/hive-merge.sh`, `bin/hive-open-pr.sh`, [hive-merge.md](hive-merge.md), [github-write-surface.md](github-write-surface.md) | REST `POST /pulls` and `PUT …/merge` return 403 for agents |
| Automerge gates (CI, review verdict, labels, mergeable, SHA re-fetch) | bad merges | `src/pkg/github/automerge/automerge_sweep.go` | skip reasons in merge-lane audit entries |
| Trusted-author automerge rules | over-trusted humans/bots | ADR-0023, `auto_merge.trusted_authors` | `hive.yaml` `auto_merge` block |
| Intent verification tiers + alignment | scope creep, guardrail edits | `src/pkg/intent`, [intent-verification.md](intent-verification.md) | `intentEnforce` / `intentShadow` in governor config |
| Hold guard (approved diff == merged diff) | post-approval drift | `src/pkg/holdguard` | evidence comment on held PR |
| Escalation circuit breaker (3 red SHAs) | fix loops | `src/pkg/escalation/escalation.go`, ADR-0010, ADR-0019 | `needs-human` + escalation comment |
| Central stage engine (one writer, anomalies metered) | label races, silent regressions | `src/pkg/stageengine`, ADR-0025 | `governor.labels.stage.engine.mode`; `stage_transition_anomaly_total` |
| Review swarm verdict relay | self-asserted approvals | `src/pkg/review`, [review-swarm.md](review-swarm.md) | `/var/run/hive-metrics/review-verdicts.json` |
| Review evidence bundles (hashed, Ed25519-signable) | unexplained merges | `src/pkg/evidence`, `src/pkg/github/review_evidence.go`, [review-evidence.md](review-evidence.md) | `hivectl evidence export`; `GET /api/evidence/summary` |
| Sentinel (8 rules, `sentinel-alert`) | malicious PRs, CI weakening, secret exfil | `src/pkg/sentinel/sentinel.go`, `src/pkg/config/sentinel.go` | `sentinel:` block (default on); label on PR |
| Contributor tiers + delegatable roles | over-trusted contributors | [contributor-trust-and-roles.md](contributor-trust-and-roles.md), `src/pkg/claims`, `src/pkg/issueclaim` | `trust_tier`; `hub.contribute_delegatable_roles` |
| Reporter trust (opt-in) | hostile issue reporters | `src/pkg/config/reporter_trust.go`, `src/pkg/github/pr_reporter_trust.go` | `project.issue_filter.reporter_trust.enabled` |
| `authorized_users` roles (read/read-write/merger/owner) | unauthorised dashboard writes | `src/pkg/config/config.go`, `src/pkg/dashboard/api_auth.go` | `HIVE_AUTHORIZED_USERS`; 403 on write as `read` |
| `/hive approve` / `/hive decision` permission gate | unauthorised un-parking | `src/pkg/github/issue_unpark_command.go`, [maintainer-commands.md](maintainer-commands.md) | reply names the refusal reason |
| Audit log + token-access log + agent audit | unattributable actions | `src/pkg/dashboard/audit.go`, `src/pkg/agentaudit`, [audit-log.md](audit-log.md), [token-access-log.md](token-access-log.md) | `GET /api/audit`; `/data/audit.jsonl` |
| Token budget + fair-share + per-kick caps | denial-of-wallet, write storms | `src/pkg/governor/governor.go`, `src/pkg/scheduler/repofilter.go`, `src/pkg/scheduler/reviewer_lane.go` | `governor.budget.*`; `DefaultSentinelMaxActions`, `DefaultIssueUnparkSweepMaxActions` |
| Repo pause / agent pause / watchdog kill switch | emergency stop | `src/pkg/config/repo_pause.go`, [repo-pause.md](repo-pause.md), `POST /api/pause/<agent>` | `project.paused_repos`; `HIVE_WATCHDOG_PAUSE` |
| Webhook HMAC verification | forged events | `src/pkg/dashboard/api_github_webhook.go`, `src/pkg/hub/webhook.go` | `GITHUB_WEBHOOK_SECRET` set; unsigned → rejected |
| Hub auth (Ed25519 sessions/SSO, per-hive bearers, constant-time compare) | platform compromise | `src/pkg/hub/saas_auth.go`, `src/pkg/hub/saas_crypto.go` | `GET /api/saas/admin/auth-rollout` |
| Dashboard CSP + security headers | XSS on the operator console | `src/pkg/dashboard/server.go`, `src/pkg/dashboard/webstatic/csp_script_src.go`, ADR-0015, ADR-0016 | response headers |
| Log scrubbing | secrets in logs | `src/pkg/logscrub`, [security.md](security.md) | redacted patterns in pod log |
| Supply-chain guards (govulncheck, gosec, Scorecard, Dependabot, SHA-pinned Actions, SUID inventory, digest-pinned bases, `--ignore-scripts`) | supply chain | `.github/workflows/go-security-analysis.yml`, `scorecard.yml`, `suid-contract.yml`, `.github/dependabot.yml`, `src/scripts/check-action-pins.sh`, `src/Dockerfile` | PR checks |
| DCO on every commit | unattributed code | `src/scripts/check-dco-trailers.sh`, `.github/workflows/dco-*.yml`, automerge DCO gate | DCO check on PR; `noteStageDCOGate` |

---

## 3. Agent runtime isolation

### Bounded, context-reset runs

Work arrives as discrete **kicks** from the governor. Context reset defaults on but can be overridden per agent; fresh bounded prompts are not a hard wall-clock kill switch:

- **Fresh context per kick.** `clear_on_kick` (`src/pkg/config/config.go`, default `true`; [supervisor.md](supervisor.md)) sends the backend's `/clear` before the kick prompt is pasted (`src/pkg/agent/manager_kick.go`), so the agent starts from the kick text, its `AGENTS.md`, and the bounded knowledge primer — not from whatever the previous issue's author wrote. Headless backends get a brand-new conversation instead of `/clear`. The deliberate exception is a resumed PR session (`src/pkg/agent/pr_session_resume.go`), which keeps the context of the PR it is already fixing. **Scope, honestly:** the backend CLI *process* in the tmux pane is long-lived; what is reset is the model context, not the OS process. A restart (watchdog or manual) drops the pane and its buffer (`src/pkg/agent/manager_tmux.go`).
- **Turn ceilings.** `continuous_turn_ceiling` (`src/pkg/config/config.go`, consumed in `src/pkg/governor/kick_outcome.go`) bounds how long the governor waits on a continuous-mode turn before treating it as finished; detached restarts carry a hard timeout so a wedged tmux path can never block the governor tick.
- **Watchdog.** On every governor tick the watchdog classifies each pane (`ready`, `shell-prompt`, `auth-required`, `stuck-overlay`, `no-output`, `no-session`), restarts dead panes with exponential backoff (`1m…16m`), and after `crash_loop_after` (default 5) consecutive failed restarts **pauses** the agent with trigger `watchdog-crashloop`, raises a banner alert, and stops — no more restarts until a human or the resume API intervenes. A login prompt is never restarted into (that was the 1042-restart loop). Backoff state persists in the hive state file so a pod restart does not reset an escalated agent into a storm. `HIVE_WATCHDOG_PAUSE=true` downgrades every hive to observe-only and can only ever reduce authority. `src/pkg/agent/watchdog_fleet.go`, `src/pkg/watchdog`, [agent-watchdog.md](agent-watchdog.md).
- **No fleet-wide shared context.** Each agent has its own tmux server on its own socket, its own `$HOME` layout under its own UID (per-agent settings seeds and 0600 session files are what the watchdog's credential probe reads), and the hive-owned `/var/run/hive-metrics` directory that agents cannot write to. Agents coordinate through the beads ledger and GitHub, never by reading each other's transcripts.

Agent `max_turn_duration` is **visibility-only**: zero disables it and exceeding it does not interrupt a turn (`AgentConfig` in `src/pkg/config/config.go`). Confirm effective budgets and provider limits rather than assuming usage charts enforce a runtime or spending ceiling.

### Agent sandboxing

Agents are unprivileged, separated, and policed at the network layer:

- **Per-agent Unix UIDs.** Each agent runs as its own user (UIDs allocated from base 2001) via `su-exec`, with its own tmux server on a per-agent socket. One agent cannot attach to another's session or signal its processes. `su-exec` is mode `4750 root:hive-launch`, so no agent UID can re-exec it to become root.
- **Per-agent GitHub tokens, not shared.** Each agent's scoped token is delivered to a per-agent cache file that is not world-readable: the entrypoint pre-creates it owned `dev:hive-<agent>` with mode `0640`, so only that agent's group can read it, and the locally-minted token path additionally writes `0600` and `chown`s to the agent's UID (deleting the file rather than leaving it shared if the chown fails). Agents cannot read each other's GitHub tokens. With [proxy-side credential injection](#proxy-side-github-credential-injection) on (opt-in: `HIVE_PROXY_INJECT_GH_AUTH=true`), that cache holds only an inert placeholder and the real token never leaves the hive process.
- **Per-agent tool denylists.** Declarative per-agent tool rules in `hive.yaml` become hard CLI flags at launch (`--disallowed-tools` for Claude, `--deny-tool` for Copilot). Separately, GitHub MCP *write* tools are denied for **every** agent in **every** mode — the MCP layer never grants agents a GitHub write path regardless of autonomy level; GitHub writes are instead governed by the token tier and the policy proxy below.
- **Claude Remote Control is pinned off.** Since Claude Code ~2.1.226 the Remote Control bridge (session publishing to claude.ai/code) auto-starts whenever no explicit `remoteControlAtStartup` value exists — a server-side rollout flag decides, so a routine image upgrade could expose every agent as a remote-controllable session under the shared claude.ai account. Hive writes a durable `"remoteControlAtStartup": false`, re-asserted at every claude-CLI launch: merged into the shared `/data/home/.claude/settings.json` for the plain claude backend, and into the per-agent settings seed for inference-gateway agents. The merge is add-if-missing only — an operator who wants the bridge sets the key to `true` in the same file and hive never clobbers it, logging an Info receipt so an enabled bridge is always explainable — and a launch-time check warns if an agent's session file records `hasUsedRemoteControl` despite the pin.
- **A default-on GitHub policy proxy.** Every agent's `HTTPS_PROXY` points at an in-pod proxy that intercepts **GitHub API traffic** and enforces, deterministically, what the agent's current ACMM mode allows: REST method/path rules, GraphQL query-vs-mutation gating, and a **repository allowlist** - writes to any repo outside the hive's configured list are blocked with `403` (`X-Hive-Proxy-Blocked`) at the network layer, regardless of what the agent was prompted (or prompt-injected) to do. The proxy MITM-inspects only `api.github.com` (plus every GitHub-family host when [proxy-side credential injection](#proxy-side-github-credential-injection) is on); other destinations are tunneled without inspection. The same proxy attributes traffic to agents by UID (read from `/proc/net/tcp`, unforgeable) for token accounting.


### Credential-free sandbox and push broker (opt-in)

For the strongest posture, a kick can run in a rootless Podman container (`src/pkg/sandbox`) or a Kubernetes Job (`src/pkg/kubejob`, #6311) that holds **no credentials at all**. Enable with `agent_sandbox.enabled: true` *and* the per-agent `sandbox.enabled: true` — both gates are required and the dashboard Security tab writes only the global one ([sandbox-isolation.md](sandbox-isolation.md)). If the sandbox produces commits, `src/pkg/pushbroker` runs outside it: it scans the committed diff for token-like secrets and protected-path edits, mints a short-lived scoped App token, pushes the branch, and opens the PR through the App-authored client. Broker rejection records audit detail and pushes nothing. Claude-family launches additionally carry host-state denials for `sudo`/`pkexec`/`doas`/`su` and the boot/deployment tools (`rpm-ostree`, `bootc`, `ostree`, `grubby`, `bootctl`, `efibootmgr`) that reach polkit without escalation — the #4918 class. Contributor-path confinement differs per backend; the per-backend matrix is in the same page.

### In-container privilege model

- The hive/proxy process runs as user `dev` (UID 1001); each agent runs as its own UID from `HIVE_UID_BASE=2001` upward.
- `su-exec` is mode **4750 `root:hive-launch`** — only root and members of the pinned `hive-launch` group (GID 1002) can exec it. This closes the earlier world-executable-setuid hole where any agent UID could become root in the pod. `HIVE_LAUNCH_GID=1002` is part of the deployment contract: Kubernetes `fsGroup` and Secret `defaultMode: 0440` rely on the numeric GID, and it must stay in sync across `src/deploy/k8s/*.yaml` and hub provisioning.
- **`su-exec` is the only setuid binary in the image.** The 4750 mode above restricts the helper Hive ships; it says nothing about the ones the base image ships, and `node:26-slim` (Debian 13) plus the `passwd`/`util-linux` packages leave eleven behind — `chfn`, `chsh`, `gpasswd`, `mount`, `newgrp`, `passwd`, `su`, `umount` (setuid root) and `chage`, `expiry`, `unix_chkpwd` (setgid shadow). All eleven were **world-executable**, so every agent UID could exec them: the same class of surface the 4750 mode exists to remove, sitting next to it. Nothing in Hive uses any of them — the entrypoint creates agent accounts with `useradd`/`groupadd`/`usermod` while still root, and every privilege hop goes through `gosu` (as root) or `su-exec` (as `dev`) — so `src/Dockerfile` strips the bits after the last install layer and **fails the build** if the resulting inventory is anything but `4750 root:hive-launch /usr/local/bin/su-exec`. Only a non-root caller needs a setuid bit, so root keeps full use of `mount`/`su`/`passwd`.
- **Why the image, and not `allowPrivilegeEscalation: false`.** Setting it would set the kernel's `no_new_privs` bit, which disables *all* setuid binaries — including `su-exec`, which would break agent launch outright. So `no_new_privs` stays off by necessity, and a setuid-root exec from an agent UID would land in a bounding set that still holds `SETUID`, `SETGID` and `DAC_OVERRIDE` — enough to read the MITM CA key whatever mode its directory carries. Removing the binaries is the only lever the deployment leaves. Enforced by `src/scripts/check-suid-contract.sh` (static, every PR) and `src/deploy/test_image_suid_inventory.sh` (boots the image and reads the inventory back; also run against the real built image in `docker.yml`).
- The MITM CA certificate is readable at `/data/proxy-ca.pem` so agent clients can trust forged certificates, but the CA private key is stored at `/data/.hive/proxy-ca-key.pem` in a `0700` directory with mode `0600`. It is never intentionally kept directly under the shared, agent-writable `/data` namespace. A legacy `/data/proxy-ca-key.pem` causes the old CA pair to be discarded on startup, because its key may already have been copied.


---

## 4. Context and memory

Hive replaces long-lived agent memory with two inspectable stores, which is what bounds a prompt-injection's blast radius to one kick:

| Store | What persists | What does not | Where |
|---|---|---|---|
| **Beads work ledger** (ADR-0004) | Typed work items, status, priority, dependencies, actor claims, notes, external issue refs — git-backed JSON per agent | Conversation text, tool output, raw issue bodies | `src/pkg/beads`, `bd` CLI ([beads-cli.md](beads-cli.md)) |
| **Knowledge store** (ADR-0011) | Typed facts with confidence, sources, tags and layer metadata; bbolt graph store; local-first with explicit promotion across layers | Unreviewed model output: the retro lane ingests lessons only after length, secret and dedup gates (`src/pkg/knowledge/retro_lesson.go`) | `src/pkg/knowledge`, [design/knowledge-system.md](design/knowledge-system.md) |

- **Agents are primed, they do not query.** A bounded set of selected facts is formatted into the kick prompt (`src/pkg/knowledge/api.go`); agents never get a raw handle on the store, so a poisoned fact cannot be amplified by an agent asking for "everything about X".
- **Untrusted text is scanned before it reaches a prompt.** ioscan (ADR-0008, `src/pkg/ioscan`, enforced in `src/pkg/scheduler/ioscan_enforce.go`) is stdlib-only and **default on**: issue/PR titles, labels and bodies are scanned for injection directives, zero-width/unicode tricks, base64-hidden instructions, secrets and high-entropy tokens; only text the block policy trips is replaced with a visible marker, benign text passes byte-identically. `fail_mode` resolves per ACMM level — `closed` (block the kick, `ioscan_fail_closed` audit entry) at L5/L6 where agents can merge, `open` (redact and continue) below — and an operator may pin either. An optional LLM-judge classifier scores already-redacted segments for plain-English semantic injection and fails open on outage. Operator surface: `ioscanEnabled`, `ioscanFailMode`, `ioscanCanaries` on `GET /api/config/governor` (owner-only to change: `src/pkg/dashboard/api_governor_security.go`).
- **Canaries.** Per-kick exfiltration markers are planted in prompts and agent-visible egress is scanned for them, encoding-aware (#6701, #6720); default on.
- **Trajectory review reads the transcript, not the agent's word.** See [§8](#8-detection-and-response).
- **Knowledge imports are secret-gated and transcripts are pruned.** `src/pkg/sessionprune` prunes per-agent home state; `src/pkg/logscrub` redacts token/JWT patterns from all log output ([security.md](security.md)).

---

## 5. Network and credentials

### API keys and credential isolation

The strongest isolation story is around inference-backend API keys — agents literally never possess them:

- **Per-agent placeholder keys.** An agent using a self-hosted inference backend (litellm / vllm / llm-d / watsonx) is launched with `ANTHROPIC_API_KEY=sk-hive-<agent-name>` and `ANTHROPIC_BASE_URL` pointing at a translator proxy bound to `127.0.0.1` inside the pod (with `NO_PROXY` keeping the call loopback-local). The placeholder authenticates the agent only to that in-pod translator. Nothing outside the pod accepts that string.
- **Server-side key swap.** The translator identifies the agent from its placeholder, converts the request from Anthropic to OpenAI format, and attaches the **real** gateway key server-side (as a `Bearer` header) before forwarding upstream. The real key lives only in the proxy's route table — never in any agent's environment, filesystem view, or prompt.
- **Same pattern for Linear.** Every ISSUES_ONLY+ agent request to `api.linear.app` passes the tier gate and then has its `Authorization` header replaced with the hive's current Linear credential (the connected app's OAuth token, or the work-source key without one). The token in the agent's environment is a courtesy for SDKs that insist on one; because the OAuth token rotates daily and a running process never sees a later environment push, the proxy's copy is the only one that stays valid.
- **Secret storage chain.** The real key is resolved from, in order: a configured key file → the Kubernetes Secret mount (`/secrets/litellm_api_key`) → an owner-only PVC file (`/data/secrets/litellm_api_key`, mode `0600` in a `0700` directory) → environment variable. When you enter a key in the dashboard it is written to the PVC file and patched into the hive's own Kubernetes Secret.
- **Never in config files.** `hive.yaml` only ever records a key file *path* or an environment variable *name* — never a key value. The dashboard's persisted config overlay (`/data/hive.yaml.dashboard`) is secret-free by design: env-derived secrets are collapsed back to `${VAR}` references and the dashboard auth token is blanked before writing.
- **Obfuscated in the UI.** Key entry uses a password field; after entry the API never returns the value — only whether a key exists, a masked tail hint, and which store it resolved from. A log-scrubbing handler additionally redacts GitHub-token and JWT patterns from all log output (details in [security.md](security.md)).
- **Inherited process credentials never reach a pane.** The hive process itself holds credentials in its environment — the Linear work-source key it expands from `${LINEAR_API_KEY}`, the Linear OAuth app's client secret and webhook secret, the full App installation token the entrypoint exports — and every per-agent tmux server it starts inherits them into its *global* environment, which tmux copies into every pane. At session creation the hive records a `set-environment -r` removal for each of them (a plain `-u` only deletes the session entry and lets the global value show through), then sets the one credential the agent's tier may hold. An ISSUES_ONLY+ agent therefore sees `LINEAR_ACCESS_TOKEN` (the connected app's grant) and nothing else Linear-related; below that floor it sees no Linear credential at all.

**Honest scope:** this placeholder mechanism applies to inference backends. Agents using cloud CLI backends (Claude, Copilot, Codex, Antigravity, Goose) authenticate with the provider credential you connected for that purpose — that credential is available to the CLI process, as it must be for the CLI to work. Cross-agent exposure of those credentials is limited by the [agent runtime isolation](#3-agent-runtime-isolation) layer.


### GitHub blast-radius controls

What can agents actually do to your repositories? As little as you've dialed in:

- **GitHub App scoping.** The recommended auth is a GitHub App — its reach is inherently limited to the repositories you installed it on. On top of that, hive mints **per-tier installation tokens** matched to each agent's autonomy mode: advisory agents get metadata/PR read-only tokens that *cannot* create issues; mid-tier agents get issues-only tokens with no code access; only trusted tiers get contents/PR write. The shared full-installation token is stripped from every agent's environment.
- **Repo allowlist, enforced twice.** The configured repo list is enforced in the policy proxy (hard, network-level) and also injected into every agent kick as an explicit `AUTHORIZED REPOS` constraint (a prompt-level reinforcement of the hard control).
- **ACMM maturity levels gate autonomy.** Six levels (L1–L6) map to per-agent policy modes enforced end-to-end by token tiers and proxy rules: advisory (observe only) → measured (file issues) → hold-gated PRs → full. At **L5**, the PR-request watcher labels every agent PR with literal `hold` so humans batch-review and approve; dashboard manual holds use `hive-pause/<hive-id>`, and `hive/<hive-id>` is provenance only; the underlying guarantee is that **merge permission simply is not granted below L6** — an L5 agent's token tier and proxy rules do not allow merging, whatever its prompt says. The system proposes; humans approve.
- **Reporter trust (opt-in).** Who *asked* is a control too. With `project.issue_filter.reporter_trust.enabled: true`, issues from reporters outside the trusted set (by default GitHub owners, members and collaborators, plus an explicit login list) are not worked until a maintainer adds a triage label, and a PR whose rationale traces to such an issue without a configured acceptance label is held for a human at **every** level, L6 included. Acceptance supplies maintainer sign-off for both admission and the PR check; it does not auto-release an existing PR hold. Maintainer-filed issues flow as before. Off by default; see [agent-configuration.md](agent-configuration.md#reporter-trust-who-filed-it-not-only-what-it-is-labelled) ([#9665](https://github.com/hivecommons/hive/issues/9665)).
- **DCO sign-off.** Agent policies require DCO-signed commits (`git commit -s`); pair this with a DCO check on your repos to make it a hard gate.
- **Proposed gate-integrity invariants.** The [gate-integrity design](design/gate-integrity-invariants.md) proposes that the GitHub App write gate / push proxy refuse agent history rewrites on branches the lane did not create, refuse agent sign-off trailers added to other authors' commits, and treat gate manipulation as an ACMM regression that demotes the lane to hold-gated pending human review.

- **History and attestation invariants.** Agent-authored branch pushes must be fast-forward updates. A lane must not force-push, rebase-and-push, or otherwise rewrite a branch it did not create; conflicts and DCO failures on contributor commits are reported back to the PR instead of being repaired by rewriting history. This invariant protects branches the lane did not create; rebasing or force-with-lease pushing the hive App's own lane-created branch (for example `agent/<lane>` or `<lane>/...`) is permitted when resolving that lane's PR conflict. Likewise, an agent may only add its own `Signed-off-by` trailer to commits it authored, because a sign-off is an attestation by the named identity.


### The `gh` wrapper, env scrub and relay scripts

- **`gh` is a wrapper.** `/usr/local/bin/gh` (`bin/gh-wrapper.sh`) sits ahead of the real binary, reads `HIVE_AGENT_ID`, applies per-agent and global restriction rules from `/etc/hive/restrictions/<agent-id>.json` (pattern → reason), and injects the agent's tier token — agents never pass `--token`. `bin/git-credential-hive.sh` does the same for git.
- **Tool shells are scrubbed.** Backend CLIs re-export their own live GitHub credential into every tool shell they spawn, after all launch-path scrubbing (#4045). `bin/agent-env-scrub.sh` is *sourced* at the start of every shell in the agent's process tree (via `BASH_ENV`/`ENV` from `bin/agent-launch.sh` and the `/etc/bash.bashrc` guard) and removes them, so `curl -H "Authorization: Bearer $GITHUB_TOKEN"` has nothing to send.
- **Writes go through relays, not the API.** REST `POST /pulls` and `PUT /pulls/{n}/merge` are hard-denied for every agent mode at the proxy; agents call `hive-open-pr`, `hive-open-issue`, `hive-merge`, `hive-review` (`bin/*.sh`), which drop a request file that the hive watcher processes as the App bot. The watcher derives the requesting agent from the **file's UID owner**, not from file contents, and applies the same `AuthorizeMerge`/`CanMerge` checks a direct write would need. The full allowlist of relay operations and what each refuses is in [github-write-surface.md](github-write-surface.md).
- **Short-lived minted credentials (opt-in).** The mint (ADR-0007, `src/pkg/mint`, [token-mint.md](token-mint.md)) issues scoped JWTs with bounded TTLs for cloud/registry authority instead of distributing long-lived shared secrets. Provider credential rotation and probing live in `src/pkg/rotation` (ADR-0024 is the proposed lifecycle).

### Proxy-side GitHub credential injection

`HIVE_PROXY_INJECT_GH_AUTH=true` ([#1861](https://github.com/hivecommons/hive/issues/1861)) moves every agent's usable GitHub credential out of the agent's reach. The hive mints each agent's tier-scoped App token as before, but keeps it in an in-memory registry inside the hive process; the agent's readable token cache receives only the placeholder `hive-proxy-injected-<agent>`, and so do the `GH_TOKEN`/`GITHUB_TOKEN` values derived from that cache. Any real `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN` or `GITHUB_ENTERPRISE_TOKEN` in the hive process's own environment is removed from every agent's tmux session, push-capable agents included, so no agent env var carries a usable GitHub token. The MITM proxy strips whatever `Authorization` an agent sends to a GitHub host and attaches the real token of the agent it identified by UID. A prompt-injected agent that prints its token leaks a string that authenticates nowhere, and a credential smuggled out of a backend CLI's `/proc/<pid>/environ` is stripped at the proxy before GitHub sees it (on spokes where forced egress is enforced, so every GitHub request passes the proxy). The rewrite is limited to GitHub hosts: a request bound for Linear, which the same proxy inspects, never carries an agent's GitHub token ([#9586](https://github.com/hivecommons/hive/issues/9586)).

Who has it on:

- **Nobody by default. Injection is opt-in only.** It is on only on a spoke whose Deployment sets `HIVE_PROXY_INJECT_GH_AUTH=true`. Unset is off on every hive - hosted or self-hosted, GitHub App or PAT, spoke or hub - and the hub renders no value onto newly provisioned spokes. A brief default-on for hosted App spokes ([#9597](https://github.com/hivecommons/hive/pull/9597), [#9625](https://github.com/hivecommons/hive/pull/9625)) was reverted before any spoke ran it ([#9586](https://github.com/hivecommons/hive/issues/9586)).
- **Opting a spoke in:** set `HIVE_PROXY_INJECT_GH_AUTH=true` on its Deployment. Injection needs GitHub App auth (the App-minted per-agent tokens are what the proxy injects); on a PAT-only spoke there is nothing to inject. The spoke logs one line at boot saying which way it resolved and why, for example `proxy GitHub auth injection: off (opt-in: set HIVE_PROXY_INJECT_GH_AUTH=true)`, and the dashboard Security tab shows the same state (`security.credentialInjection` in `GET /api/config/governor`, and "GitHub auth proxy-injected / agent-held" in the posture strip).
- **Copilot auth exchange is passed through.** The Copilot CLI trades the user's Copilot OAuth token (`COPILOT_GITHUB_TOKEN` / the device-flow token in `/data/copilot-user-token`) for its session token at `/copilot_internal/` on the GitHub API host (`/api/v3/copilot_internal/` on GHE). An App installation token cannot perform that exchange, so under injection the proxy leaves the agent's own `Authorization` untouched on those paths - nothing stripped, nothing injected. **Residual:** the Copilot user OAuth token remains agent-readable and spendable on those endpoints; moving it server-side too is a follow-up. No hub-held App token is ever attached there.
- **Explicit opt-out:** `HIVE_PROXY_INJECT_GH_AUTH=false` behaves exactly like unset (off) but records the decision on the pod spec.

Two settings are checked at spoke startup:

- **`true` together with `HIVE_PROXY_ADVISORY_OK=true` is refused** (exit code 19, log line `refusing to start: contradictory GitHub credential configuration`). Advisory mode lets the proxy trust a self-asserted `Proxy-Authorization` agent name when UID identification fails. Under injection that name picks whose real token is attached, so a caller could claim a more privileged agent and receive its token. Restore forced egress, or unset `HIVE_PROXY_INJECT_GH_AUTH` (or set it to `false`).
- **An unrecognized value** such as `1`, `TRUE`, `yes` or `on` is **not** fatal, because spokes auto-deploy shortly after a merge and a refusal would crash-loop any spoke that already carries one. It keeps its old meaning (off, so the agent still holds its real token) and is reported loudly: an ERROR log line `GitHub credential configuration warning` at boot, and an entry in the dashboard Security tab's coherence warnings (`security.credentialWarnings` in `GET /api/config/governor`). Only unset, `true` and `false` are recognized.

What this does not yet do, tracked in [#9586](https://github.com/hivecommons/hive/issues/9586): the token holder still lives in the hive process rather than a separate signing sidecar on an internal-only network, and the Copilot user OAuth token is not yet injected server-side.


### Forced proxy egress and `CAP_NET_ADMIN`

The entrypoint installs an iptables REDIRECT of all outbound `:443` through the MITM proxy, so agents cannot bypass capability enforcement with a raw token. Consequences for deployment:

- **Spokes require `CAP_NET_ADMIN` + usable iptables extensions to start enforcing.** If the chain cannot be created, or any non-optional rule (packet-mark exemption, HTTPS `REDIRECT`, or `OUTPUT` hook) cannot be appended, the entrypoint logs the exact iptables stderr, flushes the partial `HIVE_PROXY` chain, and refuses to start. Grant the capability with `--cap-add NET_ADMIN` (docker/podman) or `securityContext.capabilities.add: ["NET_ADMIN"]` (Kubernetes), and ensure the node has the required netfilter modules. The escape hatch `HIVE_PROXY_ADVISORY_OK=true` starts the spoke with egress enforcement **advisory-only** — agents can bypass the proxy — and logs a WARN saying exactly that. Chain creation retries 5× with jittered backoff so co-scheduled spokes don't fail in lockstep. This FATAL exits with a **distinct exit code, 77** (sysexits.h `EX_NOPERM`) in exactly two cases — the bounding set lacks `CAP_NET_ADMIN`, or the node's kernel is missing a required netfilter module (`xt_mark` or `xt_REDIRECT`; a #6003 preflight probes them in a throwaway chain before building the real ruleset and names the missing module in the FATAL) — so a supervisor can key off it without parsing logs; any other cause of the same FATAL still exits `1`. `xt_owner` is optional (WARN only). For the missing-module case the fix is loading the modules on the node (e.g. a MachineConfig `/etc/modules-load.d/` drop-in), not granting the capability. See [net-admin-requirement.md](net-admin-requirement.md#when-the-container-refuses-to-start-exit-77).
- **The hive binary carries no file capabilities.** Earlier channel images shipped `/usr/local/bin/hive` with a `cap_net_admin+ep` file capability, which made the kernel refuse `execve()` with a bare `Operation not permitted` wherever the container's bounding set lacked `NET_ADMIN`. The binary now execs everywhere; the entrypoint instead raises `NET_ADMIN` as an **ambient capability** at the privilege drop, gated on the bounding set actually having it. Without the grant you get a one-line NOTICE instead of a crash.
- **Agent identity is UID-based, not self-asserted.** The proxy reads `/proc/net/tcp` to resolve the calling process's UID against `uid-map.json`, which is unforgeable — a process cannot claim another UID's socket. A `Proxy-Authorization: hive <name>` header sent by the caller is a fallback ONLY, and by default the proxy does not trust it: with no UID map (or no match for this connection), the caller is treated as unidentified (`ADVISORY` mode, writes blocked) rather than as whatever name the header claims. The entrypoint persists the allocation in `/data/.hive/uid-map.json` and writes a runtime copy to `/var/run/hive/uid-map.json`; existing agents keep their UID forever, new agents append after the current maximum UID, and first boot without a persisted map adopts unique existing per-agent directory owners before assigning new IDs. `HIVE_PROXY_ADVISORY_OK=true` also permits the header fallback, for deployments (local dev, native/systemd installs with no per-agent UID separation) that have no UID map to check against.
- **Rootless podman:** the image runs, but rootless cannot meaningfully grant `NET_ADMIN`, so the egress gate cannot be installed — a rootless spoke only starts with `HIVE_PROXY_ADVISORY_OK=true`, i.e. with the capability model unenforced. Treat rootless as unsupported for enforcing deployments.
- **Hosted fleet:** the hub sweeps hosted spoke Deployments every 15 minutes and patches `NET_ADMIN` into the container securityContext where missing. The patch replaces the whole `securityContext` object — capabilities hand-added to a hosted spoke will be erased within 15 minutes. (On OpenShift/OVN the podspec request is necessary but not sufficient; the SCC must also allow it.)


### Hub↔spoke channel

Registered hives send periodic heartbeats to the hub:

- Heartbeats are authenticated with a **per-hive** bearer key derived from the hub master secret (`HIVE_HEARTBEAT_KEY`, reconciled onto hosted spokes; self-hosted spokes derive it from `HIVE_HUB_SECRET` + `HIVE_ID`), validated with a constant-time comparison. The old fleet-wide shared bearer lane was removed.
- The outbound payload carries operational telemetry only: health and cluster metrics, version/git info, agent and governor summaries, and 24-hour **token counts**. No API keys, GitHub tokens, or other credentials leave the spoke in heartbeats.
- The response channel is how the hub delivers configuration to hives it manages (including GitHub App credentials for hub-provisioned hives, scoped to the caller hive's own key) — hub-to-spoke, never spoke-to-hub.


---

## 6. Code admission: review, CI and merge gates

"CI is green" is necessary and nowhere near sufficient. Between a green check and a merged PR hive runs a chain of deterministic gates; every one is independent of the agent's own claims.

Check the enforcing mode for the actual merge lane. `intent.enforce: true` enables intent blocking; report-only and `intent.shadow` do not block, and optional alignment-model errors fail open. `governor.labels.stage.engine.mode` supports off/shadow/enforce; shadow is not enforcement. The App self-merge review gate has its own `governor.review_gate.mode` (off/shadow/enforce), distinct from `review.require_approval`. Models can miss defects; missing review capacity is not approval (`src/pkg/intent`, `src/pkg/stageengine/mode.go`, `src/pkg/github/automerge/automerge_review_gate.go`).

| Gate | What it refuses | Where |
|---|---|---|
| **Relay authorization + SHA binding** | A merge request without a non-empty `expect_sha`; a `(repo, number)` not currently in the governor's merge-eligible list *at that exact head*; a requesting UID whose mode lacks `CanMerge` | `bin/hive-merge.sh`, [hive-merge.md](hive-merge.md) |
| **Automerge sweep preconditions** | draft, closed, missing SHA, `behind`/`dirty`/not-mergeable, `hold`, `needs-human`, `needs-rebase`, `do-not-merge`, `blocked`, `hive-pause/<hive-id>`, exempt labels; CI not green on the head; a current-head `CHANGES_REQUESTED`; a missing reviewer verdict when `review.require_approval` (default on); below-L6 or `repo_policies[].auto_merge: false` | `src/pkg/github/automerge/automerge_sweep.go` |
| **Merge-critical fresh fetch** | Merging on cached state: the PR and head SHA are re-fetched from GitHub immediately before the merge call and the SHA re-verified | same file (final re-fetch before merge) |
| **No-CI repositories** | Merging where no checks exist unless `auto_merge.no_ci_ok` is explicit; otherwise `hold` is applied | [hive-merge.md](hive-merge.md) |
| **DCO** | Merging a head whose required DCO check is not passing (`noteStageDCOGate`); `src/scripts/check-dco-trailers.sh` enforces trailers in hive's own CI; agents commit with `-s` by policy | `src/pkg/github/automerge/automerge_sweep.go`, `.github/workflows/dco-*.yml` |
| **Trusted-author automerge** (ADR-0023) | Auto-merging a human/bot PR unless the author is in `auto_merge.trusted_authors`, holds a configured `authorized_users` role (`merger` default, or `owner`) **and** GitHub write/maintain/admin on the repo; permission API failures fail closed; fork non-members rejected; every other gate still applies | `src/pkg/github/automerge/automerge_sweep.go` |
| **Intent verification** | A Tier-1 PR with no linked issue; a Tier-2 feature with no approved plan or maintainer approval; any Tier-3 change (workflows, `gh` wrapper, proxy rules, `policies/`, `hive.yaml`, `OWNERS`, `CODEOWNERS`) without maintainer approval; a Tier-0 "docs-only" PR whose diff escapes docs/test paths; scope drift between stated intent and changed paths. Evidence fetch fails closed on a partial diff | `src/pkg/intent`, [intent-verification.md](intent-verification.md) |
| **Hold guard** | Lifting a hold onto a head that moved since the human saw it: the held head SHA + author set is snapshotted, drift re-applies `hold`, posts one evidence comment naming the new commits, and re-arms | `src/pkg/holdguard` |
| **Review swarm verdicts** | A self-asserted approval: verdict files are written by the review relay into hive-owned `/var/run/hive-metrics`, validated against the PR actually reviewed, aggregated deterministically (any `reject` → close; any high finding → `requires_human`; non-unanimous → human) and capped per head to stop loops; `send-to-human` applies `needs-human` | `src/pkg/review`, [review-swarm.md](review-swarm.md) |
| **Escalation circuit breaker** (ADR-0010, ADR-0019) | Dispatching a fourth fix after three distinct red head SHAs: the ledger (keyed `repo#number`, formally modelled in `src/formal/escalation/`) stops re-dispatch, labels and comments the escalation, and routes to a person — once per item, never stalling silently | `src/pkg/escalation/escalation.go`, [formal-verification.md](formal-verification.md) |
| **Central stage engine** (ADR-0025) | Two code paths writing `stage/*` labels: `src/pkg/stageengine` is the only writer, an admission test (`src/pkg/stageengine/admission_test.go`) walks `src/` with `go/ast` to keep it that way, and an unexpected from→to hop becomes a metric (`stage_transition_anomaly_total`) rather than a silently accepted regression. Mode: `governor.labels.stage.engine.mode` (`off\|shadow\|enforce`) | [stage-engine-plan.md](stage-engine-plan.md) |
| **Sentinel label** | Hive approval and every automerge lane while `sentinel-alert` is present — see [§8](#8-detection-and-response) | `src/pkg/github/automerge/automerge_stage_human.go` |
| **Repo pause** | Agent writes, relays, enumeration and automerge on a paused repo (reads and human activity continue) | `project.paused_repos`, `src/pkg/config/repo_pause.go`, [repo-pause.md](repo-pause.md) |
| **GitHub itself** | Hive does not force-merge: branch protection, rulesets, required reviews and checks apply to the App token exactly as to a human; a refusal is recorded in the result file | — |

Every merge hive performs leaves a **review evidence bundle** (`src/pkg/evidence`, `src/pkg/github/review_evidence.go`): canonical JSON of what was reviewed, which checks and verdicts were seen, hashed and optionally Ed25519-signed, stored under `<review-links-dir>/evidence/<owner>/<repo>/<PR>/<head>.json` and exportable with `hivectl evidence export` ([review-evidence.md](review-evidence.md)). Still **proposed, not code**: the [gate-integrity invariants](design/gate-integrity-invariants.md) (refuse agent history rewrites on branches a lane did not create, refuse sign-off trailers on others' commits).

---

## 7. Contributor trust

Who asked matters as much as what was asked.

- **Reporter trust (opt-in).** `project.issue_filter.reporter_trust.enabled: true` (`src/pkg/config/reporter_trust.go`, `src/pkg/config/issue_filter.go`) withholds issues from reporters outside the trusted set (owners/members/collaborators plus an explicit login list) until a maintainer triages them, and holds any PR whose rationale traces to such an issue for a human at every level including L6 (`src/pkg/github/pr_reporter_trust.go`, `src/pkg/github/reporter_trust_wait.go`). Untrusted direct PRs get the configurable `clanker-requested` hold label.
- **Dashboard/API roles.** `authorized_users` carries `read` / `read-write` / `merger` / `owner` (`src/pkg/config/config.go`); role enforcement middleware in `src/pkg/dashboard/api_auth.go` blocks writes for `read` and reserves security-setting changes, agent sandbox toggles and merge-lane releases for `owner` or `merger`. Direct-route spokes get the same tiers via `HIVE_AUTHORIZED_USERS` (see [§9](#9-platform-and-supply-chain)).
- **Contributor tiers** ([contributor-trust-and-roles.md](contributor-trust-and-roles.md)). Relay contributors carry a `trust_tier`: `newcomer` (self-registration default) → `contributor` (automatic after 5 completed PR tasks) → `trusted` (operator grant, ~20 PR tasks) → `merger` / `advisor` (maintainer/owner grant). Which hive roles a contributor may run is gated by tier *and* by `hub.contribute_delegatable_roles` (safe default `scanner`, `quality`, `outreach`; `ci-maintainer`, `sec-check`, `architect` need `trusted`; `supervisor` is never delegatable). Contributor credentials never leave the contributor's machine ([contributor-relay.md](contributor-relay.md)).
- **Claims and leases.** Issue claims (`src/pkg/claims`, `src/pkg/issueclaim`) and contributor leases (`src/pkg/dashboard/contribute_leases.go`) are fenced by task generation; the lease protocol is formally modelled in `src/formal/contribute-lease/` with the invariant that no issue is held by two contributors with valid leases at once.
- **Author kind, not author claim.** `src/pkg/authorkind` classifies PR authors (hive App, trusted bot, human, unknown bot) from GitHub identity, and the stage engine/automerge consume that classification; `role/*` labels record it on the item.
- **Command bots.** Only bots in `label_driven_trusted_bots` (`src/pkg/config/repo_policy.go`) may approve via labels; `sentinel.exempt_logins` (e.g. `dependabot[bot]`) is the only sentinel bypass and is explicit config.
- **Permission-checked slash commands.** `/hive approve` and `/hive decision <text>` are honoured only from a human (`IsPersonLogin`) with `write`/`maintain`/`admin` on the repository (`unparkPermissionLevels`, `src/pkg/github/issue_unpark_command.go`); PRs ignore them for release purposes — a PR hold is released by a GitHub **Approve** review on the current head from a trusted merger, and a contributor cannot release their own PR (`src/pkg/github/automerge/pr_approve_command.go`). Sweeps are capped (`DefaultIssueUnparkSweepMaxActions = 10`) so a misparse cannot relabel a backlog.
- **Owner self-nomination is a sentinel rule.** A PR adding its own author to `OWNERS`/`CODEOWNERS`/`MAINTAINERS` is flagged and blocked regardless of who opened it ([§8](#8-detection-and-response)).

---

## 8. Detection and response

Prevention leaks; these are the controls that notice and stop.

### Sentinel

The sentinel sweep (`src/pkg/sentinel/sentinel.go`, config `src/pkg/config/sentinel.go`, **default on** for every watched repo) evaluates every open PR — from anyone — against eight rules:

| Rule | Detects |
|---|---|
| `sensitive_path` | Touches OWNERS, workflows, policies, hive config, security docs (`sentinel.sensitive_paths`, defaults in `DefaultSensitivePaths`) |
| `owner_self_nomination` | Adds the PR author's own login to OWNERS / CODEOWNERS / MAINTAINERS |
| `permission_escalation` | Widens Actions `permissions:` or switches to `pull_request_target` |
| `secret_exposure` | Workflow change that prints, encodes or ships secrets off-host |
| `ci_gate_weakening` | `continue-on-error`, `\|\| true`, removed checks |
| `test_removal` | Deletes test files or guts coverage |
| `security_policy_edit` | Edits SECURITY.md, dependabot/CodeQL config, rulesets, git hooks |
| `remote_code_execution` | `curl \| sh`, eval of encoded payloads, reverse shells, embedded private keys |

On a match it applies `sentinel-alert` (configurable `sentinel.label`, red `b60205`), posts one marker-stamped comment naming the findings, and writes an audit entry. The label is a **hard block for Hive approval and every automerge lane** until a maintainer removes it (`src/pkg/github/automerge/automerge_stage_human.go`), and the stage engine routes the item to `stage/085-needs-decision`. Trusted authors (hive App, configured trusted bots, authorized owners) get notice-only unless `sentinel.trusted_authors_block: true`. Per-pass labelling is capped (`max_actions`, default 20). The **release sentinel** (`src/pkg/releasesentinel`, [release-sentinel.md](release-sentinel.md)) does the same for release/CI policy failures with evidence required before escalation.

### Trajectory review (ADR-0009)

Every `governor.trajectory.interval_s` (default 600) a second model compares each running agent's last assigned intent with the last `transcript_lines` (default 120) of its *rendered tmux transcript* — not the agent's self-report. `on_divergence: pause` (default) pauses the agent and raises an error alert plus high-priority notification; `alert` only alerts. Default **on**; reviewer outage, timeout (30 s) or unparseable reply fails open as non-divergent so a reviewer outage cannot become a scheduler outage. Metrics `hive_trajectory_reviews_total{decision}`, `hive_trajectory_skipped_total{reason}`. `src/pkg/trajectory`, [trajectory-review.md](trajectory-review.md).

### Audit trails

| Trail | Records | Read it |
|---|---|---|
| Dashboard audit log (`src/pkg/dashboard/audit.go`) | `ts`, `user`, `action`, `detail`, `agent`, `repo`, `target` for every dashboard/API mutation, merge-lane decision, sentinel finding, ioscan fail-closed, watchdog action; in-memory ring of 500 plus `/data/audit.jsonl` rotated at 5 MB × 3 backups × 90 days | `GET /api/audit` (read-write role, newest 200) — [audit-log.md](audit-log.md) |
| Agent lifecycle audit (`src/pkg/agentaudit`) | `agent_started`, `agent_start_failed`, tool-approval decisions, turn envelopes — same vocabulary, same sink | same |
| Token-access log | Per-agent credential use — which agent, which command — that the dashboard audit log does not capture | `/var/run/hive-metrics/token-access.jsonl` — [token-access-log.md](token-access-log.md) |
| Review evidence bundles | What was reviewed and why it merged (hashed, signable) | `hivectl evidence export`, `GET /api/evidence/summary` |
| Proxy decisions | Blocked requests (`X-Hive-Proxy-Blocked`), per-UID attribution, token accounting | proxy log, dashboard token tracking ([token-tracking.md](token-tracking.md)) |
| Agent transcripts | Per-agent session logs, scrubbed by `src/pkg/logscrub` | [agent-logging.md](agent-logging.md) |

### Governors and budgets

- **Token budget.** `governor.budget` (`src/pkg/governor/governor.go`): `weekly_limit` / coin / USD limits over `period_days` (default 7), warn at `critical_pct`, suppress kicks on exhaustion except `ignored_agents`.
- **Fair share and caps.** `fairShareByRepo` (`src/pkg/scheduler/repofilter.go`) spreads kicks across repos; `reviewerMaxPRsPerKick = 3` (`src/pkg/scheduler/reviewer_lane.go`); every sweep that writes labels/comments carries a per-pass cap (sentinel 20, un-park 10, task-list closes) so a misparse cannot rewrite a backlog in one tick.
- **Mode thresholds** ([governor-thresholds.md](governor-thresholds.md)) scale with repo count; disk pressure, provider auth failure and restart storms are kick gates ([agent-watchdog.md](agent-watchdog.md), [diskpressure-recovery-runbook.md](diskpressure-recovery-runbook.md)).
- **Loop caps.** Review fix cycles share `escalation.MaxReEngagements`; review-bot threads stop at `review_bots.max_attempts_per_thread` ([review-bot-threads.md](review-bot-threads.md)).

### Alerting and fleet signals

Sentinel findings, trajectory pauses, watchdog crash-loops and `needs-human` routing surface as dashboard banner alerts and, when configured, [notifications](notifications.md). Hub-side, [fleet-drift-signals.md](fleet-drift-signals.md) and [fleet-health.md](fleet-health.md) flag spokes missing `NET_ADMIN`, running the proxy in advisory mode, or on stale images; `src/pkg/spokealerts` carries spoke-raised alerts to the hub.

---

## 9. Platform and supply chain

### Dashboard access control (hub route)

Hives hosted on or registered with the Hive Hub are reached through a per-hive hostname (`<hive-id>.<cluster-domain>` — `*.hive.hivecommons.dev` on the default cluster, other domains on vanity/partner clusters), and every request passes the hub's authentication check before it reaches a spoke dashboard:

- **Multi-provider sign-in.** Users sign in to the hub with GitHub OAuth or Google — with IBMid, Red Hat, Microsoft Entra ID, and a generic OIDC provider available via configuration. With more than one provider configured, `/login` shows a provider picker; a single configured provider goes straight through. The GitHub login deliberately requests **no OAuth scope at all** — the hub only needs to learn *who* is logging in, not to act on their behalf. The session cookie is `HttpOnly`, `Secure`, `SameSite=Lax`, and session cookies are **Ed25519-signed**; the legacy symmetric (HMAC) session lane was deleted on v4.
- **Per-user, per-hive authorization on every request.** The hub's auth-check endpoint (wired as an nginx `auth_request` / `auth-url`) resolves the requesting user, looks up whether that specific hive is in the user's access map, and returns **403 if it isn't**. Only then does the request proceed to the spoke.
- **Unauthenticated page vs. API requests.** A signed-out (or expired) *page* navigation is redirected to the hub login (`auth-signin`). The dashboard's own XHR routes under `/api/` are served by a separate `hive-api-xhr` Ingress with the same auth gate but **no** login redirect: `fetch()` cannot follow a cross-origin redirect, so an expired session there answers a real `401` with a JSON body (`{"ok":false,"error":"unauthorized",…}`, served by `hive-error-pages`) that the dashboard turns into one *sign in again* prompt instead of rendering empty data. `/api/v1` (bearer-token API) keeps its own ungated Ingress. `/api/contribute` (public) keeps its own Ingress too, but that one *does* ask the hub who is calling (`auth-url`, no `auth-signin`): the hub answers `200` for a public path whether or not the caller is signed in, and when they are it still sets `X-Hive-User` / `X-Hive-Role` / `X-Hive-Proxy-Auth`, so a signed-in visitor's own contribution shows on `/contribute/operations` while anonymous access to the leaderboard and the contributor relay is unchanged (#7453). A signed-in user with no grant on the hive is forwarded with role `read`, which grants nothing beyond anonymous on a public path; gated paths still refuse them. That Ingress is provisioned per hive, so the dashboard does not depend on it: it sends its own same-origin `/api` requests with `redirect: 'manual'` and converts the resulting `opaqueredirect` into a `401 {"error":"session_expired","sign_in":"/"}` itself, which keeps an expired session readable on a hive that predates the Ingress or behind any other redirecting proxy (#7433). The one exemption is `GET /api/gh-user-auth/session`, the device-flow landing, whose own redirect back to `/` is its success path.
- **Identity injection.** On success, the hub injects the verified identity into the proxied request as `X-Hive-User` and `X-Hive-Role` headers (alongside a proxy-auth proof the spoke re-verifies). The spoke's role-enforcement middleware then blocks all write operations for `read`-role users.
- **Roles.** The hive owner has full read-write control and can grant other GitHub users access to their hive; grants carry one of the `read` / `read-write` / `merger` / `owner` tiers. A granter-ceiling that forbids granting a role at or above the granter's own applies on the **access-request approval** path (and exempts hub admins); the owner's direct-grant path validates the role but does not itself cap it, so grant access deliberately.


### Direct-route spoke authentication

Some hives run on clusters the hub cannot proxy (for example, an OpenShift route reached directly). These spokes cannot rely on the hub's auth check, so the dashboard enforces its own per-user authentication:

- **GitHub device-flow login with a per-hive allowlist.** At provisioning, the hub injects the authorized user list (owner plus any grants) into the spoke as `HIVE_AUTHORIZED_USERS`. When someone completes device-flow login, the spoke validates the GitHub identity and checks it against the allowlist — a non-allowlisted user is **rejected before any session or token is persisted** (the device flow returns an explicit denial; the identity is never logged in).
- **Per-user opaque sessions.** Each authorized visitor gets their own session: a 256-bit cryptographically random session ID in an `HttpOnly` cookie, mapped server-side to their username and role. Two people viewing the same dashboard each see their own identity. The session deliberately does **not** store the user's GitHub token.
- **Forged identity headers are stripped.** On direct routes there is no trusted hub in front, so incoming `X-Hive-User` / `X-Hive-Role` headers are deleted by the authentication middleware (which runs outermost) before any handler sees them. A client cannot claim an identity by setting headers.
- **The shared bearer token grants no identity.** On direct-route spokes, the legacy shared dashboard token path is disabled for user authentication, and the endpoint that would expose it returns 404. Possessing the token does not let you impersonate a user.
- **The device-flow login persists no GitHub write credential — for anyone.** The spoke does not store or install the login token as a hive write identity, so a read-only viewer's login (or logout) can never touch the owner's stored write credentials. The device flow can therefore request no scope at all.
- **Grant roles are enforced, not flattened.** Each non-owner grant is provisioned into `HIVE_AUTHORIZED_USERS` with the **exact role the hub recorded** (`read`, `read-write`, or `merger`), so a direct-route spoke enforces the same tiers as a hub-proxied one; an unparseable role fails safe to `read`.


### Hosted-platform isolation

On the hosted platform, each hive is single-tenant by construction:

- **One namespace, one pod, one PVC per hive.** Provisioning creates a dedicated `hive-hosted-<id>` namespace with its own PVC and a single-replica Deployment. There is no shared runtime between customers' hives.
- **Least-privilege RBAC.** The per-hive Role is namespace-scoped and limited to the hive's own `hive-secrets` Secret by name — a hive pod cannot read other namespaces' secrets.
- **Authenticated internal API.** Server-to-server calls inside the pod (the dashboard proxy to the Go API) carry an internal auth token header validated with a constant-time comparison; security headers (CSP, `X-Frame-Options`, etc.) are always active on the dashboard, and an optional bearer `auth_token` can be required on all API endpoints.
- **Zero-downtime upgrades.** Hosted hives roll updates with `maxSurge=1` / `maxUnavailable=0` on ReadWriteMany PVCs, so upgrades don't create a window where a half-started replacement serves requests.


### Sessions and SSO are Ed25519-only

Hub-issued session cookies and the hub→spoke SSO handoff are verified with **Ed25519 signatures only**. The legacy symmetric (HMAC) session-cookie lane was deleted, and the fleet-wide shared heartbeat bearer was replaced by per-hive bearers. There is no HMAC fallback for sessions or SSO on v4: a spoke that only holds the retired symmetric `HIVE_SSO_KEY` fails closed.

What that looks like when misconfigured:

- **Spoke has no verification key at all** → SSO renders an error page with HTTP 503: *"This hive has no hub SSO verification key configured…"* and suggests setting `HIVE_HUB_SECRET` (or `HIVE_SSO_PUBLIC_KEY`). Direct GitHub login remains available.
- **Key present but handoff rejected** → HTTP 401 (*expired, malformed, or issued for a different hive*). The spoke log line `sso handoff rejected` has the specific reason; the response body deliberately does not.


### Per-hive derived keys

Every spoke uses keys derived per hive from the hub master secret. The hub reconciles five env vars onto **hosted** spoke Deployments automatically (a 15-minute sweep):

`HIVE_HEARTBEAT_KEY`, `HIVE_SSO_PUBLIC_KEY`, `HIVE_SESSION_PUBLIC_KEY`, `HIVE_TERMINAL_KEY`, `HIVE_INVITE_KEY`

**Self-hosted spokes are never touched by that sweep** — and do not need to be. Every resolver falls back to deriving the same domain-separated key from `HIVE_HUB_SECRET` + `HIVE_ID`, so a self-hosted spoke that sets those two is byte-identical to a hub-injected one. Setting the five vars explicitly is the least-privilege alternative (the spoke then never holds the master).

The symmetric `HIVE_SESSION_KEY` that used to sit alongside `HIVE_SESSION_PUBLIC_KEY` is gone (issue [#3234](https://github.com/hivecommons/hive/issues/3234)): every lane that ever verified against it — the legacy HMAC cookie lane ([#3725](https://github.com/hivecommons/hive/pull/3725)), the terminal-key fall-through, and the Node proxy's copy — was already deleted, so shipping it at all was unjustified secret exposure. The sweep now actively **strips** it from any Deployment still carrying one, rather than merely no longer adding it.

Two gotchas:

- **Empty is worse than absent.** The resolvers fall back on empty values just like missing ones, but an empty var can make convergence accounting report the hive as converged when it is not. Unset rather than blank.
- Per-hive env material derives from the **current** master generation — relevant during rotation, below.


### Provisioning-template changes reach existing spokes only through a reconcile

The provisioning template (`k8sManifestTemplate` in `src/pkg/hub/saas_provision.go`) is `kubectl apply`ed once, when a hive is provisioned. A change to it is born onto every spoke created afterwards and reaches **no spoke that already exists** — those keep whatever object the template rendered on their day. The per-hive env sweep above, the NET_ADMIN sweep, and the vanity-host patch are the reconcilers that close such gaps for the objects they own; a change to any other existing object needs one too, or the PR must say that existing spokes have to be re-provisioned.

The case that made this rule: [#7457](https://github.com/hivecommons/hive/pull/7457) added `auth-url` / `auth-response-headers` to the `hive-contribute` Ingress so a signed-in visitor's identity reaches `/api/contribute/me`. The code half rolled out with the next image; the Ingress half reached only newly provisioned spokes, and `/api/contribute/me` kept answering `401` everywhere else ([#7517](https://github.com/hivecommons/hive/issues/7517)). The hub now reconciles those two annotations onto every hosted spoke's `hive-contribute` Ingress on nginx clusters (a 15-minute sweep, `contribute_ingress_reconcile.go`; a merge patch on the annotations, which rolls no pod). The same sweep converges the `hive-terminal` Ingress (`auth-url`, `auth-signin`, `auth-response-headers`), which drifted when the hub's public host changed and left `/terminal` answering `500` on older spokes. OpenShift-Route clusters have no nginx Ingress and are skipped.


### Master key rotation

The hub master secret supports **generations**: at most two live at once — one CURRENT (mints new material) and one PREVIOUS (verify-only, default window 7 days). Design details: [design/master-key-rotation.md](design/master-key-rotation.md).

- **Trigger:** `POST /api/saas/admin/rotate-master-key` (admin-only; refused while an impersonation grant is active; a second rotation within the 8-hour cooldown returns 409 with `retry_after_seconds`). Inspect with `GET /api/saas/admin/key-generations` and `GET /api/saas/admin/auth-rollout`.
- **Rotation is not complete when the endpoint returns 200.** Spokes converge via the per-hive env reconcile at 3 Deployment patches per 15-minute cycle — roughly 6 hours for a ~70-spoke fleet — and **each patch rolls that spoke's pod once** (interrupting running agents there). Plan rotations accordingly.
- **Dual-generation acceptance** during the window: session cookies, heartbeat bearers (bounded trial verification), and SSO/session public keys (current + previous) all verify against both generations. **Terminal and invite keys have no dual lane** — in-flight invite links are invalidated by a rotation.
- **Alerts:** `warn` means the previous generation expires within 24h while spokes still carry it — convergence can still finish in time. `stranded` means the window already closed while spokes carry the old generation — those spokes are now failing verification; converge or re-provision them. Retirement is never blocked by an unconverged spoke (otherwise one unreachable cluster could pin the old key alive forever); the alerts are what make it loud.
- **Do not "rotate" by setting a fresh master directly** (new `HIVE_HUB_SECRET` / replacing `/data/saas/hub-secret.key`): that discards all previous generations and strands every spoke at once. The only converging path is the rotate endpoint.
- Self-hosted spokes converge **manually** — the operator re-derives or re-sets their keys; no sweep reaches them.


### Inbound webhooks and internal auth

- **GitHub webhooks are HMAC-verified.** Both receivers (`src/pkg/dashboard/api_github_webhook.go` on a spoke, `src/pkg/hub/webhook.go` on the hub) compute `hmac.New(sha256, GITHUB_WEBHOOK_SECRET)` over the raw body and compare against `X-Hub-Signature-256` with a constant-time compare; with no secret configured the path logs a warning and **rejects** rather than trusting unsigned events ([webhooks.md](webhooks.md)). The Linear agent's webhook (`src/pkg/linearagent/webhook.go`) does the same.
- **Hub proof headers.** `X-Hive-Proxy-Auth` carries the hive's own dashboard token as the spoke's self-service proof to the hub, and the hub's identity injection into proxied requests is paired with the same proof (`src/pkg/hub/saas_auth.go`, `src/pkg/dashboard/server.go`); every bearer/token compare in `src/pkg/dashboard/api_auth.go`, `src/pkg/dashboard/server.go` and `src/pkg/hub/server.go` uses `subtle.ConstantTimeCompare`.
- **Terminal access** uses short-lived signed assertions (`src/pkg/terminalassert`, `src/pkg/hub/spoke/terminal_assertion.go`) and per-hive authorization.
- **Systemd units refuse planted checkouts.** `bin/hive-checkout-guard.sh` refuses to execute code from a directory an unprivileged local user could have created (#5435).

### Dashboard security headers

`src/pkg/dashboard/server.go` always sets CSP, `X-Frame-Options` and friends (covered by `src/pkg/dashboard/security_headers_test.go`). `style-src` scope is ADR-0015; `script-src` is reduced to `'self'` plus approved hashes computed by `src/pkg/dashboard/webstatic/csp_script_src.go` (ADR-0016) — no `unsafe-inline`, no nonces. An optional bearer `auth_token` can be required on every API endpoint; `/api/v1` uses its own bearer.

### Metrics endpoint

`GET /metrics` (Prometheus text format, on the dashboard port) is off unless `HIVE_METRICS_ENABLED` is truthy (`1|true|yes|on`); disabled means the route is not registered (scrapes 404). Because Prometheus cannot do device-flow auth, an enabled `/metrics` bypasses dashboard auth, so the bearer token is its only guard — and it **fails closed**: `HIVE_METRICS_TOKEN` is mandatory whenever metrics are enabled. Enabled-but-tokenless returns 403 (with an error body naming both variables, plus a startup warning); a wrong/missing bearer returns 401. The exposed series are business-sensitive: cumulative estimated cost per model/agent (`hive_estimated_cost_usd*`) and token totals per model.


### Supply chain

- **npm:** all global AI-CLI installs (claude-code, codex, copilot, goose-adjacent tooling, etc.) run with `--ignore-scripts`.
- **Base images digest-pinned:** `golang`, both `node` stages, and the contributor image's `debian:bookworm-slim` are pinned by digest; the vLLM inference image is digest-pinned with a model checksum. Refresh procedure is documented at the top of `src/Dockerfile`.
- **Tools pinned by SHA:** tmux, ttyd, gh, goose, su-exec (by commit), and friends are checksum-verified at build.
- **Workflows:** `actions/checkout` is SHA-pinned.
- **CODEOWNERS:** `.github/CODEOWNERS` covers the security-sensitive paths (Dockerfiles, workflows, deploy manifests, launch scripts, key/cookie code). It is **advisory until "Require review from Code Owners" is enabled** in branch protection — deliberately left off because the repo's automation merges green PRs without human review; enabling it is an operator/repo-settings choice.


CI enforces the above and more on every PR:

| Guard | Workflow / script | Protects |
|---|---|---|
| `govulncheck`, `gosec`, `golangci-lint` (separate jobs so one cannot hide another) | `.github/workflows/go-security-analysis.yml` | known-vulnerable calls, insecure patterns |
| OpenSSF Scorecard | `.github/workflows/scorecard.yml` | repo hygiene regression |
| Dependabot | `.github/dependabot.yml` | stale Go/npm/Action deps |
| Third-party Actions pinned to 40-hex SHAs | `src/scripts/check-action-pins.sh` (run from `go-security-analysis.yml`) | Action tag hijack |
| SUID inventory == exactly `4750 root:hive-launch su-exec` | `.github/workflows/suid-contract.yml`, `src/scripts/check-suid-contract.sh`, `src/deploy/test_image_suid_inventory.sh` | setuid escalation from an agent UID |
| Image attestations deliberately off, and guarded so a bump cannot silently turn them on | `.github/workflows/image-attestation-guard.yml`, `src/scripts/check-no-image-attestations.sh`; `docker.yml` sets `provenance: false`, `sbom: false` | multi-arch manifest breakage (#13266 class); see gaps |
| DCO trailers, squash sign-off attribution | `src/scripts/check-dco-trailers.sh`, `src/scripts/check-squash-signoff-attribution.sh`, `.github/workflows/dco-*.yml` | unattributed commits |
| Docs links and `file:line` citations resolve | `.github/workflows/docs-link-check.yml`, `src/scripts/check-docs-links.py`, `src/scripts/check-docs-citations.py` | this page drifting from the code |
| Formal models (Promela/Spin) for escalation and leases | `.github/workflows/formal-verify.yml`, [formal-verification.md](formal-verification.md) | invariant regressions |
| Podman/quadlet contract, release-line guard, changelog fragment guard | `podman-contract.yml`, `release-line-guard.yml`, `changelog-fragment-guard.yml` | deployment and release drift |

---

## 10. Human-in-the-loop and governance

- **ACMM is the autonomy dial** (ADR-0003, [acmm-policy-matrix.md](acmm-policy-matrix.md)). Six levels; merge permission is simply not minted below L6 — token tier and proxy rules enforce it regardless of labels. L5 adds a literal `hold` on every agent PR so humans batch-approve.
- **Parking and un-parking.** An agent that needs a decision parks the item (`needs-human`, `stage/045-needs-decision`) and stops; agents never re-dispatch onto a `needs-human` item. A person with `write`+ answers with `/hive approve` or `/hive decision <text>` (adds `approved-direction`); Hive lifts a `needs-human` it added itself and **never** lifts one a person added, and never removes `hold` ([maintainer-commands.md](maintainer-commands.md), #12680, #13075). Escalation-over-stalling (ADR-0019) guarantees the item moves to a person rather than waiting forever.
- **PR release is a GitHub review, not a comment.** A `requires_human` verdict is cleared only by a trusted merger's **Approve** review on the current head; `/hive approve` on a PR is acknowledged and explained, never consumed.
- **Owner-only security settings.** `PUT /api/config/governor/security` (ioscan, intent enforcement, review requirements, agent sandbox) requires `owner` (`src/pkg/dashboard/api_governor_security.go`); every sibling governor-config handler calls the same `requireOwnerRole`.
- **Tool-approval desk** (default-off, [design/tool-approval-desk.md](design/tool-approval-desk.md), `src/pkg/toolapprove`): one `(action, ACMM level, identity) → verdict` function for approval-shaped requests, with an audited reason per decision.
- **Emergency stops**, cheapest first: pause one agent (`POST /api/pause/<name>`, `hivectl`/TUI pause), pause one repo (`project.paused_repos`), disable automerge per repo (`repo_policies[].auto_merge: false`), freeze the watchdog fleet-wide (`HIVE_WATCHDOG_PAUSE=true`), drop the ACMM level (`hivectl` ACMM apply) — none of which require a redeploy.
- **Reviewed hooks.** State-triggered [hooks](hooks.md) run only vetted actions from a fixed set; the hook model is in that page.
- **Governance.** Maintainer Committee, response SLAs and escalation path are in [security-response.md](security-response.md); vulnerability intake is below.

### Advisory digests

Agent findings are posted as a single, continuously rewritten tracking-issue comment per repo. Each digest is **pinned to one analyzed commit**: the footer cites `owner/repo@<sha>` (branch included), and any finding whose file path no longer exists at that commit is flagged *"file path not found at analyzed commit — finding may be outdated"* instead of showing a dangling `file:line`. Inconclusive path checks (rate limits, transient errors) fail open — a finding is only flagged on a definitive 404.


---

## 11. What hive does not do yet

Security documentation that only lists strengths is marketing. The current, deliberate scoping:

- **Rootless containers cannot enforce the egress gate.** The forced-proxy egress redirect needs `CAP_NET_ADMIN` + iptables; a rootless podman spoke only starts with `HIVE_PROXY_ADVISORY_OK=true`, i.e. with network-layer enforcement advisory-only.
- **The policy proxy inspects GitHub API traffic.** All outbound `:443` is force-redirected through it, but non-`api.github.com` destinations are tunneled without inspection — it is a GitHub-policy enforcement point, not a content firewall for arbitrary hosts. If you need full egress control, apply Kubernetes NetworkPolicies around the hive pod.
- **The L5 `hold` label is applied by agent policy;** the hard guarantee at L5 is the withheld merge permission (token tier + proxy rules), not the label itself.
- **Cloud CLI credentials are shared with the CLI.** Claude/Copilot/Codex/Antigravity/Goose agents necessarily run with the provider credential you connected. The placeholder-key isolation applies to inference-gateway keys.
- **DCO is policy-driven** inside hive; enforce it repo-side (DCO check) for a hard guarantee.
- **Shared-container execution remains a material risk.** Per-UID isolation reduces cross-agent access but is not equivalent to per-run containers or microVMs; see the residual-risks discussion in [security-threat-model.md](security-threat-model.md).

Review evidence: the per-PR [review evidence bundle](review-evidence.md) is a hashed, optionally Ed25519-signed record of what Hive reviewed and why it merged. It is evidence, not certification; see the [SOC 2 control mapping](soc2-control-mapping.md) for what it does and does not show.


Further gaps a reviewer should know about:

- **Context reset is not process reset.** `/clear` empties the model context; the backend CLI process and its on-disk session state persist until a restart.
- **Agent sandbox is opt-in and two-gated.** Most hub-path agents run in the shared pod under per-UID separation, not in a per-kick container.
- **No image signing or SBOM.** Attestations are deliberately off (`image-attestation-guard.yml`) because they broke multi-arch manifests; images are digest-pinned at build but not signed at publish.
- **No CodeQL / container CVE scan workflow** in this repo's CI (govulncheck, gosec and Scorecard run; the sentinel treats CodeQL config edits as sensitive, but the scan itself is not configured here).
- **Branch protection is GitHub's, not hive's.** Hive does not configure or verify rulesets; it relies on them being set.
- **Gate-integrity invariants are a proposal.** Agent force-push / history-rewrite refusal is designed ([design/gate-integrity-invariants.md](design/gate-integrity-invariants.md)), not enforced; hold guard catches the drift after the fact.
- **Hub registration has no review step.** Any spoke with the derived keys can register; there is no "reviewed hub" allowlist.
- **Trust tiers do not decay.** A `trusted` contributor stays trusted until an operator changes it.
- **Trajectory and semantic ioscan fail open** by design; the deterministic floor (rules, canaries, proxy, token tiers) is what holds when a model is down.

---

## 12. How to audit a running hive

Use an owner-authorized session for protected dashboard routes (dashboard on `:3001` by default; `hivectl` reads `HIVE_DASHBOARD_TOKEN`). Keep production inspection read-only. Do not paste tokens, process environments or Secret contents into issue reports; inspect deployment YAML privately and redact literal environment values before sharing. Do not grant agents new privileges just to run an audit.

| Question | Check |
|---|---|
| Is the egress gate enforcing? | Pod log at boot: no `HIVE_PROXY_ADVISORY_OK` WARN; container did not exit `77`; `iptables -t nat -L HIVE_PROXY` inside the pod lists the `:443` REDIRECT |
| Are agents separated? | `cat /var/run/hive/uid-map.json`; `ls -l /usr/local/bin/su-exec` → `-rwsr-x--- root hive-launch`; `find / -perm /6000 -type f` lists only `su-exec` |
| Does an agent hold a usable token? | `GET /api/config/governor` → `security.credentialInjection` and `security.credentialWarnings`; compare the boot receipt without printing agent tokens |
| Can an agent write outside the allowlist? | Inspect configured repositories and existing proxy-denial receipts; test writes only in a disposable evaluation hive |
| Is ioscan on and in which fail mode? | `GET /api/config/governor` → `security.ioscanEnabled`, `ioscanFailMode`, `ioscanCanaries` |
| Is trajectory review on? | `GET /api/config/governor` → `trajectory`; Prometheus `hive_trajectory_reviews_total` (needs `HIVE_METRICS_ENABLED` + token) |
| Is the sentinel on? | Inspect `sentinel` config and recent findings/comments with the configured alert label; verify actual sweep execution, including trusted-author exceptions |
| Who merged what, and why? | `GET /api/audit` (newest 200; `?sensitive=1` for the 24 h sensitive window); `hivectl evidence export --repo <o/r> --since <date>`; `GET /api/evidence/summary` |
| Who ran a token-bearing command? | `/var/run/hive-metrics/token-access.jsonl` |
| What is the merge posture per repo? | `hive.yaml` → `acmm_level`, `project.repo_policies[].auto_merge`, `auto_merge.trusted_authors`, `review.require_approval`, `project.paused_repos` |
| Is the watchdog healing or observing? | `GET /api/agents` → `conditions[]`; `HIVE_WATCHDOG_PAUSE` on the Deployment |
| Are webhooks authenticated? | Confirm `GITHUB_WEBHOOK_SECRET` is configured without printing its value; test unsigned-request rejection only in a disposable evaluation hive |
| Hub key state | `GET /api/saas/admin/key-generations`, `GET /api/saas/admin/auth-rollout` (hub admin) |
| Dashboard headers | `curl -sI https://<hive>/ \| grep -i 'content-security-policy\|x-frame-options'` |
| Is the image what CI built? | `docker.yml` run → GHCR digest == running `imageID`; `src/deploy/test_image_suid_inventory.sh <image>` |
| Compliance posture | [compliance.md](compliance.md) maps these controls to SOC 2; `GET /api/compliance` is not an endpoint — the mapping is documentation |

### Concrete denial checks in a disposable evaluation hive

Keep the runnable checks below out of production. Use an operator-controlled test repository and disposable resources: if enforcement is broken, a probe can create an issue or deliver a webhook. Never print or publish credentials.

| Question | Evaluation check |
|---|---|
| Does an agent hold a usable token? | With injection enabled, check privately that `GH_TOKEN` has the `hive-proxy-injected-` prefix without printing it; a request to `https://api.github.com/user` authenticated with that placeholder must fail. |
| Can an agent write outside the allowlist? | From a test pane, `gh api -X POST repos/<operator-owned-test-repo>/issues -f title='disposable denial probe'` must return `403` with `X-Hive-Proxy-Blocked` when that repository is outside the configured allowlist. |
| Is the sentinel on? | With sentinel enabled, open a disposable workflow-change PR from a non-exempt test author; expect the configured alert label (default `sentinel-alert`) within one sweep. |
| Are webhooks authenticated? | With `GITHUB_WEBHOOK_SECRET` configured, `curl -i -X POST -H 'Content-Type: application/json' --data '{}' http://<evaluation-hive>:3001/api/webhook/github` without a signature must be rejected. |

### Read-only walkthrough

1. **Record identity and effective posture:** read `GET /api/status`, `GET /api/config/governor` and `GET /api/agents`. Record version, level, repository scope and enforce/shadow settings; compare persisted config, deployment and per-repo overrides ([config layering](config-layering.md)).
2. **Inspect deployment boundaries:** from an authorized operator shell inspect namespace RBAC, process UIDs, launcher/key permissions and the `HIVE_PROXY` iptables chain together. For non-Kubernetes installs inspect the equivalent container spec and startup logs.
3. **Trace one real PR:** compare the current head, native reviews/checks, holds and stage with the private merge-eligibility, review and intent records under `/var/run/hive-metrics`. Retrieve the matching [review evidence bundle](review-evidence.md), verify hashes and configured signatures, and distinguish live from backfilled records. Absence is not a pass.
4. **Check detection receipts:** read `GET /api/audit` and `GET /api/watchdog/activity?days=30`. Distinguish observed from acted watchdog events; check recent trajectory reviews, sentinel findings and scan blocks. A toggle without recent execution is not evidence of detection.
5. **Check external enforcement:** inspect repository rulesets, App installation scope, required CI/DCO/code-owner reviews and network policy. Check hub key rollout separately: a healthy spoke does not prove fleet convergence.
6. **Test denial only in a disposable evaluation hive:** verify read-role and out-of-scope writes are rejected and changed heads invalidate approvals as documented. Never probe production with destructive operations or by relaxing gates. Keep sanitized receipts with the version and policy snapshot.

## Compliance frameworks

Hive holds no SOC 2, FedRAMP, ISO 27001 or other certification. The layers above are the controls an operator's own control owners can point to; [Compliance controls](compliance.md) maps them to framework requirements (SOC 2 Type II today), reports whether the live config is in a profile's recommended posture, and lists the requirements Hive cannot cover.


## Reporting vulnerabilities

Do not report suspected vulnerabilities in public issues, pull requests, or discussions. Follow the repository Security Policy: use GitHub private vulnerability reporting from the repository **Security** tab, or contact a maintainer directly if private reporting is unavailable. Include the affected component, branch/commit, impact, reproduction steps, and any relevant logs or configuration.

