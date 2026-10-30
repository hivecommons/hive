# Security model: how Hive keeps autonomous agents from doing damage

Last reviewed: 2026-10-23

Hive runs AI coding agents against real repositories. This page is the
inventory of the controls that limit what an agent can do, with the file or
setting behind each one. Hive's design rule is that anything a human would
decide the same way every time belongs in infrastructure, not in a prompt:
the controls below are Go, shell and kernel mechanisms, not instructions to a
model. Where a control is a proposal rather than code, the page says so.

An inventory is not a promise that every running hive has every control on.
Check the effective configuration before relying on a gate; a dashboard
toggle alone is not evidence of enforcement.

Companion pages: [security-threat-model.md](security-threat-model.md)
(attacker view), [security.md](security.md) (log scrubbing),
[securing-your-hive.md](securing-your-hive.md) (first-time operator guide).

## Choose a reading path

- **First-time operator:** use the guide above for trust choices and who can
  merge. This page is a technical inventory, not a deployment checklist.
- **Check a running hive:** start with [Human in the loop](#10-human-in-the-loop)
  and the inventory's “How to check” column. Dashboard owners can inspect
  effective settings; file, kernel and network checks need deployment-admin
  access. Hosted owners should ask their hub operator for those checks.
- **Audit the implementation:** use the control sections and
  [verification procedure](#12-how-to-audit-a-running-hive). Do not change production state
  or print secrets to prove a check.

**Terms used below:** a **UID** is a Unix user ID. A **cgroup** tracks and
limits a process group's resources. **Egress** means outgoing network traffic.
**MITM** means man-in-the-middle: Hive's local inspecting proxy. A **PAT** is
a personal access token. A **JWT** is a JSON Web Token; its **TTL** is its
time to live. **CSP** is Content Security Policy, a browser restriction on
page resources and scripts. These names describe mechanisms, not proof they
are enabled on a given hive.

## 1. Threats

* **Prompt injection.** Issue and pull-request text, labels, comments,
  cloned repositories and tool output are attacker-controlled and reach the
  agent's prompt.
* **Runaway or stuck agents.** Loops, token burn, endless re-dispatch, wedged
  terminals.
* **Credential theft.** An agent printing, pushing or sending a GitHub token,
  provider key or hub secret off host, or reading another agent's.
* **Bad merges.** Code landing without CI, review or human sign-off, or at a
  different commit than the one judged.
* **Over-trusted contributors.** Humans or bots steering agents, claiming work
  above their trust, adding themselves as owners, or weakening CI.
* **Cost abuse.** Token burn, API rate-limit exhaustion, label and comment
  storms.
* **Platform compromise.** Unauthorized dashboard or terminal access, forged
  hub-to-spoke messages, cross-tenant exposure on the hosted hub.
* **Supply chain.** Poisoned base image, npm install script, unpinned Action
  or vulnerable Go dependency.

## 2. Control inventory

| Control | Mitigates | Where it lives | How to check it is on |
|---|---|---|---|
| Fresh context per task (`clear_on_kick`) | injection persistence | `src/crux/agent/manager_kick.go` | `hive.yaml` agent block |
| Watchdog and crash-loop pause | stuck agents | `src/crux/agent/watchdog_fleet.go` | `conditions[]` in `GET /api/agents` |
| Per-agent Unix users, `su-exec` 4750 | cross-agent access, privilege escalation | `src/deploy/entrypoint.sh`, `src/Dockerfile` | `/var/run/hive/uid-map.json`; `suid-contract` CI job |
| Per-agent tool denials; GitHub MCP writes denied | GitHub writes through tools | `src/crux/agent/manager_launch.go` | agent launch command |
| Credential-free sandbox and push broker (opt-in) | credential theft | `src/crux/sandbox`, `src/crux/pushbroker` | `agentSandboxEnabled` in `GET /api/config/governor` |
| Work ledger and knowledge store instead of memory | injection persistence | `src/pkg/beads`, `src/pkg/knowledge` | `bd` CLI |
| `ioscan` scanning and canaries (default on) | prompt injection, secret leakage | `src/pkg/ioscan` | `ioscanEnabled`, `ioscanFailMode` in `GET /api/config/governor` |
| Trajectory review (default on) | goal drift | `src/crux/trajectory` | `governor.trajectory.enabled` |
| Policy proxy and repo allowlist | credential theft, out-of-scope writes | `src/crux/proxy/rules.go` | `403` with `X-Hive-Proxy-Blocked` |
| Forced egress through the proxy (iptables) | proxy bypass | `src/deploy/entrypoint.sh` | boot log; no `HIVE_PROXY_ADVISORY_OK` |
| Tiered GitHub tokens per agent mode | blast radius of a leaked token | `src/crux/agentmode`, `src/crux/agent/manager_tokens.go` | token tier on the agent card |
| Proxy-side credential injection (opt-in) | token theft | `HIVE_PROXY_INJECT_GH_AUTH` | `security.credentialInjection` in governor config |
| Inference-key placeholder swap | provider key theft | `src/pkg/apiproxy` | agent env shows `sk-hive-<agent>` |
| `gh` wrapper with per-agent restrictions | denied commands | `bin/gh-wrapper.sh` | `gh` resolves to `/usr/local/bin/gh` |
| `hive-open-pr` / `hive-merge` relays | forged authorship, stale-commit merges | `bin/hive-merge.sh`, `bin/hive-open-pr.sh` | `POST /pulls` returns 403 for agents |
| Automerge gates | bad merges | `src/pkg/github/automerge/automerge_sweep.go` | skip reasons in audit entries |
| Intent verification | scope creep, guardrail edits | `src/pkg/intent` | `intentEnforce` in governor config |
| Hold guard | drift after approval | `src/pkg/holdguard` | evidence comment on held PR |
| Escalation circuit breaker | fix loops | `src/pkg/escalation` | `needs-human` label |
| Sentinel (eight rules) | malicious PRs, CI weakening | `src/pkg/sentinel` | `sentinel-alert` label |
| Reporter trust (opt-in) | hostile issue reporters | `src/pkg/config/reporter_trust.go` | `reporter_trust.enabled` |
| Dashboard roles (`read`/`read-write`/`merger`/`owner`) | unauthorized writes | `src/pkg/dashboard/api_auth.go` | 403 on write as `read` |
| Audit and token-access logs | unattributed actions | `src/pkg/dashboard/audit.go`, `src/pkg/agentaudit` | `GET /api/audit` |
| Token budget and per-pass caps | cost abuse | `src/pkg/governor/governor.go` | `governor.budget.*` |
| Pause switches | emergency stop | `project.paused_repos`, `POST /api/pause/<agent>` | config |
| Webhook signature check | forged events | `src/pkg/dashboard/api_github_webhook.go` | unsigned request rejected |
| Hub auth (Ed25519 sessions, per-hive bearers) | platform compromise | `src/pkg/hub/saas_auth.go` | `GET /api/saas/admin/auth-rollout` |
| Content Security Policy and headers | dashboard XSS | `src/pkg/dashboard/server.go` | response headers |
| Log scrubbing | secrets in logs | `src/crux/logscrub` | redacted patterns in logs |
| Supply-chain guards | supply chain | `.github/workflows/*.yml`, `src/Dockerfile` | PR checks |
| DCO on every commit | unattributed code | `src/scripts/check-dco-trailers.sh` | DCO check on PR |

## 3. Agent runtime isolation

**Bounded runs.** Work arrives as discrete tasks from the governor. By default
each task starts with a cleared model context, so the agent begins from the
task text and its instructions file, not from what a previous issue author
wrote. A resumed pull-request session is the deliberate exception. A turn
ceiling bounds how long the governor waits on a continuous turn. The watchdog
classifies every agent pane on each tick, restarts dead panes with backoff
from one to sixteen minutes, and after five failed restarts pauses the agent
and raises an alert until a human intervenes. Context reset is not process
reset: the CLI process and its on-disk session state persist until restart.

**Separation.** Each agent runs as its own Unix user (from UID 2001), in its
own `tmux` server on its own socket, with its own home directory and token
file. Token files are owned by the agent's group with mode 0640 or by the
agent with mode 0600. Agents cannot attach to each other's sessions, signal
each other's processes or read each other's tokens. Agents coordinate through
the work ledger and GitHub, never by reading transcripts.

**Tool denials.** Per-agent tool rules in `hive.yaml` become hard CLI flags
at launch. GitHub MCP write tools are denied for every agent in every mode.
Claude Code's Remote Control feature is pinned off in every agent's settings
at each launch.

**Privilege model.** The hive process runs as user `dev` (UID 1001).
`su-exec` is the only setuid binary in the image, mode 4750 `root:hive-launch`,
so no agent user can run it. The setuid bits on the eleven setuid or setgid binaries the Debian
base image ships are stripped at build, and the build fails if the
inventory is anything but `su-exec`. `allowPrivilegeEscalation: false` is not used
because it would disable `su-exec` and break agent launch. The proxy CA
private key is stored with mode 0600 in a 0700 directory.

**Credential-free sandbox (opt-in).** A task can run in a rootless Podman
container or a Kubernetes Job that holds no credentials. Both the global
`agent_sandbox.enabled` and the per-agent `sandbox.enabled` must be set. A
push broker outside the sandbox scans committed diffs for secrets and
protected-path edits, mints a short-lived token and pushes the branch. See
[sandbox-isolation.md](sandbox-isolation.md).

## 4. Context and memory

Hive replaces long-lived agent memory with two inspectable stores. The work
ledger holds typed work items, status and notes as git-backed JSON. The
knowledge store holds typed facts with sources and confidence. Neither stores
conversation text. Agents are primed with a bounded set of facts in the task
prompt; they cannot query the store, so a poisoned fact cannot be amplified.
Untrusted text is scanned by `ioscan` before it reaches a prompt
([ioscan.md](ioscan.md)); canaries are planted per task and agent output is
scanned for them. Session state is pruned and logs are scrubbed
([security.md](security.md)).

## 5. Network and credentials

**Inference keys.** An agent using a self-hosted inference gateway is
launched with a placeholder key, `sk-hive-<agent>`, and a base URL pointing
at a translator proxy on loopback inside the pod. The translator recognises
the agent from the placeholder and attaches the real key server-side. The
real key is read from a key file, a Kubernetes Secret mount, or an owner-only
file on the volume, never from `hive.yaml`, which records only paths and
variable names. The dashboard never returns a key value. Agents using cloud
CLI backends (Claude, Copilot, Codex, Goose) run with the provider credential
you connected, because the CLI needs it.

**GitHub blast radius.** The recommended authentication is a GitHub App,
whose reach is limited to the repositories it is installed on. Hive mints a
token per agent matching its mode: advisory agents get read-only tokens,
measured agents can write issues, hold-gated and full agents can write code.
The repo allowlist is enforced at the proxy and repeated in every task
prompt. Below L6, no agent token can merge. Agent policy requires DCO
sign-off; pair it with a DCO check on your repos for a hard gate.

**The `gh` wrapper and relays.** `/usr/local/bin/gh` is a wrapper that
applies per-agent restriction rules and injects the agent's token; agents
never pass tokens themselves. A scrub script runs at the start of every shell
in the agent's process tree and removes inherited credentials. `POST /pulls`
and `PUT /pulls/{n}/merge` are denied at the proxy for every mode; agents call
`hive-open-pr`, `hive-open-issue`, `hive-merge` and `hive-review`, which drop
a request file that Hive processes as the App. The merge relay requires the
expected head commit and checks the pull request is on the governor's
merge-eligible list at that commit.

**Proxy-side credential injection (opt-in).** With
`HIVE_PROXY_INJECT_GH_AUTH=true`, the agent's token file holds only a
placeholder and the proxy swaps in the real token on GitHub API requests. It
requires GitHub App authentication and is refused if advisory proxy mode is
also set, because advisory mode trusts a self-asserted agent name. A
default-on version with a signing sidecar was proposed in
[#9586](https://github.com/hivecommons/hive/issues/9586) and declined in
September 2026; injection stays opt-in. Rotation of contributor registration
tokens is a separate open item
([#13804](https://github.com/hivecommons/hive/issues/13804)).

**Forced egress.** The entrypoint installs an iptables rule redirecting all
outbound port-443 traffic through the proxy, so agents cannot bypass it with
a raw token. This needs `CAP_NET_ADMIN`; without it the spoke refuses to
start unless `HIVE_PROXY_ADVISORY_OK=true` is set, which makes the gate
advisory only. Rootless Podman cannot grant `NET_ADMIN`, so rootless
deployments are unsupported for enforcement. The proxy identifies the calling
agent by the user ID that owns the socket, read from `/proc/net/tcp`, which a
process cannot forge. Only `api.github.com` is inspected; other hosts are
tunnelled without inspection. See
[net-admin-requirement.md](net-admin-requirement.md).

**Plain-HTTP forwards.** The redirect covers port 443 only, so plain HTTP
reaches the proxy through its explicit proxy port. Those forwards are
identified like any other request and dialled only when the resolved address
is globally routable: loopback services beside the proxy, the link-local
metadata endpoint, RFC 1918/CGNAT cluster addresses and multicast are refused
with `403` and counted as violations (loopback excepted — agents are handed
`NO_PROXY` for it, so it is logged as the client misconfiguration it is). The
check runs at dial time on the resolved address, so a hostname pointed inward
is refused on the same evidence as a literal IP. Set
`HIVE_PROXY_ALLOW_PRIVATE_FORWARD=true` only where a private-network plain-HTTP
destination is intended.

**Hub-to-spoke channel.** Heartbeats carry a per-hive bearer key derived from
the hub master secret and are compared in constant time. The payload is
telemetry only: health, version, agent and governor summaries and 24-hour
token counts. No credential leaves a spoke in a heartbeat. The hub delivers
configuration, including App credentials for hosted spokes, in the response.

## 6. Code admission

A green check is necessary but not sufficient. Between green CI and a merge,
Hive runs deterministic gates that do not depend on the agent's own claims:

| Gate | Refuses |
|---|---|
| Relay authorization | A merge request without an expected commit, or for a pull request not on the merge-eligible list at that commit, or from a user whose mode cannot merge. |
| Automerge preconditions | Draft, closed, not mergeable, `hold`, `needs-human`, `needs-rebase`, `do-not-merge`, `blocked`, `hive-pause/<hive-id>`; CI not green; a current `CHANGES_REQUESTED` review; a missing reviewer verdict when required. |
| Fresh fetch | Merging on cached state: the head commit is re-fetched and re-verified immediately before the merge call. |
| No-CI repositories | Merging where no checks exist unless `auto_merge.no_ci_ok` is explicit. |
| DCO | Merging a head whose DCO check is not passing. |
| Trusted-author automerge | Auto-merging a human or bot pull request unless the author is a trusted author with a `merger` or `owner` role and GitHub write access; permission lookup failures fail closed. |
| Intent verification | A pull request with no linked issue; a feature with no approved plan; any change to workflows, the `gh` wrapper, proxy rules, policies, `hive.yaml`, `OWNERS` or `CODEOWNERS` without maintainer approval. |
| Hold guard | Lifting a hold onto a head that moved since the human saw it; drift re-applies `hold` and posts one comment naming the new commits. |
| Review swarm verdicts | A self-asserted approval; verdicts are written by the review relay into a hive-owned directory and aggregated deterministically. |
| Escalation circuit breaker | A fourth fix attempt after three distinct red commits; the item is labelled and routed to a person. |
| Stage engine | Any writer of `stage/*` labels other than `src/pkg/stageengine`; an admission test enforces this. |
| Sentinel label | Hive approval and every automerge lane while `sentinel-alert` is present. |
| Repo pause | Agent writes, relays and automerge on a paused repo. |
| GitHub | Hive never force-merges; branch protection and rulesets apply to the App token as to a human. |

Every merge leaves a review evidence bundle: canonical JSON of what was
reviewed and which checks and verdicts were seen, hashed and optionally
Ed25519-signed. See [review-evidence.md](review-evidence.md). Check the
enforcing mode: `intent.enforce: true` blocks; shadow and report-only modes
do not.

## 7. Contributor trust

* **Reporter trust (opt-in)** withholds issues from authors outside the
  trusted set until a maintainer adds a triage label, and holds pull requests
  that trace to unaccepted issues. See
  [securing-your-hive.md](securing-your-hive.md).
* **Dashboard roles.** `authorized_users` carries `read`, `read-write`,
  `merger` or `owner`. Security settings, sandbox toggles and merge-lane
  releases are owner-only.
* **Contributor tiers.** Relay contributors move from `newcomer` to
  `contributor` automatically after five completed pull-request tasks, to
  `trusted` by operator grant, and to `merger` or `advisor` by maintainer
  decision. Tiers do not decay. See
  [contributor-trust-and-roles.md](contributor-trust-and-roles.md).
* **Claims and leases** are fenced by task generation, so no issue is held
  by two contributors at once. The lease protocol is formally modelled.
* **Author kind, not author claim.** Pull-request authors are classified
  from GitHub identity (Hive App, trusted bot, human, unknown bot).
* **Issue commands (045).** A human with repo write/maintain/admin access
  can use `/hive approve` or `/hive decision <direction>` to release decision
  parking. The command lifts `needs-human`, whoever applied it, but never
  `hold`. Label-driven repos use their configured acceptance labels instead.
* **PR commands (085/070).** Bare `/hive approve` is acknowledged only.
  On an eligible Hive-authored PR, `/hive decision <instruction>` routes
  the full text to the author/fixer. `/hive decision approve <rationale>`
  records approval for the current head and can release `requires_human`.
  This requires repo write access or higher **and** configured merger trust.
  Other authors use GitHub reviews/manual merging. Commands do not waive
  CI, holds, author admission or path fences. See
  [the exact gate contract](running-at-level-6.md#does-the-hive-reviewer-stop-a-merge).
* **Owner self-nomination** is a sentinel rule: a pull request adding its
  author to `OWNERS`, `CODEOWNERS` or `MAINTAINERS` is flagged and blocked.

## 8. Detection and response

**Sentinel** (default on) checks every open pull request against eight rules:
sensitive paths, owner self-nomination, permission escalation in workflows,
secret exposure, CI gate weakening, test removal, security policy edits, and
remote code execution patterns. A match applies `sentinel-alert`, posts one
comment and writes an audit entry. The label blocks Hive approval and every
automerge lane until a maintainer removes it.
Whether that label and comment reach GitHub is decided by the **write-back
containment gate** (`pkg/writeback`): on a PR Hive did not open, sentinel
writes need ACMM L5 plus the review opt-in label (L6 without it), the repo
must not be in `issues-only`/`off` mode, and the PR must not predate Hive's
write access to the repo (backfill guard). Below the gate the finding is
recorded, audited as `sentinel-finding-suppressed`, and shown in the
Security tab and `/api/sentinel` — the scan never stops, only the
unsolicited write does. The same gate fronts hold-gate notices, stage
labels, supersession and duplicate-fix notices and reporter-trust holds; a
ratchet test fails on any new direct GitHub write outside the package.

**Trajectory review** runs every 600 seconds by default. A second model
compares each agent's assigned intent with the last 120 lines of its rendered
terminal transcript and pauses the agent on divergence. It fails open if the
reviewer is unavailable.

**Audit trails.** The dashboard audit log records every dashboard and API
mutation, merge decision, sentinel finding and watchdog action to
`/data/audit.jsonl`, rotated at 5 MB with three backups and a 90-day
retention. The token-access log records which agent used a credential for
which command. Proxy decisions, review evidence bundles and scrubbed agent
transcripts complete the set. See [audit-log.md](audit-log.md) and
[token-access-log.md](token-access-log.md).

**Budgets and caps.** A seven-day token budget warns at a configurable
percentage and suppresses tasks on exhaustion. Fair-share scheduling spreads
tasks across repositories; every sweep that writes labels or comments has a
per-pass cap. Fix cycles share a re-engagement limit.

**Alerts.** Sentinel findings, trajectory pauses, crash loops and
`needs-human` routing appear as dashboard banners and, when configured, as
notifications.

## 9. Platform and supply chain

**Hub-routed dashboards.** Each hosted hive has its own hostname. The hub
authenticates every request (GitHub, Google or a configured OIDC provider)
and returns 403 unless that user has a grant on that hive. On success it
injects the verified identity as headers that the spoke re-verifies. Grants
carry `read`, `read-write`, `merger` or `owner`.

**Direct-route spokes** enforce their own authentication: GitHub device-flow
login against a per-hive allowlist, per-user opaque sessions in `HttpOnly`
cookies, forged identity headers stripped, and the shared bearer token
disabled for user login.

**Hosted isolation.** One namespace, one pod and one volume per hive. The
per-hive Role is namespace-scoped and limited to the hive's own Secret.
Internal server-to-server calls carry an auth token compared in constant
time.

**Keys.** Sessions and single sign-on are verified with Ed25519 signatures
only; there is no symmetric fallback, and a missing key fails closed. Every
spoke uses keys derived per hive from the hub master secret. The master
secret supports two generations, current and previous, with a seven-day
verify-only window; rotation is triggered by an admin endpoint and spokes
converge through a 15-minute reconcile sweep. Setting a fresh master secret
directly strands every spoke.

**Webhooks and terminals.** GitHub webhooks are verified with HMAC-SHA256
against `X-Hub-Signature-256`. Terminal access uses short-lived signed
assertions. Systemd units refuse to run code from a directory an
unprivileged user could have created.

**Dashboard headers.** Content Security Policy, `X-Frame-Options` and related
headers are always set. The metrics endpoint is off unless
`HIVE_METRICS_ENABLED` is set.

**Supply chain.** Global npm installs run with `--ignore-scripts`. Base
images are pinned by digest. Tools such as tmux, ttyd, `gh`, goose and
`su-exec` are checksum-verified at build. Third-party Actions are pinned to
commit SHAs. CI runs `govulncheck`, `gosec` and `golangci-lint` as separate
jobs, OpenSSF Scorecard, Dependabot, an Action-pin check, a setuid inventory
check, DCO trailer checks, a documentation link check and formal-model
checks. Image attestations are turned off because they broke
multi-architecture manifests, and a guard workflow keeps them off. An SPDX
software bill of materials per image is attached to each release
([releases.md](releases.md)). Container images are not signed.
`.github/CODEOWNERS` covers the security-sensitive paths but is advisory
until code-owner review is required in branch protection.

## 10. Human in the loop

* **ACMM** is the autonomy dial. Merge permission is not minted below L6.
  L5 adds a `hold` label on every agent pull request. See
  [acmm-policy-matrix.md](acmm-policy-matrix.md).
* **Issue decision parking is 045.** A person with repository write access or
  higher answers with `/hive approve` or `/hive decision <text>`, unless the
  repo uses label-driven acceptance. This releases decision parking and
  `needs-human`, not `hold`.
* **PR decision parking is 085.** A trusted merger's GitHub **Approve**
  review must name the current head to clear the enforcing sweep gate.
  It can override any swarm outcome, including a request for changes;
  the audit records who overrode what. On eligible Hive-authored PRs,
  `/hive decision approve` can answer `requires_human`, while other decision
  text sends work to the fixer. Bare `/hive approve` never releases a PR.
  The detailed [command boundaries](#7-contributor-trust) above still apply.
* **Owner-only security settings.** Changes to `ioscan`, intent enforcement,
  review requirements and the agent sandbox require the `owner` role.
* **Emergency stops are not interchangeable.** To stop merging, disable
  automerge per repo, apply an item hold, or drop below L6. Pausing an agent
  or freezing the watchdog does not stop the App from merging existing PRs.
  Use the [L6 emergency-stop table](running-at-level-6.md#stops-smallest-to-largest)
  to choose the right boundary.
* **Hooks** run only vetted actions from a fixed set ([hooks.md](hooks.md)).
* **Advisory digests** are pinned to one analysed commit; findings whose
  file no longer exists are dropped.

## 11. What Hive does not do yet

* **Rootless containers cannot enforce the egress gate.**
* **The proxy inspects GitHub API traffic only.** Other destinations are
  tunnelled. Use Kubernetes NetworkPolicies for full egress control.
* **The L5 `hold` label is applied by agent policy.** The hard guarantee at
  L5 is the withheld merge permission, not the label.
* **Cloud CLI credentials are shared with the CLI.** The placeholder-key
  isolation applies to inference-gateway keys only.
* **DCO is policy-driven inside Hive.** Enforce it repo-side for a hard
  guarantee.
* **Agents share one container.** Per-user separation is not per-task
  containers. The sandbox is opt-in and two-gated. A separate, opt-in
  contributor-side krun microVM (`HIVE_CONTAINER_OCI_RUNTIME=krun`, see
  [contributor-relay.md](contributor-relay.md) "Hardened mode") does not
  change the isolation of the hive's own agents.
* **Container images are not signed.**
* **No CodeQL or container CVE scan** runs in this repo's CI.
* **Branch protection is GitHub's.** Hive does not configure or verify it.
* **Gate-integrity invariants are a proposal.** Refusing agent force-pushes
  and history rewrites is designed
  ([design/gate-integrity-invariants.md](design/gate-integrity-invariants.md)),
  not enforced.
* **Hub registration has no review step.** Any spoke with the derived keys
  can register.
* **Trust tiers do not decay.**
* **Trajectory review fails open; the `ioscan` classifier fails open only
  at L1–L4.** At L5–L6 text is withheld when the classifier cannot answer.
  The deterministic rules, canaries, proxy and token tiers hold when a model
  is down.

## 12. How to audit a running hive

Use an owner-authorized session. Keep production inspection read-only. Do not
paste tokens, environments or Secret contents into reports.

| Question | Check |
|---|---|
| Is the egress gate enforcing? | Boot log has no `HIVE_PROXY_ADVISORY_OK` warning; `iptables -t nat -L HIVE_PROXY` in the pod lists the port-443 redirect. |
| Are agents separated? | `cat /var/run/hive/uid-map.json`; `ls -l /usr/local/bin/su-exec` shows `-rwsr-x--- root hive-launch`; `find / -perm /6000 -type f` lists only `su-exec`. |
| Does an agent hold a usable token? | `GET /api/config/governor` → `security.credentialInjection`. |
| Is `ioscan` on, and in which mode? | `GET /api/config/governor` → `security.ioscanEnabled`, `ioscanFailMode`, `ioscanCanaries`. |
| Is trajectory review on? | `GET /api/config/governor` → `trajectory`. |
| Is the sentinel on? | `sentinel` config and recent findings with the alert label. |
| Who merged what, and why? | `GET /api/audit`; `hivectl evidence export --repo <o/r> --since <date>`. |
| Who used a token? | `/var/run/hive-metrics/token-access.jsonl`. |
| What is the merge posture per repo? | `hive.yaml` → `acmm_level`, `project.repo_policies[].auto_merge`, `auto_merge.trusted_authors`, `project.paused_repos`. |
| Are webhooks authenticated? | `GITHUB_WEBHOOK_SECRET` is configured; test unsigned-request rejection only in a disposable hive. |
| Is the image what CI built? | GHCR digest from the `docker.yml` run equals the running `imageID`. |

To test denials, use a disposable evaluation hive and an operator-owned test
repository. A write to a repository outside the allowlist must return `403`
with `X-Hive-Proxy-Blocked`; a request authenticated with a placeholder token
must fail; an unsigned webhook must be rejected. Never probe production by
relaxing gates.

## Compliance

Hive holds no SOC 2, FedRAMP, ISO 27001 or other certification.
[compliance.md](compliance.md) maps the controls above to SOC 2 requirements
and lists the requirements Hive cannot cover.

## Reporting vulnerabilities

Follow [SECURITY.md](../../SECURITY.md): use GitHub private vulnerability
reporting from the repository's Security tab, or contact a maintainer
directly. Do not open a public issue.
