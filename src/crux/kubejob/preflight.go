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

// Node preflight and fail-fast scheduling (hivecommons/hive#18811).
//
// A sandbox.preflight command list runs in the Job's own container, after the
// relay check and before the agent. The first failure exits the container with
// sandbox.ExitCodeNodeUnhealthy after printing preflightMarker, so the Job
// fails in seconds and Run can tell it from an agent that happened to exit 75.
// Run then re-creates the Job once with a node-affinity NotIn for the node the
// failed pod ran on (maxPreflightRetries), instead of waiting for the agent's
// next cadence. Nodes are never pinned: the exclusion is a NotIn on the
// kubernetes.io/hostname label added to the operator's own affinity.
//
// The commands come from configuration only. They are in the Job manifest and
// their output is in the pod log, so none of them may carry a credential on its
// command line; the script itself prints nothing from the environment.

const (
	// DefaultPreflightTimeout bounds one preflight command; the config
	// default is config.DefaultSandboxPreflightTimeoutS.
	DefaultPreflightTimeout = time.Duration(config.DefaultSandboxPreflightTimeoutS) * time.Second
	// DefaultPreflightTotalTimeout bounds all preflight commands together.
	DefaultPreflightTotalTimeout = time.Duration(config.DefaultSandboxPreflightTotalTimeoutS) * time.Second
	// DefaultScheduleTimeout is how long a pod may stay Unschedulable.
	DefaultScheduleTimeout = time.Duration(config.DefaultSandboxScheduleTimeoutS) * time.Second

	// ExitCodeNodeUnhealthy is the container exit code of a failed preflight.
	ExitCodeNodeUnhealthy = sandbox.ExitCodeNodeUnhealthy

	// maxPreflightRetries is how many times a node_unhealthy kick is
	// re-created on another node: once.
	maxPreflightRetries = 1
	// preflightMarker starts the line the preflight prints when it fails.
	preflightMarker = "hive-node-preflight: FAILED"
	// hostnameLabel is the node label the retry's NotIn matches.
	hostnameLabel = "kubernetes.io/hostname"
	// preflightMessageMax bounds the failure line quoted in the error.
	preflightMessageMax = 500
	preflightShell      = "/bin/sh"
	// preflightKillGraceS is the pause between SIGTERM and SIGKILL for a
	// preflight command that outlives its timeout.
	preflightKillGraceS = 2
)

// preflightScript runs the commands passed after the three leading arguments
// (per-command seconds, total seconds, command count) one by one with `sh -lc`,
// as publish.verify does, each under a watchdog, then execs the agent command.
var preflightScript = strings.NewReplacer(
	"@MARKER@", preflightMarker,
	"@EXIT@", strconv.Itoa(ExitCodeNodeUnhealthy),
	"@GRACE@", strconv.Itoa(preflightKillGraceS),
).Replace(`per="$1"; total="$2"; n="$3"; shift 3
start=$(date +%s 2>/dev/null || echo 0)
i=0
while [ "$i" -lt "$n" ]; do
  i=$((i+1))
  cmd="$1"; shift
  now=$(date +%s 2>/dev/null || echo 0)
  left=$((total-(now-start)))
  if [ "$left" -le 0 ]; then
    echo "@MARKER@ the ${total}s total timeout ran out before command $i of $n: $cmd" >&2
    exit @EXIT@
  fi
  t=$per
  [ "$left" -lt "$t" ] && t=$left
  sh -lc "$cmd" &
  pid=$!
  ( sleep "$t"; kill -TERM "$pid"; sleep @GRACE@; kill -KILL "$pid" ) >/dev/null 2>&1 </dev/null &
  wd=$!
  wait "$pid"
  rc=$?
  kill "$wd" >/dev/null 2>&1
  if [ "$rc" -ne 0 ]; then
    if [ "$rc" -eq 143 ] || [ "$rc" -eq 137 ]; then why="timed out after ${t}s"; else why="exited $rc"; fi
    echo "@MARKER@ command $i of $n $why: $cmd" >&2
    exit @EXIT@
  fi
done
exec "$@"`)

// preflightCommands returns the configured commands, blanks dropped.
func (o Options) preflightCommands() []string { return trimmedCommands(o.Preflight) }

// preflightCommand wraps the agent command so the preflight runs first. With no
// preflight configured, or no command to wrap, it returns command unchanged, so
// the Job manifest of a hive that never sets sandbox.preflight is byte-identical
// to what it was.
func preflightCommand(command []string, o Options) []string {
	cmds := o.preflightCommands()
	if len(cmds) == 0 || len(command) == 0 {
		return command
	}
	per := durationSeconds(o.PreflightTimeout, DefaultPreflightTimeout)
	total := durationSeconds(o.PreflightTotalTimeout, DefaultPreflightTotalTimeout)
	out := []string{preflightShell, "-c", preflightScript, "hive-node-preflight", strconv.Itoa(per), strconv.Itoa(total), strconv.Itoa(len(cmds))}
	out = append(out, cmds...)
	return append(out, command...)
}

