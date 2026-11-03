package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ProviderProfileDigest binds the complete technical/actor profile shared by
// an isolated qualification and its production destination. Destination,
// enrollment epoch and explicit scheduling bounds are not producer identity.
func (s IntegrationExecutionScope) ProviderProfileDigest() string {
	raw, _ := json.Marshal(struct {
		RepositoryID, RequestActorID, CandidateActorID, ProducerAppID, PromotionAppID, PromotionInstallationID, MaterializerID, CandidateWorkflowID int64
		DefaultBranch, Policy, WorkflowTree                                                                                                         string
		RequiredJobs                                                                                                                                []string
	}{s.RepositoryID, s.RequestActorID, s.CandidateActorID, s.ProducerAppID, s.PromotionAppID,
		s.PromotionInstallationID, s.MaterializerID, s.CandidateWorkflowID, s.DefaultBranch, s.Policy, s.WorkflowTree, s.RequiredJobs})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
