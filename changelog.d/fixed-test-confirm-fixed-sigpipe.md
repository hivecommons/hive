Deflaked `test-issue-confirm-fixed.sh`: the DRY_RUN assertion piped `printf` into `grep -q`, which raced SIGPIPE under `pipefail` and intermittently failed `build-and-test`.
