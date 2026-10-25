# `crux serve`: the runtime over a local API

`crux serve` exposes a `crux.Runtime` over HTTP+JSON so a process other than
the one that started an agent can run, observe, kick and stop it. It is the
Stage 3 process boundary of the Crux design record
([design/crux.md](design/crux.md), RFC
[#13536](https://github.com/hivecommons/hive/issues/13536), stage 2/3 tracker
[#14138](https://github.com/hivecommons/hive/issues/14138)): a harness other
than hive drives agents through it, and the `crux` CLI has a remote mode that
talks to it. Hive itself does not use the API yet; that is a later decision.

The wire contract is the stdlib-only package `crux/api`
(`src/crux/api`), which a client can import without pulling in the runtime.
The server is `crux/serve`; the Go client is `api.NewClient`.

## Start a server

```sh
cd src && go build -o crux ./cmd/crux
crux --config /etc/hive/hive.yaml serve
# serving 3 agent definition(s) on unix:///run/user/1000/crux/crux.sock
```

`serve` builds the runtime from `--config` (the enabled `agents:` of a
`hive.yaml`) and listens until interrupted, stopping nothing on exit: agents
it started keep running in tmux. Without `--config` it serves an empty
runtime and every agent must arrive as a definition on `POST /v1/agents`.

| Flag / env | Meaning |
|---|---|
| `--socket PATH`, `CRUX_SOCKET` | unix socket to serve on. Default `$XDG_RUNTIME_DIR/crux/crux.sock`, else `<tmp>/crux-<uid>/crux.sock`. The directory is created `0700`, the socket `0600`, and it is never reachable at that path with wider permissions (it is bound under a temporary name, chmod'ed, then renamed). A socket somebody already answers on is refused; a stale one is replaced. |
| `--listen HOST:PORT` | also serve on TCP. **Refused without a token.** |
| `--token T`, `CRUX_TOKEN` | bearer token every request must carry. When set it gates the socket too. |
| `--hive-callback ADDR`, `CRUX_HIVE_SOCKET` (+ `CRUX_HIVE_TOKEN`) | dial hive's callback endpoint (`crux/api/callback`) for the capabilities hive owns — scoped GitHub tokens, PR creation, Linear credentials, text/canary scanners, conditions — and feed the runtime's seams from it instead of leaving them nil. Health reports it under `hive_callback`; an unreachable endpoint raises the `hive_callback_unavailable` condition. (#14246) When the runtime is a manager it also polls hive's host policy from the same endpoint; see [Host policy from hive](#host-policy-from-hive-14138-stage-4). |

`serve` does not use the per-work-dir control socket that a foreground
`crux run` serves (`<workdir>/crux.sock`); the two are different things, see
[crux-cli.md](crux-cli.md#the-runtime-is-in-process).

### Fail closed

- No TCP without a token: `--listen` without `--token`/`CRUX_TOKEN` is a
  usage error. When the server has no token the handler accepts only
  requests it can see arrived over its own unix socket; anything else
  (including a `Handler()` mounted on a foreign `http.Server`) gets `401`.
- With a token, every request on every listener needs
  `Authorization: Bearer <token>`; anything else is `401` with
  `WWW-Authenticate: Bearer realm="crux"`.
- Socket mode `0600`, directory `0700`. The directory must be a real
  directory (not a symlink) owned by the serving uid with no group/other
  bits, or `Listen` refuses to place a socket in it; a pre-existing
  world-readable `<tmp>/crux-<uid>` is an error, not a trust.
- Request bodies are capped at 1 MiB and unknown JSON fields are rejected.

## The API

All routes are under `/v1`. Bodies and responses are JSON; errors are
`{"error": "...", "code": "unauthorized|bad_request|not_found|conflict|runtime_error"}`.

| Route | Body | Runtime method | Returns |
|---|---|---|---|
| `GET /v1/health` | | | `{"version":"v1","agents":N}` |
| `GET /v1/agents` | | `Observer.AllStatuses` | `{"processes":[Process…]}`, sorted by name |
| `POST /v1/agents` | `{"name":…, "definition":{…}}` | `Definitions.AddAgent` (when `definition` is present), `Lifecycle.Start` | `201 {"agent":…, "process":Process}`; `400` bad name, `409` the name already has a definition, `422` the runtime refused it |
| `GET /v1/agents/{name}` | | `Observer.GetStatus` | `Process` |
| `GET /v1/agents/{name}/logs?source=pane\|buffer\|full&n=N` | | `GetOutput` / `GetBufferOutput` / `FullLogHeader`+`CaptureFullLog` | `{"agent","source","lines":[…]}` or `{"header","content"}` for `full` |
| `GET /v1/agents/{name}/logs?follow=1` | | `GetBufferOutput`, polled | `text/event-stream` of `agent.log` records `{"agent","lines":[…]}` |
| `POST /v1/agents/{name}/kick` | `{"message":…}` | `Kicker.SendKick` | `{"agent","process"}` |
| `POST /v1/agents/{name}/stop` | | `Lifecycle.Stop` | `{"agent","process"}` |
| `GET /v1/events` | | watcher + `Kicker.SetKickObserver` + the `crux.Host` observers | `text/event-stream` of `Event` records |

### Runtime families (#14138 stage 3)

Every method of `crux.Runtime` has a route; `api.RuntimeRoutes` is the
method → route table and `crux/api/client`'s parity test fails when a method
is added to the interface without an entry. The sidecar client
(`crux/api/client.Runtime`, selected by `CRUX_MODE=sidecar`) is written
against this table. Routes marked ⚑ are new in stage 3; the rest predate
it or are the CLI routes above under another method.

| Family | Route | Runtime method(s) | Notes |
|---|---|---|---|
| Lifecycle | `POST /v1/agents/{name}/start` ⚑ | `Start` | |
| | `POST /v1/agents/{name}/restart` ⚑ | `Restart`, `RestartWithBootstrap` (`{"bootstrap":…}`) | |
| | `POST /v1/agents/{name}/pause` ⚑ | `Pause`, `PauseBy`, `Supervisor.PauseByCause` | body selects the variant |
| | `POST /v1/agents/{name}/resume` ⚑ | `Resume` | |
| | `DELETE /v1/agents/{name}/restart-count` ⚑ | `ResetRestartCount` | |
| | `GET /v1/agents/{name}` | `IsPaused`, `Observer.ResolveAgent` | |
| Observer | `GET /v1/snapshots`, `GET /v1/agents/{name}/snapshot` ⚑ | `AllStatuses`, `GetStatus`, `GetStatusFast` | full `crux.Process` snapshot, not the wire projection |
| Kicker | `POST /v1/agents/{name}/kick/validate` ⚑ | `ValidateKick` | `409 kick_refused` |
| | `GET /v1/agents/{name}/kick` ⚑ | `KickDispatchState` | |
| | `GET /v1/agents/{name}/kicklogs[/{id}]` ⚑ | `ListKickLogs`, `ReadKickLog` | |
| Definitions | `POST /v1/definitions`, `POST /v1/definitions/reconcile` ⚑ | `AddAgent`, `ReconcileAgents` | |
| | `DELETE /v1/agents/{name}`, `PUT /v1/agents/{name}/definition` ⚑ | `RemoveAgent`, `UpdateConfig` | |
| | `PUT /v1/agents/{name}/overrides`, `PUT`/`DELETE /v1/agents/{name}/pins/{kind}` ⚑ | `SetModelOverride`, `SetBackendOverride`, `PinCLI`, `PinModel`, `UnpinCLI`, `UnpinModel` | |
| | `POST /v1/fleet/modes/{clear,sync}`, `POST /v1/fleet/inference/refresh` ⚑ | `ClearModeOverrides`, `SyncModeFiles`, `RefreshInferenceRoutes` | |
| Fleet | `GET /v1/fleet` ⚑ | `GetACMMLevel`, `SharedDevUID`, `Supervisor.ConsentWedgedAgents`, `CountAgentsWithModel` | one read |
| | `GET`/`PUT /v1/fleet/breaker`, `POST /v1/fleet/breaker/{engage,release}` ⚑ | `BreakerState`, `Restorer.RestoreBreaker`, `EngageBreaker`, `ReleaseBreaker` | |
| | `PUT /v1/fleet/acmm`, `POST /v1/fleet/relaunch-bob` ⚑ | `SetACMMLevel`, `RelaunchBobAgentsAwaitingKey` | |
| Credentials | `POST /v1/agents/{name}/login-code` ⚑ | `SubmitLoginCode` | |
| | `GET`/`PUT /v1/credentials/copilot` ⚑ | `CopilotToken`, `CopilotTokenSource`, `SetCopilotToken`, `ActivateCopilotToken` | |
| | `GET /v1/credentials/claude`, `POST /v1/credentials/claude/reload` ⚑ | `ClaudeCredentialCandidatePaths`, `ReloadClaudeToken` | |
| Resumer | `GET /v1/agents/{name}/session` ⚑ | `ResumeHandle`, `SessionID`, `SessionMissing` | |
| | `POST /v1/agents/{name}/session/kick` ⚑ | `SendResumeKick` | `409 resume_busy`, `410 resume_session_gone`, `422 resume_unsupported` map back to the `crux.ErrResume*` sentinels |
| Supervisor | `GET /v1/agents/{name}/supervision` ⚑ | `GovernorMayStart`, `AgentCredentialRefreshable`, `AgentHasValidCredential`, `AgentCapabilities`, `EffectiveBackend`, `InvocationMetadata`, `RestartTelemetry`, `StartFailureState`, `ProviderErrorBackoffRemaining`, `CrashOOMSuspected`, `AgentAuthAvailable` | one read |
| | `POST /v1/fleet/{startup-queued,restart-crashed,tokens/refresh,kicklogs/archive}` ⚑ | `MarkStartupLaunchQueued`, `CheckAndRestartCrashedAgents`, `RefreshAgentTokens`, `ArchiveAllKickLogs` | |
| | `POST /v1/agents/{name}/tokens/refresh`, `POST /v1/agents/{name}/nudge` ⚑ | `RefreshAgentTokenFor`, `SendGitHubAPIBudgetNudge` | |
| | `GET /v1/backends/{backend}/auth`, `POST /v1/audit`, `GET /v1/trajectory` ⚑ | `BackendAuthAvailable`, `RecordAudit`, `TrajectoryAgents` | |
| Restorer | `POST /v1/agents/{name}/seed` ⚑ | `SeedPauseState`, `SeedRestartTelemetry`, `SeedTurnLoss`, `SeedLastKick`, `SeedKickHistory` | one body, each field optional |
| Background | `GET /v1/health` | `StartAgentTokenRefresh`, `StartCredentialWatchdog`, `StartCopilotSessionRefresh`, `StartTranscriptActivityRefresh` | the loops run server-side; the client's calls are no-ops |
| Authorizer | `POST /v1/authorize` ⚑ | `AuthorizePROpen`, `AuthorizePushBranch`, `AuthorizeIssueOpen`, `AuthorizeMerge`, `AuthorizeReviewRequest` | synchronous; `{"action","agent","file_uid","kind","repo"}`; `403 forbidden` carries the refusal, any transport error is a refusal (fail closed) |
| Watchdog | `GET /v1/watchdog/agents`, `GET /v1/agents/{name}/watchdog`, `PUT /v1/agents/{name}/watchdog/conditions` ⚑ | `watchdog.Source` (`AgentNames`, `Observe`, `LastProduction`, `SetConditions`) | `501 not_supported` when the runtime has no watchdog source; `crux/runtime.NewWatchdogFleet` builds the fleet over these in sidecar mode |

`Process` is the wire projection of `crux.Process`: `name`, `id`, `state`,
`backend`, `model`, `pid`, `paused` (+ reason, trigger, by), `started_at`,
`last_kick`, `restart_count`, `last_restart_reason`, `kick_refused`,
`kick_refusal_reason`, `kicks_undeliverable`, `busy_condition`,
`start_failure_class`. It is a subset by design: the full snapshot carries
engine-internal state that is neither stable nor meaningful off-process.

`definition` on `POST /v1/agents` is the JSON form of
`crux/config.AgentConfig`, the per-agent overlay hive reads from
`agents_dir`. The server applies the same gate and defaults the YAML loader
does: `name` must match `^[a-z0-9][a-z0-9_-]*$` (≤64 chars); `id` and
`role` default to the name, `replicas` to 1, `enabled` to true unless the
JSON says otherwise. A name the runtime already knows is a `409 conflict`
rather than a silent start of the old definition; stop and re-post. Without
`definition`, `name` may also be an agent ID. Parsing the portable
`AgentDefinition` format is the caller's job, as it is the host's job for
the CLI (the CLI does it before sending).

Agents start under the server's lifetime, not the request's: the runtime
ties an agent to the context it was started with, so a client disconnecting
does not kill what it started.

### Events

`GET /v1/events` is a server-sent-event stream; each record is
`event: <type>` then `data: <Event JSON>`, with `: keep-alive` comments
every 15 s. `Event` is `{"time","type","agent","state","detail","data"}`.

| Type | When |
|---|---|
| `agent.started`, `agent.stopped`, `agent.kicked` | the API performed the action |
| `agent.state` | the server's watcher (polls `AllStatuses` every second) saw an agent change state for any reason: crash, restart, pause; `state` is the new state, `detail` the last restart reason when there is one |
| `agent.kick` | the runtime's kick observer fired (`Kicker.SetKickObserver`); `detail` is the observer's event name |
| `agent.kick_outcome`, `agent.pause_transition`, `agent.relaunch`, `agent.pause_persist`, `agent.prompt`, `agent.sandbox_audit`, `runtime.audit`, `runtime.condition` | the `crux.Host` observers, relayed when the runtime is a Host; `data` carries the typed payload (`api.KickOutcome`, `api.PauseTransition`, …). A sidecar hive demuxes these into the observers its boot sequence installs (`cmd/hive`'s `sidecarHost`) |

A subscriber that stops reading is dropped after 64 buffered events so one
slow client cannot stall the others.

### Host policy from hive (#14138 stage 4)

With `--hive-callback`, `serve` also reads the values an in-process hive
hands the manager through the `crux.Host` policy setters. They travel on
the hive callback endpoint (`crux/api/callback`, runtime → hive), not on
this API:

| Callback route | Feeds |
|---|---|
| `GET /v1/policy` | a `callback.Policy` snapshot, polled every 15 s (`hivecap.WatchPolicy`): `build` → `SetBuildInfo`, `explain_mode_default` → `SetExplainModeDefaultResolver`, `kick_log_source` → `SetKickLogSourceResolver`, `gateway_backends` → `SetGatewayBackendChecker`, `dead_session_recovery_owned_elsewhere` → `SetDeadSessionRecoveryOwner`, `sandbox` (the `agent_sandbox` block) → `SetSandboxConfig`; `repo_auto_merge: true` installs the next row |
| `POST /v1/policy/repo-auto-merge` | `{"repo"}` → `{"enabled"}` for `SetRepoAutoMergeEnabledResolver`, asked per merge authorisation |

The resolvers answer from the last snapshot (the manager reads them under
its own lock, so they never call out) and keep it through a hive outage;
the auto-merge call fails closed, refusing the merge. A value hive does not
report, or a hive without the `policy` capability (`404 not_configured`),
leaves the runtime's own default in place. `SetInferenceCallbacks`,
`SetSandboxMutationBoundary` and `SetSpanStarter` are not carried yet.

## Drive it with curl

```sh
S=/run/user/$(id -u)/crux/crux.sock
curl -s --unix-socket $S http://crux/v1/health
curl -s --unix-socket $S http://crux/v1/agents | jq '.processes[] | {name, state}'

# run from a definition (an agents_dir-style AgentConfig document)
curl -s --unix-socket $S -X POST http://crux/v1/agents \
  -H 'Content-Type: application/json' \
  -d '{"name":"fixer","definition":{"backend":"claude","model":"claude-sonnet-4-6","role":"fixer"}}'

curl -s --unix-socket $S 'http://crux/v1/agents/fixer/logs?source=buffer&n=50' | jq -r '.lines[]'
curl -s -N --unix-socket $S 'http://crux/v1/agents/fixer/logs?follow=1'     # streams until Ctrl-C
curl -s --unix-socket $S -X POST http://crux/v1/agents/fixer/kick \
  -H 'Content-Type: application/json' -d '{"message":"look at the failing build on main"}'
curl -s -N --unix-socket $S http://crux/v1/events                            # in another terminal
curl -s --unix-socket $S -X POST http://crux/v1/agents/fixer/stop
```

Over TCP with a token:

```sh
CRUX_TOKEN=$(openssl rand -hex 16) crux --config hive.yaml serve --listen 127.0.0.1:7777
curl -s -H "Authorization: Bearer $CRUX_TOKEN" http://127.0.0.1:7777/v1/agents
```

The hostname in a unix-socket URL is ignored; `crux` is a convention.

## Drive it with the CLI

`--server ADDR` (or `CRUX_SERVER`) switches `run`, `ps`, `logs`, `kick` and
`stop` to the API; `--token` (or `CRUX_TOKEN`) carries the bearer token.
`ADDR` is a socket path, `unix:///path`, `host:port` or `http(s)://host:port`.

```sh
export CRUX_SERVER=/run/user/$(id -u)/crux/crux.sock
crux run examples/agents/customized-agent.yaml   # definition parsed here, sent as JSON; follows output; Ctrl-C stops
crux run --detach fixer                          # a name already defined on the server
crux ps
crux logs fixer -n 50 --buffer
crux logs fixer -f                               # follow the ring buffer over SSE
crux kick fixer "look at the failing build on main"
crux stop fixer
```

In remote mode nothing is built in this process: no runtime, no control
socket, no `--config` (the server's definitions are what exist). `serve`
itself cannot be run remotely. A `401` is reported with a hint to set the
token.

## Drive it from Go

```go
import "github.com/hivecommons/hive/crux/api"

c, err := api.NewClient(os.Getenv("CRUX_SERVER"), os.Getenv("CRUX_TOKEN"))
procs, err := c.List(ctx)
resp, err := c.Run(ctx, api.RunRequest{Name: "fixer", Definition: defJSON})
err = c.FollowLogs(ctx, "fixer", func(ch api.LogChunk) error { …; return nil })
err = c.Events(ctx, func(ev api.Event) error { …; return nil })
_, err = c.Stop(ctx, "fixer")
```

`crux/api` imports only the standard library, so a client binary does not
link the runtime. Errors from the server are `*api.StatusError`;
`api.IsNotFound` and `api.IsUnauthorized` classify them.
`Client.WaitReady` polls `/v1/health` for a caller that just launched
`crux serve`.

## What this is not

It is not tenancy, quotas or hive-as-a-client: the server trusts whoever can
reach the socket or holds the token, and hive still constructs
`*agent.Manager` in-process. Those are the rest of Stage 3 in
[design/crux.md](design/crux.md#after-the-cut).
