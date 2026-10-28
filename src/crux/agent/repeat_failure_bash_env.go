package agent

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Backend-neutral route for the repeat-failure check (hivecommons/hive#14550,
// C2/C8/C14; issue #14554).
//
// Backends that run every shell command in a fresh, non-interactive bash
// (`bash -c`, `bash -lc`) all read the file named by BASH_ENV before the
// command. When the check is on, Hive points BASH_ENV at a Hive-owned script
// in the agent's record directory. The script installs one EXIT trap that
// appends a RepeatFailureRecord for the command (`$?` and
// `$BASH_EXECUTION_STRING`) to the agent's raw record file.
//
// Only the agent's own command is recorded (C8): the trap is installed only
// when BASH_EXECUTION_STRING is non-empty (so scripts are skipped) and the
// marker variable is unset or holds the current shell's PID. The first `-c`
// shell exports the marker with its own PID, so every nested `bash -c` sees a
// foreign PID and records nothing, while a wrapper that ends in
// `exec /bin/bash -c '<cmd>'` (Codex) keeps its PID across the exec and so
// records `<cmd>` exactly once (the outer trap never fires across exec).
//
// The script is written for callers that start bash with `-euo pipefail` or
// export SHELLOPTS: the only thing it leaves visible is the trap and the
// marker. The trap body runs in a subshell with its own options and stderr
// sent to /dev/null, and ends in `|| :`, so a missing or unwritable record
// file means no record and nothing else; the command's exit status is
// untouched because bash keeps the original status unless an EXIT trap calls
// exit itself. A command that sets its own EXIT trap replaces Hive's and is
// not recorded (accepted; the not-receiving-data safeguard catches systematic
// loss).
//
// An existing BASH_ENV in Hive's environment is chained, not overwritten: its
// path is baked into the script, which sources it after installing the trap.
// The trap comes first so a chained file that sets its own EXIT trap keeps its
// behaviour (and only that command goes unrecorded).
//
// Turning the check off removes the script at the agent's next launch; a
// BASH_ENV left in a long-lived tmux session then names a missing file, which
// bash skips silently.

const (
	// RepeatFailureRouteBashEnv names the neutral route in a record's Route.
	RepeatFailureRouteBashEnv = "bash_env"
	// repeatFailureMarkerEnv is the one namespaced variable the route adds
	// to a command's environment: the PID of the shell that owns the record.
	repeatFailureMarkerEnv = "__HIVE_RF_PID"
	// repeatFailureShell is exported as SHELL while the check is on so
	// backends that pick their shell from $SHELL (bob, opencode, kilo) run
	// commands in bash, which reads BASH_ENV; bob falls back to /bin/sh
	// (dash in the agent image) otherwise.
	repeatFailureShell       = "/bin/bash"
	repeatFailureBashEnvName = "bash_env.sh"
	// repeatFailureBashEnvHeader marks a file as Hive's, so launch-time
	// cleanup never removes a file Hive did not write.
	repeatFailureBashEnvHeader = "# Hive repeat-failure BASH_ENV route (hivecommons/hive#14550)."
	// repeatFailureBashEnvSlack covers the record fields the script fills at
	// run time: time (25 bytes), exit code (3), failed (5), the truncated
	// flag (17) and the closing brace and newline, with room to spare.
	repeatFailureBashEnvSlack = 64
	// repeatFailureBashEnvMinCommand is the smallest command budget the
	// script is given, whatever the agent and backend names cost.
	repeatFailureBashEnvMinCommand = 256
)

// repeatFailureNeutralBackends run each shell command in a fresh
// non-interactive bash, so the BASH_ENV route sees their commands. The
// inference backends drive Claude Code and are covered through it.
var repeatFailureNeutralBackends = map[string]bool{
	"claude":   true,
	"codex":    true,
	"goose":    true,
	"pi":       true,
	"gemini":   true,
	bobBackend: true,
	"opencode": true,
	"kilo":     true,
}

// SetRepeatFailureEnabledResolver injects the live repeat_failure.enabled
// switch. Called from main.go with a closure over the live config so a toggle
// applies at each agent's next launch without a restart. A nil fn leaves the
// check off.
func (m *Manager) SetRepeatFailureEnabledResolver(fn func() bool) {
	if fn == nil {
		m.repeatFailureEnabledResolver.Store(nil)
		return
	}
	m.repeatFailureEnabledResolver.Store(&fn)
}

// repeatFailureEnabled reports whether the check is on. Lock-free, so the
// launch and env paths may call it while holding m.mu.
func (m *Manager) repeatFailureEnabled() bool {
	fnp := m.repeatFailureEnabledResolver.Load()
	if fnp == nil || *fnp == nil {
		return false
	}
	return (*fnp)()
}

// repeatFailureBashEnvActive reports whether the neutral route applies to a
// launch on backend.
func (m *Manager) repeatFailureBashEnvActive(backend string) bool {
	if !m.repeatFailureEnabled() {
		return false
	}
	backend = strings.ToLower(strings.TrimSpace(backend))
	return repeatFailureNeutralBackends[backend] || IsInferenceBackend(backend) || m.routableBackend(backend)
}

// repeatFailureBashEnvPath is the Hive-owned BASH_ENV script inside an agent's
// HOME, next to its raw record file.
func repeatFailureBashEnvPath(home string) string {
	return filepath.Join(filepath.Dir(RepeatFailureRecordPath(home)), repeatFailureBashEnvName)
}

// repeatFailureEnvPairs returns BASH_ENV and SHELL for a launch the neutral
// route applies to, only SHELL for the aider route (aider runs commands in
// `$SHELL -i -c`, which must be bash to read ~/.bashrc), and nothing otherwise
// (C14: off adds nothing).
func (m *Manager) repeatFailureEnvPairs(agent *AgentProcess, backend string) []agentEnvPair {
	if m.repeatFailureAiderBashrcActive(agent, backend) {
		return []agentEnvPair{{"SHELL", repeatFailureShell, false}}
	}
	if agent == nil || !m.repeatFailureBashEnvActive(backend) {
		return nil
	}
	home := AgentHome(agent.Name, agent.UID, backend)
	return []agentEnvPair{
		{"BASH_ENV", repeatFailureBashEnvPath(home), false},
		{"SHELL", repeatFailureShell, false},
	}
}

// repeatFailureChainedBashEnv is the BASH_ENV the agent would have had
// without the check: the one in Hive's own environment, which the tmux
// session inherits, unless it already is Hive's script.
func repeatFailureChainedBashEnv(script string) string {
	existing := strings.TrimSpace(os.Getenv("BASH_ENV"))
	if existing == "" || filepath.Clean(existing) == filepath.Clean(script) {
		return ""
	}
	return existing
}

// renderRepeatFailureBashEnv renders the BASH_ENV script for one agent, with
// route naming the producer in each record (the aider route sources the same
// script from ~/.bashrc). All values are baked in as quoted literals so the
// script reads no variable beyond bash's own and the marker.
func renderRepeatFailureBashEnv(agentName, backend, route, recordPath, chain string) []byte {
	agentJSON, _ := json.Marshal(agentName)
	backendJSON, _ := json.Marshal(backend)
	routeJSON, _ := json.Marshal(route)
	head := `{"agent":` + string(agentJSON) + `,"backend":` + string(backendJSON) + `,"time":"`
	tail := `,"route":` + string(routeJSON)
	fixed := len(head) + len(`","raw_command":"`) + len(`","exit_code":`) + len(`,"failed":`) + len(tail)
	budget := max(RepeatFailureRecordMaxLine-fixed-repeatFailureBashEnvSlack, repeatFailureBashEnvMinCommand)

	// The trap body. It runs in a subshell so nothing it does (options,
	// variables, fds, a failing redirection) reaches the exiting shell, and
	// `$?` inside the subshell is still the command's status. Escaping runs
	// in the C locale so lengths are bytes; the command is cut before
	// escaping (escaping at most doubles it), so no escape is ever split.
	// The write follows the record-file protocol: a shared flock while
	// writing, and only to the file still at the record path.
	body := strings.Join([]string{
		`( r=$?`,
		`set +euxEo pipefail`,
		`trap - ERR`,
		`umask 077`,
		`LC_ALL=C`,
		`f=` + shellQuote(recordPath),
		`h=` + shellQuote(head),
		`m=` + strconv.Itoa(budget),
		`s=$BASH_EXECUTION_STRING`,
		`t=`,
		`while :; do`,
		`e=${s//\\/\\\\}`,
		`e=${e//\"/\\\"}`,
		`e=${e//$'\n'/\\n}`,
		`e=${e//$'\r'/\\r}`,
		`e=${e//$'\t'/\\t}`,
		`e=${e//[[:cntrl:]]/ }`,
		`(( ${#e} <= m )) && break`,
		`s=${s:0:m/2}`,
		`t=',"truncated":true'`,
		`done`,
		`x=false`,
		`(( r )) && x=true`,
		`printf -v d '%(%Y-%m-%dT%H:%M:%S%z)T' -1`,
		`d=${d:0:22}:${d:22}`,
		`l="$h$d\",\"raw_command\":\"$e\",\"exit_code\":$r,\"failed\":$x"` + shellQuote(tail) + `"$t}"`,
		`[[ -L $f ]] && exit`,
		`for i in 1 2 3; do`,
		`exec {w}>>"$f" || exit`,
		`command -v flock >/dev/null && flock -s "$w"`,
		`if [[ /dev/fd/$w -ef $f ]]; then printf '%s\n' "$l" >&"$w"; exit; fi`,
		`exec {w}>&-`,
		`done`,
		`) 2>/dev/null || :`,
	}, "\n")

	var b strings.Builder
	b.WriteString(repeatFailureBashEnvHeader + "\n")
	b.WriteString("# Written by Hive at agent launch and removed when the check is off; do not edit.\n")
	b.WriteString(`if [[ -n ${BASH_EXECUTION_STRING-} && ( -z ${` + repeatFailureMarkerEnv + `-} || ${` + repeatFailureMarkerEnv + `-} == "$$" ) ]]; then` + "\n")
	b.WriteString("export " + repeatFailureMarkerEnv + "=$$\n")
	b.WriteString("trap -- " + shellQuote(body) + " EXIT\n")
	b.WriteString("fi\n")
	if chain != "" {
		q := shellQuote(chain)
		b.WriteString("if [[ -r " + q + " ]]; then . " + q + "; fi\n")
	}
	return []byte(b.String())
}

// provisionRepeatFailureBashEnv writes the agent's BASH_ENV script when the
// neutral route or the aider route applies to this launch, or removes a script Hive wrote
// earlier when it no longer does, so turning the check off needs nothing but
// the next launch. Best-effort like the other launch provisioning: a failure
// is logged and the agent launches without a record route (BASH_ENV then
// names a missing file, which bash skips silently).
func (m *Manager) provisionRepeatFailureBashEnv(agent *AgentProcess, backend string) {
	if agent == nil {
		return
	}
	home := AgentHome(agent.Name, agent.UID, backend)
	if home == "" {
		return
	}
	script := repeatFailureBashEnvPath(home)
	route, chain := "", ""
	switch {
	case m.repeatFailureBashEnvActive(backend):
		route, chain = RepeatFailureRouteBashEnv, repeatFailureChainedBashEnv(script)
	case m.repeatFailureAiderBashrcActive(agent, backend):
		route = RepeatFailureRouteAiderBashrc
	default:
		m.removeRepeatFailureBashEnv(agent, script)
		return
	}
	recordPath := RepeatFailureRecordPath(home)
	content := renderRepeatFailureBashEnv(agent.Name, backend, route, recordPath, chain)
	if err := writeRepeatFailureBashEnv(m.agentExecUserSpec(agent), recordPath, script, content); err != nil {
		m.logger.Warn("repeat-failure: writing the BASH_ENV route failed; agent launches without it",
			"agent", agent.Name, "path", script, "error", err)
	}
}

// writeRepeatFailureBashEnv creates the 0700 record directory and writes the
// script into it, as the agent's user when there is one (the same su-exec
// discipline as the advisor hook files) and directly otherwise.
func writeRepeatFailureBashEnv(userSpec, recordPath, script string, content []byte) error {
	if userSpec == "" {
		if err := EnsureRepeatFailureRecordDir(recordPath, 0); err != nil {
			return err
		}
		return writeRepeatFailureBashEnvFile(script, content)
	}
	dir := filepath.Dir(recordPath)
	if out, err := exec.Command("su-exec", userSpec, "sh", "-c", `umask 077 && mkdir -p "$1" && chmod 700 "$1"`, "sh", dir).CombinedOutput(); err != nil {
		return outputErr("creating "+dir+" as "+userSpec, err, out)
	}
	return writeFileAsUser(userSpec, script, content)
}

// writeRepeatFailureBashEnvFile replaces script atomically, never following a
// planted link.
func writeRepeatFailureBashEnvFile(script string, content []byte) error {
	tmp := script + ".new"
	_ = os.Remove(tmp)
	if err := writeFileNoFollow(tmp, content, repeatFailureRecordFileMode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, script); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func writeFileNoFollow(path string, content []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// removeRepeatFailureBashEnv deletes a script Hive wrote (recognised by its
// header) and drops the BASH_ENV Hive put into an existing tmux session, so a
// switched-off check leaves nothing behind at the next launch.
func (m *Manager) removeRepeatFailureBashEnv(agent *AgentProcess, script string) {
	userSpec := m.agentExecUserSpec(agent)
	var existing []byte
	var err error
	if userSpec == "" {
		existing, err = os.ReadFile(script)
	} else {
		existing, err = readFileAsUser(userSpec, script)
	}
	if err != nil || !strings.HasPrefix(string(existing), repeatFailureBashEnvHeader) {
		return
	}
	if userSpec == "" {
		err = os.Remove(script)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		err = exec.Command("su-exec", userSpec, "rm", "-f", script).Run()
	}
	if err != nil {
		m.logger.Warn("repeat-failure: removing the stale BASH_ENV route failed", "agent", agent.Name, "path", script, "error", err)
	}
	m.clearRepeatFailureSessionEnv(agent, script)
}

// clearRepeatFailureSessionEnv removes BASH_ENV from the agent's tmux session
// environment when it still names Hive's script, so CLIs started from a
// respawned pane no longer inherit it. A pane shell that already holds the
// variable keeps it until the session is recreated; it then names a missing
// file and does nothing.
func (m *Manager) clearRepeatFailureSessionEnv(agent *AgentProcess, script string) {
	if agent.tmuxSession == "" {
		return
	}
	out, err := m.tmuxCmd(agent, "show-environment", "-t", agent.tmuxSession, "BASH_ENV").Output()
	if err != nil || strings.TrimSpace(string(out)) != "BASH_ENV="+script {
		return
	}
	_ = m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, "-r", "BASH_ENV").Run()
}
