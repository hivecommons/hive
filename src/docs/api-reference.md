# Dashboard REST API reference

Pragmatic v1 endpoint index compiled by hand from route registrations in `src/pkg/dashboard/*.go` and `src/pkg/hub/*.go` (tests excluded). There is no generator; update this page in the same change that adds or renames a route. It lists method, path, coarse auth level, and one-line purpose. Request/response schemas are intentionally not hand-written here; see the handler source for exact payloads and validation.

Auth levels are derived from dashboard middleware (`isPublicPath`, dashboard token/session auth, and the `/api/v1` GitHub-token wrapper) or from hub route wrappers such as `requireAuth`. Hub rows marked handler-specific have no dashboard middleware; check the named handler for bearer secrets, admin checks, or public behavior.

## Health, status, events

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/version` | Dashboard auth/session | Build/version metadata | `pkg/dashboard/api.go:38` |
| `GET` | `/api/health` | Public | Basic health probe | `pkg/dashboard/server.go:667` |
| `GET` | `/api/health/deep` | Public | Deep health probe | `pkg/dashboard/server.go:668` |
| `GET` | `/api/livez` | Public | Kubernetes liveness probe | `pkg/dashboard/server.go:669` |
| `GET` | `/metrics` | Registered only when `HIVE_METRICS_ENABLED`; requires `Authorization: Bearer $HIVE_METRICS_TOKEN` (403 if the token is unset) | Prometheus metrics | `pkg/dashboard/server.go:674` |
| `GET` | `/api/status` | Dashboard auth/session | Dashboard aggregate status | `pkg/dashboard/server.go:676` |
| `GET` | `/api/events` | Dashboard auth/session | Server-sent event stream | `pkg/dashboard/server.go:677` |

## Snapshots and style

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/style` | Public | Sanitized custom dashboard CSS | `pkg/dashboard/api.go:39` |
| `GET` | `/api/snapshot/frame-ancestors` | Public | Snapshot framing allowlist | `pkg/dashboard/api.go:56` |
| `GET` | `/api/snapshot` | Public | Snapshot data | `pkg/dashboard/api.go:57` |
| `GET` | `/snapshot` | Public | Public read-only snapshot page | `pkg/dashboard/api.go:58` |

