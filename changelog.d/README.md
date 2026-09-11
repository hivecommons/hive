# changelog.d — one changelog fragment per PR (#5675)

Changelog entries used to be appended directly under `CHANGELOG.md`'s
`## Unreleased` heading, which made any two PRs landing near each other edit
the same lines under the same heading — the second one to merge always hit a
structural conflict, however unrelated the code was. Entries now land here as
**one file per PR**, and `src/scripts/compile-changelog.sh` folds them into
`## Unreleased` when `.github/workflows/tagged-release.yml` cuts a release,
deleting the fragments in the same release commit.

## Format

- **Filename:** `<category>-<pr-or-slug>.md`, where `<category>` is one of
  `added`, `changed`, `deprecated`, `fixed`, `security` — it picks the `###`
  subsection your entry lands under, which in turn drives the semver bump
  (`added` ⇒ minor; the rest ⇒ patch — see `src/docs/releases.md`).
  `<pr-or-slug>` is your PR/issue number, a short slug, or both:
  `fixed-5654-relay-retry.md`.
- **Content:** exactly the entry itself — one `- ` bullet in the same rich
  narrative style as the existing `CHANGELOG.md` entries (what changed, why an
  operator cares, issue links). No headings: the compiler owns the `###`
  subsection, your file is the bullet. Multiple bullets are fine when one PR
  genuinely warrants them; use one file per category if they differ. Prose or
  headings before the first bullet fail the guard with
  `a fragment must start with a '- ' entry bullet (or a '<!-- release: ... -->' marker) — it IS the entry; the compiler owns the ### headings`.
- **What qualifies:** the same rule as always (top of `CHANGELOG.md`):
  user-visible features, fixes, security changes, migrations, deprecations,
  breaking changes. Routine refactors, test-only changes, and dependency churn
  need no fragment; docs-only changes are exempt too. Put a `no-changelog`
  label on the PR if the guard asks for a fragment for a change that is not
  user-visible.
- The `<!-- release: none|major|minor|patch -->` escape hatch
  (`src/docs/releases.md`) may ride in a fragment as its first line; it is
  compiled into `Unreleased` verbatim and honoured by
  `derive-release-version.sh` exactly as a hand-placed marker is.

Example — a complete fragment, `changelog.d/fixed-1234-relay-timeout.md`:

```markdown
- The contributor relay no longer times out during long tasks ([#1234](https://github.com/hivecommons/hive/issues/1234)). Previously ...
```

## Direct `CHANGELOG.md` edits

The transition window is over: since 2026-09-09 the fragment guard no longer
accepts a direct `CHANGELOG.md` edit as a substitute for a fragment. A
user-visible code change must add a `changelog.d/` fragment, or carry the
`no-changelog` label if the author and maintainers agree no release note is
needed. If such a PR edits `CHANGELOG.md` directly without a fragment or label,
the guard reports `This PR edits CHANGELOG.md's Unreleased section directly;
the transition window ended 2026-09-09` and then fails with
`src/ code changed without a changelog.d fragment or a no-changelog label`.

Entries already sitting under `## Unreleased` keep working: the compiler merges
fragments *into* whatever is there.

This `README.md` is never treated as a fragment and keeps the directory alive
between releases.
