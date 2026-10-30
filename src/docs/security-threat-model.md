# Hive security threat model

Last reviewed: 2026-10-23

Hive runs AI coding agents against real GitHub repositories. Untrusted text
from GitHub enters Hive, agents act on it, and deterministic Hive components
limit what can leave. This page lists what is worth protecting, where the
trust boundaries are, who might attack, which controls answer each attack,
and what remains open. The control-by-control reference is
[security-model.md](security-model.md).

**Terms for this reference:** a **trust boundary** is where data or authority
moves between components with different permissions. **Egress** is outgoing
traffic. A **UID** is a Unix user ID; **MITM** means man-in-the-middle, here
Hive's request-inspecting proxy. **SSRF** (server-side request forgery) makes a
server contact an unintended destination. **XSS** (cross-site scripting) runs
attacker code in a page's browser context. **CSP** (Content Security Policy)
limits which page resources and scripts may run. Residual risk means what
remains possible even with the listed controls.

This is a threat-reference map, not a setup checklist. A **deterministic**
check follows fixed rules rather than asking a model. A **confused deputy**
misuses authority on another actor's behalf. **Exfiltration** is data leaving
without permission. A **setuid** program runs with its file owner's authority.
**Fail open** continues when a check fails; **fail closed** blocks instead.
Read each control's exceptions and the residual risks, not just its name.

## Assets

* **GitHub authority.** The GitHub App private key, per-agent installation
  tokens, user login tokens, and the ability to open and merge pull requests.
* **Repository integrity.** Source code, branches, pull requests, reviews,
  labels, and the list of pull requests Hive has judged eligible to merge.
* **Agent work state.** Terminal sessions, transcripts, the work ledger, and
  per-agent mode files.
* **Operator and contributor access.** Dashboard and API access, hosted
  terminals, hub-to-spoke messages, and the contributor relay.
* **Model credentials and spend.** Inference keys, token usage records, and
  the seven-day token budget.

## Trust boundaries

* **GitHub text → agent prompt.** Anyone can write an issue, label, body or
  comment that Hive may place in an agent's task. This is the main
  prompt-injection boundary.
* **Agent process → GitHub.** Every GitHub write from an agent passes the
  `gh` wrapper, a scoped token and the policy proxy.
* **Agent process → Hive container.** Agents share one container and one
  kernel, each under its own Unix user ID. The proxy identifies an agent by
  the user ID that owns the connection.
* **Spoke → hub.** Spokes send heartbeats to the hub and receive
  configuration, banners, branch switches and authorized-user lists back.
* **Contributor relay → spoke.** Contributors donate compute through a relay
  client; their credentials stay on their machine.
* **Browser → dashboard and terminal.** An nginx gateway fronts the Go
  dashboard; terminal traffic passes through to `ttyd`.

## Threat actors

| Actor | What they try |
|---|---|
| Malicious issue or pull-request author | Prompt injection; hidden Unicode or base64 instructions; labels that steer routing; instructions to reveal prompts, disable guardrails, leak tokens or merge their pull request. |
| Compromised or misbehaving agent | Bypass tool denials, call the GitHub API directly, use GraphQL mutations, write to repositories outside the allowlist, merge outside the gate, or drift from its assigned task. |
| Compromised contributor relay or client | Claim work above its trust level, leak contributor credentials, or use donated compute against repositories outside the allowed set. |
| Hub compromise or confused-deputy bug | Malicious callbacks, unauthorized terminal access, forged single-sign-on tokens, exposure of one tenant's keys or heartbeat data to another. |
| Token thief | Read live tokens from agent environments, files, logs, pull-request bodies, terminal sessions, heartbeats or agent output. |
| Network bypass | Send GitHub traffic around the proxy, exploit opaque `github.com` tunnelling, or use model-backend connections to exfiltrate data. |

## Controls mapped to threats

