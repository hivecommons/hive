# Security Model — Operator Guide

This page describes the security mechanisms an operator interacts with on the current (`v4`) line: authentication keys, key rotation, the forced-proxy egress gate, the in-container privilege model, and supply-chain posture. For the attacker-oriented view, see [security-threat-model.md](security-threat-model.md); for log redaction, see [security.md](security.md).

## Sessions and SSO are Ed25519-only

Hub-issued session cookies and the hub→spoke SSO handoff are verified with **Ed25519 signatures only**. The legacy symmetric (HMAC) session-cookie lane was deleted ([#3725](https://github.com/kubestellar/hive/pull/3725)), and the fleet-wide shared heartbeat bearer was replaced by per-hive bearers ([#3744](https://github.com/kubestellar/hive/pull/3744)). There is no HMAC fallback for sessions or SSO on v4: a spoke that only holds the retired symmetric `HIVE_SSO_KEY` fails closed.

What that looks like when misconfigured:

- **Spoke has no verification key at all** → SSO renders an error page with HTTP 503: *"This hive has no hub SSO verification key configured…"* and suggests setting `HIVE_HUB_SECRET` (or `HIVE_SSO_PUBLIC_KEY`). Direct GitHub login remains available.
- **Key present but handoff rejected** → HTTP 401 (*expired, malformed, or issued for a different hive*). The spoke log line `sso handoff rejected` has the specific reason; the response body deliberately does not.

## Per-hive derived keys

Every spoke uses keys derived per hive from the hub master secret. The hub reconciles six env vars onto **hosted** spoke Deployments automatically (a 15-minute sweep, [#3754](https://github.com/kubestellar/hive/pull/3754)):

`HIVE_HEARTBEAT_KEY`, `HIVE_SESSION_KEY`, `HIVE_SSO_PUBLIC_KEY`, `HIVE_SESSION_PUBLIC_KEY`, `HIVE_TERMINAL_KEY`, `HIVE_INVITE_KEY`

**Self-hosted spokes are never touched by that sweep** — and do not need to be. Every resolver falls back to deriving the same domain-separated key from `HIVE_HUB_SECRET` + `HIVE_ID`, so a self-hosted spoke that sets those two is byte-identical to a hub-injected one. Setting the six vars explicitly is the least-privilege alternative (the spoke then never holds the master).

Two gotchas:

- **Empty is worse than absent.** The resolvers fall back on empty values just like missing ones, but an empty var can make convergence accounting report the hive as converged when it is not. Unset rather than blank.
- Per-hive env material derives from the **current** master generation ([#3766](https://github.com/kubestellar/hive/pull/3766)) — relevant during rotation, below.

## Master key rotation

The hub master secret supports **generations** ([#3758](https://github.com/kubestellar/hive/pull/3758)): at most two live at once — one CURRENT (mints new material) and one PREVIOUS (verify-only, default window 7 days). Design details: [design/master-key-rotation.md](design/master-key-rotation.md).

- **Trigger:** `POST /api/saas/admin/rotate-master-key` (admin-only; refused while an impersonation grant is active; a second rotation within the 8-hour cooldown returns 409 with `retry_after_seconds`). Inspect with `GET /api/saas/admin/key-generations` and `GET /api/saas/admin/auth-rollout`.
- **Rotation is not complete when the endpoint returns 200.** Spokes converge via the per-hive env reconcile at 3 Deployment patches per 15-minute cycle — roughly 6 hours for a ~70-spoke fleet — and **each patch rolls that spoke's pod once** (interrupting running agents there). Plan rotations accordingly.
- **Dual-generation acceptance** during the window: session cookies ([#3763](https://github.com/kubestellar/hive/pull/3763)), heartbeat bearers (bounded trial verification, [#3767](https://github.com/kubestellar/hive/pull/3767)), and SSO/session public keys (current + previous, [#3778](https://github.com/kubestellar/hive/pull/3778)) all verify against both generations. **Terminal and invite keys have no dual lane** ([#3780](https://github.com/kubestellar/hive/pull/3780)) — in-flight invite links are invalidated by a rotation.
- **Alerts** ([#3776](https://github.com/kubestellar/hive/pull/3776)): `warn` means the previous generation expires within 24h while spokes still carry it — convergence can still finish in time. `stranded` means the window already closed while spokes carry the old generation — those spokes are now failing verification; converge or re-provision them. Retirement is never blocked by an unconverged spoke (otherwise one unreachable cluster could pin the old key alive forever); the alerts are what make it loud.
- **Do not "rotate" by setting a fresh master directly** (new `HIVE_HUB_SECRET` / replacing `/data/saas/hub-secret.key`): that discards all previous generations and strands every spoke at once. The only converging path is the rotate endpoint.
- Self-hosted spokes converge **manually** — the operator re-derives or re-sets their keys; no sweep reaches them.

## Forced proxy egress (F5) and `CAP_NET_ADMIN`

The entrypoint installs an iptables REDIRECT of all outbound `:443` through the MITM proxy, so agents cannot bypass capability enforcement with a raw token. Consequences for deployment:

- **Spokes require `CAP_NET_ADMIN` + iptables to start enforcing.** If the chain cannot be created, the entrypoint exits with:

  ```
  [entrypoint] FATAL: could not establish forced proxy egress (iptables redirect). …
  [entrypoint] FATAL: refusing to start. Grant NET_ADMIN + install iptables, or set HIVE_PROXY_ADVISORY_OK=true to deliberately run in advisory mode.
  ```

  Grant the capability with `--cap-add NET_ADMIN` (docker/podman) or `securityContext.capabilities.add: ["NET_ADMIN"]` (Kubernetes). The escape hatch `HIVE_PROXY_ADVISORY_OK=true` starts the spoke with egress enforcement **advisory-only** — agents can bypass the proxy — and logs a WARN saying exactly that. Chain creation retries 5× with jittered backoff so co-scheduled spokes don't fail in lockstep ([#3717](https://github.com/kubestellar/hive/pull/3717)).

- **The hive binary carries no file capabilities.** Earlier channel images shipped `/usr/local/bin/hive` with a `cap_net_admin+ep` file capability, which made the kernel refuse `execve()` with a bare `Operation not permitted` wherever the container's bounding set lacked `NET_ADMIN` — the crash-loop reported in [#3760](https://github.com/kubestellar/hive/issues/3760). Since [#3794](https://github.com/kubestellar/hive/pull/3794) the binary execs everywhere; the entrypoint instead raises `NET_ADMIN` as an **ambient capability** at the privilege drop, gated on the bounding set actually having it. Without the grant you get a one-line NOTICE (`CAP_NET_ADMIN is not in the bounding set — the forced-proxy egress exemption (SO_MARK) is unavailable…`) instead of a crash.
- **SO_MARK self-exemption:** the proxy marks its own upstream dials so they escape the redirect; on OpenShift/OVN (no `-m owner` match) that mark is the only self-exemption, and `setsockopt(SO_MARK)` needs `NET_ADMIN` in the effective set. Without it the proxy logs once and dials unmarked (degraded self-exemption; agents' forced egress is still installed).
- **Agent identity is UID-based, not self-asserted** (N7, [#3841](https://github.com/kubestellar/hive/issues/3841)): the proxy reads `/proc/net/tcp` to resolve the calling process's UID against `uid-map.json`, which is unforgeable — a process cannot claim another UID's socket. A `Proxy-Authorization: hive <name>` header sent by the caller is a fallback ONLY, and by default the proxy does not trust it: with no UID map (or no match for this connection), the caller is treated as unidentified (`ADVISORY` mode, writes blocked) rather than as whatever name the header claims. `HIVE_PROXY_ADVISORY_OK=true` is the same explicit opt-in used above for a degraded egress gate — it also permits the header fallback, for deployments (local dev, native/systemd installs with no per-agent UID separation) that have no UID map to check against in the first place.
- **Rootless podman:** the image runs (no exec failure), but rootless cannot meaningfully grant `NET_ADMIN`, so the F5 gate cannot be installed — a rootless spoke only starts with `HIVE_PROXY_ADVISORY_OK=true`, i.e. with the capability model unenforced. Treat rootless as unsupported for enforcing deployments.
- **Hosted fleet:** the hub sweeps hosted spoke Deployments every 15 minutes and patches `NET_ADMIN` into the container securityContext where missing ([#3770](https://github.com/kubestellar/hive/pull/3770)). The patch replaces the whole `securityContext` object — capabilities hand-added to a hosted spoke will be erased within 15 minutes. (On OpenShift/OVN the podspec request is necessary but not sufficient; the SCC must also allow it.)

## In-container privilege model

- The hive/proxy process runs as user `dev` (UID 1001); each agent runs as its own UID from `HIVE_UID_BASE=2001` upward.
- `su-exec` is mode **4750 `root:hive-launch`** — only root and members of the pinned `hive-launch` group (GID 1002) can exec it. This closes the earlier world-executable-setuid hole where any agent UID could become root in the pod. `HIVE_LAUNCH_GID=1002` is part of the deployment contract: Kubernetes `fsGroup` and Secret `defaultMode: 0440` rely on the numeric GID, and it must stay in sync across `v2/deploy/k8s/*.yaml` and hub provisioning.

## Supply chain

- **npm:** all global AI-CLI installs (claude-code, codex, copilot, goose-adjacent tooling, etc.) run with `--ignore-scripts` ([#3787](https://github.com/kubestellar/hive/pull/3787), [#3793](https://github.com/kubestellar/hive/pull/3793)).
- **Base images digest-pinned:** `golang`, both `node` stages, and the contributor image's `debian:bookworm-slim` are pinned by digest ([#3811](https://github.com/kubestellar/hive/pull/3811), [#3803](https://github.com/kubestellar/hive/pull/3803)); the vLLM inference image is digest-pinned with a model checksum ([#3813](https://github.com/kubestellar/hive/pull/3813)). Refresh procedure is documented at the top of `v2/Dockerfile`.
- **Tools pinned by SHA:** tmux, ttyd, gh, goose, su-exec (by commit), and friends are checksum-verified at build.
- **Workflows:** `actions/checkout` is SHA-pinned ([#3808](https://github.com/kubestellar/hive/pull/3808)).
- **CODEOWNERS:** `.github/CODEOWNERS` covers the security-sensitive paths (Dockerfiles, workflows, deploy manifests, launch scripts, key/cookie code) ([#3807](https://github.com/kubestellar/hive/pull/3807)). It is **advisory until "Require review from Code Owners" is enabled** in branch protection — deliberately left off because the repo's automation merges green PRs without human review; enabling it is an operator/repo-settings choice.

## Metrics endpoint

`GET /metrics` (Prometheus text format, on the dashboard port) is off unless `HIVE_METRICS_ENABLED` is truthy (`1|true|yes|on`); disabled means the route is not registered (scrapes 404). Because Prometheus cannot do device-flow auth, an enabled `/metrics` bypasses dashboard auth, so the bearer token is its only guard — and it **fails closed**: `HIVE_METRICS_TOKEN` is mandatory whenever metrics are enabled ([#3804](https://github.com/kubestellar/hive/pull/3804)). Enabled-but-tokenless returns 403 (with an error body naming both variables, plus a startup warning); a wrong/missing bearer returns 401. The exposed series are business-sensitive: cumulative estimated cost per model/agent (`hive_estimated_cost_usd*`) and token totals per model.

## Advisory digests

Agent findings are posted as a single, continuously rewritten tracking-issue comment per repo. Since [#3724](https://github.com/kubestellar/hive/pull/3724) each digest is **pinned to one analyzed commit**: the footer cites `owner/repo@<sha>` (branch included), and any finding whose file path no longer exists at that commit is flagged *"file path not found at analyzed commit — finding may be outdated"* instead of showing a dangling `file:line`. Inconclusive path checks (rate limits, transient errors) fail open — a finding is only flagged on a definitive 404.
