## Credentials and hive-provided capabilities

Status: **Stage 2 of the #14138 sidecar plan (decision A). Design
recorded here first; the callback contract (`crux/api/callback`), the
runtime-side adapters (`crux/hivecap`) and the hive-side mount
(`pkg/cruxhost`) follow in the next PR. Nothing is wired into `cmd/hive`
or `crux serve` yet; that is stage 3.**

The decision this section records: **the runtime never holds the GitHub
App private key, or any other hive-owned credential.** When hive runs the
runtime as a sidecar (`crux serve`), the runtime obtains short-lived scoped
tokens and every other hive-owned capability by calling back into hive over
a hive-side endpoint. In-process, the same capabilities are the direct
function calls `cmd/hive` already installs today. The seam interfaces the
Stage 2 inversions left behind are the contract in both modes; the sidecar
adds one implementation of each, not a second contract.

### Inventory

Every interface the ring defines for something the host supplies, measured
on v5 at `8bce0b0` (Oct 25, 2026) with
`grep -rn 'interface {' src/crux --include='*.go' | grep -v _test`. The
last column is the classification this stage settles: a *capability* is
something hive owns (a credential, a registry, an accounting ledger, a
forge client) that the runtime asks for; a *runtime feature* is behaviour
the runtime owns and only lets the host observe or tune.

