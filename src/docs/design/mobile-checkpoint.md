# Mobile checkpoint payload

Mobile, chat, push, and PWA checkpoint prompts must render one server-owned
payload instead of rebuilding run state per surface. The minimum safe review
object is `RunCheckpointPayload`, served by `GET /api/runs/{key}/checkpoint`.

The payload includes:

- canonical run key;
- held stage;
- lease generation;
- repository;
- title;
- plain-text summary capped by `RunCheckpointSummaryMaxBytes`;
- approve/reject decision options;
- dashboard deep link for the full artifact;
- staleness fence (`lease_gen` plus the generation value);
- approver rule (`owner`, with verified owner role required).

Decision submissions from compact surfaces use `POST /api/runs/{key}/checkpoint`
with `{"action":"approve"|"reject","gen":<lease generation>}`. Hive refuses
non-owner callers before applying a decision, and it refuses stale payloads when
the submitted generation no longer matches the live held lease generation. The
generation fence is the same lease generation already used by the run checkpoint
hold, so no new store or identity mechanism is introduced.

The summary is intentionally lossy: phones get enough context to decide whether
to open the full dashboard artifact, not an unbounded copy of the artifact.
Every surface that needs a notification body calls the same payload builder (or
the endpoint) so chat and push prompts are byte-for-byte derived from the same
contract.
