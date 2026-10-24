# Threat model: hivecommons/hive

## What this project does and where untrusted input enters

Hive is an autonomous engineering platform. A **hive** is one long-running Go
process (`src/cmd/hive`, package `github.com/hivecommons/hive` rooted at
`src/`) attached to a set of GitHub repositories. It enumerates the open work
in those repositories, decides which AI coding agents to wake (`pkg/governor`,
`pkg/scheduler`), launches each agent CLI (Claude Code, Copilot, Codex,
Goose, …) inside a per-agent tmux server (`pkg/agent/manager_tmux.go`), feeds
it a prompt built from the issues and pull requests it should work on, and
then performs the resulting GitHub writes — branches, pull requests, issues,
comments, reviews, labels, merges — with a **GitHub App installation token**
that the hive mints itself (`pkg/github/app.go`, `pkg/appkey`). A **hub**
(`pkg/hub`, the same `cmd/hive` binary started with `HIVE_MODE=hub`) fronts a
fleet of hives: it hosts
the hosted-SaaS onboarding and OAuth login, provisions per-tenant hives on
Kubernetes (`pkg/hub/saas_provision.go`), receives heartbeats and returns
configuration, and proxies the contributor relay. A **dashboard**
(`pkg/dashboard`, HTTP on port 3001 by default) is the operator UI and API of
one hive.

The scanned branch is `v5`. The authoritative long-form documents are
`src/docs/architecture.md`, `src/docs/security-model.md`,
`src/docs/security-threat-model.md`, `src/docs/acmm-policy-matrix.md`,
`src/docs/github-write-surface.md`, `src/docs/hosted-hub.md`,
`src/docs/review-swarm.md` and `src/docs/agent-configuration.md`; the package
layout under `src/pkg/` is the ground truth when they disagree.

Treat everything below as attacker-controlled:

- **GitHub content fed to agents.** Issue titles, bodies, labels and comments,
  PR titles, bodies, review comments, commit messages and diffs of the
  repositories the hive watches are written by anyone with a GitHub account.
  `pkg/github` fetches them (`ActionableResult`), `pkg/scheduler`
  (`kickmessage.go`, `kick_layout.go`, `kick_priming.go`) renders them into the
  kick prompt, `pkg/promptsrc` assembles the prompt sources, and `pkg/review`
  / `pkg/review/pipeline` feed PR diffs to reviewer agents. `pkg/ioscan`
  (`enforce.go`, `classifier.go`, `canary.go`) and
  `pkg/scheduler/ioscan_enforce.go` are the deterministic redaction layer in
  front of the model. This is the primary prompt-injection boundary: assume
  the agent can be talked into attempting anything, and ask what the
  deterministic layers around it still permit.
- **The agent process itself.** Once prompted, the agent is an untrusted
  local principal. It runs under its own UID (base 2001, launched via
  `su-exec`), inside a tmux server on a per-agent socket, with
  `HTTPS_PROXY` pointed at the hive's GitHub policy proxy
  (`pkg/proxy/github_proxy.go`, rules in `pkg/proxy/rules.go`,
  `linear_rules.go`, `autonomy_levels.go`, `write_surface_enforce.go`,
  UID attribution from `/proc/net/tcp` in `pkg/proxy/procnet.go`). Its only
  sanctioned write path above its tier is a JSON request file dropped into
  `/var/run/hive-metrics/{pr,issue,review,merge,push}-requests`, consumed by
  the relay watchers in `pkg/github` (`pr_request_watcher.go`,
  `issue_request_watcher.go`, `review_request_watcher.go`,
  `merge_request_watcher.go`, `push_branch_request_watcher.go`). Each relay
  authorises in the order documented in `src/docs/github-write-surface.md`:
  file-UID forge check and ACMM mode gate, lane allowlist, repo pause and
  scope, the relay's own content gates, then the GitHub call. Optionally the
  agent runs credential-free in a rootless Podman sandbox (`pkg/sandbox`)
  with pushes performed by the trusted post-step in `pkg/pushbroker`.
- **The GitHub API responses** the proxy and the hive parse (`pkg/proxy`,
  `pkg/github`), including GraphQL error bodies and webhook deliveries
  (`pkg/hub/webhook.go`, verified with `X-Hub-Signature-256`).
