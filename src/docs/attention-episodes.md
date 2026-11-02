# Condition episodes and attention admission

The opt-in condition delivery primitive for RFC #16075 / #16267 reuses the
writeback gate and `hive:status`. It does **not** migrate existing note producers,
change quiet mode, or enable the issue-side pilot (#16270). The authoritative
status writer (#16263) is a prerequisite to enabling concurrent producer routing.
No review, machine command, security hold or merge hold is changed. An
acknowledgement is not approval.

## Configuration

```yaml
github:
  attention:
    admitter:
      hivecommons/hive: hosted-hive-id
    flap_window: 12h
```

Keys must be fully qualified repositories. Exactly one deployment may own the
configured hive ID for a repo. Other hive IDs record **would notify** and update
status only; they cannot create attention events. This is explicit single-owner
admission, **not** a distributed lock or atomic GitHub list-before-create scheme.
Do not clone an admitter's identity into another active deployment. During
handoff, stop the old admitter before starting the new one, preserve its ledger,
and reconcile its uncertain attempts before changing the configured owner.
Unknown-owner uncertain attempts remain visible and are not automatically retried.

Unset/empty `admitter` means this new path does nothing, without opening a ledger
or changing today's quiet-mode delivery. The default flap window is 12 hours.
There is no reminder or per-recipient budget in this layer (#16270 / #16271).

## Producer contract

`github.Client.ReportAttention` takes the full current condition inventory for
one item, a writeback item/kind, and structured observations. All reporters must
use the same action class, responsible recipient and subject, independent of
producer. Do not derive identities from comment prose:

- CI: `attention.CISubject(requiredJobName, normalizedErrorClass)`. The caller
  classifies errors; run IDs and head SHAs belong in evidence only.
- Sentinel/security: `attention.FindingSubject(findingidentity.Record)` reuses the
  existing subject/predicate/location identity, without head SHA.
- Holds/decisions: `attention.DecisionSubject(requiredDecisionID)`.

A materially different risk, action or owner changes the key. Consequently a
new urgent finding does not share a previous finding's delivery budget. The
attention body is supplied by the producer (the action contract belongs to
#16270); this API must not be used for arbitrary untrusted/protocol text.

Repeated reports update `last_seen` and evidence, not the episode or delivery.
Explicit resolved observations close episodes; absence from an inventory is not
proof of recovery. Within the flap window, recovery/reappearance reopens quietly.
After that window a recorded recovery followed by recurrence starts a new episode.
`Store.Update` records acknowledgement, snooze or resolution; it has no access to
holds, permissions or reviews.

## Recovery and delivery

The ledger lives beside the writeback state (`writeback.json.attention`, following
`HIVE_WRITEBACK_STATE_PATH`), uses a versioned JSON schema and fsync/atomic rename,
and is shared by clients using the process-wide writeback gate. Save failures
fence POSTs. Unknown schema versions fail closed instead of being overwritten.

On missing/corrupt state, the first **complete inventory per item** is a recovery
baseline: list trusted Hive attention/status markers and seed current conditions
without sending. A successful empty inventory establishes a baseline too. Do not
send partial producer snapshots during recovery: aggregate the item's known
conditions before calling this API. Conditions seeded from history are marked
uncertain for human reconciliation in the status section. A subsequent genuinely
new condition can notify immediately. This deliberately chooses no historical
replay over guessing which unrecorded old incident deserves another page.

Before POST, persist a random attempt ID and its owner. Comments carry
`<!-- hive:attention key=<hash> attempt=<id> -->`. After any ambiguous failure or
restart with an unfinished attempt, paginate comments and look for that exact
trusted marker before retrying. A lookup failure never permits a retry; a found
attempt becomes notified without another POST. An absent attempt retries with
the **same** ID, even if evidence/body changed. This reconciliation is only for
the single admitter's own attempts, not cross-instance deduplication. The existing
mutation journal remains in the GitHub write path as an additional safeguard.

The file is bounded to 4096 episodes and 4096 initialized items. Capacity errors
fail closed and retain history instead of evicting an active incident and
replaying it. Operators must reconcile/archive old resolved inventory before
resetting a full ledger; a reset re-enters baseline recovery, never an automatic
re-page. Status displays up to 20 active conditions with an overflow count; all
records remain in the ledger. Status includes owner/action, last evidence,
acknowledged/snoozed state and uncertain/would-notify indicators, with copied
mentions neutralized. This is a queue snapshot, not an unbounded transcript.
