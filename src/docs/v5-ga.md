# v5 GA readiness bar

This page is the operator-facing mirror of the **v5 GA bar** tracker
([hivecommons/hive#6016](https://github.com/hivecommons/hive/issues/6016)). It is a
release-readiness definition, not a promise that every item is complete today:
`edge` is the active v5 channel, while `stable` and `candidate` remain on v4
until maintainers explicitly promote the v5 line.

A v5 stable release is ready only when every required row below has an owner or
tracker, a reproducible measurement source, and passing evidence from the named
source. If a source is missing, the row is **not measurable yet** and therefore
cannot be counted as complete.

## Required criteria

| Area | Criterion | Measurement source | GA status |
| --- | --- | --- | --- |
| Release-line CI | The `v5` branch is listed in `.github/release-lines.yml` and every workflow listed under `pinned` either runs on `v5` or records an explicit `-v5` maintainer exclusion. | `.github/release-lines.yml` plus the `release-line-guard.yml` check on `v5`. | **Evidence-ready:** `v5` is listed in `release_lines` and `release-line-guard.yml` is green on `v5` — last five runs all `success`, latest 2026-09-11T16:39Z on `2e2ebf7b`; measurement recorded in [#6887](https://github.com/hivecommons/hive/issues/6887). Row closes when a maintainer records the evidence on [#6016](https://github.com/hivecommons/hive/issues/6016) and re-verifies it is still green at GA cut. |
| Required build/test gate | **Accepted 2026-09-09 ([#6171](https://github.com/hivecommons/hive/issues/6171) closed as decided):** the protected release-line gate for `v5` has been green for 7 consecutive days, with no ignored required checks. A red required check on the protected branch restarts the window; a concurrency-cancelled run does not count as red ([#6293](https://github.com/hivecommons/hive/issues/6293)). | GitHub branch protection/check history for `v5`. Branch protection currently requires the `gate` context from `docker.yml` / **Build and Push Docker Image**; re-check with `unset GITHUB_TOKEN && gh api repos/hivecommons/hive/branches/v5/protection --jq '.required_status_checks.contexts'`. Check run evidence can come from `gh api repos/hivecommons/hive/commits/v5/check-runs` or `gh run list --branch v5 --workflow docker.yml --limit N --json conclusion,createdAt,url,headSha`. | **Evidence-ready:** the 7-day window elapsed 2026-09-10T11:25Z on the `gate` context. Window start is 2026-09-03T11:25:39Z (`f80698e2`) — the first green run after the last red, 2026-09-03T10:02:31Z (`cbd39972`, [run 33742150760](https://github.com/hivecommons/hive/actions/runs/33742150760)), which restarted the window per the accepted rule. Re-measured 2026-09-14: **zero** `failure` runs since that restart across 190 sampled `docker.yml` runs on `v5` spanning 2026-08-18 to 2026-09-14T09:44Z, so the streak now stands at ~11 days; cancelled-by-concurrency runs ignored per [#6293](https://github.com/hivecommons/hive/issues/6293); evidence recorded in [#6808](https://github.com/hivecommons/hive/issues/6808). Row closes when a maintainer records the evidence bullet on [#6016](https://github.com/hivecommons/hive/issues/6016) — note this arms the v4 feature-freeze trigger once all Release-train rows are green. |
| v5 channel publication | Every successful `v5` Docker build publishes immutable short-SHA image tags and moves only the `edge` channel; `stable`/`candidate` remain v4 until a promotion policy lands. | `docker.yml` run summary and GHCR manifests for `ghcr.io/hivecommons/hive`, `hive-contributor`, and `hive-hub`; compare `edge` digest to the `v5-latest`/short-SHA digest. | **Evidence-ready pending candidate designation:** on 2026-09-14 the `edge` digest equalled the `v5-latest` digest for all three images, and `candidate` remained pinned to `v4-latest` ([#6887](https://github.com/hivecommons/hive/issues/6887)). The remaining check — `edge` vs the GA candidate's short-SHA tag — cannot run until a maintainer designates the GA candidate SHA on [#6016](https://github.com/hivecommons/hive/issues/6016). |
| Digest-verifiable rollback | Operators can resolve the GA candidate and rollback targets to immutable digests for all three images. | GHCR manifest inspection for the GA candidate's short-SHA tag and the documented rollback/channel-switch procedure. | **Procedure documented:** the digest-verifiable rollback/channel-switch runbook landed as [`release-rollback.md`](release-rollback.md) via [#6266](https://github.com/hivecommons/hive/pull/6266) (merged 2026-09-08), covering all three images and digest pinning/verification. Row closes when GHCR manifest inspection is run for the GA candidate's short-SHA tag and recorded on [#6016](https://github.com/hivecommons/hive/issues/6016). |
| Tagged release docs | The tagged-release path documents whether semver tags are cut from `v5`, still cut only from `v4`, or deliberately deferred for the v5 GA candidate. | `src/docs/releases.md` and the tagged-release workflow triggers. | **Policy documented:** [`releases.md`](releases.md#release-lines-and-the-v5-semver-policy) now states the v5 semver policy — semver tags are cut from `v4` only, `v5` publishes continuous images, and a `v5` semver line is deliberately deferred until this GA bar closes ([#6266](https://github.com/hivecommons/hive/pull/6266), merged 2026-09-08). Row closes when a maintainer records this on [#6016](https://github.com/hivecommons/hive/issues/6016). |
| Migration guide | A v4-to-v5 migration guide exists and names supported prerequisites, data/config compatibility, downgrade/rollback limits, and the channel choice operators should use during migration. | The merged migration guide (for example `UPGRADE.md` or a docs page linked from `src/docs/README.md`). | Guide exists: [`UPGRADE.md`](../../UPGRADE.md) landed via #5559 and is linked from `src/docs/README.md`; row closes when maintainers confirm it covers the GA candidate's prerequisites and rollback limits. |
| Dual-version validation | **Bound — maintainer-recorded decision, see [#6112](https://github.com/hivecommons/hive/issues/6112):** at least one documented validation run covers v4 and v5 hub/spoke operation during transition, including heartbeat, branch/channel switching, and rollback expectations. No automated dual-version test exists today — `podman-contract.yml` runs only `v2`; `v2-ci.yml`/`v2-tests.yml` cover a single line at a time — so the row is evidenced by a dated manual run record until CI grows one. | Owner: maintainers (**@clubanderson**), with the ci-maintainer lane collecting the evidence comment on [#6016](https://github.com/hivecommons/hive/issues/6016). Evidence bullet format: `Evidence (YYYY-MM-DD, recorder): hub image tag: <ghcr short-SHA>; spoke image tag: <ghcr short-SHA>; scenario: <branch/channel switch + rollback steps performed>; hive status/heartbeat output: <pasted or linked>; conclusion: pass/fail.` Gap: no CI artifact backs this row; see the [Dual-version validation exercise](#dual-version-validation-exercise-bound) subsection below. | Bound; not yet run. Closes when the first evidence bullet is posted on #6016. |
| Reviewer lane production exercise | **Bound — maintainer-recorded decision, see [#6130](https://github.com/hivecommons/hive/issues/6130):** one reviewer-lane (review swarm, [`src/docs/review-swarm.md`](review-swarm.md)) PR on `hivecommons/hive` is processed end-to-end in production — review posted by the lane and merged by a human — scheduled during the GA candidate week. | The PR URL, the lane's review-verdict comment URL, and the governor dispatch/`review-verdicts.json` artifact, linked from [#6016](https://github.com/hivecommons/hive/issues/6016); plus the #5617 tracker closure state (closed 2026-09-03). A maintainer triggers the exercise by kicking the reviewer-capable agent from the dashboard (`POST /api/kick/<agent>`) against the candidate PR. | Bound; not yet scheduled. Closes when the PR/comment/artifact links are posted on #6016. |
| Formal-model safety gate | Formal-verification expectations for v5 are documented and the escalation/merge/hold models have no expected-fail safety witnesses for the GA candidate. | The formal-verification workflow/test results plus any model witness tracker referenced from `src/docs/formal-verification.md`. | **Evidence-ready:** the `v5` verification matrix (`src/formal/escalation/run.sh`) carries 12 rows, all expecting `pass` — zero expected-fail safety witnesses remain in GA scope — and the model is verified green at a SHA whose modeled code is identical to `v5` HEAD. Note `formal-verify.yml` triggers only on `pull_request`/`workflow_dispatch`, so it never runs on `v5` directly — evidence necessarily comes from PR runs. The last green run is 2026-09-09T17:45Z ([run 34384844388](https://github.com/hivecommons/hive/actions/runs/34384844388)) on `sync/v4-into-v5-2026-09-09` at `fa87105b`, and every path that triggers the workflow (`src/pkg/escalation/**`, `src/pkg/scheduler/reviewer_lane*`, `src/formal/**`, the workflow file) is byte-identical between `fa87105b` and `v5` HEAD `c07d5893` — so that run covers the modeled code as it stands. (`src/pkg/scheduler/scheduler.go` has changed since `fa87105b` but is outside the modeled trigger path, so it is not drift.) Measurement recorded in [#6890](https://github.com/hivecommons/hive/issues/6890). Row closes when a maintainer records the evidence on [#6016](https://github.com/hivecommons/hive/issues/6016) and re-verifies at GA cut. |
| Hold and merge-lane hygiene | Hold-guard, merge-eligible, and self-merge lanes reject moved heads and have regression tests covering the v5 branch-hygiene incident class. | `go test` results for the relevant packages (`cmd/hive`, holdguard, scheduler) and linked issue/PR evidence such as #5589. | **Evidence-ready:** v2 Tests (the `go test` source for `cmd/hive`, holdguard, scheduler, including the [#5589](https://github.com/hivecommons/hive/issues/5589) incident-shape regression tests) is green again on `v5`: [run 34865015808](https://github.com/hivecommons/hive/actions/runs/34865015808) at SHA `ce2fa3db61ef` (2026-09-14T15:52Z), the first green since `2b95e1a88` (2026-09-12T06:54Z). The intervening reds were the [#6870](https://github.com/hivecommons/hive/issues/6870)/[#6648](https://github.com/hivecommons/hive/issues/6648) apt-mirror egress loss at the cgo-toolchain step, not code regressions. The same SHA also carries a green required `gate` run (docker.yml, 2026-09-14T15:52Z), so `ce2fa3db` sits inside the unbroken 7-day soak window that restarted 2026-09-03T11:25Z. The egress flake recurred on `v4` the same day ([#6935](https://github.com/hivecommons/hive/issues/6935)), so this green window is perishable — but the row's evidence is per-SHA, and a recorded `ce2fa3db` citation survives later flakes. Measurement recorded in [#6939](https://github.com/hivecommons/hive/issues/6939), then **re-timed** per [#6984](https://github.com/hivecommons/hive/issues/6984): four hours after the `ce2fa3db` window was recorded, the post-merge DCO trailer monitor went red on `v5` ([#6971](https://github.com/hivecommons/hive/issues/6971)) — protected-branch commit `01bd2469` carried a sign-off naming a different human than its author. Because merge hygiene is a property of the branch's ongoing history rather than of a single SHA, that red contradicted a 'row is green' recording, so the window is re-timed from the disposition point rather than from `ce2fa3db`. **Disposition landed 2026-09-14T21:38:15Z** ([#6992](https://github.com/hivecommons/hive/pull/6992), merge `8cad6e3b`): `01bd2469` was waived with a documented rationale, and — the substantive half — the pre-merge gate was generalised from bot-only to reject **any** sign-off that does not name the PR author, closing the coverage gap that let this shape through. Ported to `v4` by [#6995](https://github.com/hivecommons/hive/pull/6995) so the fix cannot be reverted by the next v4→v5 sync. **The monitor has been green on every `v5` push since**, starting with [run 34899914955](https://github.com/hivecommons/hive/actions/runs/34899914955) at `8cad6e3b` (2026-09-14T21:38:17Z) and continuing through `bab972ac` ([run 34905587102](https://github.com/hivecommons/hive/actions/runs/34905587102), 2026-09-14T22:44:35Z) — six consecutive green monitor runs. **The current `v5` HEAD `a7c1cf6b` carries BOTH a green post-merge DCO monitor ([run 34922599380](https://github.com/hivecommons/hive/actions/runs/34922599380)) and a green v2 Tests run ([run 34922599329](https://github.com/hivecommons/hive/actions/runs/34922599329)), 2026-09-15T02:48Z**, so a single SHA satisfies both of this row's evidence sources at once; the previous such SHA was `bab972ac` ([run 34905587101](https://github.com/hivecommons/hive/actions/runs/34905587101)). Recorded in full: the `a7c1cf6b` v2 Tests run went green **on a rerun of its failed jobs** — attempt 1 lost the *Prepare cgo toolchain for race tests* step to the same [#6935](https://github.com/hivecommons/hive/issues/6935) apt-mirror egress flake, with `Cache not found for input keys: hive-cgo-race-apt-Linux-X64-gcc-libc6-dev-v1`. That was a **cold cache**, not a regression: the offline apt cache [#7019](https://github.com/hivecommons/hive/pull/7019) added was seeded on the PR branch, which a base branch cannot read. The rerun saved it under `v5` (`ci-install-tool: saved 68 downloaded .deb file(s)`), so subsequent `v5` runs restore it and this row's evidence should be materially less perishable from here. Two v2 Tests reds inside this window (`04d3f310`, `feed484c`) were the [#6935](https://github.com/hivecommons/hive/issues/6935) apt-mirror egress flake at the *Prepare cgo toolchain for race tests* step — verified per-job, not code regressions — and that flake is now confirmed to hit `v5` as well as the `v4` occurrences #6935 reports, so this window remains perishable in the same way the `ce2fa3db` one was. Row closes when a maintainer records the citation on [#6016](https://github.com/hivecommons/hive/issues/6016), ratifies the `01bd2469` waiver (it was authored by the same contributor who created the commit, so it wants an independent eye), and the GA candidate SHA is green on this source. **Update 2026-09-15 (supersedes the `a7c1cf6b` citation above): the evidence is no longer rerun-dependent.** `v5` now carries **three consecutive SHAs that are green on all three required sources on their FIRST attempt** — v2 Tests, the post-merge DCO monitor, and the required `gate` (docker.yml): `6f7b327d` (10:18Z — v2 Tests [34957270418](https://github.com/hivecommons/hive/actions/runs/34957270418), DCO [34957270409](https://github.com/hivecommons/hive/actions/runs/34957270409), gate [34957270357](https://github.com/hivecommons/hive/actions/runs/34957270357)); `2ae11155` (12:33Z — [34969531082](https://github.com/hivecommons/hive/actions/runs/34969531082), [34969531192](https://github.com/hivecommons/hive/actions/runs/34969531192), [34969531122](https://github.com/hivecommons/hive/actions/runs/34969531122)); and `fcba517d` (14:04Z — [34979192555](https://github.com/hivecommons/hive/actions/runs/34979192555), [34979192901](https://github.com/hivecommons/hive/actions/runs/34979192901), [34979192987](https://github.com/hivecommons/hive/actions/runs/34979192987)). Every run cited here was confirmed `attempt=1` via the runs API, so none of them depends on a rerun — unlike the `a7c1cf6b` citation, which went green only after its failed jobs were rerun. **Recorded deliberately: current `v5` HEAD `441e1c0f` is NOT the strongest candidate** — its v2 Tests run ([34979218664](https://github.com/hivecommons/hive/actions/runs/34979218664)) is `attempt=2`, having lost attempt 1 to the [#7031](https://github.com/hivecommons/hive/issues/7031) apt-mirror egress flake. On this row's own standard a first-attempt green is better evidence than a rerun green, so `fcba517d` is the newest SHA meeting the stricter bar. The perishability this row has repeatedly flagged now has a structural mitigation as well: [#7052](https://github.com/hivecommons/hive/pull/7052) found the repo was **over GitHub's 10 GB Actions cache ceiling** (10.44 GB across 549 entries), so the offline apt cache that [#7019](https://github.com/hivecommons/hive/pull/7019) added was being LRU-evicted within hours of every write — which is why this flake class kept recurring after being closed three times. |
| Open severity bar | **Bound — maintainer-recorded decision, see [#6125](https://github.com/hivecommons/hive/issues/6125):** there are zero open P1/security/adoption-blocker issues that maintainers classify as blocking v5-only code, measured by the `ga-blocker` label plus the existing `security` label. Row is green when the query below returns zero issues. | `unset GITHUB_TOKEN && gh issue list --repo hivecommons/hive --label ga-blocker --state open` (adoption/P1 blockers) and `unset GITHUB_TOKEN && gh issue list --repo hivecommons/hive --label security --state open` (security blockers). No pre-existing label fit (checked: `priority/critical-urgent`, `priority/high`, `tide/merge-blocker`, none scope to v5-GA blockage), so `ga-blocker` is a new label created for this row. | **Evidence-ready:** both queries returned zero on 2026-09-13 (measured 00:22Z, re-verified 08:24Z); measurement recorded in [#6855](https://github.com/hivecommons/hive/issues/6855). Row closes when a maintainer records the run on [#6016](https://github.com/hivecommons/hive/issues/6016). |
| Fork & self-hosted portability | **Bound — maintainer-recorded decision, see [#6090](https://github.com/hivecommons/hive/issues/6090):** a fresh fork of `hivecommons/hive` passes its CI without edits, and the #6081–#6087 portability cluster is closed. | Issue-state query: `unset GITHUB_TOKEN && gh issue view N --repo hivecommons/hive --json state,title` for `N` in 6081..6087, plus one linked fork CI run URL. As of this row's binding, #6081–#6087 are all closed and #6102 (backend-smoke fork-image fix) is merged. | Bound; issue cluster closed. Closes when a fresh-fork CI run URL with zero required edits is linked on #6016. |
| v4 lifecycle policy | **Accepted 2026-09-09 ([#6346](https://github.com/hivecommons/hive/issues/6346) closed as decided):** v4 feature-freezes when all **Release train** rows in this GA bar are green, or on 2026-10-15, whichever comes first. After freeze, v4 takes security and critical fixes only; v4→v5 movement is cherry-pick-only, with no batch syncs, and each cherry-pick carries its own DCO-valid sign-off. Before the next batch sync, [#6312](https://github.com/hivecommons/hive/issues/6312) must be fixed; until then each batch sync requires human-verified DCO state and must not use lane remediation ([#6329](https://github.com/hivecommons/hive/issues/6329), [#6339](https://github.com/hivecommons/hive/issues/6339)). v4 EOL remains blocked on this GA bar per [#6140](https://github.com/hivecommons/hive/issues/6140) and [ROADMAP.md](../../ROADMAP.md#v4--stable-line). | This row, the [v4→v5 forward-port sync policy](v5-sync-policy.md), [ROADMAP.md](../../ROADMAP.md#v4--stable-line), and the GA tracker. | Accepted 2026-09-09 ([#6346](https://github.com/hivecommons/hive/issues/6346) closed as decided; see the [v4 lifecycle policy](#v4-lifecycle-policy-accepted) section below). Row closes when the tracker records the decision. |
| v4 EOL dependency | The v4 EOL announcement is explicitly blocked on this GA bar being complete; no EOL date is announced before the checklist is closed. | Public roadmap/release docs and the GA tracker state. | Not complete until the tracker and roadmap/release docs carry the dependency. |

## v4 lifecycle policy (accepted)

**Accepted 2026-09-09 — [#6346](https://github.com/hivecommons/hive/issues/6346) closed as decided.**

- **Freeze trigger:** v4 feature-freezes when every **Release train** row in
  this GA bar is green, or on 2026-10-15, whichever comes first. From that
  point until EOL, v4 accepts only security fixes and critical fixes.
- **Post-freeze sync direction:** v4→v5 movement becomes cherry-pick-only. No
  batch syncs are opened after the freeze line, and each cherry-pick carries its
  own DCO-valid `Signed-off-by` attestation.
- **DCO precondition before the next batch sync:** [#6312](https://github.com/hivecommons/hive/issues/6312)
  must be fixed before another batch sync is treated as routine, so syncs stop
  inheriting broken attestation from the v4 merge path. Until then, every batch
  sync requires human-verified DCO state and must not use lane remediation such
  as the failure modes recorded in [#6329](https://github.com/hivecommons/hive/issues/6329)
  and [#6339](https://github.com/hivecommons/hive/issues/6339).
- **EOL dependency:** v4 EOL remains blocked on this GA bar closing, consistent
  with the v4 support-window language in [ROADMAP.md](../../ROADMAP.md#v4--stable-line)
  and the `v4-EOL-blocked-on-GA-bar` dependency tracked by [#6140](https://github.com/hivecommons/hive/issues/6140).

## Required build/test gate soak window (accepted)

**Accepted 2026-09-09 — [#6171](https://github.com/hivecommons/hive/issues/6171) closed as decided.**

- **Duration:** require 7 consecutive days of green required checks on the
  protected `v5` branch before cutting the GA candidate. This deliberately
  extends the 24-hour candidate precedent in the
  [v4 stable soak policy](stable-soak-policy.md#proposed-promotion-rule)
  because GA promotes a whole release line, not one already-supported v4
  candidate digest.
- **Required-check set:** as of this update, branch protection reports the
  required context as `gate`, produced by `docker.yml` / **Build and Push Docker
  Image**. Reconfirm before recording evidence with:

  ```sh
  unset GITHUB_TOKEN && gh api repos/hivecommons/hive/branches/v5/protection --jq '.required_status_checks.contexts'
  ```

- **Restart rule:** any red required check on the protected `v5` branch restarts
  the 7-day window at the next green protected-branch SHA, mirroring the stable
  soak rule where a superseding candidate restarts the timer. A
  concurrency-cancelled run does not count as red and is ignored for the window
  ([#6293](https://github.com/hivecommons/hive/issues/6293)).
- **Evidence source:** record the branch-protection query and check/run history
  next to the tracker checkbox. Suitable collection commands are:

  ```sh
  unset GITHUB_TOKEN && gh api repos/hivecommons/hive/commits/v5/check-runs --jq '.check_runs[] | select(.name == "gate") | {name, conclusion, completed_at, html_url}'
  unset GITHUB_TOKEN && gh run list --branch v5 --workflow docker.yml --limit N --json conclusion,createdAt,url,headSha
  ```

- **Evidence bullet format:**
  - `Evidence (YYYY-MM-DD, recorder): required contexts: gate; window:
    <start-UTC>..<end-UTC>; branch: v5; check history: <workflow/check-run
    URLs>; cancelled-by-concurrency runs ignored: <URLs or none>; conclusion:
    pass/fail.`

## Dual-version validation exercise (bound)

**Bound — maintainer-recorded decision on [#6112](https://github.com/hivecommons/hive/issues/6112).**

- **Owner:** maintainers (**@clubanderson**); the ci-maintainer lane collects
  and posts the evidence comment on [#6016](https://github.com/hivecommons/hive/issues/6016).
- **Why no automated source:** `.github/workflows/podman-contract.yml` has only
  ever run on `v2` and was deliberately never extended to `v4` or a dual-line
  scenario; `v2-ci.yml`/`v2-tests.yml` each test one branch at a time. There is
  no existing job that stands up a v4 hub against a v5 spoke (or vice versa),
  so this row is evidenced by a dated manual run record rather than a CI
  artifact until one is built.
- **Scenario:** run one hub on a `v4` image and one spoke on a `v5` image (or
  the reverse pairing maintainers intend to support during transition),
  observe heartbeat delivery, perform one branch/channel switch, and perform
  one rollback, all against named image tags.
- **Evidence bullet format:**
  - `Evidence (YYYY-MM-DD, recorder): hub image tag: <ghcr short-SHA>; spoke
    image tag: <ghcr short-SHA>; scenario: <branch/channel switch + rollback
    steps performed>; hive status/heartbeat output: <pasted or linked>;
    conclusion: pass/fail.`
- **Gap:** until this exercise runs, the row stays open; if it recurs the CI
  gap should be filed as its own follow-up issue rather than re-run manually
  every release.

## Open severity bar label and query (bound)

**Bound — maintainer-recorded decision on [#6125](https://github.com/hivecommons/hive/issues/6125).**

- **Label:** `ga-blocker` (created for this row; no existing label —
  `priority/critical-urgent`, `priority/high`, `tide/merge-blocker` — scopes to
  v5-GA blockage specifically). Description: "Blocks the v5 GA cut
  (`src/docs/v5-ga.md` open-severity row)".
- **Canonical queries:**

  ```sh
  unset GITHUB_TOKEN && gh issue list --repo hivecommons/hive --label ga-blocker --state open
  unset GITHUB_TOKEN && gh issue list --repo hivecommons/hive --label security --state open
  ```

- **Pass condition:** the row is green when both queries return zero open
  issues.
- **Sweep:** maintainers apply or decline `ga-blocker` on open candidates (for
  example the #6081–#6087 portability cluster while open) and record the
  sweep result as a comment on [#6016](https://github.com/hivecommons/hive/issues/6016).

## Reviewer lane production exercise (bound)

**Bound — maintainer-recorded decision on [#6130](https://github.com/hivecommons/hive/issues/6130).**

- **Precondition met:** the #5617 reviewer-lane follow-ups closed on
  2026-09-03.
- **Exercise:** one reviewer-lane PR (the review swarm described in
  [`review-swarm.md`](review-swarm.md)) on `hivecommons/hive`, processed
  end-to-end in production — a review-capable agent posts an aggregate
  verdict and a human performs the merge — scheduled during the GA candidate
  week.
- **Trigger:** a maintainer kicks the review-capable agent from the dashboard
  (`POST /api/kick/<agent>`) against the candidate PR; the governor's
  `review.fan_out` eval cycle then dispatches the perspective reviews.
- **Evidence:** the PR URL, the lane's review-verdict comment URL (or the
  `review-verdicts.json` aggregate for that head SHA), and any governor
  dispatch artifact, linked from [#6016](https://github.com/hivecommons/hive/issues/6016).

## Fork and self-hosted portability (bound)

**Bound — maintainer-recorded decision on [#6090](https://github.com/hivecommons/hive/issues/6090),**
umbrella for the #6081–#6087 cluster (#6102 merged the backend-smoke half of
the fix).

- **Pass condition:** #6081, #6082, #6083, #6084, #6085, #6086, and #6087 are
  all closed, **and** a fresh fork of `hivecommons/hive` passes its CI without
  any edits to workflow files, prompts, or install artifacts.
- **Issue-state query:**

  ```sh
  unset GITHUB_TOKEN && gh issue view N --repo hivecommons/hive --json state,title   # N = 6081..6087
  ```

  As of this row's binding, all seven are closed.
- **Evidence:** the issue-list query output above plus one fork CI run URL
  showing a green run with zero required edits, linked from
  [#6016](https://github.com/hivecommons/hive/issues/6016).

## Tracker checklist template

The live tracker is
[hivecommons/hive#6016](https://github.com/hivecommons/hive/issues/6016).
Keep its checklist in sync with the table above. Each checkbox should link to
the PR, workflow run, issue, or artifact that proves it is complete. If the
tracker is ever replaced (for example by a milestone), update this link in the
same change.

```markdown
## v5 GA bar

### Release train
- [ ] `v5` is present in `.github/release-lines.yml`; release-line guard is green on `v5`.
- [ ] Required `v5` build/test gates are green for 7 consecutive days, with evidence recorded in the accepted format from [#6171](https://github.com/hivecommons/hive/issues/6171).
- [ ] GHCR shows `edge` digest-pinned to the GA candidate for all three images; `stable`/`candidate` remain v4 until promotion.
- [ ] Rollback/channel-switch procedure is documented and digest-verifiable.
- [ ] Tagged-release docs state the v5 semver policy.

### Migration and operator readiness
- [ ] v4-to-v5 migration guide is merged and linked from docs.
- [ ] Dual v4/v5 hub-spoke operation validation is linked with exact SHAs and image tags, per [#6112](https://github.com/hivecommons/hive/issues/6112)'s bound evidence bullet format.
- [ ] v4 EOL announcement remains blocked on this checklist.
- [ ] v4 lifecycle freeze/sync policy is signed off on [#6346](https://github.com/hivecommons/hive/issues/6346).

### Safety and agent governance
- [ ] Reviewer lane production exercise is linked (PR URL + verdict comment/artifact) and #5617 follow-ups are closed — closed 2026-09-03, per [#6130](https://github.com/hivecommons/hive/issues/6130).
- [ ] Formal-model CI expectations are green; no expected-fail safety witnesses remain for GA scope.
- [ ] Hold/merge branch-hygiene tests are green, including the #5589 incident shape.
- [ ] Zero open issues carry `ga-blocker` or `security` (`gh issue list --repo hivecommons/hive --label ga-blocker --state open`; same for `security`), per [#6125](https://github.com/hivecommons/hive/issues/6125).
- [ ] Fork & self-hosted portability: #6081–#6087 closed and one fresh-fork CI run URL is linked, per [#6090](https://github.com/hivecommons/hive/issues/6090).
```
