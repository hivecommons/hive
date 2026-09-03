# Security Model

This page is for the evaluator asking: *is it safe to run hive — to host a hive on the hub, point agents at my repositories, and hand the system my AI subscription or API keys?*

Hive's answer is architectural, not aspirational. The project's core design rule — *if a human would give the same answer every time, it belongs in infrastructure, not in a prompt* — applies doubly to security: the controls below are enforced by code (authentication middleware, a policy proxy, scoped tokens, file permissions), not by asking an LLM to behave. Every mechanism described here is verifiable in the hive source on branch `v4` (the only maintained line).

Two companion pages go deeper along their own axis: [security-threat-model.md](security-threat-model.md) is the attacker-oriented view (assets, trust boundaries, threat actors, residual risks), and [security.md](security.md) documents the log-scrubbing and secret-redaction layer. This page is the operator- and evaluator-facing map of the mechanisms themselves.

## What hive touches

A running hive holds three things you care about:

| Asset | How hive uses it |
|-------|------------------|
| **Your GitHub repositories** | Agents read issues/PRs and open branches, issues, and pull requests using a token or GitHub App you provide |
| **Your AI subscriptions / API keys** | Agent CLIs (Claude, Copilot, Gemini, Goose) authenticate with your provider accounts; self-hosted inference backends (litellm / vllm / llm-d) use a gateway API key you enter |
| **Your cluster (or the hosted platform)** | The hive pod runs agents, the dashboard, and persistent state on a PVC |

The threats that matter: an unauthorized person reaching your dashboard, an agent (or a prompt-injected agent) exfiltrating credentials or touching repos it shouldn't, one agent reading another's credentials, and the hosted platform mixing tenants. Each layer below addresses one of these.

---

## Layer 1 — Dashboard access control (hub route)

Hives hosted on or registered with the Hive Hub are reached through a per-hive hostname (`<hive-id>.<cluster-domain>` — `*.hive.kubestellar.io` on the default cluster, other domains on vanity/partner clusters), and every request passes the hub's authentication check before it reaches a spoke dashboard:

- **Multi-provider sign-in.** Users sign in to the hub with GitHub OAuth or Google — with IBMid, Red Hat, Microsoft Entra ID, and a generic OIDC provider available via configuration. With more than one provider configured, `/login` shows a provider picker; a single configured provider goes straight through. The GitHub login deliberately requests **no OAuth scope at all** — the hub only needs to learn *who* is logging in, not to act on their behalf. The session cookie is `HttpOnly`, `Secure`, `SameSite=Lax`, and session cookies are **Ed25519-signed**; the legacy symmetric (HMAC) session lane was deleted on v4.
- **Per-user, per-hive authorization on every request.** The hub's auth-check endpoint (wired as an nginx `auth_request` / `auth-url`) resolves the requesting user, looks up whether that specific hive is in the user's access map, and returns **403 if it isn't**. Only then does the request proceed to the spoke.
- **Identity injection.** On success, the hub injects the verified identity into the proxied request as `X-Hive-User` and `X-Hive-Role` headers (alongside a proxy-auth proof the spoke re-verifies). The spoke's role-enforcement middleware then blocks all write operations for `read`-role users.
- **Roles.** The hive owner has full read-write control and can grant other GitHub users access to their hive; grants carry one of the `read` / `read-write` / `merger` / `owner` tiers. A granter-ceiling that forbids granting a role at or above the granter's own applies on the **access-request approval** path (and exempts hub admins); the owner's direct-grant path validates the role but does not itself cap it, so grant access deliberately.

## Layer 2 — Direct-route spoke authentication

Some hives run on clusters the hub cannot proxy (for example, an OpenShift route reached directly). These spokes cannot rely on the hub's auth check, so the dashboard enforces its own per-user authentication:

