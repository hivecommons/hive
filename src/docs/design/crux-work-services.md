# Crux work services: S1 pinned checkout (hivecommons/crux#114)

Status: **S0 caller principals and S1 pinned checkout shipped in hive
`src/crux` (v5).** S2 contained run and S7 minimum are in flight in a
sibling change; everything else in the RFC is unscheduled. This records
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
answer turns `DELETE` into 409 `conflict`. Delete is otherwise
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

## Deliberately not in S1

- TTL sweeper (recorded, not enforced).
- `--filter=blob:limit=` partial clones; the cap is post-hoc only.
- A per-owner allowlist; one list per server.
- Wiring `InUse` to live runs: that is S2's side of the seam.
- `ssh://` and `http://` repositories; `git@` syntax.
- Anything about the hivecommons/crux repository or the publish path:
  the service lives in hive `src/crux` and ships through the existing
  mirror unchanged.

## Package map

| Path | Role |
|---|---|
| `src/crux/checkout/` | `Service`, `Config`, `Record`, typed sentinels (`ErrBadRequest`, `ErrForbidden`, `ErrNotFound`, `ErrInUse`, `ErrMismatch`, `ErrTooLarge`, `ErrUnsupported`, `ErrGit`); store and index |
| `src/crux/api/checkouts.go` | `PathCheckouts`, `CodeTooLarge`, `CodeUnsupported`, `CodeUpstream`, request/response types, `Client.{Create,List,Get,Delete}Checkout` |
| `src/crux/serve/checkouts.go` | routes, ownership checks, error mapping; registered from `Server.Handler` after the runtime families and marked a work service for S0 |
| `src/crux/serve/principal.go` | S0: `Principal`, `CallerFrom`, work-service gating |
| `src/crux/cli/checkout.go` | `crux checkout add|rm|list`, local and remote |