- **Spoke → hub traffic.** `POST /api/heartbeat`, `/api/task-status`, the
  NPS/feedback/self-report ingest routes and the contributor relay
  (`/api/contribute/*`) in `pkg/hub/server.go`. Heartbeats are authenticated
  with a per-hive bearer derived from the hub master secret
  (`pkg/keyderive`; `verifyHeartbeatBearer` in `pkg/hub/hub_keys.go`), and
  the spoke verifies the hub's signed response in `pkg/spoke/verifier.go`
  and `heartbeat_sig.go`; the payload is JSON from a
  machine the hub does not fully trust, and the response is how the hub
  delivers GitHub App credentials and configuration back to that one hive.
- **Hub HTTP.** The hosted hub (`https://hive.hivecommons.dev`) is
  Internet-facing: GitHub/Google/OIDC OAuth login (`pkg/hub/oauth.go`,
  `pkg/auth`), Ed25519-signed session cookies (`pkg/hub/saas_auth.go`,
  `saas_crypto.go`), the nginx `auth_request` endpoint that decides whether
  a user may reach a given spoke and injects `X-Hive-User` / `X-Hive-Role`
  / a proxy-auth proof, admin impersonation (`pkg/hub/impersonate_cookie.go`),
  access grants (`saas_access.go`), the provisioning wizard, and the public
  registry/leaderboard/stats routes.
- **Dashboard HTTP.** `pkg/dashboard/server.go` (`authenticate`,
  `NewServerWithAuth`), `api_auth.go`, `session.go`,
  `session_live_role.go`. Three deployment shapes exist and each has its
  own trust assumptions: behind the hub (identity headers trusted only with
  the hub's proxy-auth proof), direct-route (GitHub device-flow login against
  `HIVE_AUTHORIZED_USERS`; inbound `X-Hive-User`/`X-Hive-Role` must be
  stripped), and standalone with an optional shared bearer
  `dashboard.auth_token` / `DASHBOARD_AUTH_TOKEN`. Read-role users must not
  be able to write; owner-only routes include config edits, agent control,
  GitHub App setup, release-channel selection and self-upgrade
  (`api_version.go` `handleSelfUpgrade`, `deployment_upgrade.go`).
- **Model-gateway traffic.** Agents talk to inference backends through the
  in-pod translator (`pkg/proxy/inference_*.go`, `anthropic_translate.go`,
  `inference_auth.go`) using a per-agent placeholder key
  `sk-hive-<agent>`; the real gateway/OpenRouter/watsonx key is swapped in
  server-side (`pkg/openrouter`, `pkg/watsonx`, `pkg/inference`).
  `cmd/apiproxy` is the standalone variant gated by `PROXY_AUTH_TOKEN`.
- **Configuration and secrets on disk.** `hive.yaml` (`pkg/config`), the
  dashboard-persisted overlay, the App private key files (`pkg/appkey`), the
  per-agent token caches, `/data/secrets/*`, knowledge and evidence stores
  (`pkg/knowledge/api.go` writes under `knowledgeBaseDir`; `pkg/evidence`
  writes signed evidence records, `sign.go`). Config is operator-authored and
  trusted; the *contents* of the stores are partly agent- and
  GitHub-derived and are not.
- **Self-upgrade.** The dashboard can pull and roll to a new container image
  (`pkg/dashboard/api_version.go`, `pkg/imageref`), and the hub can drive
  fleet upgrades (`pkg/hub/*upgrade*`). The image reference and release
  channel come from the hub; registry responses are untrusted.

## Components that matter most / least

Most important, in this order:

1. **The agent → GitHub boundary.** `pkg/proxy` (`rules.go`,
   `github_proxy.go`, `write_surface_enforce.go`, `autonomy_levels.go`,
   `procnet.go`, `agent_github_budget.go`), the relay watchers and
   `SweepSentinel` in `pkg/github`, `pkg/sentinel` (`Evaluate`),
   `pkg/pushbroker`, `pkg/sandbox`, `pkg/agent` (`mode.go`, token
   delivery, tmux launch, per-UID isolation), `pkg/mint` and
   `pkg/github/app.go` (installation-token minting and tier scoping),
   `pkg/appkey`. Any way an agent — or GitHub content acting through an
   agent — performs a write above its ACMM tier, outside the repo
   allowlist, as a different agent, or bypasses the hard-denied REST
   `POST /repos/{o}/{r}/pulls` / `PUT …/pulls/{n}/merge` paths; reads the
   full installation token, another agent's token, a gateway key or the
   dashboard token; pushes to a default branch or rewrites a branch its lane
   did not create; or escapes its UID/tmux/sandbox scope, is the top of the
   list.
