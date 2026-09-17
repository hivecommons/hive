- Moved the GitHub App credential diagnosis and classification logic out of
  `cmd/hive`'s 10,000-line `main.go` into a new `pkg/apphealth` package
  ([#7238](https://github.com/hivecommons/hive/issues/7238)). Behaviour is
  unchanged — the six functions that decide whether an App installation
  authenticates, belongs to the right account, holds the permissions the hive
  relies on and actually covers the configured repositories keep their exact
  verdicts, and `cmd/hive` keeps its call sites via thin wrappers. What changes
  is testability: the two spoke App key paths are now an explicit `KeyPaths`
  argument instead of package-level variables that tests reassigned and
  restored, so the ~600 lines of verdict tests that had to live inside
  `package main` now run as an ordinary package with no shared mutable state.
