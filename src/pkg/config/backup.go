package config

import (
	"os"
	"strings"
)

// Backup encryption-key resolution.
//
// The self-service backup is sealed with AES-256-GCM and there is NO default
// key: an unconfigured hive refuses to produce a backup rather than stream
// GitHub App private keys to a browser in plaintext.
//
// Historically the key could only arrive through the HIVE_BACKUP_KEY
// environment variable, which is set on the deployment. Hosted spoke owners do
// not have deployment-env access, so backup was unavailable to exactly the
// users who cannot rebuild a lost hive by hand (#4129). The key is therefore
// also settable through governor config: the dashboard writes the value to a
// 0600 file on the PVC and hive.yaml records only that PATH — the same
// path-not-value rule already used for the bob and LiteLLM API keys, and the
// reason Config.Save() can round-trip the config without leaking a secret.
const (
	// BackupKeyEnv is the deployment environment variable holding the
	// hex-encoded AES-256 backup key. It remains supported as a fallback for
	// hives that already set it.
	BackupKeyEnv = "HIVE_BACKUP_KEY"

	// DefaultBackupKeyFile is the read-only Kubernetes Secret mount consulted
	// when governor.backup.key_file is unset, mirroring DefaultBobAPIKeyFile.
	DefaultBackupKeyFile = "/secrets/backup_encryption_key"

	// WritableBackupKeyFile is the PVC-backed location a hosted spoke owner
	// can write the key to from the dashboard, with no cluster or deployment
	// access. Referenced by path only; never by value.
	WritableBackupKeyFile = WritableSecretsDir + "/backup_encryption_key"
)

// BackupConfig locates the self-service backup encryption key. It holds a file
// PATH and an env var NAME — never the key value itself.
type BackupConfig struct {
	// KeyFile is the path to a file containing the hex-encoded AES-256 key.
	// Set by the dashboard when an owner configures the key through governor
	// config.
	KeyFile string `yaml:"key_file,omitempty" json:"key_file,omitempty"`

	// KeyEnv names an alternative environment variable to read the key from.
	// Empty means BackupKeyEnv.
	KeyEnv string `yaml:"key_env,omitempty" json:"key_env,omitempty"`

	// KeyName is an operator-chosen LABEL ("escrowed in 1Password") recorded
	// so an owner can tell which key material this hive uses without ever
	// seeing the value. Safe to serialize.
	KeyName string `yaml:"key_name,omitempty" json:"key_name,omitempty"`
}

// ResolveKey returns the raw (still unvalidated) backup key, or "" when none
// is configured anywhere.
//
// Governor config wins over the environment so a spoke owner can rotate the
// key themselves; the env var stays as the fallback for existing deployments.
func (c *BackupConfig) ResolveKey() string {
	key, _ := c.ResolveKeyWithSource()
	return key
}

// ResolveKeySource reports WHERE the key was found without exposing the value:
// "file:<path>", "env:<NAME>", or "" when unconfigured. Safe to log and safe
// to return from an API.
func (c *BackupConfig) ResolveKeySource() string {
	_, source := c.ResolveKeyWithSource()
	return source
}

// ResolveKeyWithSource returns the raw key and its safe source label. It is
// nil-receiver safe so callers without a config (the hub cron backup) can
// resolve the environment fallback through the same code path.
func (c *BackupConfig) ResolveKeyWithSource() (string, string) {
	if c == nil {
		// No config in hand means no governor-config path to honour: this is
		// the hub cron backup and the hive-backup CLI, whose key is the
		// escrowed env var. Probing the well-known spoke key files here would
		// let a stray PVC file silently outrank the escrowed key and seal an
		// archive nobody can decrypt at restore time.
		if key := strings.TrimSpace(os.Getenv(BackupKeyEnv)); key != "" {
			return key, "env:" + BackupKeyEnv
		}
		return "", ""
	}
	envName := strings.TrimSpace(c.KeyEnv)
	files := []string{c.KeyFile, WritableBackupKeyFile, DefaultBackupKeyFile}
	seen := map[string]bool{"": true}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		if data, err := os.ReadFile(f); err == nil {
			if key := strings.TrimSpace(string(data)); key != "" {
				return key, "file:" + f
			}
		}
	}
	if envName != "" {
		if key := strings.TrimSpace(os.Getenv(envName)); key != "" {
			return key, "env:" + envName
		}
	}
	if key := strings.TrimSpace(os.Getenv(BackupKeyEnv)); key != "" {
		return key, "env:" + BackupKeyEnv
	}
	return "", ""
}
