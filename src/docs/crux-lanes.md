# Crux durable lanes

Crux owns the lane contract. Projects are peer publishers/subscribers; Hive is
one optional participant, not the broker authority. No repository, issue, pull
request, forge label or forge credential is required. Lane IDs, project IDs,
message IDs, group IDs and schema IDs are plain `^[a-z][a-z0-9_-]*$` identifiers,
at most 64 characters. These lanes are unrelated to trajectory safety lanes.

## Enable in the existing Crux service

Use `crux serve --lanes-config ./lanes.json`, or set `CRUX_LANES_CONFIG`.
This adds routes to the **existing** HTTP/JSON Unix-socket/loopback-TCP server;
it does not start another daemon. Without this option, existing behavior is
unchanged. `/v1/events` remains the lossy, in-memory legacy observer SSE stream;
it is **not** the durable lane log.

Example administrator-owned `lanes.json` (no secrets):

```json
{
  "url": "tls://broker.example.net:4222",
  "credentials_env": "CRUX_NATS_CREDENTIALS_FILE",
  "namespace": "commons",
  "principals": [
    {"project": "atlas", "token_env": "ATLAS_LANE_TOKEN"},
    {"project": "beacon", "token_env": "BEACON_LANE_TOKEN"}
  ],
  "lanes": [{
    "id": "measurements",
    "owner": "atlas",
    "publishers": ["atlas", "beacon"],
    "subscribers": ["atlas", "beacon"],
    "groups": {"summarizers": ["beacon"]},
    "recoverers": {"summarizers": ["beacon"]}
  }]
}
```

Generate distinct project bearer tokens of 32–4,096 bytes in a secret
manager; inject them through the named environment variables.
`CRUX_NATS_CREDENTIALS_FILE` contains the path to a protected NATS credentials
file, not its contents. The existing `CRUX_TOKEN` remains an **agent-control**
credential. It must differ from every project credential.

The configured project token determines the principal, never an envelope,
`X-Project` header, URI or forge identity. Project tokens cannot call existing
agent-control RPCs; the agent-control token and local socket identity do not
grant lane access. Membership is explicit per capability: ownership alone does
not grant publish, event subscription or work claims. Group membership grants
work consumption, not event subscription. No credential is returned in registry
responses. Provenance is optional, untrusted absolute URI metadata; inline URI
credentials are rejected and it must never contain secrets.

## Public HTTP and Go API

All lane routes require a configured project bearer token:

| Route | Capability / result |
|---|---|
| `GET /v1/lanes` | Registry filtered to the principal's memberships |
| `POST /v1/lanes/{lane}/publish` | Publisher; `LanePublished` |
| `GET /v1/lanes/{lane}/events?cursor=0&limit=32` | Event subscriber; `LaneReplay` |
| `POST /v1/lanes/{lane}/groups/{group}/claim` | Group member; `LaneClaim`, or 204 when empty |
| `GET /v1/lanes/{lane}/groups/{group}` | Group member; revision, reusable slot allocation, retained bounds, missing identities, own receipts/retries and recovery history |
| `POST .../groups/{group}/recover` | Explicitly configured group recoverer; revision-fenced expired-gap acceptance |
| `POST .../groups/{group}/renew` | Current lease holder; renewed expiry |
| `POST .../groups/{group}/ack` | Current lease holder; 204 |
| `POST .../groups/{group}/nack` | Current lease holder; 204 and immediate eligibility for redelivery |

Publish request:

```json
{"id":"reading-one","kind":"event","schema":"temperature-v1","payload":"eyJjZWxzaXVzIjoyMX0="}
```

`payload` is a bounded byte array encoded as base64 by JSON; here it contains
`{"celsius":21}`. Use `kind:"work"` for claimable work. `provenance` is optional.
The durable envelope adds authenticated `project`, `lane` and `created_at`.
`id` is the idempotency key within `(lane, project)`. A different kind, schema,
payload or provenance with the same retained key fails with `lane_conflict`.

Ack/nack/renew requests contain only the returned sequence and receipt:

