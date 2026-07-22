package visualhive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

const VerifiedImportPlanSchema = "hive.visual-hive-import-plan.v1"

type VerifiedPacketIdentity struct {
	PacketID            string    `json:"packet_id"`
	PacketDigest        string    `json:"packet_digest"`
	ReplayKey           string    `json:"replay_key"`
	Repository          string    `json:"repository"`
	RepositoryID        string    `json:"repository_id"`
	SourceRef           string    `json:"source_ref"`
	BaseSHA             string    `json:"base_sha"`
	WorkflowName        string    `json:"workflow_name"`
	WorkflowEvent       string    `json:"workflow_event"`
	WorkflowRunID       string    `json:"workflow_run_id"`
	WorkflowRunAttempt  string    `json:"workflow_run_attempt"`
	WorkflowArtifactID  string    `json:"workflow_artifact_id"`
	ProducerName        string    `json:"producer_name"`
	ProducerVersion     string    `json:"producer_version"`
	ProducerGitCommit   string    `json:"producer_git_commit"`
	ArtifactIndexSHA256 string    `json:"artifact_index_sha256"`
	GeneratedAt         time.Time `json:"generated_at"`
	ExpiresAt           time.Time `json:"expires_at"`
	Verdict             string    `json:"verdict"`
}

type EvidenceArtifactReceipt struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
	Kind        string `json:"kind"`
	ContentType string `json:"content_type"`
}

type VisualWorkAuthority struct {
	ProposalOnly            bool `json:"proposal_only"`
	GitHubWriteAllowed      bool `json:"github_write_allowed"`
	MergeAllowed            bool `json:"merge_allowed"`
	BaselineChangesAllowed  bool `json:"baseline_changes_allowed"`
	BaselineApprovalAllowed bool `json:"baseline_approval_allowed"`
}

// AdmittedVisualWork is the immutable source-work shape. Hive adds safe path
// policy, base-tree identity, recurrence, and attempt only after lifecycle and
// governor checks; the verified packet never invents those values.
type AdmittedVisualWork struct {
	SourceExternalRef      string                    `json:"source_external_ref"`
	Packet                 VerifiedPacketIdentity    `json:"packet"`
	FindingFingerprint     string                    `json:"finding_fingerprint"`
	ObservationFingerprint string                    `json:"observation_repository_fingerprint,omitempty"`
	RepositoryFingerprint  string                    `json:"repository_fingerprint"`
	ObservationState       string                    `json:"observation_state"`
	PublicationRole        string                    `json:"publication_role,omitempty"`
	RootCauseKey           string                    `json:"root_cause_key,omitempty"`
	BlockedByRootKeys      []string                  `json:"blocked_by_root_keys,omitempty"`
	IssueKind              string                    `json:"issue_kind"`
	Severity               string                    `json:"severity"`
	Title                  string                    `json:"title"`
	Body                   string                    `json:"body"`
	Labels                 []string                  `json:"labels,omitempty"`
	Role                   string                    `json:"role,omitempty"`
	RoutingReason          string                    `json:"routing_reason"`
	RoutingAllowed         bool                      `json:"routing_allowed"`
	AffectedContracts      []string                  `json:"affected_contracts,omitempty"`
	KnowledgeKeywords      []string                  `json:"knowledge_keywords,omitempty"`
	KnowledgeKeywordState  string                    `json:"knowledge_keyword_state"`
	EvidenceArtifacts      []EvidenceArtifactReceipt `json:"evidence_artifacts,omitempty"`
	ReproductionCommands   []string                  `json:"reproduction_commands,omitempty"`
	ReproductionSource     string                    `json:"reproduction_source,omitempty"`
	ValidationCommands     []string                  `json:"validation_commands,omitempty"`
	AllowedPaths           []string                  `json:"allowed_paths,omitempty"`
	BaseTreeSHA            string                    `json:"base_tree_sha,omitempty"`
	Recurrence             uint64                    `json:"recurrence"`
	Attempt                uint64                    `json:"attempt"`
	Authority              VisualWorkAuthority       `json:"authority"`
	Bead                   beads.BatchInput          `json:"-"`
}

