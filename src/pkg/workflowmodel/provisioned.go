package workflowmodel

import "time"

// ProvisionedLabels is what the stage engine remembers having created in
// one repository for a bound model (hivecommons/hive#17620, RFC phase 4
// follow-up): the model's identity at the time and the label names it
// provisioned. A later pass — in this process or the next — diffs it
// against the current binding to find labels that are stale, and only a
// label named here is ever a candidate for deletion.
type ProvisionedLabels struct {
	// Repo is the lower-cased owner/name.
	Repo    string `json:"repo"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	Hash    string `json:"hash,omitempty"`
	// Labels are the model labels the engine created or recoloured, in
	// declaration order, plus any stale label a previous pass kept because
	// it was still in use.
	Labels    []string  `json:"labels"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
}

// ProvisionedStore persists ProvisionedLabels per repository. The workflow
// store (pkg/workflowmodel/store) implements it on the data volume; the
// engine runs with nil when no store is wired and then never deletes.
type ProvisionedStore interface {
	// LoadProvisioned returns the record for repo, or nil (no error) when
	// none has been written.
	LoadProvisioned(repo string) (*ProvisionedLabels, error)
	SaveProvisioned(rec ProvisionedLabels) error
	DeleteProvisioned(repo string) error
}
