# ADR-0024: Provider credential lifecycle

Status: Proposed

Authored by the hive project. Reviewer: @hanthor.

**Read this as a proposal, not a setup guide.** The classification vocabulary
and future pool constraints are proposed agreement. The v5 deliverable described
below is read-only provider-status reporting, not a refresh service; v5 has no
credential pool, idle warmer or refresh knob. Present-tense clauses do not
change this record's **Proposed** status.

**Terms:** a **grant** is the provider's authorization for a consumer.
A **refresh authority** may renew access; a **pool** would select among
explicitly shared provider references; a **broker** would mediate their use.
None of these words implies that Hive can copy rotating credentials safely.

## Context

RFC [#12611](https://github.com/hivecommons/hive/issues/12611) proposes shared
provider auth pools across hives and contributors, plus an idle
"provider-warmer" that refreshes credentials while they are unused. The
motivation is real: an access token can age out while its provider is idle,
and re-logging in agent by agent across several hives does not scale.

The history argues for caution:

- #5165: a watchdog read a stale Claude access token as a fleet-wide logout,
  although the refresh grant behind it was still good. The fix
  (`claude.HasUsableToken`) separates "refreshable" from "needs login".
- #7922: copying a rotating OAuth credential into a contributor container
  let one refresh invalidate the host's copy.
- #4596: per-agent UIDs and owner-only credential rewrites made shared homes
  look logged out. A file being present does not prove that its consumer can
  read it.

Today's per-agent `backendAuthStatus`
(`unlicensed|token-expired|forbidden|unreachable|quota`) mixes two questions:
can this credential authenticate, and is the provider serving this account?
An operator cannot tell "log in again" from "wait for the provider".

## Decision

**(a) Hive is not a refresh authority for any provider in v5.** It will not
become one for a provider until that provider documents a supported
server-side (non-interactive) refresh flow for shared consumers.

- Claude: refresh stays with Claude Code, which redeems the refresh grant when
  the process starts. Hive only *reads* whether that will work
  (`claude.HasUsableToken` semantics: a live access token, or an expired one
  whose refresh grant is still valid) and prescribes a restart instead of a
  login when it will. Hive will not refresh through the public OAuth client
  for operators. That is deferred until Anthropic documents it as supported.
  This ADR leaves that option open; it does not rule it out.
- Codex, Kiro, Google and Muse: we have verified no supported idle-refresh
  path. Each consumer keeps an independent grant.
- Hive does not run an idle warmer, leases or a broker, does not deliver
  credentials to contributors, does not share pool membership across hives,
  and does not invoke any CLI to keep credentials warm.

**(b) Pools are opt-in.** A credential pool is a *provider-specific
reference*, never a shared file. Membership must be explicit. Sharing an
organisation, registering as a contributor or using the same provider name
never grants access, and credentials in different provider or CLI formats are
not assumed to be interchangeable. v5 ships no pool implementation. This
constrains any future one.

**(c) Auth state and availability state are two dimensions.** They use the
RFC's §4 vocabulary and are mapped onto `backendAuthStatus` rather than
replacing it:

| Dimension | States |
|---|---|
| Authentication (`auth_state`) | `usable`, `refreshable`, `needs-login`, `store-inaccessible`, `unknown` |
| Availability (`availability_state`) | `available`, `rate-limited`, `quota-exhausted`, `unavailable`, `unknown` |

| `backendAuthStatus` | `auth_state` | `availability_state` |
|---|---|---|
| `ok` / empty | `usable` | `available` |
| `token-expired` + refreshable | `refreshable` | no evidence |
| `token-expired` otherwise | `needs-login` | no evidence |
| `unlicensed` | `needs-login` | no evidence |
| `forbidden` | `unknown` | no evidence |
| `unreachable` | no evidence | `unavailable` |
| `quota` | `usable` | `quota-exhausted` |

A dimension with no evidence is `unknown`, never shown as healthy. An expired
access token alone is never a logout. Today no evidence maps to
`store-inaccessible` or `rate-limited`: they are reserved, and rate limits
remain transient.

The v5 deliverable is read-only. Each agent's backend-auth evidence gains
`backendAuthRefreshable`. The spoke rolls its enabled agents up into a
redacted `providers[]` block (`provider`, `auth_state`, `availability_state`,
`since`, `evidence`, `next_action`, `affected_agents`), serves it on
`/api/status`, and sends it in the hub heartbeat. `/fleet` and the spoke
header show it. `providers.status.enabled` (default `true`) turns it off.
There is no refresh knob.

### Convergence answers (RFC #12611)

1. **Service ownership:** no existing upstream auth service exists. Each hive runs standalone first,
   and the hub aggregates.
2. **Refresh boundary:** Claude can only be refreshed on restart. Every other provider needs an
   independent grant.
3. **Status transport:** the contract is carried by the hub heartbeat's `providers[]` and by
   `/api/status`.
4. **Classification before automation:** this ADR proposes that agreement before any
   backend-specific keepalive loop is added.

## Consequences

- Operators can tell "log in", "restart" and "wait for the provider" apart,
  per provider and per hive, with no credential material in status, heartbeat
  or UI.
- Agents never race a CLI that refreshes itself, and credential copies cannot
  fork. In exchange, idle credentials can still expire. A non-refreshable
  Claude grant or any other provider's expiry still needs one human login,
  which the status now names.
- A provider documenting a supported shared refresh flow is a precondition,
  not a trigger. Adopting it needs a new ADR that supersedes clause (a) for
  that provider. Pools and warming remain a v6 design discussion on #12611.
