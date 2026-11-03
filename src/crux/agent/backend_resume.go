package agent

import (
	"path/filepath"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent/resumehandle"
)

// Backend-native resume-handle discovery lives in the leaf package
// agent/resumehandle; the names below keep the agent API stable.

// ResumeHandle is crux.ResumeHandle: a backend-native conversation that
// can be resumed after the authoring tmux pane is gone.
type ResumeHandle = crux.ResumeHandle

// BackendSupportsResumeHandle reports whether backend keeps a transcript that
// CaptureResumeHandle can name.
func BackendSupportsResumeHandle(backend string) bool {
	return resumehandle.BackendSupportsResumeHandle(backend)
}

// CaptureResumeHandle names the newest conversation the agent's backend has
// persisted under its own HOME, or reports false when it has none.
func CaptureResumeHandle(agentName string, uid int, backend string, now time.Time) (ResumeHandle, bool) {
	return CaptureResumeHandleIn(agentName, uid, backend, "", now)
}

// CaptureResumeHandleIn is CaptureResumeHandle for an agent whose CLI runs in
// workDir, which project-scoped backends need.
func CaptureResumeHandleIn(agentName string, uid int, backend, workDir string, now time.Time) (ResumeHandle, bool) {
	return resumehandle.CaptureResumeHandleIn(AgentHome(agentName, uid, backend), codexHomePath(agentName), backend, workDir, now)
}

// ResumeHandle captures the backend-native resume handle of the named agent's
// current conversation, or false when the agent is unknown, not running, or
// on a backend with no capturable handle.
func (m *Manager) ResumeHandle(name string) (ResumeHandle, bool) {
	if m == nil {
		return ResumeHandle{}, false
	}
	m.mu.RLock()
	agent, found := m.agents[name]
	if !found || agent == nil || agent.State != StateRunning {
		m.mu.RUnlock()
		return ResumeHandle{}, false
	}
	backend, uid := effectiveBackend(agent), agent.UID
	workDir := ""
	if m.workDir != "" {
		workDir = filepath.Join(m.workDir, name)
	}
	m.mu.RUnlock()
	return CaptureResumeHandleIn(name, uid, backend, workDir, time.Now())
}
