# Sourcing token metering from ccusage behind a tokens.Source seam

Status: Proposed — discussion on #6234.

RFC credit: this design turns the adopter RFC in
[#6234](https://github.com/hivecommons/hive/issues/6234) into a reviewable plan
without implementing it. It also incorporates the current `v5` state after
[#6142](https://github.com/hivecommons/hive/pull/6142), so the attribution
argument is stated against the code that now exists.

Every code reference below was checked against `v4` unless explicitly marked
`v5`. Line numbers may drift; the named symbols are the stable handles.

---

## Problem

Hive can rotate across many agentic CLI backends, but token and cost metering is
narrower than backend selection. `CLIBackends` lists eleven CLI backends on `v4`:
`claude`, `copilot`, `goose`, `codex`, `pi`, `bob`, `aider`, `gemini`, `agy`,
`opencode`, and `kilo` (`src/pkg/config/config.go:4997`,
`src/pkg/config/config.go:5024`). The token collector currently merges native
session scans for Claude, Copilot, and Bob only
(`src/pkg/tokens/collector.go:319`, `src/pkg/tokens/collector.go:329`,
`src/pkg/tokens/collector.go:339`). Those scanners are
`ScanClaudeSessions` (`src/pkg/tokens/claude_scanner.go:56`),
`ScanCopilotSessions` (`src/pkg/tokens/copilot_scanner.go:68`), and
`ScanBobSessions` / `ScanBobSessionsWithLogger`
(`src/pkg/tokens/bob_scanner.go:97`, `src/pkg/tokens/bob_scanner.go:101`).

The price table is also local and hand-maintained. `pricing.go` documents it as
estimated list-price fallback for backends without native dollar figures and
preserves dashboard labeling between native and estimated cost
(`src/pkg/tokens/pricing.go:7`, `src/pkg/tokens/pricing.go:11`). Its
`priceTableDate` is a deterministic constant, currently `2026-07-24`
(`src/pkg/tokens/pricing.go:24`). That makes cost estimates reproducible, but it
also means newly shipped model IDs and price changes require code changes before
Hive sees them.

The RFC proposes using `ccusage` for parsing and pricing coverage. ccusage does
not cover every Hive backend, but it covers the major gap set called out by the
adopter: Claude, Copilot, Codex, Goose, Pi, Gemini, Antigravity (`agy`),
OpenCode, and Kilo. It does not cover Bob, Aider, or Muse, so replacing every
scanner would lose Bob coverage.

## Current v5 detector state

The RFC originally referred to `EnhancedAgentDetector`, `ScanClaudeSessions`,
and `ScanBobSessions` as symbols to preserve or delete. That has changed on
`v5`: `git log origin/v5 --oneline -3 -- src/pkg/tokens` shows
`be45d1b07 [architect] refactor: delete dead legacy Claude/Bob scanner variants
and write-only agent-names state from pkg/tokens (#6142)`. A `v5` grep for
`AgentFromTmuxEnv\|HiveAgentDetector` still finds both in
`src/pkg/tokens/claude_scanner.go`, while `EnhancedAgentDetector` is gone.
`ScanBobSessionsWithLogger` remains in `src/pkg/tokens/bob_scanner.go`; the old
wrapper spelling is not the important seam.

The attribution-survival argument should therefore be: keep the path-based Hive
agent detector, not the deleted enhanced detector. On `v4`, `AgentFromTmuxEnv`
parses Claude project paths containing `-data-agents-<name>`
(`src/pkg/tokens/claude_scanner.go:396`), and `HiveAgentDetector` tries that
path-based detector before falling back to message keywords
(`src/pkg/tokens/claude_scanner.go:421`). ccusage session JSON includes a
project path, so the ccusage implementation should feed that path into the same
Hive attribution function.

## Proposed design

Recommend a hybrid design:

- Add a `tokens.Source` seam.
- Implement a ccusage-backed source invoked as a subprocess with `--json
  --offline` for the nine covered backends.
- Keep `bob_scanner.go` because ccusage has no Bob adapter.
- Keep Hive's pricing layer only to preserve `native` versus `estimated`
  labeling and UI explanations, not as the primary model price catalog for
  ccusage-backed sources.

The ccusage binary should be pinned and bundled in the Hive image, not fetched at
runtime. Its JSON output shape becomes an API dependency, so Hive should assert
the expected fields in tests. `--offline` is mandatory to keep agent metering
inside the existing egress posture and avoid live pricing fetches during scans.

## Configuration shape

```yaml
tokens:
  source:
    mode: hybrid          # current | ccusage | hybrid
    ccusage:
      path: /usr/local/bin/ccusage
      version: "20.0.20"
      offline: true
      backends: [claude, copilot, codex, goose, pi, gemini, agy, opencode, kilo]
      parallel_diff: [claude, copilot]
```

`hybrid` means ccusage is authoritative only for backends that have completed
cutover; current scanners remain active for Bob and for any backend still in
parallel-diff mode. The implementation can hide this under deployment defaults,
but the operator-visible diagnostics should report source mode, ccusage version,
covered backends, and whether a backend's cost is native or estimated.

## Security and supply-chain considerations

- **Pinned binary.** Bundle an exact ccusage version in the image and record its
  provenance. Do not install it dynamically during scans.
- **Offline scans.** Always pass `--offline`; metering should not add network
  egress.
- **Subprocess hardening.** Invoke ccusage without a shell, pass fixed arguments,
  set a timeout, bound output size, and treat parse errors as source failures
  rather than partial success.
- **JSON shape test.** A fixture should fail if required fields such as session
  ID, project path, model name, token counts, and cost move or disappear.
- **Attribution preservation.** Continue mapping project paths through the Hive
  detector so per-agent and per-repo accounting survive parser replacement.
- **UI honesty.** Historical totals will shift when prices and parser semantics
  change. The dashboard should show a note during cutover and preserve whether a
  value is native spend or an estimate.

## Phased implementation plan

1. **Smallest first PR:** introduce a `tokens.Source` interface and wrap the
   current scanner stack behind it. No behavior change.
2. Add the ccusage implementation behind a disabled config flag. Invoke
   `ccusage <backend> session --json --offline`, assert JSON shape in tests, and
   map `projectPath` through the Hive detector.
3. Parallel-run ccusage and current scanners for Claude and Copilot. Store and
   display diffs in diagnostics without changing authoritative totals.
4. Cut over backend by backend once diffs are understood. Start with a backend
   that has no current Hive scanner, then Claude/Copilot after parallel-run
   confidence.
5. Keep `bob_scanner.go` until ccusage gains a Bob adapter or maintainers
   explicitly accept losing Bob metrics.
6. Add a UI note and release note when totals shift from Hive-estimated parsing
   to ccusage-backed parsing and pricing.

## Open questions from the RFC

1. **Should Hive delete all scanners?** Recommended answer: no. Keep Bob scanning
   until a ccusage Bob adapter exists or maintainers intentionally drop Bob
   metering.
2. **Should ccusage be the exclusive price source?** Recommended answer: use it
   for covered estimated pricing, but keep Hive's native-versus-estimated
   labeling and any native spend sources. `pricing.go` remains the label and
   fallback layer, not the primary catalog for ccusage-backed totals.
3. **How do we trust a subprocess parser?** Recommended answer: pin the binary,
   bundle it in the image, run offline, test JSON shape, bound execution, and
   expose source diagnostics.
4. **How do we prevent attribution regressions?** Recommended answer: make
   project path the seam. Feed ccusage's project/session path into the existing
   `AgentFromTmuxEnv` / `HiveAgentDetector` logic and parallel-run against
   Claude/Copilot before cutover.
5. **How should operators learn that totals changed?** Recommended answer: add a
   dashboard note during cutover and include source/version metadata with cost
   totals so historical comparisons are not silent.

## Non-goals

This design does not implement ccusage integration, delete scanners, change the
image, or alter cost totals. It defines the seam and migration path for future
code PRs.