## Auth and external accounts

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/gh-auth` | Dashboard auth/session | GitHub Auth | `pkg/dashboard/api.go:92` |
| `GET` | `/api/gh-rate-limits` | Dashboard auth/session | GitHub Rate Limits | `pkg/dashboard/api.go:93` |
| `GET` | `/api/gh-user-auth/status` | Public | GitHub User Auth Status | `pkg/dashboard/api.go:94` |
| `POST` | `/api/gh-user-auth/start` | Public | GitHub User Auth Start | `pkg/dashboard/api.go:95` |
| `POST` | `/api/gh-user-auth/poll` | Public | GitHub User Auth Poll | `pkg/dashboard/api.go:96` |
| `POST` | `/api/gh-user-auth/logout` | Public | GitHub User Auth Logout | `pkg/dashboard/api.go:97` |
| `GET` | `/api/gh-user-auth/session` | Public | GitHub User Auth Session | `pkg/dashboard/api.go:98` |
| `GET` | `/api/claude-auth/status` | Dashboard auth/session | Claude Auth Status | `pkg/dashboard/claude_auth.go:42` |
| `POST` | `/api/claude-auth/start` | Dashboard auth/session | Claude Auth Start | `pkg/dashboard/claude_auth.go:43` |
| `POST` | `/api/claude-auth/exchange` | Dashboard auth/session | Claude Auth Exchange | `pkg/dashboard/claude_auth.go:44` |
| `POST` | `/api/claude-auth/logout` | Dashboard auth/session | Claude Auth Logout | `pkg/dashboard/claude_auth.go:45` |
| `GET` | `/api/copilot-auth/status` | Dashboard auth/session | Copilot Auth Status | `pkg/dashboard/copilot_auth.go:69` |
| `POST` | `/api/copilot-auth/start` | Dashboard auth/session | Copilot Auth Start | `pkg/dashboard/copilot_auth.go:70` |
| `POST` | `/api/copilot-auth/logout` | Dashboard auth/session | Copilot Auth Logout | `pkg/dashboard/copilot_auth.go:71` |
| `GET` | `/api/openrouter/connect/start` | Dashboard auth/session | Open Router Start | `pkg/dashboard/openrouter.go:66` |
| `GET` | `/api/openrouter/qr` | Dashboard auth/session | Open Router QR | `pkg/dashboard/openrouter.go:67` |
| `GET` | `/api/openrouter/models` | Dashboard auth/session | Open Router Models | `pkg/dashboard/openrouter.go:68` |
| `GET` | `/api/openrouter/credit` | Dashboard auth/session | Open Router Credit | `pkg/dashboard/openrouter.go:69` |
| `GET` | `/openrouter/callback` | Public | Open Router Callback | `pkg/dashboard/openrouter.go:70` |
| `POST` | `/api/github-app/recheck` | Dashboard auth/session | GitHub App Recheck | `pkg/dashboard/server.go:678` |
| `POST` | `/api/github-app/install-clicked` | Dashboard auth/session | GitHub App Install Clicked | `pkg/dashboard/server.go:679` |
| `GET` | `/gh-setup` | Public | GitHub App Setup Callback | `pkg/dashboard/server.go:680` |

## Configuration

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/config` | Dashboard auth/session | Config | `pkg/dashboard/api.go:40` |
| `GET` | `/api/config/download` | Dashboard auth/session | Config Download | `pkg/dashboard/api.go:41` |
| `GET` | `/api/config/provenance` | Dashboard auth/session | Config Provenance | `pkg/dashboard/api.go:42` |
| `GET` | `/api/config/variables` | Dashboard auth/session | Variables List | `pkg/dashboard/api.go:43` |
| `GET` | `/api/config/authorized-users` | Dashboard auth/session | Authorized Users List | `pkg/dashboard/api.go:44` |
| `PUT` | `/api/config/variables/{name}` | Dashboard auth/session | Variable Upsert | `pkg/dashboard/api.go:45` |
| `DELETE` | `/api/config/variables/{name}` | Dashboard auth/session | Variable Delete | `pkg/dashboard/api.go:46` |
| `GET` | `/api/config/agent/{name}` | Dashboard auth/session | Agent Config Get | `pkg/dashboard/api.go:105` |
| `PUT` | `/api/config/agent/{name}/general` | Dashboard auth/session | Agent Config General | `pkg/dashboard/api.go:106` |
| `PUT` | `/api/config/agent/{name}/cadences` | Dashboard auth/session | Agent Config Cadences | `pkg/dashboard/api.go:107` |
| `PUT` | `/api/config/agent/{name}/models` | Dashboard auth/session | Agent Config Models | `pkg/dashboard/api.go:108` |
| `PUT` | `/api/config/agent/{name}/pipeline` | Dashboard auth/session | Agent Config Pipeline | `pkg/dashboard/api.go:109` |
| `PUT` | `/api/config/agent/{name}/hooks` | Dashboard auth/session | Agent Config Hooks | `pkg/dashboard/api.go:110` |
| `PUT` | `/api/config/agent/{name}/restrictions` | Dashboard auth/session | Agent Config Restrictions | `pkg/dashboard/api.go:111` |
| `PUT` | `/api/config/agent/{name}/stats` | Dashboard auth/session | Agent Config Stats | `pkg/dashboard/api.go:112` |
| `GET` | `/api/config/agent/{name}/prompt` | Dashboard auth/session | Agent Prompt | `pkg/dashboard/api.go:113` |
| `PUT` | `/api/config/agent/{name}/prompt` | Dashboard auth/session | Agent Prompt Save | `pkg/dashboard/api.go:114` |
| `GET` | `/api/config/agent/{name}/export` | Dashboard auth/session | Agent Export | `pkg/dashboard/api.go:115` |
| `PUT` | `/api/config/agent/{name}/channels` | Dashboard auth/session | Agent Config Channels | `pkg/dashboard/api.go:116` |
| `PUT` | `/api/config/agent/{name}/tools` | Dashboard auth/session | Agent Config Tools | `pkg/dashboard/api.go:117` |
| `PUT` | `/api/config/agent/{name}/connections` | Dashboard auth/session | Agent Config Connections | `pkg/dashboard/api.go:118` |
| `GET` | `/api/config/stat-sources` | Dashboard auth/session | Stat Sources | `pkg/dashboard/api.go:119` |
| `GET` | `/api/config/governor` | Dashboard auth/session | Governor Config Get | `pkg/dashboard/api.go:121` |
| `PUT` | `/api/config/governor/sensing` | Dashboard auth/session | Governor Sensing | `pkg/dashboard/api.go:122` |
| `PUT` | `/api/config/governor/thresholds` | Dashboard auth/session | Governor Thresholds | `pkg/dashboard/api.go:123` |
| `PUT` | `/api/config/governor/labels` | Dashboard auth/session | Governor Labels | `pkg/dashboard/api.go:124` |
| `PUT` | `/api/config/governor/budget` | Dashboard auth/session | Governor Budget | `pkg/dashboard/api.go:125` |
| `PUT` | `/api/config/governor/notifications` | Dashboard auth/session | Governor Notifications | `pkg/dashboard/api.go:126` |
| `PUT` | `/api/config/governor/health` | Dashboard auth/session | Governor Health | `pkg/dashboard/api.go:127` |
| `PUT` | `/api/config/governor/logging` | Dashboard auth/session | Governor Logging | `pkg/dashboard/api.go:128` |
| `PUT` | `/api/config/governor/attribution` | Dashboard auth/session | Governor Attribution | `pkg/dashboard/api.go:129` |
| `PUT` | `/api/config/governor/hub` | Dashboard auth/session | Governor Hub | `pkg/dashboard/api.go:130` |
| `PUT` | `/api/config/governor/litellm` | Dashboard auth/session | Governor Lite LLM | `pkg/dashboard/api.go:131` |
| `PUT` | `/api/config/governor/trajectory` | Dashboard auth/session | Governor Trajectory | `pkg/dashboard/api.go:132` |
| `GET` | `/api/config/governor/backup` | Owner only | Backup Key Status (presence + safe source label; never the key value) | `pkg/dashboard/backup_key.go` |
| `PUT` | `/api/config/governor/backup` | Owner only | Backup Key Set (64-hex AES-256 key; stored 0600, path-only in `hive.yaml`) | `pkg/dashboard/backup_key.go` |
| `DELETE` | `/api/config/governor/backup` | Owner only | Backup Key Clear (backups are refused again) | `pkg/dashboard/backup_key.go` |
| `GET` | `/api/config/governor/bob` | Dashboard auth/session | Governor Bob Status | `pkg/dashboard/api.go:136` |
| `PUT` | `/api/config/governor/bob` | Dashboard auth/session | Governor Bob Key | `pkg/dashboard/api.go:137` |
| `DELETE` | `/api/config/governor/bob` | Dashboard auth/session | Governor Bob Key Clear | `pkg/dashboard/api.go:138` |
| `POST` | `/api/config/governor/bob/test` | Dashboard auth/session | Governor Bob Key Test | `pkg/dashboard/api.go:140` |
| `POST` | `/api/config/governor/litellm/test` | Dashboard auth/session | Governor Lite LLMTest | `pkg/dashboard/api.go:141` |
| `GET` | `/api/config/governor/gateways` | Dashboard auth/session | Governor Gateways List | `pkg/dashboard/api.go:142` |
| `PUT` | `/api/config/governor/gateways` | Dashboard auth/session | Governor Gateways Upsert | `pkg/dashboard/api.go:143` |
| `DELETE` | `/api/config/governor/gateways/{name}` | Dashboard auth/session | Governor Gateways Delete | `pkg/dashboard/api.go:144` |
| `POST` | `/api/config/governor/gateways/{name}/test` | Dashboard auth/session | Governor Gateways Test | `pkg/dashboard/api.go:145` |
| `POST` | `/api/config/governor/gateways/discover` | Dashboard auth/session | Governor Gateways Discover | `pkg/dashboard/api.go:146` |
| `POST` | `/api/config/governor/agents` | Dashboard auth/session | Governor Add Agent | `pkg/dashboard/api.go:148` |
| `DELETE` | `/api/config/governor/agents/{name}` | Dashboard auth/session | Governor Remove Agent | `pkg/dashboard/api.go:149` |
| `PUT` | `/api/config/governor/repos` | Dashboard auth/session | Governor Repos | `pkg/dashboard/api.go:150` |
| `POST` | `/api/config/governor/repos/check-access` | Dashboard auth/session | Governor Repo Check Access | `pkg/dashboard/api.go:154` |
| `PUT` | `/api/config/github` | Dashboard auth/session | Config GitHub | `pkg/dashboard/api.go:155` |
| `GET` | `/api/config/github/forge-apps` | Dashboard auth/session | Config GitHub Forge Apps | `pkg/dashboard/api.go:158` |
| `GET` | `/api/config/sidebar` | Dashboard auth/session | Sidebar Get | `pkg/dashboard/api.go:172` |
| `PUT` | `/api/config/sidebar` | Dashboard auth/session | Sidebar Set | `pkg/dashboard/api.go:173` |
| `GET` | `/api/config/backends` | Dashboard auth/session | Backends | `pkg/dashboard/api.go:174` |

