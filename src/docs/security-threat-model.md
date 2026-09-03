# Hive security threat model

This document describes the public threat model for Hive (current line: branch
`v4`; the `v2` branch is retired) as documented
in the [reference architecture](architecture.md), the
[ACMM policy matrix](acmm-policy-matrix.md), and the current implementation. It
focuses on the agent-orchestration boundary: untrusted GitHub work enters Hive,
AI agents act on it, and deterministic Hive components constrain what can leave.

## Assets and trust boundaries

### Assets

- **GitHub authority:** GitHub App private key material, per-agent App
  installation tokens, user/device-flow tokens, PR merge authority, and repo
  write permissions. Architecture §5 describes scoped App tokens and the `gh`
  wrapper; §11 shows PR creation/merge as the high-impact path.
- **Repository integrity:** source code, branches, PRs, reviews, labels, and the
  merge-eligible artifact produced by the deterministic pipeline (§4).
- **Agent work state:** tmux sessions, transcripts, bead ledgers, pipeline JSON,
  and per-agent mode files (`/tmp/.hive-mode-<agent>`) described in §2, §7, and
  the data-store table.
- **Operator and contributor access:** dashboard/API access, hosted terminal
  access, hub/spoke heartbeats, ClankeR contributor relay, and hub callbacks
  (§1, §8, §10).
- **Model/backend credentials and cost controls:** inference keys, token usage
  JSONL, Copilot usage sniffing, and the seven-day token budget (§3, §9).

### Trust boundaries

- **GitHub issue/PR/comment text → agent prompt.** Public issue authors can
  place arbitrary text in titles, labels, bodies, and comments that Hive may
  include in a kick. This is the main prompt-injection boundary.
- **Agent process → GitHub.** Agents run in tmux sessions (§2). Their GitHub
  writes cross the CLI wrapper, scoped credentials, and MITM proxy guardrails
  (§4-§5).
- **Agent process → local hive host/container.** Agents share a Hive container
  process model (§2), optionally with per-agent OS users. UID attribution is
  used by the proxy to map connections to agent modes (§5).
- **Spoke → hub.** Spokes post heartbeats to the hub and receive callbacks such
  as GitHub App configuration, banners, branch switches, and authorized users
  (§8).
- **Contributor relay → spoke.** Contributors donate compute through ClankeR;
  the architecture states their credentials never leave their machine (§8).
- **Browser/dashboard/terminal → hive API and ttyd.** The nginx gateway
  (`deploy/nginx.conf`) is the public front door in Compose deployments, in
  front of the Go dashboard; terminal WebSocket traffic passes through to ttyd
  (§2, §10).

## Threat actors and abuse cases

| Actor | Representative threats |
| --- | --- |
| Malicious issue or PR author | Prompt injection; hidden unicode or base64 instructions; labels that steer routing; instructions to reveal prompts, disable guardrails, exfiltrate tokens, or merge attacker-controlled PRs. |
| Compromised or misaligned agent runtime | Attempts to bypass CLI tool-deny, call GitHub APIs directly, use GraphQL mutations, push to non-allowed repositories, merge outside the merge gate, or continue after drifting from its assigned intent. |
| Compromised contributor relay/client | Attempts to claim work with more trust than earned, leak contributor credentials, or use donated compute to affect repos outside the allowed set. |
| Hub compromise or confused-deputy bug | Malicious callbacks, unauthorized terminal access, SSO token forgery, cross-tenant exposure of App keys or heartbeat data. |
| Token exfiltration attacker | Reads live tokens from agent env/session/files, logs, PR bodies, terminal sessions, hub heartbeats, or agent output. |
| Network or proxy bypass attacker | Sends GitHub traffic around the MITM proxy, exploits opaque `github.com` tunneling, or abuses model/backend network paths for exfiltration. |

## Defense layers mapped to threats

Hive uses defense in depth: deterministic Go/shell controls make policy
before and around LLM judgment. The table below maps the documented layers to
the threats they reduce.

