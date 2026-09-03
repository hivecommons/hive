package dashboard

import (
	"fmt"
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/hubbackup"
	"github.com/hivecommons/hive/pkg/spokebackup"
)

// Self-service spoke backup.
//
// GET  /api/backup/status   — whether backup is available (owner-only)
// POST /api/backup          — build and stream an encrypted archive (owner-only)
//
// The download is a POST rather than a GET on purpose. A GET would be
// reachable by a plain <a href> or an <img> on another site, and this response
// body contains GitHub App private keys; keeping it non-idempotent and
// fetch-only keeps it off the cross-origin-navigation path.

// backupBuildTimeout bounds a single backup request so a wedged filesystem
// read cannot hold a dashboard connection open indefinitely.
const backupBuildTimeout = spokebackup.BuildTimeout

// backupKeyConfigPath is the dashboard location of the backup-encryption-key
// setting, returned to the UI so an owner who cannot reach deployment env
// still learns where to configure it.
const backupKeyConfigPath = "governor:Security"

// Owner checks below mirror handleConfigDownload and handleSelfUpgrade: an
// empty role means no per-user identity is in play (an open/dev spoke), which
// those handlers already treat as owner.

// handleBackupStatus reports whether a self-service backup can run, so the UI
// can show the menu entry in a disabled state with a real reason instead of
// failing only after the owner clicks.
func (s *Server) handleBackupStatus(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	role := r.Header.Get("X-Hive-Role")
	if role == "" {
		role = "owner"
	}
	if role != "owner" {
		jsonError(w, "owner access required", http.StatusForbidden)
		return
	}
	_, source, err := hubbackup.ResolveKeyWithSource(s.backupKeyConfig())
	if err != nil {
		jsonResponse(w, map[string]interface{}{
			"available": false,
			// The reason is operator-facing configuration state, not a secret:
			// it names where to set a key, never a key value.
			"reason": "backup encryption key is not configured on this hive — " +
				"set one in Settings → Governor → Security → Backup encryption key",
			// configPath lets the UI deep-link to the setting instead of
			// telling a hosted owner to edit a deployment they cannot see.
			"configPath": backupKeyConfigPath,
		})
		return
	}
	// source is the safe "file:<path>" / "env:<NAME>" label, never the key.
	jsonResponse(w, map[string]interface{}{"available": true, "source": source})
}

// handleBackupDownload builds an encrypted backup of this spoke and streams it
// to the owner.
func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	role := r.Header.Get("X-Hive-Role")
	if role == "" {
		role = "owner"
	}
	if role != "owner" {
		jsonError(w, "only the owner can back up this hive", http.StatusForbidden)
		return
	}

	// No key means no backup. Refusing here is deliberate: the alternative is
	// streaming GitHub App private keys to a browser in plaintext. The key is
	// resolved from governor config first so a hosted owner can configure it
	// without deployment-env access, with HIVE_BACKUP_KEY as the fallback.
	key, err := hubbackup.ResolveKey(s.backupKeyConfig())
	if err != nil {
		jsonError(w,
			"backup encryption key is not configured on this hive, so a backup "+
				"would be unencrypted; refusing — set a key in Settings → Governor "+
				"→ Security → Backup encryption key", http.StatusPreconditionFailed)
		return
	}

	hiveID := ""
	if s.deps != nil && s.deps.Config != nil {
		hiveID = s.deps.Config.HiveID
	}

	// Build with a bound so a stuck read cannot pin the connection.
	type buildResult struct {
		res *spokebackup.Result
		err error
	}
	done := make(chan buildResult, 1)
	go func() {
		res, err := spokebackup.Build(key, hiveID, s.logger)
		done <- buildResult{res, err}
	}()

	var out buildResult
	select {
	case out = <-done:
	case <-time.After(backupBuildTimeout):
		jsonError(w, "backup timed out while reading hive data", http.StatusGatewayTimeout)
		return
	}
	if out.err != nil {
		// The error may name paths but never file contents.
		s.logger.Error("spoke backup failed", "err", out.err)
		jsonError(w, "backup failed: "+out.err.Error(), http.StatusInternalServerError)
		return
	}

	res := out.res
	filename := spokebackup.ArchiveName(hiveID, time.Now())

	// Summarize what was captured in headers so the UI can tell the owner
	// what they got without the archive being decrypted client-side. These
	// carry counts and directory names only — no secrets.
	beadDirs := res.BeadDirs
	w.Header().Set("X-Hive-Backup-Bead-Dirs", fmt.Sprintf("%d", len(beadDirs)))
	w.Header().Set("X-Hive-Backup-Files", fmt.Sprintf("%d", len(res.Manifest.Files)))
	w.Header().Set("X-Hive-Backup-Encrypted", "aes-256-gcm")
	// Let the browser read the summary headers on a same-origin fetch.
	w.Header().Set("Access-Control-Expose-Headers",
		"X-Hive-Backup-Bead-Dirs, X-Hive-Backup-Files, X-Hive-Backup-Encrypted")

	// Never cache a credential-bearing response.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(res.Sealed)))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

	s.logger.Info("spoke backup created",
		"hive_id", hiveID,
		"files", len(res.Manifest.Files),
		"bead_dirs", len(beadDirs),
		"bytes", len(res.Sealed))

	_, _ = w.Write(res.Sealed)
}