## Agents and controls

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `POST` | `/api/kick/{agent}` | Dashboard auth/session | Kick — asynchronous; answers `202` once queued (see below) | `pkg/dashboard/api.go:67` |
| `GET` | `/api/kick/{agent}/status` | Dashboard auth/session | Outcome of the most recent kick | `pkg/dashboard/api.go:71` |
| `POST` | `/api/switch/{agent}/{backend}` | Dashboard auth/session | Switch | `pkg/dashboard/api.go:68` |
| `POST` | `/api/model/{agent}/{model}` | Dashboard auth/session | Model Set | `pkg/dashboard/api.go:69` |
| `POST` | `/api/pause/{agent}` | Dashboard auth/session | Pause | `pkg/dashboard/api.go:70` |
| `POST` | `/api/resume/{agent}` | Dashboard auth/session | Resume | `pkg/dashboard/api.go:71` |
| `GET` | `/api/agent-state/{agent}` | Dashboard auth/session | Agent State | `pkg/dashboard/api.go:72` |
| `GET` | `/api/breaker` | Dashboard auth/session | Breaker State | `pkg/dashboard/api.go:73` |
| `POST` | `/api/breaker/engage` | Dashboard auth/session | Breaker Engage | `pkg/dashboard/api.go:74` |
| `POST` | `/api/breaker/release` | Dashboard auth/session | Breaker Release | `pkg/dashboard/api.go:75` |
| `POST` | `/api/pin/{agent}/{dimension}` | Dashboard auth/session | Pin | `pkg/dashboard/api.go:76` |
| `POST` | `/api/unpin/{agent}/{dimension}` | Dashboard auth/session | Unpin | `pkg/dashboard/api.go:77` |
| `POST` | `/api/restart/{agent}` | Dashboard auth/session | Restart | `pkg/dashboard/api.go:78` |
| `GET` | `/api/model-advisor` | Dashboard auth/session | Model Advisor | `pkg/dashboard/api.go:88` |
| `GET` | `/api/agents` | Dashboard auth/session | Agents List | `pkg/dashboard/api.go:160` |
| `POST` | `/api/agents` | Dashboard auth/session | Agent Create | `pkg/dashboard/api.go:161` |
| `POST` | `/api/agents/import` | Dashboard auth/session | Agent Import | `pkg/dashboard/api.go:162` |
| `DELETE` | `/api/agents/{name}` | Dashboard auth/session | Agent Delete | `pkg/dashboard/api.go:163` |

### Kick is asynchronous

`POST /api/kick/{agent}` queues the message and returns immediately with `202`; it is not a delivery confirmation.

Delivery has to wait for the agent's CLI to present its input prompt, which is bounded by `inputPromptTimeout` (120s). Doing that wait on the request path made the handler outlive a typical 60s ingress idle timeout, so a proxy answered `504` for kicks that had in fact succeeded — the prompt was typed, the agent ran the session, and the operator was told it failed (kubestellar/hive#5325). Retrying a false failure delivered the prompt twice.

The contract is now:

- **`400`** — a genuine, deterministic precondition failure evaluated inline: unknown agent, paused, stopped, no tmux session, sandbox kick rejected, prompt over 10000 chars.
- **`202` with `status: "queued"`** — accepted; a background delivery started.
- **`202` with `status: "in-flight"`** — a delivery for this agent was already running, so this call was deduplicated. Delivery is exactly-once per agent, which is what makes an operator's retry harmless.

Read the result from `GET /api/kick/{agent}/status`, which returns `status` of `unknown`, `in-flight`, `delivered`, or `failed`, plus a `pending` boolean. While `pending` is true the outcome is **indeterminate** — the prompt may still be delivered — and clients must not render it as a failure. A CLI that never reaches its input prompt within `inputPromptTimeout` settles as `failed` with a reason.

## Packs and ACMM

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/packs` | Dashboard auth/session | Packs List | `pkg/dashboard/api.go:165` |
| `POST` | `/api/packs/{level}/apply` | Dashboard auth/session | Pack Apply | `pkg/dashboard/api.go:166` |
| `PUT` | `/api/packs/level` | Dashboard auth/session | Pack Set Level | `pkg/dashboard/api.go:167` |
| `GET` | `/api/acmm/evaluation` | Dashboard auth/session | ACMMEvaluation | `pkg/dashboard/api.go:169` |
| `POST` | `/api/acmm/issue` | Dashboard auth/session | ACMMCreate Issue — files on GitHub or, with `governor.acmm.issue_tracker: work_source` / body `tracker: "work_source"` on a Linear-sourced hive, on Linear; response `tracker` says which. See [ACMM policy matrix](acmm-policy-matrix.md#where-acmm-gap-issues-are-filed) | `pkg/dashboard/api.go:170` |
| `GET` | `/api/acmm-recommendation` | Dashboard auth/session | Advisory level-up recommendation (`acmmadvisor.Recommendation`, JSON): never changes the applied level — see [ACMM advisor](acmm-advisor.md) | `pkg/dashboard/api.go:230` |

## Cost, tokens, telemetry

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/trends` | Dashboard auth/session | Trends | `pkg/dashboard/api.go:60` |
| `GET` | `/api/token-access` | Dashboard auth/session | Token Access | `pkg/dashboard/api.go:81` |
| `GET` | `/api/tokens` | Dashboard auth/session | Tokens | `pkg/dashboard/api.go:82` |
| `GET` | `/api/cost` | Dashboard auth/session | Cost | `pkg/dashboard/api.go:83` |
| `GET` | `/api/repo-activity` | Dashboard auth/session | Per-repo audited activity | `pkg/dashboard/api.go` |
| `GET` | `/api/repo-cost` | Dashboard auth/session | Per-repo estimated token cost (interval join, cached) | `pkg/dashboard/api.go` |
| `GET` | `/api/cost/history` | Dashboard auth/session | Cost History | `pkg/dashboard/api.go:84` |
| `GET` | `/api/trend/history` | Dashboard auth/session | Trend History | `pkg/dashboard/api.go:85` |
| `GET` | `/api/timeseries` | Dashboard auth/session | Time Series | `pkg/dashboard/api.go:86` |

