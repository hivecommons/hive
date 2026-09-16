`hive-open-issue.md` now documents the third placeholder-enforcement point
(`CreateIssue`'s `validateIssueTemplateFilled`) and the deliberate title/body
asymmetry, which were undocumented after #7141. While documenting them, two
real defects surfaced and are fixed: `CreateIssue` now consults the shared
`pkg/issueshape` validator for bodies, so a `<analysis>`/`<fix>` skeleton body
can no longer slip past that path; and `pkg/issueshape` no longer mistakes
ordinary prose containing comparison operators (`a < b and c > d`) for an
unfilled template placeholder.
