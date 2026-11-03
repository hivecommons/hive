package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/crux/kubejob"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/crux/sandbox"
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

// SandboxGitHubWiring returns the PR client and push minter the sandbox kick
// executor will use next. The hive re-points both whenever it rebuilds its
// GitHub client (#9621), so a sandbox kick never opens a PR or mints a push
// token through a client or App credential that has been replaced.
func (m *Manager) SandboxGitHubWiring() (PRCreator, pushbroker.TokenMinter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sandboxPRClient, m.sandboxPushMinter
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

func (m *Manager) validateSandboxKickLocked(agent *AgentProcess) (string, error) {
	if agent.Paused || agent.State == StatePaused {
		return "", fmt.Errorf("agent %s cannot be kicked: %s", agent.Name, notRunningReason(agent))
	}
	if agent.State == StateStopped {
		return "", fmt.Errorf("agent %s cannot be kicked: %s", agent.Name, notRunningReason(agent))
	}
	if agent.State == StateRunning {
		return "", fmt.Errorf("agent %s cannot be kicked: sandbox execution already running", agent.Name)
	}
	if strings.TrimSpace(m.sandboxConfig.Image) == "" && strings.TrimSpace(agent.Config.SandboxImage(m.sandboxConfig)) == "" {
		return "", fmt.Errorf("agent %s sandbox image is not configured", agent.Name)
	}
	repo := m.project.PrimaryRepo()
	if repo == "" {
		return "", fmt.Errorf("agent %s sandbox execution requires a primary repo", agent.Name)
	}
	// Runtime selection (#6311). Resolved BEFORE the agent is marked running,
	// so a misconfigured runtime refuses the kick the same way a missing image
	// does, instead of failing it after the state machine has moved.
	runtime := agent.Config.SandboxRuntime(m.sandboxConfig)
	if !config.ValidSandboxRuntimes[runtime] {
		return "", fmt.Errorf("agent %s sandbox runtime %q is not one hive has (%s or %s)", agent.Name, runtime, config.SandboxRuntimePodman, config.SandboxRuntimeJob)
	}
	if runtime == config.SandboxRuntimeJob {
		job := agent.Config.SandboxJob(m.sandboxConfig)
		if strings.TrimSpace(job.WorkspaceClaim) == "" {
			return "", fmt.Errorf("agent %s sandbox job runtime requires job.workspace_claim — the PVC hive's sandbox workspace root lives on, mounted RWX so the Job can see it from another node", agent.Name)
		}
	}
	return runtime, nil
}

func (m *Manager) startSandboxKickLocked(agent *AgentProcess, message, source string) error {
	runtime, err := m.validateSandboxKickLocked(agent)
	if err != nil {
		return err
	}
	repo := m.project.PrimaryRepo()
	launcher := m.sandboxLauncher
	if runtime == config.SandboxRuntimeJob {
		job := agent.Config.SandboxJob(m.sandboxConfig)
		launcher = m.jobLauncherLocked(job)
	}
	now := time.Now()
	agent.State = StateRunning
	agent.StartedAt = &now
	agent.LastKick = &now
	agent.LastKickMessage = message
	agent.LastKickSource = source
	agent.KickRefused = false
	agent.KickRefusalReason = ""
	agent.LastError = ""
	agent.LaunchedMode = m.agentMode(agent)
	agent.HasLaunched = true
	snippet := truncateStr(message, 120)
	if len(agent.KickHistory) >= kickHistoryCapacity {
		agent.KickHistory = agent.KickHistory[1:]
	}
	record := KickRecord{Timestamp: now, Agent: agent.Name, Snippet: snippet}
	if source != "" {
		record.Source = source
	}
	agent.KickHistory = append(agent.KickHistory, record)
	if agent.OutputBuffer != nil {
		agent.OutputBuffer.Write("sandbox kick started (runtime=" + runtime + ")")
	}
	m.recordPrompt(agent.Name, source, message)
	m.logger.Info("audit: sandbox agent kicked", "name", agent.Name, "repo", repo, "runtime", runtime)

	spec := SandboxKickSpec{
		Agent: agent.Name,
		AgentConfig: configSnapshot{
			Backend:   effectiveBackend(agent),
			Model:     agent.Config.Model,
			LaunchCmd: agent.Config.LaunchCmd,
		},
		Message:       message,
		Org:           m.project.Org,
		Repo:          repo,
		WorkspaceDir:  m.sandboxWorkspaceDirLocked(),
		Image:         agent.Config.SandboxImage(m.sandboxConfig),
		EnvAllowlist:  agent.Config.SandboxEnvAllowlist(m.sandboxConfig),
		NetworkMode:   agent.Config.SandboxNetworkMode(m.sandboxConfig),
		Timeout:       time.Duration(agent.Config.SandboxTimeoutS(m.sandboxConfig)) * time.Second,
		PublishVerify: agent.Config.SandboxPublishVerify(m.sandboxConfig),
		VerifyTimeout: time.Duration(agent.Config.SandboxPublishVerifyTimeoutS(m.sandboxConfig)) * time.Second,
		// Every sandbox kick gets a review spool (hivecommons/hive#14986);
		// whether the agent may post through it is the relay's decision.
		ReviewSpoolDir: crux.SandboxReviewSpoolsDir(m.workDir),
	}
	// The verify list comes from config only. A kick message is not a trusted
	// source for commands: by the time it reaches the manager it carries
	// text from issue and PR titles, knowledge facts and delegated prompts,
	// so a directive found in it is reported and ignored rather than run.
	if cmds, ok := parsePublishVerifyDirective(message); ok {
		m.logger.Warn("sandbox kick message carries a hive:publish.verify directive; ignored — publish.verify commands are read from sandbox.publish.verify config only",
			"name", agent.Name, "commands", len(cmds))
	}
	runCtx, cancel := context.WithCancel(context.Background())
	agent.cancel = cancel
	runner := m.sandboxRunner
	mutationBoundary := m.sandboxMutation
	cloneMinter := m.tieredSandboxMinterLocked(TokenTierForRole(m.agentMode(agent), agent.Config.Role))
	var pushMinter pushbroker.TokenMinter
	var prClient PRCreator
	pushEnabled := false
	if m.agentMode(agent).CanPush() && m.project.PRsAllowed {
		pushMinter = cloneMinter
		prClient = m.sandboxPRClient
		pushEnabled = true
	}
	go m.runSandboxKick(runCtx, agent.Name, runtime, spec, launcher, runner, cloneMinter, pushMinter, pushEnabled, prClient, mutationBoundary)
	m.notifyKickObserver(agent.Name, KickObserverEventDelivered, source)
	return nil
}

func (m *Manager) sandboxWorkspaceDirLocked() string {
	return sandbox.WorkspaceRoot(m.sandboxConfig.WorkspaceDir, m.workDir)
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

func (m *Manager) runSandboxKick(ctx context.Context, name, runtime string, spec SandboxKickSpec, launcher sandbox.Launcher, runner sandboxCommandRunner, cloneMinter, minter pushbroker.TokenMinter, pushEnabled bool, prClient PRCreator, mutationBoundary effects.Boundary) {
	started := time.Now()
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
	ended := time.Now()
	// The run record is written to disk after m.mu is released (defers run
	// LIFO), so a slow volume never stalls every status reader.
	var pending *SandboxRunHistoryEntry
	var persist func(string, SandboxRunHistoryEntry) error
	var logArchive *sandboxKickLogArchive
	workDir := ""
	defer func() {
		// The log first: it is what an operator opens to understand the run
		// record, and the record names it by id.
		if perr := m.writeSandboxKickLog(name, logArchive); perr != nil {
			m.logger.Warn("sandbox kick log not archived", "name", name, "error", perr)
		}
		if pending == nil {
			return
		}
		if perr := persist(workDir, *pending); perr != nil {
			m.logger.Warn("sandbox run record not persisted", "name", name, "error", perr)
		}
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[name]
	if !ok {
		return
	}
	agent.cancel = nil
	kickSource := agent.LastKickSource
	// The run record is what a sandboxed agent's health and activity derive
	// from: there is no session to probe between kicks. Recorded on every
	// outcome but an operator cancel, and persisted so a pod roll does not
	// turn yesterday's successful run into "never ran".
	if err == nil || !agent.sandboxResumeAfterCancel {
		run := buildSandboxRun(runtime, started, ended, res, err)
		logArchive = m.prepareSandboxKickLogLocked(agent, runtime, started, res, err)
		if logArchive != nil {
			run.LogID = logArchive.id
		}
		agent.SandboxLastRun = &run
		agent.KickOutcome = KickOutcome{Kind: KickOutcomeEnded, At: ended, KickAt: started}
		if agent.LastKick != nil {
			agent.KickOutcome.KickAt = *agent.LastKick
		}
		entry := SandboxRunHistoryEntry{Agent: name, Backend: effectiveBackend(agent), Model: agent.Config.Model, Run: run}
		pending = &entry
		workDir = m.workDir
		persist = m.sandboxRunPersister
		if persist == nil {
			persist = persistSandboxRun
		}
	}
	if err != nil {
		if agent.sandboxResumeAfterCancel {
			agent.sandboxResumeAfterCancel = false
			agent.State = StateIdle
			agent.LastError = ""
			if agent.OutputBuffer != nil {
				agent.OutputBuffer.Write("sandbox kick cancelled during resume")
			}
			m.auditSandbox(name, "sandbox_cancelled", "sandbox kick cancelled while resuming")
			m.notifyKickObserver(agent.Name, KickObserverEventArchived, kickObserverArchiveDetail("sandbox-cancelled", kickSource))
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
		if errors.Is(err, ErrSandboxRunReportedFailure) {
			m.auditSandbox(name, "sandbox_pr_withheld", fmt.Sprintf("workspace=%s commits=%d branch=%s reason=%s", res.Workspace, res.CommitCount, res.Branch, res.PRWithheld))
		}
		m.auditSandbox(name, "sandbox_failed", err.Error())
		if res.Broker != nil && res.Broker.Error != "" {
			m.auditSandbox(name, "sandbox_broker_rejected", res.Broker.Error)
		}
		m.notifyKickObserver(agent.Name, KickObserverEventArchived, kickObserverArchiveDetail("sandbox-failed", kickSource))
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
	m.notifyKickObserver(agent.Name, KickObserverEventArchived, kickObserverArchiveDetail("sandbox-complete", kickSource))
}

func (m *Manager) auditSandbox(agent, action, detail string) {
	if fn := m.sandboxAuditCallback.Load(); fn != nil && *fn != nil {
		(*fn)(agent, action, detail)
	}
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
		SchedulerName:       job.SchedulerName,
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

func (m *Manager) SetSandboxMutationBoundary(boundary effects.Boundary) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxMutation = boundary
}

// SandboxAuditForTest fires the sandbox audit callback exactly as a sandbox
// outcome would, so the callback's wiring can be asserted from cmd/hive.
func (m *Manager) SandboxAuditForTest(agent, action, detail string) {
	m.auditSandbox(agent, action, detail)
}
