// Sandboxed kick execution: sandbox wiring setters, per-agent sandbox
// gating, the sandbox kick runner, and sandbox audit emission.
package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/kubejob"
	"github.com/hivecommons/hive/pkg/pushbroker"
	"github.com/hivecommons/hive/pkg/sandbox"
)

// SetSandboxConfig wires the disabled-by-default sandbox kick executor gate.
func (m *Manager) SetSandboxConfig(cfg config.AgentSandboxConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxConfig = cfg
}

func (m *Manager) SetSandboxLauncher(l sandbox.Launcher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxLauncher = l
}

func (m *Manager) setSandboxJobLauncherFactoryForTest(f func(config.SandboxJobConfig) sandbox.Launcher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxJobLauncherFactory = f
}

// jobLauncherLocked returns the launcher for a sandbox.runtime: job kick:
// the injected factory's, or a real in-cluster kubejob.Launcher.
func (m *Manager) jobLauncherLocked(job config.SandboxJobConfig) sandbox.Launcher {
	if m.sandboxJobLauncherFactory != nil {
		return m.sandboxJobLauncherFactory(job)
	}
	return &kubejob.Launcher{Options: JobOptionsFromConfig(job)}
}

// JobOptionsFromConfig converts the operator's job block into the launcher's
// options. The config package deliberately owns its own types so it does not
// import the launcher; this is the one place the two shapes meet.
func JobOptionsFromConfig(job config.SandboxJobConfig) kubejob.Options {
	opts := kubejob.Options{
		WorkspaceClaim:      job.WorkspaceClaim,
		WorkspaceClaimMount: job.WorkspaceClaimMount,
		NodeSelector:        job.NodeSelector,
		ServiceAccount:      job.ServiceAccount,
		EnvFromSecrets:      append([]string(nil), job.EnvFromSecrets...),
		TTLSeconds:          job.TTLSeconds,
		Resources: kubejob.Resources{
			Limits:   job.Resources.Limits,
			Requests: job.Resources.Requests,
		},
	}
	for _, t := range job.Tolerations {
		opts.Tolerations = append(opts.Tolerations, kubejob.Toleration{Key: t.Key, Operator: t.Operator, Value: t.Value, Effect: t.Effect})
	}
	for _, v := range job.Volumes {
		opts.ExtraVolumes = append(opts.ExtraVolumes, kubejob.VolumeMount{Name: v.Name, Claim: v.Claim, MountPath: v.MountPath, ReadOnly: v.ReadOnly})
	}
	return opts
}

func (m *Manager) setSandboxRunnerForTest(r sandboxCommandRunner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxRunner = r
}

func (m *Manager) SetSandboxPushMinter(minter pushbroker.TokenMinter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxPushMinter = minter
}

func (m *Manager) SetSandboxPRClient(client PRCreator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxPRClient = client
}

func (m *Manager) SetSandboxMutationBoundary(boundary effects.Boundary) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxMutation = boundary
}

func (m *Manager) SetSandboxAuditCallback(fn func(agent, action, detail string)) {
	if fn == nil {
		m.sandboxAuditCallback.Store(nil)
		return
	}
	m.sandboxAuditCallback.Store(&fn)
}

func (m *Manager) agentSandboxEnabledLocked(agent *AgentProcess) bool {
	return agent != nil && agent.Config.SandboxEnabled(m.sandboxConfig)
}

func (m *Manager) startSandboxKickLocked(agent *AgentProcess, message string) error {
	if agent.Paused || agent.State == StatePaused {
		return fmt.Errorf("agent %s cannot be kicked: %s", agent.Name, notRunningReason(agent))
	}
	if agent.State == StateStopped {
		return fmt.Errorf("agent %s cannot be kicked: %s", agent.Name, notRunningReason(agent))
	}
	if agent.State == StateRunning {
		return fmt.Errorf("agent %s cannot be kicked: sandbox execution already running", agent.Name)
	}
	if strings.TrimSpace(m.sandboxConfig.Image) == "" && strings.TrimSpace(agent.Config.SandboxImage(m.sandboxConfig)) == "" {
		return fmt.Errorf("agent %s sandbox image is not configured", agent.Name)
	}
	repo := m.project.PrimaryRepo()
	if repo == "" {
		return fmt.Errorf("agent %s sandbox execution requires a primary repo", agent.Name)
	}
	// Runtime selection (#6311). Resolved BEFORE the agent is marked running,
	// so a misconfigured runtime refuses the kick the same way a missing image
	// does, instead of failing it after the state machine has moved.
	runtime := agent.Config.SandboxRuntime(m.sandboxConfig)
	if !config.ValidSandboxRuntimes[runtime] {
		return fmt.Errorf("agent %s sandbox runtime %q is not one hive has (%s or %s)", agent.Name, runtime, config.SandboxRuntimePodman, config.SandboxRuntimeJob)
	}
	launcher := m.sandboxLauncher
	if runtime == config.SandboxRuntimeJob {
		job := agent.Config.SandboxJob(m.sandboxConfig)
		if strings.TrimSpace(job.WorkspaceClaim) == "" {
			return fmt.Errorf("agent %s sandbox job runtime requires job.workspace_claim — the PVC hive's sandbox workspace root lives on, mounted RWX so the Job can see it from another node", agent.Name)
		}
		launcher = m.jobLauncherLocked(job)
	}
	now := time.Now()
	agent.State = StateRunning
	agent.StartedAt = &now
	agent.LastKick = &now
	agent.LastKickMessage = message
	agent.KickRefused = false
	agent.KickRefusalReason = ""
	agent.LastError = ""
	agent.LaunchedMode = m.agentMode(agent)
	agent.HasLaunched = true
	snippet := truncateStr(message, 120)
	if len(agent.KickHistory) >= kickHistoryCapacity {
		agent.KickHistory = agent.KickHistory[1:]
	}
	agent.KickHistory = append(agent.KickHistory, KickRecord{Timestamp: now, Agent: agent.Name, Snippet: snippet})
	if agent.OutputBuffer != nil {
		agent.OutputBuffer.Write("sandbox kick started (runtime=" + runtime + ")")
	}
	m.recordPrompt(agent.Name, "sandbox-kick", message)
	m.logger.Info("audit: sandbox agent kicked", "name", agent.Name, "repo", repo, "runtime", runtime)

	spec := SandboxKickSpec{
		Agent: agent.Name,
		AgentConfig: configSnapshot{
			Backend:   effectiveBackend(agent),
			Model:     agent.Config.Model,
			LaunchCmd: agent.Config.LaunchCmd,
		},
		Message:      message,
		Org:          m.project.Org,
		Repo:         repo,
		WorkspaceDir: m.sandboxWorkspaceDirLocked(),
		Image:        agent.Config.SandboxImage(m.sandboxConfig),
		EnvAllowlist: agent.Config.SandboxEnvAllowlist(m.sandboxConfig),
		NetworkMode:  agent.Config.SandboxNetworkMode(m.sandboxConfig),
		Timeout:      time.Duration(agent.Config.SandboxTimeoutS(m.sandboxConfig)) * time.Second,
	}
	runCtx, cancel := context.WithCancel(context.Background())
	agent.cancel = cancel
	runner := m.sandboxRunner
	mutationBoundary := m.sandboxMutation
	cloneMinter := m.tieredSandboxMinterLocked(m.agentMode(agent).TokenTier())
	var pushMinter pushbroker.TokenMinter
	var prClient PRCreator
	pushEnabled := false
	if m.agentMode(agent).CanPush() && m.project.PRsAllowed {
		pushMinter = cloneMinter
		prClient = m.sandboxPRClient
		pushEnabled = true
	}
	go m.runSandboxKick(runCtx, agent.Name, spec, launcher, runner, cloneMinter, pushMinter, pushEnabled, prClient, mutationBoundary)
	return nil
}

