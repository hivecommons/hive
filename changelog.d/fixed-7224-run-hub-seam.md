Hub mode logs at boot whether the `/api/reach` GitHub PR source was wired.
Previously a missing `HIVE_GITHUB_TOKEN` surfaced only as a 503 at request
time, with nothing in the log connecting the broken endpoint to the missing
credential. (#7224)