2. **Hub authentication and tenant isolation.** `pkg/hub` (`oauth.go`,
   `saas_auth.go`, `saas_access.go`, `saas_crypto.go`,
   `impersonate_cookie.go`, `hub_keys.go`, `perhive_env_reconcile.go`,
   `saas_provision.go`, `server.go` heartbeat/registry/contribute handlers,
   `webhook.go`), `pkg/keyderive`, `pkg/delegation`, `pkg/terminalassert`,
   `pkg/spoke`. Session or SSO forgery, an `auth_request` decision that
   admits a user to a hive they were not granted, a heartbeat from hive A
   that changes or reads hive B's configuration or App credentials, a
   provisioning path that lands another tenant's secret, or SSRF from the
   hub to arbitrary spokes/cluster addresses via a tenant-supplied URL.
3. **Dashboard authentication and role enforcement.** `pkg/dashboard`
   (`server.go` `authenticate`, `api_auth.go`, `session.go`,
   `session_live_role.go`, `owner_only_handlers_deny_test.go` names the
   owner-only set, `api_version.go`, `api_config_export.go`,
   `api_snapshot.go`, the `contribute_*.go` relay admission code). Auth
   bypass, role confusion between hub-proxied and direct-route modes,
   header-forgery, CSRF on state-changing routes, token/secret disclosure
   through config export or snapshots, and self-upgrade to an attacker
   chosen image.
4. **Command and argument construction from GitHub-controlled strings.**
   Every `exec.Command` of `git`, `gh`, `tmux`, `podman`, `su-exec` or a
   shell whose arguments include a branch name, repo name, issue title,
   label, login or file path that came from GitHub or from an agent
   (`pkg/agent`, `pkg/pushbroker`, `pkg/sandbox`, `pkg/crux/gitidentity`,
   `pkg/github`, `pkg/knowledge`, `cmd/hive`). `tmux send-keys` to an agent
   pane with attacker text, `git` option injection (`-c`, `--upload-pack`,
   refspecs starting with `-`), or a path that escapes the intended
   directory.
5. **The prompt-injection defences and secret scrubbing.** `pkg/ioscan`,
   `pkg/scheduler/ioscan_enforce.go`, `pkg/logscrub`, `pkg/promptsrc`
   size caps, `pkg/toolapprove`, `pkg/claims`. A bypass of a deterministic
   rule the docs say holds (zero-width stripping, base64 rescans, canary
   detection, token redaction in logs/PR bodies) is a real finding; a model
   that follows a plain-English instruction the classifier did not flag is
   not (see "leave alone").
6. **File stores keyed by untrusted names.** `pkg/knowledge`,
   `pkg/evidence`, `pkg/beads`, `pkg/trajectory`, `pkg/timeline`,
   `pkg/snapshot`, `pkg/hubbackup`/`pkg/spokebackup`, the relay request
   directories. Path traversal from a repo/agent/issue-derived component,
   symlink following into `/data/secrets` or the App key, world-readable
   token files.
7. **`cmd/apiproxy`, `pkg/apiproxy`, `pkg/proxy/inference_*`.** Leaking the
   upstream key to an unauthenticated caller, or letting one agent's
   placeholder spend as another's.

Lower priority but in scope: `pkg/governor`, `pkg/worksource`,
`pkg/linearagent` (Linear is a second forge; the same tier gate applies),
`pkg/discord`, `pkg/notify`, `pkg/tui`, `pkg/hivectl`, `cmd/bd` and
`pkg/beads` (local work ledger), the shell pipeline under `bin/`
(`run-pipeline.sh`, `hive-config.sh`), `install.sh`/`uninstall.sh`, the
systemd/launchd/Quadlet units and `src/deploy/` (nginx gateway config,
`entrypoint.sh`).

Least important / out of scope: `src/docs/`, `docs/`, `changelog.d/`,
`CHANGELOG.md`, `examples/`, the static dashboard and hub assets
(`pkg/dashboard/static`, `pkg/hub/static`, `pkg/hub/assets`) except where a
finding is XSS or a CSP/token-injection bug that reaches an authenticated
operator's browser (`src/proxy/*.test.js` are the gateway tests for those
headers), `src/internal/testutil`, test fixtures under any `testdata/`,
`pkg/spektacular`, `cmd/flue-fixture`, GitHub Actions
workflows under `.github/`, and `cov*.out` coverage artefacts.

## How to exercise it

The image has only loopback networking. `GOPROXY=off`, `GOSUMDB=off` and
`GOTOOLCHAIN=local` are set, every module is in the module cache, every
package and test binary has been compiled, and the deployable binaries are
in `/usr/local/bin` (`hive`, `hivectl`, `bd`, `hive-backup`, `apiproxy`).
The Go module is `/src/src`; run `go` commands from there.