func (m *Manager) sandboxWorkspaceDirLocked() string {
	if strings.TrimSpace(m.sandboxConfig.WorkspaceDir) != "" {
		return m.sandboxConfig.WorkspaceDir
	}
	return filepath.Join(m.workDir, "sandbox")
}

func (m *Manager) tieredSandboxMinterLocked(tier string) pushbroker.TokenMinter {
	switch minter := m.sandboxPushMinter.(type) {
	case pushbroker.GitHubAppMinter:
		minter.Tier = tier
		return minter
	case *pushbroker.GitHubAppMinter:
		if minter == nil {
			return nil
		}
		cp := *minter
		cp.Tier = tier
		return cp
	default:
		return m.sandboxPushMinter
	}
}

func (m *Manager) runSandboxKick(ctx context.Context, name string, spec SandboxKickSpec, launcher sandbox.Launcher, runner sandboxCommandRunner, cloneMinter, minter pushbroker.TokenMinter, pushEnabled bool, prClient PRCreator, mutationBoundary effects.Boundary) {
	exec := &SandboxExecutor{
		Launcher:    launcher,
		Runner:      runner,
		CloneMinter: cloneMinter,
		Minter:      minter,
		PushEnabled: pushEnabled,
		PRClient:    prClient,
		Mutation:    mutationBoundary,
		Logger:      m.logger,
	}
	res, err := exec.Run(ctx, spec)
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[name]
	if !ok {
		return
	}
	agent.cancel = nil
	if err != nil {
		if agent.sandboxResumeAfterCancel {
			agent.sandboxResumeAfterCancel = false
			agent.State = StateIdle
			agent.LastError = ""
			if agent.OutputBuffer != nil {
				agent.OutputBuffer.Write("sandbox kick cancelled during resume")
			}
			m.auditSandbox(name, "sandbox_cancelled", "sandbox kick cancelled while resuming")
			return
		}
		if agent.Paused || agent.State == StatePaused {
			agent.State = StatePaused
		} else if agent.State != StateStopped {
			agent.State = StateFailed
		}
		agent.LastError = err.Error()
		if agent.OutputBuffer != nil {
			agent.OutputBuffer.Write("sandbox kick failed: " + err.Error())
		}
		m.auditSandbox(name, "sandbox_failed", err.Error())
		if res.Broker != nil && res.Broker.Error != "" {
			m.auditSandbox(name, "sandbox_broker_rejected", res.Broker.Error)
		}
		return
	}
	agent.sandboxResumeAfterCancel = false
	if agent.Paused || agent.State == StatePaused {
		agent.State = StatePaused
	} else if agent.State != StateStopped {
		agent.State = StateIdle
	}
	agent.LastError = ""
	if agent.OutputBuffer != nil {
		msg := fmt.Sprintf("sandbox kick complete: commits=%d", res.CommitCount)
		if res.PR != nil && res.PR.URL != "" {
			msg += " pr=" + res.PR.URL
		}
		agent.OutputBuffer.Write(msg)
	}
	detail := fmt.Sprintf("workspace=%s commits=%d branch=%s", res.Workspace, res.CommitCount, res.Branch)
	if res.PR != nil && res.PR.URL != "" {
		detail += " pr=" + res.PR.URL
	}
	m.auditSandbox(name, "sandbox_complete", detail)
}

func (m *Manager) auditSandbox(agent, action, detail string) {
	if fn := m.sandboxAuditCallback.Load(); fn != nil && *fn != nil {
		(*fn)(agent, action, detail)
	}
}
