package agent

import (
	"errors"
	"log/slog"
	"os"
	"syscall"
)

// bobStateDirNotOwnerHint tells the operator who can actually fix the dir.
const bobStateDirNotOwnerHint = "chmod needs the owning uid (or root); run `chmod g+rwx <path>` as the owner or fix ownership out of band"

// bobStateDirChmodPermanent reports whether a chmod failure can never succeed
// on retry (not the owner / not privileged), as opposed to a transient error.
func bobStateDirChmodPermanent(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EROFS)
}

// bobStateDirNotOwned reports whether this process certainly cannot chmod a
// dir owned by ownerUID: it is neither root nor the owner.
func bobStateDirNotOwned(ownerUID uint32) bool {
	euid := os.Geteuid()
	return euid != 0 && uint32(euid) != ownerUID
}

// logBobStateDirChmodFailure logs a failed .bob chmod once at WARN. The dedupe
// text for permanent errors is the errno alone: err.Error() embeds the
// /proc/self/fd/N path, which changes between passes and would otherwise make
// every repeat look like new information.
func logBobStateDirChmodFailure(logger *slog.Logger, path string, old, want os.FileMode, err error, ownerUID uint32) {
	key := dedupeKey("chmod bob state dir", path)
	text := err.Error()
	permanent := bobStateDirChmodPermanent(err)
	if permanent {
		text = dedupeClass("chmod bob state dir", err)
	}
	warn, _ := permWarnDedupe.shouldWarn(key, text, dedupeClass("chmod bob state dir", err))
	if !warn {
		logger.Debug("permissions watcher: chmod bob state dir failed", "path", path, "error", err)
		return
	}
	attrs := []any{
		"path", path,
		"old_mode", old.String(),
		"want_mode", want.String(),
		"error", err,
	}
	if permanent {
		attrs = append(attrs,
			"owner_uid", ownerUID,
			"hint", bobStateDirNotOwnerHint,
			"note", "permanent; not retried loudly, identical repeats logged at debug",
		)
	} else {
		attrs = append(attrs, "note", "identical repeats logged at debug until this changes")
	}
	logger.Warn("permissions watcher: chmod bob state dir failed; bob will keep reporting 'error saving your latest settings changes' and run degraded", attrs...)
}

// skipBobStateDirNotOwned short-circuits the chmod when the dir is owned by
// another uid; returns true when the caller must not attempt it.
func skipBobStateDirNotOwned(logger *slog.Logger, path string, mode os.FileMode, ownerUID uint32) bool {
	if !bobStateDirNotOwned(ownerUID) {
		return false
	}
	logBobStateDirChmodFailure(logger, path, mode.Perm(), mode.Perm()|bobStateDirGroupRWX,
		&os.PathError{Op: "chmod", Path: path, Err: syscall.EPERM}, ownerUID)
	return true
}
