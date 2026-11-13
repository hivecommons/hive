package runtime

import (
	"io"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
)

// ObserveCodeWorkTranscript exposes the runtime's read-only successful-edit
// collector without coupling consumers to the agent implementation. Unsupported
// transcripts and unverifiable worktrees return nil.
func ObserveCodeWorkTranscript(r io.Reader, workDir string, since, now time.Time) *crux.CodeWork {
	return agent.ObserveCodeWorkTranscript(r, workDir, since, now)
}

// UserSessions is the runtime behind the dashboard's Sessions feature
// (interactive, named CLI terminals). It is exposed here so consumers do not
// name the agent package.
type UserSessions = agent.UserSessionRuntime

// NewUserSessions returns a user-session runtime driving the real tmux binary.
func NewUserSessions(logger *slog.Logger) *UserSessions {
	return agent.NewUserSessionRuntime(logger)
}
