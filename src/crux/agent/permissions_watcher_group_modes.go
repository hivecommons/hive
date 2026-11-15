package agent

import (
	"log/slog"
	"os"
)

// Group-mode repairs the permissions watcher applies to shared state: bob's
// state directories and shared CLI credential files. Split out of
// permissions_watcher.go to keep that file under the crux file-size ratchet;
// the helpers they call (permWarnDedupe, dedupeKey, dedupeClass,
// skipBobStateDirNotOwned, logBobStateDirChmodFailure) stay there.

// fixBobStateDirGroupWrite ensures a bob state directory (.bob) grants the
// shared `node` group read+write+execute, so an agent UID reaching it through
// that group can persist bob's settings, installation_id and .bob-errors/
// logger target. It ORs bobStateDirGroupRWX into whatever the dir already has —
// never narrowing owner or other bits — so a dir that is already group-writable
// (>= 0770) is left byte-identical and the watcher stays idempotent.
//
// This is the remediation that verifyBobStateDirsWritable only ever DETECTED:
// production .bob dirs are created 0755 (r-x for the group) by their
// hive-<agent> owner, which is why bob logged "error saving your latest
// settings changes" / "Failed to initialize logger" fleet-wide and ran
// degraded. The chmod runs on every watcher tick and at startup, so the fix
// self-heals rather than requiring a hand-chmod on the pod.
//
// Safe and non-fatal: a chmod we are not permitted to perform (EPERM — we are
// neither the owner nor privileged) is logged and swallowed, exactly like the
// watcher's other chmod/chown arms, so it never blocks or panics.
func fixBobStateDirGroupWrite(path string, mode os.FileMode, chmod func(os.FileMode) error, logger *slog.Logger, ownerUID ...uint32) {
	perm := mode.Perm()
	// Nothing to do when the group already has all of r/w/x — this keeps an
	// already-remediated (>= 0770) dir untouched and the walk idempotent.
	if perm&bobStateDirGroupRWX == bobStateDirGroupRWX {
		permWarnDedupe.clear(dedupeKey("chmod bob state dir", path))
		return
	}
	if len(ownerUID) > 0 && skipBobStateDirNotOwned(logger, path, mode, ownerUID[0]) {
		return
	}
	newPerm := perm | bobStateDirGroupRWX
	var owner uint32
	if len(ownerUID) > 0 {
		owner = ownerUID[0]
	}
	if err := chmod(newPerm); err != nil {
		logBobStateDirChmodFailure(logger, path, perm, newPerm, err, owner)
		return
	}
	permWarnDedupe.clear(dedupeKey("chmod bob state dir", path))
	logger.Info("permissions watcher: fixed bob state dir permissions",
		"path", path,
		"old_mode", perm.String(),
		"new_mode", newPerm.String(),
	)
}

// fixSharedCredentialGroupRead ensures a shared CLI credential file grants the
// `node` group read, so one operator login keeps serving every agent UID after
// the CLI rewrites the file 0600 on a token refresh (#5730). It ORs
// sharedCredentialGroupRead into whatever the file already has — never
// narrowing owner or other bits — so an already-readable credential is left
// byte-identical and the walk stays idempotent.
//
// WHEN THIS CAN ACTUALLY REPAIR, and when it can only report:
//
// chmod(2) requires the caller to own the inode or hold CAP_FOWNER. The hive
// process drops to `dev` (uid 1001) at the entrypoint's privilege drop, so it
// CAN fix a credential that dev owns — the state a fresh boot leaves, since the
// entrypoint chowns the tree to dev:node — and CANNOT fix one a refresh has
// re-owned to hive-<agent> (uid 2001+). That case is repaired by the
// entrypoint's root-owned perm guard, which is why #5730 also makes those
// guards survivable.
//
// So the EPERM arm here is not a swallowed nuisance; on the deployment that
// produced #5730 it is the primary signal. It logs at ERROR with the mode, the
// owning uid and the exact one-line chmod, because the alternative — what
// actually happened — is a fleet that drops to login prompts while the credential
// watchdog reports an expired login that no re-login can fix. Deduped like every
// other arm, so a standing condition does not flood the log at the tick rate.
func fixSharedCredentialGroupRead(path string, mode os.FileMode, chmod func(os.FileMode) error, ownerUID uint32, logger *slog.Logger) {
	perm := mode.Perm()
	if perm&sharedCredentialGroupRead == sharedCredentialGroupRead {
		permWarnDedupe.clear(dedupeKey("chmod shared credential", path))
		return
	}
	newPerm := perm | sharedCredentialGroupRead
	if err := chmod(newPerm); err != nil {
		if warn, _ := permWarnDedupe.shouldWarn(dedupeKey("chmod shared credential", path), err.Error(), dedupeClass("chmod shared credential", err)); warn {
			logger.Error("permissions watcher: shared CLI credential is not group-readable and could not be repaired; agents on this backend will drop to a login prompt even though the credential itself is fine",
				"path", path,
				"old_mode", perm.String(),
				"want_mode", newPerm.String(),
				"owner_uid", ownerUID,
				"watcher_uid", os.Geteuid(),
				"error", err,
				"recovery", "chmod g+r "+path,
				"note", "identical repeats logged at debug until this changes",
			)
		} else {
			logger.Debug("permissions watcher: chmod shared credential failed",
				"path", path,
				"error", err,
			)
		}
		return
	}
	permWarnDedupe.clear(dedupeKey("chmod shared credential", path))
	logger.Info("permissions watcher: reopened shared CLI credential to the node group",
		"path", path,
		"old_mode", perm.String(),
		"new_mode", newPerm.String(),
		"owner_uid", ownerUID,
	)
}
