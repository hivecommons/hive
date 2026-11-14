package crux

import (
	"errors"
	"time"
)

// UserSessionVendor is one CLI a user session may run, as offered to the
// dashboard's new-session picker.
type UserSessionVendor struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Available is true when the vendor's binary is on PATH.
	Available bool `json:"available"`
}

// UserSessionSpec is what the runtime needs to start (or resume) one user
// session. ID is the dashboard-issued session id (lower-case hex).
type UserSessionSpec struct {
	ID     string
	Vendor string
	// UIDSlot is the session's allocated slot (1-based) in the reserved
	// per-session uid range. The runtime runs the session's tmux server and
	// CLI as the uid derived from it, never as the hive's own uid. The
	// dashboard allocates it, persists it in the session record and passes the
	// same value on every (re)start.
	UIDSlot int
	// ResumeCommand, when set, is a backend resume command captured from the
	// vendor's own transcript store (for example "claude --resume <id>"). The
	// runtime re-validates it and runs it instead of the bare CLI.
	ResumeCommand string
	// WorkspaceRepo, when set ("org/repo"), makes the session open in the
	// hive's persistent per-repo workspace: a clone of that repository, shared
	// by every session of the hive, that `spektacular init <vendor>` has been
	// run in. Empty keeps the session's own empty work directory as cwd.
	WorkspaceRepo string
	// SpektacularBinary is the spektacular executable to run `init` with
	// (runs.spektacular.binary); empty means the default name on PATH.
	SpektacularBinary string
}

// UserSessionProbe is one live read of a session's tmux state.
type UserSessionProbe struct {
	// Exists is false when tmux has no such session.
	Exists bool
	// Dead is true when the session exists but its CLI has exited (the pane is
	// kept so the scrollback stays readable).
	Dead bool
	// Attached is the number of tmux clients currently attached.
	Attached int
	// Activity is tmux's last-activity time for the session.
	Activity time.Time
}

// UserSessionResume names a vendor-native conversation a session can resume.
type UserSessionResume struct {
	SessionID string
	Command   string
}

var (
	// ErrUserSessionUploadTooLarge is returned by StoreUpload when the file
	// exceeds the byte cap; nothing is left behind.
	ErrUserSessionUploadTooLarge = errors.New("upload is too large")
	// ErrUserSessionUploadLimit is returned by StoreUpload when the session
	// already holds the maximum number of uploaded files.
	ErrUserSessionUploadLimit = errors.New("too many uploaded files")
)