```json
{"sequence":3,"receipt":"<64 hexadecimal characters returned by claim>"}
```

`crux/api.Client` exposes `LaneRegistry`, `PublishLane`, `ReplayLane`,
`ClaimLane`, `RenewLane`, `AckLane`, `NackLane`, `InspectLaneGroup` and
`RecoverLaneGroup`. It uses the existing Crux
address parsing, transport and typed `StatusError`. `ClaimLane` returns
`nil, nil` for 204, not a zero-valued work message.

Forge-free peer example:

```go
atlas, err := api.NewClient(address, os.Getenv("ATLAS_LANE_TOKEN"))
if err != nil { return err }
beacon, err := api.NewClient(address, os.Getenv("BEACON_LANE_TOKEN"))
if err != nil { return err }
_, err = atlas.PublishLane(ctx, "measurements", api.LanePublish{
    ID: "summarize-one", Kind: api.LaneWork,
    Schema: "summary-v1", Payload: []byte(`{"values":[21,22]}`),
})
if err != nil { return err }
work, err := beacon.ClaimLane(ctx, "measurements", "summarizers")
if err != nil { return err }
if work == nil { return nil }
// Perform a registered, authorized operation; make its side effects idempotent.
return beacon.AckLane(ctx, "measurements", "summarizers", api.LaneReceipt{
    Sequence: work.Sequence, Receipt: work.Receipt,
})
```

## Durability, cursors and concurrency

Every accepted publish has a JetStream acknowledgement. Immutable per-lane
file-backed streams retain both events and work; each subscriber stores its
own event cursor. Replay is a bounded pull page, never a dropping channel.
The cursor is the last **examined** stream sequence, including work records
skipped by event replay. Pages examine at most 512 records and return at most
128 events. Save the returned cursor only after processing the events.
Cursor zero explicitly starts at the earliest retained record; a future cursor
or a nonzero cursor with an expired gap fails with `lane_cursor`. Missing
retained messages during a scan also fail rather than silently skipping.

Work groups have independent persistent cursor/lease ledgers in JetStream KV.
KV compare-and-set is the ownership linearization point, not process memory.
Competing clients in a group claim a work record once per live lease; other
groups receive their own delivery. A lost response or service crash leaves a
lease that expires and redelivers. A random receipt fences each assignment and
binds lane, group, authenticated holder, sequence and ownership generation.
Stale or cross-project/group ack, nack and renew fail, including after
reassignment. No broker ACK subject is exposed to a participant.

Stream publication and KV ownership are not claimed to be one transaction.
Publication first reserves a key/payload hash in KV, then publishes with a
generation-specific JetStream message ID and a **subject expected-sequence CAS**. After a crash,
retry verifies the persisted subject before completing the publish. This
prevents concurrent mismatched payloads and repeated retained publications,
including outside the broker's finite duplicate window. A reservation has a
random persisted generation; a resumed request reuses it. The broker ACK
sequence (including **normal** and **duplicate** ACKs) is read back and checked
against the exact envelope, subject and generation, and the active KV
reservation is rechecked before success. A message-ID duplicate is never
trusted as proof of this request's payload: NATS deduplicates before subject
CAS. Generation-specific IDs prevent an expired request from inheriting the
ACK of a different payload published under a reused key.

Reservation and stream writes are separate broker operations, not an atomic
cross-resource transaction. A request fenced by expiry/replacement can fail
even if its own message reached durable storage; unavailable or expired
metadata also fails explicitly. Retry/reconcile rather than treating any
error as proof that nothing was stored. Successful responses always resolve
to matching persisted content. Reservations are
bounded by retention; IDs can be reused once both reservation and log record
have expired. **There is no infinite idempotence guarantee.**

Acknowledgements are idempotent for the configured acknowledgement-retention
period. Completed receipts occupy bounded ledger slots during that period;
once pruned, an old acknowledgement fails as stale. Unacked work retries until
successful ack or log retention expiry; no automatic dead-letter discard is
performed. Expired pending work fails explicitly until authorized gap recovery.
Receipts are bearer secrets scoped to their project/group: do not log them.

