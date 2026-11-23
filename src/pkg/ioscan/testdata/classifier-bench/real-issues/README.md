# Real-issue benign corpus

`github-issues.jsonl` holds 320 rows of ordinary issue text: the title and body
of closed issues (not pull requests) from 32 public, permissively licensed
projects. Every row is labelled `benign` and keeps its source issue URL and the
project's licence in `source`. No text comes from hivecommons repositories.

## How it was sampled

`fetch.py` is the provenance. For each repository in `REPOS` it checks the
licence against GitHub's SPDX id, then searches
`repo:<r> is:issue is:closed created:2025-01-01..2026-10-31`, newest first, and
keeps the first 10 issues that pass the filters:

- 200 to 1500 characters of title plus body;
- no `@` handles, emails, secret-shaped strings (`ghp_`, `AKIA`, private-key
  headers, the words password, token and secret) or HTML comments;
- no injection phrasing (`ignore all`, `ignore previous`), spam, `[rejected`
  titles or security reports (RCE, SSRF, command injection, vulnerabilities).

Issues are live data: bodies get edited and issues get deleted. The committed
JSONL is the pin, hashed in `MANIFEST.json` and checked by
`corpus_pin_agents_test.go`. A fresh `fetch.py` run will not reproduce it byte
for byte. Before re-pinning, run `fetch.py --verify`, which re-fetches every
row and lists any whose text no longer matches its linked issue.

## Dev and heldout

`split` is bit 0 of `sha256(id)`: set means `heldout`, clear means `dev`
(173 dev, 147 heldout). A row never moves between splits.

**Only dev rows may inform tuning.** Do not use heldout rows, or the heldout
numbers, to tune rules, thresholds or the classifier prompt. They are the
estimate of how often ordinary issue text is withheld, and they only stay
honest if nobody tunes against them.

- The deterministic scorecard (`scorecard_realissues_test.go`) gates the dev
  false-positive count in `testdata/scorecard_baseline.json` and lists the dev
  ids it withholds. It prints the heldout count without ids, and never gates
  or records it.
- The classifier benchmark (`classifierbench_test.go`) reports each split on
  its own row, with false positives at the warn and block thresholds.
