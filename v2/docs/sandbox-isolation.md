# Sandbox isolation and agent guardrails

Hive's v2 isolation model is defense in depth rather than a single sandbox switch. Agents are long-lived CLI sessions in the Hive container. Isolation comes from per-agent Unix users, scoped credentials, the ACMM mode pipeline, and a local GitHub proxy that can inspect selected HTTPS traffic.

## What Hive isolates

- **Agent work directories** — the entrypoint creates `/data/agents/<agent>` for each configured agent and, when UID isolation is available, owns it as `hive-<agent>:node`.
- **Agent process identity** — the entrypoint assigns UIDs starting at `2001`, creates `hive-<agent>` system users, and writes `/var/run/hive/uid-map.json`. The Go manager reads that map and launches each agent's tmux server through `su-exec` as the mapped user.
- **GitHub write identity** — push-capable agents can receive per-agent App token cache paths; advisory agents have `GH_TOKEN` and `GITHUB_TOKEN` stripped from tmux sessions.
- **GitHub API writes** — agent traffic is pointed at the local proxy with `HTTPS_PROXY=http://127.0.0.1:18443` and `HTTP_PROXY=http://127.0.0.1:18443`. When iptables setup succeeds, outbound TCP port 443 is also redirected to the same proxy so unsetting proxy environment variables is not enough to bypass it.
- **Merge and PR gates** — policy mode, hold labels, green checks, self-merge bans, and automerge logic run outside the prompt and remain enforcement points even if an agent proposes unsafe work.

## MITM GitHub proxy

The Go proxy listens on `127.0.0.1:18443`. It accepts both explicit HTTP `CONNECT` proxy traffic and transparent TLS connections redirected by iptables.

For `api.github.com`, the proxy forges a leaf certificate signed by Hive's local CA, terminates TLS from the agent, opens a TLS connection to GitHub, reads each HTTP request, and then either forwards or blocks it. The rule table allows read methods in advisory mode, selected issue writes in `issues-only`, PR and git-push writes in `issues-and-prs`, and merge/update-branch requests in `issues-prs-merge`. Unknown operations are denied by default. Direct `POST /repos/<org>/<repo>/pulls` is hard-denied in every mode so agents use Hive's `hive-open-pr` path instead of authoring PRs as the login user.

The proxy also checks write requests under `/repos/<org>/<repo>` against the repositories configured for the hive. A blocked request is logged as `proxy request blocked` and returns HTTP 403 with `X-Hive-Proxy-Blocked: true`.

### Traffic that is not request-inspected

The current `NeedsMITM` implementation only returns true for `api.github.com`. Other hosts are tunneled without request-level ACMM inspection, including non-GitHub hosts. `github.com` is registered as a GitHub host but is intentionally tunneled for OAuth device flow and git smart HTTP paths. GitHub Enterprise hosts can be registered as GitHub hosts, but this code path does not MITM them unless `NeedsMITM` is extended.

Copilot completion hosts (`githubcopilot.com` and `*.githubcopilot.com`) may be MITM-inspected only for token-usage accounting when an agent is identified and a token sink is configured. The proxy forwards the request and response content, except that streaming completion requests can get `stream_options.include_usage` injected so Copilot returns token usage.

## Proxy CA and trust

The proxy persists its CA certificate and key at:

- `/data/proxy-ca.pem`
- `/data/proxy-ca-key.pem`

On startup, it reuses a valid persisted CA or generates a new `Hive ACMM Proxy CA`. The entrypoint builds a combined bundle at `/data/proxy-ca-bundle.pem` by concatenating the system CA bundle and `/data/proxy-ca.pem` when both files exist. That matters because `SSL_CERT_FILE` replaces the trust set: the combined bundle trusts both public roots and the proxy CA.

Trust is injected in several places:

- Agent shells get `.bashrc` that sets `SSL_CERT_FILE` to `/data/proxy-ca-bundle.pem` when available, otherwise `/data/proxy-ca.pem`.
- Agent launch environments set `NODE_EXTRA_CA_CERTS=/data/proxy-ca.pem` and `GIT_SSL_CAINFO=/data/proxy-ca.pem`.
- The entrypoint installs the proxy CA into the system trust store when it can, and watches for CA changes to rebuild the combined bundle.
- GitHub App token-minting clients in Go build their own trust pool from system roots plus `/data/proxy-ca.pem`, lazily reloading the CA so fresh PVC boots do not depend on entrypoint ordering.

## Verifying isolation is active

On a running hive container, verify the concrete mechanisms rather than assuming the policy level is enough:

1. Check startup logs for per-agent user creation, for example `Agent user: hive-<agent> (UID <uid>)`, and for `UID map written to /var/run/hive/uid-map.json`.
2. Inspect `/var/run/hive/uid-map.json`; `agents` should map agent names to UIDs and `iptables_active` should be `true` when forced egress was installed.
3. Check startup logs for `iptables: outbound :443 -> :18443`. If you see `WARN: iptables chain creation failed` or `WARN: iptables not found`, proxy enforcement is advisory-only and relies on the agent environment variables.
4. Check Go logs for `proxy listening` on `127.0.0.1:18443` and, when the UID map loaded, `proxy loaded UID map`.
5. Confirm `/data/proxy-ca.pem` exists. When the system bundle exists too, `/data/proxy-ca-bundle.pem` should be rebuilt by the entrypoint.
6. For a negative test, make a GitHub API write that the current agent mode does not allow. The expected result is HTTP 403, `X-Hive-Proxy-Blocked: true`, and a `proxy request blocked` log line.

## Practical limits

- This is not a kernel sandbox. Agents still execute the configured CLI and tools in the container with access to their workspace and to any credentials deliberately injected for that mode.
- UID isolation separates agent-owned files and lets the proxy attribute sockets by UID, but all agents still share the container, the `node` group, and selected shared state such as `/data/home` unless a backend uses a dedicated home path.
- If iptables cannot be installed, agents can bypass request-level proxy enforcement by ignoring or removing proxy environment variables.
- The proxy's request-level ACMM checks apply to `api.github.com` in the current code. Non-GitHub endpoints and tunneled GitHub web traffic are not inspected at the HTTP request level by this proxy.
- Advisory mode does not mean network isolation. It means GitHub writes should be denied by the proxy/ruleset and by stripped GitHub write tokens; read traffic and non-inspected destinations can still occur.

Use the rest of this index for setup details: [ACMM policy matrix](acmm-policy-matrix.md), [Agent configuration](agent-configuration.md), and [Contributor trust tiers and delegated agent roles](contributor-trust-and-roles.md).
