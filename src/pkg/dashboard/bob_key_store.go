package dashboard

// Storage for the IBM bobshell ("bob") API key VALUE entered from the spoke
// dashboard's governor Bob tab. The key is hive-wide, not per-agent:
// governor.bob.api_key_file is a single hive-scoped setting resolving to one
// file, so every bob agent shares it. bobshell cannot complete its browser SSO
// flow inside a pod (the callback is http://localhost:35227/bob-callback,
// which resolves to the pod, not the operator's machine), and the 1.0.6
// bundle exposes no device-code flow — the API key is the only headless
// credential. See config.BobConfig.
//
// hive.yaml only ever records a file PATH (governor.bob.api_key_file); the
// key value itself is written to a 0600 file on the PVC
// (config.WritableBobAPIKeyFile = /data/secrets/bob_api_key), which
// BobConfig.ResolveAPIKey already consults as its third lookup. A durable
// copy is additionally patched into the hive-secrets Kubernetes Secret when
// the pod has the hive-secrets-writer Role; on hives that whole-volume-mount
// that Secret it surfaces at /secrets/bob_api_key, which ResolveAPIKey
// consults AHEAD of the PVC file.
//
// This deliberately reuses the LiteLLM key-store machinery
// (patchKeyIntoHiveSecrets, actionableWriteError, errNotInCluster) rather
// than inventing a parallel mechanism.
//
// The key value must never be logged, echoed in API responses, or written to
// hive.yaml.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	// bobSecretDataKey is the key inside the hive-secrets Secret holding the
	// bob API key; it surfaces as /secrets/bob_api_key on hives that
	// whole-volume-mount the Secret (matching entrypoint.sh, which already
	// chowns/chmods that exact path).
	bobSecretDataKey = "bob_api_key"
	// bobKeyFileMode must be group-readable (0640, group node), NOT 0600.
	// An earlier 0600 assumed "the key file is read by the hive process
	// itself" — that is wrong for bob. Unlike every other key hive stores,
	// the bobshell CLI reads BOBSHELL_API_KEY while running AS THE AGENT UID
	// (2001+, in group node), not as dev. With 0600 dev:node the agent got
	// EACCES, the key resolved to "", and bob silently fell back to the W3ID
	// browser SSO flow that cannot complete in a pod — every bob agent on the
	// hive parked at the auth prompt. Group-read only; never world-readable.
	bobKeyFileMode = 0o640
	// bobKeyDirMode gives the PVC secrets dir group EXECUTE (0710) so agent
	// UIDs can traverse it to open the key file by name. Execute without read
	// means they cannot LIST the directory, so unrelated secrets stay
	// unenumerable; per-file modes remain the real access control. Matches the
	// mode entrypoint.sh applies to /data/secrets.
	bobKeyDirMode = 0o710
	// bobKeyDirGroupExec is the `g+x` bit alone — the minimum a pre-existing
	// 0700 secrets dir needs so agent UIDs can traverse it to open the key by
	// name. Added to (never substituted for) the dir's current mode.
	bobKeyDirGroupExec = 0o010
	// bobKeyMaxLen bounds an accepted key so a stray paste (or a file upload
	// aimed at the field) cannot write an unbounded blob onto the PVC.
	// bobshell keys are far shorter than this; the limit only rejects abuse.
	bobKeyMaxLen = 4096
	// bobKeyNameMaxLen bounds the optional human LABEL for the key. It is a
	// short display string ("Team inference key"), not the secret, so the
	// limit is generous enough for a descriptive name yet small enough that a
	// stray paste into the name field cannot bloat hive.yaml.
	bobKeyNameMaxLen = 128
)

// writableBobKeyFile is a package var (not a const) so tests can point it at
// a temp dir. The production value never changes at runtime.
var writableBobKeyFile = config.WritableBobAPIKeyFile

