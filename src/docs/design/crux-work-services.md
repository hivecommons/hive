# Crux work services: S1 pinned checkout (hivecommons/crux#114)

Status: **S0 caller principals and S1 pinned checkout shipped in hive
`src/crux` (v5).** S2 contained run and the S7 minimum shipped in #17014; S3 human gate is [below](#s3-human-gate); S4 command log is [below](#s4-command-log); S5 turn signals are [below](#s5-turn-signals); S6 agent step is [below](#s6-agent-step); S7 saved run results is complete and [below](#s7-saved-run-results). How an orchestrator composes the slices end to end is the [integration guide](../crux-integration-guide.md). This records
S1 *as implemented*, including where it is weaker than the RFC text.

The RFC asks for "a working copy of this repo at this commit, owned by
this caller, on this server" as the first work service, so that S2 runs
have something to run in. This is that: `crux serve` grows a
`checkout.Service` behind `/v1/checkouts`, and the `crux` CLI grows
`checkout add|rm|list`.

## Surface

### HTTP (`src/crux/api/checkouts.go`, `src/crux/serve/checkouts.go`)

| Route | Body | Response |
|---|---|---|
| `POST /v1/checkouts` | `{"repo", "commit", "owner"?, "branches"?, "max_bytes"?, "ttl"?}` | `201 Checkout` |
| `GET /v1/checkouts[?owner=NAME]` | | `200 {"checkouts":[Checkout…]}` sorted by id |
| `GET /v1/checkouts/{id}` | | `200 Checkout` |
| `DELETE /v1/checkouts/{id}` | | `204` |

`Checkout` is `{"id","path","repo","commit","owner","branches","max_bytes",
"bytes","ttl"?,"created_at","expires_at"?}`. Nothing in it is secret: the
record never carries environment, and repository URLs that embed
credentials are refused before anything is cloned.

Error codes, all `api.Error` JSON:

| Condition | Status | `code` |
|---|---|---|
| `commit` is not a full 40-hex SHA (a tag, branch or short SHA) | 400 | `bad_request` |
| bad `ttl`, bad `owner`, negative `max_bytes`, malformed URL, unknown JSON field | 400 | `bad_request` |
| `repo` matches no allowlist prefix; a project principal naming another `owner`; reading or deleting a checkout you do not own | 403 | `forbidden` (the message names the allowed prefixes or the owner) |
| unknown id | 404 | `not_found` |
| `DELETE` while runs use it (the `InUse` hook) | 409 | `conflict` |
| clone plus worktree measure more than `max_bytes` | 413 | `too_large` |
| the server has no checkout service; the size could not be measured | 501 | `not_supported` / `unsupported` |
| the worktree's `HEAD` is not the requested SHA | 502 | `upstream_error` (names both SHAs) |
| git failed | 500 | `runtime_error` |

A server without `--checkout-allow` registers the routes and answers 501
on all of them, so a client can tell "not here" from "not found".

### CLI (`src/crux/cli/checkout.go`)

```
crux checkout add  -repo URL -commit SHA [-owner NAME] [-ttl 72h] [-max-bytes N] [-no-branches] [-json]
crux checkout rm   ID
crux checkout list [-owner NAME] [-json]
```

Local (`crux checkout …`) runs the service in-process on
`<workdir>/checkouts` (`-dir`/`CRUX_CHECKOUT_DIR`) with `-allow`/
`CRUX_CHECKOUT_ALLOWLIST` as its allowlist; without an allowlist it can
still list and remove but refuses every `add`. Remote (`crux --server …
checkout …`) drives the server's service through the API client. The
verb is not forwarded over the foreground control socket. Bad requests
and refusals exit with the usage status, everything else with the error
status.

### Server flags (`crux serve`)

| Flag / env | Meaning |
|---|---|
| `--checkout-allow PREFIX,…`, `CRUX_CHECKOUT_ALLOWLIST` | repository URL prefixes `POST /v1/checkouts` may clone. **Unset means no checkout service.** |
| `--checkout-dir PATH`, `CRUX_CHECKOUT_DIR` | where checkouts live. Default `<workdir>/checkouts`. |
| `--checkout-max-bytes N`, `CRUX_CHECKOUT_MAX_BYTES` | server ceiling on `max_bytes`: a request asking for more is 400 `bad_request`; a request naming none gets the ceiling. Default 5 GiB. |
| `CRUX_CHECKOUT_TOKEN` (env only) | HTTPS token for private repositories, handed to git through a credential helper. Never on a command line, in a record or in an error. |

## Semantics

**Pin.** `commit` must be a full 40-hex SHA; tags and branches are refused
with `bad_request` because a moving ref is not a pin. After `git worktree
add --detach <sha>` the service runs `git rev-parse HEAD` in the worktree
and refuses with 502 naming both SHAs if they differ.

**Allowlist, deny by default.** The allowlist is a list of URL prefixes
(`https://github.com/hivecommons/`, `file:///srv/mirrors/`). Each entry
and each request URL must be `https://` or `file://`, have a host (or a
path, for `file`), and carry no userinfo, query or fragment; the request
URL must start with an entry. An empty allowlist refuses everything. This
is the shape of hive's `nousdeps` trust rules, ported without the
per-package specifics. The API never accepts a credential.

**Private repositories.** The server may carry one HTTPS token,
`CRUX_CHECKOUT_TOKEN` (`checkout.Config.Token`). It reaches git through a
credential helper the service configures per invocation (`-c
credential.helper=` to drop any host-wide helper, then an inline helper
that answers `username=x-access-token` and the password from the child's
`CRUX_CHECKOUT_GIT_TOKEN` environment), so the token is never an argument,
never in the remote URL and never in a record; git failure output is
passed through `logscrub` before it reaches an error. Every git child runs
with `GIT_TERMINAL_PROMPT=0`, so a missing or wrong token fails the clone
instead of hanging on a prompt. The helper answers only when a server
asks, so public and `file://` repositories are unaffected. One token per
server: scoping it per owner or minting it from hive's callback is left to
a later slice.

