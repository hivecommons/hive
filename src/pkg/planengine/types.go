package planengine

import (
	"strings"
	"time"
)

// DocumentStatus is the #8301 `document_status` value.
type DocumentStatus string

const (
	// DocumentDraft: the artifact is still being authored.
	DocumentDraft DocumentStatus = "draft"
	// DocumentFinal: the artifact is complete and the stage may advance.
	DocumentFinal DocumentStatus = "final"
	// DocumentStale: the artifact was invalidated by a later upstream change
	// and must not advance until it is replanned or re-approved. Spektacular
	// 0.23+ emits it; 0.22 has no stale status.
	DocumentStale DocumentStatus = "stale"
	// DocumentSuperseded: a newer document replaced the artifact.
	DocumentSuperseded DocumentStatus = "superseded"
	// DocumentArchived: the artifact was withdrawn and will not be finished.
	DocumentArchived DocumentStatus = "archived"
)

// ArtifactStatus is the parsed per-artifact status document
// (spektacular#45):
//
//	{"error":false,"kind","name","artifact_id","document_status","current_step",
//	 "completed_steps":[],"created_at","updated_at","closed_at","spec","plan"}
//
// Progress is decided by DocumentStatus, CurrentStep and CompletedSteps
// only. UpdatedAt is informational: it is workflow activity only while the
// in-progress workflow state matches this artifact, and a file mtime
// otherwise (moved by a checkout, a reformat or a touch), and Spektacular
// may omit it entirely. Nothing in this package reads it to decide progress
// or staleness; whether a generation is spent is decided by the hub executor
// that ran it (#9143). Spec and Plan are the frontmatter cross-references,
// which are almost never populated; they are surfaced for diagnostics and
// are never a join key.
type ArtifactStatus struct {
	Kind           string
	Name           string
	ArtifactID     string
	DocumentStatus DocumentStatus
	CurrentStep    string
	CompletedSteps []string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ClosedAt       time.Time
	Spec           string
	Plan           string
	// Body is populated by the runner for a final Spec artifact via the
	// Spektacular file-read verb. It is not part of the status JSON contract.
	Body string
}

// Final reports whether the artifact has reached document_status final.
func (s ArtifactStatus) Final() bool { return s.DocumentStatus == DocumentFinal }

// JoinKey returns the globally durable Spektacular artifact key. Older
// Spektacular versions do not emit artifact_id, so Hive falls back to the
// historical name only for backward compatibility.
func (s ArtifactStatus) JoinKey() string {
	if strings.TrimSpace(s.ArtifactID) != "" {
		return strings.TrimSpace(s.ArtifactID)
	}
	return s.Name
}

// PlanTask is one task of a final plan as exported by Spektacular. Ref is the
// plan-local id (T1, T2, ...), DependsOn references other refs, Execution is
// agent_suitable or human_required (empty means agent_suitable).
type PlanTask struct {
	ID        string   `json:"id,omitempty"`
	Ref       string   `json:"ref"`
	Repo      string   `json:"repo,omitempty"`
	Title     string   `json:"title"`
	DependsOn []string `json:"depends_on,omitempty"`
	Execution string   `json:"execution,omitempty"`
}

// Plan is the structured plan export the runner hands to Hive's planner so no
// LLM redecomposition happens.
type Plan struct {
	Kind  string     `json:"kind"`
	Name  string     `json:"name"`
	Tasks []PlanTask `json:"tasks"`
}
