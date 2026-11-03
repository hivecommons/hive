package crux

import "time"

// CodeWork is an observed successful file edit, verified against a dirty
// Git worktree and its actual origin/head. It is not a task intention or a
// generic PR write. The host binds it to the matching dispatch's PR.
type CodeWork struct {
	Repo       string    `json:"repo"`
	Head       string    `json:"head"`
	Path       string    `json:"path"`
	Source     string    `json:"source"`
	At         time.Time `json:"at"`
	ObservedAt time.Time `json:"observed_at"`
	KickAt     time.Time `json:"kick_at"`
}

const CodeWorkSource = "successful-edit+verified-git-worktree"
