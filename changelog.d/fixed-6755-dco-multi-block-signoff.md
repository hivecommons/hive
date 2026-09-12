- The post-merge DCO monitor no longer reports `missing-signoff` for commits
  whose `Signed-off-by:` sits in an earlier paragraph than a trailing
  `Co-authored-by:` block. `check-dco-trailers.sh` parsed only the final
  trailer block (`git interpret-trailers --parse`), so a body shaped
  `Signed-off-by: …\n\nCo-authored-by: …` — which the pre-merge DCO app
  accepts — paged the monitor anyway (#6605 waived one instance; #6755 is the
  recurrence on `v5`). The checker now scans every line of the commit body,
  matching the pre-merge rule; the 8fe6bb34 entry in `DCO_WAIVED_COMMITS`
  becomes a reported `STALE-WAIVER` for maintainer removal (#6755).
