# Flue external-execution fixture

A runnable walk-through of the report-only Flue binding pilot
([#8361](https://github.com/hivecommons/hive/issues/8361); design record in
[`src/docs/design/external-workflow-admission.md`](../../src/docs/design/external-workflow-admission.md)).

The fixture at `src/pkg/extwork/flue/testdata/flue-fixture/` is a deterministic
stand-in for a Flue runtime. It runs as a second local process, speaks the
subset of Flue's native surface the adapter relies on (keyed admission with
`submission_conflict` on a changed payload, a runtime incarnation uid, abort,
per-submission artifacts, resume from its own state file), and executes a
three-stage workflow: `analyze` reads the pinned source bundle, `instrument`
attempts one outbound POST and one GitHub write, `report` emits `report.md`,
`patch.diff`, and the stage receipt. Stages advance only when ticked, so every
run is reproducible. It needs no Node, no network, no model, and no GitHub
token.

## Run it

```sh
examples/flue/run.sh
```

The script starts the fixture with `go run ./cmd/flue-fixture`, walks the
native surface with `curl` (info, keyed dispatch twice to show deduplication,
a conflicting payload, three ticks, the receipt artifact, the stats), then runs
the conformance suite, which drives a fresh fixture process through the real
adapter and binding.

To hold the fixture open for manual probing:

```sh
cd src
go run ./cmd/flue-fixture -workflow pkg/extwork/flue/testdata/flue-fixture -state /tmp/flue-state -incarnation dev
# prints: FLUE_FIXTURE_ADDR http://127.0.0.1:<port>
```

Flags: `-state` makes a restart resume (the same `-incarnation` continues; a
different one is a recreated engine with empty state), `-ignore-abort` makes the
workload acknowledge but ignore abort requests, and `-egress-proxy` is the only
route stage effects may take (without one they are recorded as `no_route` and
no connection is opened).

## What the conformance suite proves

`src/pkg/extwork/flue/conformance_test.go` runs the #8201 Gate 1 rows against
the fixture process: admitted task and replay, held task starts nothing,
missing capability or version refused, same key deduplicates and a changed
payload conflicts, a recreated engine is never adopted, the three crash
windows recover without a duplicate dispatch, an ignored cancel never yields a
false stopped state, an outage is unknown until the engine's own state wins,
artifact refusals (missing, truncated, malicious path, wrong digest,
oversized, unavailable store), verifier rejection with the execution fact
retained, store write failure, shadow and off never start, and the side-effect
inventory (both class (a) effects refused at the egress boundary, every class
(b) artifact listed in the receipt).

`src/pkg/extwork/guard_test.go` proves the adapter is not linked into the
`hive` binary unless it is built with `-tags extwork_flue`.