## Knowledge

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/knowledge` | Dashboard auth/session | Knowledge List | `pkg/dashboard/api.go:177` |
| `GET` | `/api/knowledge/export` | Dashboard auth/session | Knowledge Export | `pkg/dashboard/api.go:178` |
| `GET` | `/api/knowledge/search` | Dashboard auth/session | Knowledge Search | `pkg/dashboard/api.go:179` |
| `GET` | `/api/knowledge/health` | Dashboard auth/session | Knowledge Health | `pkg/dashboard/api.go:180` |
| `GET` | `/api/knowledge/stats` | Dashboard auth/session | Knowledge Stats | `pkg/dashboard/api.go:181` |
| `GET` | `/api/knowledge/graph` | Dashboard auth/session | Knowledge Graph | `pkg/dashboard/api.go:182` |
| `GET` | `/api/knowledge/fact-history` | Dashboard auth/session | Fact History | `pkg/dashboard/api.go:183` |
| `POST` | `/api/knowledge/create` | Dashboard auth/session | Knowledge Create | `pkg/dashboard/api.go:184` |
| `POST` | `/api/knowledge/import` | Dashboard auth/session | Knowledge Import | `pkg/dashboard/api.go:185` |
| `POST` | `/api/knowledge/promote` | Dashboard auth/session | Knowledge Promote | `pkg/dashboard/api.go:186` |
| `GET` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs List | `pkg/dashboard/api.go:187` |
| `POST` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs Add | `pkg/dashboard/api.go:188` |
| `DELETE` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs Remove | `pkg/dashboard/api.go:189` |
| `PUT` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Update | `pkg/dashboard/api.go:190` |
| `DELETE` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Delete | `pkg/dashboard/api.go:191` |
| `GET` | `/api/knowledge/{layer}` | Dashboard auth/session | Knowledge Layer | `pkg/dashboard/api.go:192` |
| `GET` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Fact | `pkg/dashboard/api.go:193` |
| `PUT` | `/api/knowledge/enabled` | Dashboard auth/session | Knowledge Toggle | `pkg/dashboard/api.go:194` |
| `GET` | `/api/knowledge/bead-synthesizer` | Dashboard auth/session | Bead Synth Status | `pkg/dashboard/api.go:195` |
| `PUT` | `/api/knowledge/bead-synthesizer/enabled` | Dashboard auth/session | Bead Synth Toggle | `pkg/dashboard/api.go:196` |
| `GET` | `/api/knowledge/vaults` | Dashboard auth/session | Vaults List | `pkg/dashboard/api.go:197` |
| `POST` | `/api/knowledge/vaults` | Dashboard auth/session | Vaults Connect | `pkg/dashboard/api.go:198` |
| `DELETE` | `/api/knowledge/vaults` | Dashboard auth/session | Vaults Disconnect | `pkg/dashboard/api.go:199` |
| `POST` | `/api/knowledge/vaults/reindex` | Dashboard auth/session | Vaults Reindex | `pkg/dashboard/api.go:200` |
| `GET` | `/api/knowledge/vaults/{name}/facts` | Dashboard auth/session | Vault Facts | `pkg/dashboard/api.go:201` |
| `GET` | `/api/knowledge/git-sources` | Dashboard auth/session | Git Sources List | `pkg/dashboard/api.go:202` |
| `POST` | `/api/knowledge/git-sources` | Dashboard auth/session | Git Sources Connect | `pkg/dashboard/api.go:203` |
| `DELETE` | `/api/knowledge/git-sources` | Dashboard auth/session | Git Sources Disconnect | `pkg/dashboard/api.go:204` |
| `POST` | `/api/knowledge/obsidian/sync` | Dashboard auth/session | Obsidian Sync | `pkg/dashboard/api.go:205` |
| `GET` | `/api/knowledge/documents` | Dashboard auth/session | Documents List | `pkg/dashboard/api.go:206` |
| `POST` | `/api/knowledge/documents` | Dashboard auth/session | Documents Import | `pkg/dashboard/api.go:207` |
| `GET` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Get | `pkg/dashboard/api.go:208` |
| `DELETE` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Delete | `pkg/dashboard/api.go:209` |
| `POST` | `/api/knowledge/documents/{slug}/reimport` | Dashboard auth/session | Document Reimport | `pkg/dashboard/api.go:210` |
| `GET` | `/api/knowledge/context7/search` | Dashboard auth/session | Context7 Search | `pkg/dashboard/api.go:211` |
| `POST` | `/api/knowledge/cleanup-orphans` | Dashboard auth/session | Cleanup Orphans | `pkg/dashboard/api.go:212` |

## Contribute

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/contribute` | Public | Contribute Landing | `pkg/dashboard/api_contribute.go:226` |
| `GET` | `/contribute/{tab}` | Public | Contribute Landing | `pkg/dashboard/api_contribute.go:232` |
| `GET` | `/api/contribute/ws` | Public | s.contribute Hub.Handle WS | `pkg/dashboard/api_contribute.go:233` |
| `POST` | `/api/contribute/register` | Public | Contribute Register | `pkg/dashboard/api_contribute.go:234` |
| `POST` | `/api/contribute/invite` | Public | Contribute Invite | `pkg/dashboard/api_contribute.go:240` |
| `POST` | `/api/contribute/reissue-token` | Public | Contribute Reissue Token | `pkg/dashboard/api_contribute.go:241` |
| `GET` | `/api/contribute/status` | Public | Contribute Status | `pkg/dashboard/api_contribute.go:242` |
| `GET` | `/api/contribute/activity` | Public | Contribute Activity | `pkg/dashboard/api_contribute.go:243` |
| `GET` | `/api/contribute/fleet` | Public | Contribute Fleet | `pkg/dashboard/api_contribute.go:244` |
| `GET` | `/api/contribute/events` | Public | Contribute Events | `pkg/dashboard/api_contribute.go:248` |
| `GET` | `/api/contribute/queue` | Public | Contribute Queue | `pkg/dashboard/api_contribute.go:251` |
| `GET` | `/api/contribute/opportunistic` | Public | Contribute Opportunistic | `pkg/dashboard/api_contribute.go:255` |
| `GET` | `/api/contribute/limits` | Public | Contribute Limits | `pkg/dashboard/api_contribute.go:260` |
| `GET` | `/api/contribute/metrics` | Public | Contribute Metrics | `pkg/dashboard/api_contribute.go:266` |
| `GET` | `/api/contribute/triage` | Public | Contribute Triage | `pkg/dashboard/api_contribute.go:273` |
| `PUT` | `/api/contribute/queue/order` | Public | Contribute Queue Order | `pkg/dashboard/api_contribute.go:277` |
| `POST` | `/api/contribute/queue/hold` | Public | Contribute Queue Hold | `pkg/dashboard/api_contribute.go:282` |
| `POST` | `/api/contribute/queue/hold/clear` | Public | Contribute Queue Hold Clear | `pkg/dashboard/api_contribute.go:286` |
| `GET` | `/api/contribute/interests` | Public | Contribute Interests | `pkg/dashboard/api_contribute.go:292` |
| `PUT` | `/api/contribute/interests` | Public | Contribute Interests | `pkg/dashboard/api_contribute.go:293` |
| `GET` | `/api/contributors` | Dashboard auth/session | Contributors List | `pkg/dashboard/api_contribute.go:294` |
| `GET` | `/api/contributors/{id}` | Dashboard auth/session | Contributor Get | `pkg/dashboard/api_contribute.go:295` |
| `PUT` | `/api/contributors/{id}/trust` | Dashboard auth/session | Contributor Trust | `pkg/dashboard/api_contribute.go:296` |
| `PUT` | `/api/contributors/{id}/agent-role` | Dashboard auth/session | Contributor Agent Role | `pkg/dashboard/api_contribute.go:297` |
| `PUT` | `/api/contributors/{id}/agent-role-grants` | Dashboard auth/session | Contributor Agent Role Grants | `pkg/dashboard/api_contribute.go:298` |
| `POST` | `/api/contributors/{id}/revoke` | Dashboard auth/session | Contributor Revoke | `pkg/dashboard/api_contribute.go:299` |
| `POST` | `/api/contributors/{id}/requeue` | Dashboard auth/session | Contributor Requeue | `pkg/dashboard/api_contribute.go:300` |
| `DELETE` | `/api/contributors/{id}` | Dashboard auth/session | Contributor Delete | `pkg/dashboard/api_contribute.go:301` |