// storeBobAPIKey persists a bob API key value and returns the api_key_file
// path to record in hive.yaml. Only the chosen store is ever logged — never
// the key.
func (s *Server) storeBobAPIKey(key string) (string, error) {
	pvcErr := writeBobKeyFile(key)
	if pvcErr == nil {
		s.logger.Info("bob api key stored", "api_key_file", writableBobKeyFile)
	} else {
		// A misowned or read-only PVC must not sink the whole save: an
		// in-cluster hive with the hive-secrets-writer Role can still persist
		// the key via the Secret. Only fail if BOTH stores fail.
		s.logger.Info("bob api key PVC file write failed; attempting hive-secrets Secret fallback",
			"api_key_file", writableBobKeyFile, "reason", pvcErr.Error())
	}

	secretErr := patchKeyIntoHiveSecrets(bobSecretDataKey, key)
	switch {
	case secretErr == nil:
		s.logger.Info("bob api key also patched into hive-secrets Secret",
			"mount_path", config.DefaultBobAPIKeyFile)
	case errors.Is(secretErr, errNotInCluster):
		// Docker/LXC hive — Secret patch legitimately unavailable.
	default:
		s.logger.Info("hive-secrets Secret not patched; PVC key file is the only store",
			"reason", secretErr.Error())
	}

	if pvcErr == nil {
		// PVC file written: hive.yaml records that path (the authoritative,
		// instantly-effective store), regardless of the Secret patch outcome.
		return writableBobKeyFile, nil
	}
	// PVC write failed. If the Secret patch succeeded the key IS persisted and
	// surfaces at DefaultBobAPIKeyFile on Secret-mounting hives — record that
	// path so ResolveAPIKey finds it.
	if secretErr == nil {
		return config.DefaultBobAPIKeyFile, nil
	}
	// Nothing persisted the key — surface the actionable PVC error.
	return "", pvcErr
}

// writeBobKeyFile writes the key value to the PVC key file with owner-only
// permissions, atomically: the value goes to a temp file in the same
// directory which is then renamed over the destination. The rename means a
// concurrent agent launch reading the path sees either the old key or the new
// one, never a truncated partial write.
//
// The hive process runs as a non-root user (uid 1001, the /data owner) and
// cannot chown. entrypoint.sh pre-creates /data/secrets as dev:node 0700; if
// that dir is nevertheless root-owned, WriteFile fails with EACCES and
// actionableWriteError explains why rather than leaking a bare
// "permission denied".
func writeBobKeyFile(key string) error {
	dir := filepath.Dir(writableBobKeyFile)
	if err := os.MkdirAll(dir, bobKeyDirMode); err != nil {
		return actionableWriteError(dir, err)
	}
	// MkdirAll is a no-op (and applies no mode) when the dir already exists,
	// which it will on every hive provisioned before this fix — leaving it at
	// the old owner-only 0700 that agent UIDs cannot traverse. Widen it in
	// place so saving a key repairs the dir without waiting for the next pod
	// restart to re-run entrypoint.sh.
	//
	// Only ADD the group-traverse bit to the dir's existing mode; never assign
	// bobKeyDirMode wholesale. A blanket assignment would also GRANT bits the
	// operator deliberately removed — in particular it would make an
	// intentionally read-only secrets dir writable again, silently undoing a
	// lockdown. Widening one bit is the least surprising repair.
	//
	// Best-effort and deliberately NOT fatal: on a hive where the dir is owned
	// by another UID this fails, but the key write below may still succeed, and
	// failing the save over a dir we could not re-mode would regress hives that
	// work today. The launch-time probe (verifyBobKeyReadable) reports any
	// resulting inaccessibility with a far more actionable message.
	if info, err := os.Stat(dir); err == nil {
		if mode := info.Mode().Perm(); mode&bobKeyDirGroupExec == 0 {
			_ = os.Chmod(dir, mode|bobKeyDirGroupExec)
		}
	}
	tmp, err := os.CreateTemp(dir, ".bob_api_key.*")
	if err != nil {
		return actionableWriteError(dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: a no-op once the rename below has succeeded.
	defer func() { _ = os.Remove(tmpName) }()

	// Chmod before writing so the secret is never briefly world-readable.
	if err := tmp.Chmod(bobKeyFileMode); err != nil {
		_ = tmp.Close() // Preserve the primary chmod error.
		return actionableWriteError(tmpName, err)
	}
	if _, err := tmp.WriteString(key); err != nil {
		_ = tmp.Close() // Preserve the primary write error.
		return actionableWriteError(tmpName, err)
	}
	// Flush to disk before the rename so a crash cannot leave an empty file
	// in place of a previously-valid key.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close() // Preserve the primary sync error.
		return actionableWriteError(tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return actionableWriteError(tmpName, err)
	}
	if err := os.Rename(tmpName, writableBobKeyFile); err != nil {
		return actionableWriteError(writableBobKeyFile, err)
	}
	return nil
}

// clearBobKeyFile removes the PVC key file so the operator can revoke or
// rotate a key. A missing file is success (already cleared) — clearing must
// be idempotent so a retry after a partial failure converges rather than
// erroring. Returns whether a file was actually removed.
func clearBobKeyFile() (removed bool, err error) {
	if err := os.Remove(writableBobKeyFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("removing %s: %w", writableBobKeyFile, err)
	}
	return true, nil
}
