package agent

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

// agy route for the repeat-failure check (hivecommons/hive#14550, C1, C3,
// C14; issue #14750).
//
// agy is a closed binary whose commands never reach the neutral BASH_ENV
// route. In the headless launch every kick runs one `hive agy-turn`, which
// already parses agy's stream-json steps: a `tool` step for `run_command`
// carries the command in Parameters["CommandLine"]. While the check is on the
// kick passes `--repeat-failure-record <file>` and the runner appends one
// record per run_command step that ends (DONE, or a failed state) to the
// agent's raw record file. agy reports no exit status field; when the tool
// output states one ("exit code: 2") it is recorded, otherwise only the step
// state decides failed/succeeded. The interactive TUI launch bypasses the
// runner and has no route. Nothing is written into agy's own files; with the
// check off the kick passes no flag and the runner writes nothing (C14).

// RepeatFailureRouteAgyTurn names the agy-turn runner in a record's Route.
const RepeatFailureRouteAgyTurn = "agy_turn"

// agyRepeatFailureRecordFlag is the agy-turn flag naming the raw record file.
const agyRepeatFailureRecordFlag = "repeat-failure-record"

// agyRunCommandExitCode matches an exit status stated in run_command output.
var agyRunCommandExitCode = regexp.MustCompile(`(?i)\bexit(?:ed with)?[ _]?(?:code|status)\b[\s:=]*(-?\d+)`)

// agyRunCommandRecord builds the record of a run_command step that ended, or
// reports false for any other step. command is the step's CommandLine, taken
// from its earlier ACTIVE update when the final one omits it.
func agyRunCommandRecord(s *agyStepUpdate, command string) (RepeatFailureRecord, bool) {
	rec := RepeatFailureRecord{Agent: os.Getenv("HIVE_AGENT"), Backend: "agy", RawCommand: command, Route: RepeatFailureRouteAgyTurn}
	switch strings.ToUpper(strings.TrimSpace(s.State)) {
	case "DONE":
	case "ERROR", "FAILED", "CANCELED", "CANCELLED":
		rec.Failed = true
	default:
		return rec, false
	}
	if strings.TrimSpace(command) == "" {
		return rec, false
	}
	if s.ToolInfo != nil {
		if m := agyRunCommandExitCode.FindAllStringSubmatch(s.ToolInfo.Output, -1); len(m) > 0 {
			if code, err := strconv.Atoi(m[len(m)-1][1]); err == nil {
				rec.ExitCode = &code
				rec.Failed = code != 0
			}
		}
	}
	return rec, true
}

// agyRunCommandLine returns a run_command step's CommandLine, or "".
func agyRunCommandLine(info *agyToolInfo) string {
	if info == nil {
		return ""
	}
	cmd, _ := info.Parameters["CommandLine"].(string)
	return cmd
}

// repeatFailureAgyRecordFile is the record file a headless agy kick passes to
// the runner, or "" when the check is off (C14). Lock-free.
func (m *Manager) repeatFailureAgyRecordFile(agent *AgentProcess) string {
	if agent == nil || !m.repeatFailureEnabled() {
		return ""
	}
	home := AgentHome(agent.Name, agent.UID, "agy")
	if home == "" {
		return ""
	}
	return RepeatFailureRecordPath(home)
}

// provisionRepeatFailureAgyRecordDir creates the agent's 0700 record
// directory at a headless agy launch while the check is on, so the runner,
// which runs as the agent, can append to the record file. Best-effort: a
// failure is logged and the runner's appends then fail open.
func (m *Manager) provisionRepeatFailureAgyRecordDir(agent *AgentProcess, backend string) {
	if !strings.EqualFold(strings.TrimSpace(backend), "agy") || !agyHeadlessEnabled() {
		return
	}
	recordPath := m.repeatFailureAgyRecordFile(agent)
	if recordPath == "" {
		return
	}
	if err := ensureRepeatFailureRecordDirAs(m.agentExecUserSpec(agent), recordPath); err != nil {
		m.logger.Warn("repeat-failure: creating the agy record dir failed; agent launches without the route",
			"agent", agent.Name, "path", recordPath, "error", err)
	}
}