### `/api/v1` contributor subpaths

`handleAPIv1` dispatches authenticated contributor API calls below `/api/v1`.
Every request must carry a GitHub personal access token in the `Authorization`
header, using either the `Bearer <token>` scheme (hosted clients) or the
legacy `token <token>` scheme (what `gh auth token` and older hive CLIs
send). Both are accepted; the legacy scheme is retained for backward
compatibility. Credentials in the query string (`?token=`) are NOT supported
and are rejected, because query strings land in ingress and access logs.

Authorization: every `/api/v1` path except `/api/v1/me` additionally requires
the caller to be in the hive's authorized-users allowlist (any role), and
returns `403` otherwise — contributor, activity and knowledge data are
hive-private, not world-readable. `/api/v1/me` is exempt because it only
returns the caller's own profile. Client-supplied `X-Hive-User`, `X-Hive-Role`
and owner-verified headers are stripped at the top of the handler; identity is
always resolved server-side from the validated token.

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET`/`POST` | `/api/v1/status` | GitHub token + allowlist | Contributor status summary | `pkg/dashboard/api_contribute.go:6777` |
| `GET`/`POST` | `/api/v1/activity` | GitHub token + allowlist | Contributor activity feed | `pkg/dashboard/api_contribute.go:6779` |
| `GET`/`POST` | `/api/v1/contributors` | GitHub token + allowlist | Contributor list | `pkg/dashboard/api_contribute.go:6781` |
| `GET`/`POST` | `/api/v1/knowledge` | GitHub token + allowlist | Knowledge export | `pkg/dashboard/api_contribute.go:6783` |
| `GET`/`POST` | `/api/v1/me` | GitHub token (self-scoped, no allowlist) | Current contributor profile | `pkg/dashboard/api_contribute.go:6785` |
| `POST` | `/api/v1/prs/{owner}/{repo}/{number}/queue-automerge` | GitHub token + merger/owner role (POST only; GET returns 405) | Queue PR auto-merge using the validated actor and current PR head | `pkg/dashboard/api_contribute.go` |

## Nous / strategy lab

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/nous/status` | Dashboard auth/session | Nous Status | `pkg/dashboard/api.go:234` |
| `GET` | `/api/nous/ledger` | Dashboard auth/session | Nous Ledger | `pkg/dashboard/api.go:235` |
| `GET` | `/api/nous/principles` | Dashboard auth/session | Nous Principles | `pkg/dashboard/api.go:236` |
| `POST` | `/api/nous/approve` | Dashboard auth/session | Nous Approve | `pkg/dashboard/api.go:237` |
| `POST` | `/api/nous/abort` | Dashboard auth/session | Nous Abort | `pkg/dashboard/api.go:238` |
| `PUT` | `/api/nous/mode` | Dashboard auth/session | Nous Mode | `pkg/dashboard/api.go:239` |
| `PUT` | `/api/nous/scope` | Dashboard auth/session | Nous Scope | `pkg/dashboard/api.go:240` |
| `GET` | `/api/nous/phase` | Dashboard auth/session | Nous Phase | `pkg/dashboard/api.go:241` |
| `PUT` | `/api/nous/gate-decision` | Dashboard auth/session | Nous Gate Decision | `pkg/dashboard/api.go:242` |
| `GET` | `/api/nous/gate-pending` | Dashboard auth/session | Nous Gate Pending | `pkg/dashboard/api.go:243` |
| `POST` | `/api/nous/gate-respond` | Dashboard auth/session | Nous Gate Respond | `pkg/dashboard/api.go:244` |
| `GET` | `/api/nous/gate-response` | Dashboard auth/session | Nous Gate Response | `pkg/dashboard/api.go:245` |
| `GET` | `/api/nous/config` | Dashboard auth/session | Nous Config Get | `pkg/dashboard/api.go:246` |
| `PUT` | `/api/nous/config/goals` | Dashboard auth/session | Nous Config Goals | `pkg/dashboard/api.go:247` |
| `PUT` | `/api/nous/config/repos` | Dashboard auth/session | Nous Config Repos | `pkg/dashboard/api.go:248` |
| `PUT` | `/api/nous/config/output` | Dashboard auth/session | Nous Config Output | `pkg/dashboard/api.go:249` |
| `PUT` | `/api/nous/config/fast-fail` | Dashboard auth/session | Nous Config Fast Fail | `pkg/dashboard/api.go:250` |
| `PUT` | `/api/nous/config/schedule` | Dashboard auth/session | Nous Config Schedule | `pkg/dashboard/api.go:251` |
| `PUT` | `/api/nous/config/controllables` | Dashboard auth/session | Nous Config Controllables | `pkg/dashboard/api.go:252` |
| `PUT` | `/api/nous/config/principles` | Dashboard auth/session | Nous Config Principles | `pkg/dashboard/api.go:253` |
| `DELETE` | `/api/nous/principles/{id}` | Dashboard auth/session | Nous Delete Principle | `pkg/dashboard/api.go:254` |