type verifiedImportPlanSeal struct {
	bundle *ValidatedBundle
	digest string
}

// VerifiedImportPlan has no exported writable state and cannot be fabricated
// from public ValidatedBundle fields.
type VerifiedImportPlan struct {
	packet VerifiedPacketIdentity
	works  []AdmittedVisualWork
	seal   *verifiedImportPlanSeal
}

func (p VerifiedImportPlan) Packet() VerifiedPacketIdentity { return p.packet }

func (p VerifiedImportPlan) Works() []AdmittedVisualWork {
	encoded, _ := json.Marshal(p.works)
	var works []AdmittedVisualWork
	_ = json.Unmarshal(encoded, &works)
	for index := range p.works {
		works[index].Bead = cloneBatchInput(p.works[index].Bead)
	}
	return works
}

func (p VerifiedImportPlan) Valid() bool { return p.validate() == nil }

func (bundle *ValidatedBundle) BuildImportPlan() (VerifiedImportPlan, error) {
	if bundle == nil || !bundle.provenanceVerified || !bundle.sourceVerified || bundle.validatedManifest == nil || bundle.artifactIndex == nil {
		return VerifiedImportPlan{}, fmt.Errorf("complete privately verified Visual Hive source evidence is required")
	}
	if !bundle.Validation.Trusted || bundle.Validation.Status != "passed" {
		return VerifiedImportPlan{}, fmt.Errorf("trusted Visual Hive validation is required")
	}
	manifest, err := cloneCanonicalManifest(*bundle.validatedManifest)
	if err != nil {
		return VerifiedImportPlan{}, err
	}
	if manifest.SchemaVersion != ManifestSchemaV3 || manifest.ArtifactIndex == nil || manifest.Producer.Name != "visual-hive" {
		return VerifiedImportPlan{}, fmt.Errorf("production Visual Hive bundle v3 is required")
	}
	packet := VerifiedPacketIdentity{
		PacketID: manifest.BundleID, PacketDigest: manifest.OverallDigest, ReplayKey: manifest.ReplayProtection.Key,
		Repository: manifest.Source.Repository, RepositoryID: manifest.Source.RepositoryID, SourceRef: manifest.Source.Ref,
		BaseSHA: manifest.Source.CommitSHA, WorkflowName: manifest.Source.WorkflowName, WorkflowEvent: manifest.Source.Event,
		WorkflowRunID: manifest.Source.WorkflowRunID, WorkflowRunAttempt: manifest.Source.WorkflowRunAttempt,
		WorkflowArtifactID: manifest.Source.WorkflowArtifactID, ProducerName: manifest.Producer.Name,
		ProducerVersion: manifest.Producer.Version, ProducerGitCommit: manifest.Producer.GitCommit,
		ArtifactIndexSHA256: manifest.ArtifactIndex.SHA256, GeneratedAt: manifest.GeneratedAt.UTC(), ExpiresAt: manifest.ExpiresAt.UTC(), Verdict: manifest.Verdict,
	}
	inputs, err := buildObservationImportInputs(manifest, bundle.artifactIndex)
	if err != nil {
		return VerifiedImportPlan{}, err
	}
	artifactByPath := make(map[string]ArtifactIndexEntry, len(bundle.artifactIndex.Artifacts))
	for _, artifact := range bundle.artifactIndex.Artifacts {
		artifactByPath[artifact.Path] = artifact
	}
	works := make([]AdmittedVisualWork, 0, len(inputs))
	for _, observation := range manifest.Observations {
		input, present := inputs[observation.RepositoryFingerprint]
		if !present && observation.State == "present" {
			return VerifiedImportPlan{}, fmt.Errorf("present observation %s has no Hive import mapping", observation.RepositoryFingerprint)
		}
		route := ResolveSpecialist(SpecialistRoutingInput{
			IssueKind: observation.IssueKind, OwningAgentHint: observation.OwningAgentHint,
			GroundedRepairScope:      strings.TrimSpace(observation.ValidationCommand) != "" && validAffectedContracts(observation.AffectedContracts),
			BaselineChangesForbidden: true,
		})
		work := AdmittedVisualWork{
			SourceExternalRef: beadExternalRef(manifest.Source.Repository, observation.RepositoryFingerprint), Packet: packet, FindingFingerprint: observation.Fingerprint,
			ObservationFingerprint: observation.RepositoryFingerprint,
			RepositoryFingerprint:  observation.RepositoryFingerprint, PublicationRole: observation.PublicationRole,
			ObservationState: observation.State,
			RootCauseKey:     observation.RootCauseKey, BlockedByRootKeys: append([]string(nil), observation.BlockedByRootKeys...),
			IssueKind: observation.IssueKind, Severity: observation.Severity, Title: observation.Title, Body: observation.Body,
			Labels: append([]string(nil), observation.Labels...), Role: route.Role, RoutingReason: route.Reason, RoutingAllowed: route.DispatchAllowed,
			AffectedContracts:     append([]string(nil), observation.AffectedContracts...),
			KnowledgeKeywordState: "unavailable_no_verified_facts", Authority: VisualWorkAuthority{ProposalOnly: true},
		}
		if present {
			work.Bead = cloneBatchInput(input)
		}
		if command := strings.TrimSpace(observation.ValidationCommand); command != "" {
			work.ValidationCommands = verifiedObservationValidationCommands(command)
			work.ReproductionCommands = []string{command}
			work.ReproductionSource = "verified_observation_validation_command"
		}
		for _, path := range observation.SourceArtifacts {
			artifact := artifactByPath[path]
			work.EvidenceArtifacts = append(work.EvidenceArtifacts, EvidenceArtifactReceipt{
				Path: artifact.Path, SHA256: artifact.SHA256, Bytes: artifact.Bytes, Kind: artifact.Kind, ContentType: artifact.ContentType,
			})
		}
		works = append(works, work)
	}
	sort.Slice(works, func(i, j int) bool { return works[i].RepositoryFingerprint < works[j].RepositoryFingerprint })
	digest, err := digestImportPlan(packet, works)
	if err != nil {
		return VerifiedImportPlan{}, err
	}
	canonical := &ValidatedBundle{
		Manifest: manifest, Validation: bundle.Validation, artifactIndex: bundle.artifactIndex, validatedManifest: &manifest,
		provenanceVerified: true, sourceVerified: true, verifiedSourceRoot: bundle.verifiedSourceRoot,
	}
	return VerifiedImportPlan{packet: packet, works: works, seal: &verifiedImportPlanSeal{bundle: canonical, digest: digest}}, nil
}

