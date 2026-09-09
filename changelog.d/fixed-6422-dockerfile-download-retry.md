- Hardened the `curl` release-tarball download retries in `src/Dockerfile`,
  `src/Dockerfile.contributor`, and the Dockerfile test fixtures under
  `src/deploy/` so the existing `--retry-max-time` budget is actually usable:
  a fixed `--retry-delay 5` with `--retry 8` exhausted all retries in ~40s,
  so a GitHub release-asset outage lasting longer than a minute failed the
  build even though a 300s (or 600s) time budget was declared. `--retry` is
  now 30 (300s budget) or 60 (600s budget) with `--retry-delay 10`, so the
  full time budget can be spent absorbing a multi-minute upstream 5xx
  incident ([#6422](https://github.com/hivecommons/hive/issues/6422)).
