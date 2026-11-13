package crux

import "time"

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
	// ResumeCommand, when set, is a backend resume command captured from the
	// vendor's own transcript store (for example "claude --resume <id>"). The
	// runtime re-validates it and runs it instead of the bare CLI.
	ResumeCommand string
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