// verifiedObservationValidationCommands translates trusted Visual Hive CLI
// guidance into the managed exact-PR-head validation seam. The granular
// producer command remains the reproduction command and in the evidence; Hive
// never executes it as target-owned shell text.
func verifiedObservationValidationCommands(command string) []string {
	if strings.HasPrefix(command, "visual-hive ") {
		return []string{ExactHeadValidationCommand}
	}
	return []string{command}
}

func (p VerifiedImportPlan) validate() error {
	if p.seal == nil || p.seal.bundle == nil {
		return fmt.Errorf("sealed verified Visual Hive import plan is required")
	}
	digest, err := digestImportPlan(p.packet, p.works)
	if err != nil {
		return err
	}
	if digest != p.seal.digest {
		return fmt.Errorf("verified Visual Hive import plan changed after construction")
	}
	return nil
}

func digestImportPlan(packet VerifiedPacketIdentity, works []AdmittedVisualWork) (string, error) {
	encoded, err := json.Marshal(struct {
		Schema string                 `json:"schema"`
		Packet VerifiedPacketIdentity `json:"packet"`
		Works  []AdmittedVisualWork   `json:"works"`
	}{VerifiedImportPlanSchema, packet, works})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func cloneBatchInput(input beads.BatchInput) beads.BatchInput {
	clone := input
	if input.DependsOn != nil {
		clone.DependsOn = append([]string{}, input.DependsOn...)
	}
	if input.Metadata != nil {
		clone.Metadata = make(map[string]interface{}, len(input.Metadata))
		for key, value := range input.Metadata {
			clone.Metadata[key] = cloneBatchMetadataValue(value)
		}
	}
	return clone
}

func cloneBatchMetadataValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		if typed == nil {
			return map[string]interface{}(nil)
		}
		clone := make(map[string]interface{}, len(typed))
		for key, nested := range typed {
			clone[key] = cloneBatchMetadataValue(nested)
		}
		return clone
	case map[string]string:
		if typed == nil {
			return map[string]string(nil)
		}
		clone := make(map[string]string, len(typed))
		for key, nested := range typed {
			clone[key] = nested
		}
		return clone
	case []interface{}:
		if typed == nil {
			return []interface{}(nil)
		}
		clone := make([]interface{}, len(typed))
		for index, nested := range typed {
			clone[index] = cloneBatchMetadataValue(nested)
		}
		return clone
	case []map[string]interface{}:
		if typed == nil {
			return []map[string]interface{}(nil)
		}
		clone := make([]map[string]interface{}, len(typed))
		for index, nested := range typed {
			clone[index] = cloneBatchMetadataValue(nested).(map[string]interface{})
		}
		return clone
	case []string:
		if typed == nil {
			return []string(nil)
		}
		return append([]string{}, typed...)
	case []int:
		if typed == nil {
			return []int(nil)
		}
		return append([]int{}, typed...)
	default:
		return value
	}
}

