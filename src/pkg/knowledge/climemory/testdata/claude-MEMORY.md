# Memory

- [Build pipeline](build-pipeline.md) — CI is the only verdict; never run go test locally
- [Deploy notes](deploy-notes.md)
- The operator prefers PRs under 400 lines and squash merges.
- User mentioned HIPAA once in passing; do not assume PHI
  is in scope for this repository.

## Repo conventions

1. Changelog fragments live at the repository root under `changelog.d/`.
2. Commit messages are signed with `-s`:
   - DCO trailer is checked in CI
   - unsigned commits are rejected