## Inception

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `POST` | `/api/inception/start` | Dashboard auth/session | Inception Start | `pkg/dashboard/api.go:217` |
| `POST` | `/api/inception/scan` | Dashboard auth/session | Inception Scan | `pkg/dashboard/api.go:218` |
| `GET` | `/api/inception/state` | Dashboard auth/session | Inception State | `pkg/dashboard/api.go:219` |
| `POST` | `/api/inception/questions` | Dashboard auth/session | Inception Set Questions | `pkg/dashboard/api.go:220` |
| `POST` | `/api/inception/answer` | Dashboard auth/session | Inception Answer | `pkg/dashboard/api.go:221` |
| `POST` | `/api/inception/facts` | Dashboard auth/session | Inception Record Facts | `pkg/dashboard/api.go:222` |
| `GET` | `/api/inception/scaffold` | Dashboard auth/session | Inception Scaffold | `pkg/dashboard/api.go:223` |
| `POST` | `/api/inception/approve` | Dashboard auth/session | Inception Approve | `pkg/dashboard/api.go:224` |
| `POST` | `/api/inception/reset` | Dashboard auth/session | Inception Reset | `pkg/dashboard/api.go:225` |
| `GET` | `/api/inception/ideation-facts` | Dashboard auth/session | Inception Ideation Facts | `pkg/dashboard/api.go:226` |
| `GET` | `/api/inception/download` | Dashboard auth/session | Inception Download | `pkg/dashboard/api.go:227` |
| `GET` | `/api/inception/has-files` | Dashboard auth/session | Inception Has Files | `pkg/dashboard/api.go:228` |
| `PUT` | `/api/inception/wiki-name` | Dashboard auth/session | Inception Rename Wiki | `pkg/dashboard/api.go:229` |
| `POST` | `/api/inception/import` | Dashboard auth/session | Inception Import | `pkg/dashboard/api.go:230` |

## Beads

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/beads` | Dashboard auth/session | Beads List | `pkg/dashboard/api.go:256` |
| `GET` | `/api/beads/{agent}` | Dashboard auth/session | Beads List | `pkg/dashboard/api.go:257` |
| `POST` | `/api/beads/{agent}` | Dashboard auth/session | Beads Create | `pkg/dashboard/api.go:258` |
| `POST` | `/api/beads/reset` | Dashboard auth/session | Beads Reset | `pkg/dashboard/api.go:259` |
| `POST` | `/api/beads/reset/{agent}` | Dashboard auth/session | Beads Reset Agent | `pkg/dashboard/api.go:260` |

## Dashboard miscellaneous

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/audit` | Dashboard auth/session | Audit Log | `pkg/dashboard/api.go:47` |
| `POST` | `/api/presence` | Dashboard auth/session | Presence | `pkg/dashboard/api.go:48` |
| `GET` | `/api/prompt-history` | Dashboard auth/session | Prompt History | `pkg/dashboard/api.go:49` |
| `POST` | `/api/self-upgrade` | Dashboard auth/session | Self Upgrade | `pkg/dashboard/api.go:50` |
| `GET` | `/api/backup/status` | Owner only | Backup Status — `available:false` with a reason when no encryption key is configured | `pkg/dashboard/api.go:53` |
| `POST` | `/api/backup` | Owner only | Backup Download — `412` when no encryption key is configured (never an unencrypted archive) | `pkg/dashboard/api.go:54` |
| `POST` | `/api/banner-dismissed` | Dashboard auth/session | Banner Dismissed | `pkg/dashboard/api.go:55` |
| `GET` | `/api/history` | Dashboard auth/session | History | `pkg/dashboard/api.go:59` |
| `GET` | `/api/timeline` | Dashboard auth/session | Timeline | `pkg/dashboard/api.go:61` |
| `GET` | `/api/widget` | Dashboard auth/session | Widget | `pkg/dashboard/api.go:62` |
| `GET` | `/api/pane/{agent}` | Dashboard auth/session | Pane | `pkg/dashboard/api.go:63` |
| `GET` | `/api/role` | Dashboard auth/session | Role | `pkg/dashboard/api.go:65` |
| `POST` | `/api/reset-restarts/{agent}` | Dashboard auth/session | Reset Restarts | `pkg/dashboard/api.go:79` |
| `GET` | `/api/budget-ignore` | Dashboard auth/session | Budget Ignore Get | `pkg/dashboard/api.go:89` |
| `POST` | `/api/budget-ignore` | Dashboard auth/session | Budget Ignore Set | `pkg/dashboard/api.go:90` |
| `GET` | `/api/summaries` | Dashboard auth/session | Summaries | `pkg/dashboard/api.go:102` |
| `POST` | `/api/prs/{owner}/{repo}/{number}/queue-automerge` | Dashboard auth/session | Queue PRAuto Merge | `pkg/dashboard/api.go:103` |
| `GET` | `/api/inference/models/{backend}` | Dashboard auth/session | Inference Models | `pkg/dashboard/api.go:175` |
| `GET` | `/api/hive-id` | Dashboard auth/session | Hive IDGet | `pkg/dashboard/api.go:214` |
| `PUT` | `/api/hive-id` | Dashboard auth/session | Hive IDSet | `pkg/dashboard/api.go:215` |
| `POST` | `/api/chat` | Dashboard auth/session | Chat | `pkg/dashboard/api.go:232` |
| `GET` | `/api/auth/token` | Public | Auth Token | `pkg/dashboard/api.go:262` |
| `GET` | `/api/v1/` | GitHub token | Contributor v1 API dispatcher | `pkg/dashboard/api_contribute.go:303` |
| `POST` | `/api/v1/` | GitHub token | Contributor v1 API dispatcher | `pkg/dashboard/api_contribute.go:304` |
| `GET` | `/api/docs` | Dashboard auth/session | APIDocs | `pkg/dashboard/api_contribute.go:305` |
| `GET` | `/leaderboard` | Public | Leaderboard Page | `pkg/dashboard/api_contribute.go:307` |
| `GET` | `/api/leaderboard` | Public | Leaderboard API | `pkg/dashboard/api_contribute.go:308` |
| `GET` | `/api/leaderboard/style` | Public | Leaderboard Style | `pkg/dashboard/api_contribute.go:309` |
| `GET` | `/api/leaderboard/contributor/{username}` | Public | Contributor Profile | `pkg/dashboard/api_contribute.go:315` |
| `GET` | `/api/hives` | Dashboard auth/session | Hives List | `pkg/dashboard/api_contribute.go:317` |
| `POST` | `/api/hives/register` | Dashboard auth/session | Hives Register | `pkg/dashboard/api_contribute.go:318` |
| `POST` | `/api/hives/{id}/heartbeat` | Dashboard auth/session | Hives Heartbeat | `pkg/dashboard/api_contribute.go:319` |
| `DELETE` | `/api/hives/{id}` | Dashboard auth/session | Hives Delete | `pkg/dashboard/api_contribute.go:320` |
| `POST` | `/api/hives/onboard` | Dashboard auth/session | Hives Onboard | `pkg/dashboard/api_contribute.go:321` |
| `GET` | `/sso` | Public | SSO | `pkg/dashboard/server.go:685` |

