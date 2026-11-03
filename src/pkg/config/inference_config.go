package config

// Discovery-auth defaults for the self-hosted inference backends. Like
// LiteLLM, hive.yaml stores only the env var NAME and/or key FILE PATH —
// never the key value itself (Config.Save() writes the expanded config
// back to disk, so a key value in YAML would be persisted in plaintext).
const (
	// DefaultVLLMAPIKeyEnv is the env var consulted for the vLLM model
	// discovery API key when governor.vllm.api_key_env is not set.
	DefaultVLLMAPIKeyEnv = "HIVE_VLLM_API_KEY"
	// DefaultLLMDAPIKeyEnv is the env var consulted for the llm-d model
	// discovery API key when governor.llm-d.api_key_env is not set.
	DefaultLLMDAPIKeyEnv = "HIVE_LLMD_API_KEY"
)

type LoggingConfig struct {
	Dir        string `yaml:"dir"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxAgeDays int    `yaml:"max_age_days"`
	MaxBackups int    `yaml:"max_backups"`
	Compress   bool   `yaml:"compress"`
	Level      string `yaml:"level"`
	// KickLogSource selects what per-kick agent logs are built from:
	// scrollback, transcript or auto. Empty or unknown means scrollback.
	KickLogSource string `yaml:"kick_log_source,omitempty" json:"kick_log_source,omitempty"`
	// ReadScanMaxMB lowers how many megabytes of log (decompressed) one admin
	// log read may scan (hivecommons/hive#11942). Zero or a value above the
	// built-in bound keeps the built-in bound; it can only be lowered.
	ReadScanMaxMB int `yaml:"read_scan_max_mb,omitempty" json:"read_scan_max_mb,omitempty"`
}
