- Jam GitHub Projects sync no longer forwards the hive's `GITHUB_TOKEN` to a
  custom `HIVE_JAM_PROJECT_SYNC_URL`: the token is sent only to
  `api.github.com` over https, overrides must use https (http only for
  loopback), and custom endpoints authenticate with the new
  `HIVE_JAM_PROJECT_SYNC_TOKEN` instead (#8811).