## Hub SaaS

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/saas/my-hives` | Hub auth | My Hives | `pkg/hub/saas.go:306` |
| `GET` | `/api/saas/usage` | Hub auth | Usage | `pkg/hub/saas.go:310` |
| `POST` | `/api/saas/hives` | Hub auth | Create Hive | `pkg/hub/saas.go:311` |
| `GET` | `/api/saas/hives/{id}/status` | Hub auth | Hive Status | `pkg/hub/saas.go:312` |
| `GET` | `/api/saas/hives/{id}/open` | Hub handler-specific | Open Hive | `pkg/hub/saas.go:317` |
| `DELETE` | `/api/saas/hives/{id}` | Hub auth | Delete Hive | `pkg/hub/saas.go:318` |
| `POST` | `/api/saas/hives/{id}/upgrade` | Hub auth | Upgrade Hive | `pkg/hub/saas.go:319` |
| `POST` | `/api/saas/hives/{id}/switch-branch` | Hub auth | Switch Branch | `pkg/hub/saas.go:320` |
| `PUT` | `/api/saas/hives/{id}/visibility` | Hub auth | Toggle Visibility | `pkg/hub/saas.go:321` |
| `PUT` | `/api/saas/hives/{id}/auto-upgrade` | Hub auth | Toggle Auto Upgrade | `pkg/hub/saas.go:322` |
| `PUT` | `/api/saas/hives/{id}/name` | Hub auth | Rename Hive | `pkg/hub/saas.go:326` |
| `POST` | `/api/saas/hives/{id}/forge` | Hub auth | Switch Forge | `pkg/hub/saas.go:330` |
| `POST` | `/api/saas/hives/{id}/reset-app` | Hub auth | Reset App | `pkg/hub/saas.go:331` |
| `POST` | `/api/saas/hives/{id}/restart-spoke` | Hub auth | Restart Spoke | `pkg/hub/saas.go:332` |
| `GET` | `/api/saas/hive-config/{hiveID}` | Hub auth | Proxy Hive Config | `pkg/hub/saas.go:333` |
| `GET` | `/api/saas/latest-sha` | Hub handler-specific | Latest SHA | `pkg/hub/saas.go:334` |
| `POST` | `/api/saas/hub/upgrade` | Hub handler-specific | Hub Self Upgrade | `pkg/hub/saas.go:335` |
| `PUT` | `/api/saas/hub/auto-upgrade` | Hub handler-specific | Hub Auto Upgrade | `pkg/hub/saas.go:336` |
| `GET` | `/api/saas/auth-check` | Hub handler-specific | Saa SAuth Check | `pkg/hub/saas.go:337` |
| `POST` | `/api/saas/user-token` | Hub auth | User Token | `pkg/hub/saas.go:338` |
| `GET` | `/api/saas/hives/{id}/access` | Hub auth | Access List | `pkg/hub/saas.go:339` |
| `GET` | `/api/saas/grantable-users` | Hub auth | Grantable Users | `pkg/hub/saas.go:340` |
| `POST` | `/api/saas/hives/{id}/access` | Hub auth | Access Add | `pkg/hub/saas.go:341` |
| `DELETE` | `/api/saas/hives/{id}/access/{username}` | Hub auth | Access Remove | `pkg/hub/saas.go:342` |
| `POST` | `/api/saas/hives/{id}/request-access` | Hub auth | Request Access | `pkg/hub/saas.go:343` |
| `GET` | `/api/saas/hives/{id}/requests` | Hub auth | Get Requests | `pkg/hub/saas.go:344` |
| `GET` | `/api/saas/hives/{id}/timeline` | Hub auth | Hive Timeline | `pkg/hub/saas.go:345` |
| `POST` | `/api/saas/hives/{id}/requests/{username}/approve` | Hub auth | Approve Request | `pkg/hub/saas.go:346` |
| `POST` | `/api/saas/hives/{id}/requests/{username}/deny` | Hub auth | Deny Request | `pkg/hub/saas.go:347` |
| `PUT` | `/api/saas/hives/{id}/approve-access/{username}` | Hub auth | Approve Access | `pkg/hub/saas.go:348` |
| `DELETE` | `/api/saas/hives/{id}/deny-access/{username}` | Hub auth | Deny Access | `pkg/hub/saas.go:349` |
| `GET` | `/api/saas/access-status` | Hub handler-specific | Access Status | `pkg/hub/saas.go:350` |
| `POST` | `/api/saas/request-provision` | Hub auth | Request Provision | `pkg/hub/saas.go:358` |
| `PUT` | `/api/saas/approve-provision/{username}` | Hub handler-specific | Approve Provision | `pkg/hub/saas.go:359` |
| `DELETE` | `/api/saas/deny-provision/{username}` | Hub handler-specific | Deny Provision | `pkg/hub/saas.go:360` |
| `GET` | `/api/saas/admin/available-placeholders` | Hub handler-specific | Available Placeholders | `pkg/hub/saas.go:361` |
| `GET` | `/api/saas/admin/users` | Hub handler-specific | Admin Users | `pkg/hub/saas.go:362` |
| `PUT` | `/api/saas/admin/users/{username}` | Hub handler-specific | Admin Update User | `pkg/hub/saas.go:363` |
| `DELETE` | `/api/saas/admin/users/{username}` | Hub handler-specific | Admin Delete User | `pkg/hub/saas.go:364` |
| `POST` | `/api/saas/admin/impersonate/exit` | Hub handler-specific | Impersonate Exit | `pkg/hub/saas.go:370` |
| `POST` | `/api/saas/admin/impersonate/{username}` | Hub handler-specific | Impersonate Start | `pkg/hub/saas.go:371` |
| `GET` | `/api/saas/impersonation-status` | Hub auth | Impersonation Status | `pkg/hub/saas.go:372` |
| `POST` | `/api/saas/hives/{id}/assign` | Hub auth | Assign Hive | `pkg/hub/saas.go:373` |
| `POST` | `/api/saas/hives/{id}/reset-assignment` | Hub handler-specific | Reset Assignment | `pkg/hub/saas.go:377` |
| `GET` | `/api/saas/cluster-health` | Hub handler-specific | Cluster Health | `pkg/hub/saas.go:379` |
| `POST` | `/api/saas/admin/alert-ack` | Hub handler-specific | Alert Ack | `pkg/hub/saas.go:382` |
| `GET` | `/api/saas/admin/cluster-app-keys` | Hub handler-specific | Get Cluster App Keys | `pkg/hub/saas.go:386` |
| `PUT` | `/api/saas/admin/cluster-app-keys/{clusterID}` | Hub handler-specific | Put Cluster App Key | `pkg/hub/saas.go:387` |
| `POST` | `/api/saas/admin/hub-banner` | Hub handler-specific | Send Hub Banner | `pkg/hub/saas.go:388` |
| `DELETE` | `/api/saas/admin/hub-banner` | Hub handler-specific | Clear Hub Banner | `pkg/hub/saas.go:389` |
| `GET` | `/api/saas/admin/hub-banner` | Hub handler-specific | Get Hub Banner | `pkg/hub/saas.go:390` |
| `POST` | `/api/saas/slack/user/{username}` | Hub auth | Slack Message User | `pkg/hub/saas.go:397` |
| `POST` | `/api/saas/hives/{id}/slack` | Hub auth | Slack Message Hive Owner | `pkg/hub/saas.go:398` |
| `POST` | `/api/saas/admin/slack/broadcast` | Hub handler-specific | Slack Broadcast | `pkg/hub/saas.go:399` |
| `POST` | `/api/saas/admin/journey-snooze` | Hub handler-specific | Journey Snooze | `pkg/hub/saas.go:400` |
| `GET` | `/api/saas/admin/journey-status` | Hub handler-specific | Journey Status | `pkg/hub/saas.go:401` |
| `POST` | `/api/saas/hives/bulk` | Hub auth | Bulk Hive Action | `pkg/hub/saas_bulk.go:105` |

## Hub server

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/login` | Hub handler-specific | Login | `pkg/hub/oauth.go:37` |
| `GET` | `/api/auth/callback` | Hub handler-specific | OAuth Callback | `pkg/hub/oauth.go:38` |
| `GET` | `/api/auth/user` | Hub handler-specific | Auth User | `pkg/hub/oauth.go:39` |
| `POST` | `/api/auth/logout` | Hub handler-specific | Logout | `pkg/hub/oauth.go:40` |
| `GET` | `/api/openrouter/connect/start` | Hub auth | Hub Open Router Start | `pkg/hub/openrouter.go:49` |
| `GET` | `/api/openrouter/qr` | Hub auth | Hub Open Router QR | `pkg/hub/openrouter.go:50` |
| `GET` | `/api/openrouter/models` | Hub auth | Hub Open Router Models | `pkg/hub/openrouter.go:51` |
| `GET` | `/api/openrouter/credit` | Hub auth | Hub Open Router Credit | `pkg/hub/openrouter.go:52` |
| `GET` | `/openrouter/callback` | Hub handler-specific | Hub Open Router Callback | `pkg/hub/openrouter.go:53` |
| `GET` | `/dashboard` | Hub handler-specific | Dashboard | `pkg/hub/saas.go:304` |
| `GET` | `/access-denied` | Hub handler-specific | Access Denied | `pkg/hub/saas.go:305` |
| `GET` | `/api/hub/clusters` | Hub auth | List Clusters | `pkg/hub/saas.go:383` |
| `POST` | `/api/heartbeat` | Hub handler-specific | Heartbeat | `pkg/hub/server.go:1100` |
| `POST` | `/api/task-status` | Hub handler-specific | Task Status | `pkg/hub/server.go:1101` |
| `GET` | `/api/registry` | Hub handler-specific | Registry | `pkg/hub/server.go:1102` |
| `GET` | `/api/hub/leaderboard` | Hub handler-specific | Leaderboard | `pkg/hub/server.go:1103` |
| `GET` | `/api/hub/stats` | Hub handler-specific | Stats | `pkg/hub/server.go:1104` |
| `GET` | `/api/fleet-stats` | Hub handler-specific | Fleet Stats | `pkg/hub/server.go:1105` |
| `GET` | `/api/hub/version` | Hub handler-specific | Hub Version | `pkg/hub/server.go:1106` |
| `DELETE` | `/api/hub/registry/{id}` | Hub handler-specific | Registry Delete | `pkg/hub/server.go:1107` |
| `POST` | `/api/contribute/register` | Hub handler-specific | Contribute Proxy | `pkg/hub/server.go:1108` |
| `GET` | `/api/contribute/status` | Hub handler-specific | Contribute Status | `pkg/hub/server.go:1109` |
| `GET` | `/api/contribute/ws` | Hub handler-specific | Contribute WSProxy | `pkg/hub/server.go:1110` |
| `POST` | `/api/github/webhook` | Hub handler-specific | GitHub Webhook | `pkg/hub/server.go:1111` |
| `GET` | `/gh-setup` | Hub handler-specific | GitHub App Setup Router | `pkg/hub/server.go:1112` |
| `GET` | `/learn` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1113` |
| `GET` | `/get-started` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1114` |
| `GET` | `/api/docs` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1115` |
| `GET` | `/api/reading-list` | Hub handler-specific | Reading List | `pkg/hub/server.go:1116` |
| `GET` | `/reading` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1117` |
| `GET` | `/cncf-reference-architecture` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1120` |
| `GET` | `/{$}` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1121` |
| `GET` | `/og-card.png` | Hub handler-specific | OGCard | `pkg/hub/server.go:1126` |
