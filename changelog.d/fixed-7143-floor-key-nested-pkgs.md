Coverage floors are now keyed by a package's full path below `pkg/` instead of its
bare directory name, so a nested package can no longer silently share the floor of
the top-level package that happens to share its name. `pkg/spoke` and
`pkg/hub/spoke` are now `spoke` and `hub/spoke` rather than both being `spoke`.
