# Crux work services: S1 pinned checkout (hivecommons/crux#114)

Status: **S0 caller principals and S1 pinned checkout shipped in hive
`src/crux` (v5).** S2 contained run and S7 minimum shipped in #17014; S3 human gate is [below](#s3-human-gate); everything else in the RFC is unscheduled. This records
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
per-package specifics. Private repositories therefore work only through
whatever the server's own git credential helper provides; the API never
accepts a credential.

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
sizes. Over `max_bytes` (default 5 GiB) it removes the worktree and
answers 413 `too_large` with both numbers; if the measurement itself
fails it answers 501 `unsupported`. The bytes were already transferred
and the shared clone is kept (a later request for the same repo reuses
it). `--filter=blob:limit=` partial clones would make the clone itself
smaller but would also make the worktree need network at checkout time;
that trade is not taken here and is deferred. The record stores both
`max_bytes` and the measured `bytes`.

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

## Deliberately not in S1

- TTL sweeper (recorded, not enforced).
- `--filter=blob:limit=` partial clones; the cap is post-hoc only.
- A per-owner allowlist; one list per server.
- `ssh://` and `http://` repositories; `git@` syntax.
- Anything about the hivecommons/crux repository or the publish path:
  the service lives in hive `src/crux` and ships through the existing
  mirror unchanged.

## Deferred runs routes

The runs routes (`POST /v1/runs`, `GET /v1/runs/{id}`,
`POST /v1/runs/{id}/cancel`) shipped in #17014. Restart and listing routes
for runs are deferred and not scheduled.

## Package map

| Path | Role |
|---|---|
| `src/crux/checkout/` | `Service`, `Config`, `Record`, typed sentinels (`ErrBadRequest`, `ErrForbidden`, `ErrNotFound`, `ErrInUse`, `ErrMismatch`, `ErrTooLarge`, `ErrUnsupported`, `ErrGit`); store and index |
| `src/crux/api/checkouts.go` | `PathCheckouts`, `CodeTooLarge`, `CodeUnsupported`, `CodeUpstream`, request/response types, `Client.{Create,List,Get,Delete}Checkout` |
| `src/crux/serve/checkouts.go` | routes, ownership checks, error mapping; registered from `Server.Handler` after the runtime families and marked a work service for S0 |
| `src/crux/serve/principal.go` | S0: `Principal`, `CallerFrom`, work-service gating |
| `src/crux/cli/checkout.go` | `crux checkout add|rm|list`, local and remote |
| `src/crux/gates/` | S3: `Service`, `Record`, `Caller`, `DeriveIdempotencyKey`, typed sentinels (`ErrBadRequest`, `ErrContextTooLarge`, `ErrNotFound`, `ErrInvalidChoice`, `ErrSelfAnswer`, `ErrForbidden`, `ErrClosed`); pending/resolved store and journal |
| `src/crux/api/gates.go` | `PathGates`, `CodeGate*`, `EventGate*`, request/response types, `Client.{Open,List,Get,Answer,Wait}Gate` |
| `src/crux/serve/gates.go` | gate routes, visibility, error mapping, `gate.*` events and override audit |
| `src/crux/cli/gate.go` | `crux gate open|list|answer|wait`, remote only |