### Explicit same-namespace group recovery

An operator configures `recoverers` per group; each recoverer must also be a
group member. Group membership, lane ownership, an agent-control token, Hive
identity and a local socket do **not** implicitly grant this capability.

Read `InspectLaneGroup`, then submit:

```json
{
  "id": "accept-expired-gap-one",
  "revision": 42,
  "cursor_through": 19,
  "expired": [{"sequence": 12, "receipt": "<original ownership receipt>"}],
  "missing": [13]
}
```

The revision must match the entire group state. Concurrent claims, renewal,
ACK or another recovery cause a CAS conflict; refresh the inspection and use
a new request ID if changing the request. `cursor_through` must be exactly the
expired prefix boundary (`first_retained - 1`) when there is a cursor gap, or
the current cursor when there is no gap. It cannot advance across retained
unclaimed work. Each explicitly retired delivery must be missing from retention,
lease-expired and unacknowledged. `expired` additionally requires the caller's
exact original receipt. `missing` lists inspected sequences without needing
another project's secret receipt: the separately configured recovery capability
authorizes expiration of absent owners' deliveries in this group. Neither form
can retire live, retained, completed or foreign-group work. At most 16 combined
deliveries can be accepted per request; inspect and submit successive batches
with refreshed revisions. Duplicate sequences are invalid.

The result durably records the accepted cursor gap, observed `missing_through`
prefix and expired delivery retirements without marking them acknowledged.
Valid pending leases and ACK history remain intact. The result's `expirations`
records the original project, sequence and attempt of every retired delivery;
its `project` identifies the authenticated recoverer. This is an explicit
expiration audit, **not** a successful ACK. Retired entries release their
ledger slots immediately, including when the owner has disconnected forever.
Recovery requires neither an unrelated unread gap nor an own-project receipt.
Original receipts can no longer ACK, NACK or renew these deliveries.
The missing-prefix checkpoint does not silently suppress unlisted pending
deliveries; those require their own explicit, fenced acceptance.

The exact request ID/body and result are persisted together in the same group
CAS update. Retrying a lost response, including after service/broker restart,
returns that result without reapplying recovery. A changed body or another
principal using the ID conflicts. Detailed history and recovery idempotence are
bounded by `ack_retention`, at most 64 recovery records per group; overflow is
explicit backpressure. The accepted missing-prefix checkpoint and cursor are
persistent fixed-size state. No new namespace or daemon is required.

Delivery is **at least once**, not exactly-once side effects or execution.
An operation may finish and crash before ack, or continue after losing its
lease; applications must honor cancellation and use downstream idempotence or
fencing for effects. Renew before expiry. No global FIFO or ordering of work
completion is promised. Stream sequence orders accepted records per lane;
eligible retry selection rotates by a persistent sequence cursor, while a
separate persisted turn alternates retries and fresh work when both exist.
Rotation wraps among ready deliveries, so an always-ready low-sequence poison
item cannot starve already-claimed higher-sequence work after response loss.
When retries exist, the next retry turn is committed before attempting fresh
value growth: even a failed shared-bucket admission cannot pin the group
forever to unsuccessful fresh attempts.
NACKs schedule durable
exponential retry delay capped by `max_retry_delay`; duplicate NACKs do not move
the deadline. Polling slower than the retry delay cannot let one rejected item
starve fresh work. Released/expired retries do not consume live-lease slots,
but remain in the bounded ledger and are never silently ACKed or discarded.

## Bounds and failure behavior

Configuration uses integer nanoseconds for duration fields; omitted or zero
values select defaults. Negative/invalid explicit values fail initialization.
Short retention remains supported even when an operation timeout is longer:
expiry produces explicit generation/cursor errors, not a false duplicate ACK.
For normal deployments choose retention comfortably above polling, retry and
operation-deadline requirements.

