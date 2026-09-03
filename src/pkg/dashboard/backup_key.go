package dashboard

// Storage for the self-service backup encryption key entered from the spoke
// dashboard's governor Security tab (#4129).
//
// Before this, the key could only come from HIVE_BACKUP_KEY on the deployment.
// A hosted spoke owner has no deployment-env access, so "Back up this hive"
// was permanently refused for exactly the users who cannot rebuild a lost hive
// by hand. The key is now settable through governor config: the VALUE is
// written to a 0600 PVC file and hive.yaml records only the resulting PATH
// (governor.backup.key_file), mirroring the bob/LiteLLM key stores.
//
// The key value must never be logged, echoed in an API response, or written
// to hive.yaml. Fail-closed is preserved end to end: clearing the key makes
// the backup endpoint refuse again.

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hubbackup"
)

const (
	// backupKeyFileMode is owner-read/write only. Unlike the bob key, this
	// file is read by the hive process itself, never by an agent UID, so it
	// needs no group bit.
	backupKeyFileMode = 0o600

	// backupKeyDirMode matches the PVC secrets directory used by the sibling
	// key stores.
	backupKeyDirMode = 0o700

	// backupKeyNameMaxLen bounds the optional human LABEL for the key
	// ("escrowed in 1Password"). It is a display string, not the secret.
	backupKeyNameMaxLen = 128

	// backupKeyHexLen is the exact length of a hex-encoded AES-256 key.
	backupKeyHexLen = 64
)

// writableBackupKeyFile is a package var (not a const) so tests can point it
// at a temp dir. The production value never changes at runtime.
var writableBackupKeyFile = config.WritableBackupKeyFile

// backupKeyConfig returns the live governor backup config, or nil when the
// server has no config wired (unit tests, dev servers). A nil result still
// resolves the HIVE_BACKUP_KEY fallback: *config.BackupConfig methods are
// nil-receiver safe.
func (s *Server) backupKeyConfig() *config.BackupConfig {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return nil
	}
	return &s.deps.Config.Governor.Backup
}

// handleBackupKeyStatus serves GET /api/config/governor/backup. It reports
// PRESENCE and the safe source label only — never the key value.
func (s *Server) handleBackupKeyStatus(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	bc := s.backupKeyConfig()
	source := bc.ResolveKeySource()
	// Presence alone is not enough to promise a working backup: a truncated
	// or non-hex key is configured but unusable, and an owner should learn
	// that here rather than from a failed backup.
	usable := true
	reason := ""
	if source == "" {
		usable = false
	} else if _, _, err := hubbackup.ResolveKeyWithSource(bc); err != nil {
		usable = false
		reason = err.Error()
	}
	keyName := ""
	if bc != nil {
		keyName = bc.KeyName
	}
	jsonResponse(w, map[string]interface{}{
		"ok":         true,
		"configured": source != "",
		"usable":     usable,
		"source":     source,
		"keyName":    keyName,
		"algorithm":  "aes-256-gcm",
		"reason":     reason,
	})
}

