package runtime

import (
	"io"
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
