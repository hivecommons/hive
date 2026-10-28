package agent

import (
	"strconv"
	"strings"
)

// Repeat-failure coverage table (hivecommons/hive#14550, C4, C5, C12, C13).
//
// Every backend Hive supports (config.CLIBackends and
// config.InferenceBackends) has exactly one row. A backend is covered only
// when a committed, re-runnable harness made its real CLI produce a correct
// command record, with the CLI's own `--version` output in the evidence
// (C4). Hand-written test data does not count. Every other backend is not
// covered: its agent rows say "repeat-failure check not available for this
// backend", and the row names the reason and the follow-up issue (C5).
//
// The docs table in docs/backend-support-tiers.md ("Repeated-failure check")
// is rendered from this table; TestRepeatFailureCoverageDocsMatchCode fails
// when the two differ or a backend is missing (C13).

// repeatFailureBackendRoutesIssue tracks the backend-specific routes and the
// real-run evidence still missing for every backend below (#14559).
const repeatFailureBackendRoutesIssue = 14559

// repeatFailureSafeguardUnavailable is the shell-activity signal of a covered
// backend that has none, so the not-receiving-data safeguard cannot run (C12).
const repeatFailureSafeguardUnavailable = "safeguard unavailable"

// RepeatFailureCoverage is one backend's row of the coverage table.
type RepeatFailureCoverage struct {
	Backend string
	// Covered is true only with committed real-run evidence (C4).
	Covered bool
	// Route is the producer of the backend's records, or, for a backend that
	// is not covered, what exists today and what is planned.
	Route string
	// Evidence names the committed harness and the CLI `--version` it
	// recorded. Empty when not covered.
	Evidence string
	// EvidencePath is the committed harness evidence, relative to src/.
	// Required when Covered.
	EvidencePath string
	// ShellActivity is the signal the not-receiving-data safeguard watches
	// for this backend, or repeatFailureSafeguardUnavailable. Empty when not
	// covered.
	ShellActivity string
	// Reason says why the backend is not covered. Empty when covered.
	Reason string
	// FollowUp is the issue that tracks covering the backend.
	FollowUp int
}

const (
	rfNeutralRouteNoEvidence = "BASH_ENV route installed while the check is on, but no committed real-run harness evidence yet (C4)"
	rfOwnRouteNotBuilt       = "commands never reach the BASH_ENV route and the backend-specific route is not built yet"
)

