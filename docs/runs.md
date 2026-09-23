# Runs

## Lifecycle

A run is represented by a staged task lease. The stages are `spec`, `plan`,
and `implement`; advancing from `spec` to `plan` and from `plan` to
`implement` records a `stage_completed` lifecycle event and fires any
`stage_completed` hooks.

The `implement` stage is terminal. When the relay holding an `implement` lease
sends `task_complete`, the dashboard records a final `stage_completed` event
with `stage_from=implement` and `stage_to=completed`, fires the same hook path
used by earlier stage advances, releases the lease, and projects the run with
`state: completed` and `completed_at` on `/api/runs` and `/api/runs/{key}`.
`task_failed` and lease expiry release or hide the work without marking the run
completed.