| Layer | How it works | Threats reduced | Evidence |
| --- | --- | --- | --- |
| Deterministic pre-kick pipeline | `run-pipeline.sh` enumerates, classifies, detects clusters/architecture impact, and writes `merge-eligible.json` before agents see work. | Reduces agent discretion over what is actionable and what can merge. | [Architecture §4](architecture.md#4-the-deterministic-pipeline) |
| ACMM autonomy dial | Human-selected ACMM level maps to per-agent modes from advisory through full autonomy; supervisor/brainstorm stay advisory. | Limits damage from a compromised agent by constraining issue/PR/merge capabilities. | [Architecture §6](architecture.md#6-acmm--controlling-agent-autonomy), [policy matrix](acmm-policy-matrix.md) |
| CLI tool-deny | Agents are launched with backend-specific denied/disallowed tools based on mode. | Blocks obvious local-tool routes before credentials or network are involved. | [Architecture §5](architecture.md#5-layered-guardrails-defense-in-depth) |
| Scoped per-agent App tokens | Mode maps to token tiers (`advisor`, `newcomer`, `contributor`, `trusted`); tokens are least-privilege and per-agent rather than one shared full token. v4 hardened token handling so agents do not receive the full installation token via `HIVE_GITHUB_TOKEN` (#2760), and shared token cache files are owner-only/fail-loud (#2747). | Limits token exfiltration and confused-deputy writes. | [Architecture §5](architecture.md#5-layered-guardrails-defense-in-depth), `pkg/agent/mode.go`, security commits #2760/#2747 |
| MITM proxy mode rules | `api.github.com` traffic is intercepted; first-match REST rules and GraphQL mutation classification require minimum modes. Blocks return `403` with `X-Hive-Proxy-Blocked`. `github.com` is tunneled opaquely for OAuth/git smart HTTP. | Blocks direct PR writes, issue writes, GraphQL mutation bypasses, and mode escalation over the network. | [Architecture §5](architecture.md#5-layered-guardrails-defense-in-depth), `pkg/proxy/rules.go`, `pkg/proxy/github_proxy.go` |
| Hard-deny relays for REST PR create/merge | REST `POST /pulls` and `PUT /pulls/{n}/merge` are hard-denied for every agent mode; agents must use `hive-open-pr`/`hive-merge` so Hive performs App-bot authorship and SHA/eligibility binding. Merge authorization was hardened to target and SHA (#2670, #2680). GraphQL mutations are still mode-classified by capability tier. | Prevents an otherwise merge-capable agent from bypassing the REST merge relay or authoring REST-created PRs as the login user. | `pkg/proxy/rules.go`, [Architecture §4](architecture.md#4-the-deterministic-pipeline), #2670/#2680 |
| Repo allowlist | The proxy permits writes only to configured repos; seed prompt/repo allowlists come from trusted config. | Prevents compromised agents from writing to arbitrary repositories reachable by a credential. | [Architecture §5](architecture.md#5-layered-guardrails-defense-in-depth), `pkg/proxy/rules.go` |
| ioscan redaction and semantic classification | `pkg/ioscan` is stdlib-first and scans input/output text for prompt injection, dangerous directives, secrets, zero-width characters, base64-hidden instructions, and high-entropy tokens. Scheduler input enforcement defaults on and replaces blocked untrusted text with an explicit marker while audit logging the rule. An optional LLM-judge classifier scores already-redacted untrusted segments for plain-English semantic injections; classifier outages fail open, while successful high-risk scores redact in open mode or block in fail-closed mode. v4 made ioscan default-on/broader (#2666) and closed the #2805 semantic-injection gap. | Reduces prompt injection and accidental secret emission before text reaches agents or external outputs. | `pkg/ioscan`, `pkg/scheduler/ioscan_enforce.go`, #2666/#2805 |
| Trajectory review | A second model periodically compares each running agent's intent to a bounded tmux transcript tail and pauses or alerts on divergent trajectories. It fails open on reviewer outage. | Catches multi-step goal drift that individual API/tool checks may not detect. | [trajectory-review.md](trajectory-review.md) |
| Token budget | The seven-day rolling token budget warns at 90% and suppresses kicks on exhaustion except for exempt agents. | Limits runaway cost or denial-of-wallet from compromised loops. | [Architecture §3](architecture.md#3-the-governor-loop--from-queue-depth-to-a-kick) |
| Per-UID isolation and attribution | Agents may run as per-agent OS users; the proxy maps connection owner UID to agent name/mode. v4 fixed token/cache permissions and pinned/restricted the SUID `su-exec` helper (#2754). | Limits cross-agent file access and makes network decisions attributable to an agent identity. | [Architecture §2](architecture.md#2-container-process-model), [§5](architecture.md#5-layered-guardrails-defense-in-depth), #2754 |
| SSO and terminal authorization | v4 hardened hosted terminal access with per-hive authorization (#2756), short-lived signed terminal assertions (#2762), and asymmetric Ed25519 SSO handoff (#2771). | Reduces hub/terminal confused-deputy and token-forgery risks. | [Architecture §2](architecture.md#2-container-process-model), #2756/#2762/#2771 |
| Hub secret and heartbeat hardening | v4 domain-separated the hub master secret (#2758) and stopped heartbeats from broadcasting every tenant's App key (#2755). | Reduces blast radius from hub compromise or cross-tenant data exposure. | [Architecture §8](architecture.md#8-hub--spoke), #2758/#2755 |
| Per-hive dashboard/terminal authz | Empty dashboard tokens fail closed and terminal cookie HMACs are verified (#2662); per-hive terminal authz was added in #2756. | Prevents unauthenticated dashboard or terminal access. | [Architecture §10](architecture.md#10-dashboard--observability), #2662/#2756 |

## Residual risks and known gaps

- **Shared-container execution remains a material risk.** Agents run as tmux
  sessions inside one Hive container (§2). Per-UID isolation reduces cross-agent
  access, but it is not equivalent to per-run containers or microVMs. Open issue
  [#2804](https://github.com/hivecommons/hive/issues/2804) tracks credential-free
  sandboxed execution.
- **Live tokens may still exist in-session.** Scoped, short-lived App tokens and
  owner-only caches reduce impact, but current agents can still need live
  credentials to push/open PRs. #2804 proposes moving GitHub writes to a trusted
  post-step so the sandbox holds no live GitHub token.
- **Network access is constrained, not absent.** GitHub API writes are mediated
  by the proxy, but agents still have network paths for allowed GitHub reads,
  model backends, and opaque `github.com` tunneling. #2804 proposes a stricter
  no-network sandbox with the MITM proxy retained as an outer layer.
- **ioscan semantic classification is optional and fail-open.** Deterministic
  rules, Unicode normalization, base64 rescans, canaries, output redaction, and
  fail-closed semantics are the dependable floor. The model-based classifier
  closes the #2805 plain-English semantic-injection gap when enabled, but it
  intentionally fails open on model errors/timeouts to avoid turning reviewer
  outages into scheduler outages.
- **Trajectory review is semantic oversight, not proof.** It reads rendered tmux
  transcripts, not hidden model reasoning, and intentionally fails open on
  reviewer outage. It complements but does not replace structural controls.
- **Hub compromise remains high impact.** v4 reduced token leakage and token
  forgery risks (#2755, #2758, #2771), but the hub still coordinates callbacks,
  registry data, terminal authorization, and fleet visibility (§8).

## Reporting vulnerabilities

Do not report suspected vulnerabilities in public issues, pull requests, or
discussions. Follow the repository [Security Policy](../../SECURITY.md): use
GitHub private vulnerability reporting from the repository **Security** tab, or
contact a maintainer directly if private reporting is unavailable. Include the
affected component, branch/commit, impact, reproduction steps, and any relevant
logs or configuration. Maintainers aim to acknowledge reports within five
business days.
