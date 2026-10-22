package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Backup key resolution (#4129): hosted spoke owners cannot set deployment
// env vars, so governor config must be able to supply the key — while
// HIVE_BACKUP_KEY keeps working for hives that already set it.

const backupCfgTestKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func writeBackupKeyFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "backup_encryption_key")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBackupResolveKeyUnconfigured(t *testing.T) {
	t.Setenv(BackupKeyEnv, "")
	var bc BackupConfig
	if key, source := bc.ResolveKeyWithSource(); key != "" || source != "" {
		t.Fatalf("unconfigured resolve = (%q, %q), want empty", key, source)
	}
}

func TestBackupResolveKeyFromEnvFallback(t *testing.T) {
	t.Setenv(BackupKeyEnv, backupCfgTestKey)
	var bc BackupConfig
	key, source := bc.ResolveKeyWithSource()
	if key != backupCfgTestKey {
		t.Fatalf("key = %q, want the env value", key)
	}
	if source != "env:"+BackupKeyEnv {
		t.Fatalf("source = %q, want env:%s", source, BackupKeyEnv)
	}
}

// The configured file wins over the environment: that is what lets a hosted
// owner rotate the key themselves on a hive whose deployment sets one.
func TestBackupResolveKeyFilePrecedesEnv(t *testing.T) {
	t.Setenv(BackupKeyEnv, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	path := writeBackupKeyFile(t, backupCfgTestKey+"\n")
	bc := BackupConfig{KeyFile: path}

	key, source := bc.ResolveKeyWithSource()
	if key != backupCfgTestKey {
		t.Fatalf("key = %q, want the configured file's value (trailing newline trimmed)", key)
	}
	if source != "file:"+path {
		t.Fatalf("source = %q, want file:%s", source, path)
	}
}

// A whitespace-only file is the same hazard as no file at all: it must not
// look configured, or the hive would claim backups work and then fail.
func TestBackupResolveKeyIgnoresBlankFile(t *testing.T) {
	t.Setenv(BackupKeyEnv, "")
	bc := BackupConfig{KeyFile: writeBackupKeyFile(t, "  \n\t")}
	if key, source := bc.ResolveKeyWithSource(); key != "" || source != "" {
		t.Fatalf("blank file resolve = (%q, %q), want empty", key, source)
	}
}

func TestBackupResolveKeyCustomEnvName(t *testing.T) {
	t.Setenv("HIVE_ALT_BACKUP_KEY", backupCfgTestKey)
	t.Setenv(BackupKeyEnv, "")
	bc := BackupConfig{KeyEnv: "HIVE_ALT_BACKUP_KEY"}
	if _, source := bc.ResolveKeyWithSource(); source != "env:HIVE_ALT_BACKUP_KEY" {
		t.Fatalf("source = %q, want env:HIVE_ALT_BACKUP_KEY", source)
	}
}

// ResolveKeySource is what APIs and logs use, so it must never expose value.
func TestBackupResolveKeySourceIsPathOnly(t *testing.T) {
	t.Setenv(BackupKeyEnv, "")
	path := writeBackupKeyFile(t, backupCfgTestKey)
	bc := BackupConfig{KeyFile: path}
	if got := bc.ResolveKeySource(); got == backupCfgTestKey {
		t.Fatal("ResolveKeySource returned the key value")
	}
}

// A nil receiver is the hub cron backup / hive-backup CLI path: env ONLY, no
// panic. It must not consult the well-known spoke key files — a stray file on
// a mounted PVC silently outranking the escrowed HIVE_BACKUP_KEY would seal
// archives with a key nobody holds, discovered only during a restore.
func TestBackupResolveKeyNilReceiverIsEnvOnly(t *testing.T) {
	t.Setenv(BackupKeyEnv, backupCfgTestKey)
	var bc *BackupConfig
	key, source := bc.ResolveKeyWithSource()
	if key != backupCfgTestKey {
		t.Fatalf("nil-receiver resolve = %q, want the env value", key)
	}
	if source != "env:"+BackupKeyEnv {
		t.Fatalf("nil-receiver source = %q, want env:%s", source, BackupKeyEnv)
	}

	t.Setenv(BackupKeyEnv, "")
	if key, source := bc.ResolveKeyWithSource(); key != "" || source != "" {
		t.Fatalf("nil-receiver with no env = (%q, %q), want empty", key, source)
	}
}

func TestBackupResolveKeySourceWithInjectedReaders(t *testing.T) {
	t.Setenv(BackupKeyEnv, "")
	const fakeFile = "/nowhere/backup_encryption_key"
	readFile := func(path string) ([]byte, error) {
		if path == fakeFile {
			return []byte(backupCfgTestKey + "\n"), nil
		}
		return nil, os.ErrNotExist
	}
	getenv := func(name string) string {
		if name == BackupKeyEnv {
			return backupCfgTestKey
		}
		return ""
	}

	bc := BackupConfig{KeyFile: fakeFile}
	if got := bc.ResolveKeySourceWith(getenv, readFile); got != "file:"+fakeFile {
		t.Fatalf("file source = %q, want file:%s", got, fakeFile)
	}
	if got := bc.ResolveKeySourceWith(getenv, func(string) ([]byte, error) { return nil, os.ErrNotExist }); got != "env:"+BackupKeyEnv {
		t.Fatalf("env fallback source = %q, want env:%s", got, BackupKeyEnv)
	}
	// Nil readers fall back to the real environment and filesystem.
	if got := bc.ResolveKeySourceWith(nil, nil); got != "" {
		t.Fatalf("nil-reader source = %q, want unconfigured", got)
	}
	var nilCfg *BackupConfig
	if key, source := nilCfg.ResolveKeyWithSourceFunc(getenv, readFile); key != backupCfgTestKey || source != "env:"+BackupKeyEnv {
		t.Fatalf("nil receiver resolve = (%q, %q), want env fallback", key, source)
	}
}