| Setting | Default / accepted bounds |
|---|---|
| `retention` | 7 days; 1 second–30 days |
| `dedup` | 2 minutes; 100 milliseconds–retention |
| `lease` | 1 minute; 1 second–1 hour |
| `ack_retention` | 1 hour; lease–retention |
| `retry_delay` | 1 second; 100 milliseconds–`max_retry_delay` |
| `max_retry_delay` | 1 minute; retry delay–1 hour |
| `timeout` | 5 seconds; positive–1 minute |
| `max_payload` | 64 KiB; positive–1 MiB |
| `max_messages` | 10,000 per lane; 1–1,000,000 |
| `max_bytes` | 64 MiB per lane; payload size–1 GiB |
| `max_kv_bytes` | 64 MiB per bucket; positive–1 GiB |
| `max_pending` | 128 live leases per group; positive and at most ledger size |
| `max_ledger` | 1,024 per group; pending size–10,000, additionally limited by worst-case reservations within 512 KiB |
| `replicas` | 1; 1–5 subject to broker topology |

Registry configuration permits at most 64 lanes, 64 principals, 64 groups per
lane and 64 members per capability/group. The serialized registry is bounded
to 512 KiB and its input file to 1 MiB. Provenance is at most 2,048 bytes.
Streams use `DiscardNew`: exceeding capacity rejects new publications; it
does not evict accepted records before age retention. All request operations
have deadlines. Reconnect buffering is disabled: disconnection returns an
error, never a success-shaped queued publication. Reconnection restores
access to persisted state; retry with the same idempotency key.

HTTP errors distinguish invalid requests (400), missing credentials (401),
denied capabilities (403), state/cursor/stale-receipt conflicts (409), group
capacity (429), unavailable broker (503), and deadlines (504).
Broker-side capacity/storage failures are unavailable errors, not accepted
publications. A successful duplicate is returned only after verifying a
durably persisted matching record.

Claim admission reserves the serialized maximum of all fields of every
delivery (including maximum-width attempts, IDs and UTC nanosecond timestamps)
plus all 64 possible recovery responses of 16 deliveries each, gap metadata
and request hashes. Space padding allocates this reservation in the actual
namespace KV value. ACK, NACK and renew do not grow its reserved size; recovery
uses already-reserved history and removes expired delivery entries. Near the
512 KiB byte limit, new claims fail before consuming completion headroom.
The effective delivery count can therefore be below `max_ledger`, even with
short actual IDs. History is pruned only after its documented retention window.
An exhausted recovery-history window applies finite backpressure until expiry.

Each persisted group has a durable, nonshrinking slot high-water mark.
An initial empty poll reserves **at least one reusable delivery slot**, not
just cursor/history bytes; its later first claim therefore needs no value
growth. Pruning completed records and retiring missing foreign deliveries
free entries **within** those slots without reducing their physical reservation.
Previously allocated slots remain reusable after service/broker restart and
response loss. Inspection reports `slots` (zero for an uninitialized group).
Slot allocation cannot exceed `max_ledger` or the 512 KiB worst-case bound.
Additional slots require successful broker admission before a new lease is
accepted; failed expansion does not remove existing reusable slots.

The same physical reservation prevents newly admitted groups from consuming
existing groups' value-growth headroom in the shared bucket. Padding also
compensates byte-for-byte for the pinned driver's decimal CAS-header growth.
Bucket exhaustion
rejects admission explicitly. Broker metadata, account limits, replication,
network and disk availability can still fail any operation; reservations are
not a guarantee of infinite broker capacity or availability. The internal
state-format generation 3 rejects incompatible older prototype writers.

Stream names use a bounded digest of the namespace/lane tuple, not ambiguous
underscore concatenation. Subject, KV and group-key boundaries use validated
identifiers and disjoint fixed delimiters/suffixes.

The registry and delivery bounds form an immutable namespace generation.
Startup rejects incompatible existing streams, KV buckets or registry; it
does not rewrite another service's permissions. Membership/admin mutations
are **not supported over the project API** (405/404); explicit group recovery
is a separate configured capability, and ownership is not a
remote administrator grant. Operators provision reviewed generations through
configuration, coordinating migration/replay before retiring the old one.

