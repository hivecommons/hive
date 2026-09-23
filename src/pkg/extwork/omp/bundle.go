package omp

import (
	"encoding/json"
	"fmt"

	"github.com/hivecommons/hive/pkg/extwork"
)

// Bundle is the bounded, read-only context handed to the workbench on
// ext_start, after it accepted the offer. It carries the admission identities
// the receipt must echo, a summary, the repository name for artifact
// attribution, optional files, and optional hints. It never carries a
// credential: no repository token, no dashboard token.
type Bundle struct {
	Admission BundleAdmission   `json:"admission"`
	Summary   string            `json:"summary"`
	Repo      string            `json:"repo"`
	Files     map[string]string `json:"files,omitempty"`
	Hints     map[string]string `json:"hints,omitempty"`
}

// BundleAdmission is the identity subset of extwork.Admission the workbench
// sees.
type BundleAdmission struct {
	WorkKey          string `json:"work_key"`
	AssignmentID     string `json:"assignment_id"`
	Generation       uint64 `json:"generation"`
	Stage            string `json:"stage"`
	ContractRevision string `json:"contract_revision"`
	InputRevision    string `json:"input_revision"`
}

// BuildBundle serialises a Bundle deterministically for adm. The caller sets
// adm.RequestDigest to extwork.RequestDigest of the result.
func BuildBundle(adm extwork.Admission, summary, repo string, files, hints map[string]string) ([]byte, error) {
	for k := range files {
		if _, err := extwork.CleanArtifactPath(k); err != nil {
			return nil, fmt.Errorf("bundle file %q: %w", k, err)
		}
	}
	return json.Marshal(Bundle{
		Admission: BundleAdmission{WorkKey: adm.WorkKey, AssignmentID: adm.AssignmentID, Generation: adm.Generation, Stage: adm.Stage, ContractRevision: adm.ContractRevision, InputRevision: adm.InputRevision},
		Summary:   summary,
		Repo:      repo,
		Files:     files,
		Hints:     hints,
	})
}
