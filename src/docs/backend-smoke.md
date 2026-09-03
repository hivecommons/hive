# Backend smoke — the live canary for contributor CLI integration

Hive runs contributor tasks by driving vendor coding-agent CLIs — Claude Code,
Codex, and the rest of the [backend list](https://github.com/hivecommons/hive/blob/v4/docs/backend-setup.md) —
through the ClankeR relay. That seam is the one integration surface hive does
not control: vendors ship CLI updates on their own schedule, and what breaks
is readiness detection, completion detection, credential layout, and one-shot
invocation behavior. Every other test in this repo pins that seam against
captured fixtures, so a vendor change ships green in CI and fails contributors
in production.

The backend smoke is the live complement:
[`bin/test_backend_smoke.sh`](https://github.com/hivecommons/hive/blob/v4/bin/test_backend_smoke.sh)
drives the **real relay** against a fake in-process hub and — where a
credential exists — the **real backend CLI** on a one-line task, asserting the
machine-checkable contract end to end: task acceptance, the `HIVE_VERDICT`
sentinel, `completion_signal: verdict` rather than the chrome-idle fallback,
and the `task_failed` wire shape.

Its first full run caught three live relay bugs (the claude `●` verdict glyph
the parser did not accept, a headless-codex stdin hang, and codex's non-git
workspace refusal) — the exact failure class it exists for. See the
[Changelog](https://github.com/hivecommons/hive/blob/v4/CHANGELOG.md) entries.

## Who runs what

| Piece | Runs where | Who acts on it |
| --- | --- | --- |
| Scheduled live smoke ([`backend-smoke.yml`](https://github.com/hivecommons/hive/blob/v4/.github/workflows/backend-smoke.yml)) | hivecommons/hive's own GitHub Actions, every 6h — gated to this repo, so forks and downstream deployments never run it | hive maintainers, via the issues it files |
| Keyless subset | v2-ci, merge-gating on every PR | the PR author, like any red check |
| `just backend-smoke` | any laptop or hive host, opt-in, with that machine's own credentials | whoever ran it |
| Run telemetry (below) | **every hive hub, passively** — no setup, no model spend | the hive's operator; fleet-level aggregation is a planned follow-up |

Nobody operating a hive has to run or fund the smoke. It answers one global
question — "is hive's integration with the vendor CLIs still sound" — so it
runs centrally, once.

## What the suite tests

Three tiers; each degrades cleanly when its prerequisites are missing:

- **Drift checks (keyless, deterministic).** The relay's `HEADLESS_BACKENDS`
  table must agree with `KNOWN_BACKENDS` in `config/backends.conf` (previously
  kept in sync by comment only), and the claude/codex version pins must match
  between `src/Dockerfile` and `src/Dockerfile.contributor`.
- **Stub wire-contract scenarios (keyless).** A stub CLI binary on `PATH`
  drives the full relay↔hub loop — handshake, `task_assign`, verdict parsing
  off one-shot output, the failure shape — with zero API spend. This tier plus
  the drift checks is what v2-ci runs.
- **Live per-backend scenarios (needs a CLI + credential).** Per backend: the
  `detect_cli` health probe, a headless end-to-end run (`claude -p` /
  `codex exec` on a one-line no-file-access task, asserting the
  `HIVE_VERDICT: no_work_needed` reply on the wire), and an interactive
  end-to-end run (the real CLI in tmux, the relay scraping the pane, asserting
  `completion_signal == "verdict"` — a `chrome_idle` completion is a FAILURE
  here, because it means the sentinel contract is broken and only the fallback
  saved the task).

Knobs (see the script header for the full list): `HIVE_SMOKE_BACKENDS`
(default `claude codex`), `HIVE_SMOKE_MODEL_CLAUDE` / `HIVE_SMOKE_MODEL_CODEX`
(cheapest tiers by default — the plumbing is under test, not the model), and
`HIVE_TEST_REQUIRE_BACKEND_SMOKE=1`, which turns every skip into a failure.
That inversion is what keeps the scheduled lane honest: on a runner where
credentials are *supposed* to exist, "skipped for lack of credentials" is a
failure, not a green.

## The two lanes, and what a red means

The scheduled workflow runs a matrix of backends × two lanes, and the
distinction is the point:

- **latest** — installs the vendor's current CLI release on the runner.
  Red means **vendor drift is incoming**: shipped contributor images still
  work, but the next pin bump will break. Fix the integration before bumping.
- **pinned** — runs the suite inside
  `ghcr.io/hivecommons/hive-contributor:latest`, against the CLI versions
  contributors actually get. Red means **contributors are likely broken right
  now**.

## How failures are reported

- A red **scheduled** run files one issue **per failing lane**, titled
  `[Backend smoke: latest] <backends> failure on <sha7>` or
  `[Backend smoke: pinned] …`, labeled `backend-smoke, ci-failure, kind/bug`.
  The body states the lane's meaning in operator terms and links the run.
- **Dedupe is per lane**: if an open `backend-smoke` issue with that lane's
  title prefix exists, the run comments on it instead of filing another — a
  multi-day vendor outage is one issue with a comment trail, not a pile.
- **Only `schedule` events file.** A red `workflow_dispatch` triage run never
  touches the tracker.
- **Evidence travels with the failure.** On any failed scenario the suite
  prints the relay log tail, the fake hub's wire-message tail, and (for
  interactive scenarios) a tmux pane capture into the workflow log, and the
  full output is uploaded as a `backend-smoke-<backend>-<lane>` artifact. The
  linked run answers "what did the CLI actually print" without a repro.

## Credentials

The workflow holds the repo's only model credentials, scoped per matrix arm
(claude arms never see codex credentials and vice versa). Two forms per
vendor; either is sufficient:

| Secret | Form | Trade-off |
| --- | --- | --- |
| `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` | metered API key | preferred for an unattended schedule: never expires, own rate limits; costs ~32 cheapest-tier one-line calls/day |
| `HIVE_SMOKE_CLAUDE_CREDENTIALS_B64` / `HIVE_SMOKE_CODEX_AUTH_B64` | `base64 -w0 ~/.claude/.credentials.json` from a Claude Pro/Max login, or `base64 -w0 ~/.codex/auth.json` from a ChatGPT sign-in | flat-rate, but the OAuth refresh chain rotates so the stored blob **goes stale** and must be re-captured from a fresh login (a stale one reds the live scenarios), and the smoke shares the account's rate limits with its human |

Local runs need none of this: the suite falls back to copying the machine's
own logged-in credential into a throwaway `HOME`, so a laptop with a
subscription login runs the full live arms with zero setup.

**Activation checklist** (maintainers, once): add one credential secret per
backend, then run one `workflow_dispatch` per lane and confirm both go green.

## Run telemetry: `task_runs.jsonl` and `/api/contribute/run-stats`

The smoke asks its questions synthetically; every hive hub also records the
answers for its **real** contributor traffic. Each accepted
`task_complete`/`task_failed` appends one JSONL record to
`/data/contributors/task_runs.jsonl` (0600, 10 MiB cap with a single `.1`
rotation) carrying the hub-normalized fields — `completion_signal`, the
failure kind, the verdict — plus the task's wall-clock duration and a derived
`scenario` from a closed vocabulary:

| Scenario | Meaning |
| --- | --- |
| `verdict_complete` | the agent ended the task with its own `HIVE_VERDICT` sentinel — the contract working |
| `idle_complete` | completed only via the chrome-idle fallback — the sentinel contract is NOT honored for this backend. **The primary ratchet metric.** |
| `headless_complete` | completed with no completion signal on the wire: the headless one-shot path (whose completion is the exit code), or a pre-#5376 relay |
| `env_failure` | the client's runtime could not run the work — the broken-integration class the smoke hunts |
| `task_failure` | the work was attempted and failed on its merits |
| `unspecified_failure` | a failure with no usable kind (older relay or unrecognized value) |

`GET /api/contribute/run-stats?days=N` (default 7; public read-only, aggregate
counts only — no usernames, reasons, or tokens) serves per-backend scenario
counts, the chrome-idle share, and duration p50/p95. The intended use is a
ratchet: watch `idle_complete` share and `env_failure` counts per backend
trend toward zero, and treat a jump as a regression to root-cause.

This is DECLARE-only telemetry, per the boundary
`src/pkg/dashboard/contribute_protocol.go` documents: nothing routes,
cooldowns, or gates on any of it, and a telemetry write failure never fails a
task.

## Running it locally

```sh
just backend-smoke              # both default backends
just backend-smoke claude       # one backend
bash bin/test_backend_smoke.sh  # same thing, no just required
```

Keyless machines run the drift and wire-contract tiers and print loud `SKIP`
lines for the live ones. `HIVE_TEST_REQUIRE_BACKEND_SMOKE=1` reproduces the
scheduled lane's strictness exactly.
