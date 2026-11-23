package kubejob

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/sandbox"
)

// Workspace setup step (hivecommons/hive#19574).
//
// sandbox.setup runs in the Job's own container after the node preflight and
// before the agent, with the workspace hive checked out as its working
// directory. The container command is, outermost first:
//
//	relay check -> preflight commands -> setup commands -> setup health commands -> agent
//
// each stage exec'ing the next. A failed setup command prints setupMarker and
// exits ExitCodeSetupFailed: the kick fails as setup_failed and is not retried
// on another node, because the cause is the install script or its mirror. A
// failed health command prints setupHealthMarker and exits
// ExitCodeNodeUnhealthy: the node cannot run what was installed, so the kick
// takes the preflight's node_unhealthy path and is retried once elsewhere.
//
// As with the preflight, the commands come from configuration only, are part of
// the Job manifest and print into the pod log, and the script itself prints
// nothing from the environment.

const (
	// DefaultSetupTimeout bounds one setup command.
	DefaultSetupTimeout = time.Duration(config.DefaultSandboxSetupTimeoutS) * time.Second
	// DefaultSetupTotalTimeout bounds both setup lists together.
	DefaultSetupTotalTimeout = time.Duration(config.DefaultSandboxSetupTotalTimeoutS) * time.Second

	// ExitCodeSetupFailed is the container exit code of a failed setup command.
	ExitCodeSetupFailed = sandbox.ExitCodeSetupFailed

	// setupMarker starts the line the step prints when a setup command fails.
	setupMarker = "hive-sandbox-setup: FAILED"
	// setupHealthMarker starts the line it prints when a health command fails.
	setupHealthMarker = "hive-sandbox-setup-health: FAILED"
	// setupScriptName is $0 of the step's shell.
	setupScriptName = "hive-sandbox-setup"
)

// setupScript takes four leading arguments (per-command seconds, total
// seconds, setup command count, health command count), then the setup
// commands, the health commands and the agent command. Each command runs with
// `sh -lc` under a watchdog, as the preflight's do; both lists share the total.
var setupScript = strings.NewReplacer(
	"@SMARK@", setupMarker,
	"@SEXIT@", strconv.Itoa(ExitCodeSetupFailed),
	"@HMARK@", setupHealthMarker,
	"@HEXIT@", strconv.Itoa(ExitCodeNodeUnhealthy),
	"@GRACE@", strconv.Itoa(preflightKillGraceS),
).Replace(`per="$1"; total="$2"; ns="$3"; nh="$4"; shift 4
start=$(date +%s 2>/dev/null || echo 0)
step() {
  marker="$1"; code="$2"; label="$3"; si="$4"; sn="$5"; cmd="$6"
  now=$(date +%s 2>/dev/null || echo 0)
  left=$((total-(now-start)))
  if [ "$left" -le 0 ]; then
    echo "$marker the ${total}s total timeout ran out before $label command $si of $sn: $cmd" >&2
    exit "$code"
  fi
  t=$per
  [ "$left" -lt "$t" ] && t=$left
  echo "hive-sandbox-setup: $label command $si of $sn: $cmd" >&2
  sh -lc "$cmd" &
  pid=$!
  ( sleep "$t"; kill -TERM "$pid"; sleep @GRACE@; kill -KILL "$pid" ) >/dev/null 2>&1 </dev/null &
  wd=$!
  wait "$pid"
  rc=$?
  kill "$wd" >/dev/null 2>&1
  if [ "$rc" -ne 0 ]; then
    if [ "$rc" -eq 143 ] || [ "$rc" -eq 137 ]; then why="timed out after ${t}s"; else why="exited $rc"; fi
    echo "$marker $label command $si of $sn $why: $cmd" >&2
    exit "$code"
  fi
}
i=0
while [ "$i" -lt "$ns" ]; do
  i=$((i+1)); c="$1"; shift
  step "@SMARK@" @SEXIT@ setup "$i" "$ns" "$c"
done
i=0
while [ "$i" -lt "$nh" ]; do
  i=$((i+1)); c="$1"; shift
  step "@HMARK@" @HEXIT@ health "$i" "$nh" "$c"
done
exec "$@"`)

// trimmedCommands returns cmds with blanks dropped.
func trimmedCommands(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// setupEnabled reports whether the step has anything to run.
func (o Options) setupEnabled() bool {
	return len(trimmedCommands(o.Setup)) > 0 || len(trimmedCommands(o.SetupHealth)) > 0
}

// setupCommand wraps the agent command so the setup step runs first. With no
// setup configured, or no command to wrap, it returns command unchanged, so the
// Job manifest of a hive that never sets sandbox.setup is byte-identical to
// what it was.
func setupCommand(command []string, o Options) []string {
	setup, health := trimmedCommands(o.Setup), trimmedCommands(o.SetupHealth)
	if len(setup)+len(health) == 0 || len(command) == 0 {
		return command
	}
	per := durationSeconds(o.SetupTimeout, DefaultSetupTimeout)
	total := durationSeconds(o.SetupTotalTimeout, DefaultSetupTotalTimeout)
	out := []string{preflightShell, "-c", setupScript, setupScriptName, strconv.Itoa(per), strconv.Itoa(total), strconv.Itoa(len(setup)), strconv.Itoa(len(health))}
	out = append(out, setup...)
	out = append(out, health...)
	return append(out, command...)
}

// validateSetup reports setup settings that could not run.
func (o Options) validateSetup() error {
	for _, list := range []struct {
		name string
		cmds []string
	}{{"setup", o.Setup}, {"setup health", o.SetupHealth}} {
		if len(list.cmds) > config.MaxSandboxSetupCommands {
			return fmt.Errorf("kubejob: %s takes at most %d commands, got %d", list.name, config.MaxSandboxSetupCommands, len(list.cmds))
		}
		for i, c := range list.cmds {
			if strings.TrimSpace(c) == "" {
				return fmt.Errorf("kubejob: %s[%d] must not be empty", list.name, i)
			}
		}
	}
	if o.SetupTimeout < 0 || o.SetupTotalTimeout < 0 {
		return errors.New("kubejob: setup timeouts must not be negative")
	}
	return nil
}

// markedFailureLine returns the last line of stdout that starts with marker,
// bounded to preflightMessageMax runes, and its offset; -1 when absent.
func markedFailureLine(stdout, marker string) (string, int) {
	i := strings.LastIndex(stdout, marker)
	if i < 0 {
		return "", -1
	}
	line, _, _ := strings.Cut(stdout[i:], "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > preflightMessageMax {
		line = string(r[:preflightMessageMax]) + "..."
	}
	return line, i
}

// setupFailure reports whether a failed run is a failed setup command, and the
// failure line it printed. As for the preflight, the exit code alone is not
// enough: the marker must be there too.
func (o Options) setupFailure(res sandbox.Result) (string, bool) {
	if len(trimmedCommands(o.Setup)) == 0 || res.ExitCode != ExitCodeSetupFailed {
		return "", false
	}
	line, at := markedFailureLine(res.Stdout, setupMarker)
	return line, at >= 0
}
