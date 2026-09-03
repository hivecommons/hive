# v5 GA readiness bar

This page is the operator-facing mirror of the **v5 GA bar** tracker. It is a
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
| Release-line CI | The `v5` branch is listed in `.github/release-lines.yml` and every workflow listed under `pinned` either runs on `v5` or records an explicit `-v5` maintainer exclusion. | `.github/release-lines.yml` plus the `release-line-guard.yml` check on `v5`. | Measurable now; must be green at GA cut. |
| Required build/test gate | The protected release-line gate for `v5` has been green for the soak window maintainers choose for GA, with no ignored required checks. | GitHub branch protection/check history for `v5`; at minimum the `gate`/Docker workflow and release-line guard results. | Measurable once branch protection/check history is reviewed. |
| v5 channel publication | Every successful `v5` Docker build publishes immutable short-SHA image tags and moves only the `edge` channel; `stable`/`candidate` remain v4 until a promotion policy lands. | `docker.yml` run summary and GHCR manifests for `ghcr.io/hivecommons/hive`, `hive-contributor`, and `hive-hub`; compare `edge` digest to the `v5-latest`/short-SHA digest. | Measurable now; must be checked for the GA candidate SHA. |
| Digest-verifiable rollback | Operators can resolve the GA candidate and rollback targets to immutable digests for all three images. | GHCR manifest inspection for the GA candidate's short-SHA tag and the documented rollback/channel-switch procedure. | Blocked until the rollback/channel-switch procedure is documented. |
| Tagged release docs | The tagged-release path documents whether semver tags are cut from `v5`, still cut only from `v4`, or deliberately deferred for the v5 GA candidate. | `src/docs/releases.md` and the tagged-release workflow triggers. | Not complete; current page is v4-oriented. |
| Migration guide | A v4-to-v5 migration guide exists and names supported prerequisites, data/config compatibility, downgrade/rollback limits, and the channel choice operators should use during migration. | The merged migration guide (for example `UPGRADE.md` or a docs page linked from `src/docs/README.md`). | Not complete until the guide lands (candidate tracker: #5559). |
| Dual-version validation | At least one documented validation run covers v4 and v5 hub/spoke operation during transition, including heartbeat, branch/channel switching, and rollback expectations. | A linked test report, PR, issue comment, or CI artifact that states the exact v4/v5 SHAs and scenario. | Not complete; no durable source is named yet. |
| Reviewer lane production exercise | A reviewer-lane PR has run in production after the #5617 follow-ups, with the result visible in the PR or governor artifacts. | The linked reviewer-lane PR/evidence artifact and the #5617 tracker closure state. | Not complete until #5617 follow-ups are closed and evidenced. |
| Formal-model safety gate | Formal-verification expectations for v5 are documented and the escalation/merge/hold models have no expected-fail safety witnesses for the GA candidate. | The formal-verification workflow/test results plus any model witness tracker referenced from `src/docs/formal-verification.md`. | Measurable only for models wired into CI; open expected-fail witnesses block GA. |
| Hold and merge-lane hygiene | Hold-guard, merge-eligible, and self-merge lanes reject moved heads and have regression tests covering the v5 branch-hygiene incident class. | `go test` results for the relevant packages (`cmd/hive`, holdguard, scheduler) and linked issue/PR evidence such as #5589. | Measurable now; must be green for the GA candidate. |
| Open severity bar | There are zero open P1/security/adoption-blocker issues that maintainers classify as blocking v5-only code. | GitHub issue search over v5 labels/severity labels plus the GA tracker checklist. | Measurable only after maintainers settle the exact label query on the tracker. |
| v4 EOL dependency | The v4 EOL announcement is explicitly blocked on this GA bar being complete; no EOL date is announced before the checklist is closed. | Public roadmap/release docs and the GA tracker state. | Not complete until the tracker and roadmap/release docs carry the dependency. |

## Tracker checklist template

Create or maintain one issue named **v5 GA bar** and keep this checklist in sync
with the table above. Each checkbox should link to the PR, workflow run, issue,
or artifact that proves it is complete.

```markdown
## v5 GA bar

### Release train
- [ ] `v5` is present in `.github/release-lines.yml`; release-line guard is green on `v5`.
- [ ] Required `v5` build/test gates are green for the agreed soak window.
- [ ] GHCR shows `edge` digest-pinned to the GA candidate for all three images; `stable`/`candidate` remain v4 until promotion.
- [ ] Rollback/channel-switch procedure is documented and digest-verifiable.
- [ ] Tagged-release docs state the v5 semver policy.

### Migration and operator readiness
- [ ] v4-to-v5 migration guide is merged and linked from docs.
- [ ] Dual v4/v5 hub-spoke operation validation is linked with exact SHAs.
- [ ] v4 EOL announcement remains blocked on this checklist.

### Safety and agent governance
- [ ] Reviewer lane production exercise is linked and #5617 follow-ups are closed or explicitly deferred.
- [ ] Formal-model CI expectations are green; no expected-fail safety witnesses remain for GA scope.
- [ ] Hold/merge branch-hygiene tests are green, including the #5589 incident shape.
- [ ] Zero open P1/security/adoption-blocker issues are classified as v5-GA blockers.
```
