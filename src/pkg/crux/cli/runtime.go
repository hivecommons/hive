package cli

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/crux"
)

// defaultPoliciesLocalDir is cmd/hive's default policies checkout.
const defaultPoliciesLocalDir = "/data/policies"

// uidMapName is the per-work-dir UID map. cmd/hive uses the entrypoint's
// /data path; standalone the map almost never exists, which the manager
// handles by running every agent as the invoking user.
const uidMapName = "uid-map.json"

// newManagerRuntime is the one place the CLI constructs *agent.Manager. It
// mirrors cmd/hive's bootAgents minimum: the enabled definitions, a logger
// and a ProjectContext; none of the governor, GitHub or dashboard wiring.
// The work dir is read by the manager from HIVE_WORK_DIR, which
// globalOptions.resolve has already exported.
func newManagerRuntime(defs map[string]config.AgentConfig, project agent.ProjectContext) crux.Runtime {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	uidMap := filepath.Join(os.Getenv(envWorkDir), uidMapName)
	return crux.FromManager(agent.NewManagerWithOptions(defs, logger, project, agent.WithUIDMapPath(uidMap)))
}

// projectContextFromConfig builds the ProjectContext cmd/hive builds in
// bootAgentsWith, minus the fields that need a GitHub client (App bot
// identity). Nil cfg yields the zero context, which is what a definition-file
// run without --config gets.
func projectContextFromConfig(cfg *config.Config) agent.ProjectContext {
	if cfg == nil {
		return agent.ProjectContext{}
	}
	pc := agent.ProjectContext{
		Org:              cfg.Project.Org,
		Repos:            cfg.Project.Repos,
		RepoPaused:       cfg.IsRepoPaused,
		AgentRepos:       cfg.ReposForAgent,
		AgentPrimaryRepo: cfg.PrimaryRepoForAgent,
		PrimaryRepoName:  cfg.Project.PrimaryRepo,
		ACMMLevel:        cfg.ACMMLevelOrZero(),
		PRsAllowed:       cfg.Project.PRsAllowed(),
		PolicyDir:        policyDir(cfg.Policies),
		AppAuthoredPRs:   cfg.GitHub.AppAuthoredPRsEnabled(),
		AppBotLogin:      cfg.GitHub.BotLogin(),
		BobSessionPrefix: cfg.Governor.Bob.SessionPrefix,
	}
	if cfg.GitHub.IsGHE() {
		pc.GHHost = cfg.GitHub.HostLabel()
	}
	return pc
}

// policyDir is where agents look for policy files; same rule as cmd/hive's
// policyDir (checkout root plus policies.path).
func policyDir(p config.PoliciesConfig) string {
	dir := p.LocalDir
	if dir == "" {
		dir = defaultPoliciesLocalDir
	}
	if p.Path != "" {
		dir += "/" + p.Path
	}
	return dir
}
