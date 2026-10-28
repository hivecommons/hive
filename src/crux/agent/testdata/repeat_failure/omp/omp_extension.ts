// Hive repeat-failure omp route (hivecommons/hive#14550).
// Written by Hive at agent launch and removed when the check is off; do not edit.
import { spawnSync } from "node:child_process";
import { closeSync, constants, fstatSync, lstatSync, openSync, writeSync } from "node:fs";

const AGENT = "rf-harness";
const RECORD_PATH = "__RF_RECORD_PATH__";
const ROUTE = "omp_extension";
const MAX_LINE = 4096;

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
