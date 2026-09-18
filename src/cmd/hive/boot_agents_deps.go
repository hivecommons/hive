package main

import (
	"context"
	"log/slog"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// bootAgentsDeps carries the effects of bootAgents that outlive the call
// (#7571, step 2): every goroutine the phase starts and the two collaborators
// that touch /data or a signing key. The manager itself is constructed for
// real — agent.NewManager is pure — so a test can assert on the wiring the
// phase installs on it (resolvers, hooks, App auth, minter) while the loops
// that would otherwise poll tmux, token caches and request directories for
// the rest of the test binary's life are recorded instead of started.
type bootAgentsDeps struct {
	// startAgentLoops starts the per-agent token refresh, the credential
	// watchdog and the Copilot session refresh. All three are unconditional
	// (see the comments at the call site for why each must not gate on App
	// state at boot).
	startAgentLoops func(ctx context.Context, mgr *agent.Manager)
	// prepareRequestDirs creates the agent-facing request queues;
	// startTokenAccessAudit folds the per-UID wrapper events into the audit
	// log. Both run regardless of App state.
	prepareRequestDirs    func(logger *slog.Logger)
	startTokenAccessAudit func(ctx context.Context, logger *slog.Logger)
	// startRequestRelays starts the PR/issue/review/merge request watchers
	// on the App client. Only called inside the usable-App gate.
	startRequestRelays func(ctx context.Context, c *github.Client, r requestRelays)
	// startSelfAuthoredSweep is Client.StartSelfAuthoredAutoMergeSweep.
	startSelfAuthoredSweep func(ctx context.Context, c *github.Client, maxMerges int, acmmAllowed bool, acmmLevel *int)
	// buildMinter is buildAgentMinter; only consulted when mint.enabled.
	buildMinter func(cfg *config.Config, logger *slog.Logger) (agent.AgentMintIssuer, error)
	// startPermissionsWatcher is agent.StartPermissionsWatcher.
	startPermissionsWatcher func(logger *slog.Logger)
}

// requestRelays is the authorization the four request watchers are started
// with. Kept as a struct so a fake can assert every relay was armed with the
// manager's own gate rather than a permissive stand-in.
type requestRelays struct {
	prOpen    github.PRRequestAuthorizer
	holdLabel func(agentName string) bool
	issueOpen github.IssueRequestAuthorizer
	review    github.ReviewRequestAuthorizer
	merge     github.MergeRequestAuthorizer
}

func defaultBootAgentsDeps() bootAgentsDeps {
	return bootAgentsDeps{
		startAgentLoops: func(ctx context.Context, mgr *agent.Manager) {
			go mgr.StartAgentTokenRefresh(ctx)
			go mgr.StartCredentialWatchdog(ctx)
			go mgr.StartCopilotSessionRefresh(ctx)
		},
		prepareRequestDirs: github.PrepareRequestDirs,
		startTokenAccessAudit: func(ctx context.Context, logger *slog.Logger) {
			github.StartTokenAccessAuditWatcher(ctx, logger)
		},
		startRequestRelays: func(ctx context.Context, c *github.Client, r requestRelays) {
			c.StartPRRequestWatcher(ctx, r.prOpen, r.holdLabel, nil)
			c.StartIssueRequestWatcher(ctx, r.issueOpen, nil)
			c.StartReviewRequestWatcher(ctx, r.review, nil)
			c.StartMergeRequestWatcher(ctx, r.merge, nil)
		},
		startSelfAuthoredSweep: func(ctx context.Context, c *github.Client, maxMerges int, acmmAllowed bool, acmmLevel *int) {
			c.StartSelfAuthoredAutoMergeSweep(ctx, maxMerges, acmmAllowed, acmmLevel)
		},
		buildMinter: func(cfg *config.Config, logger *slog.Logger) (agent.AgentMintIssuer, error) {
			m, err := buildAgentMinter(cfg, logger)
			if err != nil {
				return nil, err
			}
			return m, nil
		},
		startPermissionsWatcher: func(logger *slog.Logger) { go agent.StartPermissionsWatcher(logger) },
	}
}
