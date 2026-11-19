package agentsmd

import (
	"strings"
	"sync"
	"time"
)

// InjectionStatus is what the last kick did with one repository's AGENTS.md
// (hivecommons/hive#18495): how big the file was, the cap that applied, how
// much of it reached the kick, and what was dropped. The scheduler records it
// on every render and the dashboard repo status reads it, so a truncated
// AGENTS.md is visible to the operator without reading logs.
type InjectionStatus struct {
	// SourceBytes is the size of the AGENTS.md file that was read.
	SourceBytes int `json:"sourceBytes"`
	// RenderedBytes is the size of the block prepended to the kick.
	RenderedBytes int `json:"renderedBytes"`
	// CapBytes is the effective cap (after default, floor and ceiling).
	CapBytes int `json:"capBytes"`
	// Truncated is true when any section was dropped.
	Truncated bool `json:"truncated"`
	// WholeFile is true when nothing of the repository's text fit.
	WholeFile bool `json:"wholeFile,omitempty"`
	// OmittedSections names the dropped sections in document order.
	OmittedSections []string `json:"omittedSections,omitempty"`
	// OmittedBytes is the rendered size of what was dropped.
	OmittedBytes int `json:"omittedBytes,omitempty"`
	// UpdatedAt (RFC 3339, UTC) is when the status was recorded.
	UpdatedAt string `json:"updatedAt"`
}

var (
	statusMu sync.RWMutex
	statuses = map[string]InjectionStatus{}
)

func statusKey(repo string) string { return strings.ToLower(strings.TrimSpace(repo)) }

// RecordStatus stores repo's latest injection status, keyed case-insensitively
// by repo ("owner/repo" by convention). A blank repo is ignored.
func RecordStatus(repo string, st InjectionStatus) {
	key := statusKey(repo)
	if key == "" {
		return
	}
	if st.UpdatedAt == "" {
		st.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	st.OmittedSections = append([]string(nil), st.OmittedSections...)
	statusMu.Lock()
	statuses[key] = st
	statusMu.Unlock()
}

// StatusFor returns repo's latest recorded injection status, and whether one
// exists. A repo whose kicks never read an AGENTS.md has none.
func StatusFor(repo string) (InjectionStatus, bool) {
	key := statusKey(repo)
	if key == "" {
		return InjectionStatus{}, false
	}
	statusMu.RLock()
	st, ok := statuses[key]
	statusMu.RUnlock()
	if ok {
		st.OmittedSections = append([]string(nil), st.OmittedSections...)
	}
	return st, ok
}
