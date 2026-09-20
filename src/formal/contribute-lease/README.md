# Formal model: contributor lease/reconnect protocol

A Spin (Promela) model of the contributor task lease and websocket reconnect
protocol. It models two relays, one hub, and one issue across assignment,
abnormal socket drop, disconnect release, reconnect+resume, lease expiry, and
reassignment.

Run everything:

```console
$ bash run.sh
```

`run.sh` mirrors `src/formal/escalation/run.sh`: every row has an expected
verdict. Must-pass rows must pass; pre-fix witnesses must fail. Any drift exits
nonzero.

## Model ↔ code mapping

| Promela | Production code |
|---|---|
| `grant(i)` | `selectTask` in `contribute_select.go` commits `currentTask`, mints a task generation, records assignment time, and calls `recordLeaseForKey` |
| `lease[i]`, `leaseExp[i]` | `taskLease` in `contribute_leases.go`; `leaseTTL = wsTaskTimeout` |
| `token[i]` | scoped GitHub credential delivered for a task assignment or resume (`deliverTaskCredential`, `resumeTaskToken`) |
| `dropped0` / `releasePending0` | websocket read failure in `HandleWS` followed by deferred `releaseOnDisconnect` |
| `abandon_disconnect()` | `releaseOnDisconnect` booking `bookReleaseCooldownKey` and `appendAbandonedRun(..., abandonCauseDisconnect, ...)` |
| `live[i]` | a live `ContributorConnection` whose `currentTask` is the issue; this is what `activeIssues` sees |
| `live[C0]` re-adoption check | `taskReadoptedByLiveConnection` (#5337/#5322) |
| reconnect transition | relay `auth_ok` path in `bin/contributor-relay.js`: `task_accepted` + `task_progress`, then hub `lookupLease`, `renewLease`, and `clearReleaseCooldownKey` |
| contributor 1 selection | `selectTask` active issue, cooldown, and lease-hold exclusion gates |

## Constants and time scaling

One model tick is one production minute for hub lease/cooldown timers. The
relay's first reconnect delay is modeled as one ordered tick because the
assertion is about ordering, not wall-clock magnitude.

| Model | Production constant |
|---|---|
| `RELEASE_COOLDOWN = 10` | `failedTaskCooldownMinutes = 10` |
| `LEASE_TTL = 30` | `wsTaskTimeout = 30 * time.Minute`; `leaseTTL = wsTaskTimeout` |
| `RECONNECT_BACKOFF = 1` | `BASE_RECONNECT_DELAY_MS = 1000` |
| `RELEASE_GRACE = 2` | intended #7838 design: abnormal disconnect release waits longer than the first reconnect attempt and re-runs the re-adoption guard |

## Properties

| ID | Run | Expected | Assertion |
|---|---|---|---|
| P1 | `p1_no_double_lease` | pass | No two contributors hold valid credentials backed by unexpired leases for the same issue. This encodes the #7781 intended fix for #7773: an unexpired lease is an in-flight exclusion for every other identity. |
| P2 | `p2_backoff_no_abandon` | pass | A relay reconnecting within its first backoff window is never booked `abandoned_disconnect`. This encodes the intended #7838 grace: release is delayed, then the existing re-adoption guard is re-run. |
| W7773 | `w_7773_cooldown_lt_lease` | fail | Pre-fix witness: after a drop, the 10-minute release cooldown lapses while the 30-minute lease is still resumable, so contributor 1 can be assigned the same issue. |
| W7838 | `w_7838_instant_release` | fail | Pre-fix witness: the hub books `abandoned_disconnect` immediately on read-fail, before the relay's first reconnect attempt at `BASE_RECONNECT_DELAY_MS`. |

## Abstractions

- One issue and two contributor identities. The safety properties are per work
  item; extra issues only add independent copies of the same state.
- Generations are abstracted into lease possession. A resume succeeds only while
  the server-issued lease is unexpired, matching `lookupLease`'s generation and
  task checks.
- Token TTL is not separately modeled. The unsafe state is two contributors with
  task credentials backed by live server ownership for the same issue; the real
  token TTL (55 minutes) outlasts the modeled lease race.
- The reconnect pass run forces the relay's first reconnect attempt before time
  can advance past its due tick. That captures "within the backoff window" and
  avoids turning the property into a scheduler-starvation claim.
- Clean closes, explicit handback, completion, failure, and operator requeue are
  outside this model; they intentionally release/revoke ownership and do not use
  the abnormal-disconnect grace.

## Replaying witnesses

The expected-fail rows leave a trail in the temporary `pan` directory while the
run is active. To reproduce manually:

```console
$ spin -a -DBUG_COOLDOWN_LEASE contribute-lease.pml && gcc -O2 -DCOLLAPSE -DSAFETY -DNOCLAIM -o pan pan.c
$ ./pan -m500000
$ spin -t -p -g -DBUG_COOLDOWN_LEASE contribute-lease.pml

$ spin -a -DMON_RECONNECT -DBUG_INSTANT_RELEASE contribute-lease.pml && gcc -O2 -DCOLLAPSE -DSAFETY -DNOCLAIM -o pan pan.c
$ ./pan -m500000
$ spin -t -p -g -DMON_RECONNECT -DBUG_INSTANT_RELEASE contribute-lease.pml
```
