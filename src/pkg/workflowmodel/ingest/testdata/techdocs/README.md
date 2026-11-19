Trimmed snapshot of cncf/techdocs for the intake ingest tests
(hivecommons/hive#17620 phase 5).

- `.github/settings.yml`, `.github/workflows/*`: cncf/techdocs `main` at
  4b1f800a8c55491506ac426d755820a856747c92 (2026-10-28).
- `.github/ISSUE_TEMPLATE/assessment-request.yml`: the assessment intake
  form from nate-double-u/cncf-techdocs branch `build/03-intake`, the
  template the RFC's phase 5 definition of done refers to; `main` had no
  issue templates when the snapshot was taken.
- `.hive/branch-rules.json`: `GET /repos/cncf/techdocs/rules/branches/main`
  as read on 2026-11-11 (branch protection 404s; the org ruleset requires
  zero approvals and no checks, so nothing is inferred from it).
