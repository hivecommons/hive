package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// omp route for the repeat-failure check (hivecommons/hive#14550, C1, C3,
// C14; issue #14747).
//
// omp runs bash commands in a shell embedded in its own process and strips
// BASH_ENV, so the neutral route never sees them. While the check is on, Hive
// writes its own omp extension into the agent's 0700 record directory and
// passes it with `--extension`, independently of the advisor's extension.
// The extension listens to omp's `tool_result` event for the bash tool, whose
// `details.exitCode` is the command's exit status (omp 18.13.1), and appends
// one record per command to the agent's raw record file, following the
// record-file protocol: one write on an O_APPEND|O_NOFOLLOW descriptor, a
// shared flock(2) taken through flock(1) when it is installed, and only into
// the file still at the record path. Every failure fails open: no record, the
// tool result is never changed or delayed by an error. omp's own config.yml
// is never touched; with the check off Hive passes no flag and removes the
// file it wrote at the next launch (C14).

const (
	// RepeatFailureRouteOmpExtension names the omp extension in a record's
	// Route.
	RepeatFailureRouteOmpExtension = "omp_extension"
	repeatFailureOmpExtensionName  = "omp_extension.ts"
	// repeatFailureOmpExtensionHeader marks the extension as Hive's, so the
	// launch-time cleanup never removes a file Hive did not write.
	repeatFailureOmpExtensionHeader = "// Hive repeat-failure omp route (hivecommons/hive#14550)."
)

// repeatFailureOmpExtensionPath is the Hive-owned extension inside an agent's
// HOME, next to its raw record file.
func repeatFailureOmpExtensionPath(home string) string {
	return filepath.Join(filepath.Dir(RepeatFailureRecordPath(home)), repeatFailureOmpExtensionName)
}

// renderRepeatFailureOmpExtension renders the extension for one agent with
// the agent name and record file baked in as JSON literals.
func renderRepeatFailureOmpExtension(agentName, recordPath string) []byte {
	agent, _ := json.Marshal(agentName)
	record, _ := json.Marshal(recordPath)
	route, _ := json.Marshal(RepeatFailureRouteOmpExtension)
	return []byte(fmt.Sprintf(repeatFailureOmpExtensionTemplate, repeatFailureOmpExtensionHeader, agent, record, route, RepeatFailureRecordMaxLine))
}

const repeatFailureOmpExtensionTemplate = `%s
// Written by Hive at agent launch and removed when the check is off; do not edit.
import { spawnSync } from "node:child_process";
import { closeSync, constants, fstatSync, lstatSync, openSync, writeSync } from "node:fs";

const AGENT = %s;
const RECORD_PATH = %s;
const ROUTE = %s;
const MAX_LINE = %d;

function recordLine(command, exitCode, failed) {
  let truncated = false;
  for (;;) {
    const rec = { agent: AGENT, backend: "omp", time: new Date().toISOString(), raw_command: command };
    if (exitCode !== undefined) rec.exit_code = exitCode;
    rec.failed = failed;
    rec.route = ROUTE;
    if (truncated) rec.truncated = true;
    const line = JSON.stringify(rec) + "\n";
    if (Buffer.byteLength(line) <= MAX_LINE || command.length <= 1) return line;
    command = command.slice(0, Math.floor(command.length / 2));
    truncated = true;
  }
}

function appendRecord(line) {
  for (let attempt = 0; attempt < 3; attempt++) {
    let fd;
    try {
      fd = openSync(RECORD_PATH, constants.O_WRONLY | constants.O_APPEND | constants.O_CREAT | constants.O_NOFOLLOW, 0o600);
    } catch {
      return;
    }
    try {
      try {
        spawnSync("flock", ["-s", "3"], { stdio: ["ignore", "ignore", "ignore", fd], timeout: 5000 });
      } catch {}
      const held = fstatSync(fd);
      let current;
      try {
        current = lstatSync(RECORD_PATH);
      } catch {
        continue;
      }
      if (held.ino === current.ino && held.dev === current.dev) {
        writeSync(fd, line);
        return;
      }
    } catch {
      return;
    } finally {
      try {
        closeSync(fd);
      } catch {}
    }
  }
}

export default function (pi) {
  pi.on("tool_result", event => {
    try {
      if (!event || event.toolName !== "bash" || !event.input || typeof event.input.command !== "string") return undefined;
      const command = event.input.command;
      if (command.trim() === "") return undefined;
      const code = event.details && Number.isInteger(event.details.exitCode) ? event.details.exitCode : undefined;
      appendRecord(recordLine(command, code, code !== undefined ? code !== 0 : event.isError === true));
    } catch {}
    return undefined;
  });
}
`

// repeatFailureOmpLaunchFlag writes the record extension for an omp launch
// while the check is on and returns the flag that loads it, or removes a file
// Hive wrote earlier and returns "" when the check is off. Best-effort like
// the other launch provisioning: a failure is logged and the agent launches
// without the route.
func (m *Manager) repeatFailureOmpLaunchFlag(agent *AgentProcess, backend string) string {
	if agent == nil || !strings.EqualFold(strings.TrimSpace(backend), "omp") {
		return ""
	}
	home := AgentHome(agent.Name, agent.UID, backend)
	if home == "" {
		return ""
	}
	ext := repeatFailureOmpExtensionPath(home)
	if !m.repeatFailureEnabled() {
		m.removeRepeatFailureFile(agent, ext, repeatFailureOmpExtensionHeader)
		return ""
	}
	recordPath := RepeatFailureRecordPath(home)
	if err := writeRepeatFailureRouteFile(m.agentExecUserSpec(agent), recordPath, ext, renderRepeatFailureOmpExtension(agent.Name, recordPath)); err != nil {
		m.logger.Warn("repeat-failure: writing the omp extension failed; agent launches without it",
			"agent", agent.Name, "path", ext, "error", err)
		return ""
	}
	return " --extension " + shellQuote(ext)
}
