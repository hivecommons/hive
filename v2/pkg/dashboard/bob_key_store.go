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

	"github.com/kubestellar/hive/v2/pkg/config"
)

const (
	// bobSecretDataKey is the key inside the hive-secrets Secret holding the
	// bob API key; it surfaces as /secrets/bob_api_key on hives that
	// whole-volume-mount the Secret (matching entrypoint.sh, which already
	// chowns/chmods that exact path).
	bobSecretDataKey = "bob_api_key"
	// bobKeyFileMode: the key file is read by the hive process itself (the
	// resolver runs in-process at agent launch), so owner-only is the
	// tightest mode that works.
	bobKeyFileMode = 0o600
	// bobKeyDirMode keeps the PVC secrets dir owner-only, matching the mode
	// entrypoint.sh applies to /data/secrets.
	bobKeyDirMode = 0o700
	// bobKeyMaxLen bounds an accepted key so a stray paste (or a file upload
	// aimed at the field) cannot write an unbounded blob onto the PVC.
	// bobshell keys are far shorter than this; the limit only rejects abuse.
	bobKeyMaxLen = 4096
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
	tmp, err := os.CreateTemp(dir, ".bob_api_key.*")
	if err != nil {
		return actionableWriteError(dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: a no-op once the rename below has succeeded.
	defer func() { _ = os.Remove(tmpName) }()

	// Chmod before writing so the secret is never briefly world-readable.
	if err := tmp.Chmod(bobKeyFileMode); err != nil {
		tmp.Close()
		return actionableWriteError(tmpName, err)
	}
	if _, err := tmp.WriteString(key); err != nil {
		tmp.Close()
		return actionableWriteError(tmpName, err)
	}
	// Flush to disk before the rename so a crash cannot leave an empty file
	// in place of a previously-valid key.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
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