- **Tests.** `go test -short ./pkg/<name>/...` for any package. The image
  runs as root, so the few tests in `pkg/appkey` and `pkg/config` that chmod
  a directory read-only and expect a write to fail report FAIL here; they
  pass in CI and are not findings. `pkg/hub`
  and `pkg/agent` are large and are sharded across many runners in CI; use
  `-run` to scope them. `pkg/agent` and parts of `pkg/dashboard` spawn real
  tmux servers and send signals by UID — this is fine inside the container
  but do not run them on a host that is also running a hive. Table-driven
  tests next to the code under test are the normal place for a regression
  test; `src/internal/testutil` has shared helpers and `pkg/proxy/*_test.go`
  shows how to stand up the GitHub policy proxy against an `httptest`
  upstream.
- **The policy proxy.** `pkg/proxy/rules.go` is a readable table of
  `(method, path regexp) → minimum agent mode`; `agent_repo_scope_test.go`,
  `autonomy_enforcement_test.go`, `auth_inject_test.go` and
  `write_surface_enforce.go` tests show how to drive a request through it as
  an agent of a given mode and assert `403` with `X-Hive-Proxy-Blocked`.
  `pkg/proxy/denial_message.go` is the operator-facing denial text.
- **The relays.** `pkg/github/*_request_watcher.go` with their tests show
  the JSON request shape and the authorisation order; `pkg/sentinel`
  `Evaluate` is a pure function over a PR's author, title and unified diff.
- **A hive on loopback.** `hive --version` works with no config. For a live
  dashboard, copy `/src/src/hive.yaml.example` to a writable path, set a
  fake GitHub token (`github.token`) and a small `agents:` list, point the
  data directory at a scratch dir, and run
  `hive --config /path/hive.yaml`; the dashboard answers on
  `http://127.0.0.1:3001` (`/api/health`, `/api/*`). Without network it will log
  GitHub failures and keep serving; set `dashboard.auth_token` to test the
  bearer path and `dashboard.authorized_users` for direct-route sessions.
  `HIVE_MODE=hub hive --config …` with a `hub:` section starts the hub on
  loopback (`cmd/hive/main.go` switches on `HIVE_MODE`); see
  `pkg/hub/server.go` route registration (around `s.mux.HandleFunc`) for
  the surface. `apiproxy --port 9000 --upstream http://127.0.0.1:1` with
  `PROXY_AUTH_TOKEN` set exercises the standalone gateway.
- **Deployment context.** `src/Dockerfile`, `src/deploy/entrypoint.sh`,
  `src/deploy/nginx.conf`, the Kubernetes manifests under `src/deploy/k8s/`
  and `systemd/`/`launchd/` show the production process model (per-agent
  UIDs, `su-exec` 4750, PVC layout, which ports are published). A finding
  that depends on a non-default deployment should say so.

## How you rate severity

A finding counts when someone who is *not* the hive operator — a GitHub user
filing an issue or PR, an agent that has been prompt-injected, another
tenant on the hosted hub, or an unauthenticated network peer — can make a
hive or hub do something its ACMM level, role model or tenant boundary says
it must not. Say which deployment shape (hub-proxied, direct-route,
standalone; L1–L6) the reproducer needs; a finding against the defaults
weighs more than one that needs an opt-in switched on.

- **Critical.** A GitHub write above the agent's tier, outside the repo
  allowlist, bypassing the hard-denied REST PR create/merge paths, or
  attributed to a different agent/lane. Any read of the full installation
  token, the App private key, another agent's token, a gateway/OpenRouter/
  watsonx/Linear credential or the dashboard token from an agent's
  position. Merging or pushing to a default branch below L6 or without the
  relay's CI/hold gates. Escaping the per-agent UID, tmux server or Podman
  sandbox (including via `su-exec`). Command or argument injection from
  GitHub-controlled strings into `git`, `gh`, `tmux`, `podman` or a shell
  run by the hive process. Authentication bypass or session/SSO forgery on
  the hub or dashboard; a `read`-role user performing a write; any
  cross-tenant read or write on the hub (heartbeat, provisioning, access
  grants, impersonation). Self-upgrade to an attacker-chosen image.
