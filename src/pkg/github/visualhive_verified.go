package github

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kubestellar/hive/pkg/visualhive"
)

var verifiedVisualHiveDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

type verifiedVisualHiveArtifactSeal struct {
	identityDigest string
	plan           visualhive.VerifiedImportPlan
}

func sealVerifiedVisualHiveArtifact(verified *VerifiedVisualHiveArtifact, bundle *visualhive.ValidatedBundle) error {
	if verified == nil || bundle == nil {
		return fmt.Errorf("verified artifact and bundle are required")
	}
	plan, err := bundle.BuildImportPlan()
	if err != nil {
		return err
	}
	digest, err := verifiedVisualHiveIdentityDigest(*verified)
	if err != nil {
		return err
	}
	verified.seal = &verifiedVisualHiveArtifactSeal{identityDigest: digest, plan: plan}
	return nil
}

func (verified VerifiedVisualHiveArtifact) ImportPlan() (visualhive.VerifiedImportPlan, error) {
	if err := verified.requireSeal(); err != nil {
		return visualhive.VerifiedImportPlan{}, err
	}
	return verified.seal.plan, nil
}

func (verified VerifiedVisualHiveArtifact) VerifiedEvidenceRoot() (string, error) {
	if err := verified.requireSeal(); err != nil {
		return "", err
	}
	if strings.TrimSpace(verified.EvidenceRootPath) == "" {
		return "", fmt.Errorf("verified Visual Hive evidence root is unavailable")
	}
	return verified.EvidenceRootPath, nil
}

// SpecialistEvidenceIdentity reuses the content-bound identity previously
// owned by integrated specialist execution, but now requires the private seal
// minted only by FetchAndVerifyVisualHiveBundle.
func (verified VerifiedVisualHiveArtifact) SpecialistEvidenceIdentity(finding visualhive.FindingLifecycle) (visualhive.SpecialistEvidenceIdentity, error) {
	if err := verified.requireSeal(); err != nil {
		return visualhive.SpecialistEvidenceIdentity{}, err
	}
	return buildSpecialistEvidenceIdentity(verified, finding)
}

func (verified VerifiedVisualHiveArtifact) requireSeal() error {
	if verified.seal == nil || !verified.seal.plan.Valid() {
		return fmt.Errorf("sealed FetchAndVerifyVisualHiveBundle receipt is required")
	}
	digest, err := verifiedVisualHiveIdentityDigest(verified)
	if err != nil {
		return err
	}
	if digest != verified.seal.identityDigest {
		return fmt.Errorf("verified Visual Hive artifact identity changed after verification")
	}
	return nil
}

