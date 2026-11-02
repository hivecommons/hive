# Monthly outcomes publication

## Status and source access

This is a **supplied-input publication cadence, not a deployed data collector**.
The existing `outcome-benchmark.sh` runs `hivectl evidence benchmark` offline. It
reads schema-v1 files under the spoke's `/data/evidence`, optionally a saved
`GET /api/cost`, curated stories, red-team counts and saved hub evidence. It
does not call a dashboard or discover/authenticate to a spoke. Hub evidence
requires an admin session; the ordinary cost response also requires authorized
dashboard access. A hosted runner has neither that filesystem nor an assumed
dashboard session. No deployment credential, new service or private artifact
download is introduced here.

An evidence owner must produce and approve a complete monthly aggregate in the
source environment. Until that owner supplies the input below and the existing
publication credential, the scheduled/manual workflow **fails visibly and
publishes nothing**. Its presence does not establish operational collection.
If source collection is later automated, its timer belongs with the deployment
that owns the evidence, not on a hosted runner with expanded credentials.

## Cadence and cohort

`.github/workflows/monthly-outcomes.yml` runs at **06:17 UTC on the 15th** and
supports manual dispatch with an optional `month: YYYY-MM`. Empty selects the
prior completed UTC month, including December at January rollover. GitHub may
delay scheduled runs. The merge cohort must be exactly `[month start, next
month start)`, and its snapshot must observe at least 14 days after month end,
matching the existing maturation rule. A manual run before that point fails.
Past months can be dispatched explicitly; current/future months cannot.

The existing fold, rates, small-n suppression, Wilson intervals, lower medians,
coverage caveats and trend history are retained. The wrapper validates counts
and rate intervals; it does not invent confidence intervals for medians or
estimated spend. Cost keeps its **own** denominator and window, even if those
differ from the monthly merge cohort. The committed October snapshot remains
historical: approximately $6124.75 / 2869 hive-attributed merges from
2026-08-08 through 2026-10-27, an upper-bound/order-of-magnitude estimate, not
today's universal cost. It is not a completed-month automation input: it was
observed before October ended and contains 10 unreadable bundles.

## Owner-supplied input

1. In the evidence-owning environment, take an immutable, complete snapshot
   using existing authorized access. Check collection completion independently:
   zero unreadable files alone cannot prove a snapshot is complete. Retain the
   raw snapshot privately and calculate its SHA-256. Do not copy it into this
   public repository or upload it as an Actions artifact.
2. Record the exact generator Git revision. Use the existing generator against
   that snapshot with explicit `--month`, `--now` (snapshot observation time)
   and a private output directory. Optional cost input must be its matching
   saved response, not an extrapolation from October's figures. Check any
   `count_error`/partial or stale-count indicators in that saved response before
   approving it: the computed Report cannot reconstruct those source errors.
   Omit `Spend` entirely when its source is incomplete or unverifiable. Omit optional
   hub, story and evaluation inputs; publication strips them regardless.
3. Wrap the computed `outcomes-YYYY-MM.json` in this envelope, in a private local
   file. `report` is the complete existing `outcomebench.Report` object, not raw
   bundles. The two attestations are the evidence owner's responsibility.

   ```json
   {
     "schema": 1,
     "month": "YYYY-MM",
     "collection_complete": true,
     "publication_approved": true,
     "snapshot_sha256": "<64 lowercase hex characters>",
     "generator_revision": "<40 lowercase hex characters>",
     "report": {}
   }
   ```

4. Sanitize and validate locally, then inspect the prepared file:

   ```sh
   python3 src/scripts/monthly-outcomes.py prepare --month YYYY-MM \
     --input PRIVATE-ENVELOPE.json --out PREPARED-AGGREGATE.json
   ```

   The wrapper projects onto known aggregate fields. It replaces repository
   identities with `repository-001` labels, replaces source prose with the
   snapshot digest, allows only `agent/bot/human/unknown` author categories,
   drops unknown report fields, and omits stories, issue/review/log text,
   URLs, hub context and evaluation metadata. Dates and fixed cohort/category
   names are the only surviving input strings. It never logs input or renderer
   stderr. Aggregate counts can still be sensitive: explicit owner approval
   and human PR review are required, not just string scrubbing.
5. A repository secret administrator sets `MONTHLY_OUTCOMES_INPUT` to that
   **prepared** JSON (maximum 48000 UTF-8 bytes), using the existing GitHub
   secret management process. Do not put JSON in dispatch fields, issue bodies
   or logs. Replace the secret with a newly approved snapshot each month. A
   missing/stale month, premature observation, unreadable/empty cohort,
   invalid metric or unapproved/incomplete attestation fails closed.
   The workflow also requires the **existing** `TOPUP_PUSH_TOKEN` publication
   credential. Do not expand its scope or mint new privileges for this task;
   if unavailable, publication remains blocked. It uses no write grant on
   `GITHUB_TOKEN` and does not fall back to that token (which may suppress PR CI).

For manual offline rendering, run `monthly-outcomes.py render` with the same
input/month and an **empty** `--out` directory. It calls the existing Go
`--from-json` renderer, emitting its usual report/JSON/SVG/PNG assets plus
`outcomes-YYYY-MM.provenance.json`. Provenance records the approved snapshot
digest, input generator revision, actual renderer revision, observation time
and sanitized input digest. Completeness is an attestation; hashes enable
reproduction by authorized owners, not independent public verification.

## Review, reruns and failure handling

Only this month's rendered outputs and provenance are staged. Raw/supplied
input is never staged or uploaded. Root README graphics and prior reports are
not refreshed. Identical already-merged input skips rendering; identical input
on the open monthly branch makes no commit/push/repost. If the branch push
succeeds but PR creation fails, the run fails visibly; an unchanged rerun
retries the missing PR without another commit or provenance update. A branch
with no monthly report difference against `v5` does not open a PR. A serialized
`automation/monthly-outcomes-YYYY-MM` branch has one PR against `v5`, updated
with ordinary signed-off commits (no force push). Obsolete cards for that
month are removed. A closed/declined or already-merged PR is not automatically
reopened/reposted; a human must resolve any proposed correction.

The workflow never merges. Normal CI and authorized review gates apply,
including privacy, provenance and representativeness review. Input does not
prove that mediated and unmediated PRs are causally comparable. Required
source data/access and credential provisioning remain operator gates.
CI's focused fixtures exercise UTC selection, malformed/partial/stale input,
privacy projection, historical cost preservation, deterministic real rendering
and no-op/error handling, including local-Git/Mock-GitHub one-PR reruns,
failed-creation recovery and closed-PR refusal; no fixture makes a live
deployment/API request.