| Control | How it works | Threats reduced |
|---|---|---|
| Deterministic pre-task pipeline | Scripts enumerate, classify and mark work merge-eligible before any agent sees it. | Agent discretion over what is actionable and what can merge. |
| ACMM autonomy level | A human picks one of six levels; each maps every agent to advisory, measured, hold-gated or full mode. | Damage from a compromised agent is bounded by its mode. |
| Tool denials at launch | Agents start with backend-specific denied-tool flags based on mode. | Local-tool routes to GitHub writes. |
| Scoped per-agent tokens | Each agent gets its own installation token for its mode; the full App token is never placed in an agent environment. | Token theft and confused-deputy writes. |
| Policy proxy | All `api.github.com` traffic is intercepted; REST method and path rules and GraphQL mutation classes require a minimum mode; blocked calls return `403` with `X-Hive-Proxy-Blocked`. | Direct pull-request and issue writes, GraphQL bypasses, mode escalation. |
| Hard-denied pull-request create and merge | `POST /pulls` and `PUT /pulls/{n}/merge` are denied for every mode. Agents use `hive-open-pr` and `hive-merge`, which Hive performs as the App with the head commit bound. | A merge-capable agent bypassing the merge gate or authoring pull requests as a user. |
| Repository allowlist | The proxy allows writes only to configured repositories. | Writes to any other repository a token could reach. |
| `ioscan` | Cleans and matches untrusted text, withholds matches, plants canaries and scans agent output for them. See [ioscan.md](ioscan.md). | Prompt injection and secret leakage before and after the agent. |
| Trajectory review | A second model compares each agent's assigned intent with its recent transcript and pauses it on drift. Fails open if the reviewer is down. | Multi-step goal drift. |
| Token budget | A seven-day budget warns at 90% and stops new tasks when exhausted. | Runaway cost. |
| Per-user-ID isolation | Each agent has its own Unix user, terminal socket and token file; the proxy attributes connections by user ID. `su-exec` is the only setuid binary, mode 4750. | Cross-agent file access; unattributed network decisions. |
| Single sign-on and terminal authorization | Ed25519-signed sessions, short-lived signed terminal assertions, per-hive authorization. | Forged hub or terminal access. |
| Hub secret and heartbeat hardening | Per-hive derived keys; heartbeats carry telemetry only, never credentials. | Blast radius of a hub compromise; cross-tenant exposure. |
| Dashboard authorization | Empty dashboard tokens fail closed; webhook signatures and terminal cookies are verified. | Unauthenticated dashboard or terminal access. |

## Residual risks

* **Shared container.** Agents are separated by user ID, not by container
  or virtual machine. An opt-in sandbox mode
  ([#2804](https://github.com/hivecommons/hive/issues/2804), shipped) runs
  each task in its own rootless container with no credentials, but it is
  not the default. Stronger isolation options, including OpenShell, are
  under evaluation. A separate, opt-in contributor-side krun microVM
  (`HIVE_CONTAINER_OCI_RUNTIME=krun`, see
  [contributor-relay.md](contributor-relay.md) "Hardened mode") does not
  change the isolation of the hive's own agents.
* **Live tokens in session.** Agents that push or open pull requests still
  hold a scoped live token. Proxy-side credential injection, which replaces
  it with a placeholder, is opt-in.
* **Network is limited, not absent.** Agents can still reach GitHub reads,
  model backends and opaque `github.com` tunnels.
* **The classifier fails open at L1–L4.** It is on by default from L4 and
  fails closed at L5–L6. The deterministic rules, canaries and proxy denials
  are the floor; the language-model classifier adds coverage above it.
* **Trajectory review reads transcripts, not reasoning**, and fails open.
* **Hub compromise is high impact.** The hub still coordinates callbacks,
  registry data, terminal authorization and fleet visibility.

## Reporting vulnerabilities

Follow [SECURITY.md](../../SECURITY.md): use GitHub private vulnerability
reporting from the repository's Security tab, or contact a maintainer
directly. Do not open a public issue. Maintainers acknowledge reports within
five business days.
