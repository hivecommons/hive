package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/kubejob"
	"github.com/hivecommons/hive/crux/sandbox"
	"github.com/hivecommons/hive/pkg/config"
)

// sandboxJobSweepTimeout bounds the boot sweep's API calls as a whole.
const sandboxJobSweepTimeout = 2 * time.Minute

// sandboxJobSweepAuditAction is the audit-log action of one reaped Job.
const sandboxJobSweepAuditAction = "sandbox_job_reaped"

// sandboxJobSweepAuditUser is who the audit log names for a reap: hive
// itself, at boot, not an operator.
const sandboxJobSweepAuditUser = "hive"

// sandboxJobSweeper is the part of kubejob.Launcher the boot sweep uses.
type sandboxJobSweeper interface {
	SweepOrphans(ctx context.Context, opts kubejob.SweepOptions) ([]kubejob.SweptJob, error)
}

// newSandboxJobSweeper builds the boot sweep's sweeper: the in-cluster Job
// launcher. Tests replace it so a test run on a cluster-hosted CI runner can
// never list or delete real Jobs.
var newSandboxJobSweeper = func() sandboxJobSweeper { return &kubejob.Launcher{} }

// sandboxJobRuntimeConfigured reports whether any agent (or the global
// default) selects the Job sandbox runtime, enabled or not: a Job orphaned
// before an agent was switched off still needs reaping.
func sandboxJobRuntimeConfigured(cfg *config.Config) bool {
	if strings.TrimSpace(cfg.AgentSandbox.Runtime) == cruxconfig.SandboxRuntimeJob {
		return true
	}
	for _, a := range cfg.Agents {
		if a.SandboxRuntime(cfg.AgentSandbox) == cruxconfig.SandboxRuntimeJob {
			return true
		}
	}
	return false
}

// sandboxJobSweepMaxAge is the longest kick timeout any Job-runtime agent
// has: a Job younger than that may still be a kick that has not timed out.
func sandboxJobSweepMaxAge(cfg *config.Config) time.Duration {
	maxAge := kubejob.DefaultKickTimeout
	for _, a := range cfg.Agents {
		if a.SandboxRuntime(cfg.AgentSandbox) != cruxconfig.SandboxRuntimeJob {
			continue
		}
		if t := time.Duration(a.SandboxTimeoutS(cfg.AgentSandbox)) * time.Second; t > maxAge {
			maxAge = t
		}
	}
	return maxAge
}

// sandboxJobLive reports whether a Job's agent label belongs to an agent the
// runtime says is mid-kick. It errs towards live: the label of every Job of
// a running agent's slug counts, whichever launch (kick or verify) made it.
func sandboxJobLive(agentNames []string, agentMgr crux.Runtime) func(string) bool {
	return func(label string) bool {
		if agentMgr == nil {
			return false
		}
		for _, name := range agentNames {
			if !strings.HasPrefix(label, "hive-"+sandbox.AgentSlug(name)+"-") {
				continue
			}
			if p, err := agentMgr.GetStatusFast(name); err == nil && p != nil && p.State == crux.StateRunning {
				return true
			}
		}
		return false
	}
}

// startSandboxJobBootSweep runs the boot sweep in the background when any
// agent selects the Job runtime. The config is read here, synchronously, so
// the goroutine never touches the live config a reload may be rewriting.
func startSandboxJobBootSweep(ctx context.Context, cfg *config.Config, agentMgr crux.Runtime, audit func(user, action, detail, agent string), logger *slog.Logger) bool {
	if cfg == nil || !sandboxJobRuntimeConfigured(cfg) {
		return false
	}
	maxAge := sandboxJobSweepMaxAge(cfg)
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	go runSandboxJobBootSweep(ctx, maxAge, names, agentMgr, newSandboxJobSweeper(), audit, logger)
	return true
}

// copyAdmissionVerifier is the part of kubejob.Launcher the boot admission
// check uses.
type copyAdmissionVerifier interface {
	VerifyCopyAdmission(ctx context.Context) error
}

// newCopyAdmissionVerifier builds the boot admission check's verifier: the
// in-cluster Job launcher. Tests replace it so a test run on a cluster-hosted
// CI runner never sends a probe to a real API server.
var newCopyAdmissionVerifier = func() copyAdmissionVerifier { return &kubejob.Launcher{} }

// sandboxJobCopyConfigured reports whether any Job-runtime agent uses
// workspace_transport: copy.
func sandboxJobCopyConfigured(cfg *config.Config) bool {
	for _, a := range cfg.Agents {
		if a.SandboxRuntime(cfg.AgentSandbox) == cruxconfig.SandboxRuntimeJob && a.SandboxJob(cfg.AgentSandbox).CopiesWorkspace() {
			return true
		}
	}
	return false
}

// startSandboxCopyAdmissionCheck probes, in the background, that the copy
// transport's admission policy is enforced in hive's namespace when any agent
// uses workspace_transport: copy (hivecommons/hive#19971), and logs an ERROR
// when it is not. Every copy kick runs the same check and is refused while it
// fails, so this only surfaces the problem at boot instead of at the first
// kick.
func startSandboxCopyAdmissionCheck(ctx context.Context, cfg *config.Config, logger *slog.Logger) bool {
	if cfg == nil || !sandboxJobCopyConfigured(cfg) {
		return false
	}
	verifier := newCopyAdmissionVerifier()
	go func() {
		ctx, cancel := context.WithTimeout(ctx, sandboxJobSweepTimeout)
		defer cancel()
		if err := verifier.VerifyCopyAdmission(ctx); err != nil {
			logger.Error("sandbox copy transport refused: its admission policy is not enforced; copy-transport kicks fail until it is", "error", err)
			return
		}
		logger.Info("sandbox copy transport admission policy is enforced", "policy", kubejob.CopyAdmissionPolicyName)
	}()
	return true
}

// runSandboxJobBootSweep reaps sandbox Jobs the previous hive process lost
// track of (hivecommons/hive#18525). A kick's timeout lives in memory, so a
// restart mid-kick leaves its Job with no owner; one sat Pending for 11
// days. Each reaped Job is logged and audited. Jobs without the
// app.kubernetes.io/managed-by=hive-kubejob label are never touched.
func runSandboxJobBootSweep(ctx context.Context, maxAge time.Duration, agentNames []string, agentMgr crux.Runtime, sweeper sandboxJobSweeper, audit func(user, action, detail, agent string), logger *slog.Logger) int {
	if sweeper == nil {
		sweeper = newSandboxJobSweeper()
	}
	ctx, cancel := context.WithTimeout(ctx, sandboxJobSweepTimeout)
	defer cancel()
	swept, err := sweeper.SweepOrphans(ctx, kubejob.SweepOptions{MaxAge: maxAge, Live: sandboxJobLive(agentNames, agentMgr)})
	for _, j := range swept {
		logger.Warn("audit: reaped orphaned sandbox job",
			"job", j.Name, "agent_label", j.Agent, "age", j.Age.Round(time.Second).String(), "reason", j.Reason)
		if audit != nil {
			audit(sandboxJobSweepAuditUser, sandboxJobSweepAuditAction,
				fmt.Sprintf("job=%s age=%s reason=%s", j.Name, j.Age.Round(time.Second), j.Reason), j.Agent)
		}
	}
	if err != nil {
		logger.Warn("sandbox job boot sweep incomplete", "error", err, "reaped", len(swept))
	} else {
		logger.Info("sandbox job boot sweep done", "reaped", len(swept), "max_age", maxAge.String())
	}
	return len(swept)
}
