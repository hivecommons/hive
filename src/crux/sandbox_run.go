package crux

import (
	"os"
	"path/filepath"
)

const (
	// DefaultWorkDir is the agent manager's work dir when HIVE_WORK_DIR is
	// unset.
	DefaultWorkDir = "/data/agents"
	// sandboxRunsDirName holds one <agent>.json per sandboxed agent (its last
	// run) and runs.jsonl, the append-only history.
	sandboxRunsDirName    = "sandbox-runs"
	sandboxRunHistoryName = "runs.jsonl"
)

// WorkDir is the agent manager's work dir: $HIVE_WORK_DIR or DefaultWorkDir.
// The host and the runtime (in-process or sidecar) share it through /data.
func WorkDir() string {
	if d := os.Getenv("HIVE_WORK_DIR"); d != "" {
		return d
	}
	return DefaultWorkDir
}

// SandboxRunsDir is where sandbox run records live under workDir.
func SandboxRunsDir(workDir string) string {
	return filepath.Join(workDir, sandboxRunsDirName)
}

// SandboxRunHistoryPath is the append-only sandbox run history under
// workDir, one SandboxRun history entry per line. The token collector reads
// it to attribute sandbox usage per agent.
func SandboxRunHistoryPath(workDir string) string {
	return filepath.Join(SandboxRunsDir(workDir), sandboxRunHistoryName)
}