// repeatFailureCoverageTable lists every backend in config.CLIBackends order,
// then config.InferenceBackends.
var repeatFailureCoverageTable = []RepeatFailureCoverage{
	{Backend: "claude", Route: "BASH_ENV (neutral), unwraps `eval '…'`", FollowUp: repeatFailureBackendRoutesIssue, Reason: rfNeutralRouteNoEvidence},
	{Backend: "copilot", Route: "none yet; planned: `events.jsonl` `tool.execution_complete`", FollowUp: repeatFailureBackendRoutesIssue, Reason: "one long-lived Bash session per shell id; " + rfOwnRouteNotBuilt},
	{Backend: "goose", Route: "BASH_ENV (neutral)", FollowUp: repeatFailureBackendRoutesIssue, Reason: rfNeutralRouteNoEvidence},
	{Backend: "codex", Route: "BASH_ENV (neutral), one record across `exec /bin/bash -c`", FollowUp: repeatFailureBackendRoutesIssue, Reason: rfNeutralRouteNoEvidence},
	{Backend: "pi", Route: "BASH_ENV (neutral)", FollowUp: repeatFailureBackendRoutesIssue, Reason: rfNeutralRouteNoEvidence},
	{Backend: "bob", Route: "BASH_ENV (neutral) with `SHELL=/bin/bash` (documented shell change: dash → bash)", FollowUp: repeatFailureBackendRoutesIssue, Reason: rfNeutralRouteNoEvidence},
	{Backend: "aider", Route: "guarded `~/.bashrc` line (`aider_bashrc`) sourcing the BASH_ENV script, Hive-managed homes only, with `SHELL=/bin/bash`; the no-tty `/bin/sh -c` path reads no rc file and is not covered", FollowUp: repeatFailureBackendRoutesIssue, Reason: "interactive `$SHELL -i -c` ignores BASH_ENV; `aider_bashrc` route installed while the check is on, but no committed real-run harness evidence yet (C4)"},
	{Backend: "gemini", Route: "BASH_ENV (neutral), unwraps the `{ …; }; __code=$?` wrapper", FollowUp: repeatFailureBackendRoutesIssue, Reason: rfNeutralRouteNoEvidence},
	{Backend: "agy", Route: "none yet; planned: `agy-turn` `run_command` steps", FollowUp: repeatFailureBackendRoutesIssue, Reason: "closed binary, likely a long-lived terminal; " + rfOwnRouteNotBuilt},
	{Backend: "opencode", Route: "BASH_ENV (neutral) with `SHELL=/bin/bash`", FollowUp: repeatFailureBackendRoutesIssue, Reason: rfNeutralRouteNoEvidence},
	{Backend: "kilo", Route: "BASH_ENV (neutral) with `SHELL=/bin/bash`", FollowUp: repeatFailureBackendRoutesIssue, Reason: rfNeutralRouteNoEvidence},
	{Backend: "muse", Route: "none yet; planned: `PostToolUse` / `PostToolUseFailure` hooks once Hive launches muse with a hook file or flag it owns", FollowUp: repeatFailureBackendRoutesIssue, Reason: "sandboxed shell tools in a closed binary, so commands never reach the BASH_ENV route; Hive has no muse launch or hook wiring and writes no muse settings file, so the hook route is not built"},
	{Backend: "omp", Route: "none yet; planned: `tool_result` extension event", FollowUp: repeatFailureBackendRoutesIssue, Reason: "embedded shell that strips BASH_ENV; " + rfOwnRouteNotBuilt},
	{Backend: "openhands", Route: "none yet; planned: reader of the saved conversation events (`exit_code`)", FollowUp: repeatFailureBackendRoutesIssue, Reason: "one interactive bash kept open in tmux, so commands never reach the BASH_ENV route; Hive has no openhands launch or hook wiring and does not write `~/.openhands/hooks.json` (so no `PostToolUse` hook), and the saved-events reader is not built"},
	{Backend: "omnigent", Route: "none yet; planned: tool-call policy checkpoint (`ALLOW`/`DENY` verdicts) or saved session events", FollowUp: repeatFailureBackendRoutesIssue, Reason: "meta-harness driving Claude Code, Codex or Pi through its own policy engine; " + rfOwnRouteNotBuilt},
	{Backend: "vllm", Route: "follows `claude` (drives Claude Code)", FollowUp: repeatFailureBackendRoutesIssue, Reason: "follows `claude`, which is not covered"},
	{Backend: "llm-d", Route: "follows `claude` (drives Claude Code)", FollowUp: repeatFailureBackendRoutesIssue, Reason: "follows `claude`, which is not covered"},
	{Backend: "litellm", Route: "follows `claude` (drives Claude Code)", FollowUp: repeatFailureBackendRoutesIssue, Reason: "follows `claude`, which is not covered"},
	{Backend: "watsonx", Route: "follows `claude` (drives Claude Code)", FollowUp: repeatFailureBackendRoutesIssue, Reason: "follows `claude`, which is not covered"},
}

// RepeatFailureCoverageTable returns a copy of the coverage table.
func RepeatFailureCoverageTable() []RepeatFailureCoverage {
	return append([]RepeatFailureCoverage(nil), repeatFailureCoverageTable...)
}

// RepeatFailureBackendCovered reports whether the check covers backend. An
// unknown backend is not covered.
func RepeatFailureBackendCovered(backend string) bool {
	backend = strings.ToLower(strings.TrimSpace(backend))
	for _, row := range repeatFailureCoverageTable {
		if row.Backend == backend {
			return row.Covered
		}
	}
	return false
}

// RepeatFailureCoverageMarkdownRows renders the docs table rows, one per
// backend, in table order. The docs carry exactly these lines.
func RepeatFailureCoverageMarkdownRows() []string {
	rows := make([]string, 0, len(repeatFailureCoverageTable))
	for _, row := range repeatFailureCoverageTable {
		status := "Not covered: " + row.Reason
		evidence := "None"
		signal := "n/a"
		if row.Covered {
			status = "Covered"
			evidence = row.Evidence
			signal = row.ShellActivity
		}
		followUp := "—"
		if row.FollowUp > 0 {
			n := strconv.Itoa(row.FollowUp)
			followUp = "[#" + n + "](https://github.com/hivecommons/hive/issues/" + n + ")"
		}
		rows = append(rows, "| `"+row.Backend+"` | "+row.Route+" | "+status+" | "+evidence+" | "+signal+" | "+followUp+" |")
	}
	return rows
}
