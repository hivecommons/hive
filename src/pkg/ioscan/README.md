# ioscan

`pkg/ioscan` protects the untrusted-text boundary before GitHub issue, PR, label,
author, and comment text enters an agent kick.

- Deterministic rules normalize Unicode steganography, decode suspicious base64,
  detect known prompt-injection phrasing, dangerous directives, and secret shapes.
- Blocked text is replaced with `[ioscan: content withheld — ...]`; the raw
  segment is not injected.
- Optional canaries (`ioscan.canaries`) plant per-kick exfiltration markers and
  block/report leaks.
- Optional semantic classification (`ioscan.classifier.enabled`) sends only
  already-redacted untrusted segments to an OpenAI-compatible LLM judge. The
  classifier fails open on errors/timeouts; deterministic rules and canaries
  remain the floor. Scores above `block_threshold` redact in `fail_mode: open`
  and block the kick in `fail_mode: closed`.

The classifier is behind `Classifier.Score(ctx, text)` so a local ONNX/DeBERTa
backend can replace the current LLM judge without changing the scheduler path.

## Classifier benchmark

`classifierbench_test.go` measures `Classifier.Score` against a labelled corpus,
per backend: catch rate at the warn and block thresholds, false-positive rate,
errors (unavailable backend, quota or budget refusals, invalid verdicts), mean
latency, and token and dollar cost where the backend reports usage. It is
opt-in and never runs in the default `go test` run. A maintainer can run it
locally with their own key.

It also runs as the non-blocking `ioscan classifier bench` workflow
(`.github/workflows/ioscan-classifier-bench.yml`): weekly and on manual
dispatch, never on pull requests, and never as a required check. Each matrix
leg is one GitHub Models backend reached with the job's own `GITHUB_TOKEN`
(`models: read`). No repository secret or paid key is involved, which keeps
it clear of the concern that closed #19464. The bench's token ceiling, call
spacing and per-class cap keep a run inside the free tier's limits. The cost
column prices tokens at each model's list price, as the cost of that backend
with a paid key. The report table is in each job's summary.

The corpus is every `*.jsonl` under `testdata/classifier-bench/`, subdirectories
included, plus the in-house red-team corpus from `redteam_test.go`, converted in
memory. Each JSONL row carries exactly `id`, `label` (`attack` or `benign`),
`split` (`dev` or `heldout`), `source` and `text`, and ids are unique across the
whole corpus. New rows use the split rule from `benchSplitFor`: bit 0 of
`sha256(id)` set means `heldout`. **Heldout rows are reported separately and
must never be used to tune thresholds or the classifier prompt.**

```sh
cd src
IOSCAN_CLASSIFIER_BENCH=1 \
IOSCAN_CLASSIFIER_BENCH_OPENAI_ENDPOINT=https://api.openai.com \
IOSCAN_CLASSIFIER_BENCH_API_KEY=... \
IOSCAN_CLASSIFIER_BENCH_USD_PER_MTOK_IN=0.15 IOSCAN_CLASSIFIER_BENCH_USD_PER_MTOK_OUT=0.60 \
  go test ./pkg/ioscan/ -run 'TestClassifierBench$' -v
```

| Variable | Meaning |
|---|---|
| `IOSCAN_CLASSIFIER_BENCH` | `1` enables the benchmark; anything else skips it. |
| `IOSCAN_CLASSIFIER_BENCH_OPENAI_ENDPOINT` | OpenAI-compatible base URL for `LLMClassifier`. A URL ending in `/chat/completions`, such as `https://models.github.ai/inference/chat/completions`, is used as the exact completions URL. |
| `IOSCAN_CLASSIFIER_BENCH_API_KEY` | Bearer key for that endpoint. |
| `IOSCAN_CLASSIFIER_BENCH_BACKENDS` | Space-separated `ToollessBackends()` names, each run through `NewCLIClassifier`. |
| `IOSCAN_CLASSIFIER_BENCH_MODEL` | Model for every backend (default `DefaultClassifierModel` for the endpoint). |
| `IOSCAN_CLASSIFIER_BENCH_SPLIT` | `dev` or `heldout` to run one split only. |
| `IOSCAN_CLASSIFIER_BENCH_MAX_TOKENS` | Hard token ceiling for the endpoint (default 250000). A local metering proxy refuses to forward once it is spent, and refused cases count as errors. |
| `IOSCAN_CLASSIFIER_BENCH_MIN_INTERVAL` | Minimum spacing between calls to the endpoint, retries included, such as `5s`, to stay under a per-minute rate limit. |
| `IOSCAN_CLASSIFIER_BENCH_MAX_PER_CLASS` | Keep at most this many rows of each split and label, chosen by ascending `sha256(id)` so the sample is stable. |
| `IOSCAN_CLASSIFIER_BENCH_MAX_CALLS` | Call cap per CLI backend (default `DefaultCLIClassifierDailyQuota`). |
| `IOSCAN_CLASSIFIER_BENCH_USD_PER_MTOK_IN` / `_OUT` | USD per million prompt and completion tokens. Without them the cost column is `n/a`. |
| `IOSCAN_CLASSIFIER_BENCH_WARN_THRESHOLD` / `_BLOCK_THRESHOLD` | Thresholds to score against (defaults as in production). |
| `IOSCAN_CLASSIFIER_BENCH_REPORT` | Also write the markdown table to this file. |

CLI backends do not report usage, so their token and cost columns are `n/a`.