// durationSeconds is d in whole seconds (at least one), or def's when d is unset.
func durationSeconds(d, def time.Duration) int {
	if d <= 0 {
		d = def
	}
	if s := int(d / time.Second); s > 0 {
		return s
	}
	return 1
}

// scheduleTimeout is Options.ScheduleTimeout with the default applied.
func (o Options) scheduleTimeout() time.Duration {
	if o.ScheduleTimeout > 0 {
		return o.ScheduleTimeout
	}
	return DefaultScheduleTimeout
}

// validatePreflight reports the preflight and schedule-timeout settings that
// could not run.
func (o Options) validatePreflight() error {
	if len(o.Preflight) > config.MaxSandboxPreflightCommands {
		return fmt.Errorf("kubejob: preflight takes at most %d commands, got %d", config.MaxSandboxPreflightCommands, len(o.Preflight))
	}
	for i, c := range o.Preflight {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("kubejob: preflight[%d] must not be empty", i)
		}
	}
	for _, f := range []struct {
		name string
		d    time.Duration
	}{{"preflight timeout", o.PreflightTimeout}, {"preflight total timeout", o.PreflightTotalTimeout}, {"preflight retry delay", o.PreflightRetryDelay}, {"schedule timeout", o.ScheduleTimeout}} {
		if f.d < 0 {
			return errors.New("kubejob: " + f.name + " must not be negative")
		}
	}
	return nil
}

// preflightFailure reports whether a failed run failed a node check: the
// preflight, or a sandbox.setup health command (#19574), which takes the same
// node_unhealthy path. It returns the failure line printed. The exit code alone
// is not enough: the agent may exit 75 for its own reasons, so the marker the
// script prints must be there too. With both markers present the later wins.
func (o Options) preflightFailure(res sandbox.Result) (string, bool) {
	if res.ExitCode != ExitCodeNodeUnhealthy {
		return "", false
	}
	line, at := "", -1
	if len(o.preflightCommands()) > 0 {
		line, at = markedFailureLine(res.Stdout, preflightMarker)
	}
	if len(trimmedCommands(o.SetupHealth)) > 0 {
		if l, i := markedFailureLine(res.Stdout, setupHealthMarker); i > at {
			line, at = l, i
		}
	}
	return line, at >= 0
}

// avoidNodes makes the Job's pod template refuse the given nodes: a NotIn on the
// hostname label, ANDed into every required node-selector term (terms are ORed,
// so adding it to one would let the pod through another). The operator's own
// affinity is copied, never mutated, because it is shared across kicks.
func avoidNodes(manifest map[string]any, affinity *config.SandboxJobAffinity, nodes []string) {
	if len(nodes) == 0 {
		return
	}
	jobSpec, _ := manifest["spec"].(map[string]any)
	template, _ := jobSpec["template"].(map[string]any)
	podSpec, _ := template["spec"].(map[string]any)
	if podSpec == nil {
		return
	}
	podSpec["affinity"] = affinityAvoiding(affinity, nodes)
}

func affinityAvoiding(in *config.SandboxJobAffinity, nodes []string) *config.SandboxJobAffinity {
	var node config.SandboxJobNodeAffinity
	if in != nil && in.NodeAffinity != nil {
		node = *in.NodeAffinity
	}
	avoid := config.SandboxJobNodeSelectorRequirement{Key: hostnameLabel, Operator: "NotIn", Values: append([]string(nil), nodes...)}
	var terms []config.SandboxJobNodeSelectorTerm
	if req := node.RequiredDuringSchedulingIgnoredDuringExecution; req != nil {
		for _, t := range req.NodeSelectorTerms {
			exprs := append(append([]config.SandboxJobNodeSelectorRequirement(nil), t.MatchExpressions...), avoid)
			terms = append(terms, config.SandboxJobNodeSelectorTerm{MatchExpressions: exprs})
		}
	}
	if len(terms) == 0 {
		terms = []config.SandboxJobNodeSelectorTerm{{MatchExpressions: []config.SandboxJobNodeSelectorRequirement{avoid}}}
	}
	node.RequiredDuringSchedulingIgnoredDuringExecution = &config.SandboxJobNodeSelector{NodeSelectorTerms: terms}
	return &config.SandboxJobAffinity{NodeAffinity: &node}
}

// Event kinds Options.OnEvent receives.
const (
	// EventWaitingForNode: the pod is Unschedulable; Detail is the scheduler's message.
	EventWaitingForNode = "waiting_for_node"
	// EventScheduled: the pod no longer waits for a node; Node is where it landed.
	EventScheduled = "scheduled"
	// EventNodeRetry: a preflight failed on Node and the Job is being
	// re-created on another; Detail is the preflight's failure line.
	EventNodeRetry = "node_retry"
)

// Event is one thing worth surfacing while a Job runs.
type Event struct {
	Kind   string
	Node   string
	Detail string
}

func (l *Launcher) emit(e Event) {
	if l.Options.OnEvent != nil {
		l.Options.OnEvent(e)
	}
}
