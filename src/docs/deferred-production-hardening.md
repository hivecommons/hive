# Deferred production hardening

This ledger records non-blocking P2 work deferred by the Hive + Visual Hive
release-scope freeze on 2026-07-12. None of these items is required for the
current two-command installation, repository lifecycle, or hosted-controller
acceptance criteria. The production default has since moved from a local
per-user scheduler to the repository-owned hosted controller; local scheduler
items below apply only to the explicit `--runtime local` compatibility path.

## Deferred items

- Add an optional Windows service installation mode for local-mode operation
  before an interactive user login. Hosted production has no such dependency;
  the compatibility scheduler resumes at the owning user's next login without
  asking for stored credentials.
- Shorten the cold Windows installer recovery-matrix runtime. The complete
  matrix is bounded, passes, and remains inside the hosted release-job limit;
  further optimization is a CI-maintenance improvement.
- Add optional visual-provider adapters beyond the first-party Playwright
  path. No paid provider is required for deterministic verdicts.
- Expand package-manager-specific convenience paths beyond the npm and mixed
  JavaScript/Python proof repositories used by this release.
- Add further synthetic failure variants beyond the existing immutable
  candidate, exact-head/diff, hosted evidence, durable journal, lifecycle
  authority, and duplicate-prevention coverage.
- Normalize pre-existing Go formatting drift in untouched upstream files. The
  frozen candidate's changed and new Go files are checked independently.
- Refresh the immutable GitHub Actions commit pins to upstream releases that
  natively target Node 24. GitHub currently forces the pinned Node 20 actions
  onto Node 24 and emits a maintenance annotation, while all affected release
  steps continue to pass.
- Bound and clean up poller lifetimes in the legacy tmux agent manager under
  repeated concurrent pause/resume stress. The nil-context panic is fixed in
  this release; the remaining stress-only runtime does not affect the
  hosted repository controller, the explicit local compatibility scheduler,
  or either required lifecycle proof.

These items must be reconsidered in a separate goal with their own acceptance
criteria. They are not release blockers for the current frozen candidate.