- **High.** SSRF from the hub or a spoke to an attacker-chosen address
  using a tenant- or GitHub-supplied URL. Path traversal or symlink
  following in the knowledge/evidence/beads/relay stores that reads or
  writes outside the intended directory. Disclosure of secrets through
  logs, config export, snapshots, PR/issue bodies or heartbeats that
  `pkg/logscrub`/the secret-free overlay are meant to prevent. CSRF on an
  owner-only dashboard route. A deterministic `pkg/ioscan` rule (unicode
  normalisation, base64 rescan, canary, token patterns) that a crafted
  payload bypasses while the docs state it holds. A heartbeat or webhook
  accepted without the per-hive bearer / `X-Hub-Signature-256` check. Stored
  XSS reaching an operator session on the dashboard or hub.
- **Medium.** Denial of service of a single hive or hub from an
  unauthenticated or GitHub-content position (panic that takes the process
  down rather than one request, unbounded memory from a crafted issue body,
  heartbeat or webhook, goroutine leaks per request, a relay request that
  wedges the watcher). Information disclosure that is not a credential
  (other tenants' hive names, internal addresses, agent transcripts to a
  `read` user). Rate-limit or token-budget bypass that only costs the
  operator money. A proxy parsing differential where the rule table sees a
  different repo/path than GitHub acts on, when it does not reach a write
  above tier (then it is critical).
- **Low.** Defects that need `dashboard.auth_token` unset *and* no
  authorized-user list on a dashboard deliberately exposed off-loopback,
  a non-default debug flag, or a hive run as root outside the shipped
  images. Weak defaults in example configs. Panics confined to one request
  that `net/http` recovers. Hardening suggestions without a demonstrated
  path.
- **Out of scope / not a vulnerability.** Anything that requires the
  operator's own `hive.yaml`, environment, PVC or Kubernetes namespace to be
  malicious — the operator is the trust root. L6 auto-merge of an
  agent-authored PR that passed CI when the operator configured L6. Agents
  being able to read the repositories they were granted. A model following
  an instruction in plain English that `pkg/ioscan`'s optional, fail-open
  classifier did not score as injection (the deterministic layers are the
  floor; the classifier is best-effort by design). Missing TLS on
  loopback listeners.

## How to report

- Give a reproducer that runs in this image: a Go test added next to the
  code under test, or a `curl`/shell sequence against `hive` or `apiproxy`
  on loopback, with the config file used and the exact request. For a
  proxy/relay finding, name the agent mode (`ADVISORY`, `ISSUES_ONLY`,
  `ISSUES_AND_PRS`, `ISSUES_PRS_MERGE` — `pkg/agentmode`, re-exported by
  `pkg/agent/mode.go`), the ACMM level and
  the repo scope in force, and show the request that should have been
  refused and the write that landed. For a hub finding, show the two
  tenants and the session/bearer each one holds.
- Propose a minimal patch against `v5` with a regression test and a
  one-bullet `changelog.d/security-<slug>.md` fragment. Keep refactoring out
  of it; `gofmt` it.
- Report one root cause once. A flaw reachable through several relays or
  several routes is one finding that lists each path.

## Anything to leave alone

- Third-party agent CLIs (Claude Code, Copilot CLI, Codex, Goose,
  Antigravity, `gh`), GitHub, Linear, Discord, container runtimes and the
  inference providers are not in this repository; a bug that is wholly
  inside one of them belongs to its own process. Hive's *use* of them is
  in scope.
- Model behaviour per se — hallucinated reviews, poor code, a jailbroken
  agent *attempting* something — is not a finding unless the attempt
  succeeds past a deterministic control.
- Intentional autonomy: at L6, with reporter trust off, Hive is designed to
  merge agent PRs that pass CI without a human `/lgtm`
  (`src/docs/securing-your-hive.md` explains this at length). At L5 every
  agent PR carries `hold`. Do not report the configured behaviour.
- The agent sandbox is per-UID + tmux + policy proxy, not per-run microVMs,
  and `src/docs/security-threat-model.md` already lists this as a known
  residual risk (hivecommons/hive#2804). Report concrete escapes, not the
  architecture.
- The `pkg/ioscan` semantic classifier and trajectory review
  (`pkg/trajectory`) fail open on model outage by design.
- Agents keep read access to the GitHub repos the App is installed on and
  outbound access to their model backend; "an agent can read the repo" or
  "can reach api.anthropic.com" is not a finding.
- Standalone `hive` with no `dashboard.auth_token` and no authorized users
  is documented as unauthenticated local/dev mode; the shipped images and
  hub provisioning always set one.
- Timing differences in constant-time comparisons that already use
  `crypto/subtle`, and the absence of rate limiting on loopback-only
  listeners.
