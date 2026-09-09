- Converged the JS contributor relay's pane-tail helper onto a single
  non-blank-lines semantics — the last *n* non-blank rows of a
  `tmux capture-pane -p` dump, matching the Go agent manager's `paneTail`,
  which has always filtered blanks this way. The relay previously kept a
  second helper, `paneTailNonBlank`, beside the original blank-including
  `paneTail`, so the four `TRANSIENT_API_ERROR_TAIL_LINES` detectors stayed on
  the blank-including tail and were blind to a retryable API error on any CLI
  that renders inline near the top of its pane — the same shape that caused
  agy contributors to get stuck at `starting` forever
  ([#6413](https://github.com/hivecommons/hive/issues/6413)). `paneTail` now
  has the one semantics everywhere it is used, `paneTailNonBlank` is gone, and
  new shared golden fixtures under `bin/testdata/pane-fixtures/` are asserted
  against by both `bin/contributor-relay.test.js` and a new
  `src/pkg/agent/pane_fixtures_test.go`, so the JS and Go implementations
  cannot silently diverge again
  ([#6427](https://github.com/hivecommons/hive/issues/6427)).