func verifiedVisualHiveIdentityDigest(verified VerifiedVisualHiveArtifact) (string, error) {
	verified.seal = nil
	encoded, err := json.Marshal(verified)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func buildSpecialistEvidenceIdentity(verified VerifiedVisualHiveArtifact, finding visualhive.FindingLifecycle) (visualhive.SpecialistEvidenceIdentity, error) {
	if verified.BundleSchemaVersion != visualhive.ManifestSchemaV3 {
		return visualhive.SpecialistEvidenceIdentity{}, fmt.Errorf("specialist dispatch requires a verified content-addressed Visual Hive bundle v3")
	}
	for name, digest := range map[string]string{
		"bundle": verified.BundleSHA256, "manifest": verified.ManifestSHA256, "artifact index": verified.ArtifactIndexSHA256,
	} {
		if !verifiedVisualHiveDigest.MatchString(strings.ToLower(strings.TrimSpace(digest))) {
			return visualhive.SpecialistEvidenceIdentity{}, fmt.Errorf("verified Visual Hive %s digest is invalid", name)
		}
	}
	if !strings.EqualFold(strings.TrimSpace(finding.LastBundleDigest), verified.BundleSHA256) {
		return visualhive.SpecialistEvidenceIdentity{}, fmt.Errorf("finding bundle digest does not match the verified Visual Hive artifact")
	}
	if lastRun := strings.TrimSpace(finding.LastWorkflowRunID); lastRun != "" && lastRun != verified.WorkflowRunID {
		return visualhive.SpecialistEvidenceIdentity{}, fmt.Errorf("finding workflow run does not match the verified Visual Hive artifact")
	}
	if !validVerifiedGitObject(verified.CommitSHA) {
		return visualhive.SpecialistEvidenceIdentity{}, fmt.Errorf("verified Visual Hive workflow head is invalid")
	}
	runID, err := positiveVerifiedIdentity("workflow run", verified.WorkflowRunID)
	if err != nil {
		return visualhive.SpecialistEvidenceIdentity{}, err
	}
	runAttempt, err := positiveVerifiedIdentity("workflow run attempt", verified.WorkflowRunAttempt)
	if err != nil {
		return visualhive.SpecialistEvidenceIdentity{}, err
	}
	artifactID, err := positiveVerifiedIdentity("artifact", verified.ArtifactID)
	if err != nil {
		return visualhive.SpecialistEvidenceIdentity{}, err
	}
	sourceArtifactID, err := positiveVerifiedIdentity("source artifact", verified.SourceArtifactID)
	if err != nil {
		return visualhive.SpecialistEvidenceIdentity{}, err
	}
	for name, value := range map[string]string{
		"repository": verified.RepositoryID, "artifact name": verified.ArtifactName, "workflow name": verified.WorkflowName,
		"workflow run name": verified.WorkflowRunName, "workflow path": verified.WorkflowPath, "workflow event": verified.Event,
		"head branch": verified.HeadBranch,
	} {
		if strings.TrimSpace(value) == "" || len(value) > 512 || strings.IndexByte(value, 0) >= 0 {
			return visualhive.SpecialistEvidenceIdentity{}, fmt.Errorf("verified Visual Hive %s is invalid", name)
		}
	}
	receipt := visualhive.SpecialistEvidenceReceipt{
		RepositoryID: verified.RepositoryID, WorkflowRunID: verified.WorkflowRunID, WorkflowRunAttempt: verified.WorkflowRunAttempt,
		ArtifactID: verified.ArtifactID, SourceArtifactID: verified.SourceArtifactID, ArtifactName: verified.ArtifactName,
		CommitSHA: strings.ToLower(verified.CommitSHA), HeadBranch: verified.HeadBranch, Event: verified.Event,
		WorkflowName: verified.WorkflowName, WorkflowRunName: verified.WorkflowRunName, WorkflowPath: verified.WorkflowPath,
		BundleSchema: verified.BundleSchemaVersion, BundleSHA256: strings.ToLower(verified.BundleSHA256),
		ManifestSHA256: strings.ToLower(verified.ManifestSHA256), ArtifactIndexSHA: strings.ToLower(verified.ArtifactIndexSHA256),
	}
	encoded, err := receipt.CanonicalBytes()
	if err != nil {
		return visualhive.SpecialistEvidenceIdentity{}, fmt.Errorf("encode verified Visual Hive receipt identity: %w", err)
	}
	receiptDigest := sha256.Sum256(encoded)
	return visualhive.SpecialistEvidenceIdentity{
		Variant:             visualhive.SpecialistEvidenceVariantVisualHiveV3,
		BundleSchemaVersion: verified.BundleSchemaVersion, BundleSHA256: strings.ToLower(verified.BundleSHA256),
		ManifestSHA256: strings.ToLower(verified.ManifestSHA256), ArtifactIndexSHA256: strings.ToLower(verified.ArtifactIndexSHA256),
		VerificationReceiptSHA256: hex.EncodeToString(receiptDigest[:]), RepositoryID: verified.RepositoryID,
		WorkflowRunID: runID, WorkflowRunAttempt: runAttempt, WorkflowRunHeadSHA: strings.ToLower(verified.CommitSHA),
		WorkflowName: verified.WorkflowName, WorkflowRunName: verified.WorkflowRunName, WorkflowPath: verified.WorkflowPath,
		WorkflowEvent: verified.Event, HeadBranch: verified.HeadBranch, ArtifactID: artifactID, SourceArtifactID: sourceArtifactID,
		ArtifactName: verified.ArtifactName, ArtifactSHA256: strings.ToLower(verified.ManifestSHA256), VerificationReceipt: &receipt,
	}, nil
}

func positiveVerifiedIdentity(name, value string) (uint64, error) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("verified Visual Hive %s identity is invalid", name)
	}
	return parsed, nil
}

func validVerifiedGitObject(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
