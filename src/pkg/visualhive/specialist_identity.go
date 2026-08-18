package visualhive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// SpecialistEvidenceReceipt is the canonical verified GitHub/source receipt.
// Its JSON bytes are the single digest preimage shared by intake, admission,
// and later scheduler/mailbox execution.
type SpecialistEvidenceReceipt struct {
	RepositoryID       string `json:"repository_id"`
	WorkflowRunID      string `json:"workflow_run_id"`
	WorkflowRunAttempt string `json:"workflow_run_attempt"`
	ArtifactID         string `json:"artifact_id"`
	SourceArtifactID   string `json:"source_artifact_id"`
	ArtifactName       string `json:"artifact_name"`
	CommitSHA          string `json:"commit_sha"`
	HeadBranch         string `json:"head_branch"`
	Event              string `json:"event"`
	WorkflowName       string `json:"workflow_name"`
	WorkflowRunName    string `json:"workflow_run_name"`
	WorkflowPath       string `json:"workflow_path"`
	BundleSchema       string `json:"bundle_schema"`
	BundleSHA256       string `json:"bundle_sha256"`
	ManifestSHA256     string `json:"manifest_sha256"`
	ArtifactIndexSHA   string `json:"artifact_index_sha256"`
}

func (receipt SpecialistEvidenceReceipt) CanonicalBytes() ([]byte, error) {
	return json.Marshal(receipt)
}

func (receipt SpecialistEvidenceReceipt) SHA256() (string, error) {
	encoded, err := receipt.CanonicalBytes()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// SpecialistEvidenceIdentity is the shared, content-bound receipt carried by
// governor admission and the existing specialist mailbox work order.
type SpecialistEvidenceIdentity struct {
	Variant                   string                     `json:"variant,omitempty"`
	BundleSchemaVersion       string                     `json:"bundle_schema_version"`
	BundleSHA256              string                     `json:"bundle_sha256"`
	ManifestSHA256            string                     `json:"manifest_sha256,omitempty"`
	ArtifactIndexSHA256       string                     `json:"artifact_index_sha256,omitempty"`
	VerificationReceiptSHA256 string                     `json:"verification_receipt_sha256"`
	RepositoryID              string                     `json:"repository_id,omitempty"`
	WorkflowRunID             uint64                     `json:"workflow_run_id"`
	WorkflowRunAttempt        uint64                     `json:"workflow_run_attempt"`
	WorkflowRunHeadSHA        string                     `json:"workflow_run_head_sha"`
	WorkflowName              string                     `json:"workflow_name,omitempty"`
	WorkflowRunName           string                     `json:"workflow_run_name,omitempty"`
	WorkflowPath              string                     `json:"workflow_path,omitempty"`
	WorkflowEvent             string                     `json:"workflow_event,omitempty"`
	HeadBranch                string                     `json:"head_branch,omitempty"`
	ArtifactID                uint64                     `json:"artifact_id"`
	SourceArtifactID          uint64                     `json:"source_artifact_id,omitempty"`
	ArtifactName              string                     `json:"artifact_name"`
	ArtifactSHA256            string                     `json:"artifact_sha256"`
	VerificationReceipt       *SpecialistEvidenceReceipt `json:"verification_receipt,omitempty"`
}

const (
	SpecialistEvidenceVariantRevision     = "revision"
	SpecialistEvidenceVariantVisualHiveV3 = "visual-hive-v3"
)
