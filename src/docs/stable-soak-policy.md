# v4 stable soak and promotion policy

This policy is enforced by CI: every successful `v4` image build retags
`candidate`, while a separate scheduled or manually dispatched promotion
workflow advances `stable` by digest only after the gate below passes.

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

## CI enforcement

The existing release build continues to publish immutable short-SHA tags and move
`candidate`; it no longer moves `stable` on every `v4` merge. The
`Promote Stable Channel` workflow runs hourly and can also be manually dispatched.
It:

1. resolves the current `candidate` digest for `hive`, `hive-contributor`, and
   `hive-hub`;
2. compares the candidate digest with the current `stable` digest;
3. reads the candidate's first-seen time from the successful `docker.yml` run
   number recorded in the image metadata;
4. requires successful `v2 CI` and `v2 Tests` workflow evidence for the
   candidate SHA;
5. requires no open issue or PR labelled `release-blocker`;
6. requires maintained-hive smoke evidence from the dispatch input or the
   `STABLE_SMOKE_EVIDENCE` repository variable; and
7. retags `stable` to the candidate digest only when the gate passes and the
   moving-tag generation is newer than the currently published `stable` tag.

The workflow writes the candidate digest, SHA, generation, first-seen time, age,
checks consulted, blocker count, smoke evidence, decision, and any exception note
to the GitHub Actions step summary. If the gate fails, the workflow leaves
`stable` unchanged with a human-readable reason such as `candidate age 3600s <
required 86400s (24h)` or `newer candidate superseded this digest before the soak
window completed`.

## Emergency promotion exception

Manual dispatch may set `emergency-exception-reason` to shorten the 24-hour soak.
The reason must name the risk of waiting, the checks that passed, and the
rollback digest, and `emergency-followup-issue` must name the follow-up issue for
skipped soak or smoke evidence. The exception still requires the candidate to be
current, required workflows to be green, no open `release-blocker`, and the
monotonic generation guard to pass. When an emergency promotion succeeds, the
workflow records the exception in the step summary and comments on the follow-up
issue.