## Hive adapter

Set `HIVE_LANES_ADDRESS`, `HIVE_LANES_TOKEN`, `HIVE_LANES_LANE` and
`HIVE_LANES_GROUP` to opt Hive into an ordinary Crux project membership.
`HIVE_LANES_SNAPSHOT_PROJECTS` is an optional comma-separated explicit publisher
allowlist for `hive-governor-snapshot-request-v1` work. Without it no snapshot
handler is registered. Requests must contain `{}`; they can only read governor
state and publish a correlated `hive-governor-snapshot-v1` event, not invoke
tools, prompts, forge writes or merges. At-least-once retries may emit multiple
correlated snapshots if state changes between attempts.

Committed governor mode changes are additionally published with schema
`hive-governor-mode-change-v1`; existing local hooks remain attached. A broker
failure logs that durable delivery of the transition was **not confirmed**; this optional
adapter does not pretend the governor state commit and remote publish are an
atomic outbox. The public `pkg/laneparticipant` adapter accepts only explicit
schema-and-publisher handler registrations and bounds handlers by their lease.
Unhandled/unauthorized/failed work is nacked, not executed or acknowledged.
Hive's existing trust and forge-write policies remain unchanged; any future
write handler must independently apply them rather than treating lane
membership as write authority.

## Deployment and verification

No NATS deployment is installed by this feature. Provision JetStream with
durable storage and backups yourself; use TLS with certificate validation,
NATS account authentication and subject/API ACLs, plus a TLS proxy for remote
Crux HTTP access (the existing listener allows only loopback plain HTTP).
Only Crux service credentials may access this namespace's streams/KV/API.
Participants use Crux HTTP credentials, not direct broker credentials; giving
them direct KV access would bypass fencing and registry policy.
Protect credentials and private Unix sockets with OS ownership/modes.
Configure NATS `max_payload` above the base64-expanded envelope size and
512 KiB ledger ceiling, and provision sufficient account stream/byte limits.
Use synchronized service clocks for lease/expiry decisions; KV CAS remains
the assignment fence. Multi-replica deployments require matching configuration
and supported JetStream topology; single-replica defaults are not an HA claim.

The named CI integration step tests real JetStream lanes, API and serving in
the standalone Crux module and the Hive participant through the root module.
CI's existing two-module affected-package test matrix also discovers the new
`crux/lanes` and `pkg/laneparticipant` packages, with the normal 90% coverage
gate, and builds both the Hive host and standalone Crux module. Tests start
real bounded embedded JetStream servers in isolated project-relative fixtures
and stop/remove them. They exercise forge-free two-project HTTP exchange,
fan-out, duplicate/mismatched publish, independent/competing groups,
ack/renew/nack, expiry and stale fencing, persisted restart and pending-publish
recovery, broker outage/reconnect, retention/cursors, bounds, cancellation,
auth isolation and the Hive adapter. Independent child Crux processes exercise
delayed publication across reservation expiry and durable retry/fresh fairness.
Recovery regressions include lost responses/restart, concurrent claim/ACK CAS,
live lease and ACK-history preservation, absent foreign-owner expiration with
sole-slot/no-gap recovery, accepted missing prefixes and API
capability isolation. Byte-bound regressions admit a state within one delivery
of 512 KiB, exercise ACK/NACK/renew and retry, recover retained-history gaps,
and resume later work without pruning unexpired ACK history. Maximum-shape
and full-namespace tests cover history, timestamps and CAS-header headroom.
Reusable-slot tests poll five empty groups at a 1 MiB bucket cap before any
publication, then claim through each group; they also exercise pruning and
foreign retirement at that same replacement-growth boundary, with durable
high-water reuse, broker restart and lost-response fencing.
Existing API/SSE tests remain in their
normal CI selection. Repository policy prohibits local Go test/build/vet;
the executable verdict is CI, not a fake-store-only claim.
