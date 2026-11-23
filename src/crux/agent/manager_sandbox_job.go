package agent

import (
	"fmt"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/kubejob"
	"github.com/hivecommons/hive/crux/sandbox"
)

// Audit actions for the Job runtime's infrastructure failures
// (hivecommons/hive#18811). Each detail names the node when it is known.
const (
	auditSandboxNodeUnhealthy      = "sandbox_node_unhealthy"
	auditSandboxNodeRetry          = "sandbox_node_unhealthy_retry"
	auditSandboxWaitingForNode     = "sandbox_waiting_for_node"
	auditSandboxWaitingForNodeFail = "sandbox_waiting_for_node_failed"
	// auditSandboxSetupFailed: a sandbox.setup command failed (#19574).
	auditSandboxSetupFailed = "sandbox_setup_failed"
)

// sandboxPhases holds the live phase of each in-flight sandboxed kick, keyed by
// the agent. It lives beside AgentProcess rather than on it because manager.go
// is at its crux file-size ratchet; every read and write is under m.mu anyway,
// the sync.Map only keeps the package-level table race-free across Managers.
var sandboxPhases sync.Map

func setSandboxPhase(a *AgentProcess, p crux.SandboxPhase) { sandboxPhases.Store(a, p) }

func clearSandboxPhase(a *AgentProcess) { sandboxPhases.Delete(a) }

// sandboxPhaseOf returns a copy of the agent's live phase, or nil.
func sandboxPhaseOf(a *AgentProcess) *crux.SandboxPhase {
	v, ok := sandboxPhases.Load(a)
	if !ok {
		return nil
	}
	p, ok := v.(crux.SandboxPhase)
	if !ok {
		return nil
	}
	return &p
}

// prepareJobKickLocked wires sandbox.preflight, sandbox.setup and the
// scheduling events into the launcher of a job-runtime kick. A test factory's
// launcher is left alone. A preflight or setup configured for another runtime
// is reported and ignored: both only have a meaning on the Job runtime, where
// they run in the Job's own container.
func (m *Manager) prepareJobKickLocked(launcher sandbox.Launcher, agent *AgentProcess, runtime string) {
	preflight := agent.Config.SandboxPreflight(m.sandboxConfig)
	setup := agent.Config.SandboxSetup(m.sandboxConfig)
	if runtime != config.SandboxRuntimeJob {
		if len(preflight.Commands) > 0 {
			m.logger.Warn("sandbox.preflight is set but the runtime is not job; the preflight is ignored", "name", agent.Name, "runtime", runtime)
		}
		if setup.Enabled() {
			m.logger.Warn("sandbox.setup is set but the runtime is not job; the setup step is ignored", "name", agent.Name, "runtime", runtime)
		}
		return
	}
	kl, ok := launcher.(*kubejob.Launcher)
	if !ok {
		return
	}
	kl.Options.Preflight = preflight.Commands
	kl.Options.PreflightTimeout = time.Duration(preflight.TimeoutS) * time.Second
	kl.Options.PreflightTotalTimeout = time.Duration(preflight.TotalTimeoutS) * time.Second
	kl.Options.PreflightRetryDelay = time.Duration(preflight.RetryDelayS) * time.Second
	kl.Options.Setup = setup.Commands
	kl.Options.SetupHealth = setup.HealthCommands
	kl.Options.SetupTimeout = time.Duration(setup.TimeoutS) * time.Second
	kl.Options.SetupTotalTimeout = time.Duration(setup.TotalTimeoutS) * time.Second
	name := agent.Name
	kl.Options.OnEvent = func(e kubejob.Event) { m.onJobEvent(name, e) }
}

// onJobEvent records what the launcher reports while a Job is placed: the
// pod waiting for a node (shown on the dashboard until it lands) and a
// preflight failure that is being retried on another node.
func (m *Manager) onJobEvent(name string, e kubejob.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[name]
	if !ok {
		return
	}
	switch e.Kind {
	case kubejob.EventWaitingForNode:
		setSandboxPhase(agent, crux.SandboxPhase{Phase: sandbox.FailReasonWaitingForNode, Detail: e.Detail, Since: time.Now()})
		if agent.OutputBuffer != nil {
			agent.OutputBuffer.Write("sandbox Job waiting for a node: " + e.Detail)
		}
		m.auditSandbox(name, auditSandboxWaitingForNode, "reason="+e.Detail)
	case kubejob.EventScheduled:
		clearSandboxPhase(agent)
	case kubejob.EventNodeRetry:
		if agent.OutputBuffer != nil {
			agent.OutputBuffer.Write("sandbox node preflight failed on " + e.Node + "; retrying on another node")
		}
		m.auditSandbox(name, auditSandboxNodeRetry, fmt.Sprintf("node=%s reason=%s", e.Node, e.Detail))
	}
}

// sandboxInfraFailureLocked handles a kick that failed for the infrastructure's
// reason (node_unhealthy, waiting_for_node): the agent never ran, so it goes
// back to Idle with the reason, not to Failed, and nothing that watches Failed
// agents (restart, escalation, a failure-fix loop) sees it. The next cadence
// tries again. It reports whether it handled the failure.
func (m *Manager) sandboxInfraFailureLocked(agent *AgentProcess, name string, res SandboxKickResult, err error) bool {
	reason := res.Sandbox.FailReason
	if reason == "" {
		return false
	}
	if reason == sandbox.FailReasonSetupFailed {
		// The setup step's failure is the kick's (the repo's script, its
		// mirror): it fails like any other kick, with the reason recorded
		// and audited (#19574).
		m.auditSandbox(name, auditSandboxSetupFailed, fmt.Sprintf("node=%s reason=%s workspace=%s", res.Sandbox.Node, err.Error(), res.Workspace))
		return false
	}
	if agent.Paused || agent.State == StatePaused {
		agent.State = StatePaused
	} else if agent.State != StateStopped {
		agent.State = StateIdle
	}
	agent.LastError = err.Error()
	if agent.OutputBuffer != nil {
		agent.OutputBuffer.Write("sandbox kick not run: " + err.Error())
	}
	action := auditSandboxNodeUnhealthy
	if reason == sandbox.FailReasonWaitingForNode {
		action = auditSandboxWaitingForNodeFail
	}
	m.auditSandbox(name, action, fmt.Sprintf("node=%s reason=%s workspace=%s", res.Sandbox.Node, err.Error(), res.Workspace))
	return true
}