// handleBackupKeySet serves PUT /api/config/governor/backup. Owner-only: the
// key decrypts an archive containing this hive's GitHub App private keys.
func (s *Server) handleBackupKeySet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		EncryptionKey *string `json:"encryptionKey"`
		KeyName       *string `json:"keyName"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.EncryptionKey == nil {
		jsonError(w, "encryptionKey is required", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(*body.EncryptionKey)
	if key == "" {
		// Distinct from DELETE: an empty PUT is a mistake, not an intentional
		// revoke, so say so rather than silently disabling backups.
		jsonError(w, "encryptionKey is empty — paste a 64-character hex key "+
			"(openssl rand -hex 32), or use Clear to remove the existing one",
			http.StatusBadRequest)
		return
	}
	// Validate BEFORE storing: a key that cannot seal an archive would leave
	// the hive looking configured while every backup still failed.
	if len(key) != backupKeyHexLen {
		jsonError(w, fmt.Sprintf("encryptionKey must be exactly %d hex characters "+
			"(a 32-byte AES-256 key); generate one with: openssl rand -hex 32", backupKeyHexLen),
			http.StatusBadRequest)
		return
	}
	if _, err := hex.DecodeString(key); err != nil {
		// The hex error names the offending byte, so it is not wrapped in.
		jsonError(w, "encryptionKey must be hex characters only (0-9, a-f); "+
			"generate one with: openssl rand -hex 32", http.StatusBadRequest)
		return
	}
	var keyName string
	nameProvided := body.KeyName != nil
	if nameProvided {
		keyName = strings.TrimSpace(*body.KeyName)
		if len(keyName) > backupKeyNameMaxLen {
			jsonError(w, fmt.Sprintf("keyName is too long (limit %d characters)", backupKeyNameMaxLen),
				http.StatusBadRequest)
			return
		}
	}

	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config not available", http.StatusInternalServerError)
		return
	}
	cfg := s.deps.Config
	keyFile, err := storeBackupKey(key)
	if err != nil {
		// redactSecret guards against the key surfacing through a filesystem
		// error that happened to embed it.
		jsonError(w, "failed to store backup encryption key: "+redactSecret(err.Error(), key),
			http.StatusInternalServerError)
		return
	}
	cfg.Governor.Backup.KeyFile = keyFile
	if nameProvided {
		cfg.Governor.Backup.KeyName = keyName
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after backup key update", "error", err)
	}
	// Path only — the value is never logged.
	s.logger.Info("backup encryption key stored", "key_file", keyFile)
	s.auditFromRequest(r, "config_governor_backup_key",
		auditDetail("section", "backup", "action", "set"), "")
	s.refreshAndPersist()

	jsonResponse(w, map[string]interface{}{
		"ok":         true,
		"status":     "updated",
		"configured": true,
		"source":     cfg.Governor.Backup.ResolveKeySource(),
		"keyName":    cfg.Governor.Backup.KeyName,
	})
}

// handleBackupKeyClear serves DELETE /api/config/governor/backup. Removing the
// key is a deliberate, fail-closed action: backups are refused again until a
// new key is set (an env-provided key, if any, remains as the fallback).
func (s *Server) handleBackupKeyClear(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config not available", http.StatusInternalServerError)
		return
	}
	cfg := s.deps.Config
	paths := []string{cfg.Governor.Backup.KeyFile, writableBackupKeyFile}
	seen := map[string]bool{"": true}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			s.logger.Warn("could not remove backup key file", "path", p, "error", err)
		}
	}
	cfg.Governor.Backup.KeyFile = ""
	cfg.Governor.Backup.KeyName = ""
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after backup key clear", "error", err)
	}
	s.auditFromRequest(r, "config_governor_backup_key",
		auditDetail("section", "backup", "action", "clear"), "")
	s.refreshAndPersist()

	jsonResponse(w, map[string]interface{}{
		"ok":         true,
		"status":     "cleared",
		"configured": cfg.Governor.Backup.ResolveKeySource() != "",
		"source":     cfg.Governor.Backup.ResolveKeySource(),
	})
}

// storeBackupKey writes the key VALUE to the PVC and returns the path to
// record in hive.yaml. Only the path is ever returned or logged.
func storeBackupKey(key string) (string, error) {
	path := writableBackupKeyFile
	if err := os.MkdirAll(filepath.Dir(path), backupKeyDirMode); err != nil {
		return "", fmt.Errorf("create secrets directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(key+"\n"), backupKeyFileMode); err != nil {
		return "", fmt.Errorf("write key file: %w", err)
	}
	// A pre-existing file keeps its old mode through WriteFile, so tighten it
	// explicitly rather than trusting the create path.
	if err := os.Chmod(path, backupKeyFileMode); err != nil {
		return "", fmt.Errorf("chmod key file: %w", err)
	}
	return path, nil
}
