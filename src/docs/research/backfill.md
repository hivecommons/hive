# Evidence backfill: run plan (2026-10)

**Status:** tooling landed (`hivectl evidence backfill`, [#12371](https://github.com/hivecommons/hive/issues/12371)).
**Runs: executed by the operator (@clubanderson) right after the tooling
merges**, in order Bluefin → Console → Hive/tunaos/Frostyard/Open Horizon.
The commands below need a token with read access and write into a hive's
evidence store; they are not scheduled by CI or by any hive, the operator runs
them from a shell and reports each printed summary back on #12371.

Parent: RFC [#12062](https://github.com/hivecommons/hive/issues/12062), proposal
C (Study). Rules: [`preregistration-2026-10.md`](preregistration-2026-10.md)
§Populations and exclusion rules. Schema and field semantics:
[`../review-evidence.md#backfilled-bundles`](../review-evidence.md#backfilled-bundles).

## Guardrail

Backfilled bundles are evidence records only. Nothing reads them to decide a
merge; auto-merge stays "CI green + review-swarm eligibility" on every hive
(RFC #12062 Non-goals). Every bundle the tool writes is marked
`backfilled: true` and carries only what GitHub history still knows: verdicts,
CI, sentinel and shadow observations are **absent, never fabricated**.

## Ordering (from the RFC)

1. **Bluefin** — external maintainers, pre-Hive baseline window through today.
2. **Console** — full history, one window per ACMM level so `policy.acmm_level`
   records the L0→L6 series.
3. **Hive, tunaos, Frostyard, Open Horizon** — breadth; Hive flagged `hive_on_hive`.

## Before any run

```bash
# A token that can read the target repos. Classic PATs need `repo` (private)
# or `public_repo`; fine-grained tokens need Pull requests: read.
export GITHUB_TOKEN="$(gh auth token)"

# Where bundles go. On the spoke that owns the repo this is its /data/evidence
# (the default); for an offline study copy pass --root.
export EVIDENCE_ROOT=/data/evidence

# Always dry-run first: counts only, zero writes, one request per 100 PRs.
hivectl evidence backfill --repo <owner/repo> --since <date> --root "$EVIDENCE_ROOT" --dry-run
```

Record the dry-run summary (`fetched`, `merged`, `closed`, `open`, `human`,
`agent`, `bot`, `links`, `flags`) on #12371 next to the command. Then run the
same command without `--dry-run`. Re-running is safe: existing bundles are
skipped (`skipped` in the summary), so a run interrupted by a rate limit is
simply repeated.

Agent logins: pass `--agent-login <login>` for any account the hive uses to
open PRs under its own name (the GitHub App bot login is usually already caught
by the `— hive:` trailer or the 🐝 footer, and lands as `author_kind_rule:
attribution_trailer` / `identity_footer`). Check the first dry run's
`agent` count against what the hive's throughput card reports for the window.

## 1. Bluefin (pre-Hive baseline) — run first

Consent: @mrbobbytables (repo owner). Window: from the start of the baseline
year to today. The repo list is the Bluefin hive's `hive.yaml` `repos`; the
primary repo is shown, repeat for each of the others.

```bash
# dry run
hivectl evidence backfill --repo ublue-os/bluefin --since 2025-01-01 --root "$EVIDENCE_ROOT" --dry-run
# write
hivectl evidence backfill --repo ublue-os/bluefin --since 2025-01-01 --root "$EVIDENCE_ROOT"
```

Expected flags: none (Bluefin is the primary case and has far more than 10
merged PRs). If a satellite repo comes back `under_min_merged`, keep the run —
the flag is what the analysis filters on; do not skip the repo.

Report: fetched / written / skipped, first and last `created_at`, per-kind
counts.

## 2. Console (ACMM dose series) — run second

Consent: Console owners (KubeStellar). Full history, split at each recorded
ACMM transition so `policy.acmm_level` is stamped per window. Fill the dates
from the hub timeline / audit (`ACMM level over time` in the pre-registration's
metric list) **before** running; the placeholders below are deliberate.

```bash
# dry run over the whole history first (counts only)
hivectl evidence backfill --repo kubestellar/console --since 2025-01-01 --root "$EVIDENCE_ROOT" --dry-run

# then one write per level window: --until is exclusive, so each window's
# --until is the next window's --since. Replace the dates with the recorded
# transition timestamps.
hivectl evidence backfill --repo kubestellar/console --since <L0 start> --until <L1 start> --acmm-level 0 --root "$EVIDENCE_ROOT"
hivectl evidence backfill --repo kubestellar/console --since <L1 start> --until <L2 start> --acmm-level 1 --root "$EVIDENCE_ROOT"
hivectl evidence backfill --repo kubestellar/console --since <L2 start> --until <L3 start> --acmm-level 2 --root "$EVIDENCE_ROOT"
hivectl evidence backfill --repo kubestellar/console --since <L3 start> --until <L4 start> --acmm-level 3 --root "$EVIDENCE_ROOT"
hivectl evidence backfill --repo kubestellar/console --since <L4 start> --until <L5 start> --acmm-level 4 --root "$EVIDENCE_ROOT"
hivectl evidence backfill --repo kubestellar/console --since <L5 start> --until <L6 start> --acmm-level 5 --root "$EVIDENCE_ROOT"
hivectl evidence backfill --repo kubestellar/console --since <L6 start>                    --acmm-level 6 --root "$EVIDENCE_ROOT"
```

`under_min_merged` is computed **per run**, so a short level window may be
flagged even though the repo as a whole is not. Report the per-window summary
and the transition dates used; the analysis pools the windows by repo before
applying the ≥ 10 rule.

Where the Console hive already recorded live bundles (L5/L6), those heads are
skipped and keep their live verdicts and CI; only the gaps are backfilled.

## 3. Hive, tunaos, Frostyard, Open Horizon — run third

Consent: each hive's maintainers, recorded on #12371. Repo lists come from each
hive's `hive.yaml` `repos`; the templates below take one repo at a time.

```bash
# hivecommons/hive — Hive developing Hive; flagged automatically, never pooled
hivectl evidence backfill --repo hivecommons/hive --since 2026-01-01 --root "$EVIDENCE_ROOT" --dry-run
hivectl evidence backfill --repo hivecommons/hive --since 2026-01-01 --root "$EVIDENCE_ROOT"

# tunaos (43 repos), Frostyard, Open Horizon — repeat per repo
hivectl evidence backfill --repo <org/repo> --since 2026-01-01 --root "$EVIDENCE_ROOT" --dry-run
hivectl evidence backfill --repo <org/repo> --since 2026-01-01 --root "$EVIDENCE_ROOT"

# a placeholder / available hive slot that has a repo but no project
hivectl evidence backfill --repo <org/repo> --since 2026-01-01 --flag placeholder_hive --root "$EVIDENCE_ROOT"
```

## Exclusions are flags, not drops

| Pre-registration rule | How the backfill applies it |
|---|---|
| Placeholder / available hives excluded | `--flag placeholder_hive` on every bundle of that repo. |
| Hive-on-Hive reported separately, never pooled | `hive_on_hive` added automatically for `hivecommons/hive` (or `--flag hive_on_hive`). |
| ≥ 10 merged PRs in the window | `under_min_merged` added automatically when the run saw fewer (`--min-merged` to change the floor). |

Readers (`/api/evidence/summary`, `hivectl evidence export` [#12367](https://github.com/hivecommons/hive/issues/12367))
filter on `backfilled` and `backfill.flags`; the rows stay on disk so the
exclusion is auditable.

## What to report on #12371 after each run

- The exact command, the dry-run summary and the write summary (`written`,
  `skipped`, `relinked`, `write_errors`, `api_calls`).
- Date range: earliest and latest `created_at` among written bundles.
- Row counts per `backfill.state` and `author.kind`, and the flags applied.
- Console only: the L0→L6 transition dates used for `--acmm-level`.