| Seam | Defined in | Hive implementation today | What crosses the boundary | Sensitivity | Class |
|---|---|---|---|---|---|
| `pushbroker.TokenScoper` (`ScopedTokenForRepos(ctx, tier, repos)`) | `crux/pushbroker` | `*github.AppAuth` via `cmd/hive`'s `appMinter` | tier + repo names in; an installation token out | **credential**: the token is a live GitHub write credential | capability |
| `pushbroker.TokenMinter` (`MintPushToken(ctx, repo)`) | `crux/pushbroker` | `pushbroker.GitHubAppMinter{Auth: TokenScoper}` (ring code over the seam above) | same | same | capability (derived from `TokenScoper`) |
| `agent.AppTokenMinter` (`WriteAgentToken(ctx, agent, tier, uid)`) | `crux/agent` | `*github.AppAuth` (`Manager.SetAppAuth`) | agent, tier, uid in; hive **writes the agent's token cache file** as a side effect | **credential**, plus a filesystem write into the agents' tree | capability, but mis-shaped for a process boundary (see below) |
| `agent.AgentMintIssuer` (`Enabled`, `MintAgentToken(agent, tier)`) | `crux/agent` | `mint.AgentMinter` (`cmd/hive` `buildMinter`) | agent + tier in; an OIDC/JWT out | **credential** (opt-in, additional) | capability |
| `agent.LinearCredential` + `SetLinearCredentialResolver(func() LinearCredential)` | `crux/agent`, used by `crux/proxy` | closure in `cmd/hive` over hub OAuth / `hive.yaml` | a Linear access token or API key out | **credential** | capability |
| `agent.PRCreator` (`CreatePR(ctx, repo, head, base, title, body)`) | `crux/agent` | `*github.Client` (`SetSandboxPRClient`) | PR fields in; `crux.PRResult` out; hive authors as the App bot | hive's forge identity acts; no secret crosses | capability |
| `proxy.GitHubRESTAccounting` (`RecordRESTRequest`, `RecordInjectedRESTRequest`, `CoreRateLimitSnapshot`) | `crux/proxy` | `cmd/hive`'s `githubRESTAccounting{}` over `pkg/github` package state | caller, method, path, status and the rate-limit response headers in; the shared core-bucket snapshot out | telemetry; the snapshot feeds the per-agent GitHub budget's reserve floor | capability (hive owns the ledger) |
| `proxy.UsageSink` (`Record`, `RecordCopilot`, `RecordCopilotWithCache`) | `crux/proxy` | `*tokens.InferenceSink` | per-agent inference token counts in | telemetry (billing) | capability |
| `proxy.CanaryScanner` (`Scan(agent, text, source)`) | `crux/proxy` | `*ioscan.CanaryRegistry` | a GitHub write body in; "leaked" + which agent/source out | the body may hold agent work product; the registry holds the planted canaries, which **must not** be shipped to the runtime | capability |
| `toolapprove.TextScanner` (`ScanInput`, `ScanOutput`) | `crux/toolapprove` | `ioscan.ToolApproveScanner` (`SetDefaultTextScanner` in `cmd/hive`'s `init`) | tool payload text in; a verdict with masked snippets out | payload may hold secrets the scanner is there to catch | capability (hive owns the rule set) |
| `watchdog.UsageProber` (`Provider`, `ProbeError`) | `crux/watchdog` | `cmd/hive`'s `usageProber` over `pkg/rotation` | provider name in; sanitised probe error out | hive holds the provider credentials the probe uses | capability (deferred; stays with the rotation watchdog until stage 3 moves it) |
| `crux.ConditionReporter` (`Observe`, `Clear`) | `crux` root, used by `crux/agent`, `crux/watchdog` | `fleetreport.DefaultReporter{}` | class, key, message, scrubbed diagnostics out | diagnostics are already scrubbed at the call site | capability (hive owns the fleet report) |
| `agent.SpanStarter` | `crux/agent` | `tracing.StartSpan` | a live `trace.Span` | none | **runtime feature**: not remotable, see below |
| `agent.ConfigSnapshotter`, `agent.KickObserver`, `Kicker.SetKickObserver`, `proxy.SetAgentTokenSource`, `SetRepoPausedFunc`, `SetAgentRepoScopeFunc`, `SetWriteSurfaceEnforceFunc`, `SetWriteRefusedAuditFunc`, … | `crux/agent`, `crux/proxy` | `cmd/hive` closures over `pkg/config` | policy answers (bools, caps) and observer callbacks | none | runtime features configured by the host; they become API routes or config in stage 3, not callbacks |

Two of the capability rows are not callback-shaped as they stand:

- `AppTokenMinter.WriteAgentToken` has hive write a file into the agents'
  token-cache directory as the agent's uid. Across a process boundary the
  runtime owns that directory and those uids, so the capability collapses
  onto *mint a scoped token* and the runtime writes the cache file itself.
  Stage 3 replaces `SetAppAuth` in sidecar mode with a writer built over the
  mint callback; the in-process path keeps `*github.AppAuth`.
- `SpanStarter` returns an in-process `trace.Span` whose `End` must be
  called by the runtime; there is no sensible remote form. The runtime's
  default (the OpenTelemetry global tracer) is the sidecar answer, with the
  usual W3C context propagation on the API requests, and hive's
  per-component reach bookkeeping stays a hive concern. This is why span
  start is **not** in the callback contract.

### Direction

Runtime → hive. The runtime is the caller for every capability; hive never
pushes a credential into the runtime, and the runtime never receives the
App private key, the installation JWT, the canary registry, the Linear
OAuth refresh token or the rotation providers' credentials. Tokens are
minted per request, scoped to a tier and to the named repositories
(`ScopedTokenForRepos`' existing contract), attributed to an agent, and
carry an expiry the runtime must honour (GitHub installation tokens expire
in an hour; the response says when). The runtime keeps a token only as long
as the operation that asked for it (`pushbroker` holds it for one push, and
already keeps it out of `argv` and logs) or, for the per-agent cache file,
until the expiry the response carried.

Trust boundary: the runtime process is trusted to attribute requests to
the right agent, because it is the enforcement point that identifies the
agent today (the proxy's per-agent routing, the broker's `AgentName`). The
callback endpoint authenticates *the runtime*, not individual agents, and
hive keeps the authority to refuse (an agent not in its fleet, a repo not
in the installation, a tier the agent is not entitled to). An agent cannot
reach the hive socket: it lives in hive's private runtime directory, mode
`0600`, and agents run as their own uids.

### Transport

The callback endpoint reuses `crux/api`'s conventions: HTTP+JSON, routes
under `/v1`, `api.Error` bodies with stable `code`s, a `0600` unix socket in
a uid-owned `0700` directory, optional TCP only behind a bearer token, and
a token-less server that answers only requests it saw arrive on its own
socket. The contract is the stdlib-only package `crux/api/callback`; a
hive that mounts it links nothing of the runtime, and a runtime that calls
it links nothing of hive.

- **Hive serves.** Bidirectional means two listeners: `crux serve` keeps
  its socket for hive's commands, and hive opens a second one for the
  runtime's callbacks. Default location: `hive.sock` next to the runtime's
  socket, so `$CRUX_HIVE_SOCKET` if set, else
  `$XDG_RUNTIME_DIR/crux/hive.sock`, else `<tmp>/crux-<uid>/hive.sock`.
  The two processes run as one uid in one container by default (stage 4
  supervision), so the file mode is the credential, exactly as for
  `crux.sock`. `$CRUX_HIVE_TOKEN` adds a bearer token; `$CRUX_HIVE_LISTEN`
  (TCP, for the k8s sidecar-container option) requires one.
- **Routes** (`callback.Path*`), one per capability, request and response
  bodies as the `callback.<Name>Request/Response` types:

  | Capability | Route | In | Out |
  |---|---|---|---|
  | Health | `GET /v1/health` | — | `{version}` plus which capabilities are configured |
  | Mint scoped token | `POST /v1/tokens` | agent, repo (`owner/name`) or repos (names), tier, requested TTL | token, `expires_at` |
  | GitHub REST accounting | `POST /v1/github/rest` | caller, method, path, status, injected (bool), the `X-RateLimit-*` response headers | `204` |
  | Core rate-limit snapshot | `GET /v1/github/rate-limit` | — | remaining, reset, ok |
  | Inference usage | `POST /v1/usage` | agent, model, kind (`gateway` / `copilot` / `copilot_cache`), input, output, cache read, cache creation | `204` |
  | Canary scan | `POST /v1/scan/canary` | agent, text, source | detected, agent, source (**never the canary token**) |
  | Text scan | `POST /v1/scan/text` | direction (`input` / `output`), text | blocked, critical_injection, findings (kind, masked snippet, rule) |
  | Condition | `POST /v1/conditions` | action (`observe` / `clear`), class, key, message, diagnostics | `204` |
  | Linear credential | `GET /v1/linear/credential` | — | access_token or api_key |
  | Create PR | `POST /v1/pulls` | repo, head, base, title, body | `crux.PRResult`'s fields |

  A capability hive does not have (no GitHub App, no canary registry, no
  Linear) answers `404 not_configured`, which the runtime treats exactly as
  the nil seam it stands in for today (no App → the push fails; no registry
  → nothing is scanned; no Linear → no injection). A hive-side failure of a
  configured capability (GitHub refused the mint) is `502 upstream_error`
  with the message; a bug is `500 hive_error`. Both are refusals to the
  caller.
- **Both ends in Go.** `callback.Capabilities` is the one interface set,
  with wire types on both sides. `*callback.Client` implements it over
  HTTP; the hive-side `pkg/cruxhost` implements it directly over hive's
  packages (`*github.AppAuth`, `*ioscan.CanaryRegistry`,
  `*tokens.InferenceSink`, `fleetreport`, `*github.Client`, the Linear
  resolver) and `callback.NewServer(caps)` serves either. `crux/hivecap`
  turns any `Capabilities` into the ring's seam interfaces
  (`pushbroker.TokenScoper`, `proxy.GitHubRESTAccounting`,
  `proxy.UsageSink`, `proxy.CanaryScanner`, `toolapprove.TextScanner`,
  `crux.ConditionReporter`, `agent.PRCreator`) so the runtime's existing
  setters take them unchanged.
- **In-process mode: no socket.** `cmd/hive` keeps installing its
  implementations into the seams directly, as it does now; that path is
  not changed by this stage and is deleted only in stage 6. Because
  `crux/hivecap` is written against `callback.Capabilities` rather than
  `*callback.Client`, a hive that wants one wiring path for both modes can
  hand `pkg/cruxhost`'s `Capabilities` value to `hivecap` in-process and a
  `*callback.Client` in sidecar mode; the parity test in `crux/hivecap`
  asserts the two produce identical seam behaviour. Stage 3 selects between
  them on `crux.mode`.

### Failure semantics: fail closed

When hive is unreachable, every capability refuses rather than degrades
into a less-safe answer. The runtime already has a refusal path for each
seam, and the callback adapters return the value that takes it:

| Seam | Hive unreachable / unauthorized | Effect |
|---|---|---|
| `TokenScoper` | error wrapping `callback.ErrUnavailable` | `pushbroker` fails the push ("minting push token"); the agent's branch stays local. The per-agent cache file is not rewritten, so the proxy's token source has no token and `rewriteGitHubAuth` forwards the write **without credentials** → upstream 401. The agent continues read-only on public repos. |
| `CanaryScanner` | reports a detection with an empty token and the source suffixed ` (hive callback unavailable)` | with `canaryFailClosed` the proxy refuses the write; without it the write is audited as a leak. Both are what an unscannable body does today. |
| `TextScanner` | a `Blocked` verdict with one finding (`kind=callback`, `rule=hive_unavailable`) | `toolapprove` denies, as `ErrNoTextScanner` does with no scanner. |
| `GitHubRESTAccounting` | records are dropped; `CoreRateLimitSnapshot` returns `ok=false` | the agent GitHub budget applies the per-agent cap alone, the documented behaviour when no snapshot is installed. |
| `UsageSink` | records are dropped | billing undercounts for the outage; counted in `Client.Status()`. |
| `ConditionReporter` | reports are dropped | the fleet report misses the window; counted in `Client.Status()`. |
| `LinearCredential` | empty credential | the proxy injects nothing; the agent's Linear call fails upstream. |
| `PRCreator` | error wrapping `callback.ErrUnavailable` | the sandbox executor reports the kick as failed, same as a forge error. |

`401` from hive (token mismatch) is `callback.ErrUnauthorized` and takes
the same rows; it is never retried with a different credential. The client
has no logger and its errors never contain the bearer token, a minted
token, or a request body; `MintTokenResponse` prints with the token
redacted so an accidental `%v` cannot leak it.

The outage is a runtime condition. `callback.Client.Status()` reports
`Reachable`, the last error, when it began and how many calls failed
closed; the class name `hive_callback_unavailable` is reserved for it.
Stage 4 surfaces it on `GET /v1/health` and as an `api.Event`, and defines
the governor's reaction (no new dispatch, holds kept) alongside the other
"socket is gone" cases. Recovery is per call: the next successful callback
clears the condition; there is no circuit breaker, because the fail-closed
answers are already cheap and the per-call timeout
(`callback.DefaultTimeout`, 5 s) bounds the cost of a dead socket.

### What stage 3 wires

- `cmd/hive`: `cruxhost.New(cruxhost.Deps{...})` over the values it already
  constructs, served on `callback.Listen(callback.DefaultSocket())` next to
  the runtime socket, in sidecar mode only.
- `crux serve`: a `--hive-callback` address (or `$CRUX_HIVE_SOCKET`),
  `callback.NewClient`, `hivecap.New(client)`, and the manager and proxy
  setters fed from it instead of nil.
- `AppTokenMinter` → a mint-callback-backed cache writer in `crux/agent`;
  `AgentMintIssuer` and `UsageProber` follow the same route pattern when
  the watchdog's rotation probe moves.

