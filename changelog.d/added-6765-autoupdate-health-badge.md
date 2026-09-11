- The spoke dashboard version badge now surfaces auto-update health instead of
  looking identical for "updated as configured" and "quietly failing": a red
  `⚠ auto-update failed` badge when the self-upgrade attempt budget is
  exhausted (tooltip names the target SHA, attempts, last error, and the
  hive-self-upgrade RBAC / image-tag hint), a `⟳ auto-update retrying n/5`
  badge while attempts continue, and the "Queued for auto-upgrade" pill is
  suppressed while an upgrade marker exists so the badge cannot claim
  "queued" over a recorded failure (#6765).