func cloneCanonicalManifest(manifest Manifest) (Manifest, error) {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return Manifest{}, err
	}
	var clone Manifest
	if err := json.Unmarshal(encoded, &clone); err != nil {
		return Manifest{}, err
	}
	return clone, nil
}

// buildObservationImportInputs is Hive's sole semantic mapper from verified
// manifest observations to native beads. Producer projections are previews
// and are intentionally not accepted as input here.
func buildObservationImportInputs(manifest Manifest, index *ArtifactIndexReport) (map[string]beads.BatchInput, error) {
	artifactByPath := map[string]ArtifactIndexEntry{}
	if index != nil {
		for _, artifact := range index.Artifacts {
			artifactByPath[artifact.Path] = artifact
		}
	}
	rootOwners := map[string]string{}
	for _, observation := range manifest.Observations {
		if observation.State == "present" && observation.PublicationRole == "canonical" && observation.RootCauseKey != "" {
			rootOwners[observation.RootCauseKey] = observation.RepositoryFingerprint
		}
	}
	result := make(map[string]beads.BatchInput, len(manifest.Observations))
	for _, observation := range manifest.Observations {
		if observation.State != "present" {
			continue
		}
		route := ResolveSpecialist(SpecialistRoutingInput{
			IssueKind: observation.IssueKind, OwningAgentHint: observation.OwningAgentHint,
			GroundedRepairScope:      strings.TrimSpace(observation.ValidationCommand) != "" && validAffectedContracts(observation.AffectedContracts),
			BaselineChangesForbidden: true,
		})
		artifacts := make([]map[string]interface{}, 0, len(observation.SourceArtifacts))
		for _, path := range observation.SourceArtifacts {
			artifact, exists := artifactByPath[path]
			if manifest.SchemaVersion == ManifestSchemaV3 && !exists {
				return nil, fmt.Errorf("observation %s artifact %q is not bound by the verified artifact index", observation.RepositoryFingerprint, path)
			}
			if exists {
				artifacts = append(artifacts, map[string]interface{}{
					"path": artifact.Path, "sha256": artifact.SHA256, "bytes": artifact.Bytes,
					"kind": artifact.Kind, "content_type": artifact.ContentType,
				})
			}
		}
		dependencies := make([]string, 0, len(observation.BlockedByRootKeys))
		dependencyRefs := make([]string, 0, len(observation.BlockedByRootKeys))
		for _, root := range observation.BlockedByRootKeys {
			if fingerprint := rootOwners[root]; fingerprint != "" {
				dependencies = append(dependencies, fingerprint)
				dependencyRefs = append(dependencyRefs, beadExternalRef(manifest.Source.Repository, fingerprint))
			}
		}
		sort.Strings(dependencies)
		sort.Strings(dependencyRefs)
		validation := []string{}
		if command := strings.TrimSpace(observation.ValidationCommand); command != "" {
			validation = append(validation, command)
		}
		result[observation.RepositoryFingerprint] = beads.BatchInput{
			SourceID: observation.RepositoryFingerprint, Title: observation.Title,
			Type: beadTypeForObservation(observation.IssueKind), Status: beads.StatusBlocked,
			Priority: priorityForSeverity(observation.Severity), Actor: route.Role,
			ExternalRef: beadExternalRef(manifest.Source.Repository, observation.RepositoryFingerprint),
			Notes:       observation.Body, DependsOn: dependencies,
			Metadata: map[string]interface{}{
				"visual_hive_controller_owned": true, "visual_hive_admission_state": "pending",
				"visual_hive_fingerprint": observation.Fingerprint, "visual_hive_repository_fingerprint": observation.RepositoryFingerprint,
				"visual_hive_packet_id": manifest.BundleID, "visual_hive_packet_digest": manifest.OverallDigest,
				"visual_hive_replay_key": manifest.ReplayProtection.Key, "visual_hive_repository": manifest.Source.Repository,
				"visual_hive_repository_id": manifest.Source.RepositoryID, "visual_hive_source_ref": manifest.Source.Ref,
				"visual_hive_base_sha": manifest.Source.CommitSHA, "visual_hive_workflow_name": manifest.Source.WorkflowName,
				"visual_hive_workflow_event": manifest.Source.Event, "visual_hive_workflow_run_id": manifest.Source.WorkflowRunID,
				"visual_hive_workflow_run_attempt": manifest.Source.WorkflowRunAttempt, "visual_hive_workflow_artifact_id": manifest.Source.WorkflowArtifactID,
				"visual_hive_producer": manifest.Producer.Name, "visual_hive_producer_version": manifest.Producer.Version,
				"visual_hive_producer_git_commit": manifest.Producer.GitCommit, "visual_hive_issue_kind": observation.IssueKind,
				"visual_hive_severity": observation.Severity, "visual_hive_publication_role": observation.PublicationRole,
				"visual_hive_root_cause_key": observation.RootCauseKey, "visual_hive_blocked_by_root_keys": append([]string(nil), observation.BlockedByRootKeys...),
				"visual_hive_dependency_external_refs": dependencyRefs, "visual_hive_affected_contracts": append([]string(nil), observation.AffectedContracts...),
				"visual_hive_validation_commands": validation, "visual_hive_evidence_artifacts": artifacts,
				"visual_hive_knowledge_keywords": []string{}, "visual_hive_knowledge_keyword_state": "unavailable_no_verified_facts",
				"visual_hive_route_role": route.Role, "visual_hive_route_reason": route.Reason, "visual_hive_route_allowed": route.DispatchAllowed,
				"hive_proposal_only": true, "hive_github_write_allowed": false, "hive_merge_allowed": false,
				"hive_baseline_changes_allowed": false, "hive_baseline_approval_allowed": false,
			},
		}
	}
	return result, nil
}

func validAffectedContracts(contracts []string) bool {
	if len(contracts) == 0 || len(contracts) > 256 {
		return false
	}
	seen := make(map[string]bool, len(contracts))
	for _, contract := range contracts {
		contract = strings.TrimSpace(contract)
		if contract == "" || len(contract) > 512 || strings.IndexByte(contract, 0) >= 0 || seen[contract] {
			return false
		}
		seen[contract] = true
	}
	return true
}

func beadTypeForObservation(issueKind string) beads.BeadType {
	kind := normalizeIssueKind(issueKind)
	switch {
	case baselineHoldKind(kind):
		return beads.TypeAdvisory
	case kind == "governance" || kind == "provider_governance":
		return beads.TypeDecision
	case kind == "setup_needed" || kind == "external_repo_onboarding" || kind == "map_drift" || kind == "discovery" || kind == "triage":
		return beads.TypeTask
	default:
		return beads.TypeBug
	}
}
