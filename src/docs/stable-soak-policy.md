# v4 stable soak and promotion policy

This proposal closes the policy gap where every successful `v4` merge retags both
`candidate` and `stable`. Until the CI promotion step is implemented, operators
should treat `stable` as a compatibility tag, not as evidence that the build has
soaked longer than `candidate`.

## Goals

- Keep `candidate` fast: it should move on every green `v4` release build.
- Make `stable` deliberate: it should advance only after observable soak, or by a
  documented emergency exception.
- Preserve rollback safety with immutable short-SHA tags and digest evidence.
- Give operators clear expectations for bursty release days.

## Proposed promotion rule

A `v4` build may be promoted from `candidate` to `stable` only when all of these
conditions hold:

1. **Minimum soak:** the candidate digest has been the newest candidate for at
   least 24 hours.
2. **No superseding candidate:** if a newer `candidate` appears before the soak
   window ends, the timer restarts on the newer digest.
3. **Green release evidence:** build, lint, unit tests, changelog/release guards,
   and non-flaky required checks are passing or skipped by policy.
4. **No open blocker:** no open issue or PR label explicitly marks the candidate
   digest, release tag, or included fix set as `blocker`, `regression`, or
   `security` hold for the v4 line.
5. **Operator smoke signal:** at least one maintained hive has reported a healthy
   heartbeat on the candidate digest, or the release captain records why a
   dashboard/heartbeat smoke is not applicable for that digest.

The release captain records the promoted digest, candidate tag, stable tag, soak
start/end time, and smoke evidence in the promotion PR or workflow summary.

## Emergency promotion exception

Security and data-loss fixes may promote faster than 24 hours when the maintainer
who owns the release writes an explicit exception note naming:

- the risk of waiting;
- the checks that did pass;
- the rollback digest; and
- the follow-up issue for any skipped soak evidence.

Emergency promotion should still publish `candidate` first. The exception only
shortens the candidate-to-stable delay; it does not bypass builds or tests.

## CI shape

The existing release build continues to publish immutable short-SHA tags and move
`candidate`. A separate scheduled or manually dispatched promotion workflow should
then:

1. resolve the current `candidate` digest;
2. compare it with the current `stable` digest;
3. read the candidate's first-seen time from workflow artifacts, tag metadata, or
   a checked-in promotion ledger;
4. evaluate the gate above; and
5. retag `stable` to the candidate digest only when the gate passes.

If the gate fails, the workflow exits neutral with a human-readable reason such as
`candidate age 7h14m < 24h` or `newer candidate superseded digest`.

## Operator guidance while CI catches up

Until the promotion workflow exists, operators that need soak protection should
pin an immutable short-SHA tag, or track `candidate`/`stable` only on hives where
a same-day auto-upgrade is acceptable. Release notes and changelog entries should
call out any burst release days so fleets can decide whether to defer upgrades.