- **GitHub device-flow login with a per-hive allowlist.** At provisioning, the hub injects the authorized user list (owner plus any grants) into the spoke as `HIVE_AUTHORIZED_USERS`. When someone completes device-flow login, the spoke validates the GitHub identity and checks it against the allowlist — a non-allowlisted user is **rejected before any session or token is persisted** (the device flow returns an explicit denial; the identity is never logged in).
- **Per-user opaque sessions.** Each authorized visitor gets their own session: a 256-bit cryptographically random session ID in an `HttpOnly` cookie, mapped server-side to their username and role. Two people viewing the same dashboard each see their own identity. The session deliberately does **not** store the user's GitHub token.
- **Forged identity headers are stripped.** On direct routes there is no trusted hub in front, so incoming `X-Hive-User` / `X-Hive-Role` headers are deleted by the authentication middleware (which runs outermost) before any handler sees them. A client cannot claim an identity by setting headers.
- **The shared bearer token grants no identity.** On direct-route spokes, the legacy shared dashboard token path is disabled for user authentication, and the endpoint that would expose it returns 404. Possessing the token does not let you impersonate a user.
- **The device-flow login persists no GitHub write credential — for anyone.** The spoke does not store or install the login token as a hive write identity, so a read-only viewer's login (or logout) can never touch the owner's stored write credentials. The device flow can therefore request no scope at all.
- **Grant roles are enforced, not flattened.** Each non-owner grant is provisioned into `HIVE_AUTHORIZED_USERS` with the **exact role the hub recorded** (`read`, `read-write`, or `merger`), so a direct-route spoke enforces the same tiers as a hub-proxied one; an unparseable role fails safe to `read`.

## Layer 3 — API keys and credential isolation

The strongest isolation story is around inference-backend API keys — agents literally never possess them:

- **Per-agent placeholder keys.** An agent using a self-hosted inference backend (litellm / vllm / llm-d / watsonx) is launched with `ANTHROPIC_API_KEY=sk-hive-<agent-name>` and `ANTHROPIC_BASE_URL` pointing at a translator proxy bound to `127.0.0.1` inside the pod (with `NO_PROXY` keeping the call loopback-local). The placeholder authenticates the agent only to that in-pod translator. Nothing outside the pod accepts that string.
- **Server-side key swap.** The translator identifies the agent from its placeholder, converts the request from Anthropic to OpenAI format, and attaches the **real** gateway key server-side (as a `Bearer` header) before forwarding upstream. The real key lives only in the proxy's route table — never in any agent's environment, filesystem view, or prompt.
- **Secret storage chain.** The real key is resolved from, in order: a configured key file → the Kubernetes Secret mount (`/secrets/litellm_api_key`) → an owner-only PVC file (`/data/secrets/litellm_api_key`, mode `0600` in a `0700` directory) → environment variable. When you enter a key in the dashboard it is written to the PVC file and patched into the hive's own Kubernetes Secret.
- **Never in config files.** `hive.yaml` only ever records a key file *path* or an environment variable *name* — never a key value. The dashboard's persisted config overlay (`/data/hive.yaml.dashboard`) is secret-free by design: env-derived secrets are collapsed back to `${VAR}` references and the dashboard auth token is blanked before writing.
- **Obfuscated in the UI.** Key entry uses a password field; after entry the API never returns the value — only whether a key exists, a masked tail hint, and which store it resolved from. A log-scrubbing handler additionally redacts GitHub-token and JWT patterns from all log output (details in [security.md](security.md)).

**Honest scope:** this placeholder mechanism applies to inference backends. Agents using cloud CLI backends (Claude, Copilot, Gemini, Goose) authenticate with the provider credential you connected for that purpose — that credential is available to the CLI process, as it must be for the CLI to work. Cross-agent exposure of those credentials is limited by the sandboxing layer below.

## Layer 4 — Agent sandboxing

Agents are unprivileged, separated, and policed at the network layer:

- **Per-agent Unix UIDs.** Each agent runs as its own user (UIDs allocated from base 2001) via `su-exec`, with its own tmux server on a per-agent socket. One agent cannot attach to another's session or signal its processes. `su-exec` is mode `4750 root:hive-launch`, so no agent UID can re-exec it to become root.
- **Per-agent GitHub tokens, not shared.** Each agent's scoped token is delivered to a per-agent cache file that is not world-readable: the entrypoint pre-creates it owned `dev:hive-<agent>` with mode `0640`, so only that agent's group can read it, and the locally-minted token path additionally writes `0600` and `chown`s to the agent's UID (deleting the file rather than leaving it shared if the chown fails). Agents cannot read each other's GitHub tokens.
- **Per-agent tool denylists.** Declarative per-agent tool rules in `hive.yaml` become hard CLI flags at launch (`--disallowed-tools` for Claude, `--deny-tool` for Copilot). Separately, GitHub MCP *write* tools are denied for **every** agent in **every** mode — the MCP layer never grants agents a GitHub write path regardless of autonomy level; GitHub writes are instead governed by the token tier and the policy proxy below.
- **Claude Remote Control is pinned off.** Since Claude Code ~2.1.226 the Remote Control bridge (session publishing to claude.ai/code) auto-starts whenever no explicit `remoteControlAtStartup` value exists — a server-side rollout flag decides, so a routine image upgrade could expose every agent as a remote-controllable session under the shared claude.ai account. Hive writes a durable `"remoteControlAtStartup": false`, re-asserted at every claude-CLI launch: merged into the shared `/data/home/.claude/settings.json` for the plain claude backend, and into the per-agent settings seed for inference-gateway agents. The merge is add-if-missing only — an operator who wants the bridge sets the key to `true` in the same file and hive never clobbers it, logging an Info receipt so an enabled bridge is always explainable — and a launch-time check warns if an agent's session file records `hasUsedRemoteControl` despite the pin.
- **A default-on GitHub policy proxy.** Every agent's `HTTPS_PROXY` points at an in-pod proxy that intercepts **GitHub API traffic** and enforces, deterministically, what the agent's current ACMM mode allows: REST method/path rules, GraphQL query-vs-mutation gating, and a **repository allowlist** — writes to any repo outside the hive's configured list are blocked with `403` (`X-Hive-Proxy-Blocked`) at the network layer, regardless of what the agent was prompted (or prompt-injected) to do. The proxy MITM-inspects only `api.github.com`; other destinations are tunneled without inspection. The same proxy attributes traffic to agents by UID (read from `/proc/net/tcp`, unforgeable) for token accounting.

## Layer 5 — GitHub blast-radius controls

What can agents actually do to your repositories? As little as you've dialed in:

- **GitHub App scoping.** The recommended auth is a GitHub App — its reach is inherently limited to the repositories you installed it on. On top of that, hive mints **per-tier installation tokens** matched to each agent's autonomy mode: advisory agents get metadata/PR read-only tokens that *cannot* create issues; mid-tier agents get issues-only tokens with no code access; only trusted tiers get contents/PR write. The shared full-installation token is stripped from every agent's environment.
- **Repo allowlist, enforced twice.** The configured repo list is enforced in the policy proxy (hard, network-level) and also injected into every agent kick as an explicit `AUTHORIZED REPOS` constraint (a prompt-level reinforcement of the hard control).
- **ACMM maturity levels gate autonomy.** Six levels (L1–L6) map to per-agent policy modes enforced end-to-end by token tiers and proxy rules: advisory (observe only) → measured (file issues) → hold-gated PRs → full. At **L5**, agent policies label every PR `hold` so humans batch-review and approve; the underlying guarantee is that **merge permission simply is not granted below L6** — an L5 agent's token tier and proxy rules do not allow merging, whatever its prompt says. The system proposes; humans approve.
- **DCO sign-off.** Agent policies require DCO-signed commits (`git commit -s`); pair this with a DCO check on your repos to make it a hard gate.

## Layer 6 — Hub↔spoke channel

Registered hives send periodic heartbeats to the hub:

- Heartbeats are authenticated with a **per-hive** bearer key derived from the hub master secret (`HIVE_HEARTBEAT_KEY`, reconciled onto hosted spokes; self-hosted spokes derive it from `HIVE_HUB_SECRET` + `HIVE_ID`), validated with a constant-time comparison. The old fleet-wide shared bearer lane was removed.
- The outbound payload carries operational telemetry only: health and cluster metrics, version/git info, agent and governor summaries, and 24-hour **token counts**. No API keys, GitHub tokens, or other credentials leave the spoke in heartbeats.
- The response channel is how the hub delivers configuration to hives it manages (including GitHub App credentials for hub-provisioned hives, scoped to the caller hive's own key) — hub-to-spoke, never spoke-to-hub.

## Layer 7 — Hosted-platform isolation

On the hosted platform, each hive is single-tenant by construction:

- **One namespace, one pod, one PVC per hive.** Provisioning creates a dedicated `hive-hosted-<id>` namespace with its own PVC and a single-replica Deployment. There is no shared runtime between customers' hives.
- **Least-privilege RBAC.** The per-hive Role is namespace-scoped and limited to the hive's own `hive-secrets` Secret by name — a hive pod cannot read other namespaces' secrets.
- **Authenticated internal API.** Server-to-server calls inside the pod (the dashboard proxy to the Go API) carry an internal auth token header validated with a constant-time comparison; security headers (CSP, `X-Frame-Options`, etc.) are always active on the dashboard, and an optional bearer `auth_token` can be required on all API endpoints.
- **Zero-downtime upgrades.** Hosted hives roll updates with `maxSurge=1` / `maxUnavailable=0` on ReadWriteMany PVCs, so upgrades don't create a window where a half-started replacement serves requests.

---

## Key material, rotation, and the egress gate — operator detail

The layers above are the evaluator's map; the sections below are the operator's deep-dive into the key and network layer that Layers 1, 6, and the proxy depend on. For the attacker-oriented view of these same controls, see [security-threat-model.md](security-threat-model.md).

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

### Master key rotation

The hub master secret supports **generations**: at most two live at once — one CURRENT (mints new material) and one PREVIOUS (verify-only, default window 7 days). Design details: [design/master-key-rotation.md](design/master-key-rotation.md).

- **Trigger:** `POST /api/saas/admin/rotate-master-key` (admin-only; refused while an impersonation grant is active; a second rotation within the 8-hour cooldown returns 409 with `retry_after_seconds`). Inspect with `GET /api/saas/admin/key-generations` and `GET /api/saas/admin/auth-rollout`.
- **Rotation is not complete when the endpoint returns 200.** Spokes converge via the per-hive env reconcile at 3 Deployment patches per 15-minute cycle — roughly 6 hours for a ~70-spoke fleet — and **each patch rolls that spoke's pod once** (interrupting running agents there). Plan rotations accordingly.
- **Dual-generation acceptance** during the window: session cookies, heartbeat bearers (bounded trial verification), and SSO/session public keys (current + previous) all verify against both generations. **Terminal and invite keys have no dual lane** — in-flight invite links are invalidated by a rotation.
- **Alerts:** `warn` means the previous generation expires within 24h while spokes still carry it — convergence can still finish in time. `stranded` means the window already closed while spokes carry the old generation — those spokes are now failing verification; converge or re-provision them. Retirement is never blocked by an unconverged spoke (otherwise one unreachable cluster could pin the old key alive forever); the alerts are what make it loud.
- **Do not "rotate" by setting a fresh master directly** (new `HIVE_HUB_SECRET` / replacing `/data/saas/hub-secret.key`): that discards all previous generations and strands every spoke at once. The only converging path is the rotate endpoint.
- Self-hosted spokes converge **manually** — the operator re-derives or re-sets their keys; no sweep reaches them.

### Forced proxy egress and `CAP_NET_ADMIN`

The entrypoint installs an iptables REDIRECT of all outbound `:443` through the MITM proxy, so agents cannot bypass capability enforcement with a raw token. Consequences for deployment:

- **Spokes require `CAP_NET_ADMIN` + iptables to start enforcing.** If the chain cannot be created, the entrypoint exits and refuses to start. Grant the capability with `--cap-add NET_ADMIN` (docker/podman) or `securityContext.capabilities.add: ["NET_ADMIN"]` (Kubernetes). The escape hatch `HIVE_PROXY_ADVISORY_OK=true` starts the spoke with egress enforcement **advisory-only** — agents can bypass the proxy — and logs a WARN saying exactly that. Chain creation retries 5× with jittered backoff so co-scheduled spokes don't fail in lockstep. This FATAL exits with a **distinct exit code, 77** (sysexits.h `EX_NOPERM`) specifically when the bounding set lacks `CAP_NET_ADMIN`, so a supervisor can key off it without parsing logs; any other cause of the same FATAL still exits `1`. See [net-admin-requirement.md](net-admin-requirement.md#when-the-container-refuses-to-start-exit-77).
- **The hive binary carries no file capabilities.** Earlier channel images shipped `/usr/local/bin/hive` with a `cap_net_admin+ep` file capability, which made the kernel refuse `execve()` with a bare `Operation not permitted` wherever the container's bounding set lacked `NET_ADMIN`. The binary now execs everywhere; the entrypoint instead raises `NET_ADMIN` as an **ambient capability** at the privilege drop, gated on the bounding set actually having it. Without the grant you get a one-line NOTICE instead of a crash.
- **Agent identity is UID-based, not self-asserted.** The proxy reads `/proc/net/tcp` to resolve the calling process's UID against `uid-map.json`, which is unforgeable — a process cannot claim another UID's socket. A `Proxy-Authorization: hive <name>` header sent by the caller is a fallback ONLY, and by default the proxy does not trust it: with no UID map (or no match for this connection), the caller is treated as unidentified (`ADVISORY` mode, writes blocked) rather than as whatever name the header claims. `HIVE_PROXY_ADVISORY_OK=true` also permits the header fallback, for deployments (local dev, native/systemd installs with no per-agent UID separation) that have no UID map to check against.
- **Rootless podman:** the image runs, but rootless cannot meaningfully grant `NET_ADMIN`, so the egress gate cannot be installed — a rootless spoke only starts with `HIVE_PROXY_ADVISORY_OK=true`, i.e. with the capability model unenforced. Treat rootless as unsupported for enforcing deployments.
- **Hosted fleet:** the hub sweeps hosted spoke Deployments every 15 minutes and patches `NET_ADMIN` into the container securityContext where missing. The patch replaces the whole `securityContext` object — capabilities hand-added to a hosted spoke will be erased within 15 minutes. (On OpenShift/OVN the podspec request is necessary but not sufficient; the SCC must also allow it.)

### In-container privilege model

- The hive/proxy process runs as user `dev` (UID 1001); each agent runs as its own UID from `HIVE_UID_BASE=2001` upward.
- `su-exec` is mode **4750 `root:hive-launch`** — only root and members of the pinned `hive-launch` group (GID 1002) can exec it. This closes the earlier world-executable-setuid hole where any agent UID could become root in the pod. `HIVE_LAUNCH_GID=1002` is part of the deployment contract: Kubernetes `fsGroup` and Secret `defaultMode: 0440` rely on the numeric GID, and it must stay in sync across `src/deploy/k8s/*.yaml` and hub provisioning.
- **`su-exec` is the only setuid binary in the image.** The 4750 mode above restricts the helper Hive ships; it says nothing about the ones the base image ships, and `node:26-slim` (Debian 13) plus the `passwd`/`util-linux` packages leave eleven behind — `chfn`, `chsh`, `gpasswd`, `mount`, `newgrp`, `passwd`, `su`, `umount` (setuid root) and `chage`, `expiry`, `unix_chkpwd` (setgid shadow). All eleven were **world-executable**, so every agent UID could exec them: the same class of surface the 4750 mode exists to remove, sitting next to it. Nothing in Hive uses any of them — the entrypoint creates agent accounts with `useradd`/`groupadd`/`usermod` while still root, and every privilege hop goes through `gosu` (as root) or `su-exec` (as `dev`) — so `src/Dockerfile` strips the bits after the last install layer and **fails the build** if the resulting inventory is anything but `4750 root:hive-launch /usr/local/bin/su-exec`. Only a non-root caller needs a setuid bit, so root keeps full use of `mount`/`su`/`passwd`.
- **Why the image, and not `allowPrivilegeEscalation: false`.** Setting it would set the kernel's `no_new_privs` bit, which disables *all* setuid binaries — including `su-exec`, which would break agent launch outright. So `no_new_privs` stays off by necessity, and a setuid-root exec from an agent UID would land in a bounding set that still holds `SETUID`, `SETGID` and `DAC_OVERRIDE` — enough to read the MITM CA key whatever mode its directory carries. Removing the binaries is the only lever the deployment leaves. Enforced by `src/scripts/check-suid-contract.sh` (static, every PR) and `src/deploy/test_image_suid_inventory.sh` (boots the image and reads the inventory back; also run against the real built image in `docker.yml`).
- The MITM CA certificate is readable at `/data/proxy-ca.pem` so agent clients can trust forged certificates, but the CA private key is stored at `/data/.hive/proxy-ca-key.pem` in a `0700` directory with mode `0600`. It is never intentionally kept directly under the shared, agent-writable `/data` namespace. A legacy `/data/proxy-ca-key.pem` causes the old CA pair to be discarded on startup, because its key may already have been copied.

### Supply chain

- **npm:** all global AI-CLI installs (claude-code, codex, copilot, goose-adjacent tooling, etc.) run with `--ignore-scripts`.
- **Base images digest-pinned:** `golang`, both `node` stages, and the contributor image's `debian:bookworm-slim` are pinned by digest; the vLLM inference image is digest-pinned with a model checksum. Refresh procedure is documented at the top of `src/Dockerfile`.
- **Tools pinned by SHA:** tmux, ttyd, gh, goose, su-exec (by commit), and friends are checksum-verified at build.
- **Workflows:** `actions/checkout` is SHA-pinned.
- **CODEOWNERS:** `.github/CODEOWNERS` covers the security-sensitive paths (Dockerfiles, workflows, deploy manifests, launch scripts, key/cookie code). It is **advisory until "Require review from Code Owners" is enabled** in branch protection — deliberately left off because the repo's automation merges green PRs without human review; enabling it is an operator/repo-settings choice.

### Metrics endpoint

`GET /metrics` (Prometheus text format, on the dashboard port) is off unless `HIVE_METRICS_ENABLED` is truthy (`1|true|yes|on`); disabled means the route is not registered (scrapes 404). Because Prometheus cannot do device-flow auth, an enabled `/metrics` bypasses dashboard auth, so the bearer token is its only guard — and it **fails closed**: `HIVE_METRICS_TOKEN` is mandatory whenever metrics are enabled. Enabled-but-tokenless returns 403 (with an error body naming both variables, plus a startup warning); a wrong/missing bearer returns 401. The exposed series are business-sensitive: cumulative estimated cost per model/agent (`hive_estimated_cost_usd*`) and token totals per model.

### Advisory digests

Agent findings are posted as a single, continuously rewritten tracking-issue comment per repo. Each digest is **pinned to one analyzed commit**: the footer cites `owner/repo@<sha>` (branch included), and any finding whose file path no longer exists at that commit is flagged *"file path not found at analyzed commit — finding may be outdated"* instead of showing a dangling `file:line`. Inconclusive path checks (rate limits, transient errors) fail open — a finding is only flagged on a definitive 404.

---

## Scope and limitations, stated plainly

Security documentation that only lists strengths is marketing. The current, deliberate scoping:

- **Rootless containers cannot enforce the egress gate.** The forced-proxy egress redirect needs `CAP_NET_ADMIN` + iptables; a rootless podman spoke only starts with `HIVE_PROXY_ADVISORY_OK=true`, i.e. with network-layer enforcement advisory-only.
- **The policy proxy inspects GitHub API traffic.** All outbound `:443` is force-redirected through it, but non-`api.github.com` destinations are tunneled without inspection — it is a GitHub-policy enforcement point, not a content firewall for arbitrary hosts. If you need full egress control, apply Kubernetes NetworkPolicies around the hive pod.
- **The L5 `hold` label is applied by agent policy;** the hard guarantee at L5 is the withheld merge permission (token tier + proxy rules), not the label itself.
- **Cloud CLI credentials are shared with the CLI.** Claude/Copilot/Gemini/Goose agents necessarily run with the provider credential you connected. The placeholder-key isolation applies to inference-gateway keys.
- **DCO is policy-driven** inside hive; enforce it repo-side (DCO check) for a hard guarantee.
- **Shared-container execution remains a material risk.** Per-UID isolation reduces cross-agent access but is not equivalent to per-run containers or microVMs; see the residual-risks discussion in [security-threat-model.md](security-threat-model.md).

## Reporting vulnerabilities

Do not report suspected vulnerabilities in public issues, pull requests, or discussions. Follow the repository Security Policy: use GitHub private vulnerability reporting from the repository **Security** tab, or contact a maintainer directly if private reporting is unavailable. Include the affected component, branch/commit, impact, reproduction steps, and any relevant logs or configuration.
