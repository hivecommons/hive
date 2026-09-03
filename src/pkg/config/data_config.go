package config

type DataConfig struct {
	MetricsDir         string `yaml:"metrics_dir"`
	LogsDir            string `yaml:"logs_dir"`
	ClaudeSessionsDir  string `yaml:"claude_sessions_dir"`
	CopilotSessionsDir string `yaml:"copilot_sessions_dir"`
	BobSessionsDir     string `yaml:"bob_sessions_dir"`
	AgentsDir          string `yaml:"agents_dir"`
}
