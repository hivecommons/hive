package cli

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/config"
)

// uidMapName is the per-work-dir UID map. cmd/hive uses the entrypoint's
// /data path; standalone the map almost never exists, which the manager
// handles by running every agent as the invoking user.
const uidMapName = "uid-map.json"

// newManagerRuntime is the one place the CLI constructs *agent.Manager. It
// mirrors cmd/hive's bootAgents minimum: the enabled definitions, a logger
// and a ProjectContext; none of the governor, GitHub or dashboard wiring.
// The work dir is read by the manager from HIVE_WORK_DIR, which
// globalOptions.resolve has already exported.
func newManagerRuntime(defs map[string]config.AgentConfig, project crux.ProjectContext) crux.Runtime {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	uidMap := filepath.Join(os.Getenv(envWorkDir), uidMapName)
	return agent.AsRuntime(agent.NewManagerWithOptions(defs, logger, project, agent.WithUIDMapPath(uidMap)))
}
