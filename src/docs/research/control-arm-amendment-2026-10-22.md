# Pre-registration amendment: within-repo control arms (2026-10-22)

Amends the **Experimental arms** and **Analysis plan** of
[the October pre-registration](preregistration-2026-10.md), tracked by
[#12071](https://github.com/hivecommons/hive/issues/12071) and
[#13532](https://github.com/hivecommons/hive/issues/13532).
The registered file is unchanged. This amendment records the prospective design
before control-window assignment/collection is enabled; no control-arm result
is claimed. Later design changes require another dated amendment.

## Assignment

Primary design: within each consenting repository, schedule one seven-day
**off** window in every four-week cycle. The other three weeks are **on**.
The owner must explicitly opt in per repository; default is off (no experiment).
Record the UTC epoch, schedule version, owner consent and planned stop before
collecting data. Run for at least one complete four-week cycle and disclose
any extension before collecting the extension. Participation is reversible;
record opt-out/interruptions without reassigning already observed PRs.

Off means no new Hive dispatch for that repo; it does not disable merge safety
checks, review/write policy enforcement or human access. In-flight work can
cross a boundary: record and disclose it, not kill it or call its later merge a
fresh dispatch. Do not modify existing merge behaviour in either arm.

Assign a PR by its forge open timestamp and the prospectively registered
schedule, once, across every head. Do not assign by its outcome, the time Hive
first reviewed it or the time a backfill ran. Unknown historical assignments
remain missing. A collector must cover all forge PRs in both arms independently
of dispatch/review activity; otherwise the off arm is systematically missing.

## Reporting

Within each repo, report counts opened/closed/merged, open-to-merge time,
14-day reverts and issue reopens, and captured settled CI pass rate. Preserve
unknown outcomes, missing timestamps and unobserved/pending CI separately.
Do not pool different repositories or author kinds into an effectiveness
claim. The dashboard's all-author view is descriptive; use its author-kind
filter for stratified views. For every contrast, Δ = on minus off.

Suppress a metric's delta unless each arm has n ≥ 20 **usable observations for
that metric**. Show counts and denominators even when suppressed. Do not turn
missing observations into zero. Counts opened during three on-weeks versus
one off-week are not a throughput contrast; publication must normalize by
observed exposure and report interrupted periods. Wait for a full 14-day
follow-up for confirmatory change-failure analysis; dashboard values before
that point are provisional. Keep the original population exclusions (including
separate Hive-on-Hive reporting) and interval analysis from the pre-registration.

The initial code ships only optional bundle tags, within-repo descriptive
summaries and guarded deltas. No schedule, consent or all-PR collection is
activated by this amendment. The reporting details and pending implementation
are listed in [Evidence / Control arm](../evidence.md#control-arm).

## Held-PR shadow and secondary designs

On repos **already** at Holdgated, observe when held Hive PRs are released or
closed, overlapping human PRs opened while held, and revert/reopen outcomes.
Reuse the duplicate sweep's matcher; preserve the match basis and uncertainty.
Human overlap is not random assignment: hold duration and human choice are
confounders. Report this as a separate observational stratum, not as scheduled
`off` evidence. Do not put another repo into Holdgated for this study.

Paired repos are secondary sanity checks only. Shadow-gate “would have held”
observations estimate the gate's behaviour, not the absence of Hive. Neither
may be silently substituted for a scheduled control arm.

## Limitations

Within-repo assignment removes between-repo selection differences, **not**
time trends, carryover, seasonality or backlog/maintainer effects. Disclose
model/config changes, incomplete capture and opted-out weeks. Publish negative
results and missingness with the same prominence as favorable results.
