package config

type DataConfig struct {
	MetricsDir         string `yaml:"metrics_dir"`
	LogsDir            string `yaml:"logs_dir"`
	ClaudeSessionsDir  string `yaml:"claude_sessions_dir"`
	CopilotSessionsDir string `yaml:"copilot_sessions_dir"`
	BobSessionsDir     string `yaml:"bob_sessions_dir"`
	AgentsDir          string `yaml:"agents_dir"`

	// SessionRetentionDays is how many days of CLI session-state directories to
	// keep. Session directories are created per CLI invocation and were never
	// deleted, so they grow without bound on the shared /data PVC.
	//
	// Pointer so an explicit 0 (disable pruning) is distinguishable from the
	// key being absent (apply the default).
	SessionRetentionDays *int `yaml:"session_retention_days,omitempty"`
}
