# Hive Semgrep rules

Each rule here encodes one security obligation the code already states, and
each has a fixture: `rules/<rule>.yaml` is tested against `rules/<rule>.go`
(not compiled, only parsed). A `// ruleid: <id>` comment marks a line the rule
must report, and `// ok: <id>` marks a line it must not.

| Rule | Obligation |
|---|---|
| `hive-scan-error-returns-benign` | A classifier or scanner error is never reported as a clean result. The error goes back to the caller, which applies the fail mode (`Scheduler.classifierErrored`). |
| `hive-outbound-fetch-outside-netguard` | A server-side fetch or dial of an address a user or remote party chose goes through `netguard` (`IsPrivateURL` plus a `PinnedDialer` transport). |
| `hive-exec-shell-dynamic-script` | `sh -c` takes a constant script, with values passed as positional parameters. |
| `hive-untrusted-github-text-unscanned` | GitHub-authored title and body text reaches agent task text only through ioscan (`EnforceInputFrom` or a scheduler wrapper). The CodeQL query `hive/go/untrusted-github-text-to-kick` in `.github/codeql/` follows the same flow across functions. |

`.github/workflows/static-security.yml` runs these rules:

- `semgrep --test .semgrep/rules` checks every rule against its fixture, so a
  rule that stops matching fails CI instead of quietly guarding nothing.
- `semgrep scan --config .semgrep/rules src` blocks a PR only on findings the
  PR adds (`--baseline-commit` is set to the PR base). On push and on the
  schedule it only reports.

## Running locally

```sh
pip install semgrep==1.183.0
semgrep --test --metrics=off .semgrep/rules
semgrep scan --config .semgrep/rules --metrics=off --exclude '*_test.go' src
```

## When a rule fires

Fix the code; the rule message tells you how. If the finding cannot be
exploited (for example, the URL comes from configuration rather than from the
request), suppress that one line and give the reason a reviewer needs to check
it:

```go
resp, err := http.Get(u) // nosemgrep: hive-outbound-fetch-outside-netguard -- u is the operator-configured hub URL
```

Do not widen a rule's exclusions to make a finding go away. If a rule is
wrong, change it together with its fixture: add the case as an `// ok:` line
first.