**Ownership via S0.** `owner` is recorded on every checkout. The caller's
[principal](../crux-serve.md#caller-principals) decides what it may name:
a lane project principal may create only checkouts owned by itself and
sees only its own on `GET` (an `?owner=` filter is overridden); the
operator may name any owner (default: its own name) and sees everything.
`GET /v1/checkouts/{id}` and `DELETE` succeed for the owner and the
operator and answer 403 for anyone else.

**Live runs block delete.** `checkout.Config.InUse func(id string) bool`
is the hook S2 wires to its run state; the default returns false. A true
answer turns `DELETE` into 409 `conflict`. `crux serve` wires it to
`runs.Service.InUse`, which answers true while a run in flight names the
checkout. Delete is otherwise
`git worktree remove --force` (falling back to `os.RemoveAll`), the record
file removed, and an index line appended naming who deleted it.

**TTL is recorded, not enforced.** `ttl` is parsed as a Go duration and
stored with `expires_at`. There is no sweeper yet; `list -json` exposes
`expires_at` so one can be written against the index.

**Byte cap, honestly.** Git cannot bound a clone before it happens; there
is no pre-flight size query across the transports we allow. The cap is
therefore *post-hoc*: the service clones (`--no-checkout`, plus
`--single-branch` when `branches` is false), adds the worktree, then walks
the regular files of the shared clone and the worktree and sums their
sizes. Over `max_bytes` it removes the worktree and
answers 413 `too_large` with both numbers; if the measurement itself
fails it answers 501 `unsupported`. The bytes were already transferred
and the shared clone is kept (a later request for the same repo reuses
it). `--filter=blob:limit=` partial clones would make the clone itself
smaller but would also make the worktree need network at checkout time;
that trade is not taken here and is deferred. The record stores both
`max_bytes` and the measured `bytes`.

The server bounds what a request may ask for: `checkout.Config.MaxBytes`
(`--checkout-max-bytes`, `CRUX_CHECKOUT_MAX_BYTES`, default
`DefaultMaxBytes` = 5 GiB) is a ceiling. A request whose `max_bytes`
exceeds it is refused before any clone with 400 `bad_request`
(`checkout.ErrAboveCeiling`, which also matches `ErrBadRequest`), naming
both numbers; a request with no `max_bytes` gets the ceiling as its cap.
A caller therefore cannot opt out of the operator's bound by asking for
more.

**Bound on live worktrees.** `checkout.Config.MaxCheckouts` (default 64)
is the `sandbox.WorktreeManager` cap, counted as `.git` entries under the
runs directory.

## Storage

Under `<checkout dir>` (`WorkDir()/checkouts` by default):

```
checkouts/<id>.json           one Record per checkout, 0600, written tmp+rename
checkouts/index.jsonl         append-only {op, id, owner, repo, commit, at, by}
checkouts/src/<owner>-<h>/    the shared bare-ish clone (--no-checkout), per owner and repo
checkouts/runs/<id>/pin-1/    the worktree handed back as `path`
```

Ids are `chk_` plus twelve hex characters. The `src` directory is keyed
by owner and a hash of the repository URL so two owners never share a
clone (a project cannot be made to pay for, or read, another's fetches).
Records follow the `sandbox_run.go` conventions: one file per entity plus
a `.jsonl` index.

## Running in a checkout (S1 ↔ S2)

`POST /v1/runs {"checkout": "chk_…"}` (CLI `crux --server … runs start
--checkout chk_… -- cmd`) executes the command in the checkout's pinned
worktree; the run record carries `checkout` and `workspace`. The checkout
must belong to the caller or the caller must be the operator (403
`forbidden` otherwise, the same rule as delete); an unknown id is 400
`bad_request`; a server without a checkout service answers 501
`unsupported`. `/v1/runs` admits project principals like `/v1/checkouts`
does: a run is owned by the principal that created it, and only the owner
or the operator may read or cancel it. While a run is live its checkout
cannot be deleted (409 `conflict`); once the run settles, it can.

**Kubernetes Job network (S2 follow-up).** The `job` backend takes
`job_network: none | host | default` in the runs config. The default is
`none`: before each Job the launcher creates a `NetworkPolicy` in the Job
namespace, named after the Job and selecting its pod by the
`hive.kubestellar.io/job=<name>` label, with `policyTypes: [Ingress,
Egress]` and no rules — deny all, both ways — and deletes it when the
Job is deleted (success, timeout, failed Job create) or when the Job is
left behind for inspection. If the policy cannot be created the run is
refused rather than started open. `host` sets `hostNetwork: true` (with
`dnsPolicy: ClusterFirstWithHostNet`); `default` is the pod network with
nothing added. The backend claims the `none` capability when
`job_network` is `none` or the operator asserts `job_network_policy`
(their own policy already denies egress). As with any NetworkPolicy, the
isolation is only as real as the cluster's CNI: on a CNI that ignores
policies the Job still has network, and the capability claim is the
operator's to make honestly. `kubejob.Launcher` itself, shared with
hive's agent sandbox, keeps `default` when `Network` is empty; only the
contained-run backend defaults to `none`.

## S3. Human gate

A driver asks a person to decide something before it continues; the
answer is recorded and survives a restart of `crux serve`. Package
`src/crux/gates` (`gates.Service`), API types in `src/crux/api/gates.go`,
routes in `src/crux/serve/gates.go`, CLI in `src/crux/cli/gate.go`. The
service needs no configuration: `crux serve` always opens it on
`<workdir>/gates`.

| Route | Body | Response |
|---|---|---|
| `POST /v1/gates` | `{"subject", "context"?, "choices"?, "timeout"?, "opened_by"?, "idempotency_key"?}` | `201 Gate` new; `200 Gate` the same gate re-opened |
| `GET /v1/gates[?status=waiting\|answered\|expired]` | | `200 {"gates":[Gate…]}` oldest first |
| `GET /v1/gates/{id}` | | `200 Gate` |
| `POST /v1/gates/{id}/answer` | `{"answer", "note"?, "as_operator"?}` | `200 Gate` |
| `GET /v1/gates/{id}/wait[?timeout=30s]` | | `200 Gate` once answered or expired, or still `waiting` when the window (default 30 s, capped at 5 min) closes |

`Gate` is `{"id","status","subject","context"?,"choices","timeout",
"opened_by","opened_by_principal","opened_at","expires_at"?,"answer"?,
"answered_by"?,"answered_by_principal"?,"answered_at"?,"note"?,
"operator_override"?,"expired_at"?}`.

```
crux --server ADDR gate open --subject S [--choice C]… [--timeout D] [--context JSON] [--opened-by L] [--idempotency-key K] [--json]
crux --server ADDR gate list [--status S] [--json]
crux --server ADDR gate answer <id> --choice C [--note TEXT] [--as-operator] [--json]
crux --server ADDR gate wait <id> [--timeout D] [--json]
```

`gate wait` prints the answer and exits 0, or exits 1 with nothing on
stdout when the gate expired. Like `runs`, the verb exists only with
`--server`: the server is the single writer that records identities,
journals decisions and publishes events.

| Condition | Status | `code` |
|---|---|---|
| empty subject, bad `timeout`, duplicate or empty choices, `context` not one JSON value, unknown `status` filter | 400 | `bad_request` |
| a project answering, or reading a gate another principal opened | 403 | `forbidden` |
| the opener answering its own gate without `as_operator` | 403 | `gate_self_answer` |
| unknown id | 404 | `not_found` |
| a different answer to an answered gate, any answer to an expired one | 409 | `gate_closed` |
| `context` over 64 KiB once encoded | 413 | `gate_context_too_large` |
| `answer` not one of `choices` (free text belongs in `note`) | 422 | `gate_invalid_choice` |
| the server has no gate service | 501 | `not_supported` |

**Semantics.**

- *Idempotent open.* The key is derived the way `toolapprove`'s inbox
  derives one: from the subject, the canonically re-encoded context
  (object keys sorted), the choices, the timeout and `opened_by` — never
  timestamps — or from `idempotency_key` when given. Either form is
  scoped to the opening principal. Re-opening returns the existing record
  in whatever state it is in, including the recorded answer.
- *Who answers.* Only the operator principal. Projects open, read, list
  and wait on their own gates. When the answering principal is the one
  that opened the gate, `as_operator` is required; the record and journal
  carry `operator_override: true` and the server emits a `runtime.audit`
  event (`gate.operator_override`). Retrying the exact recorded answer is
  a no-op `200`, not a conflict.
- *Expiry.* `timeout` is a Go duration; `"0"` or absent waits
  indefinitely. A gate past its deadline becomes `expired` with no
  answer the first time anything reads it, and a waiter's own timer flips
  it at the deadline. Expiry is neither approval nor denial.
- *Secrets.* Subject, note, `opened_by` and every string inside
  `context` pass through `logscrub.ScrubString` before they are stored.
  Events and the journal never carry `context` or `note`.

**Storage.**

```
gates/pending/<id>.json   waiting gates, 0600, tmp+rename
gates/resolved/<id>.json  answered or expired gates, kept forever
gates/gates.jsonl         append-only {at, op, id, status, by?, answer?, operator_override?, expires_at?}
```

Resolving writes the resolved record before removing the pending one;
after a crash between the two, the resolved copy wins on the next start.

**Deliberately not in S3.** No background sweeper (expiry is observed on
read and by waiters, so a gate nobody looks at stays `waiting` on disk
past its deadline until the next read); no in-process `crux gate` form;
no per-gate answerer allowlist beyond "the operator".

## S4. Command log

What a contained run executed, kept with the run and readable after it.
Package `src/crux/cmdlog` (`cmdlog.Log`, `cmdlog.Read`), API types in
`src/crux/api/cmdlog.go`, the route in `src/crux/serve/runs.go`, CLI in
`src/crux/cli/cmdlog.go`. No configuration: `crux serve` wires
`runs.Service.CmdLog = cmdlog.FileOpener(cmdlog.Policy{})` whenever the
runs service is on.

| Route | Query | Response |
|---|---|---|
| `GET /v1/runs/{id}/cmdlog` | `since_seq` (default 0, exclusive), `limit` (default 100, cap 1000) | `200 {"run", "commands":[Command…], "next_seq", "more"?}` ascending by `seq` |

A `Command` is `{seq, command[], cwd?, capture, status, reason?,
started_at, ended_at?, duration_ms?, exit_code?, stdout?, stderr?}`;
`stdout`/`stderr` are `{bytes, head?, tail?, truncated?}` — the full byte
count always, and up to 4 KiB from each end of the text the runner kept.
`status` is one of `completed` (exit 0), `failed` (non-zero exit,
refusal or timeout), `unfinished` (the sandbox never reported an exit)
or `cancelled`. A `started` line with no `ended` line is an in-flight
command, returned as `unfinished` with no `ended_at`; a status is never
guessed. Paging: pass the previous page's `next_seq` as `since_seq` while
`more` is true. `next_seq` stops before the first record with no
`ended_at`, so a follower re-receives an in-flight command until it ends;
a record with `ended_at` is terminal whatever its `status`, `unfinished`
included. `more` is never true on a page whose `next_seq` equals the
`since_seq` it answered, so that loop always makes progress.
Visibility follows the run — a project reads only its own runs' logs
(403 otherwise), unknown run 404, bad `since_seq`/`limit` 400, a run that
kept no log (or an older server) is an empty page, a corrupt line 500
`runtime_error`, no runs service 501.

```
crux --server ADDR cmdlog list [--since N] [--limit N] [--all] [--json] <run>
crux --server ADDR cmdlog show [--json] <run> <seq>
```

**Hook.** The S2 runner records through `cmdlog.Recorder`:
`Started(Start) (seq, error)` is called before the launcher starts the
command and `Ended(seq, End) error` after the result is classified.
`runs.Service.CmdLog` is a `cmdlog.Opener func(runDir) (Recorder, error)`;
a nil opener, or an opener that fails, disables logging for that run and
never fails the run itself. Command text, reason and both output heads
and tails pass through `logscrub.ScrubString` before they are written.

**Storage.** `<workdir>/runs/<id>/commands.jsonl`, 0600, append-only,
two lines per command: `{"event":"started","seq",…}` and
`{"event":"ended","seq",…}`. Readers fold the pair; a stray `ended`
without its `started` is dropped. Re-opening a run's log resumes `seq`
after the highest one on disk and ends every command an earlier writer
left open with an `ended` line of status `unfinished` (reason
`cmdlog.OrphanReason`, no exit code): a run has one live writer at a
time, so that command's exit is never coming. `crux serve` reopens the
log of every run it reconciles at start, so a crash mid-command leaves a
terminal record rather than one that pins `next_seq` forever.

**Under-counting, stated plainly.** Every record carries `capture`; in
S4 the only value written is `"runner"`, which means *the command the
runner handed to the sandbox launcher*. A contained run is one launcher
invocation, so a run's log has exactly one `runner` record. Anything that
command spawns — `sh -c '…'` pipelines, `make` recipes, `go test`'s
child processes, an agent's own tool calls inside the sandbox — is **not
counted**; it is inside the `stdout`/`stderr` of its parent. The RFC's
"verify under-counting by honest agents" is therefore resolved for S4 by
making the count's meaning explicit rather than by widening it: a
consumer must not read `len(commands)` as "commands the agent ran". The
value `"hook"` is reserved in `api.CommandCaptureHook` for a later slice
in which an in-sandbox shim reports child commands over the same
`Recorder`; it is never written today.

**Deliberately not in S4.** No in-sandbox shim or shell wrapper; no
per-agent `/v1/agents/{name}/commands` view; no `crux commands verify`;
no output beyond head/tail (the run record already keeps the capped
stream); no retention policy — the log lives and dies with the run dir.

## S5. Turn signals

A driver that kicks a contained run or an agent learns, from a saved
record and not only from a live event it may miss, when the turn ended
and how, and while it runs, where it is. Package `src/crux/signals`
(`signals.Book`, `signals.Read`, `signals.AgentOutcomes`), API types in
`src/crux/api/signals.go`, routes in `src/crux/serve/runs.go` and
`src/crux/serve/signals.go`, CLI in `src/crux/cli/signal.go`. No
configuration: `crux serve` sets `runs.Service.Signals =
signals.NewBook(signals.Policy{})` and `Server.KickOutcomes` on
`<workdir>/signals/agents`.

| Route | Body / query | Response |
|---|---|---|
| `GET /v1/runs/{id}/signals` | `since_seq` (default 0, exclusive), `limit` (default 100, cap 1000) | `200 {"run", "signals":[Signal…], "next_seq", "more"?}` ascending by `seq` |
| `POST /v1/runs/{id}/signals` | `{"kind", "message"?, "data"?}` | `201 Signal` |
| `GET /v1/agents/{name}/kick-outcome` | | `200 {"agent", "outcome"?, "last_kick"?, "settled_for_last_kick"}` |

A `Signal` is `{seq, kind, source, at, message?, data?}`. `kind` is the
closed set `started | thinking | tool | blocked | needs-input | input |
step | done`; `source` is `runner`, `gate` or `caller`, and each source
appends only its own kinds:

| Source | Kinds | When |
|---|---|---|
| `runner` | `started`, `step`, `done` | `started` just before the sandbox launcher runs the command (for an agent session, once its workspace is ready); `step` once per finished agent step (S6, `data` `{"step", "status", "attempts"}`, message the step's reason); `done` on every terminal path — completed, failed, timed out, cancelled, a provisioning failure (no `started`), and the run `crux serve` marks `unfinished` at start because it was in flight at the last shutdown. `done.data` is `{"status", "exit_code"?, "timed_out"?}`, its message the run's reason. |
| `gate` | `needs-input`, `input`, `blocked` | a gate opened with `run`: opening is `needs-input` (message the subject), answering `input` (`data` `{"gate", "answer", "answered_by"}`), expiry `blocked`. `data.gate` names the gate; its note and context never ride along. |
| `caller` | `thinking`, `tool`, `blocked`, `needs-input`, `input` | `POST /v1/runs/{id}/signals` by the run's owner or the operator. |

`done` is always a run's last signal: anything after it is refused (409
`signals_closed`), including after a restart, and so is a caller's or a
gate's signal on a run whose record is already terminal (one that ended
before its `done` line, or on a server that kept no signals). The message is trimmed,
scrubbed with `logscrub.ScrubString` and bounded to 1 KiB (a caller's
longer message is 400; the runner's and a gate's are cut on a rune
boundary); `data` must be one JSON value, at most 8 KiB once encoded,
with every string leaf scrubbed. Every durable signal is published as
`run.signal` on `/v1/events` with `data` `{"run", …Signal}`, after the
line is written. A follower polls with the previous page's `next_seq`
until it reads `done`.

`POST /v1/gates` accepts an optional `run`: the run must exist (400
`bad_request` otherwise) and belong to the opener or the operator (403
`forbidden`); a server without a runs service answers 501
`not_supported`. The run is part of the derived idempotency key only when
set, so gates opened without one keep their keys. A gate signal that
cannot be written (the run is already `done`) is logged, never returned:
the gate change is already durable.

| Condition | Status | `code` |
|---|---|---|
| unknown or boundary `kind` from a caller, oversized message or `data`, `data` not one JSON value, unknown body field, bad `since_seq`/`limit` | 400 | `bad_request` |
| reading or posting another principal's run | 403 | `forbidden` |
| unknown run; agent neither held by the runtime nor recorded | 404 | `not_found` |
| posting to a run that has ended (`done` recorded, or a terminal record) | 409 | `signals_closed` |
| a corrupt `signals.jsonl` line or saved outcome | 500 | `runtime_error` |
| no runs service, or no signal book for `POST`; no outcome store for `kick-outcome` | 501 | `not_supported` |

```
crux --server ADDR signal emit <run> --kind K [--message M] [--data JSON] [--json]
crux --server ADDR signal list [--since N] [--limit N] [--all] [--json] <run>
crux --server ADDR gate open … --run RUN
```

**Kick outcome (the RFC's S5 proper).** The runtime's kick-outcome
observer now saves `api.KickOutcome` to
`<workdir>/signals/agents/<name>.json` (0600, tmp+rename, reason
scrubbed) *before* publishing `agent.kick_outcome`: the event is a
notification, the record is the proof. `GET
/v1/agents/{name}/kick-outcome` answers that record and
`settled_for_last_kick = crux.KickOutcome.Settled(last_kick)`, where
`last_kick` is the runtime's delivery time of its latest kick — the same
`Settled` rule the runtime uses, never a loose timestamp comparison. A
driver that sent a kick, missed the event and reconnected reads `true`
once that turn ended; `false` with an outcome means the outcome is of an
earlier kick (its `kickAt` says which). After a `crux serve` restart the
runtime holds no `last_kick`, so the answer is the saved outcome with
`settled_for_last_kick: false`; the driver compares `outcome.kickAt` with
the kick time it holds. Like every `/v1/agents` route it is the
operator's.

**Storage.** `<workdir>/runs/<id>/signals.jsonl`, 0600, append-only, one
`Signal` per line. `signals.Book` is the single writer per process: it
assigns `seq` under its lock, resumes it from the file the first time a
run is touched after a restart, and forgets a run once `done` is written
(the file then says it is closed).

**Deliberately not in S5.** No long-poll or stream route per run (follow
with `since_seq`, or filter `run.signal` on `/v1/events`); no signals
from inside the sandbox other than through the owner's API; no
per-agent signal log beyond the last kick outcome; no retention policy —
a run's signals live and die with the run dir.

## S6. Agent step

One step = one agent backend run inside a sandbox, in a prepared
checkout, with a prompt in and a schema-checked JSON answer out. Package
`src/crux/agentstep` (`Run`, `Host`, `Backend`, `Backends`, `Store`,
`ParseSchema`, `CorrectivePrompt`), API types in `src/crux/api/steps.go`,
routes in `src/crux/serve/steps.go`, the session in
`src/crux/runs/service.go`, CLI in `src/crux/cli/step.go`. No new
configuration: a step runs on the S2 backend and image with the S2
timeouts (`default_timeout_ms` per attempt, `max_timeout_ms` the cap).

**Session.** A step needs a sandbox that already holds the checkout, so
`POST /v1/runs` with `"profile": "agent"` opens a *step session* instead
of running a command: `command` must be empty (400), the source is any
S2 source (checkout, worktree or workspace_path), `network` defaults to
`restricted` because the agent must reach its model (a backend without
that capability answers 501 `unsupported`; a caller may ask `none` for a
process backend that needs nothing), and `env_allowlist` still refuses
credential names — a session carries no repository-write authority, as
the RFC asks. The response is always 202 with the `accepted` record,
`detach` or not: the session has no end of its own. Its runner
provisions the workspace, moves the record to `waiting`, signals
`started` and holds until `POST /v1/runs/{id}/cancel` closes it
(`cancelled`, reason `session closed after N step(s)`, `done` signal).
While a step runs the record is `running`; between steps `waiting`.
`Reconcile` at start marks a `waiting` session `unfinished` like any run
left in flight. One step at a time: a second concurrent post is 409
`step_busy`; a step on a run that is not an open session (an experiment,
a closed or reconciled session, a session whose provisioning failed) is
409 `not_session`. A step posted while the session is still provisioning
waits for it.

| Route | Body | Response |
|---|---|---|
| `POST /v1/runs/{id}/steps` | `StepRequest` | `201 Step` once the step has finished, whatever its status |
| `GET /v1/runs/{id}/steps` | | `200 {"run", "steps":[Step…]}` oldest first; a step in flight shows as `running` |
| `GET /v1/runs/{id}/steps/{n}` | | `200 Step` |

`StepRequest` is `{backend, prompt | prompt_path, output_schema, model?,
tools?, turn_limit?, timeout_ms?, retry_on_mismatch?, command?}`.
`prompt` is inline (≤ 1 MiB); `prompt_path` is relative to the session
workspace and must stay inside it. `output_schema` is a JSON Schema the
answer must match; `tools` is the allowlist of tools the agent may use
(empty: the backend's default, which for `claude` is every tool);
`turn_limit` bounds agent turns per attempt; `retry_on_mismatch` (0–5)
re-asks when the answer misses the schema; `command` is the process
backend's. A `Step` is `{run, n, status, reason?, backend, model?,
created_at, finished_at?, attempts, answer?, usage{input_tokens,
output_tokens}, transcript_path?, violations?}`. `status` is the RFC's
closed set: `succeeded | schema_mismatch | timeout | backend_error |
tool_denied | turn_limit_exceeded | cancelled | unsupported`, plus
`running` while in flight. `answer` is present only on `succeeded`.

**Execution.** The service writes the prompt to
`<workspace>/.crux/steps/<n>/prompt-<attempt>.md` (the same place the
agent sandbox keeps its kick prompt), builds the backend's command line
with the prompt as stdin, records it in the S4 command log (`capture:
runner`, status from the exit), launches it through the S2 launcher as
`crux-step-<run>-<n>-<attempt>` with the session's network and filtered
environment, and reads the answer back. The first attempt sends the
caller's prompt verbatim; every retry appends `CorrectivePrompt`: the
schema, the violations by JSON path, and how many attempts remain. A
non-JSON or missing answer counts as a mismatch (`$: the backend returned
no JSON value`) and is retried the same way. Timeout and cancel tear the
launch down by name, as S2 does. Every finished step writes its record,
appends one `step` signal to the run and leaves a transcript at
`<workdir>/runs/<id>/steps/<n>/transcript.jsonl` (one line per attempt:
command, exit, scrubbed output, status, violations, usage).

**Schema.** `agentstep.ParseSchema` enforces the subset the service can
check without a schema library: `type` (incl. unions), `enum`, `const`,
`properties`, `required`, `additionalProperties` (bool or schema),
`items`, `minItems`/`maxItems`, `minLength`/`maxLength`, `pattern`,
`minimum`/`maximum`/`exclusiveMinimum`/`exclusiveMaximum`, `multipleOf`,
boolean schemas; `title`, `description`, `default`, `examples`, `$schema`,
`$id` and `$comment` are ignored. Any other keyword (`$ref`, `oneOf`,
`format`, …) is refused with 400 rather than silently unenforced, so a
`succeeded` step means the answer was fully checked.

**Backends.** `agentstep.Backend` is `{Name; Command(Spec) ([]string,
error); Parse(Spec, sandbox.Result, readFile) Outcome}`: it turns the
request into a command line the sandbox runs and reads the attempt's
result back. `Command` returning `*UnsupportedError` for a request the
backend cannot honour (tools it cannot restrict, a turn limit it cannot
enforce) records a step with status `unsupported` and answers 501
`unsupported`: the refusal is on the record, as the RFC asks. The RFC
decision's two v1 backends and one test backend ship:

| Backend | Command | `Outcome` from |
|---|---|---|
| `claude` | `claude -p --output-format json [--model M] (--allowedTools a,b \| --dangerously-skip-permissions) [--max-turns N] < prompt` | the result object: `result` holds the answer (JSON extracted from prose or a fence), `usage` the tokens, `num_turns` the turns; `permission_denials` → `tool_denied`; subtype `error_max_turns` → `turn_limit_exceeded`; any other error → `backend_error` |
| `codex` | `codex exec --json --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox [--model M] --output-last-message .crux/steps/<n>/answer-<a>.txt - < prompt` | the last-message file, else the last `agent_message` item in the JSONL; `usage` events sum the tokens. `tools` is refused as unsupported (codex has no allowlist), and so is `turn_limit` above 1 (codex exec is one turn) |
| `process` | `sh -lc '<command…> < prompt'` | stdout as the answer, non-zero exit `backend_error`. Takes `command`; `tools`, `turn_limit` and `model` are refused as unsupported (the command offers whatever it offers). This is the test and escape-hatch backend: any CLI that reads a prompt on stdin |

The claude and codex command lines and result shapes are the adapter
contract this slice fixes; they are exercised against recorded output in
`backend_test.go`, not a live CLI. The RFC's other backends (`gemini`,
`agy`, `cline`) are not registered: adding one is a `Backend`
implementation and a line in `Backends`, with no change to the service,
the routes or the CLI.

| Condition | Status | `code` |
|---|---|---|
| unknown backend, both or neither of `prompt`/`prompt_path`, path outside the workspace, bad or unenforceable `output_schema`, `retry_on_mismatch` outside 0–5, `timeout_ms` above the cap, `command` with a non-process backend, process without `command`; agent profile with `command` | 400 | `bad_request` |
| another principal's run | 403 | `forbidden` |
| unknown run or step | 404 | `not_found` |
| the run is not an open session | 409 | `not_session` |
| the session is running a step | 409 | `step_busy` |
| the backend cannot honour `tools`, `turn_limit` or `model`; a session on a backend without `restricted` | 501 | `unsupported` |
| no runs service | 501 | `not_supported` |

```
crux --server ADDR runs start --profile agent [--network none] --checkout ID
crux --server ADDR step run <run> --backend B (--prompt P | --prompt-file F) --schema FILE
                                  [--model M] [--tool T]... [--turn-limit N] [--retry N]
                                  [--timeout-ms N] [--json] [-- CMD ARGS...]
crux --server ADDR step list [--json] <run>
crux --server ADDR step show [--json] <run> <n>
crux --server ADDR runs cancel <run>
```

`step run` exits 0 only on `succeeded`; any other status prints the
record (with violations) and exits 1, so a driver script can branch on
it.

**Storage.** `<workdir>/runs/<id>/steps/<n>/record.json` (the `Step`,
tmp+rename) and `transcript.jsonl`, 0600; steps are numbered from 1 in
creation order under a process-wide lock. Prompts and backend answer
files live in the workspace under `.crux/steps/<n>/` and go with it.

**Deliberately not in S6.** No `gemini`, `agy` or `cline` backend yet;
no streaming of a step's progress beyond the S5 `step` signal and the S4
log; no step-level idempotency key (post once, read back with `GET`);
no fan-out of one prompt to several backends; no cost accounting beyond
the usage the backend reports; no persistence of the agent's own session
between steps — each step is a fresh backend invocation in the same
checkout, and whatever the step wrote to the workspace is the state the
next one sees.

## Deliberately not in S1

- TTL sweeper (recorded, not enforced).
- `--filter=blob:limit=` partial clones; the cap is post-hoc only (the
  server ceiling bounds what may be asked, not what git transfers).
- Per-owner or callback-minted checkout tokens; one `CRUX_CHECKOUT_TOKEN`
  per server.
- A per-owner allowlist; one list per server.
- `ssh://` and `http://` repositories; `git@` syntax.
- Anything about the hivecommons/crux repository or the publish path:
  the service lives in hive `src/crux` and ships through the existing
  mirror unchanged.

## S7. Saved run results

The RFC's S7 is the promise that a run's outcome outlives the process that
produced it: stable ids at accept, idempotent create, a closed status set,
the saved record readable later, in-flight runs settled to `unfinished`
when `crux serve` restarts, and an explicit, audited restart that refuses
a run whose saved state is still live. #17014 shipped the minimum (ids,
idempotency, `GET /v1/runs/{id}`, reconcile of runs); this section is the
rest, *as implemented*.

### Surface

| Route | Who | Answer |
|---|---|---|
| `GET /v1/runs?status=S&limit=N` | owner sees own runs, operator sees all | `ContainedRunList`, newest first, stdout/stderr stripped; `limit` must be a positive integer (server default 100); `status` must be a known status |
| `POST /v1/runs/{id}/restart` | owner or operator | the new run, `200` once it has finished or `202` with `detach: true` (sessions are always `202`); `409 conflict` while the old run is `accepted`/`running`/`waiting` or when its record predates the saved request; an empty body is a plain restart |
| `GET /v1/runs/{id}/history` | owner or operator | `RunHistory`: every status the record passed through with its time and reason |
| `GET /v1/runs/{id}/steps/{n}` | owner or operator | the RFC's `GET /v1/steps/{id}`: steps are numbered per run (S6), so a step's saved record lives under its run |

CLI: `crux --server ADDR runs list [--status S] [--limit N] [--json]`,
`runs restart [--reason TEXT] [--idempotency-key KEY] [--detach] [--json] <id>`,
`runs history [--json] <id>`.

### Semantics

- **Restart re-runs the saved request.** The record keeps the
  `ContainedRunRequest` it was accepted with (minus `idempotency_key` and
  `detach`); restart validates it again as the *original owner* against
  the *current* configuration, so a removed workspace root, a deleted
  checkout or a shrunk allowlist refuse the restart with the same
  `bad_request` the original would get now. An operator restarting a
  project's run does not widen what the run may do, and the new run is
  owned by the project.
- **Two records, linked both ways.** The new run carries
  `restarted_from`; the old record gains `restarted_by` once the restart
  is accepted. Its status does not change — a restart is a new run, not
  a mutation of the finished one. `reason` is saved on the new record and
  audited.
- **Refused while live.** Any non-terminal status is `ErrLive` → `409`:
  the runner may still be alive, and two drivers must not both resume one
  task. The only ways out of a live state are the run ending, `cancel`,
  or the reconcile at serve restart.
- **Idempotent like create.** `idempotency_key` on the restart request is
  scoped the same way as on create; a replay returns the restart it
  already started, without a second launch.
- **Audited.** Every accepted restart records `run.restart` with the new
  run, `restarted_from`, the old status, the reason and the owner, through
  the same sink as the gate override.
- **History is the transition log.** `runstate` appends to `index.jsonl`
  on create and on every status change, and only then — a payload-only
  update (output, cmdlog pointer, restart back-link) is not a transition.
  `History(id)` is that file filtered by id.
- **Reconcile settles steps too.** When serve restarts, every run that
  goes `unfinished` has its `running` steps marked `unfinished` with the
  same reason, so `GET /v1/runs/{id}/steps/{n}` never shows a step that is
  "running" in a run that is not.

### Not in S7

- Pruning or archival of saved records.
- Restart of a run whose record predates the saved request (shipped
  before this slice): `409 conflict` with a message naming the cause.
- A `GET /v1/steps/{id}` route distinct from the per-run one.

## Package map

| Path | Role |
|---|---|
| `src/crux/checkout/` | `Service`, `Config` (`MaxBytes` ceiling, `Token`), `Record`, typed sentinels (`ErrBadRequest`, `ErrAboveCeiling`, `ErrForbidden`, `ErrNotFound`, `ErrInUse`, `ErrMismatch`, `ErrTooLarge`, `ErrUnsupported`, `ErrGit`); store and index; credential-helper runner |
| `src/crux/kubejob/` | `Options.Network` (`NetworkNone`, `NetworkHost`, `NetworkDefault`), per-Job deny-all `NetworkPolicy` lifecycle |
| `src/crux/runs/config.go` | `job_network` (default `none` for the `job` backend), `job_network_policy` |
| `src/crux/api/checkouts.go` | `PathCheckouts`, `CodeTooLarge`, `CodeUnsupported`, `CodeUpstream`, request/response types, `Client.{Create,List,Get,Delete}Checkout` |
| `src/crux/serve/checkouts.go` | routes, ownership checks, error mapping; registered from `Server.Handler` after the runtime families and marked a work service for S0 |
| `src/crux/serve/principal.go` | S0: `Principal`, `CallerFrom`, work-service gating |
| `src/crux/cli/checkout.go` | `crux checkout add|rm|list`, local and remote |
| `src/crux/gates/` | S3: `Service`, `Record`, `Caller`, `DeriveIdempotencyKey`, typed sentinels (`ErrBadRequest`, `ErrContextTooLarge`, `ErrNotFound`, `ErrInvalidChoice`, `ErrSelfAnswer`, `ErrForbidden`, `ErrClosed`); pending/resolved store and journal |
| `src/crux/api/gates.go` | `PathGates`, `CodeGate*`, `EventGate*`, request/response types, `Client.{Open,List,Get,Answer,Wait}Gate` |
| `src/crux/serve/gates.go` | gate routes, visibility, error mapping, `gate.*` events and override audit |
| `src/crux/cli/gate.go` | `crux gate open|list|answer|wait`, remote only |
| `src/crux/cmdlog/` | S4: `Log`, `Open`, `Read`, `Recorder`, `Opener`, `FileOpener`, `Policy`, `Start`, `End`, sentinels (`ErrNotFound`, `ErrUnknownSeq`, `ErrCorrupt`) |
| `src/crux/api/cmdlog.go` | `RunCmdlogPath`, `RunCmdlogQuery`, `Command*` status and capture constants, `CommandRecord`, `CommandOutput`, `CommandPage`, `Client.RunCmdlog` |
| `src/crux/runs/service.go` | S4 hook: `Service.CmdLog`, `Service.Commands`; `commandStarted`/`commandStatus` around the launcher call |
| `src/crux/serve/runs.go` | `GET /v1/runs/{id}/cmdlog`, ownership check, paging validation |
| `src/crux/cli/cmdlog.go` | `crux cmdlog list|show`, remote only |
| `src/crux/signals/` | S5: `Book` (`Append`, `SetObserver`), `Read`, `Allowed`, `Signal`, `Policy`, `AgentOutcomes` (`Save`, `Load`), `AgentsDir`, sentinels (`ErrBadRequest`, `ErrClosed`, `ErrNotFound`, `ErrCorrupt`) |
| `src/crux/api/signals.go` | `RunSignalsPath`, `RunSignalsQuery`, `AgentKickOutcomePath`, `Signal*` kind and source constants, `CodeSignalsClosed`, `EventRunSignal`, `RunSignal`, `SignalDoneData`, `SignalGateData`, `RunSignalRequest`, `RunSignalPage`, `RunSignalEvent`, `AgentKickOutcome`, `Client.{RunSignals,EmitRunSignal,AgentKickOutcome}` |
| `src/crux/runs/service.go` | S5 hook: `Service.Signals`, `ReadSignals`, `AppendSignal`, `ErrNoSignals`; `started` before the launcher, `done` from every terminal path and from `Reconcile` |
| `src/crux/serve/runs.go` | `GET`/`POST /v1/runs/{id}/signals`, `run.signal` relay |
| `src/crux/serve/gates.go` | `run` on gate open (ownership check) and the gate's turn signals |
| `src/crux/serve/signals.go` | `GET /v1/agents/{name}/kick-outcome`; the kick-outcome observer that saves before it publishes |
| `src/crux/cli/signal.go` | `crux signal emit|list`, remote only |
| `src/crux/agentstep/` | S6: `Run`, `Host`, `Backend`, `Backends`, `Lookup`, `Spec`, `Outcome`, `Store` (`Create`, `Write`, `Get`, `List`), `ParseSchema`, `Schema.Validate`, `CorrectivePrompt`, `RequestError`, `UnsupportedError`, sentinels (`ErrSchema`, `ErrNotFound`, `ErrCorrupt`, `ErrNoAnswer`) |
| `src/crux/api/steps.go` | `RunStepsPath`, `RunStepPath`, `Step*` status and backend constants, `CodeStepBusy`, `CodeNotSession`, `SignalStep`, `SignalStepData`, `StepRequest`, `Step`, `StepUsage`, `SchemaViolation`, `StepList`, `Client.{RunStep,ListSteps,GetStep}` |
| `src/crux/runs/service.go` | S6 session: `profile: agent` validation, `session` runner, `RunStep`, `Steps`, `Step`, `ErrNotSession`, `ErrStepBusy` |
| `src/crux/serve/steps.go` | `POST`/`GET /v1/runs/{id}/steps`, `GET /v1/runs/{id}/steps/{n}`, ownership check, error mapping |
| `src/crux/cli/step.go` | `crux step run|list|show`, remote only |
| `src/crux/api/runs.go` | S7: `RunRestartPath`, `RunHistoryPath`, `RunsListQuery`, `ContainedRun.RestartedFrom/RestartedBy`, `RunRestartRequest`, `ContainedRunList`, `RunTransition`, `RunHistory`, `Client.{ListRuns,RestartRun,RunHistory}` |
| `src/crux/runstate/runstate.go` | S7: `History`; the index is the transition log |
| `src/crux/runs/restart.go` | S7: `Restart`, `List`, `ListFilter`, `History`, `ErrLive`, `ErrNoRequest`, `DefaultListLimit`; `service.go` saves the request on the record and reconciles steps |
| `src/crux/agentstep/store.go` | S7: `Store.Reconcile` marks running steps `unfinished` |
| `src/crux/serve/runs_saved.go` | `GET /v1/runs`, `POST /v1/runs/{id}/restart`, `GET /v1/runs/{id}/history`, `run.restart` audit |
| `src/crux/cli/runs_saved.go` | `crux runs list|restart|history`, remote only |
