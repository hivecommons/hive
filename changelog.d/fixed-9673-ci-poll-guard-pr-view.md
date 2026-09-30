- Extended the #9673 CI-poll nudge guard to also detect `gh pr view --json
  statusCheckRollup` and `gh pr status` as CI-status polling: only `gh run
  watch/view/list` and `gh pr checks` were recognized before, so an agent
  checking its own PR's checks via `gh pr view` could poll indefinitely
  without tripping the stop-polling nudge or the dashboard's "Waiting on CI"
  state. Ordinary `gh pr view` lookups (title, author, files, ...) are still
  ignored.
